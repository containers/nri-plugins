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

package config

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/containers/nri-plugins/pkg/cxl/device"
)

const (
	local = device.DriverName
	pool  = device.PoolDriverName
	head  = `"apiVersion": "cxl.nri.io/v1alpha1", "kind": "MemoryConfig"`
)

// cfg is an opaque config entry of an allocation.
type cfg struct {
	source   resourceapi.AllocationConfigSource
	driver   string
	requests []string
	params   string // without the braces; head is added unless raw
	raw      bool
}

// class returns a DeviceClass entry of cxl.nri.io with head and params.
func class(params string, requests ...string) cfg {
	return cfg{source: resourceapi.AllocationConfigSourceClass, driver: local, requests: requests, params: params}
}

// claimed returns a claim entry of cxl.nri.io with head and params.
func claimed(params string, requests ...string) cfg {
	return cfg{source: resourceapi.AllocationConfigSourceClaim, driver: local, requests: requests, params: params}
}

// of returns the entry for the driver.
func (c cfg) of(driver string) cfg {
	c.driver = driver
	return c
}

// rawParams returns the entry with params as the whole parameters.
func (c cfg) rawParams() cfg {
	c.raw = true
	return c
}

// testClaim returns the allocated claim ns/a with the config entries and
// the requests r (class cxl-local), d (class cxl-dram) and p with the
// subrequests local (class cxl-local) and pool (class cxl-pool).
func testClaim(entries ...cfg) *resourceapi.ResourceClaim {
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a"},
		Spec: resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{
			{Name: "r", Exactly: &resourceapi.ExactDeviceRequest{DeviceClassName: device.ClassLocal}},
			{Name: "d", Exactly: &resourceapi.ExactDeviceRequest{DeviceClassName: device.ClassDRAM}},
			{Name: "p", FirstAvailable: []resourceapi.DeviceSubRequest{
				{Name: "local", DeviceClassName: device.ClassLocal},
				{Name: "pool", DeviceClassName: device.ClassPool},
			}},
		}}},
		Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{}},
	}
	for _, e := range entries {
		params := e.params
		if !e.raw {
			params = "{" + head
			if e.params != "" {
				params += ", " + e.params
			}
			params += "}"
		}
		claim.Status.Allocation.Devices.Config = append(claim.Status.Allocation.Devices.Config, resourceapi.DeviceAllocationConfiguration{
			Source:   e.source,
			Requests: e.requests,
			DeviceConfiguration: resourceapi.DeviceConfiguration{Opaque: &resourceapi.OpaqueDeviceConfiguration{
				Driver: e.driver, Parameters: runtime.RawExtension{Raw: []byte(params)},
			}},
		})
	}
	return claim
}

const (
	originClaimR = "claim ns/a request r"
	originClassR = "class cxl-local"
)

