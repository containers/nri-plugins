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
// prometheus types. These help enforce metrics namespacing, allow metrics
// grouping, provide dynamic runtime configurability, and allow for periodic
// collection of computationally expensive metrics which would be too costly
// to calculate each time they are externally requested.
//
// Simple Usage
//
//package main
//
//import (
//    "log"
//    "net/http"
//    "os"
//
//    "github.com/containers/nri-plugins/pkg/metrics"
//    "github.com/prometheus/client_golang/prometheus/collectors"
//    "github.com/prometheus/client_golang/prometheus/promhttp"
//)
//
//func main() {
//    metrics.MustRegister(
//        "build",
//        collectors.NewBuildInfoCollector(),
//        metrics.WithGroup("group1"),
//    )
//    metrics.MustRegister(
//       "golang",
//        collectors.NewGoCollector(),
//        metrics.WithGroup("group1"),
//    )
//    metrics.MustRegister(
//        "process",
//        collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
//        metrics.WithGroup("group2"),
//    )
//
//    enabled = []string{"*"}
//    if len(os.Args) > 1 {
//        enabled = os.Args[1:]
//    }
//
//    g, err := metrics.NewGatherer(metrics.WithMetrics(enabled, nil))
//    if err != nil {
//        log.Fatal(err)
//    }
//
//    http.Handle("/metrics", promhttp.HandlerFor(g, promhttp.HandlerOpts{}))
//    log.Fatal(http.ListenAndServe(":8891", nil))
//}
