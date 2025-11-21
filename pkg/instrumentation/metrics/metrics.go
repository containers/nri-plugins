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
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/containers/nri-plugins/pkg/http"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/metrics"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	config "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/metrics"
)

type (
	Option func() error
)

const (
	promExporter = "prometheus"
	httpExporter = "http"
	grpcExporter = "grpc"
)

var (
	namespace    = "nri"
	exporter     string
	provider     *metric.MeterProvider
	enabled      []string
	reportPeriod time.Duration
	mux          *http.ServeMux
	log          = logger.Get("metrics")
)

// WithExporter sets the type of metrics exporter to use.
func WithExporter(v string) Option {
	return func() error {
		if v != "" && exporter != "" && v != exporter {
			return fmt.Errorf("conflicting metrics exporter: %q and %q requested",
				exporter, v)
		}

		if v != "" {
			exporter = v
		}
		return nil
	}
}

// WithNamespace sets a common namespace (prefix) for all metrics.
func WithNamespace(v string) Option {
	return func() error {
		namespace = v
		return nil
	}
}

// WithReportPeriod sets the reporting period for periodic metric
// exporters (otlp-http and otlp-grpc).
func WithReportPeriod(v time.Duration) Option {
	return func() error {
		reportPeriod = v
		return nil
	}
}

// WithMetrics sets the enabled and metrics.
func WithMetrics(cfg *config.Config) Option {
	return func() error {
		if cfg != nil {
			// Notes: Polled metrics do not exist any more as such.
			// They are treated as any other enabled metrics.
			enabled = append(slices.Clone(cfg.Enabled), cfg.Polled...)
		} else {
			enabled = nil
		}
		return nil
	}
}

// Start metrics collection and exporting.
func Start(m *http.ServeMux, resource *resource.Resource, opts ...Option) error {
	Stop()

	for _, opt := range opts {
		if err := opt(); err != nil {
			return err
		}
	}

	metrics.Configure(enabled)

	if exporter == "" {
		log.Info("no metrics exporter configured, metrics collection disabled")
		metrics.SetProvider(nil)
		metrics.Configure(nil)
		return nil
	}

	if m == nil {
		log.Info("no mux provided, metrics collection disabled")
		metrics.SetProvider(nil)
		metrics.Configure(nil)
		return nil
	}

	var (
		ctx     = context.Background()
		options = []metric.Option{metric.WithResource(resource)}
	)

	switch exporter {
	case promExporter:
		log.Info("using OpenTelemetry Prometheus exporter")

		// To enable/disable 'standard' OpenTelemetry or runtime-provided
		// metrics we either use the default prometheus registerer (enabled)
		// or one-off custom one (disabled).
		registry := prometheus.DefaultRegisterer
		if !metrics.IsEnabled("standard", "") {
			registry = prometheus.NewRegistry()
		}
		gatherer := registry.(prometheus.Gatherer)

		exp, err := otelprom.New(
			otelprom.WithNamespace(namespace),
			otelprom.WithRegisterer(registry),
			otelprom.WithoutScopeInfo(),
			otelprom.WithoutTargetInfo(),
		)
		if err != nil {
			return fmt.Errorf("failed to create OpenTelemetry Prometheus exporter: %w", err)
		}

		options = append(options, metric.WithReader(exp))

		handlerOpts := promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
		}
		m.Handle("/metrics", promhttp.HandlerFor(gatherer, handlerOpts))

	case httpExporter, "otel-http":
		log.Info("using OpenTelemetry HTTP exporter")

		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return fmt.Errorf("failed to create OpenTelemetry HTTP exporter: %w", err)
		}

		options = append(options,
			metric.WithReader(
				metric.NewPeriodicReader(exp, metric.WithInterval(reportPeriod)),
			),
		)

	case grpcExporter, "otel-grpc":
		log.Info("using OpenTelemetry gRPC exporter")

		exp, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			return fmt.Errorf("failed to create OpenTelemetry gRPC exporter: %w", err)
		}

		options = append(options,
			metric.WithReader(
				metric.NewPeriodicReader(exp, metric.WithInterval(reportPeriod)),
			),
		)
	}

	log.Info("starting metrics exporter...")

	provider = metric.NewMeterProvider(options...)
	metrics.SetProvider(provider)

	mux = m

	return nil
}

// Stop metrics collection and exporting.
func Stop() {
	if provider != nil {
		err := provider.Shutdown(context.Background())
		if err != nil {
			log.Error("failed to shut down metrics provider: %v", err)
		}
		provider = nil
	}

	if mux != nil {
		mux.Unregister("/metrics")
		mux = nil
	}

	exporter = ""
	namespace = "nri"
	enabled = nil
	reportPeriod = 0
}
