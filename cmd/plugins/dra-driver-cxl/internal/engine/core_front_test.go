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

package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	cdiSpecs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/engine"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/lend"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

const (
	// frontNode and frontNS are the node and the namespace of the claims,
	// frontPool the DRA pool of the pool devices.
	frontNode = "node1"
	frontNS   = "demo"
	frontPool = "fake-cxl-pool"
	// the requester of the leases of companion claims
	frontPod    = "requester"
	frontPodUID = "pod-uid-1"
	frontCtr    = "app"
)

// The core model (engine.RunCoreModel) with kubelet's requests through both
// DRA drivers of the process over the engine and the store: the real
// Preparation steps, the CDI specs checked against the records at every
// step, companion claims lent through fakes of the lease broker and of NRI,
// and the process stopped at every commit of the store. The flags of
// TestCoreModel plan and replay a run.
func TestFrontModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	engine.RunCoreModel(t, func(f *fake.Fake, dir string) engine.CoreFront { return newFrontEnd(t, f, dir) })
}

// frontEnd runs the requests of the core model as kubelet does, through the
// plugins of cxl.nri.io and of the pool driver of the process; the fakes of
// the lease broker and of NRI outlive a restart, NRI's lends not a reboot.
type frontEnd struct {
	t   *testing.T
	f   *fake.Fake
	dir string
	// leaseFsRoot is the directory of the lease binds of the engine.
	leaseFsRoot string
	bootID      string

	st    *store.Store
	node  *dra.Node
	nodes *frontNodes
	hooks *frontHooks
}

// newFrontEnd returns the front-end of the node of the fake kernel f with
// its files in dir.
func newFrontEnd(t *testing.T, f *fake.Fake, dir string) *frontEnd {
	fe := &frontEnd{t: t, f: f, dir: dir, bootID: f.BootID()}
	fe.nodes = &frontNodes{}
	fe.hooks = &frontHooks{fe: fe}
	for _, d := range []string{fe.cdiDir(), fe.pluginDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return fe
}

// cdiDir returns the directory of the CDI specs.
func (fe *frontEnd) cdiDir() string {
	return filepath.Join(fe.dir, "cdi")
}

// pluginDir returns the plugin data directory of cxl.nri.io.
func (fe *frontEnd) pluginDir() string {
	return filepath.Join(fe.dir, device.DriverName)
}

// NewEngine returns the engine of the drivers of a start, as the process
// makes them (dra.NewNode) from the options o of the engine. NRI's lends
// do not outlive a reboot.
func (fe *frontEnd) NewEngine(o engine.Options) (*engine.Engine, error) {
	if b := fe.f.BootID(); b != fe.bootID {
		fe.bootID = b
		fe.nodes.clear()
	}
	shared, err := dra.ParseMemdevSpecs(frontShared(o))
	if err != nil {
		return nil, err
	}
	node, err := dra.NewNode(dra.NodeOptions{
		Kernel:      o.Kernel,
		Store:       o.Store,
		Dir:         fe.pluginDir(),
		PoolDriver:  device.PoolDriverName,
		LeaseFsRoot: o.LeaseFsRoot,
		CDIDir:      fe.cdiDir(),
		NodeName:    frontNode,
		Local: dra.LocalConfig{Managed: true, UsableSize: func(string, uint64) uint64 { return 0 },
			Coherent: func(string, uint64) bool { return false }},
		Publish:         dra.PublishConfig{ManageMemdevs: true, SharedDevices: shared},
		Scan:            fe.scan,
		Fence:           o.Fence,
		FSTypes:         o.FSTypes,
		ProbeAddressing: o.ProbeAddressing,
		Timeouts:        o.Timeouts,
	})
	if err != nil {
		return nil, err
	}
	fe.st, fe.node, fe.leaseFsRoot = o.Store, node, o.LeaseFsRoot
	return node.Engine, nil
}

// Recover recovers the records of a start, writes the CDI specs from them
// and sets the lenders, as the process does.
func (fe *frontEnd) Recover(ctx context.Context) error {
	if _, err := fe.node.Recover(ctx); err != nil {
		return err
	}
	if err := fe.node.WriteCDI(); err != nil {
		return err
	}
	fe.node.SetLenders(fe.nodes, fe.hooks)
	return nil
}

// frontShared returns the local memdevs of the core model that are shared
// with the options o, as the configuration lists them.
func frontShared(o engine.Options) []string {
	var shared []string
	for _, m := range engine.CoreMemdevs() {
		if m.Source == state.SourceLocal && o.Shared != nil && o.Shared(state.Serial(m.Spec.Serial)) {
			shared = append(shared, api.FormatSerial(m.Spec.Serial))
		}
	}
	return shared
}

// scan returns the local memdevs of the core model as a scan of the
// publisher finds them, ready.
func (fe *frontEnd) scan(context.Context) (*kernel.Snapshot, error) {
	snap := &kernel.Snapshot{}
	for _, m := range engine.CoreMemdevs() {
		if m.Source == state.SourceLocal {
			snap.Memdevs = append(snap.Memdevs, kernel.MemdevInfo{Name: fe.f.Memdev(m.Spec.Serial), Serial: m.Spec.Serial,
				State: kernel.MemdevReady, RamSize: m.Spec.Ram, PmemSize: m.Spec.Pmem})
		}
	}
	return snap, nil
}

// plugin returns the plugin of the source.
func (fe *frontEnd) plugin(source state.Source) *dra.Plugin {
	if source == state.SourcePool {
		return fe.node.Pool
	}
	return fe.node.Local
}

// Prepare runs kubelet's Prepare of the claim uid of a slot.
func (fe *frontEnd) Prepare(sl engine.CoreSlot, uid string) error {
	claim := fe.claim(sl, uid)
	res, err := fe.plugin(sl.Source).PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{claim})
	if err != nil {
		return err
	}
	return res[claim.UID].Err
}

