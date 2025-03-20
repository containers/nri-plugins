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

package blockio

// Config provides runtime configuration for class based block I/O
// prioritization and throttling.
// +kubebuilder:object:generate=true
type Config struct {
	// Enable class based block I/O prioritization and throttling. When
	// enabled, policy implementations can adjust block I/O priority by
	// by assigning containers to block I/O priority classes.
	// +optional
	Enable bool `json:"enable,omitempty"`
	// usePodQoSAsDefaultClass controls whether a container's Pod QoS
	// class is used as its block I/O class, if this is otherwise unset.
	// +optional
	UsePodQoSAsDefaultClass bool `json:"usePodQoSAsDefaultClass,omitempty"`
}
