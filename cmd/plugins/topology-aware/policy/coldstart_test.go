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
	"sync"
	"testing"
	"time"

	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	resourceapi "k8s.io/api/resource/v1"
)

// testOwner reports whether UpdateContainers was called with the lock held.
type testOwner struct {
	sync.Mutex
	updated chan bool
}

func (o *testOwner) UpdateContainers() error {
	locked := !o.TryLock()
	if !locked {
		o.Unlock()
	}
	o.updated <- locked
	return nil
}

func (*testOwner) PublishDRADevices([]resourceapi.Device) error { return nil }

func TestColdStart(t *testing.T) {
	// With cold start the container first gets PMEM only. DRAM is added
	// when the cold start timer fires.
	p := setupTestPolicy(t)

	owner := &testOwner{updated: make(chan bool, 1)}
	p.options.Owner = owner

	ctr := &mockContainer{
		name:                "coldstart",
		returnValueForGetID: "1234",
		pod: &mockPod{
			annotations: map[string]string{
				preferMemoryTypeKey: "dram,pmem",
				preferColdStartKey:  "duration: 10ms",
			},
		},
	}

	g, err := p.allocatePool(ctr, "")
	if err != nil {
		t.Fatalf("failed to allocate pool: %v", err)
	}
	if typ := p.memZoneType(g.GetMemoryZone()); typ != libmem.TypeMaskPMEM {
		t.Fatalf("expected PMEM before cold start, got %s", typ)
	}

	// The resource manager holds the lock while the policy handles StartContainer.
	owner.Lock()
	err = p.ContainerStarted(ctr)
	owner.Unlock()
	if err != nil {
		t.Fatalf("failed to start container: %v", err)
	}

	select {
	case locked := <-owner.updated:
		if !locked {
			t.Errorf("containers updated without the lock held")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cold start did not finish")
	}

	owner.Lock()
	defer owner.Unlock()
	if typ := p.memZoneType(g.GetMemoryZone()); typ != libmem.TypeMaskDRAM|libmem.TypeMaskPMEM {
		t.Errorf("expected DRAM and PMEM after cold start, got %s", typ)
	}
}
