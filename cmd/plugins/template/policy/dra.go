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

package template

// This file shows how a policy takes part in DRA. The policy publishes the
// CPUs it is allowed to use as one DRA device with consumable capacity: a
// claim asks for a number of CPUs, the scheduler accounts for how many of
// them are taken, and the policy decides which CPUs a claim gets.
//
// The template policy does not manage cpusets, so a claimed CPU is not
// withheld from other containers.

import (
	"fmt"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/template"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	specs "tags.cncf.io/container-device-interface/specs-go"
)

const (
	// draDevice is the one device we publish.
	draDevice = "cpus"
	// draCapacity is the capacity of that device: the number of CPUs. It has
	// no domain, so its domain is our driver name. Claims should request it
	// as "cpus", the only form every Kubernetes version matches.
	draCapacity resourceapi.QualifiedName = "cpus"
	// keyClaims is the cache entry claimed CPUs are kept in.
	keyClaims = "claims"
)

// publishDRADevices publishes our allowed CPUs. A configuration DRA cannot
// use is only a warning: it must not stop us from running without DRA, and
// publishing no devices leaves no claims to fail on it.
func (p *policy) publishDRADevices() error {
	devices, err := p.draDevices()
	if err != nil {
		log.Warnf("publishing no DRA devices: %v", err)
	}
	return p.owner.PublishDRADevices(devices)
}

// draDevices returns our allowed CPUs as a single device.
func (p *policy) draDevices() ([]resourceapi.Device, error) {
	cpus, err := p.allowedCPUs()
	if err != nil {
		return nil, err
	}

	// Without a request policy a claim which names no amount would take
	// the whole capacity. Take one CPU instead, and only whole CPUs.
	one := resource.MustParse("1")
	return []resourceapi.Device{
		{
			Name:                     draDevice,
			AllowMultipleAllocations: ptr.To(true),
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				draCapacity: {
					Value: *resource.NewQuantity(int64(cpus.Size()), resource.DecimalSI),
					RequestPolicy: &resourceapi.CapacityRequestPolicy{
						Default: &one,
						ValidRange: &resourceapi.CapacityRequestPolicyRange{
							Min:  &one,
							Step: &one,
						},
					},
				},
			},
		},
	}, nil
}

// AllocateClaim picks CPUs for a claim, as many as its results consumed.
func (p *policy) AllocateClaim(
	claim *resourceapi.ResourceClaim,
	results []resourceapi.DeviceRequestAllocationResult,
) ([]specs.ContainerEdits, error) {
	counts, total, err := consumedCPUs(results)
	if err != nil {
		return nil, fmt.Errorf("claim %s: %w", claim.UID, err)
	}

	uid := string(claim.UID)
	cpus, ok := p.claims[uid]
	if !ok {
		cpus, err = p.pickCPUs(total)
		if err != nil {
			return nil, fmt.Errorf("claim %s: %w", claim.UID, err)
		}
		p.claims[uid] = cpus
		p.cache.SetPolicyEntry(keyClaims, p.claims)
	}

	if cpus.Size() != total {
		return nil, fmt.Errorf("claim %s holds %d CPUs but consumed %d",
			claim.UID, cpus.Size(), total)
	}

	return claimEdits(cpus, counts), nil
}

// ReleaseClaim gives the CPUs of a claim back.
func (p *policy) ReleaseClaim(uid types.UID) error {
	delete(p.claims, string(uid))
	p.cache.SetPolicyEntry(keyClaims, p.claims)
	return nil
}

// restoreClaims loads the claimed CPUs saved before a restart.
func (p *policy) restoreClaims() {
	p.claims = map[string]cpuset.CPUSet{}
	p.cache.GetPolicyEntry(keyClaims, &p.claims)
}

// allowedCPUs returns the CPUs we are allowed to hand out to claims.
func (p *policy) allowedCPUs() (cpuset.CPUSet, error) {
	cpus := p.system.CPUSet()
	switch amount, kind := p.cfg.AvailableResources.Get(cfgapi.CPU); kind {
	case cfgapi.AmountAbsent:
	case cfgapi.AmountCPUSet:
		cset, err := amount.ParseCPUSet()
		if err != nil {
			return cpuset.New(), err
		}
		cpus = cset
	default:
		return cpuset.New(), fmt.Errorf("available CPUs must be a cpuset for DRA, not %q",
			p.cfg.AvailableResources[cfgapi.CPU])
	}

	// Reserved CPUs given as a quantity cannot be subtracted from a set.
	// We do not place reserved workloads, so we can publish them instead.
	if amount, kind := p.cfg.ReservedResources.Get(cfgapi.CPU); kind == cfgapi.AmountCPUSet {
		cset, err := amount.ParseCPUSet()
		if err != nil {
			return cpuset.New(), err
		}
		cpus = cpus.Difference(cset)
	}

	return cpus.Intersection(p.system.OnlineCPUs()), nil
}

// pickCPUs picks n of the CPUs no claim holds.
func (p *policy) pickCPUs(n int) (cpuset.CPUSet, error) {
	free, err := p.allowedCPUs()
	if err != nil {
		return cpuset.New(), err
	}
	for _, cpus := range p.claims {
		free = free.Difference(cpus)
	}

	if free.Size() < n {
		return cpuset.New(), fmt.Errorf("%d CPUs requested, only %d free", n, free.Size())
	}

	// Take the lowest IDs. A policy which knows the topology picks by it.
	return cpuset.New(free.List()[:n]...), nil
}

// consumedCPUs returns how many CPUs each result consumed, and their sum.
func consumedCPUs(results []resourceapi.DeviceRequestAllocationResult) ([]int, int, error) {
	counts := make([]int, len(results))
	total := 0
	for i, r := range results {
		// "cpus" and "<driver>/cpus" name the same capacity, and a result
		// may use either, so a lookup must try both.
		q, ok := r.ConsumedCapacity[draCapacity]
		if !ok {
			q, ok = r.ConsumedCapacity[resourceapi.QualifiedName(r.Driver+"/"+string(draCapacity))]
		}
		if !ok {
			return nil, 0, fmt.Errorf("result %d consumed no %s", i, draCapacity)
		}
		n, ok := q.AsInt64()
		if !ok || n < 1 {
			return nil, 0, fmt.Errorf("result %d consumed %s %s, not a whole number",
				i, q.String(), draCapacity)
		}
		counts[i] = int(n)
		total += int(n)
	}
	return counts, total, nil
}

// claimEdits hands the CPUs of a claim out to its results in order, lowest
// first, each result taking as many as it consumed. The results are the same
// every time a claim is prepared, so the edits come out the same every time.
//
// Each CPU gets a variable of its own rather than all of them sharing one:
// a container given several results would otherwise keep only the value of
// the last one, as CDI cannot merge the values of a variable.
func claimEdits(cpus cpuset.CPUSet, counts []int) []specs.ContainerEdits {
	list := cpus.List()
	edits := make([]specs.ContainerEdits, len(counts))
	for i, n := range counts {
		for _, cpu := range list[:n] {
			edits[i].Env = append(edits[i].Env, fmt.Sprintf("NRI_TEMPLATE_CPU%d=claimed", cpu))
		}
		list = list[n:]
	}
	return edits
}
