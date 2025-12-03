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

package policy

import (
	"context"
	"fmt"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	v1 "k8s.io/api/core/v1"

	"github.com/containers/nri-plugins/pkg/metrics"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

type (
	SystemCollector struct {
		cache          cache.Cache
		system         system.System
		Nodes          map[int]*NodeMetric
		Cpus           map[int]*CpuMetric
		NodeCapacity   metric.Int64Gauge
		NodeUsage      metric.Int64Gauge
		NodeContainers metric.Int64Gauge
		CpuAllocation  metric.Int64Gauge
		CpuContainers  metric.Int64Gauge
	}
	NodeMetric struct {
		Id             int
		IdLabel        string
		Type           string
		Capacity       int64
		Usage          int64
		ContainerCount int
	}
	CpuMetric struct {
		Id             int
		IdLabel        string
		Allocation     int
		ContainerCount int
	}
)

func (p *policy) newSystemCollector() (*SystemCollector, error) {
	var (
		meter = metrics.Provider("policy").Meter("system", metrics.WithOmitSubsystem())
		s     = &SystemCollector{
			cache:  p.cache,
			system: p.system,
			Nodes:  map[int]*NodeMetric{},
			Cpus:   map[int]*CpuMetric{},
		}
		err error
	)

	s.NodeCapacity, err = meter.Int64Gauge(
		"mem.node.capacity",
		metric.WithDescription("Capacity of the memory node."),
		metric.WithUnit("bytes"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create mem.node.capacity meter: %w", err)
	}

	s.NodeUsage, err = meter.Int64Gauge(
		"mem.node.usage",
		metric.WithDescription("Usage of the memory node."),
		metric.WithUnit("bytes"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create mem.node.usage meter: %w", err)
	}

	s.NodeContainers, err = meter.Int64Gauge(
		"mem.node.container.count",
		metric.WithDescription("Number of containers assigned to the memory node."),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create mem.node.container.count meter: %w", err)
	}

	s.CpuAllocation, err = meter.Int64Gauge(
		"cpu.allocation",
		metric.WithDescription("Total allocation of the CPU."),
		metric.WithUnit("milli-cores"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create cpu.allocation meter: %w", err)
	}

	s.CpuContainers, err = meter.Int64Gauge(
		"cpu.container.count",
		metric.WithDescription("Number of containers assigned to the CPU."),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create cpu.container.count meter: %w", err)
	}

	for _, id := range s.system.NodeIDs() {
		var (
			sys        = s.system.Node(id)
			capa, used = s.getMemInfo(sys)
			node       = &NodeMetric{
				Id:       sys.ID(),
				IdLabel:  strconv.Itoa(sys.ID()),
				Type:     sys.GetMemoryType().String(),
				Capacity: capa,
				Usage:    used,
			}
		)
		s.Nodes[id] = node

		s.NodeCapacity.Record(
			context.Background(),
			node.Capacity,
			metric.WithAttributes(
				attribute.String("node.id", node.IdLabel),
				attribute.String("node.type", node.Type),
			),
		)
	}

	for _, id := range s.system.CPUIDs() {
		cpu := &CpuMetric{
			Id:      id,
			IdLabel: strconv.Itoa(id),
		}
		s.Cpus[id] = cpu
	}

	s.Update()

	return s, nil
}

func (s *SystemCollector) Update() {
	if s == nil {
		return
	}

	for _, n := range s.Nodes {
		sys := s.system.Node(n.Id)
		_, used := s.getMemInfo(sys)
		n.Usage = used
		n.ContainerCount = 0
	}

	for _, c := range s.Cpus {
		c.ContainerCount = 0
		c.Allocation = 0
	}

	for _, ctr := range s.cache.GetContainers() {
		switch ctr.GetState() {
		case cache.ContainerStateCreated:
		case cache.ContainerStateRunning:
		default:
			continue
		}

		var (
			cpu, mem = s.getCpuAndMemset(ctr)
			req, _   = s.getCpuResources(ctr)
		)

		for _, id := range mem.List() {
			if n, ok := s.Nodes[id]; ok {
				n.ContainerCount++
			}
		}

		for _, id := range cpu.List() {
			if c, ok := s.Cpus[id]; ok {
				c.ContainerCount++
				if cpu.Size() > 0 {
					c.Allocation += req / cpu.Size()
				}
			}
		}
	}

	for _, n := range s.Nodes {
		s.NodeUsage.Record(
			context.Background(),
			n.Usage,
			metric.WithAttributes(
				attribute.String("node.id", n.IdLabel),
			),
		)
		s.NodeContainers.Record(
			context.Background(),
			int64(n.ContainerCount),
			metric.WithAttributes(
				attribute.String("node.id", n.IdLabel),
			),
		)
	}
	for _, c := range s.Cpus {
		s.CpuAllocation.Record(
			context.Background(),
			int64(c.Allocation),
			metric.WithAttributes(
				attribute.String("cpu.id", c.IdLabel),
			),
		)
		s.CpuContainers.Record(
			context.Background(),
			int64(c.ContainerCount),
			metric.WithAttributes(
				attribute.String("cpu.id", c.IdLabel),
			),
		)
	}
}

func (s *SystemCollector) getMemInfo(n system.Node) (capacity, used int64) {
	if n != nil {
		if i, _ := n.MemoryInfo(); i != nil {
			return int64(i.MemTotal), int64(i.MemUsed)
		}
	}
	return 0, 0
}

func (s *SystemCollector) getCpuAndMemset(ctr cache.Container) (cpu, mem cpuset.CPUSet) {
	cset, _ := cpuset.Parse(ctr.GetCpusetCpus())
	mset, _ := cpuset.Parse(ctr.GetCpusetMems())
	return cset, mset
}

func (s *SystemCollector) getCpuResources(ctr cache.Container) (request, limit int) {
	res := ctr.GetResourceRequirements()
	if qty, ok := res.Requests[v1.ResourceCPU]; ok {
		request = int(qty.MilliValue())
	}
	if qty, ok := res.Limits[v1.ResourceCPU]; ok {
		limit = int(qty.MilliValue())
	}

	return request, limit
}
