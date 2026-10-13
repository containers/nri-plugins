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

package topologyaware

// The harness of the e2eish tests, which replay e2e tests against the policy
// alone, see "Replaying test logs as unit tests" in test/e2e/README.md. The
// machine copies the qemu VM of the e2e topology. Pods and containers go
// through a real cache, and are allocated and released in the order the
// resource manager uses for NRI events.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	nri "github.com/containerd/nri/pkg/api"
	"github.com/stretchr/testify/require"
	resapi "k8s.io/apimachinery/pkg/api/resource"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/kubernetes"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"

	logcfg "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log"
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log/klogcontrol"
	logger "github.com/containers/nri-plugins/pkg/log"
)

// e2eGroup is one entry of an e2e topology.var.json. Zero fields take the
// defaults of test/e2e/lib/topology2qemuopts.py.
type e2eGroup struct {
	Mem      string      // memory per node, like 2G for 2Gi
	Cores    int         // cores per node, none for memory-only nodes
	Threads  int         // threads per core, 2 by default
	Nodes    int         // nodes per die, 1 by default
	Dies     int         // dies per package, 1 by default
	Packages int         // 1 by default
	NodeDist map[int]int // distances to other nodes, both ways
}

// e2eTopology is an e2e topology.var.json.
type e2eTopology []e2eGroup

// e2eHost is what the e2e VM changes of the machine of its topology.
type e2eHost struct {
	Caches   string // the OVERRIDE_SYS_CACHES JSON of an e2e helm config
	NoNUMA   bool   // the kernel has NUMA disabled
	Isolated string // CPUs isolated on the kernel command line
}

