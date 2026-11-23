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
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/provenance"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// Publisher publishes the ResourceSlices of the node of cxl.nri.io: one pool
// named after the node, of slices of at most 128 devices, owner the Node.
//
// Identity: one per process, of the node.
//
// States: the table, the devices of the last scan; not started (the scans
// set the table, nothing is published), started (the first publish of the
// publisher goroutine starts the controller of the node's slices, then
// every scan is published) and stopped (nothing is published). A latch:
// the API server drops nodeAllocatableResources. The configuration is
// that of newPublisher.
//
// Transitions: a scan (at the start, then on udev events, the watch's
// ticks and after the Prepares and Unprepares that changed the records,
// one at a time from the list of the pool slices to the publish) sets the
// table. The controller's first report of a dropped nodeAllocatableResources
// sets the latch: the table is published without it from then on. A
// publish of an unchanged table writes nothing to the API server: the
// controller writes a slice that differs only.
//
// Invariants: a pool device and a HELD memdev are never published as local
// ones; the devices are sorted by name, so their order is not a change; no
// local memdev is published before a list of the pool slices succeeded.
//
// Persistence: none; the table is derived from the kernel, the provenance
// records and the records of the local memdevs.
//
// Locks and waits: scanMu serializes the scans, the publishes and the
// changes of the controller, so that nothing publishes after Stop (it is
// held across the lists of the API server, bounded, and across the first
// publish, which waits for the informer of the controller until it has
// synced or Stop); mu guards the table, the controller and the latch, never
// held across a scan or a publish.
type Publisher struct {
	opts publishOptions

	scanMu  sync.Mutex
	scanned atomic.Int64

	// mu guards the fields below up to triggerMu: a leaf lock.
	mu      sync.Mutex
	devices devicesInfo
	// published is the table of the last publication, nil before the
	// first.
	published devicesInfo
	// ctl publishes the node's slices, nil before the first publish and
	// after Stop.
	ctl *resourceslice.Controller
	// noAllocatable: the API server drops nodeAllocatableResources.
	noAllocatable bool
	// logged are the messages of the errors of ctl logged, with when.
	logged map[string]time.Time
	// cancel ends the context of the publisher goroutine and ctl, done
	// is closed when the goroutine has returned; nil before Start.
	cancel context.CancelFunc
	done   chan struct{}

	// triggerMu guards the fields below: a leaf lock.
	triggerMu     sync.Mutex
	pending       bool // a scan is asked for
	pendingUrgent bool // a scan that is not periodic is asked for
	trigger       chan struct{}
	stopped       bool
}

// publishOptions are the settings of a Publisher.
type publishOptions struct {
	// Config are the settings of the configuration of the driver, fixed
	// for the life of the publisher.
	Config PublishConfig
	// NodeName is the node of the slice; Client publishes it.
	NodeName string
	Client   coreclientset.Interface
	// Scan returns an observation of the kernel objects of the node.
	Scan func(ctx context.Context) (*kernel.Snapshot, error)
	// Watcher knows the provenance of the memdevs, nil for no pool.
	Watcher *provenance.Watcher
	// PoolSerials returns the serials of the pool driver's devices, nil
	// without a pool driver.
	PoolSerials func() map[uint64]bool
	// Store has the records of the local memdevs; Probes returns the
	// results of the address line probes, BlockSize the memory block size.
	Store     *store.Store
	Probes    func() map[state.Serial]state.Probe
	BlockSize func() (uint64, error)
}

// PublishConfig are the settings of the configuration of the driver that
// the publisher applies.
type PublishConfig struct {
	// IgnoreDevices are memdevs that are never published; SharedDevices
	// the local memdevs published as shared devices.
	IgnoreDevices, SharedDevices []MemdevSpec
	// IgnoreRegions and IgnoreNodes are regions and NUMA nodes that are not
	// published.
	IgnoreRegions   []string
	IgnoreNUMANodes []int
	// IgnoreDRAM: the DRAM of the node is not published.
	IgnoreDRAM bool
	// ManageMemdevs: the local memdevs are published.
	ManageMemdevs bool
	// PublishLeaseDevice: the lease device is published.
	PublishLeaseDevice bool
	// UsableSize returns the configured usable size of a memdev, 0 for all
	// of it.
	UsableSize func(memdev string, serial uint64) uint64
}

