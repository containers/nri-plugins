// Copyright The NRI Plugins Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package steering steers the memory allocations of a cgroup v2
// across NUMA nodes along a Plan.
//
// A Plan is a sequence of waypoints, each giving a target memory
// usage per node. Plan.NextStep tells on which nodes the next
// allocations should land and how many bytes may be allocated before
// the route is re-evaluated. NewPlan builds a Plan that consumes DRAM
// and CXL quotas in the order of a Policy.
//
// A Manager applies the steps to a cgroup with two mechanisms. The
// nodes of every step are added to cpuset.mems of the cgroup, which
// confines all allocations of the cgroup to the nodes used so far.
// After the first write nodes are only added, so memory is not
// migrated. When none of the allowed nodes has normal (non-movable)
// memory, for instance a CXL node whose memory is onlined movable, the
// nearest node with normal memory is allowed too: unmovable user
// allocations (pipe buffers, for instance) obey cpuset.mems but cannot
// use movable memory, and fail with ENOMEM without such a node. The
// nodes of the step are set as the memory policy of the processes of
// the cgroup and of the cgroups below it with mpolinject. The step size
// is enforced with memory.high through a notifier: the kernel throttles
// the cgroup when the step has been allocated, the manager computes the
// next step, updates the policy and raises memory.high again. Past the
// last waypoint memory.high is max, and the manager reads the usage at
// every wake until it is below the last waypoint again.
package steering

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/mpolinject"
)

// waitTimeout is the interval at which the lower memory bound is
// checked when no memory.high event arrives, and the plan when no bound
// is set (a variable for tests).
var waitTimeout = time.Second

// errRefused is the error of a step whose memory policy a process
// refuses: the steering ends.
var errRefused = errors.New("memory policy refused")

// numaUsageCategories are the memory.numa_stat categories that count
// as usage steered by the plan.
var numaUsageCategories = []string{"anon", "shmem"}

// setMemoryPolicy sets the memory policy of the threads of processes (a
// variable for tests).
var setMemoryPolicy = mpolinject.SetMemoryPolicy

// Manager steers the memory allocations of one cgroup along a plan.
type Manager struct {
	cgroupPath string
	sysRoot    string
	name       string
	plan       *Plan
	notifier   *notifier
	// permitted are the nodes that cpuset.mems may have.
	permitted cpuset.CPUSet
	// allowed are the nodes written to cpuset.mems so far.
	allowed map[int]bool
	// written: cpuset.mems has been written.
	written bool
	// mems is the cpuset.mems of the cgroup before Start.
	mems string
	// unbounded: the last step set no memory.high bound (past the last
	// waypoint, or nothing to steer).
	unbounded bool
	// nodes are the nodes of the memory policy of the last step, which
	// the processes seen have.
	nodes []int
	seen  map[int]bool
	// onEnd is called when the steering ends on a refused memory policy.
	onEnd func(error)
	// stop is closed by Stop, done is closed when the steering
	// goroutine has exited.
	stop     chan struct{}
	done     chan struct{}
	started  bool
	stopOnce sync.Once
}

// Option configures a Manager.
type Option func(*Manager)

// WithName sets the name of the manager in log messages. The default
// is the base name of the cgroup path.
func WithName(name string) Option {
	return func(m *Manager) {
		m.name = name
	}
}

// WithOnEnd sets the function that is called once when the steering ends
// on a memory policy that a process refuses (EPERM, for instance by its
// seccomp profile), with the reason. memory.high is max by then,
// cpuset.mems is as the steering wrote it (see RestoreMems), and Stop
// returns at once.
func WithOnEnd(fn func(error)) Option {
	return func(m *Manager) {
		m.onEnd = fn
	}
}

// WithSysRoot sets the root of the sysfs where the NUMA nodes are
// found. The default is /sys.
func WithSysRoot(root string) Option {
	return func(m *Manager) {
		m.sysRoot = root
	}
}

