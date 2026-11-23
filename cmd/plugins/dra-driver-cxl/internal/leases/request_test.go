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

package leases

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// poolSlice is the pool controller's ResourceSlice with an exclusive
// device pooled0 and the shared devices shared0 and shared1.
func poolSlice() *resourcev1.ResourceSlice {
	attrs := func(serial string, shared bool) map[resourcev1.QualifiedName]resourcev1.DeviceAttribute {
		return map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"type": {StringValue: ptr("memdev")}, "source": {StringValue: ptr("pool")}, "serial": {StringValue: &serial},
			"shared": {BoolValue: ptr(shared)}, "size": {IntValue: ptr(int64(1 << 28))},
		}
	}
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-cxl-pool"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   device.PoolDriverName,
			Pool:     resourcev1.ResourcePool{Name: "fake-cxl-pool", ResourceSliceCount: 1},
			AllNodes: ptr(true),
			Devices: []resourcev1.Device{
				{Name: "pooled0", Attributes: attrs("0xc1ee0001", false),
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"memory": {Value: resource.MustParse("512Mi")}}},
				{Name: "shared0", Attributes: attrs("0xC1AE0001", true),
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"shares": {Value: resource.MustParse("1M")}}},
				{Name: "shared1", Attributes: attrs("0xc1ae0002", true)},
			},
		},
	}
}

// localResourceSlice is the cxl.nri.io ResourceSlice of node1 with the local
// memdev 0xc100e2e1, the shared local memdev 0xc100e2e2 and the persistent
// local memdev 0xc100f5e1.
func localResourceSlice() *resourcev1.ResourceSlice {
	return nodeResourceSlice(envNode, localDeviceOf("0xc100e2e1", false, false), localDeviceOf("0xc100e2e2", true, false),
		localDeviceOf("0xc100f5e1", false, true))
}

// localDeviceOf is a local memdev of 1 GiB with the serial as cxl.nri.io
// publishes it: a shared or a persistent one has shares, another one
// memory.
func localDeviceOf(serial string, shared, persistent bool) resourcev1.Device {
	capacity := map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"memory": {Value: resource.MustParse("1Gi")}}
	if shared || persistent {
		capacity = map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"shares": {Value: resource.MustParse("1M")}}
	}
	return resourcev1.Device{
		Name: "memdev-" + strings.TrimPrefix(serial, "0x"),
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"type": {StringValue: ptr("memdev")}, "source": {StringValue: ptr("local")}, "serial": {StringValue: ptr(serial)},
			"shared": {BoolValue: ptr(shared)}, "persistent": {BoolValue: ptr(persistent)}, "size": {IntValue: ptr(int64(1 << 30))},
		},
		Capacity:                 capacity,
		AllowMultipleAllocations: ptr(true),
	}
}

// nodeResourceSlice returns the cxl.nri.io ResourceSlice of a node with the
// devices.
func nodeResourceSlice(node string, devs ...resourcev1.Device) *resourcev1.ResourceSlice {
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-cxl"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   device.DriverName,
			Pool:     resourcev1.ResourcePool{Name: node, ResourceSliceCount: 1},
			NodeName: ptr(node),
			Devices:  devs,
		},
	}
}

// persistentPoolSlice is a ResourceSlice of the pool controller with the
// shared persistent device llm0 (0xc1af0001) and the exclusive persistent
// device llm1 (0xc1af0002).
func persistentPoolSlice() *resourcev1.ResourceSlice {
	attrs := func(serial string, shared bool) map[resourcev1.QualifiedName]resourcev1.DeviceAttribute {
		return map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"type": {StringValue: ptr("memdev")}, "source": {StringValue: ptr("pool")}, "serial": {StringValue: &serial},
			"shared": {BoolValue: ptr(shared)}, "size": {IntValue: ptr(int64(1 << 29))}, "persistent": {BoolValue: ptr(true)},
		}
	}
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-cxl-pool-persistent"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   device.PoolDriverName,
			Pool:     resourcev1.ResourcePool{Name: "fake-cxl-pool-persistent", ResourceSliceCount: 1},
			AllNodes: ptr(true),
			Devices: []resourcev1.Device{
				{Name: "llm0", Attributes: attrs("0xc1af0001", true),
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"shares": {Value: resource.MustParse("1M")}}},
				{Name: "llm1", Attributes: attrs("0xc1af0002", false),
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"memory": {Value: resource.MustParse("512Mi")}}},
			},
		},
	}
}

// newSlicesBrokerEnv returns a broker env that publishes the pool, the
// local and the persistent pool ResourceSlices.
func newSlicesBrokerEnv(t *testing.T) *leaseEnv {
	e := brokerEnv(t)
	e.slices = []*resourcev1.ResourceSlice{poolSlice(), localResourceSlice(), persistentPoolSlice()}
	return e
}