// e2eMachine builds the machine qemu gives the e2e VM of the topology. CPUs
// are numbered package by package, die by die, node by node, core by core,
// with sibling threads adjacent, and dies are numbered across packages. Node
// distances are 10 to self, 11 within a die and 21 otherwise, unless NodeDist
// says else. Each core has its own L1 and L2, and its first CPU is its
// cluster ID. Each die has an L3. Cache overrides of the host replace these.
// A kernel with NUMA disabled has no nodes in sysfs.
func e2eMachine(t *testing.T, topo e2eTopology, host e2eHost) system.System {
	t.Helper()

	def := func(v, d int) int {
		if v == 0 {
			return d
		}
		return v
	}

	// A node, with its socket and die as the defaults of distances see them.
	type node struct {
		group        *e2eGroup
		socket, die  int
		pkg          int // the CPU package, for nodes with CPUs
		memKB, first int // first is the first core in its die
	}
	var (
		nodes  []*node
		socket = -1
		pkg    = -1
	)
	for i := range topo {
		g := &topo[i]
		// qemu sizes are binary: 2G is 2Gi.
		mem, err := resapi.ParseQuantity(strings.TrimSuffix(g.Mem, "i") + "i")
		require.NoError(t, err, "bad topology memory %q", g.Mem)
		for range def(g.Packages, 1) {
			socket++
			if g.Cores > 0 {
				pkg++
			}
			for d := range def(g.Dies, 1) {
				for n := range def(g.Nodes, 1) {
					nodes = append(nodes, &node{
						group:  g,
						socket: socket,
						die:    d,
						pkg:    pkg,
						memKB:  int(mem.Value() / 1024),
						first:  n * g.Cores,
					})
				}
			}
		}
	}

	dist := make([][]int, len(nodes))
	for i, n := range nodes {
		dist[i] = make([]int, len(nodes))
		for j, o := range nodes {
			switch {
			case i == j:
				dist[i][j] = 10
			case n.socket == o.socket && n.die == o.die:
				dist[i][j] = 11
			default:
				dist[i][j] = 21
			}
		}
	}
	for i, n := range nodes {
		for j, d := range n.group.NodeDist {
			require.Less(t, j, len(nodes), "distance to a missing node %d", j)
			dist[i][j], dist[j][i] = d, d
		}
	}

	file := func(format string, args ...any) string {
		return fmt.Sprintf(format+"\n", args...)
	}
	var (
		fsys   = map[string]string{}
		cpu    = 0
		all    = cpuset.New()
		normal = cpuset.New()
		dies   = map[[2]int]cpuset.CPUSet{}
	)
	// Die IDs are global, in the order of the packages.
	dieIDs := map[[2]int]int{}
	for _, n := range nodes {
		key := [2]int{n.pkg, n.die}
		if _, ok := dieIDs[key]; !ok && n.group.Cores > 0 {
			dieIDs[key] = len(dieIDs)
		}
	}

	for id, n := range nodes {
		nodeCPUs := cpuset.New()
		threads := def(n.group.Threads, 2)
		for c := range n.group.Cores {
			siblings := cpuset.New()
			for th := range threads {
				siblings = siblings.Union(cpuset.New(cpu + th))
			}
			for range threads {
				dir := fmt.Sprintf("sys/devices/system/cpu/cpu%d", cpu)
				fsys[dir+"/topology/physical_package_id"] = file("%d", n.pkg)
				fsys[dir+"/topology/die_id"] = file("%d", dieIDs[[2]int{n.pkg, n.die}])
				fsys[dir+"/topology/cluster_id"] = file("%d", siblings.List()[0])
				fsys[dir+"/topology/core_id"] = file("%d", n.first+c)
				fsys[dir+"/topology/core_cpus_list"] = file("%s", siblings)
				fsys[dir+"/topology/thread_siblings_list"] = file("%s", siblings)
				fsys[fmt.Sprintf("%s/node%d/cpulist", dir, id)] = file("")
				for i, c := range []struct{ level, kind, size string }{
					{"1", "Data", "32K"}, {"1", "Instruction", "32K"}, {"2", "Unified", "4096K"},
				} {
					index := fmt.Sprintf("%s/cache/index%d/", dir, i)
					fsys[index+"level"] = file("%s", c.level)
					fsys[index+"type"] = file("%s", c.kind)
					fsys[index+"id"] = file("%d", cpu/threads)
					fsys[index+"shared_cpu_list"] = file("%s", siblings)
					fsys[index+"size"] = file("%s", c.size)
				}
				cpu++
			}
			nodeCPUs = nodeCPUs.Union(siblings)
		}
		if !nodeCPUs.IsEmpty() {
			key := [2]int{n.pkg, n.die}
			dies[key] = dies[key].Union(nodeCPUs)
		}

		row := make([]string, 0, len(nodes))
		for _, d := range dist[id] {
			row = append(row, fmt.Sprintf("%d", d))
		}
		dir := fmt.Sprintf("sys/devices/system/node/node%d", id)
		fsys[dir+"/cpulist"] = file("%s", nodeCPUs)
		fsys[dir+"/distance"] = file("%s", strings.Join(row, " "))
		fsys[dir+"/meminfo"] = file("Node %d MemTotal: %d kB\nNode %d MemFree: %d kB",
			id, n.memKB, id, n.memKB)
		normal = normal.Union(cpuset.New(id))
		all = all.Union(nodeCPUs)
	}

	for key, cpus := range dies {
		for _, id := range cpus.List() {
			dir := fmt.Sprintf("sys/devices/system/cpu/cpu%d/cache/index3", id)
			fsys[dir+"/level"] = file("3")
			fsys[dir+"/type"] = file("Unified")
			fsys[dir+"/id"] = file("%d", dieIDs[key])
			fsys[dir+"/shared_cpu_list"] = file("%s", cpus)
			fsys[dir+"/size"] = file("16384K")
		}
	}

	if host.Caches != "" {
		writeCacheOverrides(t, fsys, host.Caches)
	}

	fsys["sys/devices/system/cpu/online"] = file("%s", all)
	fsys["sys/devices/system/cpu/present"] = file("%s", all)
	fsys["sys/devices/system/cpu/possible"] = file("%s", all)
	fsys["sys/devices/system/cpu/isolated"] = file("%s", host.Isolated)
	fsys["sys/devices/system/node/has_normal_memory"] = file("%s", normal)
	fsys["sys/devices/system/node/has_memory"] = file("%s", normal)
	fsys["sys/devices/system/node/online"] = file("%s", normal)

	if host.NoNUMA {
		cpuNode := regexp.MustCompile(`^sys/devices/system/cpu/cpu[0-9]+/node[0-9]+/`)
		for name := range fsys {
			if strings.HasPrefix(name, "sys/devices/system/node/") || cpuNode.MatchString(name) {
				delete(fsys, name)
			}
		}
	}

	root := t.TempDir()
	for name, data := range fsys {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(data), 0o644))
	}

	sys, err := system.DiscoverSystemAt(filepath.Join(root, "sys"))
	require.NoError(t, err, "failed to discover the e2e test machine")
	return sys
}

