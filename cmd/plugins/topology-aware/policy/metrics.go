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

type TopologyAwareMetrics struct {
	p         *policy
	ZoneNames []string
	Zones     map[string]*Zone
	Metrics   Metrics
}

type Zone struct {
	Name                 string
	Cpus                 cpuset.CPUSet
	Mems                 libmem.NodeMask
	SharedPool           cpuset.CPUSet
	SharedAssigned       int
	SharedAvailable      int
	MemCapacity          int64
	MemAssigned          int64
	MemAvailable         int64
	ContainerCount       int
	SharedContainerCount int
}

type Metrics struct {
	zone                 *prometheus.GaugeVec
	cpuSharedCapacity    *prometheus.GaugeVec
	cpuSharedAssigned    *prometheus.GaugeVec
	cpuSharedAvailable   *prometheus.GaugeVec
	memCapacity          *prometheus.GaugeVec
	memAssigned          *prometheus.GaugeVec
	memAvailable         *prometheus.GaugeVec
	containerCount       *prometheus.GaugeVec
	sharedContainerCount *prometheus.GaugeVec
}

const (
	metricsSubsystem = "topologyaware"
)

func (p *policy) GetMetrics() policyapi.Metrics {
	return p.metrics
}

func (p *policy) NewTopologyAwareMetrics() *TopologyAwareMetrics {
	m := &TopologyAwareMetrics{
		p:     p,
		Zones: make(map[string]*Zone),
		Metrics: Metrics{
			zone: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Subsystem: metricsSubsystem,
					Name:      "zone_cpu_capacity",
					Help:      "A topology zone of CPUs.",
				},
				[]string{
					"zone",
					"cpus",
					"mems",
				},
			),
			cpuSharedCapacity: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Subsystem: metricsSubsystem,
					Name:      "zone_cpu_shared_capacity",
					Help:      "Capacity of shared CPU pool of a topology zone.",
				},
				[]string{
					"zone",
					"cpus",
				},
			),
			cpuSharedAssigned: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Subsystem: metricsSubsystem,
					Name:      "zone_cpu_shared_assigned",
					Help:      "Assigned amount of shared CPU pool of a topology zone.",
				},
				[]string{
					"zone",
					"cpus",
				},
			),
			cpuSharedAvailable: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Subsystem: metricsSubsystem,
					Name:      "zone_cpu_shared_available",
					Help:      "Available amount of shared CPU pool of a topology zone.",
				},
				[]string{
					"zone",
					"cpus",
				},
			),
			memCapacity: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Subsystem: metricsSubsystem,
					Name:      "zone_mem_capacity",
					Help:      "Memory capacity of a topology zone.",
				},
				[]string{
					"zone",
					"mems",
				},
			),
			memAssigned: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Subsystem: metricsSubsystem,
					Name:      "zone_mem_assigned",
					Help:      "Amount of assigned memory of a topology zone.",
				},
				[]string{
					"zone",
					"mems",
				},
			),
			memAvailable: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Subsystem: metricsSubsystem,
					Name:      "zone_mem_available",
					Help:      "Amount of available memory of a topology zone.",
				},
				[]string{
					"zone",
					"mems",
				},
			),
			containerCount: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Subsystem: metricsSubsystem,
					Name:      "zone_container_count",
					Help:      "Number of containers assigned to a topology zone.",
				},
				[]string{
					"zone",
				},
			),
			sharedContainerCount: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Subsystem: metricsSubsystem,
					Name:      "zone_shared_container_count",
					Help:      "Number of containers in the shared CPU pool of a topology zone.",
				},
				[]string{
					"zone",
				},
			),
		},
	}

	for _, pool := range p.pools {
		var (
			name = pool.Name()
			mems = libmem.NewNodeMask(pool.GetMemset(memoryAll).Members()...)
			capa = pool.GetSupply().(*supply)
			cpus = capa.ReservedCPUs().Union(capa.IsolatedCPUs()).Union(capa.SharableCPUs())
			zone = &Zone{
				Name:        name,
				Cpus:        cpus,
				Mems:        mems,
				MemCapacity: p.memAllocator.ZoneCapacity(mems),
			}
		)

		m.Zones[name] = zone
		m.ZoneNames = append(m.ZoneNames, name)

		m.Metrics.zone.WithLabelValues(
			zone.Name,
			zone.Cpus.String(),
			zone.Mems.String(),
		).Set(float64(zone.Cpus.Size()))

		m.Metrics.memCapacity.WithLabelValues(
			zone.Name,
			zone.Mems.String(),
		).Set(float64(zone.MemCapacity))
	}

	slices.SortFunc(m.ZoneNames, func(a, b string) int {
		poolA, poolB := p.nodes[a], p.nodes[b]
		if diff := poolA.RootDistance() - poolB.RootDistance(); diff != 0 {
			return diff
		}
		return strings.Compare(a, b)
	})

	m.Update()

	return m
}

