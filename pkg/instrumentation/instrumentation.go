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

package instrumentation

import (
	"fmt"
	"sync"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/instrumentation"
	"github.com/containers/nri-plugins/pkg/http"
	"github.com/containers/nri-plugins/pkg/instrumentation/metrics"
	"github.com/containers/nri-plugins/pkg/instrumentation/tracing"
	logger "github.com/containers/nri-plugins/pkg/log"
)

const (
	// ServiceName is our service name in external tracing and metrics services.
	ServiceName = "nri-resource-policy"
)

// KeyValue aliases tracing.KeyValue, for SetIdentity().
type KeyValue = tracing.KeyValue

var (
	// Our runtime configuration.
	cfg = &cfgapi.Config{}
	// Lock to protect against reconfiguration.
	lock sync.RWMutex
	// Our HTTP server instance.
	srv = http.NewServer()
	// Our logger instance.
	log = logger.NewLogger("instrumentation")

	// Our identity for instrumentation.
	identity []KeyValue

	// Attribute aliases tracing.Attribute(), for SetIdentity().
	Attribute = tracing.Attribute
)

// HTTPServer returns our HTTP server.
func HTTPServer() *http.Server {
	return srv
}

// SetIdentity sets (extra) process identity attributes for tracing.
func SetIdentity(attrs ...KeyValue) {
	identity = attrs
}

// Start our instrumentation services.
func Start() error {
	log.Info("starting instrumentation services...")

	lock.Lock()
	defer lock.Unlock()

	return start()
}

// Stop our instrumentation services.
func Stop() {
	lock.Lock()
	defer lock.Unlock()

	stop()
}

// Restart our instrumentation services.
func Restart() error {
	lock.Lock()
	defer lock.Unlock()

	stop()

	err := start()
	if err != nil {
		log.Error("failed to start instrumentation: %v", err)
	}

	return err
}

// Reconfigure our instrumentation services.
func Reconfigure(newCfg *cfgapi.Config) error {
	cfg = newCfg
	return Restart()
}

func start() error {
	if err := srv.Start(cfg.HTTPEndpoint); err != nil {
		return fmt.Errorf("failed to start HTTP server: %v", err)
	}

	resource, err := GetResource()
	if err != nil {
		return err
	}

	if err := tracing.Start(
		resource,
		tracing.WithCollectorEndpoint(cfg.TracingCollector),
		tracing.WithSamplingRatio(float64(cfg.SamplingRatePerMillion)/float64(1000000)),
	); err != nil {
		return fmt.Errorf("failed to start tracing: %v", err)
	}

	if cfg.PrometheusExport {
		if cfg.MetricsExporter != "" && cfg.MetricsExporter != "prometheus" {
			return fmt.Errorf("conflicting metrics exporters: '%s' and 'metricsExporter: %q'",
				"prometheusExport: true", cfg.MetricsExporter)
		}
		cfg.MetricsExporter = "prometheus"
	}

	if err := metrics.Start(
		srv.GetMux(),
		resource,
		metrics.WithNamespace("nri"),
		metrics.WithExporter(cfg.MetricsExporter),
		metrics.WithReportPeriod(cfg.ReportPeriod.Duration),
		metrics.WithMetrics(cfg.Metrics),
	); err != nil {
		return fmt.Errorf("failed to start metrics: %v", err)
	}

	return nil
}

func stop() {
	metrics.Stop()
	tracing.Stop()
	srv.Stop()
}
