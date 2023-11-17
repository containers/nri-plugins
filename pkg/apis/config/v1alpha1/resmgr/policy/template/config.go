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

package template

import (
	policy "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
)

type (
	Constraints = policy.Constraints
	Domain      = policy.Domain
	Amount      = policy.Amount
	AmountKind  = policy.AmountKind
)

const (
	CPU            = policy.CPU
	Memory         = policy.Memory
	AmountAbsent   = policy.AmountAbsent
	AmountQuantity = policy.AmountQuantity
	AmountCPUSet   = policy.AmountCPUSet
)

// +k8s:deepcopy-gen=true
// +optional
type Config struct {
	AvailableResources Constraints `json:"availableResources,omitempty"`
	// +kubebuilder:validation:Required
	ReservedResources Constraints `json:"reservedResources"`
}
