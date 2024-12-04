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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/instrumentation"
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log"
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control"
)

// ResmgrConfig provides access to policy-specific and common
// configuration data. All resource management configuration
// types must implement this interface. The resource manager
// uses it to pass configuration to the policy implementation.
// +kubebuilder:object:generate=false
type ResmgrConfig interface {
	metav1.ObjectMetaAccessor
	AgentConfig() *AgentConfig
	CommonConfig() *CommonConfig
	PolicyConfig() interface{}
}

type CommonConfig struct {
	// +optional
	Control control.Config `json:"control,omitempty"`
	// +optional
	Log log.Config `json:"log,omitempty"`
	// +optional
	Instrumentation instrumentation.Config `json:"instrumentation,omitempty"`
}
