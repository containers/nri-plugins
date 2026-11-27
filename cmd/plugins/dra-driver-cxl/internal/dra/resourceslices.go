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

package dra

import (
	"context"
	"errors"
	"maps"
	"time"

	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// resourceSliceSyncDelay is the SyncDelay of the controller of the node's
// cxl.nri.io ResourceSlices: how long it waits after an informer event of
// a slice (someone else created, changed or deleted it) before it puts
// the slice back as the driver publishes it.
//
// kubelet deletes every ResourceSlice of its node when it starts
// (k8s.io/kubernetes pkg/kubelet/cm/dra/plugin/dra_plugin_manager.go,
// NewDRAPluginManager: wipeResourceSlices of all drivers), also when only
// kubelet restarts and the driver keeps running. With the default 30 s
// (resourceslice.DefaultSyncDelay) the node had no cxl.nri.io devices for
// up to 30 s after every kubelet restart, and the claims of them did not
// schedule. The delay batches the events of a burst and lets the informer
// cache catch up with deletions; one second does both for the few slices
// of the node. Only events of the slices wait for it: the driver's own
// publishes call Update, which syncs at once. It also answers any other
// writer of the slice within a second instead of 30 s: two driver instances
// on a node, or a Node object re-created while the driver runs (the
// controller looks up the owner UID once, the garbage collector deletes the
// slice of the old owner) make the slice flap about once a second instead
// of once in 30 s. Accepted: one pool of a slice per 128 devices, one
// write of each a second at most.
const resourceSliceSyncDelay = time.Second

// errorLogInterval is how long the publisher does not log an error of the
// controller of the node's slices again: the controller retries a failed
// write, and an error that the API server keeps returning would be logged
// at every retry.
const errorLogInterval = 10 * time.Minute

// startController starts the controller of the node's ResourceSlices with
// the resources res, like kubeletplugin.Helper.PublishResources does (the
// same owner and names), but with the SyncDelay
// resourceSliceSyncDelay, which the helper has no option for, and the
// publisher's error handler. The controller runs until ctx ends. It returns
// once the informer of the controller has synced, or ctx ended.
func (p *Publisher) startController(ctx context.Context, res *resourceslice.DriverResources) (*resourceslice.Controller, error) {
	ctx = klog.NewContext(ctx, klog.LoggerWithName(klog.FromContext(ctx), "ResourceSlice controller"))
	return resourceslice.StartController(ctx, resourceslice.Options{
		DriverName: device.DriverName,
		KubeClient: p.opts.Client,
		// The UID is found by the controller.
		Owner:        &resourceslice.Owner{APIVersion: "v1", Kind: "Node", Name: p.opts.NodeName},
		Resources:    res, // a deep copy
		SyncDelay:    ptr(resourceSliceSyncDelay),
		ErrorHandler: p.handleError,
	})
}

// handleError handles an error of the controller of the node's slices. When
// the API server dropped the nodeAllocatableResources of a device (feature
// gate DRANodeAllocatableResources off), the table is published without
// them from then on, after one warning: the controller would write the
// slice again at every publish otherwise. Other errors are logged, each
// message once in errorLogInterval, and other dropped fields once whether
// dropped at a create or at an update of the slice.
func (p *Publisher) handleError(ctx context.Context, err error, msg string) {
	key := msg + ": " + err.Error()
	var dropped *resourceslice.DroppedFieldsError
	if errors.As(err, &dropped) && droppedNodeAllocatable(dropped) {
		p.mu.Lock()
		first := !p.noAllocatable
		p.noAllocatable = true
		p.mu.Unlock()
		if first {
			klog.Warning("the API server drops nodeAllocatableResources (feature gate DRANodeAllocatableResources is off): " +
				"claimed memory is not accounted against the node's allocatable memory; publishing the devices without it")
		}
		return
	}
	if dropped != nil {
		key = dropped.Error()
	}
	now := time.Now()
	p.mu.Lock()
	maps.DeleteFunc(p.logged, func(_ string, at time.Time) bool { return now.Sub(at) >= errorLogInterval })
	_, seen := p.logged[key]
	if !seen {
		p.logged[key] = now
	}
	p.mu.Unlock()
	if !seen {
		klog.FromContext(ctx).Error(err, msg)
	}
}

// droppedNodeAllocatable tells whether the API server dropped the
// nodeAllocatableResources of a device of the node's slices.
func droppedNodeAllocatable(dropped *resourceslice.DroppedFieldsError) bool {
	if dropped.DesiredSlice == nil || dropped.ActualSlice == nil {
		return false
	}
	want, got := dropped.DesiredSlice.Spec.Devices, dropped.ActualSlice.Spec.Devices
	for i := range min(len(want), len(got)) {
		if len(want[i].NodeAllocatableResources) > 0 && len(got[i].NodeAllocatableResources) == 0 {
			return true
		}
	}
	return false
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T {
	return &v
}
