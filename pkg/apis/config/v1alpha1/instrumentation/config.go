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

package instrumentation

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Config provides runtime configuration for instrumentation.
type Config struct {
	// SamplingRatePerMillion is the number of samples to collect per million spans.
	// +optional
	// +kubebuilder:example=100000
	SamplingRatePerMillion int `json:"samplingRatePerMillion,omitempty"`
	// TracingCollector defines the external endpoint for tracing data collection.
	// Endpoints are specified as full URLs, or as plain URL schemes which then
	// imply scheme-specific defaults. The supported schemes and their default
	// URLs are:
	//   - otlp-http, http: localhost:4318
	//   - otlp-grpc, grpc: localhost:4317
	//   - jaeger: $OTEL_EXPORTER_JAEGER_ENDPOINT or http://localhost:14268/api/traces
	// +optional
	// +kubebuilder:example="otlp-http://localhost:4318"
	TracingCollector string `json:"tracingCollector,omitempty"`
	// ReportPeriod is the interval between reporting aggregated metrics.
	// +optional
	// +kubebuilder:validation:Format="duration"
	ReportPeriod metav1.Duration `json:"reportPeriod,omitempty"`
	// HTTPEndpoint is the address our HTTP server listens on. This endpoint is used
	// to expose Prometheus metrics among other things.
	// +optional
	// +kubebuilder:example=":8891"
	HTTPEndpoint string `json:"httpEndpoint,omitempty"`
	// PrometheusExport enables exporting /metrics for Prometheus.
	// +optional
	PrometheusExport bool `json:"prometheusExport,omitempty"`
}
