// Copyright 2026 Intel Corporation. All Rights Reserved.
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

package topologyaware

import (
	"context"
	"os"
	"path"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/metrics"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/testutils"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// TestMetricsUpdateNilReceiver verifies that Update() on a nil
// *TopologyAwareMetrics is a safe no-op.
func TestMetricsUpdateNilReceiver(t *testing.T) {
	var m *TopologyAwareMetrics
	m.Update() // must not panic
}

// TestMetricsUpdateEmptyPools verifies that Update() with a policy that
// has no pools completes without panicking.
func TestMetricsUpdateEmptyPools(t *testing.T) {
	p := &policy{
		pools: nil,
		allocations: allocations{
			grants: make(map[string]Grant),
		},
	}
	m := &TopologyAwareMetrics{
		p:     p,
		Zones: make(map[string]*Zone),
	}
	m.Update() // must not panic
}

// Exact, fully-qualified names of the gauges exercised below. The metrics
// wrapper prefixes instruments with their group ("policy") and subsystem
// ("topologyaware"), so e.g. "zone.cpu.shared.capacity" is exported as
// "policy.topologyaware.zone.cpu.shared.capacity".
const (
	gaugeSharedCapacity      = "policy.topologyaware.zone.cpu.shared.capacity"
	gaugeSharedAssigned      = "policy.topologyaware.zone.cpu.shared.assigned"
	gaugeSharedAvailable     = "policy.topologyaware.zone.cpu.shared.available"
	zoneCPUCapacityGaugeName = "policy.topologyaware.zone.cpu.capacity"
)

var sharedPoolGaugeNames = []string{
	gaugeSharedCapacity,
	gaugeSharedAssigned,
	gaugeSharedAvailable,
}

// newServerPolicyWithMetrics builds a real topology-aware policy from the
// multi-zone "server" test sysfs and wires it to an OpenTelemetry manual
// reader so the policy meters are real (not no-ops). It returns the policy,
// the constructed metrics (NewTopologyAwareMetrics already calls Update()
// once) and the manual reader used to collect them.
func newServerPolicyWithMetrics(t *testing.T) (*policy, *TopologyAwareMetrics, *sdkmetric.ManualReader) {
	t.Helper()

	dir, err := os.MkdirTemp("", "nri-resource-policy-test-sysfs-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { removeAll(t, dir) })

	if err := testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
		t.Fatalf("failed to uncompress test sysfs data: %v", err)
	}

	// The "server" sysfs yields a multi-zone topology, which lets us assert
	// "one exported series per zone".
	sys, err := system.DiscoverSystemAt(path.Join(dir, "sysfs", "server", "sys"))
	if err != nil {
		t.Fatalf("failed to discover system: %v", err)
	}

	opts := &policyapi.BackendOptions{
		Cache:  &mockCache{},
		System: sys,
		Config: &cfgapi.Config{
			ReservedResources: cfgapi.Constraints{
				cfgapi.CPU: "750m",
			},
		},
	}

	p := New().(*policy)
	if err := p.Setup(opts); err != nil {
		t.Fatalf("failed to set up policy: %v", err)
	}

	// The policy meters are gated by pkg/metrics, so the "policy" group and the
	// provider must be set before NewTopologyAwareMetrics() builds them. These
	// globals are process-wide; restore them when the test finishes.
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics.Configure([]string{"policy/topologyaware"})
	metrics.SetProvider(provider)
	t.Cleanup(func() {
		metrics.SetProvider(nil)
		metrics.Configure(nil)
	})

	m, err := p.NewTopologyAwareMetrics()
	if err != nil {
		t.Fatalf("failed to create topology-aware metrics: %v", err)
	}

	return p, m, reader
}

// gaugePoint is a single collected gauge data point: its attribute set plus
// its value. Exactly one of i64/f64 is meaningful depending on the gauge type
// (capacity is an int64 gauge; assigned/available are float64 gauges).
type gaugePoint struct {
	attrs attribute.Set
	i64   int64
	f64   float64
}

// collectGauges collects all metrics from the reader and returns, per metric
// name, every exported data point (i.e. one entry per exported time series).
// Both int64 and float64 gauges are handled.
func collectGauges(t *testing.T, reader *sdkmetric.ManualReader) map[string][]gaugePoint {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("failed to collect metrics: %v", err)
	}

	byName := make(map[string][]gaugePoint)
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if _, ok := byName[md.Name]; !ok {
				byName[md.Name] = nil
			}
			switch g := md.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range g.DataPoints {
					byName[md.Name] = append(byName[md.Name], gaugePoint{attrs: dp.Attributes, i64: dp.Value})
				}
			case metricdata.Gauge[float64]:
				for _, dp := range g.DataPoints {
					byName[md.Name] = append(byName[md.Name], gaugePoint{attrs: dp.Attributes, f64: dp.Value})
				}
			default:
				// Ignore non-gauge metrics; tests below only assert on specific gauges.
				continue
			}
		}
	}
	return byName
}

