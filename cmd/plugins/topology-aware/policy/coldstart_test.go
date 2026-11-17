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
	"reflect"
	"sync"
	"testing"
	"time"

	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// testOwner reports whether UpdateContainers was called with the lock held.
// It keeps the DRA devices published last.
type testOwner struct {
	sync.Mutex
	updated chan bool
	devices []resourceapi.Device
}

func (o *testOwner) UpdateContainers() error {
	locked := !o.TryLock()
	if !locked {
		o.Unlock()
	}
	o.updated <- locked
	return nil
}

func (*testOwner) DRAEnabled() bool                 { return true }
func (*testOwner) DRAListAttributesSupported() bool { return false }

func (o *testOwner) PublishDRADevices(devices []resourceapi.Device) error {
	o.devices = devices
	return nil
}

func TestColdStart(t *testing.T) {
	// With cold start the container first gets PMEM only. DRAM is added
	// when the cold start timer fires. The policy then publishes the new
	// memory device capacities.
	p := setupTestPolicy(t)

	owner := &testOwner{updated: make(chan bool, 1)}
	p.options.Owner = owner

	ctr := &mockContainer{
		name:                "coldstart",
		returnValueForGetID: "1234",
		returnValueForGetResourceRequirements: v1.ResourceRequirements{
			Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("1Gi")},
			Limits:   v1.ResourceList{v1.ResourceMemory: resource.MustParse("1Gi")},
		},
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
	p.publishDRADevices()
	before := draMemDeviceSummary(owner.devices)

	// The resource manager holds the lock while the policy handles StartContainer.
	owner.Lock()
	err = p.ContainerStarted(ctr)
	owner.Unlock()
	if err != nil {
		t.Fatalf("failed to start container: %v", err)
	}

	select {
	case locked := <-owner.updated:
		if locked {
			t.Errorf("containers updated with the lock held")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cold start did not finish")
	}

	owner.Lock()
	defer owner.Unlock()
	if typ := p.memZoneType(g.GetMemoryZone()); typ != libmem.TypeMaskDRAM|libmem.TypeMaskPMEM {
		t.Errorf("expected DRAM and PMEM after cold start, got %s", typ)
	}
	after := draMemDeviceSummary(p.draDevices())
	if reflect.DeepEqual(after, before) {
		t.Errorf("memory devices %v did not change in cold start", after)
	}
	if got := draMemDeviceSummary(owner.devices); !reflect.DeepEqual(got, after) {
		t.Errorf("published memory devices %v after cold start, expected %v", got, after)
	}
}
