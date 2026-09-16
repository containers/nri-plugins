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

package policy

import (
	"errors"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	specs "tags.cncf.io/container-device-interface/specs-go"
)

// draBackend is a backend which only implements the DRA methods. Embedding the
// interface leaves the rest nil, which is fine as long as nothing calls them.
type draBackend struct {
	Backend

	devices []resourceapi.Device
	edits   []specs.ContainerEdits
	err     error

	claim     *resourceapi.ResourceClaim
	results   []resourceapi.DeviceRequestAllocationResult
	releaseID types.UID
}

func (b *draBackend) DRADevices() ([]resourceapi.Device, error) {
	return b.devices, b.err
}

func (b *draBackend) AllocateClaim(
	claim *resourceapi.ResourceClaim,
	results []resourceapi.DeviceRequestAllocationResult,
) ([]specs.ContainerEdits, error) {
	b.claim, b.results = claim, results
	return b.edits, b.err
}

func (b *draBackend) ReleaseClaim(uid types.UID) error {
	b.releaseID = uid
	return b.err
}

func TestDRADevices(t *testing.T) {
	devices := []resourceapi.Device{{Name: "cpu-0"}}
	backend := &draBackend{devices: devices}
	p := &policy{active: backend}

	got, err := p.DRADevices()
	if err != nil {
		t.Fatalf("DRADevices() failed: %v", err)
	}
	if len(got) != 1 || got[0].Name != devices[0].Name {
		t.Errorf("DRADevices() returned %v, expected %v", got, devices)
	}

	backend.err = errors.New("misconfigured")
	if _, err := p.DRADevices(); !errors.Is(err, backend.err) {
		t.Errorf("DRADevices() returned error %v, expected %v", err, backend.err)
	}
}

func TestAllocateClaim(t *testing.T) {
	claim := &resourceapi.ResourceClaim{}
	results := []resourceapi.DeviceRequestAllocationResult{{Device: "cpu-0"}}
	edits := []specs.ContainerEdits{{Env: []string{"CPUS=0"}}}
	backend := &draBackend{edits: edits}
	p := &policy{active: backend}

	got, err := p.AllocateClaim(claim, results)
	if err != nil {
		t.Fatalf("AllocateClaim() failed: %v", err)
	}
	if backend.claim != claim {
		t.Errorf("AllocateClaim() passed claim %v, expected %v", backend.claim, claim)
	}
	if len(backend.results) != 1 || backend.results[0].Device != results[0].Device {
		t.Errorf("AllocateClaim() passed results %v, expected %v", backend.results, results)
	}
	if len(got) != 1 || len(got[0].Env) != 1 || got[0].Env[0] != edits[0].Env[0] {
		t.Errorf("AllocateClaim() returned edits %v, expected %v", got, edits)
	}

	backend.err = errors.New("out of CPUs")
	if _, err := p.AllocateClaim(claim, results); !errors.Is(err, backend.err) {
		t.Errorf("AllocateClaim() returned error %v, expected %v", err, backend.err)
	}
}

func TestReleaseClaim(t *testing.T) {
	backend := &draBackend{}
	p := &policy{active: backend}

	if err := p.ReleaseClaim(types.UID("claim-uid")); err != nil {
		t.Fatalf("ReleaseClaim() failed: %v", err)
	}
	if backend.releaseID != types.UID("claim-uid") {
		t.Errorf("ReleaseClaim() passed UID %q, expected %q", backend.releaseID, "claim-uid")
	}

	backend.err = errors.New("no such claim")
	if err := p.ReleaseClaim(types.UID("claim-uid")); !errors.Is(err, backend.err) {
		t.Errorf("ReleaseClaim() returned error %v, expected %v", err, backend.err)
	}
}
