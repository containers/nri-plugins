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

package main

import (
	"path/filepath"
	"strings"

	"github.com/intel/goresctrl/pkg/monitor"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// coreAETFiles are counter files that appear under mon_PERF_PKG_* but are
// always exported (not gated by perfCounters.enabled).
var coreAETFiles = map[string]bool{
	"core_energy": true,
	"activity":    true,
}

// setupMetrics registers OTel instruments via the goresctrl adapter.
func setupMetrics(mgr *monitor.Manager, cfg telemetryConfig, resctrlRoot string, meter otelmetric.Meter) (*monitor.Registration, error) {
	return mgr.RegisterOTelInstruments(meter,
		monitor.WithFilter(perfCounterFilter(cfg)),
		monitor.WithAttributes(groupAttributesFor(resctrlRoot)),
	)
}

// perfCounterFilter returns a FilterFunc that implements the perf counter gate.
func perfCounterFilter(cfg telemetryConfig) monitor.FilterFunc {
	return func(r monitor.Reading) bool {
		// Core AET files (core_energy, activity) are always allowed regardless
		// of which domain they appear under.
		if coreAETFiles[r.Name] {
			return true
		}
		// Non-PERF_PKG domains (mon_L3_*) are always allowed.
		if !strings.HasPrefix(r.Domain, "mon_PERF_PKG_") {
			return true
		}
		// This is a perf counter under mon_PERF_PKG_*. Check the gate.
		if !cfg.PerfCounters.Enabled {
			return false
		}
		// Apply include/exclude lists if configured.
		if len(cfg.PerfCounters.Include) > 0 {
			for _, pattern := range cfg.PerfCounters.Include {
				if matchGlob(pattern, r.Name) {
					return true
				}
			}
			return false
		}
		if len(cfg.PerfCounters.Exclude) > 0 {
			for _, pattern := range cfg.PerfCounters.Exclude {
				if matchGlob(pattern, r.Name) {
					return false
				}
			}
		}
		return true
	}
}

// groupAttributesFor returns a per-group OTel attribute function bound to the
// configured resctrl root, which is needed to recognize the root ctrl_group.
func groupAttributesFor(resctrlRoot string) monitor.AttributeFunc {
	root := filepath.Clean(resctrlRoot)
	return func(key, path string) []attribute.KeyValue {
		return []attribute.KeyValue{
			attribute.String("k8s.pod.uid", key),
			attribute.String("resctrl.control_group", controlGroupOf(root, path)),
			// The manager validates and tracks pod UIDs only, so every exported
			// group is pod-sourced.
			attribute.String("resctrl.group.source", "pod"),
		}
	}
}

// matchGlob does simple glob matching (only * is supported as wildcard).
func matchGlob(pattern, name string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	parts := strings.Split(pattern, "*")
	// The name must start with the segment before the first '*' and end with
	// the segment after the last '*'.
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	last := parts[len(parts)-1]
	if !strings.HasSuffix(name, last) {
		return false
	}
	name = name[:len(name)-len(last)]
	// Any interior segments must appear in order.
	for _, seg := range parts[1 : len(parts)-1] {
		i := strings.Index(name, seg)
		if i < 0 {
			return false
		}
		name = name[i+len(seg):]
	}
	return true
}

// controlGroupOf extracts the CTRL group name from a mon_group path, relative
// to the configured resctrl root.
// e.g. root=/sys/fs/resctrl, "/sys/fs/resctrl/COS1/mon_groups/abc-123" → "COS1"
// e.g. root=/sys/fs/resctrl, "/sys/fs/resctrl/mon_groups/abc-123" → "" (root)
func controlGroupOf(resctrlRoot, groupPath string) string {
	ctrlDir := filepath.Dir(filepath.Dir(groupPath))
	if filepath.Clean(ctrlDir) == filepath.Clean(resctrlRoot) {
		return ""
	}
	return filepath.Base(ctrlDir)
}
