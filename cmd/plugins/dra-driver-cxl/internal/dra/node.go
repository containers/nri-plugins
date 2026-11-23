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

package dra

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/engine"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/lend"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/provenance"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

const (
	// mountsDirName is the directory in the plugin data directory of a
	// driver that has the node mounts of mode fs.
	mountsDirName = "mounts"
	// probesFileName is the file in the plugin data directory of
	// cxl.nri.io that keeps the usable sizes that the address line probe
	// found. It outlives restarts and reboots.
	probesFileName = "localProbes.json"
)

// NodeOptions are the options of NewNode.
type NodeOptions struct {
	// Kernel is the kernel of the node. Store keeps the records.
	Kernel *kernel.Kernel
	Store  *store.Store
	// Dir is the plugin data directory of cxl.nri.io.
	Dir string
	// PoolDriver is the name of the pool driver, "" when it is disabled.
	// Its plugin data directory is next to Dir.
	PoolDriver string
	// LeaseFsRoot is the directory that has the lease-fs directories of
	// the lease broker.
	LeaseFsRoot string
	// CDIDir is the directory of the CDI specs.
	CDIDir string
	// NodeName is the name of the node.
	NodeName string
	// Client publishes the ResourceSlice of the node and lists the pool
	// ResourceSlices. Nil: no publication and no watcher.
	Client coreclientset.Interface
	// Local are the settings of the local memdevs. Publish is the
	// configuration of the scans.
	Local   LocalConfig
	Publish PublishConfig
	// Scan observes the node, nil for Kernel.ObserveTopology.
	Scan func(ctx context.Context) (*kernel.Snapshot, error)
	// Fence, FSTypes, ProbeAddressing and Timeouts go to the engine
	// (engine.Options).
	Fence           func(ctx context.Context, node int)
	FSTypes         []string
	ProbeAddressing bool
	Timeouts        engine.Timeouts
	// Forget are the serials whose provenance records the watcher
	// removes (provenance.Options).
	Forget []uint64
	// Shutdown gets the fatal errors of the kubelet plugins
	// (Options.Shutdown).
	Shutdown func(err error)
}

// Node is the DRA drivers of a node over one device engine. The engine
// owns the CXL kernel objects of both drivers.
type Node struct {
	// Engine is the device engine.
	Engine *engine.Engine
	// Local is cxl.nri.io. Pool is the pool driver, nil when it is
	// disabled.
	Local, Pool *Plugin
	// Publisher publishes the ResourceSlice of the node.
	Publisher *Publisher
	// Watcher watches the pool devices, nil without a client.
	Watcher *provenance.Watcher

	opts NodeOptions
	scan func(ctx context.Context) (*kernel.Snapshot, error)
	cdi  *cdiapi.Cache
	// guarded is set after the recovery. From then on, localGuard checks
	// a local memdev against the pool.
	guarded atomic.Bool
	// mu guards shared.
	mu sync.Mutex
	// shared are the local memdevs that the configuration declares
	// shared, and those that a claim used shared since the start.
	shared map[state.Serial]bool
}

