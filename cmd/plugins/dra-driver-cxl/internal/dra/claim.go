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
	"cmp"
	"errors"
	"fmt"
	"slices"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/engine"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/lend"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// A claim's allocation results of a driver become the results of its
// preparation and the grant requests to the engine, every result checked
// before any device is touched: the MemoryConfig of the result (resolved
// from the claim's opaque configuration), the device section against the
// kind and the facts of the device. cxl.nri.io finds the device of a result
// in the last scan of the node (a local memdev, a region, the DRAM, the
// lease device); the pool driver takes the device's facts from the status
// data that the pool controller wrote into the claim.

// defaultFsPathDir is the directory of the default path of mode fs in
// containers, /run/cxl-fs/<KEY>.
const defaultFsPathDir = "/run/cxl-fs"

// prepare is a Prepare of a claim: its requests to the engine and its
// results.
type prepare struct {
	uid, claimName string
	lend           *lend.Companion
	reqs           []engine.GrantRequest
	results        []state.Result
	// uses are the shares of the grants of shared memdevs, for the log
	// lines.
	uses map[state.GrantRef]string
	// lease tells whether a result is on the lease device.
	lease bool
}

// newPrepare returns the Prepare of a claim.
func newPrepare(claim *resourcev1.ResourceClaim) *prepare {
	return &prepare{uid: string(claim.UID), claimName: claim.Namespace + "/" + claim.Name, uses: map[state.GrantRef]string{}}
}

// device returns the DRA device of the result of the grant ref.
func (pr *prepare) device(ref state.GrantRef) string {
	for _, res := range pr.results {
		if res.Index == ref.Result {
			return res.Device
		}
	}
	return ""
}

// grantLend returns the lend of a grant of the Prepare, nil for a claim that
// is not a companion.
func (pr *prepare) grantLend() *state.GrantLend {
	l := pr.lend
	if l == nil {
		return nil
	}
	return &state.GrantLend{Lease: l.Lease, Pod: l.Pod, Container: l.Container, FSDir: l.FSDir, Name: l.Name}
}

// leaseBind sets the lease bind of a request of mode fs of a companion
// claim; fs counts them: a lease has one filesystem device at most.
func (pr *prepare) leaseBind(req *engine.GrantRequest, fs *int) error {
	if req.Asked.Mode != state.ModeFS || pr.lend == nil {
		return nil
	}
	if *fs++; *fs > 1 {
		return &engine.BindError{Err: fmt.Errorf("lease %s: more than one filesystem device", pr.lend.Lease)}
	}
	target, err := leaseBindTarget(pr.lend)
	if err != nil {
		return &engine.BindError{Err: err}
	}
	req.BindTarget = target
	return nil
}

// resolveResults returns the MemoryConfig of each allocation result of a
// claim for driver by result index; of selects the results of the driver.
// Returns an error with the request of the first result whose configuration
// is invalid.
func resolveResults(claim *resourcev1.ResourceClaim, driver string, of func(r *resourcev1.DeviceRequestAllocationResult) bool) (map[int]*config.Resolved, error) {
	out := map[int]*config.Resolved{}
	for n := range claim.Status.Allocation.Devices.Results {
		r := &claim.Status.Allocation.Devices.Results[n]
		if !of(r) {
			continue
		}
		res, err := config.Resolve(claim, driver, r.Request)
		if err != nil {
			return nil, fmt.Errorf("request %s: %w", r.Request, err)
		}
		out[n] = res
	}
	return out, nil
}

// consumedShares returns the shares that an allocation result of the shared
// device with the serial consumed, and an error if it consumed less than one
// share or has no shares in its consumed capacity.
func consumedShares(serial uint64, r *resourcev1.DeviceRequestAllocationResult) (int64, error) {
	q, ok := r.ConsumedCapacity[device.CapacityShares]
	if !ok || q.Cmp(*resource.NewQuantity(1, resource.DecimalSI)) < 0 {
		got := ""
		if ok {
			got = q.String()
		}
		return 0, fmt.Errorf("memdev %s: consumed %s %q is less than 1: request %s: 1 (a share) or no %s (exclusive)",
			api.FormatSerial(serial), device.CapacityShares, got, device.CapacityShares, device.CapacityShares)
	}
	return q.Value(), nil
}

// lastingError is an error of a Prepare that its retries cannot heal: the
// MemoryConfig or the allocation of the claim does not fit its device.
type lastingError struct{ error }

