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

package udev

import (
	"fmt"
	"path"
)

// MonitorOption is an opaque option which can be applied to a Monitor.
type MonitorOption func(*Monitor)

// WithFilters returns a MonitorOption for filtering events by properties.
// Properties within a map have AND semantics: the map matches an event if
// all key-value pairs match the event. Multiple maps have OR semantics:
// they match an event if at least one map matches the event. Events which
// are matched are passed through. Others are filtered out.
func WithFilters(filters ...map[string]string) MonitorOption {
	return func(m *Monitor) {
		m.filters = append(m.filters, filters...)
	}
}

// WithGlobFilters returns a MonitorOption for filtering events by properties.
// Semantics are similar to WithFilters, but properties are matched using glob
// patterns instead of verbatim comparison.
func WithGlobFilters(globbers ...map[string]string) MonitorOption {
	return func(m *Monitor) {
		m.globbers = append(m.globbers, globbers...)
	}
}

// Monitor monitors udev events.
type Monitor struct {
	r        *EventReader
	filters  []map[string]string
	globbers []map[string]string
}

// NewMonitor creates an udev monitor with the given options.
func NewMonitor(options ...MonitorOption) (*Monitor, error) {
	r, err := NewEventReader()
	if err != nil {
		return nil, fmt.Errorf("failed to create udev monitor reader: %w", err)
	}

	m := &Monitor{
		r: r,
	}

	for _, o := range options {
		o(m)
	}

	return m, nil
}

// Start starts event monitoring and delivery.
func (m *Monitor) Start(events chan *Event) {
	if len(m.filters) == 0 && len(m.globbers) == 0 {
		go m.reader(events)
	} else {
		unfiltered := make(chan *Event, 64)
		go m.filterer(unfiltered, events)
		go m.reader(unfiltered)
	}
}

// Stop stops event monitoring.
func (m *Monitor) Stop() error {
	return m.r.Close()
}

func (m *Monitor) reader(events chan<- *Event) {
	for {
		evt, err := m.r.Read()
		if err != nil {
			log.Errorf("failed to read udev event: %v", err)
			m.r.Close()
			close(events)
			return
		}

		events <- evt
	}
}

func (m *Monitor) filterer(unfiltered <-chan *Event, filtered chan<- *Event) {
	var stuck bool

	for evt := range unfiltered {
		if !m.filter(evt) {
			continue
		}

		select {
		case filtered <- evt:
			if stuck {
				log.Warnf("receiver reading again, delivering udev events (%s %s)...",
					evt.Subsystem, evt.Action)
				stuck = false
			}
		default:
			if !stuck {
				log.Warnf("receiver stuck, dropping udev events (%s %s)...",
					evt.Subsystem, evt.Action)
				stuck = true
			}
		}
	}
}

func (m *Monitor) filter(evt *Event) bool {
	if len(m.filters) == 0 && len(m.globbers) == 0 {
		return true
	}

	for _, filter := range m.filters {
		match := true
		for k, v := range filter {
			if evt.Properties[k] != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}

	for _, glob := range m.globbers {
		match := true
		for k, p := range glob {
			m, err := path.Match(p, evt.Properties[k])
			if err != nil {
				log.Errorf("failed to match udev event property %q=%q by pattern %q: %v",
					k, evt.Properties[k], p, err)
				delete(glob, k)
				continue
			}
			if !m {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}

	return false
}
