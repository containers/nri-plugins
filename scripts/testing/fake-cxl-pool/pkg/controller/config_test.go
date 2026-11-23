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

package controller

import (
	"encoding/json"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
)

// memoryConfig returns an opaque config entry of a driver with the
// MemoryConfig fields in body (JSON object members) for the requests.
func memoryConfig(source resourceapi.AllocationConfigSource, driver, body string, requests ...string) resourceapi.DeviceAllocationConfiguration {
	raw := `{"apiVersion": "cxl.nri.io/v1alpha1", "kind": "MemoryConfig"`
	if body != "" {
		raw += ", " + body
	}
	raw += "}"
	return resourceapi.DeviceAllocationConfiguration{
		Source:   source,
		Requests: requests,
		DeviceConfiguration: resourceapi.DeviceConfiguration{Opaque: &resourceapi.OpaqueDeviceConfiguration{
			Driver: driver, Parameters: runtime.RawExtension{Raw: []byte(raw)},
		}},
	}
}

// claimConfig is a claim entry of the pool driver for all requests.
func claimConfig(body string) resourceapi.DeviceAllocationConfiguration {
	return memoryConfig(resourceapi.AllocationConfigSourceClaim, device.PoolDriverName, body)
}

func (p *fakePool) requests() []api.AttachRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]api.AttachRequest(nil), p.attachReqs...)
}

func reqSettings(r api.AttachRequest) string {
	return PoolSettings{Slot: r.Slot, NUMANode: r.NUMANode}.String()
}