// writeCacheOverrides replaces the caches of the CPUs in the overrides with
// them. Each CPU set of an override is one cache.
func writeCacheOverrides(t *testing.T, fsys map[string]string, overrides string) {
	t.Helper()

	var entries []struct {
		Cpusets []string `json:"cpusets"`
		Level   int      `json:"level"`
		Size    string   `json:"size"`
		Kind    string   `json:"kind"`
	}
	require.NoError(t, json.Unmarshal([]byte(overrides), &entries), "bad cache overrides")

	type cache struct {
		id, level  int
		kind, size string
		cpus       cpuset.CPUSet
	}
	var (
		caches = map[int][]cache{}
		ids    = map[string]int{}
	)
	for _, e := range entries {
		size, err := resapi.ParseQuantity(strings.ToUpper(e.Size) + "i")
		require.NoError(t, err, "bad cache size %q", e.Size)
		kind := map[string]string{"d": "Data", "i": "Instruction", "": "Unified"}[e.Kind]
		require.NotEmpty(t, kind, "bad cache kind %q", e.Kind)
		for _, set := range e.Cpusets {
			cpus, err := cpuset.Parse(set)
			require.NoError(t, err, "bad cache CPUs %q", set)
			key := fmt.Sprintf("%d/%s", e.Level, kind)
			c := cache{ids[key], e.Level, kind, fmt.Sprintf("%dK", size.Value()/1024), cpus}
			ids[key]++
			for _, id := range cpus.List() {
				caches[id] = append(caches[id], c)
			}
		}
	}

	for id, list := range caches {
		dir := fmt.Sprintf("sys/devices/system/cpu/cpu%d/cache", id)
		for name := range fsys {
			if strings.HasPrefix(name, dir+"/") {
				delete(fsys, name)
			}
		}
		slices.SortFunc(list, func(a, b cache) int {
			return cmp.Or(cmp.Compare(a.level, b.level), cmp.Compare(a.kind, b.kind))
		})
		for i, c := range list {
			index := fmt.Sprintf("%s/index%d/", dir, i)
			fsys[index+"id"] = fmt.Sprintf("%d\n", c.id)
			fsys[index+"level"] = fmt.Sprintf("%d\n", c.level)
			fsys[index+"type"] = c.kind + "\n"
			fsys[index+"size"] = c.size + "\n"
			fsys[index+"shared_cpu_list"] = c.cpus.String() + "\n"
		}
	}
}

// e2eReexec runs the test again in a child process with the given VAR=VALUE
// environment, for variables a package reads when the test binary starts. It
// returns true in the parent, which has nothing left to do then.
func e2eReexec(t *testing.T, env ...string) bool {
	t.Helper()

	if os.Getenv("E2EISH_REEXEC") == t.Name() {
		return false
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$",
		"-test.count=1", "-test.v")
	cmd.Env = append(append(os.Environ(), env...), "E2EISH_REEXEC="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed in a child process with %v: %v\n%s", t.Name(), env, err, out)
	}
	return true
}

