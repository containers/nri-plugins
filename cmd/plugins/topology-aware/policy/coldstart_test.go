// Copyright 2020 Intel Corporation. All Rights Reserved.
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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	idset "github.com/intel/goresctrl/pkg/utils"
)

var globalPolicy *policy
var mutex sync.Mutex

func sendEvent(param any) error {
	// Simulate event synchronization in the upper levels.
	mutex.Lock()
	defer mutex.Unlock()

	fmt.Printf("Event received: %v", param)
	event := param.(*events.Policy)
	if _, err := globalPolicy.HandleEvent(event); err != nil {
		log.Warnf("failed to handle test event: %v", err)
	}
	return nil
}

func TestColdStart(t *testing.T) {

	// Idea with cold start is that the workload is first allocated only PMEM node. Only when timer expires
	// (or some other event is triggered) is the DRAM node added to the memset. This causes the initial
	// memory allocations to be made from PMEM only.

	tcases := []struct {
		name                     string
		numaNodes                []synthNode
		req                      Request
		affinities               map[int]int32
		container                cache.Container
		expectedColdStartTimeout time.Duration
		expectedDRAMNodeID       int
		expectedPMEMNodeID       int
		expectedDRAMSystemNodeID idset.ID
		expectedPMEMSystemNodeID idset.ID
	}{
		{
			name: "three node cold start",
			// node0 has CPUs, so it is ordinary memory. node1 has none and is
			// larger, which is how a persistent memory node presents itself.
			numaNodes: []synthNode{
				{cpus: "0-1", memKB: 10000, distance: []int{10, 50}},
				{cpus: "", memKB: 50000, distance: []int{50, 10}},
			},
			container: &mockContainer{
				name:                "demo-coldstart-container",
				returnValueForGetID: "1234",
				pod: &mockPod{
					// The policy reads both preferences with
					// GetEffectiveAnnotation, so they have to be annotations
					// scoped to this container, in the form it parses them.
					annotations: map[string]string{
						preferMemoryTypeKey + "/container.demo-coldstart-container": "pmem,dram",
						preferColdStartKey + "/container.demo-coldstart-container":  "{ duration: 1s }",
					},
				},
			},
			expectedColdStartTimeout: 1000 * time.Millisecond,
			expectedDRAMNodeID:       101,
			expectedDRAMSystemNodeID: 0,
			expectedPMEMSystemNodeID: 1,
			expectedPMEMNodeID:       102,
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			m := synthMachine(t, tc.numaNodes)

			policy := &policy{
				machine: m,
				cache: &mockCache{
					returnValue1ForLookupContainer: tc.container,
					returnValue2ForLookupContainer: true,
				},
				allocations: allocations{
					grants: make(map[string]Grant, 0),
				},
				options:      &policyapi.BackendOptions{},
				cpuAllocator: &mockCPUAllocator{},
			}
			policy.allocations.policy = policy
			policy.options.SendEvent = sendEvent
			ma, err := libmem.NewAllocator(libmem.WithMachineNodes(m))
			if err != nil {
				t.Fatalf("failed to create memory allocator: %v", err)
			}
			policy.memAllocator = ma

			if err := policy.buildPoolsByTopology(); err != nil {
				t.Errorf("failed to build topology pool")
			}

			grant, err := policy.allocatePool(tc.container, "")
			if err != nil {
				panic(err)
			}
			if grant.ColdStart() != tc.expectedColdStartTimeout {
				t.Errorf("Expected coldstart value '%v', but got '%v'", tc.expectedColdStartTimeout, grant.ColdStart())
			}

			policy.allocations.addGrant(grant)

			mems := grant.GetMemoryZone()
			if mems.Size() != 1 || mems.Slice()[0] != tc.expectedPMEMSystemNodeID {
				t.Errorf("Expected one memory controller %v, got: %v", tc.expectedPMEMSystemNodeID, mems)
			}

			// FIXME: should we report only the limited memory types or the granted types
			// while the cold start is going on?
			//if grant.MemoryType()&memoryDRAM != 0 {
			//    t.Errorf("No DRAM was expected before coldstart timer: %v", grant.MemoryType())
			//}

			globalPolicy = policy

			if err := policy.options.SendEvent(&events.Policy{
				Type: events.ContainerStarted,
				Data: tc.container,
			}); err != nil {
				log.Warnf("failed to send test event: %v", err)
			}

			time.Sleep(tc.expectedColdStartTimeout * 2)

			newMems := grant.GetMemoryZone()
			if newMems.Size() != 2 {
				t.Errorf("Expected two memory controllers, got %d: %s", newMems.Size(), newMems)
			}
			if !newMems.Contains(tc.expectedPMEMSystemNodeID) || !newMems.Contains(tc.expectedDRAMSystemNodeID) {
				t.Errorf("Didn't get all expected system nodes in mems, got: %v", newMems)
			}
		})
	}
}
