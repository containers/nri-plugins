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
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.19.0"
	"go.opentelemetry.io/otel/trace"

	logger "github.com/containers/nri-plugins/pkg/log"
)

// Option represents an option which can be applied to tracing.
type Option func(*tracing) error

type tracing struct {
	service  string
	identity []attribute.KeyValue
	endpoint string
	sampling float64
	exporter *spanExporter
	sampler  *sampler
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
}

var (
	log = logger.Get("tracing")
	trc = &tracing{
		service:  filepath.Base(os.Args[0]),
		sampler:  &sampler{},
		exporter: &spanExporter{},
	}
)

const (
	// timeouts for flushing trace providers and shutting down exporters
	flushTimeout    = 3 * time.Second
	shutdownTimeout = 3 * time.Second
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
func Start(resource *resource.Resource, options ...Option) error {
	return trc.start(resource, options...)
}

// Stop tracing.
func Stop() {
	// Notes:
	//   We only ever flush our provider, we never shut it down.
	//
	//   Our tracer provider is set as the global otel tracer provider.
	//   We cannot shut it down, because once a global provider is set
	//   it cannot be changed and once a provider is stopped it cannot
	//   be restarted. Therefore, we effectively shut tracing down by
	//   never sampling if tracing is disabled (endpoint == "").
	trc.flush()
}

func (t *tracing) start(resource *resource.Resource, options ...Option) error {
	log.Info("starting tracing exporter...")

	for _, opt := range options {
		if err := opt(t); err != nil {
			return fmt.Errorf("failed to set tracing option: %w", err)
		}
	}

	err := t.exporter.setEndpoint(t.endpoint)
	if err != nil {
		return fmt.Errorf("failed to configure tracing exporter: %w", err)
	}

	if t.endpoint == "" {
		log.Info("tracing effectively disabled, no endpoint set")
		t.sampler.setSampler(nil)
		return nil
	}

	t.sampler.setSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(t.sampling)))

	if t.provider != nil {
		return nil
	}

	/*
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
			)*/

	t.provider = sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource),
		sdktrace.WithSpanProcessor(
			sdktrace.NewBatchSpanProcessor(t.exporter),
		),
		sdktrace.WithSampler(
			t.sampler,
		),
	)
	t.tracer = t.provider.Tracer(t.service, trace.WithSchemaURL(semconv.SchemaURL))

	otel.SetTracerProvider(t.provider)

	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	otel.SetTextMapPropagator(propagator)

	return nil
}

func (t *tracing) flush() {
	if t.provider == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()

	if err := t.provider.ForceFlush(ctx); err != nil {
		log.Errorf("failed to flush tracer provider: %v", err)
	}
}
