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
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/intel/goresctrl/pkg/monitor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestResctrl creates a minimal resctrl-like filesystem for testing.
// Returns the root path and a cleanup function.
func setupTestResctrl(t *testing.T, groups map[string]map[string]map[string]string) string {
	t.Helper()
	root := t.TempDir()
	monGroupsDir := filepath.Join(root, "mon_groups")
	require.NoError(t, os.MkdirAll(monGroupsDir, 0o755))

	// Collect all domains/counters to also create root-level mon_data
	// (used by RegisterOTelInstruments to discover available instruments).
	allDomains := make(map[string]map[string]string)

	for groupName, domains := range groups {
		groupDir := filepath.Join(monGroupsDir, groupName)
		require.NoError(t, os.MkdirAll(groupDir, 0o755))
		// Create tasks file.
		require.NoError(t, os.WriteFile(filepath.Join(groupDir, "tasks"), []byte(""), 0o644))
		monDataDir := filepath.Join(groupDir, "mon_data")
		for domain, counters := range domains {
			domDir := filepath.Join(monDataDir, domain)
			require.NoError(t, os.MkdirAll(domDir, 0o755))
			for file, value := range counters {
				require.NoError(t, os.WriteFile(filepath.Join(domDir, file), []byte(value+"\n"), 0o644))
			}
			// Merge into allDomains.
			if allDomains[domain] == nil {
				allDomains[domain] = make(map[string]string)
			}
			for file, value := range counters {
				allDomains[domain][file] = value
			}
		}
	}

	// Create root-level mon_data for instrument discovery.
	rootMonData := filepath.Join(root, "mon_data")
	for domain, counters := range allDomains {
		domDir := filepath.Join(rootMonData, domain)
		require.NoError(t, os.MkdirAll(domDir, 0o755))
		for file, value := range counters {
			require.NoError(t, os.WriteFile(filepath.Join(domDir, file), []byte(value+"\n"), 0o644))
		}
	}

	return root
}

