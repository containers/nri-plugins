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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"k8s.io/klog/v2"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiSpecs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/lend"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
)

// The CDI spec of a driver: one device claim-<uid>-<n> per allocation
// result n of every claim that the driver has prepared, but companion
// claims and claims whose devices did not verify at a start: the result
// marker CXL_CLAIM_<uid>_<n>, the environment of the result's memory
// (api.Env), the device node of a dax or block device (none of a dax
// device that is gone) and the bind mount of the node mount of an fs device
// (none for the kept device of an UNPREPARING claim whose node mount a
// failed release unmounted). The spec of cxl.nri.io with the lease broker
// has also one device lease-<uid> per lease claim, of the container edits
// that the broker gives. It is built from the records (not from the
// existing file), so it is right also after a restart, and the same for
// both drivers but its kind and file name.

// ClaimCDIDeviceName returns the CDI device name of the allocation result n
// of a claim: "claim-<uid>-<n>".
func ClaimCDIDeviceName(claimUID string, n int) string {
	return "claim-" + claimUID + "-" + strconv.Itoa(n)
}

// leaseCDIDeviceName is the CDI device name of a claim on the lease
// device: its socket directory, lease-fs directory and CLI.
func leaseCDIDeviceName(claimUID string) string {
	return "lease-" + claimUID
}

// envKeys counts the memories of each base env key in one claim.
type envKeys map[string]int

// next returns the env key of the next memory with the base key: the base
// key for the first one, then "<base>_<n>" (api.Nth).
func (k envKeys) next(base string) string {
	k[base]++
	return api.Nth(base, k[base])
}

// daxDeviceNode returns the CDI device node of a dax device. A read-only
// node has permissions "r": the runtime turns that into a device cgroup
// rule that refuses opens for writing, whatever the container's
// capabilities.
func daxDeviceNode(path string, major, minor int, readOnly bool) *cdiSpecs.DeviceNode {
	node := &cdiSpecs.DeviceNode{
		Path:  path,
		Type:  "c",
		Major: int64(major),
		Minor: int64(minor),
	}
	if readOnly {
		node.Permissions = "r"
	}
	return node
}

// blockDeviceNode returns the CDI device node of a block device, with
// permissions "r" when read-only.
func blockDeviceNode(dev string, major, minor int, readOnly bool) *cdiSpecs.DeviceNode {
	node := &cdiSpecs.DeviceNode{Path: dev, Type: "b", Major: int64(major), Minor: int64(minor)}
	if readOnly {
		node.Permissions = "r"
	}
	return node
}

// fsCDIMount returns the CDI mount of a node mount at path in a container:
// options bind, ro, nosuid, nodev, without ro for a use that writes.
func fsCDIMount(nodeMount, path string, readOnly bool) *cdiSpecs.Mount {
	options := []string{"bind", "ro", "nosuid", "nodev"}
	if !readOnly {
		options = []string{"bind", "nosuid", "nodev"}
	}
	return &cdiSpecs.Mount{HostPath: nodeMount, ContainerPath: path, Type: "bind", Options: options}
}

// cdiClaims tells whether the CDI spec of a driver has the devices of a
// preparation: a claim that is not a companion, PREPARED, UNPREPARING (until
// kubelet's Unprepare retry), or the claim pending whose Prepare is
// finishing.
func cdiClaims(prep *state.Preparation, pending string) bool {
	switch {
	case prep.Lease != "":
		return false
	case prep.UID == pending, prep.State == state.PreparationPrepared:
		return true
	}
	return prep.State == state.PreparationUnpreparing
}

