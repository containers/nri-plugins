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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	nriapi "github.com/containerd/nri/pkg/api"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	cdiSpecs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/containers"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/engine"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

const (
	testNode = "node1"
	testNS   = "demo"
	// localSerial is the serial of the local memdev of the tests, mem0
	// under host bridge 12 of the fake kernel, published as
	// localDeviceName.
	localSerial     = 0xc100e2e1
	localDeviceName = "memdev-c100e2e1"
	// poolSerial is the serial of the pool device of the tests, pooled0
	// in the ResourceSlice of the pool driver.
	poolSerial = 0xc1ee0001
	mib        = 1 << 20
	// localBlockSize is the memory block size of the fake kernel.
	localBlockSize = 128 * mib
	// localDaxConfig is the config of a claim of mode dax.
	localDaxConfig = `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"dax"}}`
)

// driverTestEnv is a node for the tests of the driver as run starts it:
// newDriver over the fake kernel of pkg/cxl/kernel/fake, a fake clientset
// and a fake NRI runtime, with the plugin, registry and CDI directories in
// a temporary directory. The kernel has the local memdev localSerial and
// the pool device poolSerial; the clientset has the node and the
// ResourceSlice of the pool device. The kernel, the clientset and the
// files outlive a restart.
type driverTestEnv struct {
	t      *testing.T
	k      *fake.Fake
	client *k8sfake.Clientset
	config *pluginConfig
	// runtime is the NRI connection of the driver.
	runtime *fakeRuntime
	// d is the running driver, nil while it is stopped.
	d *driver
	// base is the command log of the fake before the driver started.
	base string
	// ctrs counts the containers that the env created.
	ctrs int
}

// fakeRuntime is the NRI stub of the env: Start calls onStart, if set,
// and returns its error.
type fakeRuntime struct {
	onStart func() error
}

func (f *fakeRuntime) Start(context.Context) error {
	if f.onStart != nil {
		return f.onStart()
	}
	return nil
}

func (f *fakeRuntime) Stop() {}

func (f *fakeRuntime) UpdateContainers([]*nriapi.ContainerUpdate) ([]*nriapi.ContainerUpdate, error) {
	return nil, nil
}

// newDriverTestEnv returns a node with the driver stopped. The test may
// change e.config before start.
func newDriverTestEnv(t *testing.T) *driverTestEnv {
	t.Helper()
	root := t.TempDir()
	e := &driverTestEnv{t: t, k: fake.New(t), client: sliceClient(), runtime: &fakeRuntime{}}
	e.k.Plug(fake.MemdevSpec{Serial: localSerial, Ram: 1 << 30})
	e.k.Plug(fake.MemdevSpec{Serial: poolSerial})
	e.base = e.k.Commands()
	e.config = &pluginConfig{
		Flags: &flags{NodeName: testNode, CDIRoot: filepath.Join(root, "cdi"), Pool: true,
			PoolPrepareTimeout: 5 * time.Second, LeaseCLI: filepath.Join(root, "cxl-request"), LeaseWait: time.Second},
		Client:                    e.client,
		LeaseClient:               e.client,
		Kernel:                    e.k.Kernel(),
		NRIStub:                   e.runtime,
		KubeletPluginDir:          filepath.Join(root, "plugins", device.DriverName),
		KubeletPluginsRegistryDir: filepath.Join(root, "plugins_registry"),
	}
	for _, d := range []string{e.config.KubeletPluginDir, e.config.KubeletPluginsRegistryDir, e.config.Flags.CDIRoot} {
		noErr(t, os.MkdirAll(d, 0o750))
	}
	old := newKernelEvents
	newKernelEvents = func(time.Duration) (kernelEvents, error) { return &fakeEvents{}, nil }
	t.Cleanup(func() {
		e.stop()
		newKernelEvents = old
	})
	return e
}

// sliceClient returns a fake clientset with the node testNode and the
// ResourceSlice of the pool device poolSerial. Its creates and updates of
// ResourceSlices do what the API server does: a name from GenerateName
// and a resource version.
func sliceClient() *k8sfake.Clientset {
	serial := fmt.Sprintf("0x%x", poolSerial)
	pool := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-cxl-pool"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   device.PoolDriverName,
			Pool:     resourcev1.ResourcePool{Name: "fake-cxl-pool", ResourceSliceCount: 1},
			AllNodes: ptr(true),
			Devices:  []resourcev1.Device{{Name: "pooled0", Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{device.AttrSerial: {StringValue: &serial}}}},
		},
	}
	client := k8sfake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNode, UID: "uid-" + testNode}}, pool)
	var mu sync.Mutex
	rv := 0
	stored := func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		rs := a.(interface{ GetObject() runtime.Object }).GetObject().(*resourcev1.ResourceSlice)
		rv++
		if rs.Name == "" && rs.GenerateName != "" {
			rs.Name = rs.GenerateName + strconv.Itoa(rv)
		}
		rs.ResourceVersion = strconv.Itoa(rv)
		return false, nil, nil // the tracker stores it
	}
	client.PrependReactor("create", "resourceslices", stored)
	client.PrependReactor("update", "resourceslices", stored)
	return client
}