func TestTelemetryPrometheusEndpoint(t *testing.T) {
	podUID := "12345678-1234-1234-1234-123456789abc"
	root := setupTestResctrl(t, map[string]map[string]map[string]string{
		podUID: {
			"mon_L3_00": {
				"llc_occupancy":   "4096",
				"mbm_local_bytes": "1000000",
				"mbm_total_bytes": "2000000",
			},
			"mon_PERF_PKG_00": {
				"core_energy": "54446119.644974",
				"activity":    "12345.6789",
				"c6_res":      "9999",
			},
		},
	})

	mgr, err := monitor.New(monitor.Options{
		ResctrlRoot:      root,
		KeyValidator:     monitor.PodUIDValidator,
		KeyCanonicalizer: monitor.CanonicalizePodUID,
	})
	require.NoError(t, err)
	_, err = mgr.EnsureGroup(podUID, "")
	require.NoError(t, err)

	// Bind an ephemeral port to avoid conflicts on shared CI runners.
	cfg := defaultTelemetryConfig()
	cfg.Prometheus.ListenAddress = "127.0.0.1:0"
	cfg.PerfCounters.Enabled = false // default: suppress perf counters

	state, err := newTelemetry(context.Background(), cfg)
	require.NoError(t, err)
	defer state.shutdown(context.Background())

	meter := state.provider.Meter("nri-resctrl-mon-test")
	_, err = setupMetrics(mgr, cfg, root, meter)
	require.NoError(t, err)

	// Scrape the actual address the listener bound to.
	addr := state.promListener.Addr().String()

	// Wait for server to be ready.
	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get("http://" + addr + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	metrics := string(body)

	// Verify core instruments are registered and produce correct Prometheus names.
	// Instrument names are derived from domain + counter:
	//   mon_L3_00/llc_occupancy → l3.llc.occupancy (unit By) → l3_llc_occupancy_bytes
	//   mon_L3_00/mbm_local_bytes → l3.mbm.local.bytes (unit By) → l3_mbm_local_bytes_total
	//   mon_L3_00/mbm_total_bytes → l3.mbm.total.bytes (unit By) → l3_mbm_bytes_total
	//     (the otlptranslator collapses the semantic "total" into the counter
	//     "_total" suffix; the local counter above disambiguates it)
	//   mon_PERF_PKG_00/core_energy → perf.core.energy (unit J) → perf_core_energy_joules_total
	//   mon_PERF_PKG_00/activity → perf.activity (unit farads) → perf_activity_farads_total
	assert.Contains(t, metrics, "l3_llc_occupancy_bytes")
	assert.Contains(t, metrics, "l3_mbm_local_bytes_total")
	assert.Contains(t, metrics, "l3_mbm_bytes_total")
	assert.Contains(t, metrics, "perf_core_energy_joules_total")
	assert.Contains(t, metrics, "perf_activity_farads_total")

	// Verify float64 fidelity through the full pipeline.
	assert.Contains(t, metrics, "5.4446119644974e+07",
		"core_energy float64 value must survive OTel→Prometheus rendering")
}

func TestFloat64Fidelity(t *testing.T) {
	podUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	root := setupTestResctrl(t, map[string]map[string]map[string]string{
		podUID: {
			"mon_PERF_PKG_00": {
				"core_energy": "54446119.644974",
			},
		},
	})

	mgr, err := monitor.New(monitor.Options{
		ResctrlRoot:      root,
		KeyValidator:     monitor.PodUIDValidator,
		KeyCanonicalizer: monitor.CanonicalizePodUID,
	})
	require.NoError(t, err)
	_, err = mgr.EnsureGroup(podUID, "")
	require.NoError(t, err)

	readings, err := mgr.ReadCounters(podUID)
	require.NoError(t, err)
	require.Len(t, readings, 1)
	assert.Equal(t, 54446119.644974, readings[0].Value,
		"float64 value must preserve kernel precision without integer truncation")
}

func TestPerfCountersGate(t *testing.T) {
	podUID := "11111111-2222-3333-4444-555555555555"
	root := setupTestResctrl(t, map[string]map[string]map[string]string{
		podUID: {
			"mon_PERF_PKG_00": {
				"core_energy":          "100.5",
				"activity":             "50.0",
				"c6_res":               "9999",
				"unhalted_core_cycles": "123456",
			},
			"mon_L3_00": {
				"llc_occupancy": "8192",
			},
		},
	})

	mgr, err := monitor.New(monitor.Options{
		ResctrlRoot:      root,
		KeyValidator:     monitor.PodUIDValidator,
		KeyCanonicalizer: monitor.CanonicalizePodUID,
	})
	require.NoError(t, err)
	_, err = mgr.EnsureGroup(podUID, "")
	require.NoError(t, err)

	readings, err := mgr.ReadCounters(podUID)
	require.NoError(t, err)

	t.Run("disabled suppresses perf-only counters", func(t *testing.T) {
		cfg := defaultTelemetryConfig()
		cfg.PerfCounters.Enabled = false
		filter := perfCounterFilter(cfg)

		var allowed []string
		for _, r := range readings {
			if filter(r) {
				allowed = append(allowed, r.Name)
			}
		}
		// core_energy, activity (from PERF_PKG) + llc_occupancy (from L3) should pass.
		assert.Contains(t, allowed, "core_energy")
		assert.Contains(t, allowed, "activity")
		assert.Contains(t, allowed, "llc_occupancy")
		// c6_res and unhalted_core_cycles should be blocked.
		assert.NotContains(t, allowed, "c6_res")
		assert.NotContains(t, allowed, "unhalted_core_cycles")
	})

	t.Run("enabled allows all perf counters", func(t *testing.T) {
		cfg := defaultTelemetryConfig()
		cfg.PerfCounters.Enabled = true
		filter := perfCounterFilter(cfg)

		var allowed []string
		for _, r := range readings {
			if filter(r) {
				allowed = append(allowed, r.Name)
			}
		}
		assert.Contains(t, allowed, "c6_res")
		assert.Contains(t, allowed, "unhalted_core_cycles")
	})

	t.Run("include list filters", func(t *testing.T) {
		cfg := defaultTelemetryConfig()
		cfg.PerfCounters.Enabled = true
		cfg.PerfCounters.Include = []string{"unhalted_*"}
		filter := perfCounterFilter(cfg)

		var allowed []string
		for _, r := range readings {
			if filter(r) {
				allowed = append(allowed, r.Name)
			}
		}
		assert.Contains(t, allowed, "unhalted_core_cycles")
		assert.Contains(t, allowed, "core_energy") // always allowed (core AET)
		assert.NotContains(t, allowed, "c6_res")   // not in include list
	})
}

func TestControlGroupOf(t *testing.T) {
	tests := []struct {
		root string
		path string
		want string
	}{
		{"/sys/fs/resctrl", "/sys/fs/resctrl/mon_groups/abc-123", ""},
		{"/sys/fs/resctrl", "/sys/fs/resctrl/COS1/mon_groups/abc-123", "COS1"},
		{"/sys/fs/resctrl", "/sys/fs/resctrl/my-class/mon_groups/abc-123", "my-class"},
		{"/mnt/rdt", "/mnt/rdt/mon_groups/abc-123", ""},
		{"/mnt/rdt", "/mnt/rdt/COS1/mon_groups/abc-123", "COS1"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, controlGroupOf(tt.root, tt.path))
		})
	}
}