// zoneOf returns the value of a data point's "zone" attribute, failing the
// test if it is absent.
func zoneOf(t *testing.T, set attribute.Set) string {
	t.Helper()
	v, ok := set.Value(attribute.Key("zone"))
	if !ok {
		t.Fatalf("metric data point is missing the %q attribute", "zone")
	}
	return v.AsString()
}

// attrKeys returns the set of attribute keys present in an attribute set.
func attrKeys(set attribute.Set) map[string]bool {
	keys := make(map[string]bool, set.Len())
	for it := set.Iter(); it.Next(); {
		keys[string(it.Attribute().Key)] = true
	}
	return keys
}

// TestSharedPoolMetricsHaveNoCPUsAttribute verifies the shared-pool gauges are
// identified by "zone" only (no volatile "cpus"/"mems"), while the static
// zone.cpu.capacity gauge still carries its "cpus"/"mems" topology attributes.
func TestSharedPoolMetricsHaveNoCPUsAttribute(t *testing.T) {
	_, _, reader := newServerPolicyWithMetrics(t)

	byName := collectGauges(t, reader)

	for _, name := range sharedPoolGaugeNames {
		points, ok := byName[name]
		if !ok {
			t.Fatalf("metric %q not found in collected metrics", name)
		}
		if len(points) == 0 {
			t.Fatalf("metric %q has no data points", name)
		}
		for i, pt := range points {
			keys := attrKeys(pt.attrs)
			if !keys["zone"] {
				t.Errorf("%s data point %d: missing %q attribute (have %v)", name, i, "zone", keys)
			}
			if keys["cpus"] {
				t.Errorf("%s data point %d: unexpected %q attribute (have %v)", name, i, "cpus", keys)
			}
			if keys["mems"] {
				t.Errorf("%s data point %d: unexpected %q attribute (have %v)", name, i, "mems", keys)
			}
			if len(keys) != 1 {
				t.Errorf("%s data point %d: expected exactly {zone}, got %v", name, i, keys)
			}
		}
	}

	// The static topology gauge intentionally keeps cpus/mems.
	capSets, ok := byName[zoneCPUCapacityGaugeName]
	if !ok {
		t.Fatalf("metric %q not found in collected metrics", zoneCPUCapacityGaugeName)
	}
	if len(capSets) == 0 {
		t.Fatalf("metric %q has no data points", zoneCPUCapacityGaugeName)
	}
	for i, pt := range capSets {
		keys := attrKeys(pt.attrs)
		if !keys["zone"] || !keys["cpus"] || !keys["mems"] {
			t.Errorf("%s data point %d: expected zone/cpus/mems attributes, got %v", zoneCPUCapacityGaugeName, i, keys)
		}
	}
}

// TestSharedPoolMetricsDoNotLeakSeriesOnCpusetChange is the core regression test
// for the series leak. It simulates the shared pool changing over time by
// mutating each pool's sharable CPU set between Update() calls, then asserts each
// shared gauge still exports exactly one series per zone. Pre-fix, the volatile
// "cpus" label produced one series per (zone, layout) instead.
func TestSharedPoolMetricsDoNotLeakSeriesOnCpusetChange(t *testing.T) {
	p, m, reader := newServerPolicyWithMetrics(t)

	numZones := len(m.Zones)
	if numZones == 0 {
		t.Fatalf("expected a multi-zone topology, got 0 zones")
	}

	// NewTopologyAwareMetrics already recorded the original layout. Each round
	// shrinks every pool's shared pool by one CPU (mimicking an exclusive
	// allocation), then re-records. The `changed` guard fails loudly if a round
	// finds nothing left to shrink, so the test can't silently stop exercising
	// the leak.
	const rounds = 3
	for round := 0; round < rounds; round++ {
		changed := false
		for _, pool := range p.pools {
			s := pool.FreeSupply().(*supply)
			list := s.sharable.List()
			if len(list) == 0 {
				continue
			}
			s.sharable = s.sharable.Difference(cpuset.New(list[0]))
			changed = true
		}
		if !changed {
			t.Fatalf("round %d: no pool had a shared CPU left to mutate", round)
		}
		m.Update()
	}

	byName := collectGauges(t, reader)

	for _, name := range sharedPoolGaugeNames {
		points, ok := byName[name]
		if !ok {
			t.Fatalf("metric %q not found in collected metrics", name)
		}
		// One series per zone, regardless of how many distinct shared-pool
		// layouts were recorded. Pre-fix this would be numZones*(rounds+1)
		// because of the volatile "cpus" label.
		if len(points) != numZones {
			t.Errorf("%s exported %d time series, want %d (one per zone); a higher count means the shared pool cpuset is leaking series",
				name, len(points), numZones)
		}
		// Every series must be a distinct zone and carry no "cpus" label.
		zones := make(map[string]bool, len(points))
		for i, pt := range points {
			v, has := pt.attrs.Value(attribute.Key("zone"))
			if !has {
				t.Errorf("%s data point %d: missing %q attribute", name, i, "zone")
				continue
			}
			if pt.attrs.HasValue(attribute.Key("cpus")) {
				t.Errorf("%s data point %d: unexpected %q attribute (series leak)", name, i, "cpus")
			}
			if zones[v.AsString()] {
				t.Errorf("%s: duplicate series for zone %q (series leak)", name, v.AsString())
			}
			zones[v.AsString()] = true
		}
		if len(zones) != numZones {
			t.Errorf("%s: got %d distinct zones, want %d", name, len(zones), numZones)
		}
	}
}