// publishedDevice returns the driver and the device of a serial in the
// published ResourceSlices, nil if there is none.
func (e *leaseEnv) publishedDevice(serial uint64) (string, *resourcev1.Device) {
	for _, rs := range e.slices {
		for i := range rs.Spec.Devices {
			d := &rs.Spec.Devices[i]
			if a, ok := deviceAttribute(d, rs.Spec.Driver, device.AttrSerial); ok && a.StringValue != nil {
				if s, err := api.ParseSerial(*a.StringValue); err == nil && s == serial {
					return rs.Spec.Driver, d
				}
			}
		}
	}
	return "", nil
}

func TestRequestOptions(t *testing.T) {
	e := newSlicesBrokerEnv(t)
	calls := len(e.client.Actions())
	for _, tc := range []struct {
		req  api.Request
		want askedText
		err  string
	}{
		{req: api.Request{Size: "1Gi"}, want: askedText{Size: "1Gi", Mode: "numa"}},
		{req: api.Request{Size: "1024Mi", Mode: "dax", ReadOnly: ptr(true)}, want: askedText{Size: "1Gi", Mode: "dax", ReadOnly: true}},
		{req: api.Request{Share: true, Size: "256Mi"}, want: askedText{Shared: true, SharedDevice: true, Size: "256Mi", Mode: "dax"}},
		// a reader and the writer of a shared device
		{req: api.Request{Serial: "0xC1AE0001", Share: true, ReadOnly: ptr(true)}, want: askedText{Serial: "0xc1ae0001", Shared: true, SharedDevice: true, DeviceSource: "pool", Mode: "dax", ReadOnly: true}},
		{req: api.Request{Serial: "3249405953"}, want: askedText{Serial: "0xc1ae0001", Mode: "dax", SharedDevice: true, DeviceSource: "pool"}},
		{req: api.Request{Serial: "0xc1ae0001", Mode: "dax", ReadOnly: ptr(true), Source: "pool"}, want: askedText{Source: "pool", Serial: "0xc1ae0001", Mode: "dax", ReadOnly: true, SharedDevice: true}},
		// a shared device has no capacity that the broker reads
		{req: api.Request{Serial: "0xc1ae0002"}, want: askedText{Serial: "0xc1ae0002", Mode: "dax", SharedDevice: true, DeviceSource: "pool"}},
		// a reader and the writer of a shared local device
		{req: api.Request{Serial: "0xc100e2e2", Share: true, ReadOnly: ptr(true)}, want: askedText{Serial: "0xc100e2e2", Shared: true, SharedDevice: true, DeviceSource: "local", Mode: "dax", ReadOnly: true}},
		{req: api.Request{Serial: "0xc100e2e2", Source: "local"}, want: askedText{Source: "local", Serial: "0xc100e2e2", Mode: "dax", SharedDevice: true}},
		{req: api.Request{Source: "local", Share: true, Size: "64Mi"}, want: askedText{Source: "local", Shared: true, SharedDevice: true, Size: "64Mi", Mode: "dax"}},
		{req: api.Request{Size: "64Mi", Source: "local"}, want: askedText{Source: "local", Size: "64Mi", Mode: "numa"}},
		{req: api.Request{Size: "64Mi", Source: "local", Mode: "dax", ReadOnly: ptr(true), Serial: "0xC100E2E1"},
			want: askedText{Source: "local", Size: "64Mi", Mode: "dax", ReadOnly: true, Serial: "0xc100e2e1"}},
		{req: api.Request{Size: "64Mi", Source: "pool", Serial: "0xc1ee0001"}, want: askedText{Source: "pool", Size: "64Mi", Mode: "numa", Serial: "0xc1ee0001"}},
		// a serial of no published device
		{req: api.Request{Serial: "0x1234", Share: true}, want: askedText{Serial: "0x1234", Shared: true, SharedDevice: true, Mode: "dax"}},
		{req: api.Request{Serial: "0"}, want: askedText{Serial: "0x0", Mode: "numa"}},
		{req: api.Request{Serial: "0xc1ee0001", Share: true}, err: "memdev 0xc1ee0001: not shareable"},
		{req: api.Request{Serial: "0xc100e2e1", Share: true}, err: "memdev 0xc100e2e1: not shareable"},
		{req: api.Request{Serial: "0xc1ae0001", Mode: "numa"}, err: "shared memory is never onlined as RAM: use mode dax"},
		{req: api.Request{Serial: "0xc1ae0001", Source: "local"}, err: "memdev 0xc1ae0001: not a local device"},
		{req: api.Request{Serial: "0xc100e2e2", Source: "pool"}, err: "memdev 0xc100e2e2: not a pool device"},
		{req: api.Request{Serial: "0xc100e2e2", Mode: "numa"}, err: "shared memory is never onlined as RAM: use mode dax"},
		// the refusals in their order
		{req: api.Request{Serial: "0xc100e2e2", Source: "pool", Mode: "numa"}, err: "memdev 0xc100e2e2: not a pool device"},
		{req: api.Request{Serial: "0xc1ae0001", Source: "local", Mode: "numa"}, err: "memdev 0xc1ae0001: not a local device"},
		{req: api.Request{Size: "lots"}, err: `invalid size "lots"`},
		// mode fs on persistent devices, shared or not
		{req: api.Request{Serial: "0xc1af0001", Share: true, ReadOnly: ptr(true), Mode: "fs"},
			want: askedText{Serial: "0xc1af0001", Shared: true, SharedDevice: true, DeviceSource: "pool", Mode: "fs", ReadOnly: true}},
		{req: api.Request{Serial: "0xc1af0001", ReadOnly: ptr(true), Mode: "fs", Access: "cached", Name: "models", Source: "pool"},
			want: askedText{Source: "pool", Serial: "0xc1af0001", SharedDevice: true, Mode: "fs", ReadOnly: true, Access: "cached", Name: "models"}},
		{req: api.Request{Serial: "0xc1af0002", ReadOnly: ptr(true), Mode: "fs", Size: "512Mi"},
			want: askedText{Serial: "0xc1af0002", DeviceSource: "pool", Mode: "fs", ReadOnly: true, Size: "512Mi"}},
		{req: api.Request{Share: true, ReadOnly: ptr(true), Mode: "fs", Size: "256Mi"},
			want: askedText{Shared: true, SharedDevice: true, Mode: "fs", ReadOnly: true, Size: "256Mi"}},
		// read-only without readOnly, read-write with readOnly false, a type
		{req: api.Request{Serial: "0xc1af0001", Share: true, Mode: "fs", FSType: "ext4"},
			want: askedText{Serial: "0xc1af0001", Shared: true, SharedDevice: true, DeviceSource: "pool", Mode: "fs", ReadOnly: true, FSType: "ext4"}},
		{req: api.Request{Serial: "0xc1af0001", Share: true, Mode: "fs", ReadOnly: ptr(false), Access: "cached"},
			want: askedText{Serial: "0xc1af0001", Shared: true, SharedDevice: true, DeviceSource: "pool", Mode: "fs", Access: "cached"}},
		// a persistent local memdev: a share although it is not shared
		{req: api.Request{Serial: "0xc100f5e1", Share: true, Mode: "fs"},
			want: askedText{Serial: "0xc100f5e1", Shared: true, SharedDevice: true, DeviceSource: "local", Mode: "fs", ReadOnly: true}},
		{req: api.Request{Serial: "0xc100f5e1", Source: "local", Mode: "block"},
			want: askedText{Source: "local", Serial: "0xc100f5e1", Mode: "block"}},
		{req: api.Request{Serial: "0xc100f5e1", Mode: "dax"}, err: "memdev 0xc100f5e1: a persistent device needs mode block or fs"},
		// the source checks of a persistent local memdev that is not shared
		{req: api.Request{Serial: "0xc100f5e1", Source: "pool", Mode: "fs"}, err: "memdev 0xc100f5e1: not a pool device"},
		{req: api.Request{Serial: "0xc1af0002", Source: "local", Mode: "block"}, err: "memdev 0xc1af0002: not a local device"},
		// B3, B4 after the checks of the device and its source
		{req: api.Request{Serial: "0xc1ae0001", Share: true, ReadOnly: ptr(true), Mode: "fs"}, err: "memdev 0xc1ae0001: mode fs needs a persistent device"},
		{req: api.Request{Serial: "0xc1ee0001", ReadOnly: ptr(true), Mode: "fs"}, err: "memdev 0xc1ee0001: mode fs needs a persistent device"},
		{req: api.Request{Serial: "0xc100e2e2", Share: true, ReadOnly: ptr(true), Mode: "fs"}, err: "memdev 0xc100e2e2: mode fs needs a persistent device"},
		{req: api.Request{Serial: "0xc1af0001", Share: true, ReadOnly: ptr(true)}, err: "memdev 0xc1af0001: a persistent device needs mode block or fs"},
		{req: api.Request{Serial: "0xc1af0001", Mode: "numa"}, err: "memdev 0xc1af0001: a persistent device needs mode block or fs"},
		{req: api.Request{Serial: "0xc1af0002", Size: "64Mi"}, err: "memdev 0xc1af0002: a persistent device needs mode block or fs"},
		{req: api.Request{Serial: "0xc1af0002", Mode: "dax"}, err: "memdev 0xc1af0002: a persistent device needs mode block or fs"},
		{req: api.Request{Serial: "0xc1ee0001", Share: true, ReadOnly: ptr(true), Mode: "fs"}, err: "memdev 0xc1ee0001: not shareable"},
		{req: api.Request{Serial: "0xc1af0001", Source: "local", Mode: "numa"}, err: "memdev 0xc1af0001: not a local device"},
		// mode block on persistent devices: the whole device, or a share read-only
		{req: api.Request{Serial: "0xc1af0001", Mode: "block"}, want: askedText{Serial: "0xc1af0001", SharedDevice: true, DeviceSource: "pool", Mode: "block"}},
		{req: api.Request{Serial: "0xc1af0001", Share: true, ReadOnly: ptr(true), Mode: "block", Source: "pool"},
			want: askedText{Source: "pool", Serial: "0xc1af0001", Shared: true, SharedDevice: true, Mode: "block", ReadOnly: true}},
		{req: api.Request{Serial: "0xc1af0002", Mode: "block", Size: "512Mi"}, want: askedText{Serial: "0xc1af0002", DeviceSource: "pool", Mode: "block", Size: "512Mi"}},
		{req: api.Request{Size: "256Mi", Mode: "block"}, want: askedText{Size: "256Mi", Mode: "block"}},
		{req: api.Request{Serial: "0xc1ae0001", Mode: "block"}, err: "memdev 0xc1ae0001: mode block needs a persistent device"},
		{req: api.Request{Serial: "0xc1ee0001", Mode: "block", ReadOnly: ptr(true)}, err: "memdev 0xc1ee0001: mode block needs a persistent device"},
		{req: api.Request{Serial: "0xc100e2e2", Share: true, ReadOnly: ptr(true), Mode: "block"}, err: "memdev 0xc100e2e2: mode block needs a persistent device"},
		{req: api.Request{Serial: "0xc1ee0001", Share: true, ReadOnly: ptr(true), Mode: "block"}, err: "memdev 0xc1ee0001: not shareable"},
	} {
		var a state.LeaseAsked
		tc.req.Op = api.OpRequest
		mustOK(t, tc.req.Validate())
		err := e.b.requestOptions(&a, &tc.req)
		switch {
		case tc.err != "" && (err == nil || err.Error() != tc.err):
			t.Errorf("%+v: expected error %q, got %v", tc.req, tc.err, err)
		case tc.err == "" && (err != nil || !reflect.DeepEqual(a, tc.want.asked())):
			t.Errorf("%+v: got %+v %v, expected %+v", tc.req, a, err, tc.want)
		}
	}
	if a := e.client.Actions()[calls:]; len(a) != 0 {
		t.Errorf("the requests called the API server: %v", a)
	}

	// without the pool driver only local memory can be leased
	e.b.opts.PoolDriver = ""
	for _, tc := range []struct {
		req  api.Request
		want askedText
		err  string
	}{
		{req: api.Request{Size: "1Gi", Source: "pool"}, err: "pool driver disabled (--pool=false)"},
		{req: api.Request{Size: "1Gi", Source: "pool", Share: true}, err: "pool driver disabled (--pool=false)"},
		{req: api.Request{Size: "1Gi", Source: "pool", Mode: "fs", ReadOnly: ptr(true)}, err: "pool driver disabled (--pool=false)"},
		// persistent local memdevs serve modes fs and block
		{req: api.Request{Size: "1Gi", Mode: "fs", ReadOnly: ptr(true)}, want: askedText{Size: "1Gi", Mode: "fs", ReadOnly: true}},
		{req: api.Request{Size: "1Gi", Mode: "block"}, want: askedText{Size: "1Gi", Mode: "block"}},
		// shared local devices serve --share
		{req: api.Request{Share: true, Size: "64Mi"}, want: askedText{Shared: true, SharedDevice: true, Size: "64Mi", Mode: "dax"}},
		{req: api.Request{Serial: "0xc100e2e2", Share: true}, want: askedText{Serial: "0xc100e2e2", Shared: true, SharedDevice: true, DeviceSource: "local", Mode: "dax"}},
		{req: api.Request{Size: "1Gi"}, want: askedText{Size: "1Gi", Mode: "numa"}},
		{req: api.Request{Size: "1Gi", Source: "local"}, want: askedText{Source: "local", Size: "1Gi", Mode: "numa"}},
	} {
		var a state.LeaseAsked
		tc.req.Op = api.OpRequest
		err := e.b.requestOptions(&a, &tc.req)
		switch {
		case tc.err != "" && (err == nil || err.Error() != tc.err):
			t.Errorf("no pool driver: %+v: expected error %q, got %v", tc.req, tc.err, err)
		case tc.err == "" && (err != nil || !reflect.DeepEqual(a, tc.want.asked())):
			t.Errorf("no pool driver: %+v: got %+v %v, expected %+v", tc.req, a, err, tc.want)
		}
	}
}