func TestResolveMerge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []cfg
		request string
		driver  string
		want    string
		origins map[string]string
	}{
		{
			name: "nothing",
			want: `{}`,
		},
		{
			name:    "empty entry",
			entries: []cfg{claimed(``)},
			want:    `{}`,
		},
		{
			name:    "empty sections",
			entries: []cfg{claimed(`"device": {}, "container": {"steering": {"waypoints": []}}`)},
			want:    `{}`,
		},
		{
			name: "class then claim, fields merge",
			entries: []cfg{
				class(`"device": {"mode": "dax"}, "container": {"steering": {"policy": "auto"}}`, "r"),
				claimed(`"device": {"readOnly": true}, "container": {"steering": {"maxStep": "1Gi"}}`),
			},
			want:    `{"device":{"mode":"dax","readOnly":true},"container":{"steering":{"policy":"auto","maxStep":"1Gi"}}}`,
			origins: map[string]string{FieldPolicy: originClassR, FieldMaxStep: originClaimR},
		},
		{
			name: "class entries first whatever the list order",
			entries: []cfg{
				claimed(`"container": {"steering": {"policy": "none"}}`),
				class(`"container": {"steering": {"policy": "auto"}}`, "r"),
			},
			want:    `{"container":{"steering":{"policy":"none"}}}`,
			origins: map[string]string{FieldPolicy: originClaimR},
		},
		{
			name: "later claim entry wins",
			entries: []cfg{
				claimed(`"container": {"steering": {"policy": "first-cxl"}}`),
				claimed(`"container": {"steering": {"policy": "first-dram"}}`),
			},
			want:    `{"container":{"steering":{"policy":"first-dram"}}}`,
			origins: map[string]string{FieldPolicy: originClaimR},
		},
		{
			name: "null removes a field",
			entries: []cfg{
				class(`"container": {"steering": {"policy": "first-dram", "maxStep": "1Gi"}}`, "r"),
				claimed(`"container": {"steering": {"maxStep": null}}`),
			},
			want:    `{"container":{"steering":{"policy":"first-dram"}}}`,
			origins: map[string]string{FieldPolicy: originClassR},
		},
		{
			name: "null removes an object",
			entries: []cfg{
				class(`"device": {"mode": "dax"}, "container": {"steering": {"policy": "first-dram", "maxStep": "1Gi"}}`, "r"),
				claimed(`"container": {"steering": null}`),
			},
			want: `{"device":{"mode":"dax"}}`,
		},
		{
			name: "null then set again",
			entries: []cfg{
				class(`"container": {"steering": {"policy": "auto"}}`, "r"),
				claimed(`"container": null`),
				claimed(`"container": {"steering": {"policy": "auto"}}`),
			},
			want:    `{"container":{"steering":{"policy":"auto"}}}`,
			origins: map[string]string{FieldPolicy: originClaimR},
		},
		{
			name: "null of an absent field",
			entries: []cfg{
				claimed(`"device": {"mode": null}, "container": {"fencing": null}, "pool": null`),
			},
			want: `{}`,
		},
		{
			name: "lists replace",
			entries: []cfg{
				class(`"container": {"steering": {"policy": "waypoints", "waypoints": [
					{"targetUsages": [{"memoryType": "DRAM", "usage": "1Gi"}]},
					{"targetUsages": [{"memoryType": "CXL", "usage": "2Gi"}]}]}}`, "r"),
				claimed(`"container": {"steering": {"waypoints": [{"targetUsages": [{"memoryType": "CXL", "usage": "4Gi"}]}]}}`),
			},
			want:    `{"container":{"steering":{"policy":"waypoints","waypoints":[{"targetUsages":[{"memoryType":"CXL","usage":"4Gi"}]}]}}}`,
			origins: map[string]string{FieldPolicy: originClassR, FieldWaypoints: originClaimR},
		},
		{
			name: "booleans replace",
			entries: []cfg{
				class(`"device": {"mode": "dax", "readOnly": true}`, "r"),
				claimed(`"device": {"readOnly": false}`),
			},
			want: `{"device":{"mode":"dax","readOnly":false}}`,
		},
		{
			name: "readOnly false is kept",
			entries: []cfg{
				claimed(`"device": {"mode": "fs", "readOnly": false, "fsType": "ext4"}`),
			},
			want: `{"device":{"mode":"fs","readOnly":false,"fsType":"ext4"}}`,
		},
		{
			name: "null removes readOnly",
			entries: []cfg{
				class(`"device": {"mode": "fs", "readOnly": false, "fsType": "ext4"}`, "r"),
				claimed(`"device": {"readOnly": null}`),
			},
			want: `{"device":{"mode":"fs","fsType":"ext4"}}`,
		},
		{
			name: "null removes readOnly and the device section",
			entries: []cfg{
				class(`"device": {"readOnly": false}`, "r"),
				claimed(`"device": {"readOnly": null}`),
			},
			want: `{}`,
		},
		{
			name: "pool opaque merges per key",
			entries: []cfg{
				class(`"pool": {"opaque": {"a": {"slot": "x"}}}`, "r").of(pool),
				claimed(`"pool": {"opaque": {"b": {"numaNode": 1}}}`).of(pool),
				claimed(`"pool": {"opaque": {"a": {"numaNode": 0}}}`).of(pool),
			},
			driver: pool,
			want:   `{"pool":{"opaque":{"a":{"numaNode":0,"slot":"x"},"b":{"numaNode":1}}}}`,
		},
		{
			name: "pool opaque key removed",
			entries: []cfg{
				class(`"pool": {"opaque": {"a": {"slot": "x"}, "b": {"x": 1}}}`, "r").of(pool),
				claimed(`"pool": {"opaque": {"a": null}}`).of(pool),
			},
			driver: pool,
			want:   `{"pool":{"opaque":{"b":{"x":1}}}}`,
		},
		{
			name: "entries of other drivers are ignored",
			entries: []cfg{
				claimed(`"device": {"mode": "dax"}`).of(pool),
				claimed(`"device": {"mode": "numa"}`).of("gpu.example.com"),
				cfg{source: resourceapi.AllocationConfigSourceClaim, driver: local, raw: true, params: `not json`}.of("other.example.com"),
				claimed(`"container": {"steering": {"policy": "auto"}}`),
			},
			want:    `{"container":{"steering":{"policy":"auto"}}}`,
			origins: map[string]string{FieldPolicy: originClaimR},
		},
		{
			name: "origin of a subrequest class",
			entries: []cfg{
				class(`"container": {"steering": {"policy": "auto"}}`, "p/pool").of(pool),
				claimed(`"container": {"steering": {"minStep": "64Mi"}}`, "p").of(pool),
			},
			request: "p/pool",
			driver:  pool,
			want:    `{"container":{"steering":{"policy":"auto","minStep":"64Mi"}}}`,
			origins: map[string]string{FieldPolicy: "class cxl-pool", FieldMinStep: "claim ns/a request p/pool"},
		},
		{
			name:    "origin of an unknown class",
			entries: []cfg{class(`"container": {"steering": {"policy": "auto"}}`, "x")},
			request: "x",
			want:    `{"container":{"steering":{"policy":"auto"}}}`,
			origins: map[string]string{FieldPolicy: "class of request x"},
		},
		{
			name: "every field",
			entries: []cfg{claimed(`"device": {"mode": "fs", "readOnly": true, "access": "cached", "path": "/models"},
				"container": {"steering": {"policy": "waypoints", "minStep": "64Mi", "maxStep": "1Gi",
				"waypoints": [{"targetUsages": [{"memoryType": "DRAM", "usage": "1Gi"}, {"memoryType": "CXL", "usage": "0"}]}]}},
				"pool": {"opaque": {"fake-cxl-pool": {"numaNode": 1}}}`).of(pool)},
			driver: pool,
			want: `{"device":{"mode":"fs","readOnly":true,"access":"cached","path":"/models"},` +
				`"container":{"steering":{"policy":"waypoints","waypoints":[{"targetUsages":[{"memoryType":"DRAM","usage":"1Gi"},{"memoryType":"CXL","usage":"0"}]}],"minStep":"64Mi","maxStep":"1Gi"}},` +
				`"pool":{"opaque":{"fake-cxl-pool":{"numaNode":1}}}}`,
			origins: map[string]string{FieldPolicy: originClaimR, FieldWaypoints: originClaimR, FieldMinStep: originClaimR, FieldMaxStep: originClaimR},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, driver := tc.request, tc.driver
			if request == "" {
				request = "r"
			}
			if driver == "" {
				driver = local
			}
			res, err := Resolve(testClaim(tc.entries...), driver, request)
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Config.Compact(); got != tc.want {
				t.Errorf("got\n%s\nexpected\n%s", got, tc.want)
			}
			if res.Config.APIVersion != APIVersion || res.Config.Kind != Kind {
				t.Errorf("apiVersion %q kind %q", res.Config.APIVersion, res.Config.Kind)
			}
			if !reflect.DeepEqual(res.Origins, tc.origins) {
				t.Errorf("origins %v, expected %v", res.Origins, tc.origins)
			}
			s := res.Settings()
			if s.Container != res.Config.Container || !reflect.DeepEqual(s.Origins, res.Origins) {
				t.Errorf("settings %+v", s)
			}
		})
	}
}