// usableSize returns the configured usable size of a memdev.
func (c *PublishConfig) usableSize(memdev string, serial uint64) uint64 {
	if c.UsableSize == nil {
		return 0
	}
	return c.UsableSize(memdev, serial)
}

// newPublisher returns the publisher of the node's ResourceSlices of the
// scans of opts.Scan and the records of opts.Store; it publishes after
// Start.
func newPublisher(opts publishOptions) (*Publisher, error) {
	if opts.Store == nil || opts.Scan == nil || opts.NodeName == "" {
		return nil, errors.New("the publisher needs the state store, the scan of the devices and the node name")
	}
	return &Publisher{opts: opts, devices: devicesInfo{}, logged: map[string]time.Time{}, trigger: make(chan struct{}, 1)}, nil
}

// Published returns the devices of the last publication, by name, those of
// the last scan before the first publication.
func (p *Publisher) Published() devicesInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.published == nil {
		return p.devices
	}
	return p.published
}

// PublishedDevice returns the ResourceSlice device of the local memdev with
// the serial in Published, false if there is none.
func (p *Publisher) PublishedDevice(serial uint64) (resourcev1.Device, bool) {
	for name, d := range p.Published() {
		if m, ok := d.Dev.(*Memdev); ok && m.Serial == serial {
			return LocalMemdevDevice(name, m), true
		}
	}
	return resourcev1.Device{}, false
}

// Devices returns the devices of the last scan, by name.
func (p *Publisher) Devices() devicesInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.devices
}

// SetDevices sets the devices of the table, as a scan does.
func (p *Publisher) SetDevices(devs devicesInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.devices = devs
}

// LastScan returns the time of the last scan, or of the start.
func (p *Publisher) LastScan() time.Time {
	return time.Unix(0, p.scanned.Load())
}

// Resources returns the resources of the node's ResourceSlices: the devices
// of the table, sorted by name.
func (p *Publisher) Resources() resourceslice.DriverResources {
	p.mu.Lock()
	devs, noAllocatable := p.devices, p.noAllocatable
	p.mu.Unlock()
	return p.resourcesOf(devs, noAllocatable)
}

// resourcesOf returns the resources of the node's ResourceSlices of the
// devices of a table, sorted by name, in slices of at most
// ResourceSliceMaxDevices, one empty slice for no devices; without
// nodeAllocatableResources if noAllocatable.
func (p *Publisher) resourcesOf(devs devicesInfo, noAllocatable bool) resourceslice.DriverResources {
	out := resources(devs)
	if noAllocatable {
		for i := range out {
			out[i].NodeAllocatableResources = nil
		}
	}
	var pool resourceslice.Pool
	for chunk := range slices.Chunk(out, resourcev1.ResourceSliceMaxDevices) {
		pool.Slices = append(pool.Slices, resourceslice.Slice{Devices: chunk})
	}
	if len(pool.Slices) == 0 {
		pool.Slices = []resourceslice.Slice{{}}
	}
	return resourceslice.DriverResources{Pools: map[string]resourceslice.Pool{p.opts.NodeName: pool}}
}

// Scan scans the node and sets the table, without publishing it.
func (p *Publisher) Scan(ctx context.Context) error {
	p.scanMu.Lock()
	defer p.scanMu.Unlock()
	defer p.scanned.Store(time.Now().UnixNano())
	devs, err := p.scan(ctx)
	if err != nil {
		return err
	}
	p.SetDevices(devs)
	return nil
}

// Start starts the publisher goroutine, once: its first publish of the
// table starts the controller of the node's slices, then it scans and
// publishes when Trigger asks for it, until ctx ends or Stop. Start does
// not wait for the API server.
func (p *Publisher) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	p.mu.Lock()
	p.cancel, p.done = cancel, done
	p.mu.Unlock()
	p.scanned.Store(time.Now().UnixNano())
	go func() {
		defer close(done)
		p.run(ctx)
	}()
}