// Unprepare runs kubelet's Unprepare of the claim uid of a slot.
func (fe *frontEnd) Unprepare(sl engine.CoreSlot, uid string) error {
	res, err := fe.plugin(sl.Source).UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{{UID: types.UID(uid)}})
	if err != nil {
		return err
	}
	return res[types.UID(uid)]
}

// leaseOf returns the lease of the companion claim uid.
func leaseOf(uid string) string {
	return "lease-" + uid
}

// claim returns the claim uid of a slot as the scheduler allocated it: a
// pool device with the status data of its controller, or a local memdev of
// the node with the size (local) or the shares it consumed; the
// MemoryConfig of the slot's mode; a companion claim of its lease.
func (fe *frontEnd) claim(sl engine.CoreSlot, uid string) *resourcev1.ResourceClaim {
	c := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid), Name: "claim-" + uid, Namespace: frontNS},
		Status:     resourcev1.ResourceClaimStatus{Allocation: &resourcev1.AllocationResult{}},
	}
	r := resourcev1.DeviceRequestAllocationResult{Request: "mem", Driver: device.DriverName, Pool: frontNode, Device: dra.MemdevDeviceName(sl.Serial)}
	shares := func(n int64) map[resourcev1.QualifiedName]resource.Quantity {
		return map[resourcev1.QualifiedName]resource.Quantity{device.CapacityShares: *resource.NewQuantity(n, resource.DecimalSI)}
	}
	switch {
	case sl.Source == state.SourcePool:
		r.Driver, r.Pool, r.Device = device.PoolDriverName, frontPool, "pooled-"+sl.ID
		data := fmt.Sprintf(`{"serial":"%s","shared":%v,"size":268435456,"persistent":%v}`, api.FormatSerial(sl.Serial),
			sl.Kind.Shared(), sl.Kind.Persistent())
		if sl.Kind.Shared() {
			r.ConsumedCapacity = shares(1)
		}
		c.Status.Devices = []resourcev1.AllocatedDeviceStatus{{Driver: r.Driver, Pool: r.Pool, Device: r.Device,
			Data: &runtime.RawExtension{Raw: []byte(data)}}}
	case sl.Kind == state.KindLocal:
		r.ConsumedCapacity = map[resourcev1.QualifiedName]resource.Quantity{device.CapacityMemory: *resource.NewQuantity(256<<20, resource.BinarySI)}
	case sl.Kind.Persistent() && !sl.ReadOnly:
		r.ConsumedCapacity = shares(device.SharesUnlimited)
	default:
		r.ConsumedCapacity = shares(1)
	}
	c.Status.Allocation.Devices.Results = []resourcev1.DeviceRequestAllocationResult{r}
	dev := map[string]any{"mode": string(sl.Mode)}
	if sl.Mode == state.ModeFS || sl.Mode == state.ModeBlock {
		dev["readOnly"] = sl.ReadOnly
	}
	params, _ := json.Marshal(map[string]any{"apiVersion": "cxl.nri.io/v1alpha1", "kind": "MemoryConfig", "device": dev})
	c.Status.Allocation.Devices.Config = []resourcev1.DeviceAllocationConfiguration{{
		Source: resourcev1.AllocationConfigSourceClaim,
		DeviceConfiguration: resourcev1.DeviceConfiguration{Opaque: &resourcev1.OpaqueDeviceConfiguration{
			Driver: r.Driver, Parameters: runtime.RawExtension{Raw: params},
		}},
	}}
	if sl.Lease {
		id := leaseOf(uid)
		c.Name = "cxl-lease-" + id
		c.Labels = map[string]string{device.LeaseLabel: id}
		controller := true
		c.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: frontPod, UID: frontPodUID, Controller: &controller}}
	}
	return c
}

