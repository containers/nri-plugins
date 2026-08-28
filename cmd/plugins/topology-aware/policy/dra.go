// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package topologyaware

import (
	"encoding/json"
	"fmt"
	"sort"

	resapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"

	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"

	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
)

// buildDRAPlugin constructs p.draPlugin from the current policy
// configuration (p.cfg, p.cpuClasses, p.cache) and the given backend
// options. Called once from Setup() when cfg.DRAEnabled() is true.
//
// Any required externally-supplied dependency being unavailable at this
// point in time — no kube client yet, no node name — is treated as "DRA not
// ready yet" rather than a hard Setup() failure: this logs a warning and
// leaves p.draPlugin nil. Every other Backend lifecycle method already
// nil-checks p.draPlugin, so this degrades to "DRA disabled" behavior
// rather than crashing.
//
// An empty (nil) p.cpuClasses — no cpuClasses configured at Setup() time —
// is *not* treated as "not ready yet": the plugin is still built, just with
// an empty device set (every *cpuclass.Handler method the adapter calls is
// nil-receiver-safe, see policyDRAAdapter's doc comment). buildDRAPlugin
// only ever runs once, from Setup(); if it left p.draPlugin nil here, a
// later Reconfigure() that adds cpuClasses could never recover, since
// Reconfigure() never (re)builds p.draPlugin (see this function's own doc
// comment above on why).
//
// Genuine construction failures (CDI writer setup, dra.New's own
// dependency validation) are returned as errors, since those indicate a
// real misconfiguration rather than a timing issue.
//
// buildDRAPlugin is only ever called once, from the initial Setup() call
// (never from Reconfigure()): p.cfg is nil at construction time (the policy
// is zero-valued until Setup() runs), so a "detect Reconfigure by checking
// p.cfg" heuristic here would be unreachable dead code. Flip detection for
// cfg.DRAEnabled() across a later Reconfigure() therefore lives in
// Reconfigure() itself, which refuses any such change outright rather than
// tearing down/rebuilding p.draPlugin (see Reconfigure's own comment).
//
// opts.KubeClientFn (see the BackendOptions doc) may return a nil
// kubernetes.Interface — either because opts.KubeClientFn itself is nil
// (defensive; should not happen in production wiring) or because the
// agent has no kube client yet (local-config mode, or too early in
// startup). Both are treated identically: no kube client, no DRA plugin
// this Setup() call.
func (p *policy) buildDRAPlugin(opts *policyapi.BackendOptions) error {
	if opts.KubeClientFn == nil {
		log.Warnf("dra: no KubeClientFn provided, DRA plugin not started")
		return nil
	}
	kubeClient := opts.KubeClientFn()
	if kubeClient == nil {
		log.Warnf("dra: no kube client available yet, DRA plugin not started")
		return nil
	}
	if opts.NodeName == "" {
		log.Warnf("dra: node name not known yet, DRA plugin not started")
		return nil
	}

	adapter := &policyDRAAdapter{p: p}

	cdiWriter, err := dra.NewCDIWriter(DRADriverName, p.cdiDir)
	if err != nil {
		return policyError("failed to create DRA CDI writer: %w", err)
	}

	deps := dra.Deps{
		KubeClient: kubeClient,
		NodeName:   opts.NodeName,
		// ValidateClasses closure captures p, not a config snapshot: it
		// must observe whatever p.cfg is live at call time, including
		// after a later Reconfigure() swaps p.cfg for a new *Config.
		// DRASharedCounters() is the nil-safe getter (see the
		// ValidateClasses nil-guard note in the plan's Context section):
		// a Reconfigure that removes the dra: section would otherwise
		// panic on p.cfg.DRA.SharedCounters inside deps.WithLock.
		ValidateClasses: func() error {
			return cpuclass.ValidateCPUClassesForDRA(p.cfg.CPUClasses, p.cfg.DRASharedCounters())
		},
		DeviceLister:   adapter,
		ClaimAllocator: adapter,
		CDIWriter:      cdiWriter,
		ClaimStore:     dra.NewCacheClaimStore(p.cache),
		SupplyUnclaim: func(uid types.UID, cpus cpuset.CPUSet) {
			pool, err := p.poolForCPUs(cpus)
			if err != nil {
				log.Errorf("dra: unprepare claim %s: poolForCPUs: %v", uid, err)
				return
			}
			pool.FreeSupply().UnclaimCPUs(uid)
			p.resetCpuClass(fmt.Sprintf("dra: unprepare claim %s", uid), cpus)
			p.updateSharedAllocations(nil)
		},
		WithLock: opts.WithLock,
		Logger:   log,
	}

	plugin, err := dra.New(DRADriverName, deps)
	if err != nil {
		return policyError("failed to create DRA plugin: %w", err)
	}

	p.draPlugin = plugin

	return nil
}

