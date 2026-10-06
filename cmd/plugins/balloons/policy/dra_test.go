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

package balloons

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/balloons"
	resmgr "github.com/containers/nri-plugins/pkg/apis/resmgr/v1alpha1"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// draTestContainer is a container with CDI devices and annotations only.
type draTestContainer struct {
	cache.Container
	cdiDevices  []string
	annotations map[string]string
}

func (c *draTestContainer) GetCDIDevices() []string {
	return c.cdiDevices
}

func (c *draTestContainer) GetEffectiveAnnotation(key string) (string, bool) {
	value, ok := c.annotations[key]
	return value, ok
}

func (c *draTestContainer) PrettyName() string {
	return "ns/pod/ctr"
}

func (c *draTestContainer) GetPodID() string {
	return "pod"
}

// draTestCDIDevice returns the CDI device name written for a claim result.
func draTestCDIDevice(driver, uid string, index int) string {
	return driver + "/device=claim-" + uid + "-" + strconv.Itoa(index)
}

// newDRATestPolicy returns a policy with a DRA balloon type "fast" of two
// instances and a regular balloon type "slow".
func newDRATestPolicy() *balloons {
	fast := &BalloonDef{Name: "fast", MinBalloons: 2, MaxCpus: 4, DRA: &cfgapi.BalloonDRA{}}
	slow := &BalloonDef{Name: "slow", MinBalloons: 1}
	return &balloons{
		bpoptions: &BalloonsOptions{BalloonDefs: []*BalloonDef{fast, slow}},
		draDriver: dra.DriverName(PolicyName),
		draClaims: draClaimRecords{},
		balloons: []*Balloon{
			{Def: fast, Instance: 0},
			{Def: fast, Instance: 1},
			{Def: slow, Instance: 0},
		},
	}
}

func TestDRADeviceName(t *testing.T) {
	tcases := []struct {
		template string
		instance int
		expected string
	}{
		{"", 0, "fast-0"},
		{"", 12, "fast-12"},
		{"cpus-${instance}", 3, "cpus-3"},
		{"${balloonType}x${balloonType}-${instance}", 1, "fastxfast-1"},
	}
	for _, tc := range tcases {
		blnDef := &BalloonDef{Name: "fast", DRA: &cfgapi.BalloonDRA{DeviceName: tc.template}}
		if got := draDeviceName(blnDef, tc.instance); got != tc.expected {
			t.Errorf("draDeviceName(%q, %d) = %q, expected %q", tc.template, tc.instance, got, tc.expected)
		}
	}
}

func TestDRADevices(t *testing.T) {
	p := newDRATestPolicy()
	unmapped := &BalloonDef{Name: "unmapped", MinBalloons: 1, MaxCpus: 2,
		DRA: &cfgapi.BalloonDRA{NodeAllocatable: ptr.To(false)}}
	p.bpoptions.BalloonDefs = append(p.bpoptions.BalloonDefs, unmapped)
	p.balloons = append(p.balloons, &Balloon{Def: unmapped, Instance: 0})
	devices := p.draDevices()
	names := []string{}
	for _, dev := range devices {
		names = append(names, dev.Name)
	}
	if !slices.Equal(names, []string{"fast-0", "fast-1", "unmapped-0"}) {
		t.Fatalf("unexpected devices %v", names)
	}
	dev := devices[1]
	if dev.AllowMultipleAllocations == nil || !*dev.AllowMultipleAllocations {
		t.Errorf("device %s is not shareable", dev.Name)
	}
	if v := dev.Attributes[draAttrBalloonType].StringValue; v == nil || *v != "fast" {
		t.Errorf("device %s has unexpected balloonType %v", dev.Name, v)
	}
	if v := dev.Attributes[draAttrInstance].IntValue; v == nil || *v != 1 {
		t.Errorf("device %s has unexpected instance %v", dev.Name, v)
	}
	if q := dev.Capacity[draCapacityCPU].Value; q.Value() != 4 {
		t.Errorf("device %s has cpu capacity %s, expected 4", dev.Name, q.String())
	}
	m := dev.NodeAllocatableResources["cpu"].Mapping
	if m == nil || m.CapacityKey == nil || *m.CapacityKey != draCapacityCPU ||
		m.CapacityMultiplier == nil || m.CapacityMultiplier.Value() != 1 {
		t.Errorf("device %s has unexpected node allocatable mapping %+v", dev.Name, m)
	}
	if dev := devices[2]; dev.NodeAllocatableResources != nil {
		t.Errorf("device %s has node allocatable resources %+v, expected none", dev.Name, dev.NodeAllocatableResources)
	}
}