// Stop stops the publisher goroutine and the controller of the node's
// slice and waits for them; the published slice stays. Nothing for a nil
// publisher.
func (p *Publisher) Stop() {
	if p == nil {
		return
	}
	p.triggerMu.Lock()
	if !p.stopped {
		p.stopped = true
		close(p.trigger)
	}
	p.triggerMu.Unlock()
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	p.scanMu.Lock()
	p.mu.Lock()
	ctl := p.ctl
	p.ctl = nil
	p.mu.Unlock()
	p.scanMu.Unlock()
	ctl.Stop() // nil-safe
}

// Trigger asks the publisher goroutine for a scan and a publish; a periodic
// one logs at klog V(4) unless it changed the devices. A scan that is asked
// for already covers it.
func (p *Publisher) Trigger(_ context.Context, periodic bool) {
	p.triggerMu.Lock()
	defer p.triggerMu.Unlock()
	if p.stopped {
		return
	}
	p.pending = true
	p.pendingUrgent = p.pendingUrgent || !periodic
	select {
	case p.trigger <- struct{}{}:
	default:
	}
}

// run makes the first publish, then scans and publishes at every trigger
// until ctx ends or Stop.
func (p *Publisher) run(ctx context.Context) {
	p.scanMu.Lock()
	err := p.publish(ctx)
	p.scanMu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			klog.Errorf("publisher: %v", err)
		}
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-p.trigger:
			if !ok {
				return
			}
			p.triggerMu.Lock()
			pending, urgent := p.pending, p.pendingUrgent
			p.pending, p.pendingUrgent = false, false
			p.triggerMu.Unlock()
			if pending {
				p.Rescan(ctx, !urgent)
			}
		}
	}
}

// Rescan scans the node, sets the table and publishes it when the
// publisher runs; a periodic one logs at klog V(4) unless it changed the
// devices. A failed scan keeps the table.
func (p *Publisher) Rescan(ctx context.Context, periodic bool) {
	p.scanMu.Lock()
	defer p.scanMu.Unlock()
	defer p.scanned.Store(time.Now().UnixNano())
	logf := klog.Infof
	if periodic {
		logf = klog.V(4).Infof
	}
	logf("publisher: rescanning devices")
	devs, err := p.scan(ctx)
	if err != nil {
		klog.Errorf("publisher: %v", err)
		return
	}
	p.mu.Lock()
	changed := !slices.Equal(slices.Sorted(maps.Keys(p.devices)), slices.Sorted(maps.Keys(devs)))
	p.devices = devs
	publishing := p.ctl != nil
	p.mu.Unlock()
	if periodic && changed {
		logf = klog.Infof
		logf("publisher: the periodic rescan changed the devices")
	}
	if !publishing {
		return
	}
	if err := p.publish(ctx); err != nil {
		klog.Errorf("publisher: publishing: %v", err)
		return
	}
	logf("publisher: published %d devices", len(devs))
}