func (e lastingError) Unwrap() error { return e.error }

// lasting marks an error of a Prepare that its retries cannot heal.
func lasting(err error) error {
	if err == nil {
		return nil
	}
	return lastingError{err}
}

// sharesText returns how an allocation result that consumed shares of a device
// of total shares uses it: "exclusive" when it consumed all of them, else
// "1 share" or "<n> shares".
func sharesText(shares, total int64) string {
	switch {
	case shares >= total:
		return "exclusive"
	case shares == 1:
		return "1 share"
	}
	return fmt.Sprintf("%d shares", shares)
}

// defaultFsPath returns the path of mode fs of the memory with the env key
// in a container when its device settings have none.
func defaultFsPath(key string) string {
	return defaultFsPathDir + "/" + key
}

// LocalConfig is what cxl.nri.io needs of the driver configuration to map
// the allocation results on local memdevs.
type LocalConfig struct {
	// Managed: the engine manages the local memdevs (manageMemdevs).
	Managed bool
	// UsableSize returns the configured usable size of a memdev (0: all of
	// it), Coherent whether it is configured coherent.
	UsableSize func(memdev string, serial uint64) uint64
	Coherent   func(memdev string, serial uint64) bool
}

// validate returns the Prepare of a claim, or an error when the claim cannot
// be prepared: no allocation, a MemoryConfig that does not decode, a result
// that cannot be prepared (localResult, poolResult), a companion claim that
// cannot be lent. The broker hears of a companion claim that passed its
// gate whose lease cannot be bound or whose error is lasting.
func (p *Plugin) validate(claim *resourcev1.ResourceClaim) (*prepare, error) {
	if claim.Status.Allocation == nil {
		return nil, errors.New("no allocation")
	}
	if err := config.CheckEntries(claim, p.rules.Name); err != nil {
		return nil, err
	}
	resolved, err := resolveResults(claim, p.rules.Name, p.ofDriver)
	if err != nil {
		return nil, err
	}
	allocatable := p.allocatable()
	pr := newPrepare(claim)
	if id := leaseOf(claim); id != "" {
		if !p.memdevsOnly(claim, allocatable) {
			return nil, fmt.Errorf("the companion claim of lease %s needs %s devices only", id, device.DeviceTypeMemdev)
		}
		if pr.lend, err = p.lender.Check(id, claim); err != nil {
			return nil, err
		}
	}
	err = p.results(claim, pr, allocatable, resolved)
	var be *engine.BindError
	if pr.lend != nil && (errors.As(err, &be) || errors.As(err, new(lastingError))) {
		p.lender.Failed(pr.lend.Lease, err)
	}
	return pr, err
}

// ofDriver tells whether an allocation result is the driver's: of
// cxl.nri.io on the node, or of the pool driver.
func (p *Plugin) ofDriver(r *resourcev1.DeviceRequestAllocationResult) bool {
	return r.Driver == p.rules.Name && (p.rules.Source == state.SourcePool || r.Pool == p.nodeName)
}

// memdevsOnly tells whether the allocation results of the driver in a claim
// are memdevs, at least one, as a companion claim needs: every device of
// the pool driver is, cxl.nri.io looks its devices up in allocatable.
func (p *Plugin) memdevsOnly(claim *resourcev1.ResourceClaim, allocatable devicesInfo) bool {
	n := 0
	for i := range claim.Status.Allocation.Devices.Results {
		r := &claim.Status.Allocation.Devices.Results[i]
		if !p.ofDriver(r) {
			continue
		}
		if p.rules.Source == state.SourceLocal {
			d := allocatable[r.Device]
			if d == nil {
				return false
			}
			if _, ok := d.Dev.(*Memdev); !ok {
				return false
			}
		}
		n++
	}
	return n > 0
}

