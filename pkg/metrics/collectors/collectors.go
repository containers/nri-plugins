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

package collectors

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/metrics"
	"github.com/containers/nri-plugins/pkg/version"
)

var (
	log = logger.Get("metrics")
)

func NewVersionInfoCollector(v, b string) prometheus.Collector {
	return prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "version_info",
			Help: "A metric with constant '1' value labeled by version and build info.",
			ConstLabels: prometheus.Labels{
				"version": v,
				"build":   b,
			},
		},
		func() float64 { return 1 },
	)
}

func init() {
	var (
		collectors = map[string]prometheus.Collector{
			"buildinfo":   collectors.NewBuildInfoCollector(),
			"golang":      collectors.NewGoCollector(),
			"process":     collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
			"versioninfo": NewVersionInfoCollector(version.Version, version.Build),
		}
		options = []metrics.RegisterOption{
			metrics.WithGroup("standard"),
			metrics.WithCollectorOptions(
				metrics.WithoutNamespace(),
				metrics.WithoutSubsystem(),
			),
		}
	)

	for name, collector := range collectors {
		if err := metrics.Register(name, collector, options...); err != nil {
			log.Error("failed to register %s collector: %v", name, err)
		}
	}
}
