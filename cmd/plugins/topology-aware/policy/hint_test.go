// Copyright 2019 Intel Corporation. All Rights Reserved.
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

import (
	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
	"testing"

	"github.com/containers/nri-plugins/pkg/topology"
	idset "github.com/intel/goresctrl/pkg/utils"
)

func TestCpuHintScore(t *testing.T) {
	tcases := []struct {
		name     string
		expected float64
		hint     topology.Hint
		cpus     *libcpu.CpuMask
		disabled bool // TODO(rojkov): remove this field when the code is fixed.
	}{
		{
			name:     "handle zero cpu size gracefully",
			disabled: true,
		},
		{
			name: "handle unparsable cpu size gracefully",
			hint: topology.Hint{
				CPUs: "unparsable",
			},
		},
		{
			name: "non-zero cpu size hint and empty CPUs",
			hint: topology.Hint{
				CPUs: "1",
			},
		},
		{
			name: "hint corresponding to given CPU",
			hint: topology.Hint{
				CPUs: "1,2",
			},
			cpus:     libcpu.NewCpuMask(1),
			expected: 0.5,
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.disabled {
				t.Skipf("The case '%s' is skipped", tc.name)
			}
			actual := cpuHintScore(tc.hint, tc.cpus)
			if actual != tc.expected {
				t.Errorf("Expected %f, but got %f", tc.expected, actual)
			}
		})
	}
}

func TestNumaHintScore(t *testing.T) {
	tcases := []struct {
		name     string
		expected float64
		hint     topology.Hint
		ids      []idset.ID
	}{
		{
			name: "handle unparsable NUMAs gracefully",
			hint: topology.Hint{
				NUMAs: "unparsable",
			},
		},
		{
			name: "non-zero NUMA hint and empty NUMAs",
			hint: topology.Hint{
				NUMAs: "1",
			},
		},
		{
			name: "hint corresponding to a given ID",
			ids:  []idset.ID{1},
			hint: topology.Hint{
				NUMAs: "1,2",
			},
			expected: 1.0,
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			actual := numaHintScore(tc.hint, tc.ids...)
			if actual != tc.expected {
				t.Errorf("Expected %f, but got %f", tc.expected, actual)
			}
		})
	}
}

func TestSocketHintScore(t *testing.T) {
	tcases := []struct {
		name     string
		expected float64
		hint     topology.Hint
		id       idset.ID
	}{
		{
			name: "handle unparsable Sockets gracefully",
			hint: topology.Hint{
				Sockets: "unparsable",
			},
		},
		{
			name: "non-zero Sockets hint and empty Sockets",
			hint: topology.Hint{
				Sockets: "1",
			},
		},
		{
			name: "hint corresponding to a given ID",
			id:   1,
			hint: topology.Hint{
				Sockets: "1,2",
			},
			expected: 1.0,
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			actual := socketHintScore(tc.hint, tc.id)
			if actual != tc.expected {
				t.Errorf("Expected %f, but got %f", tc.expected, actual)
			}
		})
	}
}

func TestHintCpus(t *testing.T) {
	tcases := []struct {
		name     string
		supply   *supply
		hint     topology.Hint
		expected *libcpu.CpuMask
	}{
		{
			name:   "handle unparsable Sockets gracefully",
			supply: &supply{},
			hint: topology.Hint{
				Sockets: "unparsable",
			},
		},
		{
			name: "Sockets hint naming a socket the machine does not have",
			supply: &supply{
				node: &node{
					policy: &policy{
						machine: oneCpuMachine(t),
					},
				},
			},
			hint: topology.Hint{
				Sockets: "1",
			},
		},
		{
			name:   "handle unparsable NUMAs gracefully",
			supply: &supply{},
			hint: topology.Hint{
				NUMAs: "unparsable",
			},
		},
		{
			name: "NUMAs hint naming a node the machine does not have",
			supply: &supply{
				node: &node{
					policy: &policy{
						machine: oneCpuMachine(t),
					},
				},
			},
			hint: topology.Hint{
				NUMAs: "1",
			},
		},
		{
			// Two packages of two CPUs, one NUMA node each. A hint naming a
			// socket resolves to that socket's CPUs, and one naming a NUMA node
			// to that node's.
			name: "Sockets hint resolves to the socket's CPUs",
			supply: &supply{
				node: &node{
					policy: &policy{machine: twoSocketMachine(t)},
				},
			},
			hint: topology.Hint{
				Sockets: "1",
			},
			expected: libcpu.NewCpuMask(2, 3),
		},
		{
			name: "NUMAs hint resolves to the node's CPUs",
			supply: &supply{
				node: &node{
					policy: &policy{machine: twoSocketMachine(t)},
				},
			},
			hint: topology.Hint{
				NUMAs: "0",
			},
			expected: libcpu.NewCpuMask(0, 1),
		},
		{
			name:   "non-zero CPUs hint",
			supply: &supply{},
			hint: topology.Hint{
				CPUs: "1",
			},
			expected: libcpu.NewCpuMask(1),
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			actual := tc.supply.hintCpus(tc.hint)
			if tc.expected.IsEmpty() && actual.IsEmpty() {
				return
			}
			if !tc.expected.Equals(actual) {
				t.Errorf("Expected %+v, but got %+v", tc.expected, actual)
			}
		})
	}
}
