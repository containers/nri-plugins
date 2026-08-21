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

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	promexp "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// telemetryConfig holds the OTel exporter configuration for the plugin.
type telemetryConfig struct {
	Prometheus struct {
		Enabled       bool   `json:"enabled"`
		ListenAddress string `json:"listenAddress"`
		Namespace     string `json:"namespace"`
	} `json:"prometheus"`
	OTLP struct {
		Enabled  bool   `json:"enabled"`
		Endpoint string `json:"endpoint"`
		Protocol string `json:"protocol"`
		Interval string `json:"interval"`
		Insecure bool   `json:"insecure"`
	} `json:"otlp"`
	PerfCounters struct {
		Enabled bool     `json:"enabled"`
		Include []string `json:"include"`
		Exclude []string `json:"exclude"`
	} `json:"perfCounters"`
	ResourceAttributes map[string]string `json:"resourceAttributes"`
}

// telemetryState holds the running telemetry components for graceful shutdown.
type telemetryState struct {
	provider     *metric.MeterProvider
	server       *http.Server
	promListener net.Listener
}

// defaultTelemetryConfig returns the config defaults (Prometheus on :9100).
func defaultTelemetryConfig() telemetryConfig {
	var cfg telemetryConfig
	cfg.Prometheus.Enabled = true
	cfg.Prometheus.ListenAddress = ":9100"
	return cfg
}

// validateTelemetryConfig checks the telemetry config for invalid combinations.
func validateTelemetryConfig(cfg *telemetryConfig) error {
	if cfg.OTLP.Enabled && cfg.OTLP.Endpoint == "" {
		return fmt.Errorf("telemetry: otlp.enabled requires a non-empty endpoint")
	}
	if cfg.OTLP.Protocol == "" {
		cfg.OTLP.Protocol = "grpc"
	}
	if cfg.OTLP.Protocol != "grpc" && cfg.OTLP.Protocol != "http" {
		return fmt.Errorf("telemetry: otlp.protocol must be \"grpc\" or \"http\", got %q", cfg.OTLP.Protocol)
	}
	if cfg.OTLP.Interval == "" {
		cfg.OTLP.Interval = "15s"
	}
	if _, err := time.ParseDuration(cfg.OTLP.Interval); err != nil {
		return fmt.Errorf("telemetry: otlp.interval %q: %w", cfg.OTLP.Interval, err)
	}
	if len(cfg.PerfCounters.Include) > 0 && len(cfg.PerfCounters.Exclude) > 0 {
		return fmt.Errorf("telemetry: perfCounters.include and perfCounters.exclude are mutually exclusive")
	}
	if cfg.Prometheus.ListenAddress == "" {
		cfg.Prometheus.ListenAddress = ":9100"
	}
	return nil
}

// newTelemetry creates the MeterProvider with configured exporters.
func newTelemetry(ctx context.Context, cfg telemetryConfig) (*telemetryState, error) {
	var opts []metric.Option

	res, err := resource.New(ctx,
		resource.WithAttributes(resourceAttrs(cfg.ResourceAttributes)...),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: failed to create resource: %w", err)
	}
	opts = append(opts, metric.WithResource(res))

	state := &telemetryState{}

	if cfg.Prometheus.Enabled {
		reg := prometheus.NewRegistry()
		peOpts := []promexp.Option{promexp.WithRegisterer(reg)}
		if cfg.Prometheus.Namespace != "" {
			peOpts = append(peOpts, promexp.WithNamespace(cfg.Prometheus.Namespace))
		}
		pe, err := promexp.New(peOpts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: prometheus exporter: %w", err)
		}
		opts = append(opts, metric.WithReader(pe))

		// Bind synchronously so a failure (e.g. address already in use) is
		// returned to the caller instead of only surfacing asynchronously from
		// the serving goroutine, which would leave the endpoint advertised for
		// scraping with nothing actually listening.
		ln, err := net.Listen("tcp", cfg.Prometheus.ListenAddress)
		if err != nil {
			return nil, fmt.Errorf("telemetry: prometheus listen on %s: %w", cfg.Prometheus.ListenAddress, err)
		}
		state.promListener = ln

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		state.server = &http.Server{Addr: cfg.Prometheus.ListenAddress, Handler: mux}
	}

	if cfg.OTLP.Enabled {
		interval, _ := time.ParseDuration(cfg.OTLP.Interval)
		exp, err := newOTLPExporter(ctx, cfg)
		if err != nil {
			if state.promListener != nil {
				_ = state.promListener.Close()
			}
			return nil, fmt.Errorf("telemetry: OTLP exporter: %w", err)
		}
		opts = append(opts, metric.WithReader(
			metric.NewPeriodicReader(exp, metric.WithInterval(interval)),
		))
		log.Infof("telemetry: OTLP push enabled → %s (%s, interval=%s)",
			cfg.OTLP.Endpoint, cfg.OTLP.Protocol, cfg.OTLP.Interval)
	}

	state.provider = metric.NewMeterProvider(opts...)

	// All fallible setup has succeeded; only now start serving the (already
	// bound) Prometheus listener.
	if state.promListener != nil {
		go func() {
			if err := state.server.Serve(state.promListener); err != nil && err != http.ErrServerClosed {
				log.Warnf("telemetry: prometheus server error: %v", err)
			}
		}()
		log.Infof("telemetry: Prometheus endpoint listening on %s/metrics", state.promListener.Addr())
	}

	return state, nil
}

// shutdown gracefully shuts down the telemetry stack.
func (t *telemetryState) shutdown(ctx context.Context) {
	if t.provider != nil {
		if err := t.provider.Shutdown(ctx); err != nil {
			log.Warnf("telemetry: provider shutdown: %v", err)
		}
	}
	if t.server != nil {
		if err := t.server.Shutdown(ctx); err != nil {
			log.Warnf("telemetry: http server shutdown: %v", err)
		}
	}
}

// newOTLPExporter creates a gRPC or HTTP OTLP metric exporter.
func newOTLPExporter(ctx context.Context, cfg telemetryConfig) (metric.Exporter, error) {
	switch cfg.OTLP.Protocol {
	case "http":
		opts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpoint(cfg.OTLP.Endpoint),
		}
		if cfg.OTLP.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		return otlpmetrichttp.New(ctx, opts...)
	default: // grpc
		opts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithEndpoint(cfg.OTLP.Endpoint),
		}
		if cfg.OTLP.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		return otlpmetricgrpc.New(ctx, opts...)
	}
}

// resourceAttrs builds OTel resource attributes from the config map.
func resourceAttrs(m map[string]string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.ServiceName("nri-resctrl-mon"),
	}
	for k, v := range m {
		if k == "service.name" {
			// Override the default service name.
			attrs[0] = semconv.ServiceName(v)
			continue
		}
		attrs = append(attrs, attribute.String(k, v))
	}
	return attrs
}

// startTelemetry initializes the MeterProvider and registers metrics instruments.
func (p *plugin) startTelemetry(ctx context.Context) error {
	cfg := p.config.Telemetry
	if err := validateTelemetryConfig(&cfg); err != nil {
		return err
	}
	state, err := newTelemetry(ctx, cfg)
	if err != nil {
		return err
	}
	p.telemetry = state

	meter := state.provider.Meter("nri-resctrl-mon")
	reg, err := setupMetrics(p.mgr, cfg, meter)
	if err != nil {
		state.shutdown(ctx)
		return fmt.Errorf("metrics registration: %w", err)
	}
	p.metrics = reg
	return nil
}
