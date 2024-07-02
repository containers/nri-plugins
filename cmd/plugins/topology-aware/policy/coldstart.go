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
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
)

// trigger cold start for the container if necessary.
func (p *policy) triggerColdStart(c cache.Container) error {
	log.Info("coldstart: triggering coldstart for %s...", c.PrettyName())
	g, ok := p.allocations.grants[c.GetID()]
	if !ok {
		log.Warn("coldstart: no grant found, nothing to do...")
		return nil
	}

	coldStart := g.ColdStart()
	if coldStart <= 0 {
		log.Info("coldstart: no coldstart, nothing to do...")
		return nil
	}

	// Start a timer to restore the grant memset to full. Store the
	// timer so that we can release it if the grant is destroyed before
	// the timer elapses.
	duration := coldStart
	timer := time.AfterFunc(duration, func() {
		e := &events.Policy{
			Type:   ColdStartDone,
			Source: PolicyName,
			Data:   c.GetID(),
		}
		if err := p.options.SendEvent(e); err != nil {
			// we should retry this later, the channel is probably full...
			log.Error("Ouch... we'should retry this later.")
		}
	})
	g.AddTimer(timer)
	return nil
}

// finish an ongoing coldstart for the container.
func (p *policy) finishColdStart(c cache.Container) (bool, error) {
	g, ok := p.allocations.grants[c.GetID()]
	if !ok {
		log.Warn("coldstart: no grant found, nothing to do...")
		return false, policyError("coldstart: no grant found for %s", c.PrettyName())
	}

	log.Info("reallocating %s after coldstart", g)
	err := g.ReallocMemory(p.memZoneType(g.GetMemoryZone()) | libmem.TypeMaskDRAM)
	if err != nil {
		log.Error("failed to reallocate %s after coldstart: %v", g, err)
	} else {
		log.Info("reallocated %s", g)
	}
	g.ClearTimer()

	return true, nil
}
