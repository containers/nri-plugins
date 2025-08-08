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

package resmgr

import (
	"fmt"
	"log/slog"

	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control/rdt"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	rdtlog = logger.Get("goresctrl")
)

type rdtControl struct {
	hostRoot  string
	resmgr    *resmgr
	collector *rdtCollector
}

func newRdtControl(resmgr *resmgr, hostRoot string) *rdtControl {
	slog.SetDefault(slog.New(rdtlog.SlogHandler()))

	if hostRoot != "" {
		rdt.SetPrefix(opt.HostRoot)
	}

	collector, err := registerRdtCollector()
	if err != nil {
		log.Error("failed to register RDT metrics collector: %v", err)
	}

	return &rdtControl{
		resmgr:    resmgr,
		hostRoot:  hostRoot,
		collector: collector,
	}
}

func (c *rdtControl) configure(cfg *rdt.Config) error {
	if cfg == nil {
		return nil
	}

	if cfg.Enable {
		nativeCfg, force, err := cfg.ToGoresctrl()
		if err != nil {
			return err
		}

		if err := rdt.Initialize(""); err != nil {
			return fmt.Errorf("failed to initialize goresctrl/rdt: %w", err)
		}
		log.Info("goresctrl/rdt initialized")

		if nativeCfg != nil {
			if err := rdt.SetConfig(nativeCfg, force); err != nil {
				return fmt.Errorf("failed to configure goresctrl/rdt: %w", err)
			}
			log.Info("goresctrl/rdt configuration updated")
		} else {
			log.Info("goresctrl/rdt running in discovery mode")
		}
	}

	c.resmgr.cache.ConfigureRDTControl(cfg.Enable)
	c.collector.enable(cfg.Enable)

	return nil
}

type rdtCollector struct {
	prometheus.Collector
	enabled bool
}

func registerRdtCollector() (*rdtCollector, error) {
	options := []metrics.RegisterOption{
		metrics.WithGroup("policy"),
		metrics.WithCollectorOptions(
			metrics.WithoutSubsystem(),
		),
	}

	c := &rdtCollector{Collector: rdt.NewCollector()}

	if err := metrics.Register("rdt", c, options...); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *rdtCollector) enable(enabled bool) {
	c.enabled = enabled
}

func (c *rdtCollector) Describe(ch chan<- *prometheus.Desc) {
	rdtlog.Debug("describing RDT metrics")
	c.Collector.Describe(ch)
}

func (c *rdtCollector) Collect(ch chan<- prometheus.Metric) {
	rdtlog.Debug("collecting RDT metrics")
	c.Collector.Collect(ch)
}
