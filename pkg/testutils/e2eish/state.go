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
	"fmt"
	"slices"
	"strconv"

	"github.com/stretchr/testify/require"

	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// stateVars are the variables of py_allowed.py the accessors compute.
var stateVars = []string{"cpus", "mems", "nodes", "cores", "threads", "dies", "packages"}

// topoNames are the names topology.py gives the CPUs and memory nodes of a
// machine: each number zero-padded to the width of the largest one of its
// level among the CPUs.
type topoNames struct {
	m                                    system.System
	pkgW, dieW, nodeW, coreW, thrW, cpuW int
	thread                               map[int]int // CPU to its rank among its siblings
}

func newTopoNames(m system.System) *topoNames {
	n := &topoNames{m: m, thread: map[int]int{}}
	width := func(w *int, id int) { *w = max(*w, len(strconv.Itoa(id))) }
	for _, id := range m.CPUIDs() {
		c := m.CPU(id)
		n.thread[id] = slices.Index(c.ThreadCPUSet().List(), id)
		width(&n.pkgW, c.PackageID())
		width(&n.dieW, max(c.DieID(), 0))
		width(&n.nodeW, c.NodeID())
		width(&n.coreW, c.CoreID())
		width(&n.thrW, n.thread[id])
		width(&n.cpuW, id)
	}
	return n
}

func pad(prefix string, id, w int) string {
	return fmt.Sprintf("%s%0*d", prefix, w, id)
}

func (n *topoNames) cpu(id int) string  { return pad("cpu", id, n.cpuW) }
func (n *topoNames) node(id int) string { return pad("node", id, n.nodeW) }
func (n *topoNames) pkg(id int) string  { return pad("package", id, n.pkgW) }

// state is what py_allowed.py computes for a container allowed the CPUs and
// the memory nodes, nil for all of them.
func (n *topoNames) state(cpus cpuset.CPUSet, mems []int) map[string]Set {
	st := map[string]Set{}
	for _, v := range stateVars {
		st[v] = Set{}
	}
	for _, id := range cpus.List() {
		c := n.m.CPU(id)
		pkg, node := n.pkg(c.PackageID()), n.node(c.NodeID())
		core := node + "/" + pad("core", c.CoreID(), n.coreW)
		st["cpus"][n.cpu(id)] = struct{}{}
		st["nodes"][node] = struct{}{}
		st["cores"][core] = struct{}{}
		st["threads"][core+"/"+pad("thread", n.thread[id], n.thrW)] = struct{}{}
		st["dies"][pkg+"/"+pad("die", max(c.DieID(), 0), n.dieW)] = struct{}{}
		st["packages"][pkg] = struct{}{}
	}
	if mems == nil {
		mems = n.m.NodeIDs()
	}
	for _, id := range mems {
		st["mems"][n.node(id)] = struct{}{}
	}
	return st
}

// stateOf is the state of a running container, as py_allowed.py computes it.
func (s *Script) stateOf(ctr string) map[string]Set {
	s.t.Helper()
	require.NoError(s.t, e2eVisible(ctr))
	require.NotNil(s.t, s.h, "no policy launched")
	cpus, memStr := s.h.Allowed(ctr)
	var mems []int
	if memStr != "" {
		set, err := cpuset.Parse(memStr)
		require.NoError(s.t, err, "%s: bad memory nodes %q", ctr, memStr)
		mems = set.List()
	}
	return newTopoNames(s.h.Machine).state(cpus, mems)
}

// CPUs is cpus["ctr"] of e2e verify expressions, and the others likewise.
func (s *Script) CPUs(ctr string) Set {
	s.t.Helper()
	return s.stateOf(ctr)["cpus"]
}

// Mems is mems["ctr"].
func (s *Script) Mems(ctr string) Set {
	s.t.Helper()
	return s.stateOf(ctr)["mems"]
}

// Nodes is nodes["ctr"].
func (s *Script) Nodes(ctr string) Set {
	s.t.Helper()
	return s.stateOf(ctr)["nodes"]
}

// Cores is cores["ctr"].
func (s *Script) Cores(ctr string) Set {
	s.t.Helper()
	return s.stateOf(ctr)["cores"]
}

// Threads is threads["ctr"].
func (s *Script) Threads(ctr string) Set {
	s.t.Helper()
	return s.stateOf(ctr)["threads"]
}

// Dies is dies["ctr"].
func (s *Script) Dies(ctr string) Set {
	s.t.Helper()
	return s.stateOf(ctr)["dies"]
}

// Packages is packages["ctr"].
func (s *Script) Packages(ctr string) Set {
	s.t.Helper()
	return s.stateOf(ctr)["packages"]
}

// CPUIDs is the set of the CPUs, as topology.py names them on the machine.
func (s *Script) CPUIDs(ids ...int) Set {
	s.t.Helper()
	return s.names((*topoNames).cpu, ids)
}

// NodeIDs is the set of the nodes, as topology.py names them on the machine.
func (s *Script) NodeIDs(ids ...int) Set {
	s.t.Helper()
	return s.names((*topoNames).node, ids)
}

// PackageIDs is the set of the packages, as topology.py names them.
func (s *Script) PackageIDs(ids ...int) Set {
	s.t.Helper()
	return s.names((*topoNames).pkg, ids)
}

func (s *Script) names(name func(*topoNames, int) string, ids []int) Set {
	s.t.Helper()
	require.NotNil(s.t, s.h, "no policy launched")
	n := newTopoNames(s.h.Machine)
	out := Set{}
	for _, id := range ids {
		out[name(n, id)] = struct{}{}
	}
	return out
}

// e2eVisible tells if an e2e test sees the state of a container: it finds
// only those named like pod0c0.
func e2eVisible(ctr string) error {
	if !testContainer.MatchString(ctr) {
		return fmt.Errorf("container %s, which e2e tests do not see, as it is not named like pod0c0", ctr)
	}
	return nil
}
