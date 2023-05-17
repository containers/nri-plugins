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

	promcli "github.com/prometheus/client_golang/prometheus"

	"github.com/containers/nri-plugins/pkg/http"
	"github.com/containers/nri-plugins/pkg/instrumentation/metrics"
	"github.com/containers/nri-plugins/pkg/instrumentation/tracing"
	logger "github.com/containers/nri-plugins/pkg/log"
)

type KeyValue = tracing.KeyValue

const (
	// ServiceName is our service name in external tracing and metrics services.
	ServiceName = "nri-resource-policy"
)

var (
	// Our logger instance.
	log = logger.NewLogger("instrumentation")
	// Our HTTP server instance.
	srv = http.NewServer()
	// Lock to protect against reconfiguration.
	lock sync.RWMutex
	// Our identity for instrumentation.
	identity []tracing.KeyValue

	// Attribute allows setting up identity without an import of tracing.
	Attribute = tracing.Attribute
)

// RegisterGatherer registers a prometheus metrics gatherer.
func RegisterGatherer(g promcli.Gatherer) {
	metrics.RegisterGatherer(g)
}

// GetHTTPMux returns our HTTP request mux for external services.
func GetHTTPMux() *http.ServeMux {
	return srv.GetMux()
}

// SetIdentity sets (extra) process identity attributes for tracing.
func SetIdentity(attrs ...KeyValue) {
	identity = attrs
}

// Start our internal instrumentation services.
func Start() error {
	log.Info("starting instrumentation services...")

	lock.Lock()
	defer lock.Unlock()

	return start()
}

// Stop our internal instrumentation services.
func Stop() {
	lock.Lock()
	defer lock.Unlock()

	stop()
}

// Restart our internal instrumentation services.
func Restart() error {
	lock.Lock()
	defer lock.Unlock()

	stop()
	return start()
}

func start() error {
	err := startHTTPServer()
	if err != nil {
		return instrumentationError("failed to start HTTP server: %v", err)
	}

	err = startTracing()
	if err != nil {
		return instrumentationError("failed to start tracing exporter: %v", err)
	}

	err = startMetrics()
	if err != nil {
		return instrumentationError("failed to start metrics exporter: %v", err)
	}

	return nil
}

func startHTTPServer() error {
	if srv == nil {
		return nil
	}
	return srv.Start(opt.HTTPEndpoint)
}

func startTracing() error {
	return tracing.Start(
		tracing.WithServiceName(ServiceName),
		tracing.WithIdentity(identity...),
		tracing.WithCollectorEndpoint(opt.TracingCollector),
		tracing.WithSamplingRatio(opt.Sampling.Ratio()),
	)
}

func startMetrics() error {
	return metrics.Start(
		GetHTTPMux(),
		metrics.WithExporterDisabled(!opt.PrometheusExport),
		metrics.WithServiceName(ServiceName),
		metrics.WithPeriod(opt.ReportPeriod),
	)
}

func stop() {
	stopMetrics()
	stopTracing()
	stopHTTPServer()
}

func stopHTTPServer() {
	if srv != nil {
		srv.Stop()
	}
}

func stopTracing() {
	tracing.Stop()
}

func stopMetrics() {
	metrics.Stop()
}

func reconfigure() error {
	lock.Lock()
	defer lock.Unlock()

	stop()
	return start()
}

// instrumentationError produces a formatted instrumentation-specific error.
func instrumentationError(format string, args ...interface{}) error {
	return fmt.Errorf("instrumentation: "+format, args...)
}
