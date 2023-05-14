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

package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"contrib.go.opencensus.io/exporter/prometheus"
	promcli "github.com/prometheus/client_golang/prometheus"
	model "github.com/prometheus/client_model/go"
	"go.opencensus.io/stats/view"

	"github.com/containers/nri-plugins/pkg/http"
	logger "github.com/containers/nri-plugins/pkg/log"
)

// Option represents an option which can be applied to metrics.
type Option func(*metrics) error

type metrics struct {
	disabled bool
	service  string
	period   time.Duration
	exporter *prometheus.Exporter
	mux      *http.ServeMux
}

var (
	log = logger.Get("metrics")
	mtr = &metrics{
		service: filepath.Base(os.Args[0]),
	}
)

const (
	// HTTP path our mux serves metrics on.
	httpMetricsPath = "/metrics"
)

// WithExporterDisabled can be used to disable the metrics exporter.
func WithExporterDisabled(disabled bool) Option {
	return func(m *metrics) error {
		m.disabled = disabled
		return nil
	}
}

// WithPeriod sets the internal metrics collection period.
func WithPeriod(period time.Duration) Option {
	return func(m *metrics) error {
		m.period = period
		return nil
	}
}

// WithServiceName sets the service name reported for metrics.
func WithServiceName(name string) Option {
	return func(m *metrics) error {
		m.service = name
		return nil
	}
}

// Start metrics exporter.
func Start(mux *http.ServeMux, options ...Option) error {
	return mtr.start(mux, options...)
}

// Stop metrics exporter.
func Stop() {
	mtr.shutdown()
}

func (m *metrics) start(mux *http.ServeMux, options ...Option) error {
	m.shutdown()

	for _, opt := range options {
		if err := opt(m); err != nil {
			return fmt.Errorf("failed to set metrics option: %w", err)
		}
	}

	if m.disabled {
		log.Info("metrics exporter disabled")
		return nil
	}

	log.Info("starting metrics exporter...")

	exporter, err := prometheus.NewExporter(
		prometheus.Options{
			Namespace: strings.ReplaceAll(strings.ToLower(m.service), "-", "_"),
			Gatherer:  promcli.Gatherers{registeredGatherers},
			OnError:   func(err error) { log.Error("prometheus export error: %v", err) },
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create prometheus exporter: %w", err)
	}

	m.mux = mux
	m.exporter = exporter

	m.mux.Handle(httpMetricsPath, m.exporter)
	view.RegisterExporter(m.exporter)
	view.SetReportingPeriod(m.period)

	return nil
}

func (m *metrics) shutdown() {
	if m.exporter == nil {
		return
	}

	view.UnregisterExporter(m.exporter)
	m.mux.Unregister(httpMetricsPath)

	m.exporter = nil
	m.mux = nil
}

// Our registered prometheus gatherers.
var (
	registeredGatherers = &gatherers{gatherers: promcli.Gatherers{}}
)

type gatherers struct {
	sync.RWMutex
	gatherers promcli.Gatherers
}

func (g *gatherers) register(gatherer promcli.Gatherer) {
	g.Lock()
	defer g.Unlock()
	g.gatherers = append(g.gatherers, gatherer)
}

func (g *gatherers) Gather() ([]*model.MetricFamily, error) {
	g.RLock()
	defer g.RUnlock()
	return g.gatherers.Gather()
}

func RegisterGatherer(g promcli.Gatherer) {
	registeredGatherers.register(g)
}
