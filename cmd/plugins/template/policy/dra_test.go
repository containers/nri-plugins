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

package template

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/template"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	specs "tags.cncf.io/container-device-interface/specs-go"
)

// testSystem has CPUs 0-7, all of them online.
type testSystem struct {
	system.System
}

func (testSystem) CPUSet() cpuset.CPUSet     { return cpuset.MustParse("0-7") }
func (testSystem) OnlineCPUs() cpuset.CPUSet { return cpuset.MustParse("0-7") }

// testOwner keeps the DRA devices published last.
type testOwner struct {
	devices []resourceapi.Device
}

func (o *testOwner) PublishDRADevices(devices []resourceapi.Device) error {
	o.devices = devices
	return nil
}

// testConfig is a configuration with the given available and reserved CPUs.
func testConfig(available, reserved string) *cfgapi.Config {
	cfg := &cfgapi.Config{
		AvailableResources: cfgapi.Constraints{},
		ReservedResources:  cfgapi.Constraints{cfgapi.CPU: cfgapi.Amount(reserved)},
	}
	if available != "" {
		cfg.AvailableResources[cfgapi.CPU] = cfgapi.Amount(available)
	}
	return cfg
}

// testPolicy is a started policy with its cache under dir. The cache itself
// is a directory of its own, as the cache refuses one others may write to,
// which is what a temporary directory is.
func testPolicy(t *testing.T, dir string, cfg *cfgapi.Config) *policy {
	t.Helper()
	cch, err := cache.NewCache(cache.Options{CacheDir: filepath.Join(dir, "cache")})
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}
	p := New().(*policy)
	err = p.Setup(&policyapi.BackendOptions{
		System: testSystem{},
		Cache:  cch,
		Owner:  &testOwner{},
		Config: cfg,
	})
	if err != nil {
		t.Fatalf("failed to set up policy: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("failed to start policy: %v", err)
	}
	return p
}

// testClaim is a claim with one result per given amount of consumed CPUs.
// An empty amount consumes nothing.
func testClaim(uid types.UID, amounts ...string) (*resourceapi.ResourceClaim, []resourceapi.DeviceRequestAllocationResult) {
	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: uid}}
	results := make([]resourceapi.DeviceRequestAllocationResult, len(amounts))
	for i, amount := range amounts {
		results[i] = resourceapi.DeviceRequestAllocationResult{
			Request: fmt.Sprintf("req-%d", i),
			Driver:  "template.nri.io",
			Device:  draDevice,
		}
		if amount != "" {
			results[i].ConsumedCapacity = map[resourceapi.QualifiedName]resource.Quantity{
				draCapacity: resource.MustParse(amount),
			}
		}
	}
	return claim, results
}

// allocate allocates a claim, failing the test on an error.
func allocate(t *testing.T, p *policy, uid types.UID, amounts ...string) []specs.ContainerEdits {
	t.Helper()
	edits, err := p.AllocateClaim(testClaim(uid, amounts...))
	if err != nil {
		t.Fatalf("failed to allocate claim %s: %v", uid, err)
	}
	return edits
}

// cpuEdits are the edits of results holding the given CPUs.
func cpuEdits(cpus ...[]int) []specs.ContainerEdits {
	edits := make([]specs.ContainerEdits, len(cpus))
	for i, list := range cpus {
		for _, cpu := range list {
			edits[i].Env = append(edits[i].Env, fmt.Sprintf("NRI_TEMPLATE_CPU%d=claimed", cpu))
		}
	}
	return edits
}

func checkEdits(t *testing.T, got, want []specs.ContainerEdits) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got edits %v, want %v", got, want)
	}
}

// TestDRADevices verifies the device we publish when we start. A configuration
// DRA cannot use publishes no devices, without failing the start.
func TestDRADevices(t *testing.T) {
	one := resource.MustParse("1")
	policy := &resourceapi.CapacityRequestPolicy{
		Default:    &one,
		ValidRange: &resourceapi.CapacityRequestPolicyRange{Min: &one, Step: &one},
	}

	for _, tc := range []struct {
		name      string
		available string
		reserved  string
		cpus      int64
		fail      bool
	}{
		{name: "default", reserved: "750m", cpus: 8},
		{name: "reserved cpuset", reserved: "cpuset:0-1", cpus: 6},
		{name: "available cpuset", available: "cpuset:2-5,9", reserved: "cpuset:5", cpus: 3},
		{name: "available quantity", available: "2", reserved: "750m", fail: true},
		{name: "available exclude-cpuset", available: "exclude-cpuset:0", reserved: "750m", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPolicy(t, t.TempDir(), testConfig(tc.available, tc.reserved))
			devices := p.owner.(*testOwner).devices
			if tc.fail {
				if len(devices) != 0 {
					t.Fatalf("got devices %v, want none", devices)
				}
				return
			}

			if len(devices) != 1 {
				t.Fatalf("got %d devices, want 1", len(devices))
			}
			d := devices[0]
			if d.Name != draDevice || d.AllowMultipleAllocations == nil || !*d.AllowMultipleAllocations {
				t.Errorf("got device %s allowing multiple allocations %v", d.Name, d.AllowMultipleAllocations)
			}
			capacity := d.Capacity[draCapacity]
			if capacity.Value.Value() != tc.cpus {
				t.Errorf("got capacity %s, want %d", capacity.Value.String(), tc.cpus)
			}
			if !reflect.DeepEqual(capacity.RequestPolicy, policy) {
				t.Errorf("got request policy %v, want %v", capacity.RequestPolicy, policy)
			}
		})
	}
}