// results sets the requests to the engine and the results of a Prepare of
// a claim: one for every allocation result of the driver, as its
// MemoryConfig (resolved) says, of a device of the node in allocatable
// (localResult) or of a pool device (poolResult).
func (p *Plugin) results(claim *resourcev1.ResourceClaim, pr *prepare, allocatable devicesInfo, resolved map[int]*config.Resolved) error {
	// the env keys of the memdevs, as the CDI spec counts them
	keys := envKeys{}
	fs := 0
	for n := range claim.Status.Allocation.Devices.Results {
		r := &claim.Status.Allocation.Devices.Results[n]
		if !p.ofDriver(r) {
			continue
		}
		res := resolved[n]
		klog.V(3).Infof("%s: claim %s request %s result %d: MemoryConfig %s", p.rules.Name, pr.claimName, r.Request, n, res.Config.Compact())
		result := state.Result{Index: n, Request: r.Request, Pool: r.Pool, Device: r.Device, Settings: res.Settings()}
		if r.ShareID != nil {
			result.ShareID = string(*r.ShareID)
		}
		cfg := res.Config.DeviceSection()
		var req *engine.GrantRequest
		var err error
		switch p.rules.Source {
		case state.SourceLocal:
			req, err = p.localResult(pr, &result, r, allocatable, keys, cfg)
		default:
			req, err = p.poolResult(claim, pr, r, n, keys, cfg)
		}
		if err != nil {
			return err
		}
		if req != nil {
			if err := pr.leaseBind(req, &fs); err != nil {
				return err
			}
			pr.reqs = append(pr.reqs, *req)
			ref := req.Ref
			result.Kind, result.Grant = state.ResultGrant, &ref
		}
		pr.results = append(pr.results, result)
	}
	return nil
}

// localResult returns the request to the engine of an allocation result r of
// cxl.nri.io on a local memdev in allocatable (memdevRequest), or sets the
// result of another device of the node (otherResult) and returns no request.
func (p *Plugin) localResult(pr *prepare, result *state.Result, r *resourcev1.DeviceRequestAllocationResult, allocatable devicesInfo, keys envKeys, cfg config.Device) (*engine.GrantRequest, error) {
	dev, found := allocatable[r.Device]
	if !found {
		return nil, fmt.Errorf("could not find allocatable device %v (pool %v)", r.Device, r.Pool)
	}
	if d, ok := dev.Dev.(*Memdev); ok {
		req, err := p.memdevRequest(pr, r, result.Index, keys.next(api.Key(d.Serial, 1)), d, cfg)
		return &req, err
	}
	if err := otherResult(result, r, dev.Dev, cfg); err != nil {
		return nil, fmt.Errorf("request %s: %w", r.Request, err)
	}
	pr.lease = pr.lease || result.Kind == state.ResultLeaseClaim
	if result.LeaseClaim != nil {
		result.LeaseClaim.UID = pr.uid
	}
	return nil, nil
}

// otherResult sets the result of an allocation result r of a device of the
// node that is not a local memdev (a region, the DRAM, the lease device),
// and returns an error if its device settings cfg do not fit it.
func otherResult(res *state.Result, r *resourcev1.DeviceRequestAllocationResult, dev any, cfg config.Device) error {
	var consumed int64
	if q, ok := r.ConsumedCapacity[device.CapacityMemory]; ok {
		consumed = q.Value()
	}
	switch d := dev.(type) {
	case *Region:
		if err := cfg.NUMAOnly(device.DeviceTypeRegion); err != nil {
			return err
		}
		reg := &state.RegionResult{Name: d.Name, NUMANode: d.NUMANode, Size: d.Size, Consumed: consumed}
		for _, a := range d.NUMAAffinity {
			if a >= 0 {
				reg.NUMAAffinity = append(reg.NUMAAffinity, a)
			}
		}
		res.Kind, res.Region = state.ResultRegion, reg
	case *DRAM:
		if err := cfg.NUMAOnly(device.DeviceTypeDRAM); err != nil {
			return err
		}
		res.Kind, res.DRAM = state.ResultDRAM, &state.DRAMResult{NUMANodes: slices.Clone(d.NUMANodes), Size: d.Size, Consumed: consumed}
	case *Lease:
		if err := cfg.CheckLease(); err != nil {
			return err
		}
		res.Kind, res.LeaseClaim = state.ResultLeaseClaim, &state.LeaseClaimResult{Consumed: consumed}
	default:
		return fmt.Errorf("unknown device type %T for %v", dev, r.Device)
	}
	return nil
}

