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

package topologyaware

import (
	"fmt"

	"github.com/containers/nri-plugins/pkg/agent/podresapi"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
)

func (p *policy) getMemOffer(pool Node, req Request) (*libmem.Offer, error) {
	var (
		zone libmem.NodeMask
		mtyp libmem.TypeMask
		ctr  = req.GetContainer()
	)

	if memType := req.MemoryType(); memType == memoryPreserve {
		zone = libmem.NewNodeMask(pool.GetMemset(memoryAll).Members()...)
		mtyp = p.memAllocator.ZoneType(zone)
	} else {
		zone = libmem.NewNodeMask(pool.GetMemset(memType).Members()...)
		mtyp = libmem.TypeMask(memType)
	}

	o, err := p.memAllocator.GetOffer(
		libmem.ContainerWithTypes(
			ctr.GetID(),
			ctr.PrettyName(),
			string(ctr.GetQOSClass()),
			req.MemAmountToAllocate(),
			zone,
			mtyp,
		),
	)

	return o, err
}

func (p *policy) getMemOfferByHints(pool Node, req Request) (*libmem.Offer, error) {
	ctr := req.GetContainer()

	memType := req.MemoryType()
	if memType == memoryPreserve {
		return nil, fmt.Errorf("%s by hints: memoryPreserve requested", pool.Name())
	}

	zone := libmem.NodeMask(0)
	from := libmem.NewNodeMask(pool.GetMemset(memType).Members()...)
	mtyp := libmem.TypeMask(memType)

	for provider, hint := range ctr.GetTopologyHints() {
		if !podresapi.IsPodResourceHint(provider) {
			continue
		}

		m, err := libmem.ParseNodeMask(hint.NUMAs)
		if err != nil {
			return nil, err
		}

		if !from.And(m).Contains(m.Slice()...) {
			return nil, fmt.Errorf("%s by hints: %s of wrong type (%s)", pool.Name(), m, mtyp)
		}

		zone = zone.Set(m.Slice()...)
	}

	if zone == libmem.NodeMask(0) {
		return nil, fmt.Errorf("%s by hints: no pod resource API hints", pool.Name())
	}

	if zoneType := p.memAllocator.ZoneType(zone); zoneType != mtyp {
		return nil, fmt.Errorf("%s by hints: no type %s", pool.Name(), mtyp.Clear(zoneType.Slice()...))
	}

	o, err := p.memAllocator.GetOffer(
		libmem.ContainerWithTypes(
			ctr.GetID(),
			ctr.PrettyName(),
			string(ctr.GetQOSClass()),
			req.MemAmountToAllocate(),
			zone,
			mtyp,
		),
	)

	if err != nil {
		return nil, err
	}

	return o, nil
}

func (p *policy) restoreMemOffer(g Grant) (*libmem.Offer, error) {
	var (
		ctr  = g.GetContainer()
		zone = g.GetMemoryZone()
		mtyp = p.memAllocator.ZoneType(zone)
	)

	if _, ok := p.memAllocator.AssignedZone(ctr.GetID()); ok {
		if err := p.memAllocator.Release(ctr.GetID()); err != nil {
			return nil, err
		}
	}

	o, err := p.memAllocator.GetOffer(
		libmem.ContainerWithTypes(
			ctr.GetID(),
			ctr.PrettyName(),
			string(ctr.GetQOSClass()),
			g.GetMemorySize(),
			zone,
			mtyp,
		),
	)

	return o, err
}

func (p *policy) reallocMem(id string, nodes libmem.NodeMask, types libmem.TypeMask) (libmem.NodeMask, map[string]libmem.NodeMask, error) {
	return p.memAllocator.Realloc(id, nodes, types)
}

func (p *policy) releaseMem(id string) error {
	return p.memAllocator.Release(id)
}

func (p *policy) memZoneType(zone libmem.NodeMask) libmem.TypeMask {
	return p.memAllocator.ZoneType(zone)
}

func (p *policy) poolZoneCapacity(pool Node, memType memoryType) int64 {
	return p.memAllocator.ZoneCapacity(libmem.NewNodeMask(pool.GetMemset(memType).Members()...))
}

func (p *policy) poolZoneFree(pool Node, memType memoryType) int64 {
	return p.memAllocator.ZoneFree(libmem.NewNodeMask(pool.GetMemset(memType).Members()...))
}