func TestDRAClaimEdits(t *testing.T) {
	rec := &draClaim{Devices: []draClaimDevice{{Device: "fast-0"}, {Device: "fast-1"}}}
	edits := draClaimEdits(rec)
	if len(edits) != 2 {
		t.Fatalf("got %d edits, expected 2", len(edits))
	}
	for i, expected := range []string{"DRA_BALLOON=fast-0", "DRA_BALLOON=fast-1"} {
		if !slices.Equal(edits[i].Env, []string{expected}) {
			t.Errorf("edit %d has env %v, expected [%s]", i, edits[i].Env, expected)
		}
	}
}

func TestAllocateClaimWithoutResize(t *testing.T) {
	p := newDRATestPolicy()
	p.draClaims["uid1"] = &draClaim{Devices: []draClaimDevice{
		{Device: "fast-1", BalloonType: "fast", Instance: 1, MilliCPUs: 2000},
	}}
	claim := &resourceapi.ResourceClaim{}

	claim.UID = "uid1"
	edits, err := p.AllocateClaim(claim, []resourceapi.DeviceRequestAllocationResult{{Device: "fast-1"}})
	if err != nil || len(edits) != 1 || edits[0].Env[0] != "DRA_BALLOON=fast-1" {
		t.Errorf("re-prepare returned %v, %v", edits, err)
	}
	if _, err = p.AllocateClaim(claim, nil); err == nil {
		t.Errorf("re-prepare with a different number of results succeeded")
	}

	claim.UID = "uid2"
	_, err = p.AllocateClaim(claim, []resourceapi.DeviceRequestAllocationResult{{Device: "slow-0"}})
	if err == nil || !strings.Contains(err.Error(), "is not a balloon published by this node") {
		t.Errorf("claim on an unpublished device returned %v", err)
	}
	if _, ok := p.draClaims["uid2"]; ok {
		t.Errorf("failed claim was recorded")
	}

	if err := p.ReleaseClaim(types.UID("unknown")); err != nil {
		t.Errorf("releasing an unknown claim failed: %v", err)
	}
}

func TestDRABalloonForContainer(t *testing.T) {
	p := newDRATestPolicy()
	p.draClaims["uid1"] = &draClaim{Devices: []draClaimDevice{
		{Device: "fast-0", BalloonType: "fast", Instance: 0, MilliCPUs: 1000},
	}}
	p.draClaims["uid2"] = &draClaim{Devices: []draClaimDevice{
		{Device: "fast-0", BalloonType: "fast", Instance: 0, MilliCPUs: 500},
	}}
	p.draClaims["uid3"] = &draClaim{Devices: []draClaimDevice{
		{Device: "fast-1", BalloonType: "fast", Instance: 1, MilliCPUs: 1000},
	}}
	driver := p.draDriver

	tcases := []struct {
		name        string
		cdiDevices  []string
		annotations map[string]string
		balloon     string // expected balloon, empty if no claims
		milliCpus   int    // expected claimed CPUs of the container
		errFragment string // expected error, empty if none
	}{
		{
			name:       "no CDI devices",
			cdiDevices: nil,
		},
		{
			name:       "CDI devices of other drivers",
			cdiDevices: []string{draTestCDIDevice("gpu.example.com", "uid1", 0), "vendor.com/gpu=gpu0"},
		},
		{
			name:       "one claim",
			cdiDevices: []string{draTestCDIDevice(driver, "uid1", 0)},
			balloon:    "fast[0]",
			milliCpus:  1000,
		},
		{
			name:       "two claims on the same balloon",
			cdiDevices: []string{draTestCDIDevice(driver, "uid2", 0), draTestCDIDevice(driver, "uid1", 0)},
			balloon:    "fast[0]",
			milliCpus:  1500,
		},
		{
			name:        "matching annotation",
			cdiDevices:  []string{draTestCDIDevice(driver, "uid3", 0)},
			annotations: map[string]string{balloonKey: "fast"},
			balloon:     "fast[1]",
			milliCpus:   1000,
		},
		{
			name:        "claims on two balloons",
			cdiDevices:  []string{draTestCDIDevice(driver, "uid1", 0), draTestCDIDevice(driver, "uid3", 0)},
			errFragment: "requests DRA devices of more than one balloon",
		},
		{
			name:        "unprepared claim",
			cdiDevices:  []string{draTestCDIDevice(driver, "uid9", 0)},
			errFragment: "that this policy has not prepared",
		},
		{
			name:        "contradicting annotation",
			cdiDevices:  []string{draTestCDIDevice(driver, "uid1", 0)},
			annotations: map[string]string{balloonKey: "slow"},
			errFragment: "requests balloon type",
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			c := &draTestContainer{cdiDevices: tc.cdiDevices, annotations: tc.annotations}
			uids := p.draContainerClaims(c)
			if len(uids) == 0 {
				if tc.balloon != "" || tc.errFragment != "" {
					t.Fatalf("no claims found from CDI devices %v", tc.cdiDevices)
				}
				return
			}
			bln, err := p.draBalloonForContainer(c, uids)
			switch {
			case tc.errFragment != "":
				if err == nil || !strings.Contains(err.Error(), tc.errFragment) {
					t.Errorf("expected error containing %q, got %v", tc.errFragment, err)
				}
			case err != nil:
				t.Errorf("unexpected error: %v", err)
			case bln.PrettyName() != tc.balloon:
				t.Errorf("got balloon %s, expected %s", bln.PrettyName(), tc.balloon)
			case p.draContainerMilliCpus(c, bln) != tc.milliCpus:
				t.Errorf("draContainerMilliCpus() = %d, expected %d", p.draContainerMilliCpus(c, bln), tc.milliCpus)
			}
		})
	}
}

