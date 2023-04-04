// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package cache

import (
	"testing"

	kubecm "k8s.io/kubernetes/pkg/kubelet/cm"
)

const (
	// anything below 2 millicpus will yield 0 as an estimate
	minNonZeroRequest = 2
	// check CPU request/limit estimate accuracy up to this many CPU cores
	maxCPU = (kubecm.MaxShares / kubecm.SharesPerCPU) * kubecm.MilliCPUToCPU
	// we expect our estimates to be within 1 millicpu from the real ones
	expectedAccuracy = 1
)

func TestCPURequestCalculationAccuracy(t *testing.T) {
	for request := 0; request < maxCPU; request++ {
		shares := MilliCPUToShares(request)
		estimate := SharesToMilliCPU(shares)

		diff := int64(request) - estimate
		if diff > expectedAccuracy || diff < -expectedAccuracy {
			if diff < 0 {
				diff = -diff
			}
			if request > minNonZeroRequest {
				t.Errorf("CPU request %v: estimate %v, unexpected inaccuracy %v > %v",
					request, estimate, diff, expectedAccuracy)
			} else {
				t.Logf("CPU request %v: estimate %v, inaccuracy %v > %v (OK, this was expected)",
					request, estimate, diff, expectedAccuracy)
			}
		}

		// fail if our estimates are not accurate for full CPUs worth of millicpus
		if (request%1000) == 0 && diff != 0 {
			t.Errorf("CPU request %v != estimate %v (diff %v)", request, estimate, diff)
		}
	}
}

func TestCPULimitCalculationAccuracy(t *testing.T) {
	for limit := int64(0); limit < int64(maxCPU); limit++ {
		quota, period := MilliCPUToQuota(limit)
		estimate := QuotaToMilliCPU(quota, period)

		diff := limit - estimate
		if diff > expectedAccuracy || diff < -expectedAccuracy {
			if diff < 0 {
				diff = -diff
			}
			if quota != kubecm.MinQuotaPeriod {
				t.Errorf("CPU limit %v: estimate %v, unexpected inaccuracy %v > %v",
					limit, estimate, diff, expectedAccuracy)
			} else {
				t.Logf("CPU limit %v: estimate %v, inaccuracy %v > %v (OK, this was expected)",
					limit, estimate, diff, expectedAccuracy)
			}
		}

		// fail if our estimates are not accurate for full CPUs worth of millicpus
		if (limit%1000) == 0 && diff != 0 {
			t.Errorf("CPU limit %v != estimate %v (diff %v)", limit, estimate, diff)
		}
	}
}
