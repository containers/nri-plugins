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
	"context"
	"reflect"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"

	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
)

// DeviceName turns a server device name into a DNS label: lower-case,
// characters other than [a-z0-9-] replaced by '-', '-' trimmed at both
// ends, at most 63 characters. "" if nothing is left.
func DeviceName(name string) string {
	b := []byte(strings.ToLower(name))
	for i, ch := range b {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
			b[i] = '-'
		}
	}
	s := strings.Trim(string(b), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

// convertDevice returns the ResourceSlice device of a pool device, as the
// pool controller contract (docs/memory/cxl/pool.md) defines it.
func (c *Controller) convertDevice(name string, d api.Device) resourceapi.Device {
	rd := resourceapi.Device{
		Name: name,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			device.AttrType:       {StringValue: ptr.To(device.DeviceTypeMemdev)},
			device.AttrSource:     {StringValue: ptr.To(cxlapi.SourcePool)},
			device.AttrSerial:     {StringValue: ptr.To(strings.ToLower(d.Serial))},
			device.AttrShared:     {BoolValue: ptr.To(d.Shared)},
			device.AttrPersistent: {BoolValue: ptr.To(d.Persistent)},
			device.AttrSize:       {IntValue: ptr.To(d.Size)},
			// a missing coherent attribute is false
			device.AttrCoherent: {BoolValue: ptr.To(devicesCoherent)},
		},
		BindsToNode:              ptr.To(true),
		BindingConditions:        []string{c.condAttached()},
		BindingFailureConditions: []string{c.condFailed()},
	}
	if d.Shared {
		// the size of a shared device is never a capacity: every
		// allocation would consume it
		// no requestPolicy: a claim without a shares request consumes
		// the whole value and has the device exclusively (attached with
		// AttachRequest.SingleHost), shares: "1" is one sharer
		rd.AllowMultipleAllocations = ptr.To(true)
		rd.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			device.CapacityShares: {Value: *resource.NewQuantity(device.SharesUnlimited, resource.DecimalSI)},
		}
	} else {
		rd.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			device.CapacityMemory: {Value: *resource.NewQuantity(d.Size, resource.BinarySI)},
		}
	}
	return rd
}

// buildDevices converts the pool devices of the server. Quarantined
// devices (state error) and devices without a valid serial are left out,
// serials get the form 0x<lower-case hex>, names are sanitized, duplicates
// skipped. It returns the devices and the map published name -> device.
func (c *Controller) buildDevices(ds []api.Device) ([]resourceapi.Device, map[string]api.Device) {
	sort.Slice(ds, func(i, j int) bool { return ds[i].Name < ds[j].Name })
	out := make([]resourceapi.Device, 0, len(ds))
	byName := map[string]api.Device{}
	for _, d := range ds {
		if d.Scope != "" && d.Scope != api.ScopePool {
			continue
		}
		if d.State == api.DeviceError {
			c.debugf("device %s: state %s, not published", d.Name, d.State)
			continue
		}
		sn, err := cxlapi.ParseSerial(d.Serial)
		if err != nil {
			c.warnName(d.Name, "device %s: %v, not published", d.Name, err)
			continue
		}
		d.Serial = cxlapi.FormatSerial(sn)
		name := DeviceName(d.Name)
		if name == "" {
			c.warnName(d.Name, "device %s: no valid DNS label in the name, not published", d.Name)
			continue
		}
		if other, ok := byName[name]; ok {
			c.warnName(d.Name, "device %s: published name %s is already used by device %s, not published", d.Name, name, other.Name)
			continue
		}
		byName[name] = d
		out = append(out, c.convertDevice(name, d))
	}
	return out, byName
}

func (c *Controller) warnName(device, format string, args ...any) {
	if !c.nameWarned[device] {
		c.nameWarned[device] = true
		c.logf(format, args...)
	}
}

