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

package e2eish

// Replaying a DRA test needs the two actors which sit between an e2e test and
// the policy. Neither is in the log, because neither is driven by the test.
//
// The scheduler turns a ResourceClaimTemplate into a ResourceClaim and
// allocates it, which picks the device each request gets and the capacity it
// consumes of it. That is upstream's own allocator here, not a
// reimplementation: it evaluates the CEL selectors of the requests and of
// their device class against the devices the policy published, so what a
// claim resolves to is what it would resolve to on a cluster.
//
// The kubelet then prepares the claim, which is where the policy allocates
// the resources of it and the CDI devices granting them to the containers are
// named. The replay names them the same way, so the policy finds a
// container's claims exactly as it does in production.

import (
	"context"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/dynamic-resource-allocation/structured"
	"k8s.io/utils/ptr"

	"github.com/containers/nri-plugins/pkg/resmgr/dra"
)

// draNodeName is the node of the replay. It names the pool of the devices
// the policy publishes, as the resource manager names it after its node.
const draNodeName = "e2eish"

// draCluster holds the DRA objects an e2e test created and the claims
// allocated from them.
type draCluster struct {
	driver    string
	classes   map[string]*resourceapi.DeviceClass
	templates map[string]*resourceapi.ResourceClaimTemplate
	claims    map[types.UID]*resourceapi.ResourceClaim
	pods      map[string][]types.UID // the claims of a pod, by namespace/name
}

// podClaim is a claim of a pod, under the name the pod knows it by.
type podClaim struct {
	name  string
	claim *resourceapi.ResourceClaim
}

func newDRACluster(driver string) *draCluster {
	return &draCluster{
		driver:    driver,
		classes:   map[string]*resourceapi.DeviceClass{},
		templates: map[string]*resourceapi.ResourceClaimTemplate{},
		claims:    map[types.UID]*resourceapi.ResourceClaim{},
		pods:      map[string][]types.UID{},
	}
}

// addClass adds a DeviceClass.
func (d *draCluster) addClass(class *resourceapi.DeviceClass) {
	d.classes[class.Name] = class
}

// addTemplate adds a ResourceClaimTemplate of a namespace.
func (d *draCluster) addTemplate(ns string, tmpl *resourceapi.ResourceClaimTemplate) {
	d.templates[ns+"/"+tmpl.Name] = tmpl
}

// List implements structured.DeviceClassLister.
func (d *draCluster) List() ([]*resourceapi.DeviceClass, error) {
	classes := make([]*resourceapi.DeviceClass, 0, len(d.classes))
	for _, name := range slices.Sorted(maps.Keys(d.classes)) {
		classes = append(classes, d.classes[name])
	}
	return classes, nil
}

// Get implements structured.DeviceClassLister.
func (d *draCluster) Get(name string) (*resourceapi.DeviceClass, error) {
	class, ok := d.classes[name]
	if !ok {
		return nil, fmt.Errorf("no DeviceClass %s", name)
	}
	return class, nil
}

// podClaims returns the claims of a pod, created from the templates it names.
// The claims are not allocated yet.
func (d *draCluster) podClaims(ns string, pod *corev1.Pod) ([]podClaim, error) {
	var claims []podClaim
	for _, pc := range pod.Spec.ResourceClaims {
		if pc.ResourceClaimTemplateName == nil {
			return nil, fmt.Errorf("claim %q of pod %s/%s names no template", pc.Name, ns, pod.Name)
		}
		tmpl, ok := d.templates[ns+"/"+*pc.ResourceClaimTemplateName]
		if !ok {
			return nil, fmt.Errorf("claim %q of pod %s/%s names the unknown template %s/%s",
				pc.Name, ns, pod.Name, ns, *pc.ResourceClaimTemplateName)
		}
		spec := tmpl.Spec.Spec.DeepCopy()
		defaultClaimSpec(spec)
		// A cluster gives a template-generated claim a random name and UID.
		// The replay derives both, so that a regenerated test does not churn.
		claims = append(claims, podClaim{
			name: pc.Name,
			claim: &resourceapi.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: ns,
					Name:      pod.Name + "-" + pc.Name,
					UID:       types.UID(ns + "-" + pod.Name + "-" + pc.Name + "-claim"),
				},
				Spec: *spec,
			},
		})
	}
	return claims, nil
}

// defaultClaimSpec fills in the fields of the device requests of a claim
// which the API server defaults. The log of an e2e test has the object the
// test submitted, so the defaults are not in it, and the allocator refuses a
// request with no allocation mode.
//
// An unspecified allocation mode is ExactCount, and the count of an
// ExactCount request defaults to one.
func defaultClaimSpec(spec *resourceapi.ResourceClaimSpec) {
	exact := func(e *resourceapi.ExactDeviceRequest) {
		if e == nil {
			return
		}
		if e.AllocationMode == "" {
			e.AllocationMode = resourceapi.DeviceAllocationModeExactCount
		}
		if e.AllocationMode == resourceapi.DeviceAllocationModeExactCount && e.Count == 0 {
			e.Count = 1
		}
	}
	for i := range spec.Devices.Requests {
		req := &spec.Devices.Requests[i]
		exact(req.Exactly)
		for j := range req.FirstAvailable {
			sub := &req.FirstAvailable[j]
			if sub.AllocationMode == "" {
				sub.AllocationMode = resourceapi.DeviceAllocationModeExactCount
			}
			if sub.AllocationMode == resourceapi.DeviceAllocationModeExactCount && sub.Count == 0 {
				sub.Count = 1
			}
		}
	}
}