// memdevRequest returns the request to the engine of the allocation result
// n of a Prepare on a local memdev, by the device settings cfg: a slice of an
// unshared memdev (mode numa or dax), the device of a shared one (mode dax),
// or a use of a persistent one (mode block or fs, the default path of mode
// fs by the env key of the result).
func (p *Plugin) memdevRequest(pr *prepare, r *resourcev1.DeviceRequestAllocationResult, n int, key string, dev *Memdev, cfg config.Device) (engine.GrantRequest, error) {
	req := engine.GrantRequest{
		Ref:    state.GrantRef{Claim: pr.uid, Result: n},
		Serial: state.Serial(dev.Serial),
		Asked:  state.Asked{Source: state.SourceLocal, ReadOnly: cfg.IsReadOnly()},
		Facts: state.MemdevFacts{Shared: dev.Shared, Persistent: dev.Persistent,
			UsableSize: p.local.UsableSize(dev.Name, dev.Serial)},
		Lend: pr.grantLend(),
	}
	if dev.NUMAAffinity >= 0 {
		req.Facts.NUMAAffinity = state.NUMANodePtr(dev.NUMAAffinity)
	}
	if !p.local.Managed {
		return req, fmt.Errorf("device %s: local memdevs are not managed (manageMemdevs: false)", r.Device)
	}
	if dev.Persistent {
		return req, p.persistentRequest(pr, &req, r, key, dev, cfg)
	}
	shares := int64(0)
	var err error
	if dev.Shared {
		shares, err = consumedShares(dev.Serial, r)
	}
	mode := ""
	if err == nil {
		mode, err = cfg.MemdevMode(dev.Shared)
	}
	if err != nil {
		return req, fmt.Errorf("request %s: %w", r.Request, lasting(err))
	}
	req.Asked.Mode = state.Mode(mode)
	if dev.Shared {
		req.Asked.Kind, req.Asked.Shared = state.KindLocalShared, true
		pr.uses[req.Ref] = sharesText(shares, device.SharesUnlimited)
		return req, nil
	}
	if _, ok := r.ConsumedCapacity[device.CapacityShares]; ok {
		return req, lasting(fmt.Errorf("request %s: memdev %s: allocated as a shared device, which it is not any more", r.Request, api.FormatSerial(dev.Serial)))
	}
	size := dev.Size
	if dev.BlockSize > 0 {
		size = localUsableSize(dev.Size, dev.BlockSize)
	}
	if consumed, ok := r.ConsumedCapacity[device.CapacityMemory]; ok && consumed.Value() > 0 {
		size = uint64(consumed.Value())
	}
	if size == 0 {
		return req, fmt.Errorf("device %s: size 0", r.Device)
	}
	req.Asked.Kind, req.Asked.Size = state.KindLocal, size
	return req, nil
}

// persistentRequest sets the request of an allocation result on a local
// memdev of persistent memory by the device settings cfg, its facts as the
// configuration and the allocation give them.
func (p *Plugin) persistentRequest(pr *prepare, req *engine.GrantRequest, r *resourcev1.DeviceRequestAllocationResult, key string, dev *Memdev, cfg config.Device) error {
	fail := func(err error) error {
		return fmt.Errorf("request %s: %w", r.Request, lasting(err))
	}
	if _, ok := r.ConsumedCapacity[device.CapacityMemory]; ok {
		return fail(fmt.Errorf("memdev %s: allocated as a device of volatile memory, which it is not any more", api.FormatSerial(dev.Serial)))
	}
	shares, err := consumedShares(dev.Serial, r)
	if err != nil {
		return fail(err)
	}
	facts := config.DeviceFacts{
		Persistent: true,
		Shared:     dev.Shared,
		Exclusive:  shares >= device.SharesUnlimited,
		SingleHost: !dev.Shared,
		Coherent:   p.local.Coherent(dev.Name, dev.Serial),
	}
	mode, err := cfg.PersistentMode(facts)
	if err != nil {
		return fail(err)
	}
	req.Asked.Kind, req.Asked.Mode, req.Asked.Shared = state.KindLocalPersistent, state.Mode(mode), dev.Shared
	req.Asked.Access, req.Asked.FSType = state.Access(cfg.Access), cfg.FSType
	if mode == api.ModeFS {
		if req.Asked.Path = cfg.Path; req.Asked.Path == "" {
			req.Asked.Path = defaultFsPath(key)
		}
	}
	req.Facts.Coherent, req.SingleHost = facts.Coherent, facts.SingleHost
	return nil
}