// nodeSelector returns the node selector of the nodes that are hosts of
// the server: kubernetes.io/hostname In [their hostname labels, sorted].
// nil if there is no such node.
func (c *Controller) nodeSelector(hm hostMap) *corev1.NodeSelector {
	values := make([]string, 0, len(hm.nodeHost))
	for n := range hm.nodeHost {
		values = append(values, hm.hostnames[n])
	}
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	return &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key:      corev1.LabelHostname,
			Operator: corev1.NodeSelectorOpIn,
			Values:   slices.Compact(values),
		}},
	}}}
}

// driverResources returns the pool of the devices for the nodes of the
// node selector, no pool if the selector is nil. The pool has a slice for
// every resourceapi.ResourceSliceMaxDevices devices, and one slice if it has
// no devices.
func (c *Controller) driverResources(devs []resourceapi.Device, sel *corev1.NodeSelector) *resourceslice.DriverResources {
	res := &resourceslice.DriverResources{Pools: map[string]resourceslice.Pool{}}
	if sel == nil {
		return res
	}
	pool := resourceslice.Pool{NodeSelector: sel}
	for chunk := range slices.Chunk(devs, resourceapi.ResourceSliceMaxDevices) {
		pool.Slices = append(pool.Slices, resourceslice.Slice{Devices: chunk})
	}
	if len(pool.Slices) == 0 {
		pool.Slices = []resourceslice.Slice{{Devices: devs}}
	}
	res.Pools[c.cfg.DRAPoolName] = pool
	return res
}

// publish reads the pool devices from the server and updates the
// ResourceSlice of the pool for the nodes that are hosts of the server
// when the devices or the nodes changed. Without such nodes the pool has
// no ResourceSlice.
func (c *Controller) publish(ctx context.Context, hm hostMap) error {
	ds, err := c.pool.Devices(ctx, api.ScopePool)
	if err != nil {
		return err
	}
	devs, byName := c.buildDevices(ds)
	c.devices = byName
	sel := c.nodeSelector(hm)
	res := c.driverResources(devs, sel)
	if c.resourceSlices == nil {
		sc, err := resourceslice.StartController(ctx, resourceslice.Options{
			DriverName: device.PoolDriverName,
			KubeClient: c.kube,
			Owner:      nil,
			Resources:  res,
			ErrorHandler: func(_ context.Context, err error, msg string) {
				c.logf("ERROR: ResourceSlice: %s: %v", msg, err)
			},
			// other pools of the driver belong to other controllers
			ReconcilePoolWithName: c.cfg.DRAPoolName,
		})
		if err != nil {
			return err
		}
		c.resourceSlices = sc
	} else {
		if reflect.DeepEqual(res, c.published) {
			return nil
		}
		c.resourceSlices.Update(res)
	}
	c.published = res
	if sel == nil {
		c.logf("pool %s: no node of the cluster is a host of the server, publishing nothing", c.cfg.DRAPoolName)
	} else {
		c.logf("publishing pool %s for nodes %s: %d devices %s", c.cfg.DRAPoolName, sel.NodeSelectorTerms[0].MatchExpressions[0].Values, len(devs), deviceNames(devs))
	}
	return nil
}

func deviceNames(devs []resourceapi.Device) []string {
	ns := make([]string, 0, len(devs))
	for _, d := range devs {
		ns = append(ns, d.Name)
	}
	return ns
}

// deleteResourceSlices deletes the cluster-scoped slices of the driver and pool.
func (c *Controller) deleteResourceSlices(ctx context.Context) error {
	rs := c.kube.ResourceV1().ResourceSlices()
	list, err := rs.List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.driver", device.PoolDriverName).String(),
	})
	if err != nil {
		return err
	}
	for _, s := range list.Items {
		if s.Spec.Driver != device.PoolDriverName || s.Spec.Pool.Name != c.cfg.DRAPoolName || ptr.Deref(s.Spec.NodeName, "") != "" {
			continue
		}
		if err := rs.Delete(ctx, s.Name, metav1.DeleteOptions{}); err != nil {
			return err
		}
		c.logf("deleted ResourceSlice %s", s.Name)
	}
	return nil
}
