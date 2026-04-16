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

package logging

import (
	"context"
	"fmt"
	"time"

	logger "github.com/containers/nri-plugins/pkg/log"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Option is an option for log exporting.
type Option func() error

const (
	httpExporter = "otlp-http"
	grpcExporter = "otlp-grpc"

	flushTimeout    = 3 * time.Second
	shutdownTimeout = 3 * time.Second
)

var (
	exporter string
	interval time.Duration
	provider *sdklog.LoggerProvider

	log = logger.Get("logging")
)

// WithExporter sets the logging exporter to use.
func WithExporter(v string) Option {
	return func() error {
		exporter = v
		return nil
	}
}

// WithExportInterval sets the batch interval for exporting logs.
func WithExportInterval(v time.Duration) Option {
	return func() error {
		interval = v
		return nil
	}
}

// Start log exporting.
func Start(loggerName string, resource *resource.Resource, opts ...Option) error {
	Stop()

	for _, opt := range opts {
		if err := opt(); err != nil {
			return err
		}
	}

	var (
		exp sdklog.Exporter
		err error
	)

	switch exporter {
	case httpExporter, "otel-http":
		exp, err = otlploghttp.New(context.Background())
		if err != nil {
			return fmt.Errorf("failed to create otel http log exporter: %w", err)
		}
	case grpcExporter, "otel-grpc":
		exp, err = otlploggrpc.New(context.Background())
		if err != nil {
			return fmt.Errorf("failed to create otel grpc log exporter: %w", err)
		}

	default:
		log.Infof("exporter effectively disabled, no exporter set")
		return nil
	}

	log.Infof("starting logging exporter...")

	provider = sdklog.NewLoggerProvider(
		sdklog.WithProcessor(
			sdklog.NewBatchProcessor(exp, sdklog.WithExportInterval(interval)),
		),
	)
	global.SetLoggerProvider(provider)

	otelLogger := otelslog.NewLogger("nri-plugin", otelslog.WithLoggerProvider(provider))
	logger.SetOtelHandler(otelLogger.Handler())

	return nil
}

// Stop log exporting.
func Stop() {
	if provider != nil {
		if err := Flush(); err != nil {
			log.Errorf("failed to flush down log exporter: %v", err)
		}

		if err := shutdown(); err != nil {
			log.Errorf("failed to shut down log exporter: %v", err)
		}

		provider = nil
	}
	exporter = ""
	interval = 0

	logger.SetOtelHandler(nil)
}

// Flush any pending collected logs.
func Flush() error {
	if provider == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()

	return provider.ForceFlush(ctx)
}

func shutdown() error {
	if provider == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	return provider.Shutdown(ctx)
}
