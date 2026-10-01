// Copyright 2026 Intel Corporation. All Rights Reserved.
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

package topologyaware

import (
	"strconv"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
)

// A refused reconfiguration leaves the configuration in effect as it was.
func TestReconfigureRefusedKeepsConfig(t *testing.T) {
	tcs := []struct {
		name string
		// grant gives a container every sharable CPU before reconfiguring.
		grant   bool
		cfg     *cfgapi.Config
		refusal string
	}{
		{
			name:    "refused by initialize",
			cfg:     &cfgapi.Config{DefaultCPUPriority: cfgapi.PriorityLow},
			refusal: "without CPU reservation",
		},
		{
			name:  "refused by restoring the allocations",
			grant: true,
			cfg: &cfgapi.Config{
				ReservedResources:  cfgapi.Constraints{cfgapi.CPU: "8"},
				DefaultCPUPriority: cfgapi.PriorityLow,
			},
			refusal: "failed to allocate",
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			p, dir := setupTestPolicy(t)
			defer removeAll(t, dir)

			if tc.grant {
				cpus := resource.MustParse(strconv.Itoa(p.root.GetSupply().SharableCPUs().Size()))
				c := &mockContainer{
					name:                "c",
					returnValueForGetID: "c",
					returnValueForGetResourceRequirements: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: cpus},
						Limits:   v1.ResourceList{v1.ResourceCPU: cpus},
					},
				}
				grant, err := p.allocatePool(c, "")
				if err != nil {
					t.Fatalf("failed to allocate a grant: %v", err)
				}
				p.applyGrant(grant)
			}

			cfg, prio := opt, defaultPrio

			err := p.Reconfigure(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("got error %v reconfiguring, expected a refusal with %q", err, tc.refusal)
			}

			if opt != cfg || p.cfg != cfg {
				t.Errorf("configuration is %+v after a refused reconfiguration, expected %+v", opt, cfg)
			}
			if defaultPrio != prio {
				t.Errorf("default CPU priority is %s after a refused reconfiguration, expected %s", defaultPrio, prio)
			}
		})
	}
}
