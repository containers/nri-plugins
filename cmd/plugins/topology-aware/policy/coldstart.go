// Copyright 2019-2020 Intel Corporation. All Rights Reserved.
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
	"time"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
)

// trigger cold start for the container if necessary.
func (p *policy) triggerColdStart(c cache.Container) error {
	log.Infof("coldstart: triggering coldstart for %s...", c.PrettyName())
	g, ok := p.allocations.getGrant(c.GetID())
	if !ok {
		log.Warnf("coldstart: no grant found, nothing to do...")
		return nil
	}

	coldStart := g.ColdStart()
	if coldStart <= 0 {
		log.Infof("coldstart: no coldstart, nothing to do...")
		return nil
	}

	// Start a timer to restore the grant memset to full. Store the
	// timer so that we can release it if the grant is destroyed before
	// the timer elapses.
	duration := coldStart
	timer := time.AfterFunc(duration, func() {
		owner := p.options.Owner
		owner.Lock()
		defer owner.Unlock()

		if err := p.finishColdStart(c); err != nil {
			log.Errorf("%v", err)
			return
		}
		if err := owner.UpdateContainers(); err != nil {
			log.Errorf("coldstart: failed to update containers: %v", err)
		}
	})
	g.AddTimer(timer)
	return nil
}

// finish an ongoing coldstart for the container.
func (p *policy) finishColdStart(c cache.Container) error {
	g, ok := p.allocations.getGrant(c.GetID())
	if !ok {
		return policyError("coldstart: no grant found for %s", c.PrettyName())
	}

	log.Infof("reallocating %s after coldstart", g)
	err := g.ReallocMemory(p.memZoneType(g.GetMemoryZone()) | libmem.TypeMaskDRAM)
	if err != nil {
		log.Errorf("failed to reallocate %s after coldstart: %v", g, err)
	} else {
		log.Infof("reallocated %s", g)
		p.saveAllocations()
	}
	g.ClearTimer()

	return nil
}