// askedText is what a lease asks for in words.
type askedText struct {
	Source, Mode, DeviceSource, Access, FSType, Name, Size, Serial string
	Shared, SharedDevice, ReadOnly                                 bool
}

// asked returns what a lease asks for.
func (a askedText) asked() state.LeaseAsked {
	serial, _ := api.ParseSerial(a.Serial)
	return state.LeaseAsked{Source: state.Source(a.Source), DeviceSource: state.Source(a.DeviceSource), Mode: state.Mode(a.Mode),
		Share: a.Shared, Shared: a.SharedDevice, ReadOnly: a.ReadOnly, Access: state.Access(a.Access),
		FSType: a.FSType, Name: a.Name, Size: a.Size, Serial: state.Serial(serial), SerialZero: a.Serial != "" && serial == 0}
}

// opaqueConfig is a companion claim config: its driver, the requests it
// applies to and its parameters.
type opaqueConfig struct {
	driver   string
	requests []string
	params   string
}

// companionConfigsOf returns the configs of a companion claim.
func companionConfigsOf(c *resourcev1.ResourceClaim) []opaqueConfig {
	var out []opaqueConfig
	for _, cfg := range c.Spec.Devices.Config {
		out = append(out, opaqueConfig{driver: cfg.Opaque.Driver, requests: cfg.Requests, params: string(cfg.Opaque.Parameters.Raw)})
	}
	return out
}