// allocate allocates the claims of one pod from the given devices, all of
// them or none, the way the scheduler allocates the claims of a pod it is
// placing on a node. It tells whether they fit.
//
// A successful allocation writes the result into each claim and keeps the
// claims, so that the devices they hold are taken for the claims allocated
// after them.
func (d *draCluster) allocate(devices []resourceapi.Device, claims []podClaim) (bool, error) {
	if len(claims) == 0 {
		return true, nil
	}

	var (
		ctx      = context.Background()
		features = structured.Features{ConsumableCapacity: true}
		list     = make([]*resourceapi.ResourceClaim, 0, len(claims))
	)
	for _, pc := range claims {
		list = append(list, pc.claim)
	}

	allocator, err := structured.NewAllocator(ctx, features, d.allocatedState(), d,
		[]*resourceapi.ResourceSlice{d.slice(devices)},
		cel.NewCache(10, cel.Features{EnableConsumableCapacity: true}))
	if err != nil {
		return false, fmt.Errorf("failed to create the DRA allocator: %w", err)
	}

	results, err := allocator.Allocate(ctx, d.node(), list)
	switch {
	case err != nil:
		// A claim which does not fit the node is not an error of the
		// replay: the e2e test may be checking that it stays pending.
		Log.Infof("===== claims of %s do not fit: %v", list[0].Namespace+"/"+list[0].Name, err)
		return false, nil
	case len(results) != len(list):
		return false, nil
	}

	for i, claim := range list {
		claim.Status.Allocation = &results[i]
		d.claims[claim.UID] = claim
	}
	return true, nil
}

// own records the claims as those of the pod, which releases them when it is
// deleted.
func (d *draCluster) own(ns, pod string, claims []podClaim) {
	var uids []types.UID
	for _, pc := range claims {
		uids = append(uids, pc.claim.UID)
	}
	d.pods[ns+"/"+pod] = uids
}

// release drops the claims of a pod and returns their UIDs, in the order
// they were created.
func (d *draCluster) release(ns, pod string) []types.UID {
	key := ns + "/" + pod
	uids := d.pods[key]
	delete(d.pods, key)
	for _, uid := range uids {
		delete(d.claims, uid)
	}
	return uids
}

// results returns the allocation results of a claim which are the driver's
// own, as the DRA plugin filters them before the policy sees them.
func (d *draCluster) results(claim *resourceapi.ResourceClaim) []resourceapi.DeviceRequestAllocationResult {
	if claim.Status.Allocation == nil {
		return nil
	}
	var results []resourceapi.DeviceRequestAllocationResult
	for _, r := range claim.Status.Allocation.Devices.Results {
		if r.Driver == d.driver {
			results = append(results, r)
		}
	}
	return results
}

// containerDevices returns the CDI devices of each container of the pod, by
// container name. A container holding a claim gets the device of every result
// of it, one naming a single request only that request's.
func (d *draCluster) containerDevices(pod *corev1.Pod, claims []podClaim) map[string][]string {
	byName := map[string]*resourceapi.ResourceClaim{}
	for _, pc := range claims {
		byName[pc.name] = pc.claim
	}

	devices := map[string][]string{}
	ctrs := append(slices.Clone(pod.Spec.InitContainers), pod.Spec.Containers...)
	for _, ctr := range ctrs {
		for _, cc := range ctr.Resources.Claims {
			claim, ok := byName[cc.Name]
			if !ok {
				continue
			}
			for i, r := range d.results(claim) {
				if cc.Request != "" && resourceclaim.BaseRequestRef(r.Request) != cc.Request {
					continue
				}
				devices[ctr.Name] = append(devices[ctr.Name],
					dra.CDIDevice(d.driver, claim.UID, i))
			}
		}
	}
	return devices
}

// slice is the ResourceSlice of the devices, as the resource manager
// publishes them: one pool named after the node, of the driver of the policy.
func (d *draCluster) slice(devices []resourceapi.Device) *resourceapi.ResourceSlice {
	return &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: draNodeName + "-" + d.driver},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   d.driver,
			NodeName: ptr.To(draNodeName),
			Pool: resourceapi.ResourcePool{
				Name:               draNodeName,
				ResourceSliceCount: 1,
			},
			Devices: devices,
		},
	}
}

// node is the node of the replay, the only one claims are allocated for.
func (d *draCluster) node() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: draNodeName}}
}

// allocatedState is what the claims allocated so far take of the devices.
// A device of several claims at once is held by the share IDs of their
// results and the capacity they consume, not by the device itself, which
// would make it unavailable to any further claim.
func (d *draCluster) allocatedState() structured.AllocatedState {
	state := structured.AllocatedState{
		AllocatedDevices:         sets.New[structured.DeviceID](),
		AllocatedSharedDeviceIDs: sets.New[structured.SharedDeviceID](),
		AggregatedCapacity:       structured.NewConsumedCapacityCollection(),
	}
	for _, claim := range d.claims {
		if claim.Status.Allocation == nil {
			continue
		}
		for _, r := range claim.Status.Allocation.Devices.Results {
			id := structured.MakeDeviceID(r.Driver, r.Pool, r.Device)
			if r.ShareID == nil {
				state.AllocatedDevices.Insert(id)
			} else {
				state.AllocatedSharedDeviceIDs.Insert(structured.MakeSharedDeviceID(id, r.ShareID))
			}
			if len(r.ConsumedCapacity) > 0 {
				state.AggregatedCapacity.Insert(
					structured.NewDeviceConsumedCapacity(id, r.ConsumedCapacity))
			}
		}
	}
	return state
}
