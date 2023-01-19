/*
Copyright 2023 Intel Corporation

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	policyapi "github.com/intel/nri-resmgr/pkg/policy"
)

// UpdateNrtCR updates the node's node resource topology CR using the given data.
func (a *agent) UpdateNrtCR(policy string, zones []*policyapi.TopologyZone) error {
	a.Info("updating node resource topology CR")

	if a.nrtCli == nil {
		a.Warn("no node resource topology client, can't update CR")
		return nil
	}

	a.Info("*** should export policy %s node resource topology CR for node %s...",
		policy, nodeName)
}
