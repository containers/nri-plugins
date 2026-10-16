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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

func TestTopoNames(t *testing.T) {
	// n4c16: 2 packages of 2 nodes of 2 cores of 2 threads, CPUs 0-15.
	m := NewMachine(t, Topology{{Mem: "2G", Cores: 2, Nodes: 2, Packages: 2}}, Host{})
	n := newTopoNames(m)
	st := n.state(cpuset.New(4, 5), []int{1})
	require.Equal(t, NewSet("cpu04", "cpu05"), st["cpus"])
	require.Equal(t, NewSet("node1"), st["nodes"])
	require.Equal(t, NewSet("node1"), st["mems"])
	require.Len(t, st["cores"], 1)
	require.Len(t, st["threads"], 2)
	require.Equal(t, NewSet("package0"), st["packages"])
	require.Equal(t, NewSet("package0/die0"), st["dies"])
	require.Equal(t, "cpu04", n.cpu(4))
	require.Equal(t, "node3", n.node(3))
}

func TestTopoNamesPadding(t *testing.T) {
	// n4c128: CPU ids up to 127 pad to three digits.
	m := NewMachine(t, Topology{{Mem: "2G", Cores: 16, Nodes: 2, Packages: 2}}, Host{})
	n := newTopoNames(m)
	require.Equal(t, "cpu004", n.cpu(4))
	st := n.state(cpuset.New(4), nil)
	require.Equal(t, NewSet("cpu004"), st["cpus"])
	// nil mems is all of them, as res_allowed reports an unset cpuset.mems.
	require.Len(t, st["mems"], len(m.NodeIDs()))
}

func TestVerifyPyWithoutPython(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	s := &Script{t: t, h: &Harness{T: t}}
	s.verify(`len(cpus["pod0c0"]) == 1`, true)
	require.Equal(t, 1, s.skipped)
}

func TestReplayVerifyWithoutPython(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	reached := false
	t.Run("replay", func(t *testing.T) {
		s := &Script{t: t, h: &Harness{T: t}, requirePython: true}
		s.verify(`len(cpus["pod0c0"]) == 1`, true)
		reached = true
	})
	require.False(t, reached, "a replay with no python3 must skip, not pass")
}
