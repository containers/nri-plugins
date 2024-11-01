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

package topologyaware

import (
	"slices"
	"strings"

	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	descZone = iota
	descZoneCpuSharedCapacity
	descZoneCpuSharedAssigned
	descZoneCpuSharedAvailable
	descZoneMemCapacity
	descZoneMemAssigned
	descZoneMemAvailable
	descZoneContainerCount
	descZoneSharedContainerCount
)

var (
	descriptors = []*prometheus.Desc{
		descZone: prometheus.NewDesc(
			"topologyaware_policy_zone_cpu_capacity",
			"A topology zone of CPUs.",
			[]string{
				"zone",
				"cpus",
				"mems",
			},
			nil,
		),
		descZoneCpuSharedCapacity: prometheus.NewDesc(
			"topologyaware_policy_zone_cpu_shared_capacity",
			"Capacity of shared CPU pool of a topology zone.",
			[]string{
				"zone",
				"cpus",
			},
			nil,
		),
		descZoneCpuSharedAssigned: prometheus.NewDesc(
			"topologyaware_policy_zone_cpu_shared_assigned",
			"Assigned amount of shared CPU pool of a topology zone.",
			[]string{
				"zone",
				"cpus",
			},
			nil,
		),
		descZoneCpuSharedAvailable: prometheus.NewDesc(
			"topologyaware_policy_zone_cpu_shared_available",
			"Available amount of shared CPU pool of a topology zone.",
			[]string{
				"zone",
				"cpus",
			},
			nil,
		),
		descZoneMemCapacity: prometheus.NewDesc(
			"topologyaware_zone_mem_capacity",
			"Memory capacity of a topology zone.",
			[]string{
				"zone",
				"mems",
			},
			nil,
		),
		descZoneMemAssigned: prometheus.NewDesc(
			"topologyaware_zone_mem_assigned",
			"Amount of assigned memory of a topology zone.",
			[]string{
				"zone",
				"mems",
			},
			nil,
		),
		descZoneMemAvailable: prometheus.NewDesc(
			"topologyaware_zone_mem_available",
			"Amount of available memory of a topology zone.",
			[]string{
				"zone",
				"mems",
			},
			nil,
		),
		descZoneContainerCount: prometheus.NewDesc(
			"topologyaware_zone_container_count",
			"Number of containers assigned to a topology zone.",
			[]string{
				"zone",
			},
			nil,
		),
		descZoneSharedContainerCount: prometheus.NewDesc(
			"topologyaware_zone_shared_container_count",
			"Number of containers in the shared CPU pool of a topology zone.",
			[]string{
				"zone",
			},
			nil,
		),
	}
)

type ZoneMetrics struct {
	Name             string
	Cpus             cpuset.CPUSet
	Mems             libmem.NodeMask
	SharedPool       cpuset.CPUSet
	SharedAssigned   int
	SharedAvailable  int
	MemCapacity      int64
	MemAssigned      int64
	MemAvailable     int64
	Containers       int
	SharedContainers int
}

type TopologyAwareMetrics struct {
	Zones   []string
	Metrics map[string]*ZoneMetrics
}