// NewNode returns the DRA drivers of a node over a new device engine. The
// engine owns the pool devices unless the pool driver is disabled. It
// owns the local memdevs when Local.Managed is set.
func NewNode(opts NodeOptions) (*Node, error) {
	n := &Node{opts: opts, scan: opts.Scan, shared: map[state.Serial]bool{}}
	if n.scan == nil {
		n.scan = opts.Kernel.ObserveTopology
	}
	k := opts.Kernel
	eopts := engine.Options{
		Kernel:     k,
		Store:      opts.Store,
		MountsDirs: map[state.Source]string{},
		// an engine without sources owns nothing, nil sources own all
		Sources:         []state.Source{},
		LeaseFsRoot:     opts.LeaseFsRoot,
		FSTypes:         opts.FSTypes,
		Fence:           opts.Fence,
		ProbeAddressing: opts.ProbeAddressing,
		Timeouts:        opts.Timeouts,
		LocalGuard:      n.localGuard,
		Shared:          n.isShared,
		Revoke:          n.revoke,
	}
	local := localRules(filepath.Join(opts.Dir, mountsDirName))
	var pool SourceRules
	if opts.PoolDriver != "" {
		poolDir := filepath.Join(filepath.Dir(opts.Dir), opts.PoolDriver)
		if err := os.MkdirAll(poolDir, 0o750); err != nil {
			return nil, fmt.Errorf("pool driver data directory: %w", err)
		}
		pool = poolRules(opts.PoolDriver, filepath.Join(poolDir, mountsDirName))
		eopts.Sources = append(eopts.Sources, state.SourcePool)
		eopts.MountsDirs[state.SourcePool] = pool.MountsDir
	}
	if opts.Local.Managed {
		eopts.Sources = append(eopts.Sources, state.SourceLocal)
		eopts.MountsDirs[state.SourceLocal] = local.MountsDir
		eopts.ProbesFile = filepath.Join(opts.Dir, probesFileName)
	}
	e, err := engine.New(eopts)
	if err != nil {
		return nil, fmt.Errorf("device engine: %w", err)
	}
	n.Engine = e
	// the cache only writes: no refresh after the first, no watch
	n.cdi, err = cdiapi.NewCache(cdiapi.WithSpecDirs(opts.CDIDir), cdiapi.WithAutoRefresh(false))
	if err != nil {
		return nil, fmt.Errorf("CDI cache of %s: %w", opts.CDIDir, err)
	}
	if opts.Client != nil {
		n.Watcher = provenance.New(provenance.Options{
			Store:    opts.Store,
			Client:   opts.Client,
			Driver:   opts.PoolDriver,
			NodeName: opts.NodeName,
			Name:     filepath.Join(opts.Dir, store.FileName),
			Forget:   opts.Forget,
		})
	}
	n.Publisher, err = newPublisher(publishOptions{
		Config:      opts.Publish,
		NodeName:    opts.NodeName,
		Client:      opts.Client,
		Scan:        n.scan,
		Watcher:     n.Watcher,
		PoolSerials: func() map[uint64]bool { return n.Pool.Serials() },
		Store:       opts.Store,
		Probes:      e.Probes,
		BlockSize:   k.BlockSize,
	})
	if err != nil {
		return nil, err
	}
	if opts.PoolDriver != "" {
		n.Pool, err = New(Options{
			Rules:     pool,
			Store:     opts.Store,
			Engine:    e,
			NodeName:  opts.NodeName,
			SysRoot:   k.SysRoot,
			DevRoot:   k.DevRoot,
			CDI:       n.cdi,
			Shutdown:  opts.Shutdown,
			Publisher: n.Publisher,
		})
		if err != nil {
			return nil, err
		}
	}
	n.Local, err = New(Options{
		Rules:     local,
		Store:     opts.Store,
		Engine:    e,
		NodeName:  opts.NodeName,
		SysRoot:   k.SysRoot,
		DevRoot:   k.DevRoot,
		CDI:       n.cdi,
		Local:     opts.Local,
		OnShared:  n.addShared,
		Shutdown:  opts.Shutdown,
		Publisher: n.Publisher,
	})
	if err != nil {
		return nil, err
	}
	return n, nil
}

// Recover recovers the records at a start, makes the first scan and
// returns what the recovery of the engine did. The steps are:
//   - the shared local memdevs of the configuration become known;
//   - the engine forgets the records of the local memdevs that are pool
//     devices now and whose region was being made;
//   - the engine matches the records with the kernel (engine.Recover);
//   - localGuard starts to check;
//   - the publisher lists the pool ResourceSlices and scans the node.
//
// With a LOST store, the engine does not forget or match records. Then
// every Prepare fails with the loss and nothing is released.
func (n *Node) Recover(ctx context.Context) (*engine.Recovered, error) {
	k := n.opts.Kernel
	snap, err := n.scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to detect devices: %v", err)
	}
	if !k.HasCXLBus() {
		klog.Warningf("no CXL bus in %s: the node has no CXL memdevs or regions (are the CXL modules loaded?)", k.SysRoot)
	}
	// the serials of pool devices in the records: those that the pool driver
	// prepares and the remembered ones
	known := map[uint64]bool{}
	maps.Copy(known, n.Pool.Serials())
	maps.Copy(known, n.Watcher.RecordedPoolSerials())
	n.setShared(sharedSerials(n.opts.Publish, snap.Memdevs, known))
	rec := &engine.Recovered{Why: map[state.GrantRef]error{}}
	if err := n.opts.Store.Lost(); err != nil {
		klog.Errorf("%v: the devices of the drivers are not recovered", err)
	} else {
		var serials []state.Serial
		for s := range known {
			serials = append(serials, state.Serial(s))
		}
		slices.Sort(serials)
		if err := n.Engine.ForgetLocal(ctx, serials); err != nil {
			return nil, err
		}
		// the recovery comes before any scan of the publisher: it never
		// publishes a region of the drivers as a region device
		if rec, err = n.Engine.Recover(ctx); err != nil {
			return nil, fmt.Errorf("recovering the devices: %w", err)
		}
	}
	n.Pool.Recovered(rec)
	n.guarded.Store(true)
	// the first list of the pool ResourceSlices gates the publication of
	// local memdevs
	cfg, poolSerials := n.Publisher.ScanInput(ctx)
	n.setShared(sharedSerials(cfg, snap.Memdevs, poolSerials))
	if n.opts.Local.Managed {
		if _, err := k.BlockSize(); err != nil {
			klog.Warningf("publisher: memory block size: %v: local memdevs are not published until the memory block size is known", err)
		}
	}
	if err := n.Publisher.ScanWith(ctx, cfg, poolSerials); err != nil {
		return nil, err
	}
	n.Local.Recovered(rec)
	return rec, nil
}