// e2eConfig is the policy configuration test/e2e/policies.test-suite/
// topology-aware/helm-config.yaml.in produces with no variables set, with
// the CRD defaults applied. Options adjust it like the variables do.
// ControllableInterrupts stays unset, so the test never touches host IRQs.
func e2eConfig(options ...func(*cfgapi.Config)) *cfgapi.Config {
	cfg := &cfgapi.Config{
		ReservedResources:  cfgapi.Constraints{cfgapi.CPU: "750m"},
		PinCPU:             true,
		PinMemory:          true,
		PreferShared:       e2eFalse(),
		PreferIsolated:     e2eFalse(),
		DefaultCPUPriority: cfgapi.PriorityNone,
		UnlimitedBurstable: cfgapi.CPUTopologyLevelPackage,
	}
	for _, o := range options {
		o(cfg)
	}
	return cfg
}

func e2eFalse() *bool { v := false; return &v }

// e2eish is one e2e test run: a machine, and the policy and cache of the
// currently launched policy.
type e2eish struct {
	t       *testing.T
	machine system.System
	cache   cache.Cache
	policy  *policy
	podCnt  int                        // the e2e pod counter
	ctrCnt  int                        // makes container IDs unique
	pods    map[string]cache.Pod       // live pods by name
	ctrs    map[string]cache.Container // live containers by name
	specs   map[string]ctrSpec         // containers by name, for restarts
	owner   *draTestOwner              // the DRA devices the policy publishes
}

// ctrSpec is what a container is created from.
type ctrSpec struct {
	pod cache.Pod
	qos string
	res containerResources
}

func newE2eish(t *testing.T, topo e2eTopology) *e2eish {
	return newE2eishWithCaches(t, topo, "")
}

// newE2eishWithCaches is newE2eish with the OVERRIDE_SYS_CACHES of an e2e
// helm config.
func newE2eishWithCaches(t *testing.T, topo e2eTopology, cacheOverrides string) *e2eish {
	captureLogs(t)
	return &e2eish{
		t:       t,
		machine: e2eMachine(t, topo, e2eHost{Caches: cacheOverrides}),
	}
}

var (
	e2eLog       = logger.NewLogger("e2eish")
	e2eLogDir    string
	e2eLogDirSet sync.Once
)

// captureLogs collects every log message of the test, debug ones included,
// and writes them to a file if the test fails.
func captureLogs(t *testing.T) {
	t.Helper()

	buf := &bytes.Buffer{}
	var out io.Writer = buf
	if os.Getenv("E2EISH_VERBOSE") == "1" {
		out = io.MultiWriter(buf, os.Stderr)
	}
	old := logger.SetOutput(out)
	skipHeaders := true
	require.NoError(t, logger.Configure(&logcfg.Config{
		Debug:     []string{"*"},
		LogSource: true,
		Klog:      klogcontrol.Config{Skip_headers: &skipHeaders},
	}))
	e2eLog.Infof("===== %s", t.Name())

	t.Cleanup(func() {
		logger.SetOutput(old)
		if err := logger.Configure(logger.ConfigFromEnv()); err != nil {
			t.Errorf("failed to restore logging: %v", err)
		}
		if !t.Failed() {
			return
		}

		e2eLogDirSet.Do(func() {
			if e2eLogDir = os.Getenv("E2EISH_LOG_DIR"); e2eLogDir == "" {
				e2eLogDir = filepath.Join(os.TempDir(), "e2eish",
					time.Now().Format("20060102-150405")+fmt.Sprintf("-%d", os.Getpid()))
			}
		})
		name := strings.ReplaceAll(t.Name(), "/", "_") + ".log"
		path := filepath.Join(e2eLogDir, name)
		if err := os.MkdirAll(e2eLogDir, 0o755); err != nil {
			t.Errorf("failed to create log directory: %v", err)
			return
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Errorf("failed to write test log: %v", err)
			return
		}
		t.Logf("test log: %s", path)
	})
}

