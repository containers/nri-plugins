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

package log

import (
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log/klogcontrol"
)

// +k8s:deepcopy-gen=true
type Config struct {
	// Debub turns on debug messages matching listed logger sources.
	// +optional
	Debug []string `json:"debug,omitempty"`
	// Source controls whether messages are prefixed with their logger source.
	// +optional
	LogSource bool `json:"source,omitempty"`
	// Klog configures the klog backend.
	// +optional
	Klog klogcontrol.Config `json:"klog,omitempty"`
}