func TestConfigSettingsPassed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		key    string // -controller-key, "" for the default
		config []resourceapi.DeviceAllocationConfiguration
		want   string // settings of the attach request
		slot   string // slot in the Attached message
	}{
		{name: "no config", slot: "ds1"},
		{
			name:   "device and container sections only",
			config: []resourceapi.DeviceAllocationConfiguration{claimConfig(`"device": {"mode": "dax"}, "container": {"steering": {"policy": "auto"}}`)},
			slot:   "ds1",
		},
		{
			name:   "numa node",
			config: []resourceapi.DeviceAllocationConfiguration{claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"numaNode": 1}}}`)},
			want:   "NUMA node 1",
			slot:   "ds1",
		},
		{
			name: "own key of merged entries, other keys and sections ignored",
			config: []resourceapi.DeviceAllocationConfiguration{
				claimConfig(`"device": {"mode": "dax"}, "pool": {"opaque": {"fake-cxl-pool": {"slot": "ds7", "numaNode": 1}, "vendor-x": {"port": 3, "qos": "high"}}}`),
				// class entries come first whatever the list order
				memoryConfig(resourceapi.AllocationConfigSourceClass, device.PoolDriverName, `"pool": {"opaque": {"fake-cxl-pool": {"numaNode": 0, "slot": "ds2"}}}`),
				// another request, another driver
				memoryConfig(resourceapi.AllocationConfigSourceClaim, device.PoolDriverName, `"pool": {"opaque": {"fake-cxl-pool": {"numaNode": 5}}}`, "other"),
				memoryConfig(resourceapi.AllocationConfigSourceClaim, device.DriverName, `"device": {"mode": "dax"}`),
				{Source: resourceapi.AllocationConfigSourceClaim, DeviceConfiguration: resourceapi.DeviceConfiguration{Opaque: &resourceapi.OpaqueDeviceConfiguration{
					Driver: "other.example.com", Parameters: runtime.RawExtension{Raw: []byte(`{"anything": true}`)},
				}}},
			},
			want: "slot ds7, NUMA node 1",
			slot: "ds7",
		},
		{
			name:   "null removes the setting of the class",
			config: []resourceapi.DeviceAllocationConfiguration{memoryConfig(resourceapi.AllocationConfigSourceClass, device.PoolDriverName, `"pool": {"opaque": {"fake-cxl-pool": {"numaNode": 0, "slot": "ds2"}}}`), claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"slot": null}}}`)},
			want:   "NUMA node 0",
			slot:   "ds1",
		},
		{
			name: "another config key: the default key is another controller's",
			key:  "vendor-x",
			config: []resourceapi.DeviceAllocationConfiguration{
				claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"port": 3}, "vendor-x": {"slot": "ds4"}}}`),
			},
			want: "slot ds4",
			slot: "ds4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnvWith(t, func(_ *fakePool, c *Config) { c.ControllerKey = tc.key })
			e.createClaimConfig("c", "pooled0", "node1", "", 0, tc.config)
			e.sync()
			reqs := e.pool.requests()
			if len(reqs) != 1 || reqSettings(reqs[0]) != tc.want {
				t.Fatalf("attach requests %+v, want settings %q", reqs, tc.want)
			}
			cond, _ := e.condition("c", "cxl-pool.nri.io/Attached")
			if cond == nil || cond.Message != "pooled0 attached to vm1 (slot "+tc.slot+")" {
				t.Fatalf("unexpected condition %+v", cond)
			}
		})
	}
}

func TestConfigInvalid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		config []resourceapi.DeviceAllocationConfiguration
		err    string
	}{
		{
			name:   "unknown field of the own object",
			config: []resourceapi.DeviceAllocationConfiguration{claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"slot": "ds1", "port": 3}}}`)},
			err:    `MemoryConfig: pool.opaque.fake-cxl-pool: json: unknown field "port"`,
		},
		{
			name:   "negative numa node",
			config: []resourceapi.DeviceAllocationConfiguration{claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"numaNode": -1}}}`)},
			err:    `MemoryConfig: pool.opaque.fake-cxl-pool.numaNode: invalid NUMA node -1`,
		},
		{
			name: "invalid entry of another request of the claim",
			config: []resourceapi.DeviceAllocationConfiguration{
				memoryConfig(resourceapi.AllocationConfigSourceClaim, device.PoolDriverName, `"pool": {"slot": "ds1"}`, "other"),
			},
			err: `MemoryConfig: FromClaim config 0: json: unknown field "pool.slot"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.createClaimConfig("c", "pooled0", "node1", "", 0, tc.config)
			e.sync()
			if att, _ := e.pool.calls(); len(att) != 0 {
				t.Fatalf("unexpected attach requests %v", att)
			}
			cond, _ := e.condition("c", "cxl-pool.nri.io/AttachFailed")
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonInvalidConfig || !strings.HasSuffix(cond.Message, tc.err) {
				t.Fatalf("unexpected condition %+v, want message %q", cond, tc.err)
			}
			if c, _ := e.condition("c", "cxl-pool.nri.io/Attached"); c != nil {
				t.Fatalf("unexpected Attached condition %+v", c)
			}
			if ev := e.events(); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning AttachFailed "+cond.Message) {
				t.Fatalf("unexpected events %v", ev)
			}
			logLine := "ERROR: claim default/c: request mem device pooled0: invalid configuration: " + cond.Message
			if n := strings.Count(e.logs.String(), logLine); n != 1 {
				t.Fatalf("log %q %d times", logLine, n)
			}
			// stable: no attach, no status write, no event, no new log
			e.waitStatus("c", 1)
			rv := e.claim("c").ResourceVersion
			e.sync()
			e.sync()
			if att, det := e.pool.calls(); len(att) != 0 || len(det) != 0 {
				t.Fatalf("unexpected requests %v %v", att, det)
			}
			if e.claim("c").ResourceVersion != rv {
				t.Fatal("status rewritten")
			}
			if ev := e.events(); len(ev) != 0 {
				t.Fatalf("unexpected events %v", ev)
			}
			if n := strings.Count(e.logs.String(), logLine); n != 1 {
				t.Fatalf("log %q %d times", logLine, n)
			}
		})
	}
}

func TestConfigInvalidNotHalfAttached(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// the device of the invalid claim is attached without an Attached
	// condition: detached
	claim := e.createClaimConfig("c", "pooled0", "node1", "", 0, []resourceapi.DeviceAllocationConfiguration{claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"port": 3}}}`)})
	e.pool.addAttachment(api.Attachment{Device: "pooled0", Host: "vm1", Owner: e.c.Owner(claim.UID), Slot: api.Slot{Bus: "ds9"}})
	e.sync()
	if att, det := e.pool.calls(); len(att) != 0 || len(det) != 1 || det[0] != "pooled0@vm1" {
		t.Fatalf("attach %v detach %v", att, det)
	}
	if c, _ := e.condition("c", "cxl-pool.nri.io/AttachFailed"); c == nil || c.Reason != ReasonInvalidConfig {
		t.Fatalf("unexpected condition %+v", c)
	}
}

func TestConfigInvalidSharedWithValid(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createClaimConfig("a", "shared0", "node1", "share-a", 1, nil)
	e.createClaimConfig("b", "shared0", "node1", "share-b", 1, []resourceapi.DeviceAllocationConfiguration{claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"slot": 1}}}`)})
	e.sync()
	if att, _ := e.pool.calls(); len(att) != 1 || att[0] != "shared0@vm1 k8s:resourceclaim/uid-a" {
		t.Fatalf("unexpected attach requests %v", att)
	}
	if c, _ := e.condition("a", "cxl-pool.nri.io/Attached"); c == nil {
		t.Fatal("claim a: no Attached condition")
	}
	if c, _ := e.condition("b", "cxl-pool.nri.io/AttachFailed"); c == nil || c.Reason != ReasonInvalidConfig {
		t.Fatalf("claim b: unexpected condition %+v", c)
	}
	e.sync()
	if _, det := e.pool.calls(); len(det) != 0 {
		t.Fatalf("unexpected detach requests %v", det)
	}
}