// launch is helm-launch: it starts a fresh policy with the configuration.
// Like helm-terminate, it drops the state of any earlier policy, so the
// tests delete their pods before relaunching, as the e2e tests do.
func (e *e2eish) launch(cfg *cfgapi.Config) {
	e.t.Helper()
	if data, err := json.Marshal(cfg); err == nil {
		e2eLog.Infof("===== launch %s", data)
	}

	cch, err := cache.NewCache(cache.Options{CacheDir: e.t.TempDir()})
	require.NoError(e.t, err, "failed to create cache")

	owner := &draTestOwner{}
	p := New().(*policy)
	require.NoError(e.t, p.Setup(&policyapi.BackendOptions{
		System: e.machine,
		Cache:  cch,
		Owner:  owner,
		Config: cfg,
	}), "failed to set up policy")
	require.NoError(e.t, p.Start(), "failed to start policy")

	e.cache = cch
	e.policy = p
	e.owner = owner
	e.pods = map[string]cache.Pod{}
	e.ctrs = map[string]cache.Container{}
	e.specs = map[string]ctrSpec{}
}

// launchFails is expect-launch-failure.
func (e *e2eish) launchFails(cfg *cfgapi.Config) {
	e.t.Helper()

	cch, err := cache.NewCache(cache.Options{CacheDir: e.t.TempDir()})
	require.NoError(e.t, err, "failed to create cache")

	err = New().Setup(&policyapi.BackendOptions{
		System: e.machine,
		Cache:  cch,
		Owner:  &draTestOwner{},
		Config: cfg,
	})
	require.Error(e.t, err, "policy setup succeeded, expected it to fail")
	e2eLog.Infof("===== launch failed as expected: %v", err)
}

// resetCounters is reset counters.
func (e *e2eish) resetCounters() {
	e.podCnt = 0
}

// podSpec has the variables of the e2e pod templates in test/e2e/files.
// Zero fields take the defaults of test/e2e/run.sh.
type podSpec struct {
	Namespace   string
	ContCount   int    // CONTCOUNT, 1 by default
	IContCount  int    // ICONTCOUNT
	CPU, Mem    string // guaranteed, 1 and 100M by default
	CPUReq      string // burstable, 1 by default
	CPULim      string // burstable, 2 by default, "0" for none
	MemReq      string // burstable, 100M by default
	MemLim      string // burstable, 200M by default, "0" for none
	Annotations map[string]string
	WantError   string // if set, the containers must fail with this error
}

// containerResources are the resources of one container of a pod.
type containerResources struct {
	cpuReq, cpuLim int64 // in milli-CPUs, 0 for unset
	memReq, memLim int64 // in bytes, 0 for unset
}

func (e *e2eish) quantity(s, def string) *resapi.Quantity {
	e.t.Helper()
	if s == "" {
		s = def
	}
	q, err := resapi.ParseQuantity(s)
	require.NoError(e.t, err, "bad quantity %q", s)
	return &q
}

// createGuaranteed is create guaranteed.
func (e *e2eish) createGuaranteed(spec podSpec) string {
	e.t.Helper()
	cpu := e.quantity(spec.CPU, "1").MilliValue()
	mem := e.quantity(spec.Mem, "100M").Value()
	return e.create("", spec, containerResources{cpu, cpu, mem, mem})
}

// createBurstable is create burstable.
func (e *e2eish) createBurstable(spec podSpec) string {
	e.t.Helper()
	return e.create("burstable", spec, containerResources{
		cpuReq: e.quantity(spec.CPUReq, "1").MilliValue(),
		cpuLim: e.quantity(spec.CPULim, "2").MilliValue(),
		memReq: e.quantity(spec.MemReq, "100M").Value(),
		memLim: e.quantity(spec.MemLim, "200M").Value(),
	})
}

// createBestEffort is create besteffort.
func (e *e2eish) createBestEffort(spec podSpec) string {
	e.t.Helper()
	return e.create("besteffort", spec, containerResources{})
}