func TestAllocateClaim(t *testing.T) {
	p := testPolicy(t, t.TempDir(), testConfig("", "750m"))

	checkEdits(t, allocate(t, p, "a", "2"), cpuEdits([]int{0, 1}))
	checkEdits(t, allocate(t, p, "b", "1", "2"), cpuEdits([]int{2}, []int{3, 4}))

	// Preparing a claim again allocates nothing further.
	checkEdits(t, allocate(t, p, "a", "2"), cpuEdits([]int{0, 1}))
	checkEdits(t, allocate(t, p, "b", "1", "2"), cpuEdits([]int{2}, []int{3, 4}))
	checkEdits(t, allocate(t, p, "c", "3"), cpuEdits([]int{5, 6, 7}))

	if _, err := p.AllocateClaim(testClaim("d", "1")); err == nil {
		t.Errorf("allocated a claim with no CPUs left")
	}
}

// TestAllocateClaimQualified checks that a result naming our capacity with
// our driver name as its domain is read too, and one with another domain not.
func TestAllocateClaimQualified(t *testing.T) {
	p := testPolicy(t, t.TempDir(), testConfig("", "750m"))

	for name, want := range map[resourceapi.QualifiedName]bool{
		"template.nri.io/cpus": true,
		"other.nri.io/cpus":    false,
	} {
		claim, results := testClaim(types.UID(name), "")
		results[0].ConsumedCapacity = map[resourceapi.QualifiedName]resource.Quantity{
			name: resource.MustParse("1"),
		}
		_, err := p.AllocateClaim(claim, results)
		if got := err == nil; got != want {
			t.Errorf("allocating with %s: got error %v", name, err)
		}
	}
}

func TestAllocateClaimErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		amounts []string
	}{
		{name: "missing amount", amounts: []string{"1", ""}},
		{name: "zero amount", amounts: []string{"0"}},
		{name: "fractional amount", amounts: []string{"500m"}},
		{name: "too many CPUs", amounts: []string{"4", "5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPolicy(t, t.TempDir(), testConfig("", "750m"))
			if edits, err := p.AllocateClaim(testClaim("a", tc.amounts...)); err == nil {
				t.Fatalf("got edits %v, want an error", edits)
			}
			if len(p.claims) != 0 {
				t.Errorf("failed claim left CPUs claimed: %v", p.claims)
			}
		})
	}
}

func TestReleaseClaim(t *testing.T) {
	p := testPolicy(t, t.TempDir(), testConfig("", "750m"))

	allocate(t, p, "a", "2")
	allocate(t, p, "b", "2")
	if err := p.ReleaseClaim("a"); err != nil {
		t.Fatalf("failed to release claim: %v", err)
	}
	if err := p.ReleaseClaim("unknown"); err != nil {
		t.Errorf("failed to release an unknown claim: %v", err)
	}

	checkEdits(t, allocate(t, p, "c", "3"), cpuEdits([]int{0, 1, 4}))
}

func TestRestoreClaims(t *testing.T) {
	dir := t.TempDir()
	p := testPolicy(t, dir, testConfig("", "750m"))
	allocate(t, p, "a", "1")
	allocate(t, p, "b", "1", "2")
	if err := p.ReleaseClaim("a"); err != nil {
		t.Fatalf("failed to release claim: %v", err)
	}
	if err := p.cache.Save(); err != nil {
		t.Fatalf("failed to save cache: %v", err)
	}

	// Claim b holds CPUs 1-3, not the lowest free ones, so only a restored
	// claim gets them again.
	p = testPolicy(t, dir, testConfig("", "750m"))
	checkEdits(t, allocate(t, p, "b", "1", "2"), cpuEdits([]int{1}, []int{2, 3}))
	checkEdits(t, allocate(t, p, "c", "2"), cpuEdits([]int{0, 4}))
}

func TestReconfigure(t *testing.T) {
	p := testPolicy(t, t.TempDir(), testConfig("cpuset:0-3", "750m"))

	if err := p.Reconfigure(testConfig("cpuset:0-3", "750m")); err != nil {
		t.Errorf("failed to reconfigure with the same CPUs: %v", err)
	}
	if err := p.Reconfigure(testConfig("cpuset:0-4", "750m")); err == nil {
		t.Errorf("changed available CPUs at runtime")
	}
	if err := p.Reconfigure(testConfig("cpuset:0-3", "1")); err == nil {
		t.Errorf("changed reserved CPUs at runtime")
	}
}