// start starts the driver as run does.
func (e *driverTestEnv) start() {
	e.t.Helper()
	d, err := newDriver(context.Background(), e.config)
	noErr(e.t, err)
	e.d = d
	e.check("started")
}

// stop shuts the driver down; nothing if it is stopped.
func (e *driverTestEnv) stop() {
	if e.d != nil {
		e.d.Shutdown()
		e.d = nil
	}
}

// restart stops the driver and starts it again.
func (e *driverTestEnv) restart() {
	e.t.Helper()
	e.stop()
	e.start()
}

// synchronize synchronizes the NRI plugin of the driver with a runtime
// that has no containers.
func (e *driverTestEnv) synchronize() {
	e.t.Helper()
	_, err := e.d.nri.Synchronize(context.Background(), nil, nil)
	noErr(e.t, err)
}

// prepare prepares the claim with the driver p as kubelet does, and checks
// the invariants after it.
func (e *driverTestEnv) prepare(p *dra.Plugin, claim *resourcev1.ResourceClaim) error {
	e.t.Helper()
	res, err := p.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{claim})
	noErr(e.t, err)
	e.check("prepared " + string(claim.UID))
	return res[claim.UID].Err
}

// unprepare unprepares the claim uid with the driver p as kubelet does,
// and checks the invariants after it.
func (e *driverTestEnv) unprepare(p *dra.Plugin, uid string) error {
	e.t.Helper()
	claim := kubeletplugin.NamespacedObject{UID: types.UID(uid)}
	if info := p.ClaimInfo(uid); info != nil {
		claim.Namespace, claim.Name, _ = strings.Cut(info.ClaimName, "/")
	}
	res, err := p.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{claim})
	noErr(e.t, err)
	e.check("unprepared " + uid)
	return res[types.UID(uid)]
}

// mustPrepare prepares a claim, failing the test on an error.
func (e *driverTestEnv) mustPrepare(p *dra.Plugin, claim *resourcev1.ResourceClaim) {
	e.t.Helper()
	if err := e.prepare(p, claim); err != nil {
		e.t.Fatalf("prepare %s: %v", claim.UID, err)
	}
}

// mustUnprepare unprepares a claim, failing the test on an error.
func (e *driverTestEnv) mustUnprepare(p *dra.Plugin, uid string) {
	e.t.Helper()
	if err := e.unprepare(p, uid); err != nil {
		e.t.Fatalf("unprepare %s: %v", uid, err)
	}
}

// check fails the test on a violation of the invariants of the records and
// the kernel now, and on a change of a kernel object that no row of its
// table has since the last check.
func (e *driverTestEnv) check(what string) {
	e.t.Helper()
	eng := e.d.local.Engine()
	snap, err := eng.Observe(context.Background())
	noErr(e.t, err)
	for _, v := range eng.Check(snap, []state.Source{state.SourceLocal, state.SourcePool}, nil) {
		e.t.Errorf("%s: %s", what, v)
	}
	for _, line := range eng.UnknownSeen() {
		e.t.Errorf("%s: %s%s", what, engine.UnknownTransition, line)
	}
}

// records returns the records of the store of the driver.
func (e *driverTestEnv) records() *state.Records {
	return e.d.local.Records()
}

// grant returns the grant of result n of the claim uid, nil if there is
// none.
func (e *driverTestEnv) grant(uid string, n int) *state.Grant {
	return e.records().Grant(state.GrantRef{Claim: uid, Result: n})
}

// commands returns the commands that the fake ran for the driver.
func (e *driverTestEnv) commands() string {
	return e.k.CommandsSince(e.base)
}