// create creates the next pod and starts its containers. Init containers run
// one at a time to completion first, then the containers start in order. It
// returns the pod name.
func (e *e2eish) create(qos string, spec podSpec, res containerResources) string {
	e.t.Helper()

	name := fmt.Sprintf("pod%d", e.podCnt)
	e.podCnt++
	e2eLog.Infof("===== create %s %s %+v", cmp.Or(qos, "guaranteed"), name, spec)

	pod := e.insertPod(cmp.Or(spec.Namespace, "default"), name, qos, spec.Annotations)

	for i := 0; i < spec.IContCount; i++ {
		c, err := e.createContainer(ctrSpec{pod, qos, res}, fmt.Sprintf("%sc%di", name, i))
		require.NoError(e.t, err)
		e.stopContainer(c)
	}
	for i := 0; i < max(spec.ContCount, 1); i++ {
		_, err := e.createContainer(ctrSpec{pod, qos, res}, fmt.Sprintf("%sc%d", name, i))
		if spec.WantError != "" {
			require.ErrorContains(e.t, err, spec.WantError)
		} else {
			require.NoError(e.t, err)
		}
	}

	return name
}

// insertPod does what the resource manager does for RunPodSandbox. qos is ""
// for guaranteed, burstable or besteffort.
func (e *e2eish) insertPod(ns, name, qos string, annotations map[string]string) cache.Pod {
	e.t.Helper()

	uid := fmt.Sprintf("%s-%s-uid", ns, name)
	parent := "/kubepods/pod" + uid
	if qos != "" {
		parent = "/kubepods/" + qos + "/pod" + uid
	}

	e.cache.InsertPod(&nri.PodSandbox{
		Id:          uid + "-sandbox",
		Uid:         uid,
		Name:        name,
		Namespace:   ns,
		Annotations: annotations,
		Linux:       &nri.LinuxPodSandbox{CgroupParent: parent},
	}, nil)
	pod, ok := e.cache.LookupPod(uid + "-sandbox")
	require.True(e.t, ok, "failed to look up pod %s", name)
	e.pods[name] = pod

	return pod
}

// insertContainer adds a container to the cache, as the resource manager does
// first for CreateContainer, with the resources the kubelet would give it.
func (e *e2eish) insertContainer(spec ctrSpec, name string) cache.Container {
	e.t.Helper()

	res := spec.res
	r := &nri.LinuxResources{
		Cpu:    &nri.LinuxCPU{},
		Memory: &nri.LinuxMemory{},
	}
	// The kubelet sets the minimal shares for no request, and the limit
	// of a guaranteed container for its request.
	r.Cpu.Shares = nri.UInt64(cache.MilliCPUToShares(res.cpuReq))
	if res.cpuLim > 0 {
		quota, period := cache.MilliCPUToQuota(res.cpuLim)
		r.Cpu.Quota = nri.Int64(quota)
		r.Cpu.Period = nri.UInt64(period)
	}
	if res.memLim > 0 {
		r.Memory.Limit = nri.Int64(res.memLim)
	}

	// The cache estimates a burstable memory request from the OOM score
	// adjustment, at a resolution of 1/1000 of the memory of the host.
	var oomAdj int64
	switch spec.qos {
	case "":
		oomAdj = kubernetes.GuaranteedOOMScoreAdj
	case "besteffort":
		oomAdj = kubernetes.BestEffortOOMScoreAdj
	default:
		oomAdj = min(max(kubernetes.MemReqToOomAdj(res.memReq),
			kubernetes.MinBurstableOOMScoreAdj), kubernetes.MaxBurstableOOMScoreAdj)
	}

	e.ctrCnt++
	c, err := e.cache.InsertContainer(&nri.Container{
		Id:           fmt.Sprintf("%s-%d", name, e.ctrCnt),
		PodSandboxId: spec.pod.GetID(),
		Name:         name,
		Linux: &nri.LinuxContainer{
			Resources:   r,
			OomScoreAdj: nri.Int(int(oomAdj)),
		},
	}, cache.WithContainerState(cache.ContainerStateCreating))
	require.NoError(e.t, err, "failed to insert container %s", name)
	e.specs[name] = spec

	return c
}