// poolFacts returns the facts of a pool device of its status data (the
// contract of the pool): a device that is not shared has one claim at most,
// and no other host while it is allocated.
func poolFacts(d *device.PoolDevice) config.DeviceFacts {
	single := d.Exclusive || !d.Shared
	return config.DeviceFacts{
		Persistent: d.Persistent,
		Shared:     d.Shared,
		Exclusive:  single,
		SingleHost: d.SingleHost || single,
		Coherent:   d.Coherent,
	}
}

// poolResult returns the request to the engine of an allocation result r of
// the pool driver, from the status data of its device, or an error when it
// cannot be prepared: no status data, shares that do not fit, a mode the
// device does not have, another claim's preparation of the device in another
// way.
func (p *Plugin) poolResult(claim *resourcev1.ResourceClaim, pr *prepare, r *resourcev1.DeviceRequestAllocationResult, n int, keys envKeys, cfg config.Device) (*engine.GrantRequest, error) {
	data, err := device.PoolStatus(claim, r)
	if err != nil {
		return nil, err
	}
	if data.Shared {
		// before the device is touched: shares "0" fits next to an
		// exclusive holder
		if _, err := consumedShares(data.Serial, r); err != nil {
			return nil, lasting(err)
		}
	}
	mode, err := cfg.PoolMode(poolFacts(data))
	if err != nil {
		return nil, fmt.Errorf("device %s (memdev 0x%x): %w", r.Device, data.Serial, lasting(err))
	}
	asked := state.Asked{Source: state.SourcePool, Kind: state.KindOf(state.SourcePool, data.Shared, data.Persistent), Mode: state.Mode(mode),
		ReadOnly: cfg.IsReadOnly(), Size: data.Size, Shared: data.Shared}
	key := keys.next(api.Key(data.Serial, 1))
	if mode == api.ModeFS {
		// access "": by the type of the filesystem, at its Prepare
		asked.Access, asked.Path, asked.FSType = state.Access(cfg.Access), cmp.Or(cfg.Path, defaultFsPath(key)), cfg.FSType
	}
	if err := p.checkSamePreparation(data, asked, pr.uid); err != nil {
		return nil, fmt.Errorf("device %s (memdev 0x%x): %w", r.Device, data.Serial, err)
	}
	req := poolRequest(pr, n, data, asked)
	return &req, nil
}

// poolRequest returns the request to the engine of the allocation result n
// of a Prepare of a claim, with its parsed status data and what it asks.
func poolRequest(pr *prepare, n int, data *device.PoolDevice, asked state.Asked) engine.GrantRequest {
	return engine.GrantRequest{
		Ref:        state.GrantRef{Claim: pr.uid, Result: n},
		Serial:     state.Serial(data.Serial),
		Asked:      asked,
		Facts:      state.MemdevFacts{Shared: data.Shared, Persistent: data.Persistent, Coherent: data.Coherent},
		SingleHost: poolFacts(data).SingleHost,
		Lend:       pr.grantLend(),
	}
}

// preparedGrant returns a grant of a prepared claim of the driver other
// than exceptUID on the serial, or nil.
func (p *Plugin) preparedGrant(serial uint64, exceptUID string) *state.Grant {
	r := p.records()
	for i := range r.Preparations {
		prep := &r.Preparations[i]
		if prep.Driver != p.rules.Name || prep.UID == exceptUID || !prep.Prepared() {
			continue
		}
		for _, res := range prep.Results {
			if g := grantOf(r, &res); g != nil && uint64(g.Serial) == serial {
				return g
			}
		}
	}
	return nil
}

// checkSamePreparation refuses a device (its status data, what a claim
// asks of it) that another claim prepared differently: as shared or
// exclusive, or in another mode. Read-only may differ: it is a property of
// the CDI device node, not of the region. Modes block and fs, and fs with
// either access, are compatible: they share one pmem region.
func (p *Plugin) checkSamePreparation(data *device.PoolDevice, asked state.Asked, exceptUID string) error {
	other := p.preparedGrant(data.Serial, exceptUID)
	switch {
	case other == nil:
		return nil
	case other.Asked.Shared != data.Shared:
		return fmt.Errorf("shared=%v, but another claim prepared it with shared=%v", data.Shared, other.Asked.Shared)
	case other.Asked.Mode != asked.Mode && (!api.PersistentMode(string(other.Asked.Mode)) || !api.PersistentMode(string(asked.Mode))):
		return fmt.Errorf("mode %s, but another claim prepared it in mode %s", asked.Mode, other.Asked.Mode)
	}
	return nil
}
