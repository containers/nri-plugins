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
	"context"
	"fmt"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/containers/nri-plugins/pkg/metrics"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

type TopologyAwareMetrics struct {
	p                    *policy
	ZoneNames            []string
	Zones                map[string]*Zone
	zone                 metric.Int64Gauge
	cpuSharedCapacity    metric.Int64Gauge
	cpuSharedAssigned    metric.Float64Gauge
	cpuSharedAvailable   metric.Float64Gauge
	memCapacity          metric.Int64Gauge
	memAssigned          metric.Int64Gauge
	memAvailable         metric.Int64Gauge
	containerCount       metric.Int64Gauge
	sharedContainerCount metric.Int64Gauge
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

const (
	metricsSubsystem = "topologyaware"
)

func (p *policy) NewTopologyAwareMetrics() (*TopologyAwareMetrics, error) {
	var (
		meter = metrics.Provider("policy").Meter(metricsSubsystem)
		m     = &TopologyAwareMetrics{
			p:     p,
			Zones: make(map[string]*Zone),
		}
		err error
	)

	m.zone, err = meter.Int64Gauge(
		"zone.cpu.capacity",
		metric.WithDescription("A topology zone of CPUs."),
		metric.WithUnit("cores"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone.cpu.capacity meter: %w", err)
	}

	m.cpuSharedCapacity, err = meter.Int64Gauge(
		"zone.cpu.shared.capacity",
		metric.WithDescription("Capacity of shared CPU pool of a topology zone."),
		metric.WithUnit("cores"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone.cpu.shared.capacity meter: %w", err)
	}

	m.cpuSharedAssigned, err = meter.Float64Gauge(
		"zone.cpu.shared.assigned",
		metric.WithDescription("Assigned amount of shared CPU pool of a topology zone."),
		metric.WithUnit("cores"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone.cpu.shared.assigned meter: %w", err)
	}

	m.cpuSharedAvailable, err = meter.Float64Gauge(
		"zone.cpu.shared.available",
		metric.WithDescription("Available amount of shared CPU pool of a topology zone."),
		metric.WithUnit("cores"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone.cpu.shared.available meter: %w", err)
	}

	m.memCapacity, err = meter.Int64Gauge(
		"zone.mem.capacity",
		metric.WithDescription("Memory capacity of a topology zone."),
		metric.WithUnit("bytes"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone.mem.capacity meter: %w", err)
	}

	m.memAssigned, err = meter.Int64Gauge(
		"zone.mem.assigned",
		metric.WithDescription("Amount of assigned memory of a topology zone."),
		metric.WithUnit("bytes"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone.mem.assigned meter: %w", err)
	}

	m.memAvailable, err = meter.Int64Gauge(
		"zone.mem.available",
		metric.WithDescription("Amount of available memory of a topology zone."),
		metric.WithUnit("bytes"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone.mem.available meter: %w", err)
	}

	m.containerCount, err = meter.Int64Gauge(
		"zone.container.count",
		metric.WithDescription("Number of containers assigned to a topology zone."),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone.container.count meter: %w", err)
	}

	m.sharedContainerCount, err = meter.Int64Gauge(
		"zone.shared.container.count",
		metric.WithDescription("Number of containers in the shared CPU pool of a topology zone."),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone.shared.container.count meter: %w", err)
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

		m.zone.Record(
			context.Background(),
			int64(zone.Cpus.Size()),
			metric.WithAttributes(
				attribute.String("zone", zone.Name),
				attribute.String("cpus", zone.Cpus.String()),
				attribute.String("mems", zone.Mems.String()),
			),
		)

		m.memCapacity.Record(
			context.Background(),
			zone.MemCapacity,
			metric.WithAttributes(
				attribute.String("zone", zone.Name),
				attribute.String("mems", zone.Mems.String()),
			),
		)
	}

	slices.SortFunc(m.ZoneNames, func(a, b string) int {
		poolA, poolB := p.nodes[a], p.nodes[b]
		if diff := poolA.RootDistance() - poolB.RootDistance(); diff != 0 {
			return diff
		}
		return strings.Compare(a, b)
	})

	m.Update()

	return m, nil
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

		m.cpuSharedCapacity.Record(
			context.Background(),
			int64(zone.SharedPool.Size()),
			metric.WithAttributes(
				attribute.String("zone", zone.Name),
				attribute.String("cpus", zone.SharedPool.String()),
			),
		)

		m.cpuSharedAssigned.Record(
			context.Background(),
			float64(zone.SharedAssigned)/1000.0,
			metric.WithAttributes(
				attribute.String("zone", zone.Name),
				attribute.String("cpus", zone.SharedPool.String()),
			),
		)

		m.cpuSharedAvailable.Record(
			context.Background(),
			float64(zone.SharedAvailable)/1000.0,
			metric.WithAttributes(
				attribute.String("zone", zone.Name),
				attribute.String("cpus", zone.SharedPool.String()),
			),
		)

		m.memAssigned.Record(
			context.Background(),
			zone.MemAssigned,
			metric.WithAttributes(
				attribute.String("zone", zone.Name),
				attribute.String("mems", zone.Mems.MemsetString()),
			),
		)

		m.memAvailable.Record(
			context.Background(),
			zone.MemAvailable,
			metric.WithAttributes(
				attribute.String("zone", zone.Name),
				attribute.String("mems", zone.Mems.MemsetString()),
			),
		)

		m.containerCount.Record(
			context.Background(),
			int64(zone.ContainerCount),
			metric.WithAttributes(
				attribute.String("zone", zone.Name),
			),
		)

		m.sharedContainerCount.Record(
			context.Background(),
			int64(zone.SharedContainerCount),
			metric.WithAttributes(
				attribute.String("zone", zone.Name),
			),
		)
	}
}
