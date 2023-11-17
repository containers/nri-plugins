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
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// StatusSuccess indicates that a configuration was taken into use.
	StatusSuccess = metav1.StatusSuccess
	// StatusFailure indicates failure to take a configuration into use.
	StatusFailure = metav1.StatusFailure
)

// NewNodeStatus create a node status for the given generation and error.
func NewNodeStatus(err error, generation int64) *NodeStatus {
	s := &NodeStatus{
		Generation: generation,
		Timestamp:  metav1.Now(),
	}
	if err == nil {
		s.Status = StatusSuccess
		// TODO(klihub): 'Patch away' any old errors from lingering. I don't
		//     know if there is a nicer way of doing this with Patch().
		e := ""
		s.Error = &e
	} else {
		s.Status = StatusFailure
		e := fmt.Sprintf("%v", err)
		s.Error = &e
	}
	return s
}

// NodeStatusPatch creates a (MergePatch) for the given node status.
func NodeStatusPatch(node string, status *NodeStatus) ([]byte, types.PatchType, error) {
	cfg := &patchConfig{
		Status: patchStatus{
			Nodes: map[string]*NodeStatus{
				node: status,
			},
		},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, types.PatchType(""), fmt.Errorf("failed to marshal patch: %v", err)
	}

	return data, types.MergePatchType, nil
}

type patchConfig struct {
	Status patchStatus `json:"status,omitempty"`
}

type patchStatus struct {
	Nodes map[string]*NodeStatus `json:"nodes,omitempty"`
}
