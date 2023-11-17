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
)

type Constraints map[Domain]Amount
type Domain string
type Amount string
type AmountKind int

const (
	CPU    Domain = "cpu"
	Memory Domain = "memory"

	AmountAbsent AmountKind = iota
	AmountQuantity
	AmountCPUSet

	PrefixCPUSet = "cpuset:"
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