// A result that is Attached keeps its attachment when the controller
// refuses its configuration: a device in use is never detached.
func TestConfigInvalidAttachedKept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	claim := e.createClaimConfig("c", "pooled0", "node1", "", 0, []resourceapi.DeviceAllocationConfiguration{claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"port": 3}}}`)})
	e.pool.addAttachment(api.Attachment{Device: "pooled0", Host: "vm1", Owner: e.c.Owner(claim.UID), Slot: api.Slot{Bus: "ds9"}})
	e.markAttached("c")
	e.sync()
	e.sync()
	if att, det := e.pool.calls(); len(att) != 0 || len(det) != 0 {
		t.Fatalf("attach %v detach %v", att, det)
	}
	if c, _ := e.condition("c", "cxl-pool.nri.io/Attached"); c == nil {
		t.Fatal("Attached condition removed")
	}
	logLine := `ERROR: claim default/c: request mem device pooled0: invalid configuration: MemoryConfig: pool.opaque.fake-cxl-pool: json: unknown field "port" (attached already, kept)`
	if n := strings.Count(e.logs.String(), logLine); n != 1 {
		t.Fatalf("log %q %d times", logLine, n)
	}
}

func TestConfigExistingAttachmentKept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// attached in another slot: no reattach
	claim := e.createClaimConfig("c", "pooled0", "node1", "", 0, []resourceapi.DeviceAllocationConfiguration{claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"slot": "ds7"}}}`)})
	e.pool.addAttachment(api.Attachment{Device: "pooled0", Host: "vm1", Owner: e.c.Owner(claim.UID), Slot: api.Slot{Bus: "ds9"}})
	e.sync()
	if att, det := e.pool.calls(); len(att) != 0 || len(det) != 0 {
		t.Fatalf("attach %v detach %v", att, det)
	}
	if c, _ := e.condition("c", "cxl-pool.nri.io/Attached"); c == nil || c.Message != "pooled0 attached to vm1 (slot ds9)" {
		t.Fatalf("unexpected condition %+v", c)
	}
	logLine := "claim default/c device pooled0 node node1: attachment pooled0@vm1 is in slot ds9, not in the configured slot ds7: kept"
	if !strings.Contains(e.logs.String(), logLine) {
		t.Fatalf("no log %q", logLine)
	}
	// the exclusive flag of an existing attachment is set without the
	// settings; the status has the share ID and the data of the attachment
	w := e.createClaimConfig("w", "shared0", "node1", "share-w", device.SharesUnlimited, []resourceapi.DeviceAllocationConfiguration{claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"slot": "ds7", "numaNode": 1}}}`)})
	e.pool.addAttachment(api.Attachment{Device: "shared0", Host: "vm1", Serial: "0xc1ae0001", Owner: e.c.Owner(w.UID), Slot: api.Slot{Bus: "ds3"},
		QemuDeviceID: "fcp_shared0.hp7"})
	e.sync()
	reqs := e.pool.requests()
	if len(reqs) != 1 || !reqs[0].SingleHost || reqSettings(reqs[0]) != "" {
		t.Fatalf("unexpected attach requests %+v", reqs)
	}
	c, st := e.condition("w", "cxl-pool.nri.io/Attached")
	if c == nil || c.Message != "shared0 attached to vm1 (slot ds3)" || st.ShareID == nil || *st.ShareID != "share-w" {
		t.Fatalf("unexpected status %+v %+v", c, st)
	}
	pd, err := device.ParsePoolStatusData(st.Data.Raw)
	if err != nil || !pd.Shared || pd.Serial != 0xc1ae0001 || pd.Size != 256<<20 {
		t.Fatalf("unexpected data %s %v", st.Data.Raw, err)
	}
	var data DeviceData
	if err := json.Unmarshal(st.Data.Raw, &data); err != nil || data.QemuDeviceID != "fcp_shared0.hp7" {
		t.Fatalf("unexpected data %s %v", st.Data.Raw, err)
	}
}
