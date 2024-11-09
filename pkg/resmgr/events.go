// Copyright 2020 Intel Corporation. All Rights Reserved.
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

package resmgr

import (
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
)

// Our logger instance for events.
var evtlog = logger.NewLogger("events")

// setupEventProcessing sets up event and metrics processing.
func (m *resmgr) setupEventProcessing() error {
	m.events = make(chan interface{}, 8)
	m.stop = make(chan interface{})

	return nil
}

// startEventProcessing starts event and metrics processing.
func (m *resmgr) startEventProcessing() error {
	stop := m.stop
	go func() {
		for {
			select {
			case _ = <-stop:
				return
			case event := <-m.events:
				m.processEvent(event)
			}
			logger.Flush()
		}
	}()

	return nil
}

// stopEventProcessing stops event and metrics processing.
func (m *resmgr) stopEventProcessing() {
	if m.stop != nil {
		close(m.stop)
		m.stop = nil
	}
}

// SendEvent injects the given event to the resource manager's event processing loop.
func (m *resmgr) SendEvent(event interface{}) error {
	if m.events == nil {
		return resmgrError("can't send event, no event channel")
	}
	select {
	case m.events <- event:
		return nil
	default:
		return resmgrError("can't send event of type %T, event channel full", event)
	}
}

// processEvent processes the given event.
func (m *resmgr) processEvent(e interface{}) {
	evtlog.Debug("received event of type %T...", e)

	switch event := e.(type) {
	case string:
		evtlog.Debug("'%s'...", event)
		//case *events.Policy:
		//m.DeliverPolicyEvent(event)
	default:
		evtlog.Warn("event of unexpected type %T...", e)
	}
}

// resolveCgroupPath resolves a cgroup path to a container.
func (m *resmgr) resolveCgroupPath(path string) (cache.Container, bool) {
	return m.cache.LookupContainerByCgroup(path)
}