// Check returns the violations of the CDI specs, the lends and the lease binds
// against the records and the kernel now (design.md, How a restart recovers):
// the spec of each driver has the devices of the results of its PREPARED and
// UNPREPARING claims that are not companions, each naming the dax device, the
// block device or the node mount that the records and the kernel have; a NUMA
// node is lent only for a grant that records the lend, and every prepared numa
// companion's node is lent; a lease bind is mounted only for a grant that
// records it, and every prepared fs companion's bind is mounted.
func (fe *frontEnd) Check() []string {
	doc := fe.st.Snapshot()
	snap, err := fe.f.Kernel().Observe(context.Background())
	if err != nil {
		return []string{"observe: " + err.Error()}
	}
	var out []string
	for _, p := range []*dra.Plugin{fe.node.Local, fe.node.Pool} {
		out = append(out, fe.checkCDI(doc, snap, p.Rules())...)
	}
	return append(out, fe.checkLends(doc, snap)...)
}

// checkCDI returns the violations of the CDI spec of the driver of rules.
func (fe *frontEnd) checkCDI(doc *store.Document, snap *kernel.Snapshot, rules dra.SourceRules) []string {
	var out []string
	devs := map[string]*cdiSpecs.Device{}
	spec, err := fe.cdiSpec(rules.CDISpecName())
	if err != nil {
		return []string{err.Error()}
	}
	if spec != nil {
		for i := range spec.Devices {
			devs[spec.Devices[i].Name] = &spec.Devices[i]
		}
	}
	r := &doc.Records
	for i := range r.Preparations {
		prep := &r.Preparations[i]
		if prep.Driver != rules.Name {
			continue
		}
		for _, res := range prep.Results {
			name := dra.ClaimCDIDeviceName(prep.UID, res.Index)
			d := devs[name]
			delete(devs, name)
			switch {
			case prep.Lease == "" && prep.State == state.PreparationPrepared:
				if d == nil {
					out = append(out, fmt.Sprintf("%s: no CDI device %s of PREPARED claim %s", rules.Name, name, prep.UID))
					continue
				}
				out = append(out, fe.checkCDIDevice(r, snap, d, prep, &res)...)
			case d != nil && (prep.Lease != "" || prep.State != state.PreparationUnpreparing):
				out = append(out, fmt.Sprintf("%s: CDI device %s of %s claim %s (lease %q)", rules.Name, name, prep.State, prep.UID, prep.Lease))
			}
		}
	}
	for name := range devs {
		out = append(out, fmt.Sprintf("%s: CDI device %s of no claim", rules.Name, name))
	}
	return out
}

