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

package kubernetes

import (
	"fmt"

	"github.com/containers/nri-plugins/pkg/sysfs"
)

const (
	// Constants for converting back and forth between CPU requirements in
	// terms of milli-CPUs and kernel cgroup/scheduling parameters.

	// MinShares is the minimum cpu.shares accepted by cgroups.
	MinShares = 2
	// MaxShares is the minimum cpu.shares accepted by cgroups.
	MaxShares = 262144
	// SharesPerCPU is cpu.shares worth one full CPU.
	SharesPerCPU = 1024
	// MilliCPUToCPU is milli-CPUs worth a full CPU.
	MilliCPUToCPU = 1000
	// QuotaPeriod is 100000 microseconds, or 100ms
	QuotaPeriod = 100000
	// MinQuotaPeriod is 1000 microseconds, or 1ms
	MinQuotaPeriod = 1000

	GuaranteedOOMScoreAdj = -997
	BestEffortOOMScoreAdj = 1000

	MinBurstableOOMScoreAdj = 1000 + GuaranteedOOMScoreAdj // 1000 - 997 = 3
	MaxBurstableOOMScoreAdj = BestEffortOOMScoreAdj - 1    // 1000 - 1 = 999

)

var (
	// memCapacity is the total memory capacity of the node.
	memCapacity int64
	// oomAdjToMemReqEstimates is a table of memory request estimates for OOM score adjustments.
	oomAdjToMemReqEstimates map[int64]int64
)

// MilliCPUToQuota converts milliCPU to CFS quota and period values.
// (Almost) identical to the same function in kubelet.
func MilliCPUToQuota(milliCPU int64) (quota, period int64) {
	if milliCPU == 0 {
		return 0, 0
	}

	// TODO(klihub): this is behind the CPUSFSQuotaPerdiod feature gate in kubelet
	period = int64(QuotaPeriod)

	quota = (milliCPU * period) / MilliCPUToCPU

	if quota < MinQuotaPeriod {
		quota = MinQuotaPeriod
	}

	return quota, period
}

// MilliCPUToShares converts the milliCPU to CFS shares.
// Identical to the same function in kubelet.
func MilliCPUToShares(milliCPU int64) uint64 {
	if milliCPU == 0 {
		return MinShares
	}
	shares := (milliCPU * SharesPerCPU) / MilliCPUToCPU
	if shares < MinShares {
		return MinShares
	}
	if shares > MaxShares {
		return MaxShares
	}
	return uint64(shares)
}

// SharesToMilliCPU converts CFS CPU shares to milli-CPUs.
func SharesToMilliCPU(shares int64) int64 {
	if shares == MinShares {
		return 0
	}
	return int64(float64(shares*MilliCPUToCPU)/float64(SharesPerCPU) + 0.5)
}

// QuotaToMilliCPU converts CFS quota and period to milli-CPUs.
func QuotaToMilliCPU(quota, period int64) int64 {
	if quota == 0 || period == 0 {
		return 0
	}
	return int64(float64(quota*MilliCPUToCPU)/float64(period) + 0.5)
}

// MemReqToOomAdj estimates OOM score adjustment based on memory request.
func MemReqToOomAdj(memRequest int64) int64 {
	return 1000 - (1000*memRequest)/memCapacity
}

// OomAdjToMemReq estimates memory request based on OOM score adjustment.
func OomAdjToMemReq(oomAdj int64, memLimit int64) *int64 {
	if oomAdj < MinBurstableOOMScoreAdj || oomAdj > MaxBurstableOOMScoreAdj {
		return nil
	}

	if req := oomAdjToMemReqEstimates[oomAdj]; req < memLimit || memLimit == 0 {
		return &req
	}

	return nil
}

// CalculateOomAdjToMemReqEstimates calculates a table of memory request estimates
// for OOM score adjustments in the range [0, 1000].
func CalculateOomAdjToMemReqEstimates() map[int64]int64 {
	var (
		milliMem = float64(memCapacity) / 1000.0
		adjToReq = map[int64]int64{
			1000: 0,
			0:    memCapacity,
		}
		iToReq = map[int]int64{
			0:    0,
			1000: memCapacity,
		}
	)

	for i := 1; i < 1000; i++ {
		var (
			prevReq = iToReq[i-1]
			prevAdj = MemReqToOomAdj(prevReq)
			currReq = int64(float64(prevReq) + milliMem + 0.5)
			currAdj = MemReqToOomAdj(currReq)
		)

		switch {
		case currAdj < prevAdj:
			// find smallest request which gives one adjustment smaller than the previous
			for j := 0; currAdj < prevAdj && j < int(milliMem); j++ {
				if currAdj == prevAdj-1 {
					adjToReq[currAdj] = currReq
					iToReq[i] = currReq
				}
				currReq--
				currAdj = MemReqToOomAdj(currReq)
			}
		case currAdj == prevAdj:
			// find first request which gives one adjustment smaller than the previous
			for j := 0; currAdj == prevAdj && j < int(milliMem); j++ {
				currReq++
				currAdj = MemReqToOomAdj(currReq)
			}
			if currAdj == prevAdj-1 {
				adjToReq[currAdj] = currReq
				iToReq[i] = currReq
				break
			}

			fallthrough
		default:
			panic(
				fmt.Sprintf("capacity %d: failed to calculate request estimate #%d",
					memCapacity, i),
			)
		}
	}

	return adjToReq
}

// Set memory capacity for OOM adjustment to memory request estimation.
// Exported to allow testing the estimator code with different memory
// capacities.
func SetMemoryCapacity(capacity int64) {
	if capacity == 0 {
		panic(fmt.Errorf("failed to set memory capacity, invalid capacity 0"))
	}

	memCapacity = capacity
	oomAdjToMemReqEstimates = CalculateOomAdjToMemReqEstimates()
}

func GetMemoryCapacity() int64 {
	return memCapacity
}

func init() {
	SetMemoryCapacity(sysfs.GetMemoryCapacity())
}