// New returns a Manager for the cgroup and the plan. Nothing is
// written to the cgroup before Start. The plan must not be modified
// after this call. It returns an error when memory policies cannot be
// set on this platform.
//   - permitted: the nodes that cpuset.mems may have, the nodes of the
//     plan among them
func New(cgroupPath string, plan *Plan, permitted cpuset.CPUSet, opts ...Option) (*Manager, error) {
	if !mpolinject.Supported {
		return nil, mpolinject.ErrUnsupported
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}
	if nodes := plan.Nodes(); !nodes.IsSubsetOf(permitted) {
		return nil, fmt.Errorf("plan nodes %s are not in the permitted nodes %s", nodes, permitted)
	}
	info, err := os.Stat(cgroupPath)
	if err != nil {
		return nil, fmt.Errorf("cgroup: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("cgroup %s is not a directory", cgroupPath)
	}
	m := &Manager{
		cgroupPath: cgroupPath,
		sysRoot:    "/sys",
		name:       filepath.Base(cgroupPath),
		plan:       plan,
		permitted:  permitted,
		allowed:    make(map[int]bool),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Start applies the first step of the plan to the cgroup and starts
// the goroutine that applies the following steps. It returns an error,
// with the cgroup as it was, when memory.high of the cgroup is not max
// or the memory policy of its processes cannot be set.
func (m *Manager) Start() error {
	if m.started {
		return errors.New("manager already started")
	}
	high, err := readTrim(filepath.Join(m.cgroupPath, "memory.high"))
	if err != nil {
		return err
	}
	if high != "max" {
		return fmt.Errorf("memory.high is %s, not max", high)
	}
	if m.mems, err = readTrim(filepath.Join(m.cgroupPath, "cpuset.mems")); err != nil {
		return err
	}
	// Permitted nodes that hold anon or file memory stay allowed: writing
	// cpuset.mems migrates the pages that the processes map off the other
	// nodes.
	held, err := numaUsage(m.cgroupPath, "anon", "file")
	if err != nil {
		return err
	}
	for node, bytes := range held {
		if bytes > 0 && m.permitted.Contains(node) {
			m.allowed[node] = true
		}
	}
	usage, err := numaUsage(m.cgroupPath, numaUsageCategories...)
	if err != nil {
		return err
	}

	m.notifier, err = newNotifier(m.cgroupPath)
	if err != nil {
		return err
	}
	klog.Infof("%s: start steering %s: %s", m.name, m.cgroupPath, m.plan)
	if err := m.step(usage, true); err != nil {
		_ = m.notifier.SetBounds(bounds{})
		_ = m.notifier.Close()
		m.notifier = nil
		if m.written {
			m.restoreMems()
		}
		return err
	}
	m.started = true
	go m.loop()
	return nil
}

// Stop stops steering, removes memory.high from the cgroup and waits
// for the steering goroutine to exit. cpuset.mems and the memory
// policies of the threads are left as they are.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		close(m.stop)
		if !m.started {
			return
		}
		if err := m.notifier.Interrupt(); err != nil {
			klog.Errorf("%s: interrupt failed: %v", m.name, err)
		}
		<-m.done
		m.closeNotifier()
	})
}

// gone tells whether an error of a cgroup file says that the cgroup is
// removed.
func gone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENODEV)
}

// loop applies a step whenever the memory usage of the cgroup
// crosses a bound, until Stop is called or the cgroup is removed.
func (m *Manager) loop() {
	var refused error
	defer func() {
		m.release()
		close(m.done)
		if refused != nil && m.onEnd != nil {
			m.onEnd(refused)
		}
	}()
	for {
		ev, err := m.notifier.Wait(waitTimeout)
		select {
		case <-m.stop:
			return
		default:
		}
		if errors.Is(err, errInterrupted) {
			continue
		}
		if gone(err) {
			klog.Infof("%s: cgroup %s is gone, stop steering", m.name, m.cgroupPath)
			return
		}
		if err != nil {
			klog.Errorf("%s: wait failed: %v", m.name, err)
			select {
			case <-m.stop:
				return
			case <-time.After(waitTimeout):
			}
			continue
		}
		switch {
		case ev.crossing != noCrossing:
			klog.V(4).Infof("%s: %s bound crossed, memory.current=%d", m.name, ev.crossing, ev.memoryCurrent)
		case !m.unbounded:
			continue
		}
		usage, err := numaUsage(m.cgroupPath, numaUsageCategories...)
		if err == nil {
			err = m.step(usage, false)
		}
		switch {
		case gone(err):
			klog.Infof("%s: cgroup %s is gone, stop steering", m.name, m.cgroupPath)
			return
		case errors.Is(err, errRefused):
			klog.Warningf("%s: %v: steering ends", m.name, err)
			refused = err
			return
		case err != nil:
			klog.Errorf("%s: step failed: %v", m.name, err)
		}
	}
}