// WriteCDI writes the CDI specs of the prepared claims of both drivers in
// place. It removes the spec of the pool driver when the driver is
// disabled. With a LOST store it does nothing: the prepared claims are
// unknown, so their specs stay.
func (n *Node) WriteCDI() error {
	if n.opts.Store.Lost() != nil {
		return nil
	}
	if err := n.Local.WriteCDI(); err != nil {
		return err
	}
	if n.Pool == nil {
		return n.cdi.RemoveSpec(device.PoolCDISpecName)
	}
	return n.Pool.WriteCDI()
}

// SetLenders sets the lenders of the companion claims of both drivers:
// nodes lends NUMA nodes, hooks tell the lease broker.
func (n *Node) SetLenders(nodes NUMALender, hooks lend.LeaseHooks) {
	n.Local.Lender().Set(nodes, hooks)
	n.Pool.Lender().Set(nodes, hooks)
}

// RetryStaleBinds unmounts the stale lease binds again and finishes the
// releases that they kept (engine.RetryStaleBinds). With a LOST store it does
// nothing: then nothing is released.
func (n *Node) RetryStaleBinds(ctx context.Context) {
	if n.opts.Store.Lost() != nil {
		return
	}
	n.Engine.RetryStaleBinds(ctx, state.TriggerUnprepare)
}

// localGuard returns an error for a local memdev that the pool driver
// prepares, or that a pool ResourceSlice lists now (a fresh list of the
// watcher). It returns nil before the recovery.
func (n *Node) localGuard(ctx context.Context, serial state.Serial) error {
	if !n.guarded.Load() {
		return nil
	}
	if n.Pool.Serials()[uint64(serial)] {
		return fmt.Errorf("memdev 0x%x is prepared by the pool driver %s: it is not used as a local memdev", uint64(serial), n.Pool.Name())
	}
	return n.Watcher.CheckLocal(ctx, uint64(serial))
}

// setShared sets the local memdevs that the configuration declares
// shared.
func (n *Node) setShared(serials map[uint64]bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.shared = map[state.Serial]bool{}
	for s := range serials {
		n.shared[state.Serial(s)] = true
	}
}

// addShared records that a claim uses the local memdev with the serial
// shared. The engine keeps its idle region from then on.
func (n *Node) addShared(serial uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.shared[state.Serial(serial)] = true
}

// isShared tells whether the local memdev with the serial is shared.
func (n *Node) isShared(serial state.Serial) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.shared[serial]
}

// revoke revokes the NUMA node that the grant ref of a companion claim
// lends. The lender of the driver of the grant revokes it.
func (n *Node) revoke(ctx context.Context, ref state.GrantRef, gl state.GrantLend) error {
	r := &n.opts.Store.Snapshot().Records
	p := n.Pool
	if g := r.Grant(ref); g != nil && g.Asked.Source == state.SourceLocal {
		p = n.Local
	}
	claimName := ref.Claim
	if prep := r.Preparation(p.Name(), ref.Claim); prep != nil && prep.ClaimName != "" {
		claimName = prep.ClaimName
	}
	return p.Lender().RevokeGrant(ctx, gl, claimName)
}