func TestResolveTargeting(t *testing.T) {
	// every entry sets its own key of pool.opaque
	entry := func(source resourceapi.AllocationConfigSource, key string, requests ...string) cfg {
		return cfg{source: source, driver: pool, requests: requests, params: `"pool": {"opaque": {"` + key + `": {}}}`}
	}
	claim := testClaim(
		entry(resourceapi.AllocationConfigSourceClass, "class-r", "r"),
		entry(resourceapi.AllocationConfigSourceClass, "class-p/pool", "p/pool"),
		entry(resourceapi.AllocationConfigSourceClaim, "all"),
		entry(resourceapi.AllocationConfigSourceClaim, "r", "r"),
		entry(resourceapi.AllocationConfigSourceClaim, "p", "p"),
		entry(resourceapi.AllocationConfigSourceClaim, "p/local", "p/local"),
		entry(resourceapi.AllocationConfigSourceClaim, "p/pool", "p/pool"),
		entry(resourceapi.AllocationConfigSourceClaim, "r+p/pool", "r", "p/pool"),
		entry(resourceapi.AllocationConfigSourceClaim, "d", "d"),
	)
	for request, want := range map[string][]string{
		"r":       {"class-r", "all", "r", "r+p/pool"},
		"d":       {"all", "d"},
		"p":       {"all", "p"},
		"p/local": {"all", "p", "p/local"},
		"p/pool":  {"class-p/pool", "all", "p", "p/pool", "r+p/pool"},
		"x":       {"all"},
		"x/pool":  {"all"},
	} {
		res, err := Resolve(claim, pool, request)
		if err != nil {
			t.Fatalf("%s: %v", request, err)
		}
		var got []string
		if res.Config.Pool != nil {
			for key := range res.Config.Pool.Opaque {
				got = append(got, key)
			}
		}
		slices.Sort(got)
		want = slices.Clone(want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("request %s: entries %v, expected %v", request, got, want)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	const unsteered = `"container": {"steering": {"policy": "none"}}`
	for _, tc := range []struct {
		name    string
		entries []cfg
		driver  string
		err     string
	}{
		{
			name:    "unknown field, index among the entries of the source",
			entries: []cfg{claimed(``).of(pool), claimed(`"x": 1`)},
			err:     `MemoryConfig: FromClaim config 1: json: unknown field "x"`,
		},
		{
			name:    "unknown field of a class entry",
			entries: []cfg{claimed(``), class(`"device": {"x": true}`, "r")},
			err:     `MemoryConfig: FromClass config 0: json: unknown field "device.x"`,
		},
		{
			name:    "reserved device.online",
			entries: []cfg{claimed(`"device": {"online": "claimed"}`)},
			err:     `MemoryConfig: FromClaim config 0: json: unknown field "device.online"`,
		},
		{
			name:    "reserved container.memoryLimit",
			entries: []cfg{claimed(`"container": {"memoryLimit": {"raise": "claimed"}}`)},
			err:     `MemoryConfig: FromClaim config 0: json: unknown field "container.memoryLimit"`,
		},
		{
			name:    "generic pool field",
			entries: []cfg{claimed(`"pool": {"slot": "x"}`).of(pool)},
			driver:  pool,
			err:     `MemoryConfig: FromClaim config 0: json: unknown field "pool.slot"`,
		},
		{
			name:    "field names are case sensitive",
			entries: []cfg{claimed(`"device": {"Mode": "dax"}`)},
			err:     `MemoryConfig: FromClaim config 0: json: unknown field "device.Mode"`,
		},
		{
			name:    "unknown nested field",
			entries: []cfg{claimed(`"container": {"steering": {"order": "first-dram"}}`)},
			err:     `MemoryConfig: FromClaim config 0: json: unknown field "container.steering.order"`,
		},
		{
			name:    "duplicate field",
			entries: []cfg{claimed(`"device": {"mode": "dax", "mode": "numa"}`)},
			err:     `MemoryConfig: FromClaim config 0: json: duplicate field "device.mode"`,
		},
		{
			name:    "wrong type",
			entries: []cfg{claimed(`"device": {"readOnly": "yes"}`)},
			err:     `MemoryConfig: FromClaim config 0: json: cannot unmarshal string into Go struct field Device.device.readOnly of type bool`,
		},
		{
			name:    "an invalid entry that a later one would fix",
			entries: []cfg{claimed(`"device": {"mode": 1}`), claimed(`"device": {"mode": "dax"}`)},
			err:     `MemoryConfig: FromClaim config 0: json: cannot unmarshal number into Go struct field Device.device.mode of type string`,
		},
		{
			name:    "not an object",
			entries: []cfg{claimed(`[1]`).rawParams()},
			err:     `MemoryConfig: FromClaim config 0: not a JSON object`,
		},
		{
			name:    "wrong kind",
			entries: []cfg{claimed(`{"apiVersion": "cxl.nri.io/v1alpha1", "kind": "DeviceConfig", "mode": "dax"}`).rawParams()},
			err:     `MemoryConfig: FromClaim config 0: kind "DeviceConfig", expected "MemoryConfig"`,
		},
		{
			name:    "no kind",
			entries: []cfg{claimed(`{"apiVersion": "cxl.nri.io/v1alpha1"}`).rawParams()},
			err:     `MemoryConfig: FromClaim config 0: kind "", expected "MemoryConfig"`,
		},
		{
			name:    "kind is case sensitive",
			entries: []cfg{claimed(`{"apiVersion": "cxl.nri.io/v1alpha1", "Kind": "MemoryConfig"}`).rawParams()},
			err:     `MemoryConfig: FromClaim config 0: kind "", expected "MemoryConfig"`,
		},
		{
			name:    "pool apiVersion",
			entries: []cfg{claimed(`{"apiVersion": "cxl-pool.nri.io/v1alpha1", "kind": "MemoryConfig"}`).rawParams().of(pool)},
			driver:  pool,
			err:     `MemoryConfig: FromClaim config 0: apiVersion "cxl-pool.nri.io/v1alpha1", expected "cxl.nri.io/v1alpha1"`,
		},
		{
			name:    "other apiVersion",
			entries: []cfg{claimed(`{"apiVersion": "cxl.nri.io/v1", "kind": "MemoryConfig"}`).rawParams()},
			err:     `MemoryConfig: FromClaim config 0: apiVersion "cxl.nri.io/v1", expected "cxl.nri.io/v1alpha1"`,
		},
		{
			name:    "fencing in a claim entry",
			entries: []cfg{claimed(`"container": {"fencing": "none"}`)},
			err:     `MemoryConfig: container.fencing: set by the driver configuration only (defaults.container.fencing)`,
		},
		{
			name:    "fencing in a class entry",
			entries: []cfg{class(`"container": {"fencing": "allowed-mems"}`, "r")},
			err:     `MemoryConfig: container.fencing: set by the driver configuration only (defaults.container.fencing)`,
		},
		{
			name:    "fencing in a pool entry",
			entries: []cfg{class(`"container": {"fencing": "none"}`, "r").of(pool)},
			driver:  pool,
			err:     `MemoryConfig: container.fencing: set by the driver configuration only (defaults.container.fencing)`,
		},
		{
			name:    "pool in a cxl.nri.io entry",
			entries: []cfg{claimed(`"pool": {"opaque": {"fake-cxl-pool": {"numaNode": 1}}}`)},
			err:     `MemoryConfig: pool: only for devices of cxl-pool.nri.io`,
		},
		{
			name:    "pool in a cxl.nri.io class entry",
			entries: []cfg{class(`"pool": {}`, "r")},
			err:     `MemoryConfig: pool: only for devices of cxl-pool.nri.io`,
		},
		{
			name:    "invalid policy",
			entries: []cfg{claimed(`"container": {"steering": {"policy": "x"}}`)},
			err:     `MemoryConfig: container.steering.policy: invalid policy "x" (none, auto, first-dram, first-cxl, start-interleaved, end-interleaved or waypoints)`,
		},
		{
			name:    "policy is case sensitive",
			entries: []cfg{claimed(`"container": {"steering": {"policy": "First-DRAM"}}`)},
			err:     `MemoryConfig: container.steering.policy: invalid policy "First-DRAM" (none, auto, first-dram, first-cxl, start-interleaved, end-interleaved or waypoints)`,
		},
		{
			name:    "none with maxStep",
			entries: []cfg{claimed(`"container": {"steering": {"policy": "none", "maxStep": "1Gi"}}`)},
			err:     `MemoryConfig: container.steering.maxStep needs a policy other than none`,
		},
		{
			name: "none of the claim with maxStep of the class",
			entries: []cfg{
				class(`"container": {"steering": {"policy": "first-dram", "maxStep": "1Gi"}}`, "r"),
				claimed(unsteered),
			},
			err: `MemoryConfig: container.steering.maxStep needs a policy other than none`,
		},
		{
			name:    "waypoints with another policy",
			entries: []cfg{claimed(`"container": {"steering": {"policy": "first-cxl", "waypoints": [{"targetUsages": [{"memoryType": "CXL", "usage": "1Gi"}]}]}}`)},
			err:     `MemoryConfig: container.steering.waypoints needs policy waypoints`,
		},
		{
			name:    "minStep exceeds maxStep",
			entries: []cfg{claimed(`"container": {"steering": {"minStep": "2Gi", "maxStep": "1Gi"}}`)},
			err:     `MemoryConfig: container.steering.minStep 2Gi exceeds maxStep 1Gi`,
		},
		{
			name:    "invalid quantity",
			entries: []cfg{claimed(`"container": {"steering": {"policy": "auto", "minStep": "lots"}}`)},
			err:     `MemoryConfig: container.steering.minStep: invalid quantity "lots"`,
		},
		{
			name:    "zero step",
			entries: []cfg{claimed(`"container": {"steering": {"policy": "auto", "maxStep": "0"}}`)},
			err:     `MemoryConfig: container.steering.maxStep: invalid quantity "0" (must be positive)`,
		},
		{
			name:    "invalid memory type",
			entries: []cfg{claimed(`"container": {"steering": {"policy": "waypoints", "waypoints": [{"targetUsages": [{"memoryType": "HBM", "usage": "1Gi"}]}]}}`)},
			err:     `MemoryConfig: container.steering.waypoints[0].targetUsages[0].memoryType: invalid memory type "HBM" (DRAM or CXL)`,
		},
		{
			name:    "pool opaque value not an object",
			entries: []cfg{claimed(`"pool": {"opaque": {"fake-cxl-pool": 1}}`).of(pool)},
			driver:  pool,
			err:     `MemoryConfig: pool.opaque.fake-cxl-pool: not a JSON object`,
		},
		{
			name:    "pool opaque empty key",
			entries: []cfg{claimed(`"pool": {"opaque": {"": {}}}`).of(pool)},
			driver:  pool,
			err:     `MemoryConfig: pool.opaque: empty controller key`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := tc.driver
			if driver == "" {
				driver = local
			}
			_, err := Resolve(testClaim(tc.entries...), driver, "r")
			if !checkErr(err, tc.err) {
				t.Errorf("got  %v\nwant %s", err, tc.err)
			}
		})
	}
}

func TestResolveOtherRequestNotChecked(t *testing.T) {
	claim := testClaim(claimed(`"x": 1`, "d"), claimed(`"container": {"steering": {"policy": "x"}}`, "d"))
	if _, err := Resolve(claim, local, "r"); err != nil {
		t.Errorf("Resolve of r: %v", err)
	}
	if err := CheckEntries(claim, local); !checkErr(err, `MemoryConfig: FromClaim config 0: json: unknown field "x"`) {
		t.Errorf("CheckEntries: %v", err)
	}
}

func TestCheckEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []cfg
		driver  string
		err     string
	}{
		{name: "none"},
		{
			name: "valid entries, values are not checked",
			entries: []cfg{
				class(`"device": {"mode": "dax"}`, "r"),
				claimed(`"container": {"steering": {"policy": "x"}}`, "d"),
			},
		},
		{
			name:    "other drivers are not checked",
			entries: []cfg{claimed(`"kind": "Other"`).rawParams().of("gpu.example.com")},
		},
		{
			name:    "kind of an entry of another request",
			entries: []cfg{claimed(``), claimed(`{"kind": "DeviceConfig"}`, "d").rawParams()},
			err:     `MemoryConfig: FromClaim config 1: kind "DeviceConfig", expected "MemoryConfig"`,
		},
		{
			name:    "pool in an entry of another request",
			entries: []cfg{claimed(`"pool": {}`, "d")},
			err:     `MemoryConfig: pool: only for devices of cxl-pool.nri.io`,
		},
		{
			name:    "pool in a pool entry",
			entries: []cfg{claimed(`"pool": {"opaque": {"a": {}}}`, "d").of(pool)},
			driver:  pool,
		},
		{
			name:    "fencing in a pool entry",
			entries: []cfg{class(`"container": {"fencing": "none"}`, "p/pool").of(pool)},
			driver:  pool,
			err:     `MemoryConfig: container.fencing: set by the driver configuration only (defaults.container.fencing)`,
		},
		{
			name:    "fencing in a claim entry",
			entries: []cfg{claimed(`"container": {"fencing": "none"}`, "r")},
			err:     `MemoryConfig: container.fencing: set by the driver configuration only (defaults.container.fencing)`,
		},
		{
			name:    "pool in a class entry",
			entries: []cfg{class(`"pool": {"opaque": {"a": {}}}`, "r")},
			err:     `MemoryConfig: pool: only for devices of cxl-pool.nri.io`,
		},
		{
			name: "every other section in pool entries",
			entries: []cfg{
				class(`"device": {"mode": "dax"}, "container": {"steering": {"policy": "auto"}}`, "r").of(pool),
				claimed(`"pool": {"opaque": {"a": {}}}, "container": {"steering": {"policy": "auto"}}`, "r").of(pool),
			},
			driver: pool,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := tc.driver
			if driver == "" {
				driver = local
			}
			if err := CheckEntries(testClaim(tc.entries...), driver); !checkErr(err, tc.err) {
				t.Errorf("got  %v\nwant %s", err, tc.err)
			}
		})
	}
}

func TestNoAllocation(t *testing.T) {
	claim := testClaim()
	claim.Status.Allocation = nil
	const want = "MemoryConfig: claim ns/a has no allocation"
	if _, err := Resolve(claim, local, "r"); !checkErr(err, want) {
		t.Errorf("Resolve: %v", err)
	}
	if err := CheckEntries(claim, local); !checkErr(err, want) {
		t.Errorf("CheckEntries: %v", err)
	}
}

func TestLookup(t *testing.T) {
	var params map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"a": {"b": {"c": 1}, "n": null}, "x": null, "s": "t"}`), &params); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{
		"a":       valueSet,
		"a.b.c":   valueSet,
		"a.b.d":   valueAbsent,
		"a.n":     valueNull,
		"a.n.c":   valueNull,
		"x":       valueNull,
		"x.y":     valueNull,
		"s.t":     valueAbsent,
		"missing": valueAbsent,
	} {
		if got := lookup(params, strings.Split(path, ".")); got != want {
			t.Errorf("%s: %d, expected %d", path, got, want)
		}
	}
}