// closeNotifier closes the notifier of the manager.
func (m *Manager) closeNotifier() {
	if err := m.notifier.Close(); err != nil {
		klog.Errorf("%s: closing notifier failed: %v", m.name, err)
	}
}

// release removes memory.high from the cgroup.
func (m *Manager) release() {
	err := m.notifier.SetBounds(bounds{})
	switch {
	case gone(err):
		klog.V(4).Infof("%s: cgroup is gone, memory.high not removed", m.name)
	case err != nil:
		klog.Errorf("%s: removing memory.high failed: %v", m.name, err)
	}
	klog.Infof("%s: stopped steering %s", m.name, m.cgroupPath)
}

// Release removes memory.high from a cgroup that no manager steers any
// more, as Stop does: memory.high is set to max, cpuset.mems and the memory
// policies of the threads are left as they are. A manager whose process was
// killed leaves the cgroup throttled at the memory.high of its last step.
// Returns the memory.high that was replaced, "" when it was max already
// (nothing is written then). An error wrapping os.ErrNotExist tells that
// the cgroup has no memory.high (it is gone); the file is never created.
func Release(cgroupPath string) (string, error) {
	path := filepath.Join(cgroupPath, "memory.high")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	high := strings.TrimSpace(string(data))
	if high == "max" {
		return "", nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return "", err
	}
	_, err = f.WriteString("max\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("write memory.high: %w", err)
	}
	return high, nil
}

// step applies the next step of the plan for the usage: allows the
// step nodes in cpuset.mems, sets the memory policy of the threads
// in the cgroup and sets the memory bounds for the next step.
//   - first: the step of Start, which fails when a memory policy is
//     not set
func (m *Manager) step(usage NodeMem, first bool) error {
	step, err := m.plan.NextStep(usage)
	if err != nil {
		return err
	}
	if m.unbounded && step.Bytes == 0 && slices.Equal(step.Nodes, m.nodes) {
		return nil
	}
	if len(step.Nodes) == 0 {
		klog.V(4).Infof("%s: step: usage=%s nothing to steer, memory.high max", m.name, usage)
		m.unbounded, m.nodes = true, nil
		return m.notifier.SetBounds(bounds{})
	}

	if err := m.allowNodes(step.Nodes); err != nil {
		return err
	}

	pids, err := procs(m.cgroupPath)
	if err != nil {
		return fmt.Errorf("reading processes: %w", err)
	}
	// The processes that have the policy of the step already are left
	// alone: setting a policy stops every thread of a process.
	targets := pids
	if slices.Equal(step.Nodes, m.nodes) {
		targets = slices.DeleteFunc(slices.Clone(pids), func(pid int) bool { return m.seen[pid] })
	}
	if len(targets) > 0 {
		mode := unix.MPOL_PREFERRED
		if len(step.Nodes) > 1 {
			mode = unix.MPOL_INTERLEAVE
		}
		err := setMemoryPolicy(targets, mode, step.Nodes)
		switch {
		case err != nil && first:
			return fmt.Errorf("setting memory policy: %w", err)
		case errors.Is(err, unix.EPERM):
			return fmt.Errorf("%w: %w", errRefused, err)
		case err != nil:
			// The bounds are set even if some threads could not be
			// updated, otherwise the cgroup would stay throttled.
			klog.Warningf("%s: setting memory policy failed: %v", m.name, err)
		}
	}
	m.seen = make(map[int]bool, len(pids))
	for _, pid := range pids {
		m.seen[pid] = true
	}

	if step.Past {
		if err := m.notifier.SetBounds(bounds{}); err != nil {
			return err
		}
		m.unbounded, m.nodes = true, step.Nodes
		klog.Infof("%s: past the last waypoint: usage=%s nodes=%v procs=%d/%d cpuset.mems=%s, memory.high max",
			m.name, usage, step.Nodes, len(targets), len(pids), m.allowedNodes())
		return nil
	}
	if m.unbounded {
		klog.Infof("%s: steering resumes: usage=%s", m.name, usage)
	}
	m.unbounded, m.nodes = false, step.Nodes

	current, err := memoryCurrent(m.cgroupPath)
	if err != nil {
		return fmt.Errorf("reading memory.current: %w", err)
	}
	b := bounds{Upper: current + step.Bytes}
	if current > step.Bytes {
		b.Lower = current - step.Bytes
	}
	if err := m.notifier.SetBounds(b); err != nil {
		return err
	}
	klog.V(4).Infof("%s: step: usage=%s nodes=%v bytes=%d procs=%d/%d cpuset.mems=%s lower=%d upper=%d",
		m.name, usage, step.Nodes, step.Bytes, len(targets), len(pids), m.allowedNodes(), b.Lower, b.Upper)
	return nil
}