// TestSharedPoolMetricValues is the only test that checks the shared-pool
// gauges report the correct NUMBERS (the other two only check labels and the
// series count). Its goal is to catch a value regression: a wrong CPU count on
// capacity, or a broken milli-cores -> cores (/1000.0) conversion on
// assigned/available - for example a /1000 -> /100 typo or recording the wrong
// supply field.
//
// Two details keep the check from silently passing on a real bug: it seeds a
// known non-zero granted amount before recording (otherwise assigned is zero
// and 0/1000 == 0/100 would hide a broken divisor), and it derives the expected
// values from the free supply, not from m.Zones, so the metric is never
// compared against itself.
func TestSharedPoolMetricValues(t *testing.T) {
	p, m, reader := newServerPolicyWithMetrics(t)

	const (
		grantedSharedMilli   = 2500 // 2.5 cores
		grantedReservedMilli = 1500 // 1.5 cores
	)
	for _, pool := range p.pools {
		s := pool.FreeSupply().(*supply)
		s.grantedShared = grantedSharedMilli
		s.grantedReserved = grantedReservedMilli
	}
	// Re-record the gauges from the now non-zero granted state.
	m.Update()

	byName := collectGauges(t, reader)

	poolByName := make(map[string]Node, len(p.pools))
	for _, pool := range p.pools {
		poolByName[pool.Name()] = pool
	}

	freeSupplyForZone := func(t *testing.T, gauge, zone string) *supply {
		t.Helper()
		pool, ok := poolByName[zone]
		if !ok {
			t.Fatalf("%s: no pool matches zone %q", gauge, zone)
		}
		return pool.FreeSupply().(*supply)
	}

	// capacity (int64): |sharable union reserved|.
	capPoints := byName[gaugeSharedCapacity]
	if len(capPoints) == 0 {
		t.Fatalf("metric %q has no data points", gaugeSharedCapacity)
	}
	for _, pt := range capPoints {
		zone := zoneOf(t, pt.attrs)
		free := freeSupplyForZone(t, gaugeSharedCapacity, zone)
		want := int64(free.SharableCPUs().Union(free.ReservedCPUs()).Size())
		if pt.i64 != want {
			t.Errorf("%s zone %q = %d cores, want %d", gaugeSharedCapacity, zone, pt.i64, want)
		}
	}

	// assigned (float64 cores): (GrantedReserved + GrantedShared) / 1000.
	assignedPoints := byName[gaugeSharedAssigned]
	if len(assignedPoints) == 0 {
		t.Fatalf("metric %q has no data points", gaugeSharedAssigned)
	}
	sawNonZeroAssigned := false
	for _, pt := range assignedPoints {
		zone := zoneOf(t, pt.attrs)
		free := freeSupplyForZone(t, gaugeSharedAssigned, zone)
		want := float64(free.GrantedReserved()+free.GrantedShared()) / 1000.0
		if pt.f64 != want {
			t.Errorf("%s zone %q = %v cores, want %v", gaugeSharedAssigned, zone, pt.f64, want)
		}
		if pt.f64 != 0 {
			sawNonZeroAssigned = true
		}
	}
	if !sawNonZeroAssigned {
		t.Errorf("%s: expected at least one non-zero value to exercise the /1000 conversion", gaugeSharedAssigned)
	}

	// available (float64 cores): AllocatableSharedCPU() / 1000.
	availPoints := byName[gaugeSharedAvailable]
	if len(availPoints) == 0 {
		t.Fatalf("metric %q has no data points", gaugeSharedAvailable)
	}
	sawNonZeroAvail := false
	for _, pt := range availPoints {
		zone := zoneOf(t, pt.attrs)
		free := freeSupplyForZone(t, gaugeSharedAvailable, zone)
		want := float64(free.AllocatableSharedCPU()) / 1000.0
		if pt.f64 != want {
			t.Errorf("%s zone %q = %v cores, want %v", gaugeSharedAvailable, zone, pt.f64, want)
		}
		if pt.f64 != 0 {
			sawNonZeroAvail = true
		}
	}
	if !sawNonZeroAvail {
		t.Errorf("%s: expected at least one non-zero value to exercise the /1000 conversion", gaugeSharedAvailable)
	}
}