// createContainer does what the resource manager does for CreateContainer and
// StartContainer. A container which fails allocation is left stale.
func (e *e2eish) createContainer(spec ctrSpec, name string) (cache.Container, error) {
	e.t.Helper()

	c := e.insertContainer(spec, name)
	e.owner.Lock()
	defer e.owner.Unlock()
	if err := e.policy.AllocateResources(c); err != nil {
		c.UpdateState(cache.ContainerStateStale)
		return c, err
	}
	c.UpdateState(cache.ContainerStateCreated)
	c.UpdateState(cache.ContainerStateRunning)
	require.NoError(e.t, e.policy.ContainerStarted(c), "failed to start %s", name)
	e.ctrs[name] = c

	return c, nil
}

// restartContainer is a container crashing and the kubelet restarting it.
func (e *e2eish) restartContainer(name string) {
	e.t.Helper()

	c, ok := e.ctrs[name]
	require.True(e.t, ok, "no container %s to restart", name)
	e2eLog.Infof("===== restart %s", name)
	e.stopContainer(c)
	_, err := e.createContainer(e.specs[name], name)
	require.NoError(e.t, err, "failed to restart %s", name)
}

// exitContainer is a container exiting for good.
func (e *e2eish) exitContainer(name string) {
	e.t.Helper()

	c, ok := e.ctrs[name]
	require.True(e.t, ok, "no container %s to exit", name)
	e2eLog.Infof("===== exit %s", name)
	e.stopContainer(c)
}

// hasGrant tells if the policy has allocations for the running container.
func (e *e2eish) hasGrant(name string) bool {
	c, ok := e.ctrs[name]
	if !ok {
		return false
	}
	_, ok = e.policy.allocations.grants[c.GetID()]
	return ok
}

// stopContainer does what the resource manager does for StopContainer and
// RemoveContainer.
func (e *e2eish) stopContainer(c cache.Container) {
	e.t.Helper()

	e.owner.Lock()
	defer e.owner.Unlock()
	require.NoError(e.t, e.policy.ReleaseResources(c), "failed to release %s", c.GetName())
	c.UpdateState(cache.ContainerStateExited)
	e.cache.DeleteContainer(c.GetID())
	maps.DeleteFunc(e.ctrs, func(_ string, ctr cache.Container) bool { return ctr == c })
}

// deletePods is kubectl delete pods.
func (e *e2eish) deletePods(names ...string) {
	e.t.Helper()

	for _, name := range names {
		pod, ok := e.pods[name]
		require.True(e.t, ok, "no pod %s to delete", name)
		e2eLog.Infof("===== delete %s", name)
		ctrs := pod.GetContainers()
		sort.Slice(ctrs, func(i, j int) bool { return ctrs[i].GetName() < ctrs[j].GetName() })
		for _, c := range ctrs {
			e.stopContainer(c)
		}
		e.cache.DeletePod(pod.GetID())
		delete(e.pods, name)
	}
}

// deleteAllPods is kubectl delete pods --all, in any namespace.
func (e *e2eish) deleteAllPods() {
	e.t.Helper()

	names := make([]string, 0, len(e.pods))
	for name := range e.pods {
		names = append(names, name)
	}
	slices.Sort(names)
	e.deletePods(names...)
}

// idSet is a set of e2e topology names, like cpu04, node1 or package0.
type idSet map[string]struct{}

func (s idSet) String() string {
	names := make([]string, 0, len(s))
	for n := range s {
		names = append(names, n)
	}
	slices.Sort(names)
	return "{" + strings.Join(names, ", ") + "}"
}

// cpuIds gives the set of the given CPUs, like the e2e cpu_ids.
func cpuIds(ids ...int) idSet {
	s := idSet{}
	for _, id := range ids {
		s[fmt.Sprintf("cpu%02d", id)] = struct{}{}
	}
	return s
}

// nodeIds gives the set of the given nodes, like the e2e node_ids.
func nodeIds(ids ...int) idSet {
	s := idSet{}
	for _, id := range ids {
		s[fmt.Sprintf("node%d", id)] = struct{}{}
	}
	return s
}