// triggerDRARepublish enqueues a DRA ResourceSlice republication when
// PCT-based HP capacity is active. Called (deferred) from AllocateResources
// and ReleaseResources so that any change to non-DRA HP CPU usage (hpUsed)
// caused by those NRI-path allocations is reflected in the next ResourceSlice.
// The enqueue is non-blocking and safe to call while holding the resmgr lock;
// the actual publish runs in the DRA plugin's republisherLoop goroutine, which
// acquires the lock itself after the NRI handler releases it.
func (p *policy) triggerDRARepublish() {
	if p.draPlugin == nil {
		return
	}
	if p.cpuClasses == nil || !p.cpuClasses.PctActive() {
		return
	}
	p.draPlugin.TriggerRepublish()
}

// PostReconfigure re-publishes DRA resources after a successful
// Reconfigure(), once the caller (resmgr's updateConfig, via the resmgr
// write lock's own deferred Unlock inside m.reconfigure()) has released the
// resource manager's write lock: PublishResources must not run while that
// lock is held. A no-op when DRA is disabled (draPlugin == nil) — Setup()
// never rebuilds draPlugin here, and Reconfigure() refuses any change to
// cfg.DRAEnabled(), so nil is a stable signal that DRA has been disabled
// since construction.
func (p *policy) PostReconfigure() error {
	if p.draPlugin == nil {
		return nil
	}
	return p.draPlugin.PublishResources(p.draCtx)
}

// draClassNameOf returns the cpuclass.AttrCPUClass attribute value of a DRA
// device, or "" if the device carries no such attribute (should not happen
// for devices built by cpuclass.buildDRADevices, but handled defensively).
func draClassNameOf(d resapi.Device) string {
	if attr, ok := d.Attributes[cpuclass.AttrCPUClass]; ok && attr.StringValue != nil {
		return *attr.StringValue
	}
	return ""
}

// groupDRADevicesByClass partitions devices by their cpuclass.AttrCPUClass
// attribute, sorting each class's devices by name so that two snapshots of
// the same logical device set compare equal regardless of slice order.
func groupDRADevicesByClass(devices []resapi.Device) map[string][]resapi.Device {
	byClass := make(map[string][]resapi.Device)
	for _, d := range devices {
		class := draClassNameOf(d)
		byClass[class] = append(byClass[class], d)
	}
	for class := range byClass {
		sort.Slice(byClass[class], func(i, j int) bool {
			return byClass[class][i].Name < byClass[class][j].Name
		})
	}
	return byClass
}

// changedDRAClasses returns the cpuClass names whose DRA-visible device set
// differs between oldDevices and newDevices — added/removed devices for the
// class, or any attribute/capacity change on a device that class already
// had. Used by Reconfigure() to decide which classes need a live-claim
// check before a new cpuClass configuration can be committed.
func changedDRAClasses(oldDevices, newDevices []resapi.Device) []string {
	oldByClass := groupDRADevicesByClass(oldDevices)
	newByClass := groupDRADevicesByClass(newDevices)

	var changed []string
	seen := make(map[string]bool)
	for class, oldDevs := range oldByClass {
		seen[class] = true
		if !devicesEqual(oldDevs, newByClass[class]) {
			changed = append(changed, class)
		}
	}
	for class, newDevs := range newByClass {
		if seen[class] {
			continue
		}
		if !devicesEqual(oldByClass[class], newDevs) {
			changed = append(changed, class)
		}
	}

	return changed
}

// deviceConfigKey is the config-driven identity of a DRA device: its name,
// attributes, and maximum capacity. Both snapshots passed to devicesEqual are
// produced by DRADevicesAtMaxCapacity (which uses GuaranteedHpCpus, not the
// workload-adjusted HPCapacity = GuaranteedHpCpus − hpUsed), so including
// Capacity here reliably detects hardware-level capacity changes.
type deviceConfigKey struct {
	Name       string
	Attributes map[resapi.QualifiedName]resapi.DeviceAttribute
	Capacity   map[resapi.QualifiedName]resapi.DeviceCapacity
}

// devicesEqual reports whether a and b have the same config-driven identity
// (name + attributes + max capacity). Uses JSON marshaling for safe comparison
// — resapi.DeviceAttribute and resapi.DeviceCapacity both contain
// resource.Quantity whose zero value carries a lazily-cached string;
// reflect.DeepEqual would see two numerically-equal Quantities as different
// whenever that cache differs.
func devicesEqual(a, b []resapi.Device) bool {
	toKeys := func(devs []resapi.Device) []deviceConfigKey {
		keys := make([]deviceConfigKey, len(devs))
		for i, d := range devs {
			keys[i] = deviceConfigKey{Name: d.Name, Attributes: d.Attributes, Capacity: d.Capacity}
		}
		return keys
	}
	aj, aerr := json.Marshal(toKeys(a))
	bj, berr := json.Marshal(toKeys(b))
	if aerr != nil || berr != nil {
		// Should never happen for well-formed k8s API types; fall back to
		// "not equal" so a marshal failure surfaces as a refused Reconfigure
		// (safe direction) rather than a silently accepted attribute change.
		return false
	}
	return string(aj) == string(bj)
}
