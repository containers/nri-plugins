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
	"fmt"
	"slices"

	logger "github.com/containers/nri-plugins/pkg/log"
)

var (
	log     = logger.Get("libmem")
	details = logger.Get("libmem-details")
)

func (a *Allocator) DumpNodes(context ...interface{}) {
	prefix := formatPrefix(context...)
	a.ForeachNode(a.masks.AvailableNodes(), func(n *Node) bool {
		var (
			capa = n.Capacity()
			cpus = n.CloseCPUs().String()
		)

		if capa != 0 {
			log.Infof("%s  %s node #%d with %d memory (%s)", prefix,
				n.Type(), n.ID(), capa, prettySize(capa))
		} else {
			log.Infof("%s  memoryless %s node #%d", prefix,
				n.Type(), n.ID())
		}

		log.Infof("%s    distance vector %v", prefix, n.Distance().Vector())
		n.ForeachDistance(func(d int, nodes NodeMask) bool {
			log.Infof("%s      at distance %d: %s %s", prefix, d, a.zoneType(nodes), nodes)
			return true
		})

		if cpus != "" {
			log.Infof("%s    close CPUs: %s", prefix, cpus)
		} else {
			log.Infof("%s    no close CPUs", prefix)
		}

		return true
	})
}

func (a *Allocator) DumpConfig(context ...interface{}) {
	prefix := formatPrefix(context...)
	log.Infof("%smemory allocator configuration", prefix)
	a.DumpNodes(prefix)
}

func (a *Allocator) DumpState(context ...interface{}) {
	prefix := formatPrefix(context...)
	a.DumpRequests(prefix)
	a.DumpZones(prefix)
}

func (a *Allocator) DumpRequests(context ...interface{}) {
	if !details.DebugEnabled() {
		return
	}

	prefix := formatPrefix(context...)

	if len(a.users) == 0 {
		details.Debugf("%s  no requests", prefix)
		return
	}

	details.Debugf("%s  requests:", prefix)
	for _, req := range SortRequests(a.requests, nil, RequestsByAge) {
		details.Debugf("%s    - %s (assigned zone %s)", prefix, req, req.Zone())
	}
}

func (a *Allocator) DumpZones(prefixFmt ...interface{}) {
	if !details.DebugEnabled() {
		return
	}

	prefix := formatPrefix(prefixFmt...)

	if len(a.zones) == 0 {
		details.Debugf("%s  no zones in use", prefix)
		return
	}

	zones := make([]NodeMask, 0, len(a.zones))
	for z := range a.zones {
		zones = append(zones, z)
	}
	slices.SortFunc(zones, func(z1, z2 NodeMask) int {
		if diff := z1.Size() - z2.Size(); diff != 0 {
			return diff
		}
		return int(z1 - z2)
	})

	details.Debugf("%s  zones:", prefix)
	for _, z := range zones {
		var (
			zone = a.zones[z]
			free = prettySize(a.ZoneFree(z))
			capa = prettySize(a.ZoneCapacity(z))
			used = prettySize(a.ZoneUsage(z))
		)
		details.Debugf("%s   - zone %s, free %s (capacity %s, used %s)", prefix, z, free, capa, used)
		if len(zone.users) == 0 {
			continue
		}

		for _, req := range SortRequests(zone.users, nil, RequestsByAge) {
			details.Debugf("%s      %s", prefix, req)
		}
	}
}

func (a *Allocator) dumpOvercommit(where string, oc []NodeMask, spill map[NodeMask]int64) {
	if !log.DebugEnabled() {
		return
	}

	log.Debugf("%s", where)
	for _, z := range oc {
		log.Debugf("  %s: %s", zoneName(z), prettySize(spill[z]))
		for _, r := range a.zones[z].users {
			log.Debugf("    - user %s", r)
		}
	}
}

func formatPrefix(args ...interface{}) string {
	narg := len(args)
	if narg == 0 {
		return ""
	}

	format, ok := args[0].(string)
	if !ok {
		return "%%(!libmem:Bad-Prefix)"
	}

	if len(args) == 1 {
		return format
	}

	return fmt.Sprintf(format, args[1:]...)
}
