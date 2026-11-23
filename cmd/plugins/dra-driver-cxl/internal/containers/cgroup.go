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

package containers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/nri/pkg/api"
	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"
)

// The cgroups of the containers: the container's own cgroup directory and
// what the plugin reads of it.

// procDir is the proc filesystem root (a variable for tests).
var procDir = "/proc"

// cgroupRoot is the cgroup v2 mount point (a variable for tests).
var cgroupRoot = "/sys/fs/cgroup"

// resolveContainerCgroupDir returns the cgroup v2 directory of a container:
// the container's own cgroup (containerCgroupDir) of the cgroup of its init
// process in /proc/<pid>/cgroup, which is more reliable than the
// CgroupsPath of NRI (which may use the systemd slice notation).
func resolveContainerCgroupDir(ctr *api.Container) (string, error) {
	pid := ctr.GetPid()
	if pid == 0 {
		return "", fmt.Errorf("container PID is 0, cannot resolve cgroup")
	}
	path := filepath.Join(procDir, fmt.Sprint(pid), "cgroup")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", path, err)
	}
	rel, ok := cgroupV2Path(data)
	if !ok {
		return "", fmt.Errorf("no cgroup v2 entry found in %s (content: %q)", path, string(data))
	}
	dir := containerCgroupDir(filepath.Join(cgroupRoot, rel), ctr.GetId())
	klog.V(5).Infof("container %s: cgroup %s (pid %d, /proc cgroup path %q)", ctr.GetId(), dir, pid, rel)
	return dir, nil
}

// pidCgroupDir returns the absolute cgroup v2 directory of a process.
func pidCgroupDir(pid int) (string, error) {
	path := filepath.Join(procDir, fmt.Sprint(pid), "cgroup")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	rel, ok := cgroupV2Path(data)
	if !ok {
		return "", fmt.Errorf("no cgroup v2 entry in %s", path)
	}
	return filepath.Join(cgroupRoot, rel), nil
}

// cgroupV2Path returns the path of the cgroup v2 line "0::<path>" of the
// content of /proc/<pid>/cgroup.
func cgroupV2Path(data []byte) (string, bool) {
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			return rel, true
		}
	}
	return "", false
}

// containerCgroupDir returns the cgroup directory of the container id
// from the cgroup directory dir of its init process: the nearest of dir
// and its ancestors under the cgroup root whose name has the id, as the
// runtimes name the cgroup of a container (cri-containerd-<id>.scope,
// crio-<id>.scope, <id> with the cgroupfs driver); dir if none does. The
// init process is in the container's cgroup at StartContainer (runc
// init), but the workload may have moved into a child cgroup by the time
// of a Synchronize: systemd in the container moves itself to init.scope,
// nested runtimes delegate. The runtime updates the container's cgroup, the
// cpuset.mems of the child stays "".
func containerCgroupDir(dir, id string) string {
	if id == "" {
		return dir
	}
	root := filepath.Clean(cgroupRoot)
	for d := filepath.Clean(dir); strings.HasPrefix(d, root+"/"); d = filepath.Dir(d) {
		if strings.Contains(filepath.Base(d), id) {
			return d
		}
	}
	return dir
}

// inCgroup tells whether the cgroup directory dir of a process is in the
// cgroup of a container: its cgroup directory or one below it, or, while
// its cgroup is not resolved, a directory that names its ID (the runtimes
// name the scope after the id).
func inCgroup(c *Container, dir string) bool {
	if c.CgroupDir != "" {
		return dir == c.CgroupDir || strings.HasPrefix(dir, c.CgroupDir+"/")
	}
	return c.ID != "" && strings.Contains(dir, c.ID)
}

