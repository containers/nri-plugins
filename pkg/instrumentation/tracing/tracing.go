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

package tracing

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/version"
)

// Option represents an option which can be applied to tracing.
type Option func(*tracing) error

type tracing struct {
	service  string
	identity []attribute.KeyValue
	endpoint string
	sampling float64
	exporter sdktrace.SpanExporter
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
}

var (
	log = logger.Get("tracing")
	trc = &tracing{
		service: filepath.Base(os.Args[0]),
	}
)

const (
	// timeout for shutting down exporters and providers
	shutdownTimeout = 5 * time.Second
)

// WithCollectorEndpoint sets the given collector endpoint.
func WithCollectorEndpoint(endpoint string) Option {
	return func(t *tracing) error {
		t.endpoint = endpoint
		return nil
	}
}

// WithSamplingRatio sets the given sampling ratio.
func WithSamplingRatio(ratio float64) Option {
	return func(t *tracing) error {
		if ratio < 0.0 || ratio > 1.0 {
			return fmt.Errorf("invalid sampling ratio %f", ratio)
		}
		t.sampling = ratio
		return nil
	}
}

// WithServiceName sets the service name reported for tracing.
func WithServiceName(name string) Option {
	return func(t *tracing) error {
		t.service = name
		return nil
	}
}

// WithIdentity sets extra tracing resource/identity attributes.
func WithIdentity(attributes ...KeyValue) Option {
	return func(t *tracing) error {
		t.identity = attributes
		return nil
	}
}

// Start tracing.
func Start(options ...Option) error {
	return trc.start(options...)
}

// Stop tracing.
func Stop() {
	trc.shutdown()
}

func (t *tracing) start(options ...Option) error {
	t.shutdown()

	for _, opt := range options {
		if err := opt(t); err != nil {
			return fmt.Errorf("failed to set tracing option: %w", err)
		}
	}

	switch {
	case t.endpoint == "":
		log.Info("tracing disabled, no endpoint set")
		return nil
	case t.sampling == 0.0:
		log.Info("tracing disabled, sampling ratio is 0.0")
		return nil
	}

	log.Info("starting tracing exporter...")

	hostname, _ := os.Hostname()
	resource := resource.NewWithAttributes(
		semconv.SchemaURL,
		append(
			[]attribute.KeyValue{
				semconv.ServiceName(t.service),
				semconv.HostNameKey.String(hostname),
				semconv.ProcessPIDKey.Int64(int64(os.Getpid())),
				attribute.String("Version", version.Version),
				attribute.String("Build", version.Build),
			},
			t.identity...,
		)...,
	)

	exporter, err := getExporter(t.endpoint)
	if err != nil {
		return fmt.Errorf("failed to start tracing exporter: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource),
		sdktrace.WithSpanProcessor(
			sdktrace.NewBatchSpanProcessor(exporter),
		),
		sdktrace.WithSampler(
			sdktrace.TraceIDRatioBased(t.sampling),
		),
	)

	t.exporter = exporter
	t.provider = provider
	t.tracer = provider.Tracer(t.service, trace.WithSchemaURL(semconv.SchemaURL))

	otel.SetTracerProvider(provider)

	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	otel.SetTextMapPropagator(propagator)

	return nil
}

func (t *tracing) shutdown() {
	if t.provider == nil {
		return
	}

	go func(p *sdktrace.TracerProvider, timeout time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		if err := p.ForceFlush(ctx); err != nil {
			log.Errorf("failed to flush tracer provider: %v", err)
		}
		if err := p.Shutdown(ctx); err != nil {
			log.Errorf("failed tp shutdown tracer provider: %v", err)
		}
	}(t.provider, shutdownTimeout)

	go func(e sdktrace.SpanExporter, timeout time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		if err := e.Shutdown(ctx); err != nil {
			log.Errorf("failed tp shutdown span exporter: %v", err)
		}
	}(t.exporter, shutdownTimeout)

	t.provider = nil
	t.exporter = nil
}

func getExporter(endpoint string) (sdktrace.SpanExporter, error) {
	var (
		u   *url.URL
		err error
	)

	// Notes:
	//   We allow collector endpoint URLs to be given as a plain scheme-prefix,
	//   IOW, without a host, port, and path. If only a prefix is given, the
	//   exporters use defaults defined by the OTLP library. These are:
	//     - otlp-http, http: localhost:4318
	//     - otlp-grpc, grpc: localhost:4317

	switch endpoint {
	case "otlp-http", "http", "otlp-grpc", "grpc":
		u = &url.URL{Scheme: endpoint}
	default:
		u, err = url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("invalid tracing endpoint %q: %w", endpoint, err)
		}
	}

	switch u.Scheme {
	case "otlp-http", "http":
		opts := []otlptracehttp.Option{otlptracehttp.WithInsecure()}
		if u.Host != "" {
			opts = append(opts, otlptracehttp.WithEndpoint(u.Host))
		}
		return otlptracehttp.New(context.Background(), opts...)
	case "otlp-grpc", "grpc":
		opts := []otlptracegrpc.Option{otlptracegrpc.WithInsecure()}
		if u.Host != "" {
			opts = append(opts, otlptracegrpc.WithEndpoint(u.Host))
		}
		return otlptracegrpc.New(context.Background(), opts...)
	}

	return nil, fmt.Errorf("unsupported tracing endpoint %q", endpoint)
}