func (zm *ZoneMetrics) Collect() []prometheus.Metric {
	if zm == nil {
		return nil
	}

	var metrics []prometheus.Metric

	metrics = append(metrics,
		prometheus.MustNewConstMetric(
			descriptors[descZone],
			prometheus.GaugeValue,
			float64(zm.Cpus.Size()),
			zm.Name,
			zm.Cpus.String(),
			zm.Mems.MemsetString(),
		),
		prometheus.MustNewConstMetric(
			descriptors[descZoneCpuSharedCapacity],
			prometheus.GaugeValue,
			float64(zm.SharedPool.Size()),
			zm.Name,
			zm.SharedPool.String(),
		),
		prometheus.MustNewConstMetric(
			descriptors[descZoneCpuSharedAssigned],
			prometheus.GaugeValue,
			float64(zm.SharedAssigned)/1000.0,
			zm.Name,
			zm.SharedPool.String(),
		),
		prometheus.MustNewConstMetric(
			descriptors[descZoneCpuSharedAvailable],
			prometheus.GaugeValue,
			float64(zm.SharedAvailable)/1000.0,
			zm.Name,
			zm.SharedPool.String(),
		),
		prometheus.MustNewConstMetric(
			descriptors[descZoneMemCapacity],
			prometheus.GaugeValue,
			float64(zm.MemCapacity),
			zm.Name,
			zm.Mems.MemsetString(),
		),
		prometheus.MustNewConstMetric(
			descriptors[descZoneMemAssigned],
			prometheus.GaugeValue,
			float64(zm.MemAssigned),
			zm.Name,
			zm.Mems.MemsetString(),
		),
		prometheus.MustNewConstMetric(
			descriptors[descZoneMemAvailable],
			prometheus.GaugeValue,
			float64(zm.MemAvailable),
			zm.Name,
			zm.Mems.MemsetString(),
		),
		prometheus.MustNewConstMetric(
			descriptors[descZoneContainerCount],
			prometheus.GaugeValue,
			float64(zm.Containers),
			zm.Name,
		),
		prometheus.MustNewConstMetric(
			descriptors[descZoneSharedContainerCount],
			prometheus.GaugeValue,
			float64(zm.SharedContainers),
			zm.Name,
		),
	)

	log.Debug("collected zone %s metrics...", zm.Name)

	return metrics
}

// DescribeMetrics generates policy-specific prometheus metrics data descriptors.
func (p *policy) DescribeMetrics() []*prometheus.Desc {
	log.Debug("has %d metrics descriptors", len(descriptors))
	return descriptors
}

// PollMetrics provides policy metrics for monitoring.
func (p *policy) PollMetrics() policyapi.Metrics {
	m := &TopologyAwareMetrics{
		Zones:   make([]string, 0, len(p.pools)),
		Metrics: make(map[string]*ZoneMetrics),
	}

	for _, pool := range p.pools {
		m.Zones = append(m.Zones, pool.Name())

		var (
			capa       = pool.GetSupply().(*supply)
			free       = pool.FreeSupply().(*supply)
			cpus       = capa.ReservedCPUs().Union(capa.IsolatedCPUs()).Union(capa.SharableCPUs())
			mems       = libmem.NewNodeMask(pool.GetMemset(memoryAll).Members()...)
			sharedPool = free.SharableCPUs().Union(free.ReservedCPUs())
			containers []string
			sharedctrs []string
		)

		for id, g := range p.allocations.grants {
			if g.GetCPUNode().Name() == pool.Name() {
				containers = append(containers, id)
				if g.ReservedPortion() != 0 || g.CPUPortion() != 0 {
					sharedctrs = append(sharedctrs, id)
				}
			}
		}

		zone := &ZoneMetrics{
			Name:             pool.Name(),
			Cpus:             cpus,
			Mems:             mems,
			SharedPool:       sharedPool,
			SharedAssigned:   free.GrantedReserved() + free.GrantedShared(),
			SharedAvailable:  free.AllocatableSharedCPU(),
			MemCapacity:      p.memAllocator.ZoneCapacity(mems),
			MemAssigned:      p.memAllocator.ZoneUsage(mems),
			MemAvailable:     p.memAllocator.ZoneAvailable(mems),
			Containers:       len(containers),
			SharedContainers: len(sharedctrs),
		}

		m.Metrics[zone.Name] = zone

		log.Debug("polled zone %s for metrics...", pool.Name())
	}

	slices.SortFunc(m.Zones, func(a, b string) int {
		poolA, poolB := p.nodes[a], p.nodes[b]
		if diff := poolA.RootDistance() - poolB.RootDistance(); diff != 0 {
			return diff
		}
		return strings.Compare(a, b)
	})

	return m
}

// CollectMetrics generates prometheus metrics from cached/polled policy-specific metrics data.
func (p *policy) CollectMetrics(pm policyapi.Metrics) ([]prometheus.Metric, error) {
	m, ok := pm.(*TopologyAwareMetrics)
	if !ok {
		return nil, policyError("unexpected policy metrics type %T", pm)
	}

	var collected []prometheus.Metric

	for _, name := range m.Zones {
		collected = append(collected, m.Metrics[name].Collect()...)
	}

	return collected, nil
}