// curMems returns the cpuset.mems of a container: that of its cgroup once
// its task exists (an update before its start may have reached its spec
// only), else that of its spec as the plugin knows it.
func curMems(c *Container) string {
	if c.started() && c.CgroupDir != "" {
		if data, err := os.ReadFile(filepath.Join(c.CgroupDir, "cpuset.mems")); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return c.SpecMems
}

// ownClaimed returns the claimed nodes that the cpuset.mems of a container
// (curMems) has: its own, before any lend can reach its cpuset.mems. After
// a plugin restart a claimed node that a lend of the previous plugin added
// counts as its own too: Synchronize cannot tell the two apart, and a
// claimed node is better kept than taken.
func ownClaimed(c *Container) cpuset.CPUSet {
	if set, err := cpuset.Parse(curMems(c)); err == nil {
		return c.Claimed.Intersection(set)
	}
	return cpuset.New()
}

// baseMems returns the memory nodes a container has of its own: its
// cpuset.mems.effective (or, if unreadable, the cpuset.mems of its spec),
// minus the nodes lent to any container by the plugin. An unconstrained
// container started while a lent CXL node is online would otherwise count
// that node as its own. Called with mu held.
func (p *Plugin) baseMems(c *Container, ctr *api.Container) cpuset.CPUSet {
	var mems string
	if c.CgroupDir != "" {
		if data, err := os.ReadFile(filepath.Join(c.CgroupDir, "cpuset.mems.effective")); err == nil {
			mems = strings.TrimSpace(string(data))
		} else {
			klog.V(3).Infof("NRI lend: ctr=%s: %v", c.name(), err)
		}
	}
	if mems == "" {
		mems = specMems(ctr)
	}
	set, err := cpuset.Parse(mems)
	if err != nil {
		klog.Warningf("NRI lend: ctr=%s: invalid memory nodes %q: %v", c.name(), mems, err)
		return cpuset.New()
	}
	all := cpuset.New()
	for _, o := range p.ctrs {
		all = all.Union(o.Lends.nodes())
	}
	return set.Difference(all)
}

// cgroupLacks returns the nodes that the cpuset.mems of the cgroup dir
// lacks, of its cpuset.mems.effective when its cpuset.mems is empty (it
// has the nodes of its parent); none when the cgroup cannot be read.
func cgroupLacks(dir string, nodes cpuset.CPUSet) cpuset.CPUSet {
	if dir == "" || nodes.IsEmpty() {
		return cpuset.New()
	}
	for _, file := range []string{"cpuset.mems", "cpuset.mems.effective"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return cpuset.New()
		}
		if s := strings.TrimSpace(string(data)); s != "" {
			set, err := cpuset.Parse(s)
			if err != nil {
				return cpuset.New()
			}
			return nodes.Difference(set)
		}
	}
	return cpuset.New()
}

// cgroupMemsContain tells whether cpuset.mems of a cgroup contains node.
func cgroupMemsContain(dir string, node int) bool {
	if dir == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, "cpuset.mems"))
	if err != nil {
		return false
	}
	set, err := cpuset.Parse(strings.TrimSpace(string(data)))
	return err == nil && set.Contains(node)
}

// memNodes reads the NUMA nodes with memory from a sysfs file, once.
type memNodes struct {
	path  string
	read  bool
	nodes cpuset.CPUSet
	err   error
}

// get returns the NUMA nodes with memory.
func (m *memNodes) get() (cpuset.CPUSet, error) {
	if !m.read {
		m.read = true
		data, err := os.ReadFile(m.path)
		if err == nil {
			m.nodes, err = cpuset.Parse(strings.TrimSpace(string(data)))
		}
		if err == nil && m.nodes.IsEmpty() {
			err = fmt.Errorf("no NUMA nodes with memory in %s", m.path)
		}
		if err != nil {
			m.err = fmt.Errorf("nodes with memory: %w", err)
		}
	}
	return m.nodes, m.err
}

// memNodes returns the reader of the NUMA nodes with memory.
func (p *Plugin) memNodes() *memNodes {
	return &memNodes{path: p.hasMemory}
}