func (m *TopologyAwareMetrics) Describe(ch chan<- *prometheus.Desc) {
	if m == nil {
		return
	}

	m.Metrics.zone.Describe(ch)
	m.Metrics.cpuSharedCapacity.Describe(ch)
	m.Metrics.cpuSharedAssigned.Describe(ch)
	m.Metrics.cpuSharedAvailable.Describe(ch)
	m.Metrics.memCapacity.Describe(ch)
	m.Metrics.memAssigned.Describe(ch)
	m.Metrics.memAvailable.Describe(ch)
	m.Metrics.containerCount.Describe(ch)
	m.Metrics.sharedContainerCount.Describe(ch)
}

func (m *TopologyAwareMetrics) Collect(ch chan<- prometheus.Metric) {
	if m == nil {
		return
	}

	m.Update()

	m.Metrics.zone.Collect(ch)
	m.Metrics.cpuSharedCapacity.Collect(ch)
	m.Metrics.cpuSharedAssigned.Collect(ch)
	m.Metrics.cpuSharedAvailable.Collect(ch)
	m.Metrics.memCapacity.Collect(ch)
	m.Metrics.memAssigned.Collect(ch)
	m.Metrics.memAvailable.Collect(ch)
	m.Metrics.containerCount.Collect(ch)
	m.Metrics.sharedContainerCount.Collect(ch)
}

// Update updates our metrics.
func (m *TopologyAwareMetrics) Update() {
	if m == nil {
		return
	}

	p := m.p
	for _, pool := range p.pools {
		log.Debug("updating metrics for pool %s...", pool.Name())

		var (
			zone       = m.Zones[pool.Name()]
			free       = pool.FreeSupply().(*supply)
			mems       = libmem.NewNodeMask(pool.GetMemset(memoryAll).Members()...)
			sharedPool = free.SharableCPUs().Union(free.ReservedCPUs())
			containers = 0
			sharedctrs = 0
		)

		if zone == nil {
			log.Error("metrics zone not found for pool %s", pool.Name())
			continue
		}

		for _, g := range p.allocations.grants {
			if g.GetCPUNode().Name() == pool.Name() {
				containers++
				if g.ReservedPortion() != 0 || g.CPUPortion() != 0 {
					sharedctrs++
				}
			}
		}

		zone.SharedPool = sharedPool
		zone.SharedAssigned = free.GrantedReserved() + free.GrantedShared()
		zone.SharedAvailable = free.AllocatableSharedCPU()
		zone.MemAssigned = p.memAllocator.ZoneUsage(mems)
		zone.MemAvailable = p.memAllocator.ZoneAvailable(mems)
		zone.ContainerCount = containers
		zone.SharedContainerCount = sharedctrs

		m.Metrics.cpuSharedCapacity.WithLabelValues(
			zone.Name,
			zone.SharedPool.String(),
		).Set(float64(zone.SharedPool.Size()))

		m.Metrics.cpuSharedAssigned.WithLabelValues(
			zone.Name,
			zone.SharedPool.String(),
		).Set(float64(zone.SharedAssigned) / 1000.0)

		m.Metrics.cpuSharedAvailable.WithLabelValues(
			zone.Name,
			zone.SharedPool.String(),
		).Set(float64(zone.SharedAvailable) / 1000.0)

		m.Metrics.memAssigned.WithLabelValues(
			zone.Name,
			zone.Mems.MemsetString(),
		).Set(float64(zone.MemAssigned))

		m.Metrics.memAvailable.WithLabelValues(
			zone.Name,
			zone.Mems.MemsetString(),
		).Set(float64(zone.MemAvailable))

		m.Metrics.containerCount.WithLabelValues(
			zone.Name,
		).Set(float64(zone.ContainerCount))

		m.Metrics.sharedContainerCount.WithLabelValues(
			zone.Name,
		).Set(float64(zone.SharedContainerCount))
	}
}