// allowed is report allowed: the CPUs and memory nodes a container may use,
// as the e2e tests see them in /proc/self/status.
func (e *e2eish) allowed(name string) (cpuset.CPUSet, string) {
	e.t.Helper()

	c, ok := e.ctrs[name]
	require.True(e.t, ok, "no container %s", name)

	// A timer of the policy may update containers, with the owner locked.
	e.owner.Lock()
	cpuStr, mems := c.GetCpusetCpus(), c.GetCpusetMems()
	e.owner.Unlock()

	cpus := e.machine.OnlineCPUs()
	if cpuStr != "" {
		var err error
		cpus, err = cpuset.Parse(cpuStr)
		require.NoError(e.t, err, "%s: bad cpuset %q", name, cpuStr)
	}
	return cpus, mems
}

// topoSet maps the allowed CPUs of a container to e2e topology names.
func (e *e2eish) topoSet(name string, label func(system.CPU) string) idSet {
	e.t.Helper()

	cpus, _ := e.allowed(name)
	s := idSet{}
	for _, id := range cpus.List() {
		s[label(e.machine.CPU(id))] = struct{}{}
	}
	return s
}

// cpus is cpus["name"].
func (e *e2eish) cpus(name string) idSet {
	return e.topoSet(name, func(c system.CPU) string {
		return fmt.Sprintf("cpu%02d", c.ID())
	})
}

// cores is cores["name"].
func (e *e2eish) cores(name string) idSet {
	return e.topoSet(name, func(c system.CPU) string {
		return fmt.Sprintf("package%d/core%d", c.PackageID(), c.CoreID())
	})
}

// nodes is nodes["name"]: the nodes of the allowed CPUs.
func (e *e2eish) nodes(name string) idSet {
	return e.topoSet(name, func(c system.CPU) string {
		return fmt.Sprintf("node%d", c.NodeID())
	})
}

// dies is dies["name"].
func (e *e2eish) dies(name string) idSet {
	return e.topoSet(name, func(c system.CPU) string {
		return fmt.Sprintf("package%d/die%d", c.PackageID(), c.DieID())
	})
}

// packages is packages["name"].
func (e *e2eish) packages(name string) idSet {
	return e.topoSet(name, func(c system.CPU) string {
		return fmt.Sprintf("package%d", c.PackageID())
	})
}

// mems is mems["name"]: the allowed memory nodes.
func (e *e2eish) mems(name string) idSet {
	e.t.Helper()

	_, mems := e.allowed(name)
	if mems == "" {
		return nodeIds(e.machine.NodeIDs()...)
	}
	set, err := cpuset.Parse(mems)
	require.NoError(e.t, err, "%s: bad memset %q", name, mems)
	return nodeIds(set.List()...)
}

// requireDisjoint is verify 'disjoint_sets(...)'.
func requireDisjoint(t *testing.T, sets ...idSet) {
	t.Helper()

	seen := map[string]int{}
	for i, s := range sets {
		for n := range s {
			if j, ok := seen[n]; ok {
				t.Fatalf("sets #%d %s and #%d %s share %s", j, sets[j], i, s, n)
			}
			seen[n] = i
		}
	}
}

// union is set.union(...).
func union(sets ...idSet) idSet {
	u := idSet{}
	for _, s := range sets {
		for n := range s {
			u[n] = struct{}{}
		}
	}
	return u
}

// running gives the names of the running containers, sorted.
func (e *e2eish) running() []string {
	names := make([]string, 0, len(e.ctrs))
	for name := range e.ctrs {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// unshared is cpus[name] - set.union(cpus of every other container).
func (e *e2eish) unshared(name string) idSet {
	e.t.Helper()

	s := e.cpus(name)
	for _, other := range e.running() {
		if other == name {
			continue
		}
		for n := range e.cpus(other) {
			delete(s, n)
		}
	}
	return s
}
