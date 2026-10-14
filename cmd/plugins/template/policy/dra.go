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
// withheld from other containers. A container finds the CPUs of its claims
// in the resources we export to it.

import (
	"fmt"
	"math"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/template"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
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
	// draCapacity is the capacity of that device: the number of CPUs.
	draCapacity resourceapi.QualifiedName = "dra.cpu/cpu"
	// keyClaims is the cache entry claimed CPUs are kept in.
	keyClaims = "claims"
)

// draDriverName is the name of our DRA driver.
var draDriverName = dra.DriverName(PolicyName)

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

// AllocateClaim picks CPUs for each result of a claim, as many as it
// consumed.
func (p *policy) AllocateClaim(
	claim *resourceapi.ResourceClaim,
	results []resourceapi.DeviceRequestAllocationResult,
) ([]specs.ContainerEdits, error) {
	counts, err := consumedCPUs(results)
	if err != nil {
		return nil, fmt.Errorf("claim %s: %w", claim.UID, err)
	}

	uid := string(claim.UID)
	cpus, ok := p.claims[uid]
	if !ok {
		cpus, err = p.pickCPUs(counts)
		if err != nil {
			return nil, fmt.Errorf("claim %s: %w", claim.UID, err)
		}
		p.claims[uid] = cpus
		p.cache.SetPolicyEntry(keyClaims, p.claims)
	}

	if len(cpus) != len(counts) {
		return nil, fmt.Errorf("claim %s holds %d results but has %d", claim.UID, len(cpus), len(counts))
	}
	for i, n := range counts {
		if cpus[i].Size() != n {
			return nil, fmt.Errorf("claim %s holds %d CPUs for result %d but it consumed %d",
				claim.UID, cpus[i].Size(), i, n)
		}
	}

	// Containers find their CPUs in the resources we export to them.
	return nil, nil
}

// containerClaims returns the CPUs of the claims a container holds: the CPUs
// of the allocation results it holds of each claim.
func (p *policy) containerClaims(c cache.Container) cpuset.CPUSet {
	cpus := cpuset.New()
	for uid, indexes := range dra.ContainerClaims(c, draDriverName) {
		results := p.claims[string(uid)]
		for _, i := range indexes {
			if i < len(results) {
				cpus = cpus.Union(results[i])
			}
		}
	}
	return cpus
}

// ReleaseClaim gives the CPUs of a claim back.
func (p *policy) ReleaseClaim(uid types.UID) error {
	delete(p.claims, string(uid))
	p.cache.SetPolicyEntry(keyClaims, p.claims)
	return nil
}

// restoreClaims loads the claimed CPUs saved before a restart. A claim keeps
// its CPUs even if we are not allowed some of them any more: its containers
// were told them when they were created, so it cannot move, and until it is
// released no other claim may get them. We only warn about it.
func (p *policy) restoreClaims() {
	p.claims = map[string][]cpuset.CPUSet{}
	p.cache.GetPolicyEntry(keyClaims, &p.claims)

	allowed, err := p.allowedCPUs()
	if err != nil {
		return
	}
	for uid, results := range p.claims {
		cpus := cpuset.New().Union(results...)
		if !cpus.IsSubsetOf(allowed) {
			log.Warnf("claim %s keeps CPUs %s, which are not allowed any more",
				uid, cpus.Difference(allowed))
		}
	}
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

// pickCPUs picks CPUs no claim holds, counts[i] of them for result i.
func (p *policy) pickCPUs(counts []int) ([]cpuset.CPUSet, error) {
	free, err := p.allowedCPUs()
	if err != nil {
		return nil, err
	}
	for _, results := range p.claims {
		free = free.Difference(cpuset.New().Union(results...))
	}

	total := 0
	for _, n := range counts {
		total += n
	}
	if free.Size() < total {
		return nil, fmt.Errorf("%d CPUs requested, only %d free", total, free.Size())
	}

	// Take the lowest IDs. A policy which knows the topology picks by it.
	ids := free.List()
	cpus := make([]cpuset.CPUSet, 0, len(counts))
	for _, n := range counts {
		cpus = append(cpus, cpuset.New(ids[:n]...))
		ids = ids[n:]
	}
	return cpus, nil
}

// consumedCPUs returns how many CPUs each result consumed.
func consumedCPUs(results []resourceapi.DeviceRequestAllocationResult) ([]int, error) {
	counts := make([]int, 0, len(results))
	total := 0
	for i, r := range results {
		q, ok := r.ConsumedCapacity[draCapacity]
		if !ok {
			return nil, fmt.Errorf("result %d consumed no %s", i, draCapacity)
		}
		n, ok := q.AsInt64()
		if !ok || n < 1 {
			return nil, fmt.Errorf("result %d consumed %s %s, not a positive whole number",
				i, q.String(), draCapacity)
		}
		if n > int64(math.MaxInt-total) {
			return nil, fmt.Errorf("results consumed too many %s", draCapacity)
		}
		total += int(n)
		counts = append(counts, int(n))
	}
	return counts, nil
}