// A request of serial 0 asks for the device of serial 0: the selector of
// the companion claim, the response and the view say 0x0.
func TestSerialZero(t *testing.T) {
	e := brokerEnv(t)
	e.openClaim()
	e.start()
	resp, _ := e.call(api.Request{Op: api.OpRequest, Serial: "0", Wait: "0s"})
	if resp.Serial != "0x0" || resp.State != api.StatePending {
		t.Fatalf("response %+v", resp)
	}
	claim := e.waitClaim(CompanionName(resp.Lease))
	sel := claim.Spec.Devices.Requests[0].FirstAvailable[0].Selectors
	if len(sel) != 1 || sel[0].CEL.Expression != `device.attributes["cxl.nri.io"].serial == "0x0"` {
		t.Fatalf("selectors %+v", sel)
	}
	if l := e.stored(resp.Lease); l == nil || !l.Asked.AsksSerial() || l.Asked.Serial != 0 {
		t.Fatalf("stored lease %+v, want one that asks for serial 0x0", l)
	}
}

// A Ready lease whose device has serial 0 tells that serial and its
// environment.
func TestReadySerialZero(t *testing.T) {
	l := &state.Lease{ID: "000000a1", State: state.LeaseReady, Asked: state.LeaseAsked{Mode: state.ModeDax},
		Got: &state.LeaseGot{Source: state.SourcePool, Size: 1 << 28, Dax: "/dev/dax0.0"}}
	r := leaseInfo(l)
	if r.Serial != "0x0" || r.Env["CXL_"+api.Key(0, 1)+"_SERIAL"] != "0x0" {
		t.Fatalf("response %+v", r)
	}
}