// checkCDIDevice returns the violations of the CDI device d of the result
// res of a PREPARED claim: its marker, and the device node or the mount of
// what the grant got, as the records and the kernel have it.
func (fe *frontEnd) checkCDIDevice(r *state.Records, snap *kernel.Snapshot, d *cdiSpecs.Device, prep *state.Preparation,
	res *state.Result) []string {
	var out []string
	bad := func(format string, args ...any) {
		out = append(out, fmt.Sprintf("CDI device %s: ", d.Name)+fmt.Sprintf(format, args...))
	}
	if marker := api.ResultMarker(prep.UID, res.Index) + "=1"; !slices.Contains(d.ContainerEdits.Env, marker) {
		bad("no %s in %v", marker, d.ContainerEdits.Env)
	}
	if res.Grant == nil {
		return out
	}
	g := r.Grant(*res.Grant)
	if g == nil {
		bad("no grant %s", *res.Grant)
		return out
	}
	nodes, mounts := d.ContainerEdits.DeviceNodes, d.ContainerEdits.Mounts
	switch g.Asked.Mode {
	case state.ModeDax:
		path := filepath.Join(fe.f.DevRoot(), g.Got.Dax)
		var kd *kernel.DaxDeviceInfo
		for i := range snap.DaxDevices {
			if snap.DaxDevices[i].Name == g.Got.Dax {
				kd = &snap.DaxDevices[i]
			}
		}
		switch {
		case kd == nil:
			bad("the kernel has no dax device %q of grant %s", g.Got.Dax, g.Ref)
		case len(nodes) != 1 || nodes[0].Path != path || nodes[0].Major != int64(kd.Major) || nodes[0].Minor != int64(kd.Minor) ||
			(nodes[0].Permissions == "r") != g.Asked.ReadOnly:
			bad("device nodes %s, the kernel has %s %d:%d (read-only %v)", nodeList(nodes), path, kd.Major, kd.Minor, g.Asked.ReadOnly)
		}
	case state.ModeBlock:
		bd := g.Got.BlockDevice
		var kb *kernel.BlockDevice
		for i := range snap.Devices {
			if bd != nil && filepath.Base(snap.Devices[i].Path) == filepath.Base(bd.Name) {
				kb = &snap.Devices[i]
			}
		}
		switch {
		case kb == nil:
			bad("the kernel has no block device of grant %s (%+v)", g.Ref, bd)
		case len(nodes) != 1 || nodes[0].Path != bd.Name || nodes[0].Major != int64(kb.Major) || nodes[0].Minor != int64(kb.Minor) ||
			(nodes[0].Permissions == "r") != g.Asked.ReadOnly:
			bad("device nodes %s, the kernel has %s %d:%d (read-only %v)", nodeList(nodes), kb.Path, kb.Major, kb.Minor, g.Asked.ReadOnly)
		}
	case state.ModeFS:
		m := r.MountOf(g)
		switch {
		case m == nil:
			bad("grant %s has no node mount", g.Ref)
		case !slices.ContainsFunc(snap.Mounts, func(mi kernel.MountInfo) bool { return mi.Target == m.Path }):
			bad("the kernel has no node mount %s", m.Path)
		case len(mounts) != 1 || mounts[0].HostPath != m.Path || slices.Contains(mounts[0].Options, "ro") != g.Asked.ReadOnly:
			bad("mounts %+v, the node mount %s (read-only %v)", mounts, m.Path, g.Asked.ReadOnly)
		}
	default:
		if len(nodes)+len(mounts) > 0 {
			bad("device nodes %s, mounts %+v of mode %s", nodeList(nodes), mounts, g.Asked.Mode)
		}
	}
	return out
}

// nodeList returns the device nodes as text.
func nodeList(nodes []*cdiSpecs.DeviceNode) string {
	var out []string
	for _, n := range nodes {
		out = append(out, fmt.Sprintf("%s %d:%d %q", n.Path, n.Major, n.Minor, n.Permissions))
	}
	return "[" + strings.Join(out, " ") + "]"
}

// cdiSpec returns the CDI spec of the name on disk, nil if there is none.
func (fe *frontEnd) cdiSpec(name string) (*cdiSpecs.Spec, error) {
	path := filepath.Join(fe.cdiDir(), name)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	spec := &cdiSpecs.Spec{}
	if err := json.Unmarshal(data, spec); err != nil {
		return nil, fmt.Errorf("CDI spec %s: %w", path, err)
	}
	return spec, nil
}

