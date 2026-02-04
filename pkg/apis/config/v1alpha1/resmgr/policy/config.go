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
	"fmt"
	"strings"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	"k8s.io/apimachinery/pkg/api/resource"

	nriapi "github.com/containerd/nri/pkg/api"
)

type (
	Constraints               map[Domain]Amount
	Domain                    string
	Amount                    string
	AmountKind                int
	CPUTopologyLevel          string
	ComponentCreationStrategy string
	SchedulingPolicy          string
	SchedulingFlag            string
	SchedulingFlags           []SchedulingFlag
	IOPriorityClass           string
)

const (
	CPU    Domain = "cpu"
	Memory Domain = "memory"

	AmountAbsent AmountKind = iota
	AmountQuantity
	AmountCPUSet

	PrefixCPUSet = "cpuset:"

	CPUTopologyLevelUndefined CPUTopologyLevel = ""
	CPUTopologyLevelSystem    CPUTopologyLevel = "system"
	CPUTopologyLevelPackage   CPUTopologyLevel = "package"
	CPUTopologyLevelDie       CPUTopologyLevel = "die"
	CPUTopologyLevelNuma      CPUTopologyLevel = "numa"
	CPUTopologyLevelL2Cache   CPUTopologyLevel = "l2cache"
	CPUTopologyLevelCore      CPUTopologyLevel = "core"
	CPUTopologyLevelThread    CPUTopologyLevel = "thread"

	ComponentCreationAll             ComponentCreationStrategy = "all"
	ComponentCreationBalanceBalloons ComponentCreationStrategy = "balance-balloons"

	// Scheduling policy and flag user-facing strings are formed from
	// SCHED_* and SCHED_FLAG_* by stripping prefix, lowercase and s/_/-/g.
	SchedulingPolicyUndefined  SchedulingPolicy = ""
	SchedulingPolicyNone       SchedulingPolicy = "none"
	SchedulingPolicyOther      SchedulingPolicy = "other"
	SchedulingPolicyFifo       SchedulingPolicy = "fifo"
	SchedulingPolicyRr         SchedulingPolicy = "rr"
	SchedulingPolicyBatch      SchedulingPolicy = "batch"
	SchedulingPolicyIdle       SchedulingPolicy = "idle"
	SchedulingPolicyDeadline   SchedulingPolicy = "deadline"
	SchedulingFlagResetOnFork  SchedulingFlag   = "reset-on-fork"
	SchedulingFlagReclaimable  SchedulingFlag   = "reclaimable"
	SchedulingFlagDlOverrun    SchedulingFlag   = "dl-overrun"
	SchedulingFlagKeepPolicy   SchedulingFlag   = "keep-policy"
	SchedulingFlagKeepParams   SchedulingFlag   = "keep-params"
	SchedulingFlagUtilClampMin SchedulingFlag   = "util-clamp-min"
	SchedulingFlagUtilClampMax SchedulingFlag   = "util-clamp-max"

	// IO priority classes are constructed from the
	// IOPRIO_CLASS_* constants by stripping prefix and lowercase.
	IOPriorityClassUndefined IOPriorityClass = ""
	IOPriorityClassNone      IOPriorityClass = "none"
	IOPriorityClassRt        IOPriorityClass = "rt"
	IOPriorityClassBe        IOPriorityClass = "be"
	IOPriorityClassIdle      IOPriorityClass = "idle"
)

var (
	noQ = resource.Quantity{}
)

func (c Constraints) Get(d Domain) (Amount, AmountKind) {
	amount, ok := c[d]
	if !ok {
		return "", AmountAbsent
	}

	a := string(amount)
	switch {
	case strings.HasPrefix(a, PrefixCPUSet):
		return Amount(strings.TrimPrefix(a, PrefixCPUSet)), AmountCPUSet
	default:
		return amount, AmountQuantity
	}
}

func (amount Amount) ParseCPUSet() (cpuset.CPUSet, error) {
	cset, err := cpuset.Parse(string(amount))
	if err != nil {
		return cset, fmt.Errorf("failed to parse amount '%s' as cpuset: %w", amount, err)
	}
	return cset, nil
}

func (amount Amount) ParseQuantity() (resource.Quantity, error) {
	q, err := resource.ParseQuantity(string(amount))
	if err != nil {
		return noQ, fmt.Errorf("failed to parse amount '%s' as resource quantity: %w", amount, err)
	}
	return q, nil
}

var (
	cpuTopologyLevelValues = map[CPUTopologyLevel]int{
		CPUTopologyLevelUndefined: 0,
		CPUTopologyLevelSystem:    1,
		CPUTopologyLevelPackage:   2,
		CPUTopologyLevelDie:       3,
		CPUTopologyLevelNuma:      4,
		CPUTopologyLevelL2Cache:   5,
		CPUTopologyLevelCore:      6,
		CPUTopologyLevelThread:    7,
	}

	CPUTopologyLevelCount = len(cpuTopologyLevelValues)
)

func (l CPUTopologyLevel) String() string {
	return string(l)
}

func (l CPUTopologyLevel) Value() int {
	if i, ok := cpuTopologyLevelValues[l]; ok {
		return i
	}
	return cpuTopologyLevelValues[CPUTopologyLevelUndefined]
}

func (sp SchedulingPolicy) String() string {
	return string(sp)
}

func (sp SchedulingPolicy) ToNRI() (nriapi.LinuxSchedulerPolicy, error) {
	cstyleSp := "SCHED_" + strings.ToUpper(strings.ReplaceAll(string(sp), "-", "_"))
	n, ok := nriapi.LinuxSchedulerPolicy_value[cstyleSp]
	if !ok {
		return 0, fmt.Errorf("unknown scheduling policy '%s'", sp)
	}
	return nriapi.LinuxSchedulerPolicy(n), nil
}

func (sf SchedulingFlag) String() string {
	return string(sf)
}

func (sf SchedulingFlag) ToNRI() (nriapi.LinuxSchedulerFlag, error) {
	cstyleSf := "SCHED_FLAG_" + strings.ToUpper(strings.ReplaceAll(string(sf), "-", "_"))
	n, ok := nriapi.LinuxSchedulerFlag_value[cstyleSf]
	if !ok {
		return 0, fmt.Errorf("unknown scheduling flag '%s'", sf)
	}
	return nriapi.LinuxSchedulerFlag(n), nil
}

func (sfl SchedulingFlags) ToNRI() ([]nriapi.LinuxSchedulerFlag, error) {
	var nriFlags []nriapi.LinuxSchedulerFlag
	for _, sf := range sfl {
		nriFlag, err := sf.ToNRI()
		if err != nil {
			return nil, err
		}
		nriFlags = append(nriFlags, nriFlag)
	}
	return nriFlags, nil
}

func (ioc IOPriorityClass) String() string {
	return string(ioc)
}

func (ioc IOPriorityClass) ToNRI() (nriapi.IOPrioClass, error) {
	cstyleIoc := "IOPRIO_CLASS_" + strings.ToUpper(strings.ReplaceAll(string(ioc), "-", "_"))
	n, ok := nriapi.IOPrioClass_value[cstyleIoc]
	if !ok {
		return 0, fmt.Errorf("unknown IO priority class '%s'", ioc)
	}
	return nriapi.IOPrioClass(n), nil
}
