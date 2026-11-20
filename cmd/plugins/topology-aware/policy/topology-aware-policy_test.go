// Copyright 2026 Intel Corporation. All Rights Reserved.
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
	"strconv"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
)

// A refused reconfiguration leaves the configuration in effect as it was.
func TestReconfigureRefusedKeepsConfig(t *testing.T) {
	tcs := []struct {
		name string
		// grant gives a container every sharable CPU before reconfiguring.
		grant   bool
		cfg     *cfgapi.Config
		refusal string
	}{
		{
			name:    "refused by initialize",
			cfg:     &cfgapi.Config{DefaultCPUPriority: cfgapi.PriorityLow},
			refusal: "without CPU reservation",
		},
		{
			name:  "refused by restoring the allocations",
			grant: true,
			cfg: &cfgapi.Config{
				ReservedResources:  cfgapi.Constraints{cfgapi.CPU: "8"},
				DefaultCPUPriority: cfgapi.PriorityLow,
			},
			refusal: "failed to allocate",
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			p := setupTestPolicy(t)

			if tc.grant {
				cpus := resource.MustParse(strconv.Itoa(p.root.GetSupply().SharableCPUs().Size()))
				c := &mockContainer{
					name:                "c",
					returnValueForGetID: "c",
					returnValueForGetResourceRequirements: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: cpus},
						Limits:   v1.ResourceList{v1.ResourceCPU: cpus},
					},
				}
				grant, err := p.allocatePool(c, "")
				if err != nil {
					t.Fatalf("failed to allocate a grant: %v", err)
				}
				p.applyGrant(grant)
			}

			cfg, prio := opt, defaultPrio

			err := p.Reconfigure(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("got error %v reconfiguring, expected a refusal with %q", err, tc.refusal)
			}

			if opt != cfg || p.cfg != cfg {
				t.Errorf("configuration is %+v after a refused reconfiguration, expected %+v", opt, cfg)
			}
			if defaultPrio != prio {
				t.Errorf("default CPU priority is %s after a refused reconfiguration, expected %s", defaultPrio, prio)
			}
		})
	}
}

// unreservedLeaf returns a leaf pool with more than one sharable CPU and no
// reserved ones.
func unreservedLeaf(t *testing.T, p *policy) Node {
	t.Helper()
	var leaf Node
	for _, n := range p.pools {
		s := n.GetSupply()
		if n.IsLeafNode() && s.ReservedCPUs().IsEmpty() && s.SharableCPUs().Size() > 1 {
			leaf = n
		}
	}
	if leaf == nil {
		t.Fatal("no leaf pool without reserved CPUs")
	}
	return leaf
}

// cpuContainer returns a guaranteed container asking for cpu CPUs.
func cpuContainer(cpu string) *mockContainer {
	q := resource.MustParse(cpu)
	return &mockContainer{
		name:                "c",
		returnValueForGetID: "c",
		returnValueForGetResourceRequirements: v1.ResourceRequirements{
			Requests: v1.ResourceList{v1.ResourceCPU: q},
			Limits:   v1.ResourceList{v1.ResourceCPU: q},
		},
	}
}

// A reconfiguration that leaves no CPUs in the pool of a shared grant moves
// the container to a pool with room instead of failing.
func TestReconfigureMovesGrantOffExcludedPool(t *testing.T) {
	p := setupTestPolicy(t)
	p.options.Owner = &testOwner{updated: make(chan bool, 1)}

	leaf := unreservedLeaf(t, p)
	excluded := leaf.GetSupply().SharableCPUs()

	c := cpuContainer("1500m")
	grant, err := p.allocatePool(c, leaf.Name())
	if err != nil {
		t.Fatalf("failed to allocate a grant from %s: %v", leaf.Name(), err)
	}
	p.applyGrant(grant)

	err = p.Reconfigure(&cfgapi.Config{
		ReservedResources:  cfgapi.Constraints{cfgapi.CPU: "750m"},
		AvailableResources: cfgapi.Constraints{cfgapi.CPU: cfgapi.Amount("exclude-cpuset:" + excluded.String())},
	})
	if err != nil {
		t.Fatalf("failed to reconfigure: %v", err)
	}

	g, ok := p.allocations.grants["c"]
	if !ok {
		t.Fatal("container lost its grant")
	}
	if g.GetCPUNode().Name() == leaf.Name() {
		t.Errorf("grant still in %s, which has no CPUs left", leaf.Name())
	}
	if cpus := g.SharedCPUs(); !cpus.Intersection(excluded).IsEmpty() {
		t.Errorf("grant has excluded CPUs %s", cpus.Intersection(excluded))
	}
}

// A container resized beyond the CPUs of its pool moves to a pool with room
// instead of failing the update.
func TestUpdateResourcesMovesGrantOffFullPool(t *testing.T) {
	p := setupTestPolicy(t)
	p.options.Owner = &testOwner{updated: make(chan bool, 1)}

	leaf := unreservedLeaf(t, p)
	size := leaf.GetSupply().SharableCPUs().Size()

	c := cpuContainer("1")
	grant, err := p.allocatePool(c, leaf.Name())
	if err != nil {
		t.Fatalf("failed to allocate a grant from %s: %v", leaf.Name(), err)
	}
	p.applyGrant(grant)

	c.returnValueForGetResourceRequirements = cpuContainer(strconv.Itoa(size + 1)).returnValueForGetResourceRequirements
	if err := p.UpdateResources(c); err != nil {
		t.Fatalf("failed to update resources: %v", err)
	}

	g, ok := p.allocations.grants["c"]
	if !ok {
		t.Fatal("container lost its grant")
	}
	if g.GetCPUNode().Name() == leaf.Name() {
		t.Errorf("grant still in %s, which has only %d CPUs", leaf.Name(), size)
	}
	if n := g.ExclusiveCPUs().Size(); n != size+1 {
		t.Errorf("grant has %d exclusive CPUs, expected %d", n, size+1)
	}
}
