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
	"sync"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	httpExporter = "otlp-http"
	grpcExporter = "otlp-grpc"
)

var (
	_ sdktrace.SpanExporter = (*spanExporter)(nil)
)

type spanExporter struct {
	sync.RWMutex
	exporter sdktrace.SpanExporter
}

func (e *spanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.RLock()
	defer e.RUnlock()

	if e.exporter == nil {
		return nil
	}
	return e.exporter.ExportSpans(ctx, spans)
}

func (e *spanExporter) Shutdown(ctx context.Context) error {
	e.Lock()
	defer e.Unlock()

	if e.exporter == nil {
		return nil
	}

	err := e.exporter.Shutdown(ctx)
	e.exporter = nil

	return err
}

func (e *spanExporter) setEndpoint(endpoint string) error {
	if err := e.shutdown(); err != nil {
		log.Warnf("failed to shutdown tracing exporter: %v", err)
	}

	if endpoint == "" {
		return nil
	}

	e.Lock()
	defer e.Unlock()

	var (
		exp sdktrace.SpanExporter
		err error
	)

	switch endpoint {
	case httpExporter:
		exp, err = otlptracehttp.New(context.Background())
		e.exporter = exp
		return err
	case grpcExporter:
		exp, err = otlptracegrpc.New(context.Background())
		e.exporter = exp
		return err
	}

	return fmt.Errorf("unsupported tracing endpoint %q", endpoint)
}

func (e *spanExporter) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	return e.Shutdown(ctx)
}
