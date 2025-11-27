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
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Config provides runtime configuration for instrumentation.
// +kubebuilder:object:generate=true
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
	// +optional
	// +kubebuilder:example="otlp-http://localhost:4318"
	TracingCollector string `json:"tracingCollector,omitempty"`
	// MetricsExporter defines which exporter is used to export metrics.
	// The supported exporters are:
	//   - prometheus: use OpenTelemetry prometheus exporter
	//   - otlp-http: use OpenTelemetry HTTP metrics exporter
	//   - otlp-grpc: use OpenTelemetry gRPC metrics exporter
	// +optional
	// +kubebuilder:validation:Enum=prometheus;otlp-http;otlp-grpc
	// +kubebuilder:example="prometheus"
	MetricsExporter string `json:"metricsExporter,omitempty"`
	// ReportPeriod is the interval between collecting polled metrics.
	// +optional
	// +kubebuilder:validation:Format="duration"
	// +kubebuilder:default="30s"
	ReportPeriod metav1.Duration `json:"reportPeriod,omitempty"`
	// HTTPEndpoint is the address our HTTP server listens on. This endpoint is used
	// to expose Prometheus metrics among other things.
	// +optional
	// +kubebuilder:example=":8891"
	HTTPEndpoint string `json:"httpEndpoint,omitempty"`
	// PrometheusExport enables exporting /metrics for Prometheus. This is
	// equivalent to setting MetricsExporter to "prometheus".
	// +optional
	PrometheusExport bool `json:"prometheusExport,omitempty"`
	// Metrics defines which metrics to collect.
	// +kubebuilder:default={"enabled": {"policy", "buildinfo"}}
	Metrics *metrics.Config `json:"metrics,omitempty"`
}