// buildCDISpec returns the CDI spec of the kind of the claims of the driver
// in doc that cdiClaims takes and, with leases, of the lease claims in doc,
// and an error for a PREPARED or pending claim with a device of mode fs
// that has no node mount.
func buildCDISpec(doc *store.Document, driver, kind string, rs roots, pending string, leases lend.LeaseClaims) (*cdiSpecs.Spec, error) {
	spec := &cdiSpecs.Spec{Kind: kind}
	byUID := map[string]*state.Preparation{}
	for i := range doc.Preparations {
		prep := &doc.Preparations[i]
		if prep.Driver == driver && cdiClaims(prep, pending) {
			byUID[prep.UID] = prep
		}
	}
	var errs []error
	for _, uid := range slices.Sorted(maps.Keys(byUID)) {
		info := claimInfo(doc, byUID[uid], rs)
		keys := envKeys{}
		for i := range info.Devices {
			d := &info.Devices[i]
			if err := incomplete(d); err != nil && byUID[uid].State != state.PreparationUnpreparing {
				errs = append(errs, fmt.Errorf("claim %s: %w", byUID[uid].ClaimName, err))
				continue
			}
			spec.Devices = append(spec.Devices, cdiDevice(uid, d, keys))
		}
	}
	if leases != nil {
		for _, lc := range doc.LeaseClaims {
			spec.Devices = append(spec.Devices, cdiSpecs.Device{Name: leaseCDIDeviceName(lc.UID), ContainerEdits: leases.CDIEdits(lc.UID)})
		}
	}
	return spec, errors.Join(errs...)
}

// incomplete returns an error if a prepared device of mode fs has no node
// mount for its CDI mount.
func incomplete(d *state.DeviceInfo) error {
	if d.Persistent && d.Mode == api.ModeFS && d.NodeMount == "" {
		return fmt.Errorf("result %d: no node mount", d.Result)
	}
	return nil
}

// cdiDevice returns the CDI device of a prepared device of the claim uid,
// with the env key of its memory from keys.
func cdiDevice(uid string, d *state.DeviceInfo, keys envKeys) cdiSpecs.Device {
	env := map[string]string{api.ResultMarker(uid, d.Result): "1"}
	edits := cdiSpecs.ContainerEdits{}
	if m, ok := deviceMemory(d, keys); ok {
		maps.Copy(env, api.Env(m))
	}
	switch {
	case d.Mode == api.ModeDax && d.DaxDevice != "":
		edits.DeviceNodes = []*cdiSpecs.DeviceNode{daxDeviceNode(d.DaxDevice, d.DaxMajor, d.DaxMinor, d.ReadOnly)}
	case d.Persistent && d.Mode == api.ModeBlock:
		edits.DeviceNodes = []*cdiSpecs.DeviceNode{blockDeviceNode(d.BlockDevice, d.BlockMajor, d.BlockMinor, d.ReadOnly)}
	case d.Persistent && d.Mode == api.ModeFS && d.NodeMount != "":
		edits.Mounts = []*cdiSpecs.Mount{fsCDIMount(d.NodeMount, d.Path, d.ReadOnly)}
	}
	edits.Env = api.EnvList(env)
	return cdiSpecs.Device{Name: ClaimCDIDeviceName(uid, d.Result), ContainerEdits: edits}
}

// writeCDISpec writes the CDI spec file name to the cache, or removes it
// when the spec has no devices. The log lines name the driver.
func writeCDISpec(cache *cdiapi.Cache, spec *cdiSpecs.Spec, driver, name string) error {
	if len(spec.Devices) == 0 {
		if err := cache.RemoveSpec(name); err != nil {
			klog.V(5).Infof("%s: could not remove CDI spec %s (may not exist): %v", driver, name, err)
		}
		return nil
	}
	version, err := cdiSpecs.MinimumRequiredVersion(spec)
	if err != nil {
		return fmt.Errorf("CDI spec version: %w", err)
	}
	spec.Version = version
	if err := cache.WriteSpec(spec, name); err != nil {
		return fmt.Errorf("write CDI spec %s: %w", name, err)
	}
	klog.V(5).Infof("%s: wrote CDI spec %s with %d devices", driver, name, len(spec.Devices))
	return nil
}
