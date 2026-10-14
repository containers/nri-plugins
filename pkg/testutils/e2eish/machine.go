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

package e2eish

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	resapi "k8s.io/apimachinery/pkg/api/resource"

	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/topology"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// Group is one entry of an e2e topology.var.json. Zero fields take the
// defaults of test/e2e/lib/topology2qemuopts.py.
type Group struct {
	Mem      string      // memory per node, like 2G for 2Gi
	Cores    int         // cores per node, none for memory-only nodes
	Threads  int         // threads per core, 2 by default
	Nodes    int         // nodes per die, 1 by default
	Dies     int         // dies per package, 1 by default
	Packages int         // 1 by default
	NodeDist map[int]int // distances to other nodes, both ways
}

// Topology is an e2e topology.var.json.
type Topology []Group

// readTopology reads a topology.var.json.
func readTopology(path string) (Topology, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var groups []map[string]any
	if err := json.Unmarshal(data, &groups); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	topo, err := topologyOfGroups(groups)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return topo, nil
}

// topologyOfGroups is the topology of the groups of a topology.var.json.
func topologyOfGroups(groups []map[string]any) (Topology, error) {
	topo := Topology{}
	for _, g := range groups {
		group := Group{}
		for key, value := range g {
			n, _ := value.(float64)
			switch key {
			case "mem":
				group.Mem, _ = value.(string)
			case "cores":
				group.Cores = int(n)
			case "threads":
				group.Threads = int(n)
			case "nodes":
				group.Nodes = int(n)
			case "dies":
				group.Dies = int(n)
			case "packages":
				group.Packages = int(n)
			case "node-dist":
				dist, _ := value.(map[string]any)
				group.NodeDist = map[int]int{}
				for node, d := range dist {
					id, err := strconv.Atoi(node)
					if err != nil {
						return nil, fmt.Errorf("bad node-dist node %q", node)
					}
					dd, _ := d.(float64)
					group.NodeDist[id] = int(dd)
				}
			default:
				return nil, fmt.Errorf("topology key %q is not supported", key)
			}
		}
		topo = append(topo, group)
	}
	return topo, nil
}

// Host is what the e2e VM changes of the machine of its topology.
type Host struct {
	Caches   string // the OVERRIDE_SYS_CACHES JSON of an e2e helm config
	NoNUMA   bool   // the kernel has NUMA disabled
	Isolated string // CPUs isolated on the kernel command line
}

// NewMachine builds the machine qemu gives the e2e VM of the topology. CPUs
// are numbered package by package, die by die, node by node, core by core,
// with sibling threads adjacent, and dies are numbered across packages. Node
// distances are 10 to self, 11 within a die and 21 otherwise, unless NodeDist
// says else. Each core has its own L1 and L2, and its first CPU is its
// cluster ID. Each die has an L3. Cache overrides of the host replace these.
// A kernel with NUMA disabled has no nodes in sysfs.
func NewMachine(t *testing.T, topo Topology, host Host) system.System {
	t.Helper()

	def := func(v, d int) int {
		if v == 0 {
			return d
		}
		return v
	}

	// A node, with its socket and die as the defaults of distances see them.
	type node struct {
		group        *Group
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

	// Device paths, like those of topology hints, are of the machine too.
	topology.SetSysRoot(root)
	t.Cleanup(func() { topology.SetSysRoot("") })

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
		kind := map[string]string{
			"d": "Data", "data": "Data", "i": "Instruction", "instruction": "Instruction",
			"u": "Unified", "unified": "Unified", "": "Unified",
		}[strings.ToLower(e.Kind)]
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

// Reexec runs the test again in a child process with the given VAR=VALUE
// environment, for variables a package reads when the test binary starts. It
// returns true in the parent, which has nothing left to do then. The parent
// logs what the child test logged, and skips if the child skipped.
func Reexec(t *testing.T, env ...string) bool {
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

	var logs []string
	skipped := false
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "--- SKIP: "+t.Name()+" "):
			skipped = true
		case testLogLine.MatchString(line):
			logs = append(logs, strings.TrimSpace(line))
		}
	}
	if skipped {
		t.Skipf("in a child process with %v:\n%s", env, strings.Join(logs, "\n"))
	}
	if len(logs) > 0 {
		t.Logf("in a child process with %v:\n%s", env, strings.Join(logs, "\n"))
	}
	return true
}

// testLogLine is a line of t.Log output in go test -v.
var testLogLine = regexp.MustCompile(`^\s+\S+\.go:\d+: `)
