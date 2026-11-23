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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"

	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// Condition reasons.
const (
	ReasonAttached    = "Attached"
	ReasonNoHost      = "NoHost"
	ReasonAttachError = "AttachError"
	ReasonTimeout     = "Timeout"
	ReasonConflict    = "Conflict"
	// ReasonInvalidConfig: the MemoryConfig of the result or the
	// controller's object in it is invalid.
	ReasonInvalidConfig = "InvalidConfig"
	// Event reasons.
	EventAttached     = "Attached"
	EventAttachFailed = "AttachFailed"
	EventDetached     = "Detached"
)

func (c *Controller) condAttached() string { return device.AttachedCondition(device.PoolDriverName) }
func (c *Controller) condFailed() string   { return device.AttachFailedCondition(device.PoolDriverName) }

// DeviceData is the status data of an attached device: the pool contract
// that the pool driver reads, and the attachment of the pool server.
type DeviceData struct {
	device.PoolStatusData
	Host         string `json:"host"`
	HostUUID     string `json:"hostUUID,omitempty"`
	Attachment   string `json:"attachment"`
	QemuDeviceID string `json:"qemuDeviceID"`
}

// hasCondition returns the condition of the status entry of a result if it
// is True.
func hasCondition(claim *resourceapi.ResourceClaim, r resourceapi.DeviceRequestAllocationResult, condType string) *metav1.Condition {
	e := device.StatusOf(claim, &r)
	if e == nil {
		return nil
	}
	if cond := meta.FindStatusCondition(e.Conditions, condType); cond != nil && cond.Status == metav1.ConditionTrue {
		return cond
	}
	return nil
}

// attachedUpToDate returns true if the status entry of a result has the
// Attached condition cond (True, same reason and message) and the device
// data.
func attachedUpToDate(claim *resourceapi.ResourceClaim, r resourceapi.DeviceRequestAllocationResult, cond metav1.Condition, data DeviceData) bool {
	old := hasCondition(claim, r, cond.Type)
	if old == nil || old.Reason != cond.Reason || old.Message != cond.Message {
		return false
	}
	e := device.StatusOf(claim, &r)
	var oldData DeviceData
	return e.Data != nil && json.Unmarshal(e.Data.Raw, &oldData) == nil && reflect.DeepEqual(oldData, data)
}

// hasResult returns true if the claim is still allocated with the result.
func hasResult(claim *resourceapi.ResourceClaim, r resourceapi.DeviceRequestAllocationResult) bool {
	if claim.Status.Allocation == nil {
		return false
	}
	for _, x := range claim.Status.Allocation.Devices.Results {
		if x.Driver == r.Driver && x.Pool == r.Pool && x.Device == r.Device && x.Request == r.Request &&
			(r.ShareID == nil || x.ShareID != nil && *x.ShareID == *r.ShareID) {
			return true
		}
	}
	return false
}

// setClaimDeviceStatus sets a True condition (and clears the opposite
// one) and the data in the status entry of a result, on a fresh copy of
// the claim, with conflict retry. It returns false if nothing was written:
// the claim is gone, no longer allocated with the result, or the entry is
// already up to date.
func (c *Controller) setClaimDeviceStatus(ctx context.Context, claim *resourceapi.ResourceClaim, r resourceapi.DeviceRequestAllocationResult, cond metav1.Condition, data any) (bool, error) {
	var raw []byte
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return false, err
		}
		raw = b
	}
	opposite := c.condFailed()
	if cond.Type == c.condFailed() {
		opposite = c.condAttached()
	}
	written := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		written = false
		fresh, err := c.kube.ResourceV1().ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if fresh.UID != claim.UID || !hasResult(fresh, r) {
			return nil
		}
		e := device.StatusOf(fresh, &r)
		if e == nil {
			fresh.Status.Devices = append(fresh.Status.Devices, resourceapi.AllocatedDeviceStatus{
				Driver: r.Driver, Pool: r.Pool, Device: r.Device,
			})
			e = &fresh.Status.Devices[len(fresh.Status.Devices)-1]
		}
		changed := false
		if r.ShareID != nil {
			if id := string(*r.ShareID); e.ShareID == nil || *e.ShareID != id {
				e.ShareID = &id
				changed = true
			}
		}
		if meta.RemoveStatusCondition(&e.Conditions, opposite) {
			changed = true
		}
		if old := meta.FindStatusCondition(e.Conditions, cond.Type); old == nil || old.Status != cond.Status ||
			old.Reason != cond.Reason || old.Message != cond.Message {
			meta.SetStatusCondition(&e.Conditions, cond)
			changed = true
		}
		if raw != nil && (e.Data == nil || !bytes.Equal(e.Data.Raw, raw)) {
			e.Data = &runtime.RawExtension{Raw: raw}
			changed = true
		}
		if !changed {
			return nil
		}
		if e.Conditions == nil {
			e.Conditions = []metav1.Condition{}
		}
		if _, err := c.kube.ResourceV1().ResourceClaims(fresh.Namespace).UpdateStatus(ctx, fresh, metav1.UpdateOptions{}); err != nil {
			return err
		}
		written = true
		return nil
	})
	return written, err
}