func TestCompanionClaims(t *testing.T) {
	e := newSlicesBrokerEnv(t)
	pod := requester.Pod
	const (
		localNUMA = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"numa","readOnly":false}}`
		localDax  = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"dax","readOnly":false}}`
		localRO   = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"dax","readOnly":true}}`
		poolNUMA  = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"numa","readOnly":false}}`
		poolDax   = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"dax","readOnly":false}}`
		poolRO    = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"dax","readOnly":true}}`
		// no path: the companion pod gets no mount
		// readOnly explicitly, access and fsType only when requested
		poolFs       = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"fs","readOnly":true}}`
		poolFsCached = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"fs","readOnly":true,"access":"cached"}}`
		poolFsRW     = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"fs","readOnly":false,"access":"direct","fsType":"ext4"}}`
		localFs      = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"fs","readOnly":true}}`
		localBlock   = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"block","readOnly":false}}`
		poolBlock    = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"block","readOnly":false}}`
		poolBlockRO  = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"block","readOnly":true}}`
	)
	memory := func(q string) *resourcev1.CapacityRequirements {
		return &resourcev1.CapacityRequirements{Requests: map[resourcev1.QualifiedName]resource.Quantity{"memory": resource.MustParse(q)}}
	}
	share := &resourcev1.CapacityRequirements{Requests: map[resourcev1.QualifiedName]resource.Quantity{"shares": resource.MustParse("1")}}
	sel := func(exprs ...string) []resourcev1.DeviceSelector {
		var out []resourcev1.DeviceSelector
		for _, e := range exprs {
			out = append(out, resourcev1.DeviceSelector{CEL: &resourcev1.CELDeviceSelector{Expression: e}})
		}
		return out
	}
	serial := func(driver, serial string) string {
		return `device.attributes["` + driver + `"].serial == "` + serial + `"`
	}
	// the claim and the pod of every companion: owned by the requester
	// pod, the pod pinned to the node
	wantLabels := map[string]string{"cxl.nri.io/lease": "x", "cxl.nri.io/requester-uid": envPodUID, "cxl.nri.io/node": envNode}
	wantAnnotations := map[string]string{"cxl.nri.io/requester": "demo/requester/app"}
	owner := metav1.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: envPod, UID: envPodUID}
	claimOwner := owner
	claimOwner.Controller = ptr(true)
	res := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("16Mi")}
	wantPodSpec := corev1.PodSpec{
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{envNode}}}}},
		}}},
		Tolerations:                   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
		RestartPolicy:                 corev1.RestartPolicyAlways,
		AutomountServiceAccountToken:  ptr(false),
		EnableServiceLinks:            ptr(false),
		TerminationGracePeriodSeconds: ptr(int64(1)),
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(65535)), RunAsGroup: ptr(int64(65535)),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		ResourceClaims: []corev1.PodResourceClaim{{Name: "lease", ResourceClaimName: ptr("cxl-lease-x")}},
		Containers: []corev1.Container{{
			Name: "pause", Image: "registry.k8s.io/pause:3.10",
			Resources: corev1.ResourceRequirements{Requests: res, Limits: res},
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
				Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
		}},
	}
	for _, tc := range []struct {
		name    string
		req     api.Request
		noPool  bool
		want    resourcev1.DeviceRequest
		configs []opaqueConfig
	}{
		{
			name: "local numa",
			req:  api.Request{Size: "128Mi", Source: "local"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local", Capacity: memory("128Mi")}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localNUMA}},
		},
		{
			name: "local dax read-only of a device",
			req:  api.Request{Size: "64Mi", Source: "local", Mode: "dax", ReadOnly: ptr(true), Serial: "0xC100E2E1"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local", Capacity: memory("64Mi"), Selectors: sel(serial("cxl.nri.io", "0xc100e2e1"))}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localRO}},
		},
		{
			name: "pool numa of a device",
			req:  api.Request{Size: "256Mi", Source: "pool", Serial: "0xc1ee0001"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool", Capacity: memory("256Mi"), Selectors: sel(serial("cxl-pool.nri.io", "0xc1ee0001"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolNUMA}},
		},
		{
			name: "local, then pool, dax",
			req:  api.Request{Size: "64Mi", Mode: "dax"},
			want: resourcev1.DeviceRequest{Name: "lease", FirstAvailable: []resourcev1.DeviceSubRequest{
				{Name: "local", DeviceClassName: "cxl-local", Capacity: memory("64Mi")},
				{Name: "pool", DeviceClassName: "cxl-pool", Capacity: memory("64Mi")},
			}},
			configs: []opaqueConfig{
				{driver: "cxl.nri.io", requests: []string{"lease/local"}, params: localDax},
				{driver: "cxl-pool.nri.io", requests: []string{"lease/pool"}, params: poolDax},
			},
		},
		{
			name: "numa of a local device: its source only",
			req:  api.Request{Size: "64Mi", Serial: "0xc100e2e1"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local", Capacity: memory("64Mi"), Selectors: sel(serial("cxl.nri.io", "0xc100e2e1"))}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localNUMA}},
		},
		{
			name: "numa of a pool device: its source only",
			req:  api.Request{Size: "64Mi", Serial: "0xc1ee0001"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool", Capacity: memory("64Mi"), Selectors: sel(serial("cxl-pool.nri.io", "0xc1ee0001"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolNUMA}},
		},
		{
			name: "local, then pool, numa of a serial that no slice has",
			req:  api.Request{Size: "64Mi", Serial: "0x1234"},
			want: resourcev1.DeviceRequest{Name: "lease", FirstAvailable: []resourcev1.DeviceSubRequest{
				{Name: "local", DeviceClassName: "cxl-local", Capacity: memory("64Mi"), Selectors: sel(serial("cxl.nri.io", "0x1234"))},
				{Name: "pool", DeviceClassName: "cxl-pool", Capacity: memory("64Mi"), Selectors: sel(serial("cxl-pool.nri.io", "0x1234"))},
			}},
			configs: []opaqueConfig{
				{driver: "cxl.nri.io", requests: []string{"lease/local"}, params: localNUMA},
				{driver: "cxl-pool.nri.io", requests: []string{"lease/pool"}, params: poolNUMA},
			},
		},
		{
			name: "a share of a serial that no slice has, local then pool",
			req:  api.Request{Serial: "0x1234", Share: true},
			want: resourcev1.DeviceRequest{Name: "lease", FirstAvailable: []resourcev1.DeviceSubRequest{
				{Name: "local", DeviceClassName: "cxl-local-shared", Capacity: share, Selectors: sel(serial("cxl.nri.io", "0x1234"))},
				{Name: "pool", DeviceClassName: "cxl-pool-shared", Capacity: share, Selectors: sel(serial("cxl-pool.nri.io", "0x1234"))},
			}},
			configs: []opaqueConfig{
				{driver: "cxl.nri.io", requests: []string{"lease/local"}, params: localDax},
				{driver: "cxl-pool.nri.io", requests: []string{"lease/pool"}, params: poolDax},
			},
		},
		{
			name:   "local without the pool driver",
			req:    api.Request{Size: "64Mi"},
			noPool: true,
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local", Capacity: memory("64Mi")}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localNUMA}},
		},
		{
			name: "join a shared pool device",
			req:  api.Request{Serial: "0xc1ae0001", Share: true, Source: "pool"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-shared", Capacity: share, Selectors: sel(serial("cxl-pool.nri.io", "0xc1ae0001"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolDax}},
		},
		{
			name: "a reader of a shared pool device without --source",
			req:  api.Request{Serial: "0xc1ae0001", Share: true, ReadOnly: ptr(true)},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-shared", Capacity: share, Selectors: sel(serial("cxl-pool.nri.io", "0xc1ae0001"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolRO}},
		},
		{
			name: "a reader of a shared local device without --source",
			req:  api.Request{Serial: "0xc100e2e2", Share: true, ReadOnly: ptr(true)},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local-shared", Capacity: share, Selectors: sel(serial("cxl.nri.io", "0xc100e2e2"))}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localRO}},
		},
		{
			name: "a shared local device exclusively without --source",
			req:  api.Request{Serial: "0xc100e2e2"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local-shared", Selectors: sel(serial("cxl.nri.io", "0xc100e2e2"))}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localDax}},
		},
		{
			name: "any shared device of a size, local then pool",
			req:  api.Request{Share: true, Size: "256Mi"},
			want: resourcev1.DeviceRequest{Name: "lease", FirstAvailable: []resourcev1.DeviceSubRequest{
				{Name: "local", DeviceClassName: "cxl-local-shared", Capacity: share, Selectors: sel(`device.attributes["cxl.nri.io"].size >= 268435456`)},
				{Name: "pool", DeviceClassName: "cxl-pool-shared", Capacity: share, Selectors: sel(`device.attributes["cxl-pool.nri.io"].size >= 268435456`)},
			}},
			configs: []opaqueConfig{
				{driver: "cxl.nri.io", requests: []string{"lease/local"}, params: localDax},
				{driver: "cxl-pool.nri.io", requests: []string{"lease/pool"}, params: poolDax},
			},
		},
		{
			name: "a shared pool device exclusively (the writer)",
			req:  api.Request{Serial: "0xc1ae0001", Size: "128Mi", Source: "pool"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-shared",
				Selectors:       sel(serial("cxl-pool.nri.io", "0xc1ae0001"), `device.attributes["cxl-pool.nri.io"].size >= 134217728`)}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolDax}},
		},
		{
			name: "a shared pool device exclusively without --source",
			req:  api.Request{Serial: "0xc1ae0001"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-shared", Selectors: sel(serial("cxl-pool.nri.io", "0xc1ae0001"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolDax}},
		},
		{
			name: "a shared local device exclusively (the writer)",
			req:  api.Request{Serial: "0xc100e2e2", Source: "local"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local-shared", Selectors: sel(serial("cxl.nri.io", "0xc100e2e2"))}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localDax}},
		},
		{
			name: "a reader of a shared local device",
			req:  api.Request{Serial: "0xc100e2e2", Share: true, ReadOnly: ptr(true), Source: "local"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local-shared", Capacity: share, Selectors: sel(serial("cxl.nri.io", "0xc100e2e2"))}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localRO}},
		},
		{
			name: "a share of any shared local device of a size",
			req:  api.Request{Share: true, Size: "64Mi", Source: "local"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local-shared", Capacity: share, Selectors: sel(`device.attributes["cxl.nri.io"].size >= 67108864`)}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localDax}},
		},
		{
			name: "fs of a share of a shared persistent device",
			req:  api.Request{Serial: "0xc1af0001", Share: true, ReadOnly: ptr(true), Mode: "fs"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-persistent", Capacity: share, Selectors: sel(serial("cxl-pool.nri.io", "0xc1af0001"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolFs}},
		},
		{
			name: "fs cached of a shared persistent device, whole",
			req:  api.Request{Serial: "0xc1af0001", ReadOnly: ptr(true), Mode: "fs", Access: "cached", Name: "models"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-persistent", Selectors: sel(serial("cxl-pool.nri.io", "0xc1af0001"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolFsCached}},
		},
		{
			name: "fs of an exclusive persistent device",
			req:  api.Request{Serial: "0xc1af0002", ReadOnly: ptr(true), Mode: "fs", Size: "512Mi"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-persistent", Capacity: memory("512Mi"), Selectors: sel(serial("cxl-pool.nri.io", "0xc1af0002"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolFs}},
		},
		{
			name: "fs of a share of any persistent device of a size, local then pool",
			req:  api.Request{Share: true, ReadOnly: ptr(true), Mode: "fs", Size: "64Mi"},
			want: resourcev1.DeviceRequest{Name: "lease", FirstAvailable: []resourcev1.DeviceSubRequest{
				{Name: "local", DeviceClassName: "cxl-local-persistent", Capacity: share, Selectors: sel(`device.attributes["cxl.nri.io"].size >= 67108864`)},
				{Name: "pool", DeviceClassName: "cxl-pool-persistent", Capacity: share, Selectors: sel(`device.attributes["cxl-pool.nri.io"].size >= 67108864`)},
			}},
			configs: []opaqueConfig{
				{driver: "cxl.nri.io", requests: []string{"lease/local"}, params: localFs},
				{driver: "cxl-pool.nri.io", requests: []string{"lease/pool"}, params: poolFs},
			},
		},
		{
			name: "fs read-write of a share of a shared persistent device",
			req:  api.Request{Serial: "0xc1af0001", Share: true, ReadOnly: ptr(false), Mode: "fs", Access: "direct", FSType: "ext4"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-persistent", Capacity: share, Selectors: sel(serial("cxl-pool.nri.io", "0xc1af0001"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolFsRW}},
		},
		{
			name: "fs of a share of a persistent local memdev",
			req:  api.Request{Serial: "0xc100f5e1", Share: true, Mode: "fs"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local-persistent", Capacity: share, Selectors: sel(serial("cxl.nri.io", "0xc100f5e1"))}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localFs}},
		},
		{
			name:   "block of a persistent local memdev, whole, without the pool driver",
			req:    api.Request{Serial: "0xc100f5e1", Mode: "block", Size: "64Mi"},
			noPool: true,
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local-persistent",
				Selectors:       sel(serial("cxl.nri.io", "0xc100f5e1"), `device.attributes["cxl.nri.io"].size >= 67108864`)}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localBlock}},
		},
		{
			name: "block of a shared persistent device, whole (the writer)",
			req:  api.Request{Serial: "0xc1af0001", Mode: "block"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-persistent", Selectors: sel(serial("cxl-pool.nri.io", "0xc1af0001"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolBlock}},
		},
		{
			name: "block of a share of a shared persistent device, read-only",
			req:  api.Request{Serial: "0xc1af0001", Share: true, ReadOnly: ptr(true), Mode: "block"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-persistent", Capacity: share, Selectors: sel(serial("cxl-pool.nri.io", "0xc1af0001"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolBlockRO}},
		},
		{
			name: "block of an exclusive persistent device",
			req:  api.Request{Serial: "0xc1af0002", Mode: "block", Size: "512Mi"},
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-pool-persistent", Capacity: memory("512Mi"), Selectors: sel(serial("cxl-pool.nri.io", "0xc1af0002"))}},
			configs: []opaqueConfig{{driver: "cxl-pool.nri.io", params: poolBlock}},
		},
		{
			name:   "a share without the pool driver",
			req:    api.Request{Share: true, Size: "64Mi"},
			noPool: true,
			want: resourcev1.DeviceRequest{Name: "lease", Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "cxl-local-shared", Capacity: share, Selectors: sel(`device.attributes["cxl.nri.io"].size >= 67108864`)}},
			configs: []opaqueConfig{{driver: "cxl.nri.io", params: localDax}},
		},
	} {
		e.b.opts.PoolDriver = device.PoolDriverName
		if tc.noPool {
			e.b.opts.PoolDriver = ""
		}
		l := &state.Lease{ID: "x", Pod: pod, Container: envCtr}
		tc.req.Op = api.OpRequest
		mustOK(t, tc.req.Validate())
		mustOK(t, e.b.requestOptions(&l.Asked, &tc.req))
		claim, cpod, err := e.b.companionObjects(l)
		mustOK(t, err)
		if got := claim.Spec.Devices.Requests; len(got) != 1 || !reflect.DeepEqual(got[0], tc.want) {
			data, _ := json.Marshal(got)
			t.Errorf("%s: requests %s", tc.name, data)
		}
		if got := companionConfigsOf(claim); !reflect.DeepEqual(got, tc.configs) {
			t.Errorf("%s: configs\n%+v\nexpected\n%+v", tc.name, got, tc.configs)
		}
		if claim.Name != "cxl-lease-x" || claim.Namespace != envNS || !reflect.DeepEqual(claim.Labels, wantLabels) ||
			!reflect.DeepEqual(claim.Annotations, wantAnnotations) || !reflect.DeepEqual(claim.OwnerReferences, []metav1.OwnerReference{claimOwner}) {
			t.Errorf("%s: claim %s/%s labels %v annotations %v owners %+v", tc.name, claim.Namespace, claim.Name, claim.Labels, claim.Annotations, claim.OwnerReferences)
		}
		if cpod.Name != claim.Name || cpod.Namespace != envNS || !reflect.DeepEqual(cpod.Labels, wantLabels) ||
			!reflect.DeepEqual(cpod.Annotations, wantAnnotations) || !reflect.DeepEqual(cpod.OwnerReferences, []metav1.OwnerReference{owner}) {
			t.Errorf("%s: pod %s/%s labels %v annotations %v owners %+v", tc.name, cpod.Namespace, cpod.Name, cpod.Labels, cpod.Annotations, cpod.OwnerReferences)
		}
		if !reflect.DeepEqual(cpod.Spec, wantPodSpec) {
			data, _ := json.Marshal(cpod.Spec)
			t.Errorf("%s: pod spec %s", tc.name, data)
		}
	}
	// pool memory without the pool driver
	e.b.opts.PoolDriver = ""
	for _, a := range []state.LeaseAsked{
		{Source: state.SourcePool, Share: true, Mode: state.ModeDax},
		{Source: state.SourcePool, Share: true, ReadOnly: true, Mode: state.ModeFS, Access: state.AccessDirect},
		{Mode: state.ModeBlock, Serial: serialPoolP, Shared: true, DeviceSource: state.SourcePool},
	} {
		_, _, err := e.b.companionObjects(&state.Lease{ID: "x", Asked: a, Pod: pod})
		wantErrText(t, err, "pool driver disabled (--pool=false)")
	}
}
