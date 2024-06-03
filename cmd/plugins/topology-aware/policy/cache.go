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
	"time"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

const (
	keyAllocations = "allocations"
	keyConfig      = "config"
)

func (p *policy) saveAllocations() {
	p.cache.SetPolicyEntry(keyAllocations, cache.Cachable(&p.allocations))
	p.cache.Save()
}

func (p *policy) restoreAllocations(allocations *allocations) error {
	savedAllocations := allocations.clone()
	p.allocations = p.newAllocations()

	//
	// Try to reinstate all grants with the exact same resource assignments
	// as saved. If that fails, release and try to reallocate all corresponding
	// containers with pool hints pointing to the currently assigned pools. If
	// this fails too, save the original allocations unchanged to the cache and
	// return an error.
	//

	if err := p.reinstateGrants(allocations.grants); err != nil {
		log.Error("failed to reinstate grants verbatim: %v", err)
		containers, poolHints := allocations.getContainerPoolHints()
		if err := p.reallocateResources(containers, poolHints); err != nil {
			p.allocations = savedAllocations
			p.saveAllocations() // undo any potential changes in saved cache
			return err
		}
	}

	return nil
}

// reinstateGrants tries to restore the given grants exactly as such.
func (p *policy) reinstateGrants(grants map[string]Grant) error {
	for id, grant := range grants {
		c := grant.GetContainer()

		pool := grant.GetCPUNode()
		supply := pool.FreeSupply()

		o, err := p.restoreMemOffer(grant)
		if err != nil {
			return policyError("failed to get libmem offer for pool %q, grant of %s: %w",
				pool.Name(), c.PrettyName(), err)
		}

		updates, err := supply.Reserve(grant, o)
		if err != nil {
			return policyError("failed to update pool %q with CPU grant of %q: %v",
				pool.Name(), c.PrettyName(), err)
		}

		for uID, uZone := range updates {
			if ug, ok := p.allocations.grants[uID]; !ok {
				log.Error("failed to update grant %s to memory zone to %s, grant not found",
					uID, uZone)
			} else {
				ug.SetMemoryZone(uZone)
				if opt.PinMemory {
					ug.GetContainer().SetCpusetMems(uZone.MemsetString())
				}
				log.Info("updated grant %s to memory zone %s", uID, uZone)
			}
		}

		log.Info("updated pool %q with reinstated CPU grant of %q, memory zone %s",
			pool.Name(), c.PrettyName(), grant.GetMemoryZone())

		p.allocations.grants[id] = grant
		p.applyGrant(grant)
	}

	p.updateSharedAllocations(nil)

	return nil
}

type cachedGrant struct {
	Exclusive  string
	Part       int
	CPUType    cpuClass
	Container  string
	Pool       string
	MemoryPool libmem.NodeMask
	MemType    memoryType
	MemSize    int64
	ColdStart  time.Duration
}

func newCachedGrant(cg Grant) *cachedGrant {
	ccg := &cachedGrant{}
	ccg.Exclusive = cg.ExclusiveCPUs().String()
	ccg.Part = cg.CPUPortion()
	ccg.CPUType = cg.CPUType()
	ccg.Container = cg.GetContainer().GetID()
	ccg.Pool = cg.GetCPUNode().Name()
	ccg.MemoryPool = cg.GetMemoryZone()
	ccg.MemType = cg.MemoryType()
	ccg.MemSize = cg.GetMemorySize()
	ccg.ColdStart = cg.ColdStart()

	return ccg
}

func (ccg *cachedGrant) ToGrant(policy *policy) (Grant, error) {
	node, ok := policy.nodes[ccg.Pool]
	if !ok {
		return nil, policyError("cache error: failed to restore %v, unknown pool/node", *ccg)
	}
	container, ok := policy.cache.LookupContainer(ccg.Container)
	if !ok {
		return nil, policyError("cache error: failed to restore %v, unknown container", *ccg)
	}

	g := newGrant(
		node,
		container,
		ccg.CPUType,
		cpuset.MustParse(ccg.Exclusive),
		ccg.Part,
		ccg.MemType,
		ccg.ColdStart,
	)

	g.SetMemoryZone(ccg.MemoryPool)
	g.SetMemorySize(ccg.MemSize)

	return g, nil
}

func (cg *grant) MarshalJSON() ([]byte, error) {
	return json.Marshal(newCachedGrant(cg))
}

func (cg *grant) UnmarshalJSON(data []byte) error {
	ccg := cachedGrant{}

	if err := json.Unmarshal(data, &ccg); err != nil {
		return policyError("failed to restore grant: %v", err)
	}

	cg.exclusive = cpuset.MustParse(ccg.Exclusive)

	return nil
}

func (a *allocations) MarshalJSON() ([]byte, error) {
	cgrants := make(map[string]*cachedGrant)
	for id, cg := range a.grants {
		cgrants[id] = newCachedGrant(cg)
	}

	return json.Marshal(cgrants)
}

func (a *allocations) UnmarshalJSON(data []byte) error {
	var err error

	cgrants := make(map[string]*cachedGrant)
	if err := json.Unmarshal(data, &cgrants); err != nil {
		return policyError("failed to restore allocations: %v", err)
	}

	a.grants = make(map[string]Grant, 32)
	for id, ccg := range cgrants {
		a.grants[id], err = ccg.ToGrant(a.policy)
		if err != nil {
			log.Error("removing unresolvable cached grant %v: %v", *ccg, err)
			delete(a.grants, id)
		} else {
			log.Debug("resolved cache grant: %v", a.grants[id].String())
		}
	}

	return nil
}

func (a *allocations) Get() interface{} {
	return a
}

func (a *allocations) Set(value interface{}) {
	var from *allocations

	switch value.(type) {
	case allocations:
		v := value.(allocations)
		from = &v
	case *allocations:
		from = value.(*allocations)
	}

	a.grants = make(map[string]Grant, 32)
	for id, cg := range from.grants {
		a.grants[id] = cg
	}
}

func (a *allocations) Dump(logfn func(format string, args ...interface{}), prefix string) {
	for _, cg := range a.grants {
		logfn(prefix+"%s", cg)
	}
}
