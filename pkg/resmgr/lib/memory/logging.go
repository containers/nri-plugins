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

package libmem

import (
	"slices"
	"strings"

	logger "github.com/containers/nri-plugins/pkg/log"
)

var (
	log = logger.Get("libmem")
)

func (a *Allocator) DumpConfig() {
	log.Info("memory allocator configuration")
	for id := range a.masks.nodes.all.Slice() {
		n := a.nodes[id]
		log.Info("  %s node #%d with %s memory", n.memType, n.id, prettySize(n.capacity))
		log.Info("    distance vector %v", n.distance.vector)
		n.ForeachDistance(func(d int, nodes NodeMask) bool {
			log.Info("      at distance %d: %s", d, nodes)
			return true
		})
	}
}

func (a *Allocator) DumpZones() {
	zones := []*Zone{}
	for _, z := range a.zones {
		zones = append(zones, z)
	}
	slices.SortFunc(zones, func(a, b *Zone) int {
		if a.nodes.Size() < b.nodes.Size() {
			return -1
		}
		if b.nodes.Size() < a.nodes.Size() {
			return 1
		}
		return int(a.nodes - b.nodes)
	})

	for _, z := range zones {
		capa := prettySize(a.zoneCapacity(z.nodes))
		use := prettySize(a.zoneUsage(z.nodes))
		free := prettySize(a.zoneFree(z.nodes))
		log.Info("%s (%s): capacity %s, usage: %s, free: %s", z.nodes, z.types, capa, use, free)
		for _, req := range z.users {
			log.Info("  + %s", req)
		}
	}
}

func (a *Allocator) DumpRequests() {
	ids := []string{}
	for id := range a.requests {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		return strings.Compare(a, b)
	})

	for _, id := range ids {
		req := a.requests[id]
		log.Info("  - %s", req)
	}
}
