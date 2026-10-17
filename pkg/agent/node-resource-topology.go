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
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	nrt "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

// nrtState is the desired content of the node resource topology CR.
type nrtState struct {
	policy string
	zones  []*policyapi.TopologyZone
}

// UpdateNrtCR records the given data as the desired state of the node's
// node resource topology CR and wakes the reconciler to write it.
//
// A newer request replaces older ones. The reconciler writes the latest
// snapshot only, so bursts coalesce and an old snapshot never overwrites a
// new one.
func (a *Agent) UpdateNrtCR(policy string, zones []*policyapi.TopologyZone) error {
	if a.hasLocalConfig() {
		return nil
	}

	a.nrtLock.Lock()
	a.nrtWant = &nrtState{policy: policy, zones: zones}
	a.nrtLock.Unlock()

	wakeUp(a.nrtWake)
	return nil
}

// reconcileNrtCR writes the most recently requested desired state to the
// node's node resource topology CR. It does nothing without an NRT client
// or without a desired state.
func (a *Agent) reconcileNrtCR() error {
	a.nrtLock.Lock()
	cli, want := a.nrtCli, a.nrtWant
	a.nrtLock.Unlock()
	if cli == nil || want == nil {
		return nil
	}

	nrtCli := cli.NodeResourceTopologies()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cr, err := nrtCli.Get(ctx, a.nodeName, metav1.GetOptions{})
	if err != nil {
		cr = nil
		if !errors.IsNotFound(err) {
			log.Warnf("failed to look up current node resource topology CR: %v", err)
		}
	}

	attributes := nrt.AttributeList{
		nrt.AttributeInfo{
			Name:  "TopologyPolicy",
			Value: want.policy,
		},
	}
	zones := zonesToNrt(want.zones)

	// XXX TODO Deletion should be handled differently:
	//   1. add expiration timestamp to nrt.NodeResourceTopology
	//   2. GC CRs that are past their expiration time (for instance by NFD)
	// update CR if one exists
	if cr != nil {
		// A periodic resync only writes when the CR differs.
		if equality.Semantic.DeepEqual(cr.Attributes, attributes) && equality.Semantic.DeepEqual(cr.Zones, zones) {
			return nil
		}

		log.Infof("updating node resource topology CR")

		cr.Attributes = attributes
		cr.Zones = zones

		_, err = nrtCli.Update(ctx, cr, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update node resource topology CR: %w", err)
		}

		return nil
	}

	// or create a new one
	log.Infof("creating node resource topology CR")

	cr = &nrt.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: a.nodeName,
		},
		Attributes: attributes,
		Zones:      zones,
	}

	_, err = nrtCli.Create(ctx, cr, metav1.CreateOptions{})
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
