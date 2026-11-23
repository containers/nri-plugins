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

package containers

import (
	"fmt"
	"slices"
	"strings"

	"github.com/containerd/nri/pkg/api"
	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/steering"
	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// Results are the prepared claims of a DRA driver.
type Results interface {
	// PreparedResult returns the claim name and the prepared device of
	// the allocation result n of a prepared claim, and false if there is
	// none.
	PreparedResult(uid string, n int) (string, state.DeviceInfo, bool)
}

// resultRef is an allocation result of a claim: the claim UID and the
// index of the result in the allocation of the claim.
type resultRef struct {
	uid string
	n   int
}

// result is a prepared allocation result of either driver that a container
// uses.
type result struct {
	// deviceType is the device type of the result.
	deviceType string
	mode       string
	numaNodes  []int
	// bytes is the memory of the result.
	bytes    int64
	settings config.ResultSettings
}

// numaCXL tells whether the result has CXL memory in mode numa: a numa
// slice of a local memdev, a region or a pool device in mode numa.
func (r *result) numaCXL() bool {
	return r.mode == cxlapi.ModeNUMA && (r.deviceType == device.DeviceTypeMemdev || r.deviceType == device.DeviceTypeRegion)
}

// decision is the management of the memory of a container that the
// plugin decides from its results.
type decision struct {
	management Management
	// eff are the effective container settings.
	eff *config.Effective
	// claimed are the NUMA nodes of the CXL numa memory of the results.
	claimed cpuset.CPUSet
	// plan is the steering plan of a steered container, if built.
	plan *steering.Plan
	// policy is the steering policy of the plan.
	policy steering.Policy
	// mem is the memory to steer.
	mem steering.MemoryTypes
	// capacityOnly: the results have DRAM or region memory that is not
	// steered.
	capacityOnly bool
}

// extractResults returns the unique allocation results of the markers
// CXL_CLAIM_<uid>_<n> in an environment: kubelet does not deduplicate CDI
// device IDs.
func extractResults(env []string) []resultRef {
	seen := make(map[resultRef]bool)
	var refs []resultRef
	for _, e := range env {
		uid, n, ok := cxlapi.ParseResultMarker(e)
		if ref := (resultRef{uid: uid, n: n}); ok && !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	return refs
}

// results returns the prepared allocation results of both drivers that refs
// refer to; results that are not prepared, and those of claims that are not
// the pod's (foreignClaim), are left out.
func (p *Plugin) results(pod *api.PodSandbox, ctrName string, refs []resultRef) []result {
	var out []result
	for _, ref := range refs {
		if why := p.foreignClaim(pod, ref.uid); why != "" {
			klog.Warningf("NRI CreateContainer: ctr=%s: ignoring the marker of claim %s: %s", ctrName, ref.uid, why)
			continue
		}
		if r, ok := p.result(ctrName, ref); ok {
			out = append(out, r)
			continue
		}
		klog.V(5).Infof("NRI CreateContainer: ctr=%s claim=%s result %d: not prepared (driver may have restarted)", ctrName, ref.uid, ref.n)
	}
	return out
}

// foreignClaim tells why the claim uid of a marker in the environment of a
// container of the pod is not the pod's, "" when it may be: a preparation
// of the claim in another namespace, or a pod that is not in the
// reservedFor that its lease claim recorded.
func (p *Plugin) foreignClaim(pod *api.PodSandbox, uid string) string {
	if p.opts.Store == nil {
		return ""
	}
	doc := p.opts.Store.Snapshot()
	for i := range doc.Preparations {
		prep := &doc.Preparations[i]
		if ns, _, _ := strings.Cut(prep.ClaimName, "/"); prep.UID == uid && ns != pod.GetNamespace() {
			return fmt.Sprintf("the claim %s is not of namespace %s", prep.ClaimName, pod.GetNamespace())
		}
	}
	for i := range doc.LeaseClaims {
		lc := &doc.LeaseClaims[i]
		if lc.UID == uid && len(lc.Pods) > 0 && !slices.ContainsFunc(lc.Pods, func(r state.PodRef) bool { return r.UID == pod.GetUid() }) {
			return fmt.Sprintf("pod %s/%s is not in the reservedFor of the claim %s/%s", pod.GetNamespace(), pod.GetName(), lc.Namespace, lc.Name)
		}
	}
	return ""
}

// result returns the prepared result ref of a driver, and false if there
// is none.
func (p *Plugin) result(ctrName string, ref resultRef) (result, bool) {
	for _, drv := range p.opts.Results {
		claim, d, ok := drv.PreparedResult(ref.uid, ref.n)
		if !ok {
			continue
		}
		klog.V(5).Infof("NRI CreateContainer: ctr=%s claim=%s (%s) result %d: name=%s type=%s mode=%s sysfs=%s numa=%v affinities=%v total=%s consumed=%s request=%s",
			ctrName, ref.uid, claim, ref.n,
			d.DeviceName, d.DeviceType, d.Mode, d.SysfsPath, d.NUMANodes, d.NUMAAffinities,
			formatBytes(d.TotalBytes), formatBytes(uint64(d.ConsumedBytes)), d.RequestName)
		return result{deviceType: d.DeviceType, mode: d.Mode, numaNodes: nonNegative(d.NUMANodes), bytes: d.ConsumedBytes,
			settings: d.ResultSettings}, true
	}
	return result{}, false
}

// decide returns the management of the memory of a container that uses
// the results, with the steering plan of a steered container if plan is
// set. A container is steered when its effective steering policy is not
// none and its results have DRAM or CXL numa memory; otherwise it is
// fenced when its effective fencing is allowed-mems, else none. Returns an
// error if the container settings of the results conflict or are invalid,
// or the steering plan cannot be built.
func (p *Plugin) decide(ctrName string, results []result, plan bool) (*decision, error) {
	settings := make([]config.ResultSettings, 0, len(results))
	for _, r := range results {
		settings = append(settings, r.settings)
	}
	eff, err := config.Combine(settings, p.opts.Defaults)
	if err != nil {
		return nil, fmt.Errorf("container %s: %w", ctrName, err)
	}
	d := &decision{management: ManagementUnfenced, eff: eff, claimed: claimedNodes(results), mem: steeredMemory(results)}
	if policy, ok := steeringPolicy(eff.Container.Steering, d.mem); ok {
		d.management, d.policy = ManagementSteered, policy
		if plan {
			if d.plan, err = steeringPlan(eff.Container.Steering, policy, d.mem); err != nil {
				return nil, fmt.Errorf("NRI CreateContainer: ctr=%s: failed to build memory steering plan: %w", ctrName, err)
			}
		}
		return d, nil
	}
	d.management = unsteered(eff)
	for _, r := range results {
		if r.deviceType == device.DeviceTypeDRAM || (r.deviceType == device.DeviceTypeRegion && r.mode == cxlapi.ModeNUMA) {
			d.capacityOnly = true
		}
	}
	return d, nil
}

// unsteered returns the management of the memory of a container that is
// not steered: fenced when its effective fencing is allowed-mems, else
// none.
func unsteered(eff *config.Effective) Management {
	if eff.Container.Fencing == config.FencingAllowedMems {
		return ManagementFenced
	}
	return ManagementUnfenced
}

// defaultManagement returns the management of the memory of a container
// that uses no allocation results.
func (p *Plugin) defaultManagement() Management {
	eff, err := config.Combine(nil, p.opts.Defaults)
	if err != nil {
		return ManagementUnfenced
	}
	return unsteered(eff)
}

// logDecision logs the management of the memory of a new container.
//   - hasResults: the container uses allocation results
func logDecision(ctrName string, d *decision, hasResults bool) {
	klog.V(3).Infof("NRI CreateContainer: ctr=%s: memory %s (steering %s, fencing %s)",
		ctrName, d.management.text(), d.eff.PolicyText(), d.eff.FencingText())
	switch {
	case d.management == ManagementSteered && d.plan != nil:
		klog.V(5).Infof("NRI CreateContainer: ctr=%s: prepared memory steering (policy=%s, dramNodes=%v, cxlNodes=%v, %s)",
			ctrName, d.policy, d.mem.DRAMNUMANodes, d.mem.CXLNUMANodes, d.plan)
	case d.management == ManagementSteered:
	case d.eff.Steers() && hasResults:
		klog.V(3).Infof("NRI CreateContainer: ctr=%s: steering policy %s: no numa memory to steer", ctrName, d.eff.Container.Steering.Policy)
	}
	if d.capacityOnly {
		klog.V(3).Infof("NRI CreateContainer: ctr=%s: dram and region memory is not steered: the claims reserve capacity only", ctrName)
	}
}

// text returns the management as the log lines name it: steered, fenced
// or none.
func (m Management) text() string {
	switch m {
	case ManagementSteered:
		return "steered"
	case ManagementFenced:
		return "fenced"
	case ManagementUnfenced:
		return "none"
	}
	return string(m)
}

// claimedNodes returns the NUMA nodes of the CXL numa memory of the
// results.
func claimedNodes(results []result) cpuset.CPUSet {
	nodes := cpuset.New()
	for i := range results {
		if results[i].numaCXL() {
			nodes = nodes.Union(cpuset.New(results[i].numaNodes...))
		}
	}
	return nodes
}

// steeredMemory returns the memory types and quotas of the results: DRAM
// of dram devices, CXL of numa slices of local memdevs, regions and pool
// devices in mode numa.
func steeredMemory(results []result) steering.MemoryTypes {
	dram, cxl := cpuset.New(), cpuset.New()
	var mem steering.MemoryTypes
	for i := range results {
		r := &results[i]
		switch {
		case r.deviceType == device.DeviceTypeDRAM:
			dram = dram.Union(cpuset.New(r.numaNodes...))
			mem.DRAMQuota += r.bytes
		case r.numaCXL():
			cxl = cxl.Union(cpuset.New(r.numaNodes...))
			mem.CXLQuota += r.bytes
		}
	}
	mem.DRAMNUMANodes, mem.CXLNUMANodes = dram.List(), cxl.List()
	return mem
}

// nonNegative returns the nodes that are not negative.
func nonNegative(nodes []int) []int {
	out := make([]int, 0, len(nodes))
	for _, n := range nodes {
		if n >= 0 {
			out = append(out, n)
		}
	}
	return out
}

// formatBytes formats a byte count as a human-readable string using
// binary units (KiB, MiB, GiB).
func formatBytes(bytes uint64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case bytes >= gib:
		return fmt.Sprintf("%.1fGiB", float64(bytes)/float64(gib))
	case bytes >= mib:
		return fmt.Sprintf("%.1fMiB", float64(bytes)/float64(mib))
	case bytes >= kib:
		return fmt.Sprintf("%.1fKiB", float64(bytes)/float64(kib))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}
