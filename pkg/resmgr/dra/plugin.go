/*
Copyright The NRI Plugins Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-logr/logr"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"

	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	"tags.cncf.io/container-device-interface/pkg/parser"
)

// Sentinel errors for PrepareResourceClaims.
var (
	errNilAllocation           = errors.New("dra plugin: claim has nil Allocation")
	errMissingConsumedCapacity = errors.New("dra plugin: ConsumedCapacity[nri/cpus] absent or zero")
	errNonHPNotSupported       = errors.New("dra plugin: non-HP CPU class not supported (deferred)")
)

// deviceInfo holds the device attributes looked up from the published device list.
type deviceInfo struct {
	ClassName string
	PkgID     int
	PunitID   int
}

// Plugin is the DRA kubelet plugin.
type Plugin struct {
	mu         sync.Mutex // mu guards helper
	driverName string
	deps       Deps
	helper     *kubeletplugin.Helper
	claims     map[types.UID]*ClaimState
}

// New constructs a Plugin with the given driver name and dependencies.
// Returns an error if any required dependency is missing.
func New(driverName string, deps Deps) (*Plugin, error) {
	if driverName == "" {
		return nil, fmt.Errorf("dra plugin: driverName must not be empty")
	}
	if deps.KubeClient == nil {
		return nil, fmt.Errorf("dra plugin: KubeClient must not be nil")
	}
	if deps.NodeName == "" {
		return nil, fmt.Errorf("dra plugin: NodeName must not be empty")
	}
	if deps.ValidateClasses == nil {
		return nil, fmt.Errorf("dra plugin: ValidateClasses must not be nil")
	}
	if deps.DeviceLister == nil {
		return nil, fmt.Errorf("dra plugin: DeviceLister must not be nil")
	}
	if deps.Logger == nil {
		return nil, fmt.Errorf("dra plugin: Logger must not be nil")
	}
	if deps.ClaimAllocator == nil {
		return nil, fmt.Errorf("dra plugin: ClaimAllocator must not be nil")
	}
	if deps.CDIWriter == nil {
		return nil, fmt.Errorf("dra plugin: CDIWriter must not be nil")
	}
	if deps.ClaimStore == nil {
		return nil, fmt.Errorf("dra plugin: ClaimStore must not be nil")
	}
	if deps.WithLock == nil {
		return nil, fmt.Errorf("dra plugin: WithLock must not be nil")
	}
	return &Plugin{driverName: driverName, deps: deps, claims: make(map[types.UID]*ClaimState)}, nil
}

// shareIDPtr converts a ShareID string to *types.UID. Returns nil if s is "".
func shareIDPtr(s string) *types.UID {
	if s == "" {
		return nil
	}
	uid := types.UID(s)
	return &uid
}

// deviceIndex builds a map from device name to deviceInfo by calling
// deps.DeviceLister.DRADevices. Must be called inside deps.WithLock.
// Called once per PrepareResourceClaims invocation, not per result.
func (p *Plugin) deviceIndex() (map[string]deviceInfo, error) {
	devs, err := p.deps.DeviceLister.DRADevices(p.driverName)
	if err != nil {
		return nil, fmt.Errorf("dra plugin: DRADevices: %w", err)
	}
	idx := make(map[string]deviceInfo, len(devs))
	for _, d := range devs {
		info := deviceInfo{}
		if attr, ok := d.Attributes[cpuclass.AttrCPUClass]; ok && attr.StringValue != nil {
			info.ClassName = *attr.StringValue
		}
		if attr, ok := d.Attributes[cpuclass.AttrPackageID]; ok && attr.IntValue != nil {
			info.PkgID = int(*attr.IntValue)
		}
		if attr, ok := d.Attributes[cpuclass.AttrPunitID]; ok && attr.IntValue != nil {
			info.PunitID = int(*attr.IntValue)
		}
		idx[d.Name] = info
	}
	return idx, nil
}

// allClaimedCPUs returns the union of all CPUs currently tracked in p.claims.
// Must be called inside deps.WithLock.
func (p *Plugin) allClaimedCPUs() cpuset.CPUSet {
	result := cpuset.New()
	for _, cs := range p.claims {
		for _, alloc := range cs.Allocs {
			parsed, err := cpuset.Parse(alloc.CPUs)
			if err == nil {
				result = result.Union(parsed)
			}
		}
	}
	return result
}

// PrepareResourceClaims prepares all resource claims allocated for this driver.
// For each claim it picks HP CPUs, writes a CDI spec, and persists claim state.
// The entire body runs inside deps.WithLock to serialize with Reconfigure.
func (p *Plugin) PrepareResourceClaims(_ context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	result := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	p.deps.WithLock(func() {
		devIdx, idxErr := p.deviceIndex()
		if idxErr != nil {
			// Fill all UIDs with the same error and return.
			for _, claim := range claims {
				result[claim.UID] = kubeletplugin.PrepareResult{Err: idxErr}
			}
			return
		}

		for _, claim := range claims {
			uid := claim.UID
			result[uid] = func() kubeletplugin.PrepareResult {
				// Step 2: nil allocation check.
				if claim.Status.Allocation == nil {
					return kubeletplugin.PrepareResult{Err: errNilAllocation}
				}

				// Step 3: filter results to our driver.
				allResults := claim.Status.Allocation.Devices.Results
				var filtered []resourceapi.DeviceRequestAllocationResult
				for _, r := range allResults {
					if r.Driver == p.driverName {
						filtered = append(filtered, r)
					}
				}
				if len(filtered) == 0 {
					// No results for our driver — valid, no work to do.
					return kubeletplugin.PrepareResult{}
				}

				// Step 4: idempotency — check if already prepared.
				if _, exists := p.claims[uid]; exists {
					if p.deps.CDIWriter.ClaimSpecExists(uid) {
						// Spec present — re-build PrepareResult from stored state.
						return p.buildPrepareResult(uid, filtered)
					}
					// Spec missing (e.g. node reboot) — re-write CDI spec from
					// existing stored state without re-picking CPUs.
					cdiDevices := p.cdiDevicesFromClaims(uid, filtered)
					if len(cdiDevices) != len(p.claims[uid].Allocs) {
						// Some (or all) stored CPU strings failed to parse — stored
						// state is corrupt; writing a partial CDI spec and then
						// returning CDIDeviceIDs for all allocs would give the runtime
						// device IDs that don't exist in the spec, causing container
						// start failures.
						return kubeletplugin.PrepareResult{Err: fmt.Errorf("dra plugin: idempotent re-write: only %d of %d CDI devices rebuilt from stored state (stored CPUs unparseable)", len(cdiDevices), len(p.claims[uid].Allocs))}
					}
					if len(cdiDevices) > 0 {
						if writeErr := p.deps.CDIWriter.WriteClaim(uid, cdiDevices); writeErr != nil {
							return kubeletplugin.PrepareResult{Err: fmt.Errorf("dra plugin: re-write CDI spec: %w", writeErr)}
						}
					}
					return p.buildPrepareResult(uid, filtered)
				}

				// Steps 5-9: allocate CPUs for each filtered result.
				heldCPUs := p.allClaimedCPUs()
				var pickedAllocs []ResultAlloc
				var cdiDevices []CDIDevice

				for i, r := range filtered {
					// Step 5: look up device attrs.
					attrs, attrOk := devIdx[r.Device]
					if !attrOk {
						// Unknown device — rollback and report error.
						p.rollbackPicks(pickedAllocs)
						return kubeletplugin.PrepareResult{Err: fmt.Errorf("dra plugin: unknown device %q", r.Device)}
					}
					if attrs.ClassName == "" {
						p.rollbackPicks(pickedAllocs)
						return kubeletplugin.PrepareResult{Err: fmt.Errorf("dra plugin: device %q missing nri/cpuClass attribute", r.Device)}
					}

					// Step 6: read CPU count from ConsumedCapacity.
					q, ok := r.ConsumedCapacity[cpuclass.CapacityCPUs]
					if !ok {
						p.rollbackPicks(pickedAllocs)
						return kubeletplugin.PrepareResult{Err: errMissingConsumedCapacity}
					}
					n := int(q.Value())
					if n <= 0 {
						p.rollbackPicks(pickedAllocs)
						return kubeletplugin.PrepareResult{Err: errMissingConsumedCapacity}
					}

					// Step 7: HP gate.
					if !p.deps.ClaimAllocator.IsHPClass(attrs.ClassName) {
						p.rollbackPicks(pickedAllocs)
						return kubeletplugin.PrepareResult{Err: errNonHPNotSupported}
					}

					// Step 8: pick CPUs.
					picked, pickErr := p.deps.ClaimAllocator.PickHpCpus(attrs.PkgID, attrs.PunitID, n, heldCPUs)
					if pickErr != nil {
						p.rollbackPicks(pickedAllocs)
						return kubeletplugin.PrepareResult{Err: fmt.Errorf("dra plugin: PickHpCpus: %w", pickErr)}
					}
					heldCPUs = heldCPUs.Union(picked)

					// Determine ShareID.
					shareID := ""
					if r.ShareID != nil {
						shareID = string(*r.ShareID)
					}

					pickedAllocs = append(pickedAllocs, ResultAlloc{
						Request:   r.Request,
						Pool:      r.Pool,
						Device:    r.Device,
						ShareID:   shareID,
						ClassName: attrs.ClassName,
						PkgID:     attrs.PkgID,
						PunitID:   attrs.PunitID,
						CPUs:      picked.String(),
					})
					name := cdiDeviceName(uid, r.Request, r.Device, i)
					cdiDevices = append(cdiDevices, CDIDevice{
						Name:      name,
						ClassName: attrs.ClassName,
						CPUs:      picked,
					})
				}

				// Step 10: write CDI spec.
				if writeErr := p.deps.CDIWriter.WriteClaim(uid, cdiDevices); writeErr != nil {
					p.rollbackPicks(pickedAllocs)
					return kubeletplugin.PrepareResult{Err: fmt.Errorf("dra plugin: WriteClaim: %w", writeErr)}
				}

				// Step 11: persist state.
				p.claims[uid] = &ClaimState{UID: string(uid), Allocs: pickedAllocs}
				if saveErr := p.deps.ClaimStore.Save(p.claims); saveErr != nil {
					p.deps.Logger.Errorf("dra plugin: ClaimStore.Save: %v", saveErr)
				}

				// Step 12: build PrepareResult.
				return p.buildPrepareResult(uid, filtered)
			}()
		}
	})
	return result, nil
}

// cdiDevicesFromClaims rebuilds the []CDIDevice slice for uid from the stored
// ClaimState. filtered provides the allocation results for our driver, in the
// same order as the original allocs. Used by the idempotency spec-missing path.
func (p *Plugin) cdiDevicesFromClaims(uid types.UID, filtered []resourceapi.DeviceRequestAllocationResult) []CDIDevice {
	cs, ok := p.claims[uid]
	if !ok {
		return nil
	}
	devices := make([]CDIDevice, 0, len(cs.Allocs))
	for i, alloc := range cs.Allocs {
		if i >= len(filtered) {
			break
		}
		r := filtered[i]
		name := cdiDeviceName(uid, r.Request, r.Device, i)
		cpus, err := cpuset.Parse(alloc.CPUs)
		if err != nil {
			continue
		}
		devices = append(devices, CDIDevice{
			Name:      name,
			ClassName: alloc.ClassName,
			CPUs:      cpus,
		})
	}
	return devices
}

// rollbackPicks releases all CPU picks accumulated so far for a claim that
// encountered an error mid-way through allocation.
func (p *Plugin) rollbackPicks(allocs []ResultAlloc) {
	for _, a := range allocs {
		cs, err := cpuset.Parse(a.CPUs)
		if err != nil {
			continue
		}
		p.deps.ClaimAllocator.ReleaseHpCpus(a.PkgID, a.PunitID, cs)
	}
}

// buildPrepareResult constructs a kubeletplugin.PrepareResult from the stored
// claim state for uid. filtered contains the allocation results for our driver,
// in the same order they were originally processed (positional index matches
// cdiDeviceName index).
func (p *Plugin) buildPrepareResult(uid types.UID, filtered []resourceapi.DeviceRequestAllocationResult) kubeletplugin.PrepareResult {
	cs, ok := p.claims[uid]
	if !ok {
		return kubeletplugin.PrepareResult{}
	}
	devices := make([]kubeletplugin.Device, 0, len(cs.Allocs))
	for i, alloc := range cs.Allocs {
		if i >= len(filtered) {
			break
		}
		r := filtered[i]
		name := cdiDeviceName(uid, r.Request, r.Device, i)
		devices = append(devices, kubeletplugin.Device{
			Requests:     []string{r.Request},
			PoolName:     r.Pool,
			DeviceName:   r.Device,
			CDIDeviceIDs: []string{parser.QualifiedName(p.driverName, "device", name)},
			ShareID:      shareIDPtr(alloc.ShareID),
		})
	}
	return kubeletplugin.PrepareResult{Devices: devices}
}

// UnprepareResourceClaims releases CPUs and CDI specs for the given claims and
// removes them from persisted state. The entire body runs inside deps.WithLock
// to serialize with Reconfigure.
func (p *Plugin) UnprepareResourceClaims(_ context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	perUID := make(map[types.UID]error, len(claims))
	p.deps.WithLock(func() {
		for _, obj := range claims {
			uid := obj.UID
			cs, exists := p.claims[uid]
			if !exists {
				p.deps.Logger.Warnf("dra plugin: UnprepareResourceClaims: claim %s not found in state", uid)
				perUID[uid] = nil
				continue
			}
			// Release CPUs for each allocation result; parse errors are logged
			// but do not block CDI removal or claim deletion.
			for _, alloc := range cs.Allocs {
				cpus, err := cpuset.Parse(alloc.CPUs)
				if err != nil {
					p.deps.Logger.Warnf("dra plugin: UnprepareResourceClaims: claim %s device %s: parse CPUs %q: %v (skipping release)", uid, alloc.Device, alloc.CPUs, err)
					continue
				}
				p.deps.ClaimAllocator.ReleaseHpCpus(alloc.PkgID, alloc.PunitID, cpus)
			}
			// Remove CDI spec unconditionally; log but do not block deletion.
			if err := p.deps.CDIWriter.RemoveClaim(uid); err != nil {
				p.deps.Logger.Warnf("dra plugin: UnprepareResourceClaims: claim %s: RemoveClaim: %v", uid, err)
			}
			delete(p.claims, uid)
			perUID[uid] = nil
		}
		// Persist the updated claims map in a single batch write.
		if saveErr := p.deps.ClaimStore.Save(p.claims); saveErr != nil {
			p.deps.Logger.Errorf("dra plugin: UnprepareResourceClaims: ClaimStore.Save: %v", saveErr)
		}
	})
	return perUID, nil
}

// LiveClaimClasses returns a map from className to the number of live claims
// using that class. Each claim is counted once per distinct class it uses.
// Caller must hold the resmgr lock (do not call from inside a WithLock
// callback — the resmgr lock is not reentrant). Used by the Step 8
// Reconfigure refusal check (resolved decision 8 / Option B: refuse
// Reconfigure that changes class-derived attributes while claims are live).
func (p *Plugin) LiveClaimClasses() map[string]int {
	result := make(map[string]int)
	for _, cs := range p.claims {
		// Count each claim once per distinct class it uses.
		seen := make(map[string]bool)
		for _, alloc := range cs.Allocs {
			if alloc.ClassName != "" && !seen[alloc.ClassName] {
				result[alloc.ClassName]++
				seen[alloc.ClassName] = true
			}
		}
	}
	return result
}

// LiveClaimsLocked returns a snapshot of the currently live claims as
// map[types.UID][]ResultAlloc. Caller must hold the resmgr lock (do not call
// from inside a WithLock callback — the resmgr lock is not reentrant). Used
// by the Step 8 pool-accounting re-apply path (reapplyDRAClaims) after
// Start()/Reconfigure() rebuild policy state.
func (p *Plugin) LiveClaimsLocked() map[types.UID][]ResultAlloc {
	result := make(map[types.UID][]ResultAlloc, len(p.claims))
	for uid, cs := range p.claims {
		allocs := make([]ResultAlloc, len(cs.Allocs))
		copy(allocs, cs.Allocs)
		result[uid] = allocs
	}
	return result
}

// RestoreClaimsLocked re-runs AccountHpCpus for every entry in p.claims,
// rebuilding HP accounting after a Reconfigure that reset hpDRAUsed. It does
// not reload from cache and does not acquire any lock.
//
// Caller must already hold the resmgr lock; do not call via WithLock.
// RestoreClaims is the complementary WithLock wrapper for callers that are not
// already holding the lock.
func (p *Plugin) RestoreClaimsLocked() error {
	var errs []error
	for _, cs := range p.claims {
		for _, alloc := range cs.Allocs {
			cpus, err := cpuset.Parse(alloc.CPUs)
			if err != nil {
				p.deps.Logger.Warnf("dra plugin: RestoreClaimsLocked: claim %s device %s: parse CPUs %q: %v (skipping)", cs.UID, alloc.Device, alloc.CPUs, err)
				continue
			}
			if err := p.deps.ClaimAllocator.AccountHpCpus(alloc.PkgID, alloc.PunitID, cpus); err != nil {
				p.deps.Logger.Warnf("dra plugin: RestoreClaimsLocked: claim %s device %s: AccountHpCpus: %v", cs.UID, alloc.Device, err)
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// RestoreClaims wraps RestoreClaimsLocked inside deps.WithLock for callers
// that are not already holding the resmgr lock.
//
// RestoreClaims must not be called while holding the resmgr lock
// (see RestoreClaimsLocked for the complementary lock-already-held variant).
func (p *Plugin) RestoreClaims() error {
	var err error
	p.deps.WithLock(func() {
		err = p.RestoreClaimsLocked()
	})
	return err
}

// Start registers this plugin with the kubelet and begins serving DRA
// requests. It validates cpuClass configuration, loads and reconciles
// persisted claim state, creates the plugin data directory, injects a
// logr.Logger into the context, and calls kubeletplugin.Start. Returns an
// error if the plugin is already started, if ValidateClasses fails, or if
// the kubelet plugin cannot be started.
//
// Start must not be called while holding the resmgr lock
// (see RestoreClaimsLocked for the complementary lock-already-held variant).
//
// cpuclass.Handler must have completed Configure() before Start() so that
// AccountHpCpus finds active punits. If the allocator is inactive when Start
// is called, live claims are kept with a warning (same over-commit model as
// PrepareResourceClaims).
func (p *Plugin) Start(ctx context.Context) error {
	p.mu.Lock()
	alreadyStarted := p.helper != nil
	p.mu.Unlock()
	if alreadyStarted {
		return fmt.Errorf("dra plugin: already started")
	}
	if err := p.deps.ValidateClasses(); err != nil {
		return fmt.Errorf("dra plugin: ValidateClasses failed: %w", err)
	}

	// Reconcile persisted claims inside the resmgr lock before registering
	// with the kubelet so that accounting is consistent before any new
	// Prepare/Unprepare calls can arrive.
	p.deps.WithLock(func() {
		loaded, loadErr := p.deps.ClaimStore.Load()
		if loadErr != nil {
			p.deps.Logger.Warnf("dra plugin: Start: ClaimStore.Load: %v (proceeding with empty claims)", loadErr)
		} else if loaded != nil {
			p.claims = loaded
		}

		// For each persisted claim: if CDI spec is present on disk, re-account
		// CPUs and keep the claim. If the spec is absent the container has been
		// cleaned up; drop the stale claim.
		var dropped bool
		for uid, cs := range p.claims {
			if p.deps.CDIWriter.ClaimSpecExists(uid) {
				for _, alloc := range cs.Allocs {
					cpus, err := cpuset.Parse(alloc.CPUs)
					if err != nil {
						p.deps.Logger.Warnf("dra plugin: Start: claim %s device %s: parse CPUs %q: %v (skipping AccountHpCpus)", uid, alloc.Device, alloc.CPUs, err)
						continue
					}
					if err := p.deps.ClaimAllocator.AccountHpCpus(alloc.PkgID, alloc.PunitID, cpus); err != nil {
						p.deps.Logger.Warnf("dra plugin: Start: claim %s device %s: AccountHpCpus: %v (keeping claim — container may be running)", uid, alloc.Device, err)
					}
				}
			} else {
				p.deps.Logger.Warnf("dra plugin: dropping stale claim %s (CDI spec missing)", uid)
				delete(p.claims, uid)
				dropped = true
			}
		}
		if dropped {
			if saveErr := p.deps.ClaimStore.Save(p.claims); saveErr != nil {
				p.deps.Logger.Errorf("dra plugin: Start: ClaimStore.Save after dropping stale claims: %v", saveErr)
			}
		}

		// Orphan sweep: remove CDI specs that have no corresponding live claim.
		listed, listErr := p.deps.CDIWriter.ListClaims()
		if listErr != nil {
			p.deps.Logger.Warnf("dra plugin: Start: CDIWriter.ListClaims: %v (skipping orphan sweep)", listErr)
			return
		}
		for _, uid := range listed {
			if _, ok := p.claims[uid]; !ok {
				if removeErr := p.deps.CDIWriter.RemoveClaim(uid); removeErr != nil {
					p.deps.Logger.Warnf("dra plugin: Start: orphan sweep: RemoveClaim %s: %v", uid, removeErr)
				}
			}
		}
	})

	// Resolve the plugin data directory default before creating it: an empty
	// string passed to os.MkdirAll would fail immediately.
	pluginDataDir := p.deps.PluginDataDir
	if pluginDataDir == "" {
		pluginDataDir = filepath.Join(kubeletplugin.KubeletPluginsDir, p.driverName)
	}
	if err := os.MkdirAll(pluginDataDir, 0750); err != nil {
		return fmt.Errorf("dra plugin: create plugin data dir %q: %w", pluginDataDir, err)
	}
	ctx = logr.NewContext(ctx, newLogr(p.deps.Logger))
	opts := []kubeletplugin.Option{
		kubeletplugin.DriverName(p.driverName),
		kubeletplugin.KubeClient(p.deps.KubeClient),
		kubeletplugin.NodeName(p.deps.NodeName),
		kubeletplugin.PluginDataDirectoryPath(pluginDataDir),
		kubeletplugin.GRPCVerbosity(-1),
	}
	// Only override the registrar directory when explicitly set; passing an
	// empty string would clobber kubeletplugin's built-in KubeletRegistryDir
	// default (the option setter stores the value unconditionally).
	if p.deps.RegistrarDir != "" {
		opts = append(opts, kubeletplugin.RegistrarDirectoryPath(p.deps.RegistrarDir))
	}
	helper, err := kubeletplugin.Start(ctx, p, opts...)
	if err != nil {
		return fmt.Errorf("dra plugin: kubeletplugin.Start: %w", err)
	}
	// Re-check under lock to close the TOCTOU window between the initial
	// guard (above) and this assignment. If a racing Start() won, stop the
	// helper we just created and return an error.
	p.mu.Lock()
	if p.helper != nil {
		p.mu.Unlock()
		helper.Stop()
		return fmt.Errorf("dra plugin: already started")
	}
	p.helper = helper
	p.mu.Unlock()
	return nil
}

// Stop shuts down the kubelet plugin and releases resources. It is
// idempotent: calling Stop on an already-stopped Plugin is safe.
func (p *Plugin) Stop() {
	p.mu.Lock()
	h := p.helper
	p.helper = nil
	p.mu.Unlock()
	if h != nil {
		h.Stop()
	}
}

// PublishResources validates classes, lists DRA devices, paginates them into
// ResourceSlice objects (at most resourceapi.ResourceSliceMaxDevices per
// slice), and hands the resulting DriverResources to the helper for
// publishing. Even zero devices produce one empty slice so the pool remains
// visible. Returns an error if the plugin has not been started yet.
//
// PublishResources must not be called while holding the resmgr lock
// (see RestoreClaimsLocked for the complementary lock-already-held variant).
func (p *Plugin) PublishResources(ctx context.Context) error {
	var (
		validateErr error
		devices     []resourceapi.Device
		devicesErr  error
	)
	p.deps.WithLock(func() {
		validateErr = p.deps.ValidateClasses()
		if validateErr != nil {
			return
		}
		devices, devicesErr = p.deps.DeviceLister.DRADevices(p.driverName)
	})
	if validateErr != nil {
		return fmt.Errorf("dra plugin: ValidateClasses failed: %w", validateErr)
	}
	p.mu.Lock()
	h := p.helper
	p.mu.Unlock()
	if h == nil {
		return fmt.Errorf("dra plugin: PublishResources called before Start")
	}
	if devicesErr != nil {
		return fmt.Errorf("dra plugin: DRADevices: %w", devicesErr)
	}
	resources := buildDriverResources(p.deps.NodeName, devices)
	if err := h.PublishResources(ctx, resources); err != nil {
		return fmt.Errorf("dra plugin: helper.PublishResources: %w", err)
	}
	return nil
}

// buildDriverResources paginates devices into ResourceSlice objects and
// returns a DriverResources ready for Helper.PublishResources. The pool name
// is the node name. At most resourceapi.ResourceSliceMaxDevices devices are
// placed per slice; even zero devices produce one empty slice.
func buildDriverResources(nodeName string, devices []resourceapi.Device) resourceslice.DriverResources {
	maxPerSlice := resourceapi.ResourceSliceMaxDevices
	var slices []resourceslice.Slice
	if len(devices) == 0 {
		slices = []resourceslice.Slice{{}}
	} else {
		for i := 0; i < len(devices); i += maxPerSlice {
			end := i + maxPerSlice
			if end > len(devices) {
				end = len(devices)
			}
			slices = append(slices, resourceslice.Slice{
				Devices: devices[i:end],
			})
		}
	}
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {Slices: slices},
		},
	}
}

// HandleError handles background errors from the kubelet plugin helper.
// Recoverable errors (errors.Is(err, kubeletplugin.ErrRecoverable)) are
// logged at Warn level; all other errors are logged at Error level.
func (p *Plugin) HandleError(_ context.Context, err error, msg string) {
	if errors.Is(err, kubeletplugin.ErrRecoverable) {
		p.deps.Logger.Warnf("%s: %v", msg, err)
	} else {
		p.deps.Logger.Errorf("%s: %v", msg, err)
	}
}

// WatchHealthStatus is not implemented: this driver does not report
// per-device health, so it returns ErrHealthNotSupported as documented by
// kubeletplugin.DRAPlugin.
func (p *Plugin) WatchHealthStatus(_ context.Context, _ chan<- kubeletplugin.DeviceHealthReport) error {
	return kubeletplugin.ErrHealthNotSupported
}