// publish publishes the table: the first publish starts the controller of
// the node's slices, which runs until ctx ends, and waits until its informer
// has synced; the later ones update the controller and do not wait for the
// API server. Called with scanMu held.
func (p *Publisher) publish(ctx context.Context) error {
	p.mu.Lock()
	table, noAllocatable, ctl := p.devices, p.noAllocatable, p.ctl
	p.mu.Unlock()
	res := p.resourcesOf(table, noAllocatable)
	n := 0
	for _, s := range res.Pools[p.opts.NodeName].Slices {
		for i := range s.Devices {
			klog.V(5).Infof("publisher: device %s: %+v", s.Devices[i].Name, s.Devices[i])
		}
		n += len(s.Devices)
	}
	klog.Infof("publisher: publishing %d devices", n)
	if ctl != nil {
		ctl.Update(&res) // a deep copy
	} else {
		ctl, err := p.startController(ctx, &res)
		if err != nil {
			return fmt.Errorf("start ResourceSlice controller: %w", err)
		}
		p.mu.Lock()
		p.ctl = ctl
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.published = table
	p.mu.Unlock()
	return nil
}

// scan returns the devices to publish of a scan of the node: the serials of
// the pool driver's devices and of the pool slices noted, the memdevs held
// or remembered as pool devices left out.
func (p *Publisher) scan(ctx context.Context) (devicesInfo, error) {
	cfg, poolSerials := p.ScanInput(ctx)
	return p.scanWith(ctx, cfg, poolSerials)
}

// ScanWith scans the node with the configuration and the serials of pool
// devices of ScanInput and sets the table, without publishing it.
func (p *Publisher) ScanWith(ctx context.Context, cfg PublishConfig, poolSerials map[uint64]bool) error {
	p.scanMu.Lock()
	defer p.scanMu.Unlock()
	defer p.scanned.Store(time.Now().UnixNano())
	devs, err := p.scanWith(ctx, cfg, poolSerials)
	if err != nil {
		return err
	}
	p.SetDevices(devs)
	return nil
}

// scanWith returns the devices to publish of a scan of the node with the
// configuration and the serials of pool devices of ScanInput.
func (p *Publisher) scanWith(ctx context.Context, cfg PublishConfig, poolSerials map[uint64]bool) (devicesInfo, error) {
	snap, err := p.opts.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to detect devices: %v", err)
	}
	local := p.LocalRecords()
	poolSerials, held := p.opts.Watcher.Gate(ctx, snap, poolSerials, local.claims())
	if len(snap.Regions) == 0 {
		klog.Info("No region devices detected")
	}
	hollowRegions.observe(snap.Regions, p.ours())
	return BuildDevices(cfg, snap, poolSerials, held, local), nil
}

// ours returns whether the records have the region with the name as one
// of the device engine.
func (p *Publisher) ours() func(name string) bool {
	r := &p.opts.Store.Snapshot().Records
	return func(name string) bool {
		reg := r.Region(state.RegionRef(name))
		return reg != nil && reg.Ours()
	}
}

// ScanInput returns the configuration and the serials of pool devices of a
// scan: the serials of the memdevs that the pool driver prepares, of the
// devices that pool controllers publish, and the remembered serials of pool
// devices. While the devices of the pool controllers are not known, the
// configuration publishes no local memdevs.
func (p *Publisher) ScanInput(ctx context.Context) (PublishConfig, map[uint64]bool) {
	serials := map[uint64]bool{}
	if p.opts.PoolSerials != nil {
		maps.Copy(serials, p.opts.PoolSerials())
	}
	w := p.opts.Watcher
	w.NotePrepared(serials)
	remote, known := w.ListPoolSerials(ctx)
	for s := range remote {
		serials[s] = true
	}
	cfg := p.opts.Config
	if known || !cfg.ManageMemdevs {
		return cfg, serials
	}
	klog.Warning("not publishing local memdevs: the devices of the pool controllers are not known")
	cfg.ManageMemdevs = false
	return cfg, serials
}

// LocalRecords returns the uses of the local memdevs that the records
// have, the memory block size and the probed usable sizes; nil when the
// local memdevs are not managed.
func (p *Publisher) LocalRecords() *localRecords {
	if !p.opts.Config.ManageMemdevs {
		return nil
	}
	snap := &localRecords{Memdevs: map[uint64]localMemdev{}, Probed: map[uint64]uint64{}}
	r := &p.opts.Store.Snapshot().Records
	for i := range r.Memdevs {
		m := &r.Memdevs[i]
		if m.Source != state.SourceLocal {
			continue
		}
		u := localMemdev{Shared: m.Facts.Shared, Persistent: m.Facts.Persistent, Claims: []string{}}
		for _, ref := range m.Grants {
			if !slices.Contains(u.Claims, ref.Claim) {
				u.Claims = append(u.Claims, ref.Claim)
			}
		}
		slices.Sort(u.Claims)
		snap.Memdevs[uint64(m.Serial)] = u
	}
	if p.opts.Probes != nil {
		for serial, pr := range p.opts.Probes() {
			snap.Probed[uint64(serial)] = pr.UsableSize
		}
	}
	if p.opts.BlockSize != nil {
		snap.BlockSize, _ = p.opts.BlockSize()
	}
	return snap
}
