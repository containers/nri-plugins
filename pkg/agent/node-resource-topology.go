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

package agent

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	nrt "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

// UpdateNrtCR updates the node's node resource topology CR using the given data.
func (a *Agent) UpdateNrtCR(policy string, zones []*policyapi.TopologyZone) error {
	if a.hasLocalConfig() {
		return nil
	}

	if a.nrtCli == nil {
		return fmt.Errorf("no node resource topology client, can't update CR")
	}

	log.Info("updating node resource topology CR")

	// To minimize the risk of an NRI request timeout (and the plugin getting
	// kicked out) we do the update asynchronously. We can rework this to use
	// a single goroutine that reads update requests from a channel to mimic
	// the rest if necessary.
	// XXX TODO(klihub): We can't/don't propagate update errors now back
	//     to the caller. We could do that (using a channel) if necessary...
	go func() {
		if err := a.updateNrtCR(policy, zones); err != nil {
			log.Errorf("failed to update topology CR: %v", err)
		}
	}()

	return nil
}

// updateNrtCR updates the node's node resource topology CR using the given data.
func (a *Agent) updateNrtCR(policy string, zones []*policyapi.TopologyZone) error {
	a.nrtLock.Lock()
	defer a.nrtLock.Unlock()

	cli := a.nrtCli.NodeResourceTopologies()
	ctx := context.Background()
	cr, err := cli.Get(ctx, a.nodeName, metav1.GetOptions{})
	if err != nil {
		cr = nil
		if !errors.IsNotFound(err) {
			log.Warn("failed to look up current node resource topology CR: %v", err)
		}
	}

	// XXX TODO Deletion should be handled differently:
	//   1. add expiration timestamp to nrt.NodeResourceTopology
	//   2. GC CRs that are past their expiration time (for instance by NFD)
	//   3. make sure we refresh our CR (either here or preferably/easier
	//      by triggering in resmgr an updateTopologyZones() during longer
	//      periods of inactivity)
	// update CR if one exists
	if cr != nil {
		cr.Attributes = nrt.AttributeList{
			nrt.AttributeInfo{
				Name:  "TopologyPolicy",
				Value: policy,
			},
		}

		cr.Zones = zonesToNrt(zones)

		_, err = cli.Update(ctx, cr, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update node resource topology CR: %w", err)
		}

		return nil
	}

	// or create a new one
	cr = &nrt.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: a.nodeName,
		},

		Attributes: nrt.AttributeList{
			nrt.AttributeInfo{
				Name:  "TopologyPolicy",
				Value: policy,
			},
		},
		Zones: zonesToNrt(zones),
	}

	_, err = cli.Create(ctx, cr, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create node resource topology CR: %w", err)
	}

	return nil
}

func zonesToNrt(in []*policyapi.TopologyZone) nrt.ZoneList {
	out := nrt.ZoneList{}
	for _, i := range in {
		resources := nrt.ResourceInfoList{}
		for _, r := range i.Resources {
			resources = append(resources, nrt.ResourceInfo{
				Name:        r.Name,
				Capacity:    r.Capacity,
				Allocatable: r.Allocatable,
				Available:   r.Available,
			})
		}
		out = append(out, nrt.Zone{
			Name:       i.Name,
			Type:       i.Type,
			Parent:     i.Parent,
			Resources:  resources,
			Attributes: attributesToNrt(i.Attributes),
		})
	}
	return out
}

func attributesToNrt(in []*policyapi.ZoneAttribute) nrt.AttributeList {
	var out nrt.AttributeList
	for _, i := range in {
		out = append(out, nrt.AttributeInfo{
			Name:  i.Name,
			Value: i.Value,
		})
	}

	return out
}
