// Copyright 2022 Intel Corporation. All Rights Reserved.
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

package balloons

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/containers/nri-plugins/pkg/metrics"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// Metrics defines the balloons-specific metric instruments.
type Meters struct {
	p       *balloons
	meter   metric.Meter
	balloon metric.Int64ObservableGauge
}

// BalloonMetrics define metrics of a balloon instance.
type BalloonMetrics struct {
	// Balloon type metrics
	DefName  string
	CpuClass string
	MinCpus  int
	MaxCpus  int
	// Balloon instance metrics
	PrettyName            string
	Groups                string
	Cpus                  cpuset.CPUSet
	CpusCount             int
	Numas                 []string
	NumasCount            int
	Dies                  []string
	DiesCount             int
	Packages              []string
	PackagesCount         int
	SharedIdleCpus        cpuset.CPUSet
	SharedIdleCpusCount   int
	CpusAllowed           cpuset.CPUSet
	CpusAllowedCount      int
	Mems                  string
	ContainerNames        string
	ContainerReqMilliCpus int
}

func (b *balloons) BlockMeters() {
	b.meterLock.RLock()
}

func (b *balloons) UnblockMeters() {
	b.meterLock.RUnlock()
}

func (b *balloons) RequestMeters() {
	b.meterLock.Lock()
}

func (b *balloons) ReleaseMeters() {
	b.meterLock.Unlock()
}

func (b *balloons) NewMeters() {
	var (
		meter = metrics.Provider("policy").Meter("balloons", metrics.WithOmitSubsystem())
		m     = &Meters{
			p:     b,
			meter: meter,
		}
		err error
	)

	m.balloon, err = m.meter.Int64ObservableGauge(
		"balloons",
		metric.WithDescription("CPUs"),
		metric.WithUnit("cores"),
	)

	if err != nil {
		log.Errorf("failed to create balloons meter: %v", err)
		return
	}

	_, err = m.meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			for _, bln := range b.balloons {
				select {
				case <-ctx.Done():
					log.Errorf("balloon metric collection cancelled: %v", ctx.Err())
				default:
					m.Observe(o, bln)
				}
			}
			return nil
		},
		m.balloon,
	)

	if err != nil {
		log.Errorf("failed to register balloons callback: %v", err)
	}

	b.meters = m
}

func (m *Meters) Observe(o metric.Observer, bln *Balloon) {
	m.p.RequestMeters()
	defer m.p.ReleaseMeters()

	var (
		defName               = bln.Def.Name
		prettyName            = bln.PrettyName()
		cpuClass              = bln.Def.CpuClass
		minCpus               = bln.Def.MinCpus
		maxCpus               = bln.Def.MaxCpus
		cpuLoc                = m.p.cpuTree.CpuLocations(bln.Cpus)
		cpus                  = bln.Cpus
		cpusCount             = cpus.Size()
		numas                 []string
		dies                  []string
		packages              []string
		numasCount            = 0
		diesCount             = 0
		packagesCount         = 0
		sharedIdleCpus        = bln.SharedIdleCpus
		sharedIdleCpusCount   = sharedIdleCpus.Size()
		cpusAllowed           = cpus.Union(sharedIdleCpus)
		cpusAllowedCount      = cpusAllowed.Size()
		mems                  = bln.Mems.String()
		containerNames        string
		containerReqMilliCpus = 0
		groups                strings.Builder
	)

	sep := ""
	for group, cCount := range bln.Groups {
		if cCount > 0 {
			groups.WriteString(sep)
			groups.WriteString(group)
			sep = ","
		}
	}

	if len(cpuLoc) > 3 {
		numas = cpuLoc[3]
		numasCount = len(numas)
		dies = cpuLoc[2]
		diesCount = len(dies)
		packages = cpuLoc[1]
		packagesCount = len(packages)
	}

	cNames := []string{}
	// Get container names and total requested milliCPUs.
	for _, containerIDs := range bln.PodIDs {
		for _, containerID := range containerIDs {
			if c, ok := m.p.cch.LookupContainer(containerID); ok {
				cNames = append(cNames, c.PrettyName())
				containerReqMilliCpus += m.p.containerRequestedMilliCpus(containerID)
			}
		}
	}
	sort.Strings(cNames)
	containerNames = strings.Join(cNames, ",")

	o.ObserveInt64(
		m.balloon,
		int64(cpus.Size()),
		metric.WithAttributes(
			attribute.String("balloon_type", defName),
			attribute.String("cpu_class", cpuClass),
			attribute.String("cpus_min", strconv.Itoa(minCpus)),
			attribute.String("cpus_max", strconv.Itoa(maxCpus)),
			attribute.String("balloon", prettyName),
			attribute.String("groups", groups.String()),
			attribute.String("cpus", cpus.String()),
			attribute.String("cpus_count", strconv.Itoa(cpusCount)),
			attribute.String("numas", strings.Join(numas, ",")),
			attribute.String("numas_count", strconv.Itoa(numasCount)),
			attribute.String("dies", strings.Join(dies, ",")),
			attribute.String("dies_count", strconv.Itoa(diesCount)),
			attribute.String("packages", strings.Join(packages, ",")),
			attribute.String("packages_count", strconv.Itoa(packagesCount)),
			attribute.String("sharedidlecpus", sharedIdleCpus.String()),
			attribute.String("sharedidlecpus_count", strconv.Itoa(sharedIdleCpusCount)),
			attribute.String("cpus_allowed", cpusAllowed.String()),
			attribute.String("cpus_allowed_count", strconv.Itoa(cpusAllowedCount)),
			attribute.String("mems", mems),
			attribute.String("containers", containerNames),
			attribute.String("tot_req_millicpu", strconv.Itoa(containerReqMilliCpus)),
		),
	)
}
