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

import libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"

func (p *policy) getMemOffer(pool Node, req Request) (*libmem.Offer, error) {
	var (
		ctr  = req.GetContainer()
		zone = libmem.NodeMask(0)
		mtyp = libmem.TypeMask(0)
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

func (p *policy) restoreMemOffer(g Grant) (*libmem.Offer, error) {
	var (
		ctr  = g.GetContainer()
		zone = g.GetMemoryZone()
		mtyp = p.memAllocator.ZoneType(zone)
	)

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

func (p *policy) poolZoneType(pool Node, memType memoryType) libmem.TypeMask {
	return p.memAllocator.ZoneType(libmem.NewNodeMask(pool.GetMemset(memType).Members()...))
}

func (p *policy) memZoneType(zone libmem.NodeMask) libmem.TypeMask {
	return p.memAllocator.ZoneType(zone)
}

func (p *policy) poolZone(pool Node, memType memoryType) libmem.NodeMask {
	return libmem.NewNodeMask(pool.GetMemset(memType).Members()...)
}

func (p *policy) poolZoneCapacity(pool Node, memType memoryType) int64 {
	return p.memAllocator.ZoneCapacity(libmem.NewNodeMask(pool.GetMemset(memType).Members()...))
}

func (p *policy) poolZoneFree(pool Node, memType memoryType) int64 {
	return p.memAllocator.ZoneFree(libmem.NewNodeMask(pool.GetMemset(memType).Members()...))
}