func TestDRAClaimedMilliCpus(t *testing.T) {
	p := newDRATestPolicy()
	p.draClaims["uid1"] = &draClaim{Devices: []draClaimDevice{
		{Device: "fast-0", BalloonType: "fast", Instance: 0, MilliCPUs: 1000},
		{Device: "fast-1", BalloonType: "fast", Instance: 1, MilliCPUs: 3000},
	}}
	p.draClaims["uid2"] = &draClaim{Devices: []draClaimDevice{
		{Device: "fast-0", BalloonType: "fast", Instance: 0, MilliCPUs: 1500},
	}}
	for i, expected := range []int{2500, 3000, 0} {
		if got := p.draClaimedMilliCpus(p.balloons[i]); got != expected {
			t.Errorf("draClaimedMilliCpus(%s) = %d, expected %d", p.balloons[i].PrettyName(), got, expected)
		}
	}
}

func TestValidateDRAConfig(t *testing.T) {
	fast := func(modify func(*BalloonDef)) *BalloonDef {
		blnDef := &BalloonDef{Name: "fast", MinBalloons: 2, MaxCpus: 4, DRA: &cfgapi.BalloonDRA{}}
		if modify != nil {
			modify(blnDef)
		}
		return blnDef
	}
	claimOn := func(defName string, instance, milliCpus int) draClaimRecords {
		return draClaimRecords{"uid1": &draClaim{Devices: []draClaimDevice{
			{Device: "dev", BalloonType: defName, Instance: instance, MilliCPUs: milliCpus},
		}}}
	}
	tcases := []struct {
		name        string
		blnDefs     []*BalloonDef
		claims      draClaimRecords
		errFragment string
	}{
		{
			name:    "valid",
			blnDefs: []*BalloonDef{fast(nil), {Name: "slow"}},
			claims:  claimOn("fast", 1, 4000),
		},
		{
			name:        "built-in type",
			blnDefs:     []*BalloonDef{fast(func(d *BalloonDef) { d.Name = defaultBalloonDefName })},
			errFragment: "built-in",
		},
		{
			name:        "no maxCPUs",
			blnDefs:     []*BalloonDef{fast(func(d *BalloonDef) { d.MaxCpus = 0 })},
			errFragment: "maxCPUs",
		},
		{
			name:        "no minBalloons",
			blnDefs:     []*BalloonDef{fast(func(d *BalloonDef) { d.MinBalloons = 0 })},
			errFragment: "minBalloons",
		},
		{
			name:        "namespaces",
			blnDefs:     []*BalloonDef{fast(func(d *BalloonDef) { d.Namespaces = []string{"*"} })},
			errFragment: "namespaces",
		},
		{
			name: "matchExpressions",
			blnDefs: []*BalloonDef{fast(func(d *BalloonDef) {
				d.MatchExpressions = []resmgr.Expression{{Key: "name", Op: resmgr.Equals, Values: []string{"x"}}}
			})},
			errFragment: "matchExpressions",
		},
		{
			name: "component",
			blnDefs: []*BalloonDef{fast(nil), {
				Name:       "composite",
				Components: []cfgapi.BalloonDefComponent{{DefName: "fast"}},
			}},
			errFragment: "cannot be a component",
		},
		{
			name:        "invalid device name",
			blnDefs:     []*BalloonDef{fast(func(d *BalloonDef) { d.DRA.DeviceName = "Fast_${instance}" })},
			errFragment: "not a DNS label",
		},
		{
			name: "duplicate device name",
			blnDefs: []*BalloonDef{
				fast(func(d *BalloonDef) { d.DRA.DeviceName = "cpu-${instance}" }),
				fast(func(d *BalloonDef) { d.Name = "faster"; d.DRA.DeviceName = "cpu-${instance}" }),
			},
			errFragment: "already used",
		},
		{
			name:        "constant device name",
			blnDefs:     []*BalloonDef{fast(func(d *BalloonDef) { d.DRA.DeviceName = "cpu" })},
			errFragment: "already used",
		},
		{
			name:        "claimed balloon type removed",
			blnDefs:     []*BalloonDef{{Name: "slow"}},
			claims:      claimOn("fast", 0, 1000),
			errFragment: "does not provide",
		},
		{
			name:        "claimed balloon type without dra",
			blnDefs:     []*BalloonDef{fast(func(d *BalloonDef) { d.DRA = nil })},
			claims:      claimOn("fast", 0, 1000),
			errFragment: "does not provide",
		},
		{
			name:        "claimed balloon instance removed",
			blnDefs:     []*BalloonDef{fast(func(d *BalloonDef) { d.MinBalloons = 1 })},
			claims:      claimOn("fast", 1, 1000),
			errFragment: "does not provide",
		},
		{
			name:        "claims exceed maxCPUs",
			blnDefs:     []*BalloonDef{fast(func(d *BalloonDef) { d.MaxCpus = 2 })},
			claims:      claimOn("fast", 1, 3000),
			errFragment: "maxCPUs is 2",
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			p := &balloons{draClaims: tc.claims}
			err := p.validateDRAConfig(&BalloonsOptions{BalloonDefs: tc.blnDefs})
			switch {
			case tc.errFragment == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.errFragment != "" && (err == nil || !strings.Contains(err.Error(), tc.errFragment)):
				t.Errorf("expected error containing %q, got %v", tc.errFragment, err)
			}
		})
	}
}

func TestDRAClaimsPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	cch, err := cache.NewCache(cache.Options{CacheDir: dir})
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}
	if claims := loadDRAClaims(cch); len(claims) != 0 {
		t.Fatalf("got claims %v from an empty cache", claims)
	}

	p := &balloons{cch: cch, draClaims: draClaimRecords{
		"uid1": &draClaim{Devices: []draClaimDevice{
			{Device: "fast-1", BalloonType: "fast", Instance: 1, MilliCPUs: 2500},
		}},
	}}
	p.saveDRAClaims()
	if claims := loadDRAClaims(cch); len(claims) != 1 || claims["uid1"].Devices[0].MilliCPUs != 2500 {
		t.Errorf("got claims %v from the cache, expected the saved ones", claims)
	}
	if err := cch.Save(); err != nil {
		t.Fatalf("failed to save cache: %v", err)
	}

	restored, err := cache.NewCache(cache.Options{CacheDir: dir})
	if err != nil {
		t.Fatalf("failed to load cache: %v", err)
	}
	claims := loadDRAClaims(restored)
	if len(claims) != 1 || claims["uid1"].Devices[0] != p.draClaims["uid1"].Devices[0] {
		t.Errorf("got claims %v from a restored cache, expected the saved ones", claims)
	}
	restored.ResetPolicyEntries()
	if claims := loadDRAClaims(restored); len(claims) != 0 {
		t.Errorf("got claims %v after reset", claims)
	}
}