func TestValidateTelemetryConfig(t *testing.T) {
	t.Run("valid defaults", func(t *testing.T) {
		cfg := defaultTelemetryConfig()
		assert.NoError(t, validateTelemetryConfig(&cfg))
	})

	t.Run("otlp enabled requires endpoint", func(t *testing.T) {
		cfg := defaultTelemetryConfig()
		cfg.OTLP.Enabled = true
		assert.Error(t, validateTelemetryConfig(&cfg))
	})

	t.Run("invalid protocol", func(t *testing.T) {
		cfg := defaultTelemetryConfig()
		cfg.OTLP.Enabled = true
		cfg.OTLP.Endpoint = "localhost:4317"
		cfg.OTLP.Protocol = "websocket"
		assert.Error(t, validateTelemetryConfig(&cfg))
	})

	t.Run("include and exclude mutually exclusive", func(t *testing.T) {
		cfg := defaultTelemetryConfig()
		cfg.PerfCounters.Include = []string{"c6_res"}
		cfg.PerfCounters.Exclude = []string{"c1_res"}
		assert.Error(t, validateTelemetryConfig(&cfg))
	})
}

func TestInstrumentNaming(t *testing.T) {
	// Verify the library's derived naming matches expectations.
	t.Run("L3 counters match rdt convention", func(t *testing.T) {
		assert.Equal(t, "l3.llc.occupancy", monitor.InstrumentName("mon_L3_00", "llc_occupancy"))
		assert.Equal(t, "l3.mbm.local.bytes", monitor.InstrumentName("mon_L3_00", "mbm_local_bytes"))
		assert.Equal(t, "l3.mbm.total.bytes", monitor.InstrumentName("mon_L3_00", "mbm_total_bytes"))
	})

	t.Run("PERF_PKG counters", func(t *testing.T) {
		assert.Equal(t, "perf.core.energy", monitor.InstrumentName("mon_PERF_PKG_00", "core_energy"))
		assert.Equal(t, "perf.activity", monitor.InstrumentName("mon_PERF_PKG_00", "activity"))
		assert.Equal(t, "perf.c1.res", monitor.InstrumentName("mon_PERF_PKG_00", "c1_res"))
	})
}

func TestMatchGlob(t *testing.T) {
	assert.True(t, matchGlob("unhalted_*", "unhalted_core_cycles"))
	assert.True(t, matchGlob("unhalted_*", "unhalted_ref_cycles"))
	assert.False(t, matchGlob("unhalted_*", "c6_res"))
	assert.True(t, matchGlob("c6_res", "c6_res"))
	assert.True(t, matchGlob("*_bytes", "mbm_local_bytes"))
}