// checkLends returns the violations of the lends of NRI and of the lease
// binds of the kernel against the grants of the companion claims.
func (fe *frontEnd) checkLends(doc *store.Document, snap *kernel.Snapshot) []string {
	r := &doc.Records
	var out []string
	recorded := map[string]bool{}
	binds := map[string]bool{}
	for i := range r.Grants {
		g := &r.Grants[i]
		if l := g.Got.Lend; l != nil && l.NUMANode != nil {
			recorded[fmt.Sprintf("%d %s", *l.NUMANode, l.Lease)] = true
		}
		if b := g.Got.Bind; b != nil && b.State != state.BindAbsent {
			binds[b.Target] = true
		}
	}
	lent := fe.nodes.lends()
	for _, l := range lent {
		if !recorded[l] {
			out = append(out, fmt.Sprintf("NRI lends node and lease %s that no grant records", l))
		}
	}
	mounted := map[string]bool{}
	for _, mi := range snap.Mounts {
		if strings.HasPrefix(mi.Target, fe.leaseFsRoot+"/") {
			mounted[mi.Target] = true
			if !binds[mi.Target] {
				out = append(out, fmt.Sprintf("lease bind %s that no grant records", mi.Target))
			}
		}
	}
	for i := range r.Preparations {
		prep := &r.Preparations[i]
		if prep.Lease == "" || prep.State != state.PreparationPrepared {
			continue
		}
		for _, res := range prep.Results {
			g := r.Grant(*res.Grant)
			switch {
			case g == nil:
			case g.Asked.Mode == state.ModeNUMA && g.Got.NUMANode != nil &&
				!slices.Contains(lent, fmt.Sprintf("%d %s", *g.Got.NUMANode, prep.Lease)):
				out = append(out, fmt.Sprintf("node %d of PREPARED companion %s is not lent (lent %v)", *g.Got.NUMANode, prep.UID, lent))
			case g.Asked.Mode == state.ModeFS && (g.Got.Bind == nil || !mounted[g.Got.Bind.Target]):
				out = append(out, fmt.Sprintf("the lease bind of PREPARED companion %s is not mounted (%+v)", prep.UID, g.Got.Bind))
			}
		}
	}
	return out
}

// frontNodes is NRI's lender of NUMA nodes to the containers of leases.
type frontNodes struct {
	mu   sync.Mutex
	lent map[string]bool // "<node> <holder>"
}

// LendNUMANode lends a node to the container on behalf of holder.
func (n *frontNodes) LendNUMANode(_ context.Context, _, _ string, numaNode int, lease string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lent == nil {
		n.lent = map[string]bool{}
	}
	n.lent[fmt.Sprintf("%d %s", numaNode, lease)] = true
	return nil
}

// RevokeNUMANode drops holder of the node.
func (n *frontNodes) RevokeNUMANode(_ context.Context, _, _ string, numaNode int, lease string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.lent, fmt.Sprintf("%d %s", numaNode, lease))
	return nil
}

// lends returns the lent nodes and their holders, sorted.
func (n *frontNodes) lends() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for l := range n.lent {
		out = append(out, l)
	}
	slices.Sort(out)
	return out
}

// clear forgets every lend: the containers are gone.
func (n *frontNodes) clear() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lent = nil
}

// frontHooks is the lease broker of the companion claims: every lease is
// the requester's and lends; an fs lease binds into its lease claim's
// directory.
type frontHooks struct {
	fe *frontEnd
}

// OnLending returns the lend of the lease's container.
func (h *frontHooks) OnLending(id string, _ *resourcev1.ResourceClaim) (*lend.Companion, error) {
	return &lend.Companion{Lease: id, Pod: state.PodRef{Namespace: frontNS, Name: frontPod, UID: frontPodUID}, Container: frontCtr,
		FSDir: filepath.Join(h.fe.leaseFsRoot, "lc-"+id), Name: "data"}, nil
}

// OnPrepared is told the prepared companion.
func (h *frontHooks) OnPrepared(string, state.Source, []state.DeviceInfo) {}

// OnLendFailed is told a failed lend.
func (h *frontHooks) OnLendFailed(string, error) {}

// OnReleasing is told the Unprepare of a companion.
func (h *frontHooks) OnReleasing(string) {}