// wantClean fails the test unless nothing is prepared: no claim, no record
// of the engine, no region in the kernel, no CDI device.
func (e *driverTestEnv) wantClean() {
	e.t.Helper()
	r := e.records()
	if len(r.Preparations)+len(r.Grants)+len(r.Memdevs)+len(r.Regions) > 0 {
		e.t.Fatalf("records left: preparations %+v grants %+v memdevs %+v regions %+v", r.Preparations, r.Grants, r.Memdevs, r.Regions)
	}
	snap, err := e.k.Kernel().Observe(context.Background())
	noErr(e.t, err)
	for _, ri := range snap.Regions {
		e.t.Errorf("region %s (%s) left", ri.Name, ri.State)
	}
	for _, name := range []string{device.ClaimsCDISpecName, device.PoolCDISpecName} {
		data, err := os.ReadFile(filepath.Join(e.config.Flags.CDIRoot, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		noErr(e.t, err)
		spec := &cdiSpecs.Spec{}
		noErr(e.t, json.Unmarshal(data, spec))
		if len(spec.Devices) > 0 {
			e.t.Errorf("CDI devices left in %s: %+v", name, spec.Devices)
		}
	}
}

// createContainer creates a container <namespace>/pod/ctr with the NRI
// plugin of the driver, as the runtime does. Its env has the markers of
// the results of the claim uid of cxl.nri.io, and the namespace is the
// claim's. It returns the record of the container and the error of its
// creation.
func (e *driverTestEnv) createContainer(uid string, results ...int) (containers.Container, error) {
	e.t.Helper()
	e.ctrs++
	pod := &nriapi.PodSandbox{Id: "sb", Uid: "pod-uid", Namespace: testNS, Name: "pod"}
	if info := e.d.local.ClaimInfo(uid); info != nil {
		pod.Namespace, _, _ = strings.Cut(info.ClaimName, "/")
	}
	ctr := &nriapi.Container{Id: fmt.Sprintf("ctr-%d", e.ctrs), PodSandboxId: "sb", Name: "ctr"}
	for _, n := range results {
		ctr.Env = append(ctr.Env, api.ResultMarker(uid, n)+"=1")
	}
	_, _, err := e.d.nri.CreateContainer(context.Background(), pod, ctr)
	c, _ := e.d.nri.Record(ctr.Id)
	return c, err
}

// makeOpaqueConfig returns an allocation config of the driver with config
// as its opaque parameters.
func makeOpaqueConfig(driver string, source resourcev1.AllocationConfigSource, config any) resourcev1.DeviceAllocationConfiguration {
	raw, _ := json.Marshal(config)
	return resourcev1.DeviceAllocationConfiguration{
		Source: source,
		DeviceConfiguration: resourcev1.DeviceConfiguration{
			Opaque: &resourcev1.OpaqueDeviceConfiguration{Driver: driver, Parameters: runtime.RawExtension{Raw: raw}},
		},
	}
}

// localClaim returns a claim allocated the local memdev for request "mem",
// consuming bytes of it, with opaque cxl.nri.io claim configs.
func localClaim(uid string, consumed int64, configs ...string) *resourcev1.ResourceClaim {
	r := resourcev1.DeviceRequestAllocationResult{Request: "mem", Driver: device.DriverName, Pool: testNode, Device: localDeviceName,
		ConsumedCapacity: map[resourcev1.QualifiedName]resource.Quantity{device.CapacityMemory: *resource.NewQuantity(consumed, resource.BinarySI)}}
	c := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid), Name: "claim-" + uid, Namespace: testNS},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{Results: []resourcev1.DeviceRequestAllocationResult{r}},
			},
		},
	}
	for _, params := range configs {
		c.Status.Allocation.Devices.Config = append(c.Status.Allocation.Devices.Config,
			makeOpaqueConfig(device.DriverName, resourcev1.AllocationConfigSourceClaim, json.RawMessage(params)))
	}
	return c
}

// poolClaim returns a claim allocated the pool device pooled0 for request
// "mem", exclusive, with the status entry of the pool controller.
func poolClaim(uid string) *resourcev1.ResourceClaim {
	data := fmt.Sprintf(`{"serial":"0x%x","shared":false,"size":268435456,"host":%q}`, poolSerial, testNode)
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid), Name: "claim-" + uid, Namespace: testNS},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{{Request: "mem", Driver: device.PoolDriverName, Pool: "fake-cxl-pool", Device: "pooled0"}},
				},
			},
			Devices: []resourcev1.AllocatedDeviceStatus{
				{Driver: device.PoolDriverName, Pool: "fake-cxl-pool", Device: "pooled0", Data: &runtime.RawExtension{Raw: []byte(data)}},
			},
		},
	}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T {
	return &v
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected an error with %q, got %v", substr, err)
	}
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// kernelAt returns the kernel of the node with its sysfs in root/sys.
func kernelAt(root string) *kernel.Kernel {
	k := kernel.New()
	k.SysRoot = filepath.Join(root, "sys")
	return k
}
