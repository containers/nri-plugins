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

package metrics

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	model "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	logger "github.com/intel/nri-resmgr/pkg/log"

	"github.com/intel/nri-resmgr/pkg/instrumentation"
	"github.com/intel/nri-resmgr/pkg/metrics"
	"github.com/intel/nri-resmgr/pkg/resmgr/events"

	// pull in all metrics collectors
	_ "github.com/intel/nri-resmgr/pkg/metrics/register"
)

// Options describes options for metrics collection and processing.
type Options struct {
	// PollInterval is the interval for polling raw metrics.
	PollInterval time.Duration
	// Events is the channel for delivering metrics events.
	Events chan interface{}
}

// Metrics implements collecting, caching and processing of raw metrics.
type Metrics struct {
	sync.RWMutex
	opts Options               // metrics collecting options
	g    prometheus.Gatherer   // prometheus/raw metrics gatherer
	stop chan interface{}      // channel to stop polling goroutine
	raw  []*model.MetricFamily // latest set of raw metrics
	pend []*model.MetricFamily // pending metrics for forwarding
}

// Our logger instance.
var log = logger.NewLogger("metrics")

// NewMetrics creates a new instance for metrics collecting and processing.
func NewMetrics(opts Options) (*Metrics, error) {
	if opts.Events == nil {
		return nil, metricsError("invalid options, nil Event channel")
	}

	g, err := metrics.NewMetricGatherer()
	if err != nil {
		return nil, metricsError("failed to create raw metrics gatherer: %v", err)
	}

	m := &Metrics{
		opts: opts,
		raw:  make([]*model.MetricFamily, 0),
		g:    g,
	}

	m.poll()
	instrumentation.RegisterGatherer(m)

	return m, nil
}

// Start starts metrics collection and processing.
func (m *Metrics) Start() error {
	if m.stop != nil {
		return nil
	}

	stop := make(chan interface{})
	go func() {
		var pollTimer *time.Ticker
		var pollChan <-chan time.Time

		if m.opts.PollInterval > 0 {
			pollTimer = time.NewTicker(m.opts.PollInterval)
			pollChan = pollTimer.C
		} else {
			log.Info("periodic collection of metrics is disabled")
		}

		for {
			select {
			case _ = <-stop:
				if pollTimer != nil {
					pollTimer.Stop()
				}
				return
			case _ = <-pollChan:
				if err := m.poll(); err != nil {
					log.Error("failed to poll raw metrics: %v", err)
				}
			}
		}
	}()
	m.stop = stop

	return nil
}

// Stop stops metrics collection and processing.
func (m *Metrics) Stop() {
	if m.stop != nil {
		close(m.stop)
		m.stop = nil
	}
}

// poll does a single round of raw metrics collection.
func (m *Metrics) poll() error {
	m.Lock()
	defer m.Unlock()

	f, err := m.g.Gather()
	if err != nil {
		return metricsError("failed to poll raw metrics: %v", err)
	}
	m.raw = f
	m.pend = f
	return nil
}

// sendEvent sends a metrics-based event for processing.
func (m *Metrics) sendEvent(e *events.Metrics) error {
	select {
	case m.opts.Events <- e:
		return nil
	default:
		return metricsError("failed to deliver event %v (channel full?)", *e)
	}
}

// dump debug-dumps the given MetricFamily data
func dump(prefix string, f *model.MetricFamily) {
	if !log.DebugEnabled() {
		return
	}
	buf := &bytes.Buffer{}
	if _, err := expfmt.MetricFamilyToText(buf, f); err != nil {
		return
	}
	log.DebugBlock("  <"+prefix+"> ", "%s", strings.TrimSpace(buf.String()))
}

// metricsError returns a new formatted error specific to metrics-processing.
func metricsError(format string, args ...interface{}) error {
	return fmt.Errorf("metrics: "+format, args...)
}