// allowNodes adds the nodes to cpuset.mems of the cgroup if they are
// not allowed yet. The first call writes cpuset.mems also when it adds no
// node.
func (m *Manager) allowNodes(nodes []int) error {
	added := false
	for _, node := range nodes {
		if !m.allowed[node] {
			m.allowed[node] = true
			added = true
		}
	}
	// until the first write the cgroup inherits every node
	if !added && m.written {
		return nil
	}
	if node, ok := normalMemoryFallback(m.sysRoot, m.allowed, m.permitted, nodes[0]); ok {
		m.allowed[node] = true
		klog.Infof("%s: NUMA nodes %s have no normal (non-movable) memory, allowing NUMA node %d for unmovable allocations",
			m.name, m.allowedNodes().Difference(cpuset.New(node)), node)
	}
	mems := m.allowedNodes().String()
	if err := os.WriteFile(filepath.Join(m.cgroupPath, "cpuset.mems"), []byte(mems+"\n"), 0644); err != nil {
		return fmt.Errorf("write cpuset.mems %q: %w", mems, err)
	}
	m.written = true
	klog.V(4).Infof("%s: cpuset.mems=%s", m.name, mems)
	return nil
}

// RestoreMems writes back the cpuset.mems that the cgroup had before Start,
// after Stop.
func (m *Manager) RestoreMems() {
	m.restoreMems()
}

// restoreMems writes back the cpuset.mems that the cgroup had before
// Start.
func (m *Manager) restoreMems() {
	if err := os.WriteFile(filepath.Join(m.cgroupPath, "cpuset.mems"), []byte(m.mems+"\n"), 0644); err != nil {
		klog.Errorf("%s: restoring cpuset.mems %q failed: %v", m.name, m.mems, err)
	}
}

// allowedNodes returns the nodes allowed in cpuset.mems so far.
func (m *Manager) allowedNodes() cpuset.CPUSet {
	nodes := make([]int, 0, len(m.allowed))
	for node := range m.allowed {
		nodes = append(nodes, node)
	}
	sort.Ints(nodes)
	return cpuset.New(nodes...)
}

// normalMemoryFallback returns the permitted node with normal
// (non-movable) memory that is nearest to node near, if none of the
// allowed nodes has normal memory. ok is false if one has, or if no
// permitted node is known to have normal memory.
func normalMemoryFallback(sysRoot string, allowed map[int]bool, permitted cpuset.CPUSet, near int) (node int, ok bool) {
	nodeDir := filepath.Join(sysRoot, "devices", "system", "node")
	b, err := os.ReadFile(filepath.Join(nodeDir, "has_normal_memory"))
	if err != nil {
		return 0, false
	}
	normal, err := cpuset.Parse(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	normal = normal.Intersection(permitted)
	if normal.IsEmpty() {
		return 0, false
	}
	for n := range allowed {
		if allowed[n] && normal.Contains(n) {
			return 0, false
		}
	}
	// the distances of near to the online nodes, in the order of the
	// online nodes; unknown distances leave the lowest normal node
	distance := map[int]int{}
	b, err = os.ReadFile(filepath.Join(nodeDir, "node"+strconv.Itoa(near), "distance"))
	if err == nil {
		online, err := readTrim(filepath.Join(nodeDir, "online"))
		nodes, perr := cpuset.Parse(online)
		fields := strings.Fields(string(b))
		if err == nil && perr == nil && nodes.Size() == len(fields) {
			for i, n := range nodes.List() {
				if d, err := strconv.Atoi(fields[i]); err == nil {
					distance[n] = d
				}
			}
		}
	}
	best, bestDist := -1, 0
	for _, n := range normal.List() {
		d := distance[n]
		if best < 0 || d < bestDist {
			best, bestDist = n, d
		}
	}
	return best, true
}
