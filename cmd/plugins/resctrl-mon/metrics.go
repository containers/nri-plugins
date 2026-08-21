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
	"regexp"
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
func setupMetrics(mgr *monitor.Manager, cfg telemetryConfig, meter otelmetric.Meter) (*monitor.Registration, error) {
	return mgr.RegisterOTelInstruments(meter,
		monitor.WithFilter(perfCounterFilter(cfg)),
		monitor.WithAttributes(groupAttributes),
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

// groupAttributes provides per-group OTel attributes.
func groupAttributes(key, path string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("k8s.pod.uid", key),
		attribute.String("resctrl.control_group", controlGroupOf(path)),
		attribute.String("resctrl.group.source", sourceFor(key)),
	}
}

// matchGlob does simple glob matching (only * is supported as wildcard).
func matchGlob(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if strings.Contains(pattern, "*") {
		re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
		matched, _ := regexp.MatchString(re, name)
		return matched
	}
	return false
}

// controlGroupOf extracts the CTRL group name from a mon_group path.
// e.g. "/sys/fs/resctrl/COS1/mon_groups/abc-123" → "COS1"
// e.g. "/sys/fs/resctrl/mon_groups/abc-123" → "" (root/default)
func controlGroupOf(groupPath string) string {
	ctrlDir := filepath.Dir(filepath.Dir(groupPath))
	base := filepath.Base(ctrlDir)
	if base == "resctrl" || base == "." || base == "/" {
		return ""
	}
	return base
}

// uuidPattern matches standard dashed UUID (pod UIDs).
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sourceFor classifies a group key as "pod" (UUID) or "other".
func sourceFor(key string) string {
	if uuidPattern.MatchString(key) {
		return "pod"
	}
	return "other"
}
