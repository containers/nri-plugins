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

// The metrics package provides a simple framework for collecting and
// exporting metrics. It is implemented as a set of simple wrappers around
// OpenTelemetry types. These allow metrics grouping and provide dynamic
// runtime configurability.
//
// Simple Usage
//
// package main
//
// import (
//     "github.com/containers/nri-plugins/pkg/metrics"
//	   "go.opentelemetry.io/otel/attribute"
//     "go.opentelemetry.io/otel/metric"
// )
//
// var (
//     myCounter metric.Int64Counter
// )
//
// func MyMeteredCodeSetup() error {
//    meter := metrics.Provider("my-meter-group").Meter("subsys")
//
//    myCounter, err = meter.Int64Counter(
//        "my.counter",
//        metric.WithDescription("A simple counter metric"),
//        metric.WithUnit("bytes"),
//    if err != nil {
//        return fmt.Errorf("failed to create MyCounter metric: %w", err)
//    }
//
//    return nil
// }
//
// func MyMeterUpdate()
//   ...
//   myCounter.Add(ctx, 654, attribute.String("label", "value1"))
//   myCounter.Add(ctx, 456, attribute.String("label", "value2"))
//   ...
// }
