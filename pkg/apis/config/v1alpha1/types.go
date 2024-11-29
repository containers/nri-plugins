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
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/balloons"
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/template"
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
)

// TopologyAwarePolicy represents the configuration for the topology-aware policy.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +genclient
type TopologyAwarePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TopologyAwarePolicySpec `json:"spec"`
	Status ConfigStatus            `json:"status,omitempty"`
}

// TopologyAwarePolicySpec describes a topology-aware policy.
type TopologyAwarePolicySpec struct {
	topologyaware.Config `json:",inline"`
	// +optional
	Control control.Config `json:"control,omitempty"`
	// +optional
	Log log.Config `json:"log,omitempty"`
	// +optional
	Instrumentation instrumentation.Config `json:"instrumentation,omitempty"`
	// +optional
	// +kubebuilder:default={"nodeResourceTopology": true }
	Agent AgentConfig `json:"agent,omitempty"`
}

// TopologyAwarePolicyList represents a list of TopologyAwarePolicies.
// +kubebuilder:object:root=true
type TopologyAwarePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []TopologyAwarePolicy `json:"items"`
}

// BalloonsPolicy represents the configuration for the balloons policy.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +genclient
type BalloonsPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BalloonsPolicySpec `json:"spec"`
	Status ConfigStatus       `json:"status,omitempty"`
}

// BalloonsPolicySpec describes a balloons policy.
type BalloonsPolicySpec struct {
	balloons.Config `json:",inline"`
	// +optional
	Control control.Config `json:"control,omitempty"`
	// +optional
	Log log.Config `json:"log,omitempty"`
	// +optional
	Instrumentation instrumentation.Config `json:"instrumentation,omitempty"`
	// +optional
	// +kubebuilder:default={"nodeResourceTopology": true }
	Agent AgentConfig `json:"agent,omitempty"`
}

// BalloonsPolicyList represents a list of BalloonsPolicies.
// +kubebuilder:object:root=true
type BalloonsPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []BalloonsPolicy `json:"items"`
}

// TemplatePolicy represents the configuration for the template policy.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +genclient
type TemplatePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TemplatePolicySpec `json:"spec"`
	Status ConfigStatus       `json:"status,omitempty"`
}

// TemplatePolicySpec describes a template policy.
type TemplatePolicySpec struct {
	template.Config `json:",inline"`
	// +optional
	Control control.Config `json:"control,omitempty"`
	// +optional
	Log log.Config `json:"log,omitempty"`
	// +optional
	Instrumentation instrumentation.Config `json:"instrumentation,omitempty"`
	// +optional
	// +kubebuilder:default={"nodeResourceTopology": true }
	Agent AgentConfig `json:"agent,omitempty"`
}

// TemplatePolicyList represents a list of TemplatePolicies.
// +kubebuilder:object:root=true
type TemplatePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []TemplatePolicy `json:"items"`
}

// ConfigStatus is the per-node status for a configuration resource.
type ConfigStatus struct {
	Nodes map[string]NodeStatus `json:"nodes"`
}

// NodeStatus is the configuration status for a single node.
type NodeStatus struct {
	// Status of activating the configuration on this node.
	// +kubebuilder:validation:Enum=Success;Failure
	Status string `json:"status"`
	// Generation is the generation the configuration this status was set for.
	Generation int64 `json:"generation"`
	// Error can provide further details of a configuration error.
	Error *string `json:"errors,omitempty"`
	// Timestamp of setting this status.
	Timestamp metav1.Time `json:"timestamp,omitempty"`
}

func init() {
	SchemeBuilder.Register(
		&TopologyAwarePolicy{}, &TopologyAwarePolicyList{},
		&BalloonsPolicy{}, &BalloonsPolicyList{},
		&TemplatePolicy{}, &TemplatePolicyList{},
	)
}
