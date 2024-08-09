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

package main

import (
	"strings"

	"github.com/containerd/nri/pkg/api"
	oci "github.com/opencontainers/runtime-spec/specs-go"
	"tags.cncf.io/container-device-interface/pkg/cdi"
)

type cdiCache struct {
	*cdi.Cache
}

// InjectDevices applies the specified CDI devices to the NRI container adjustments.
// This implementation first applies the edits to an empty OCI Spec and then
// converts these edits to NRI adjustments.
func (c *cdiCache) InjectDevices(adjustments *api.ContainerAdjustment, devices ...string) ([]string, error) {
	ociSpec := &oci.Spec{}

	unresolved, err := c.Cache.InjectDevices(ociSpec, devices...)
	if err != nil {
		return unresolved, err
	}

	for _, ociEnv := range ociSpec.Process.Env {
		parts := strings.SplitN(ociEnv, "=", 2)
		var value string
		if len(parts) > 1 {
			value = parts[1]
		}
		adjustments.AddEnv(parts[0], value)
	}

	for _, oci := range ociSpec.Linux.Devices {
		device := (ociLinuxDevice)(oci).toNRI()
		adjustments.AddDevice(device)
	}

	for _, oci := range ociSpec.Linux.Resources.Devices {
		deviceCgroup := (ociLinuxDeviceCgroup)(oci).toNRI()
		adjustments.Linux.Resources.Devices = append(adjustments.Linux.Resources.Devices, deviceCgroup)
	}

	for _, oci := range ociSpec.Mounts {
		mount := (ociMount)(oci).toNRI()
		adjustments.AddMount(mount)
	}

	if oci := ociSpec.Hooks; oci != nil {
		hooks := (*ociHooks)(oci).toNRI()
		adjustments.AddHooks(hooks)
	}

	// TODO: Handle IntelRdt fields
	// TODO: Handle additional GID fields
	return nil, nil
}

type ociLinuxDevice oci.LinuxDevice

func (o ociLinuxDevice) toNRI() *api.LinuxDevice {
	var filemode *api.OptionalFileMode
	if mode := o.FileMode; mode != nil {
		filemode = &api.OptionalFileMode{
			Value: (uint32)(*mode),
		}
	}
	var uid *api.OptionalUInt32
	if u := o.UID; u != nil {
		uid = &api.OptionalUInt32{
			Value: *u,
		}
	}
	var gid *api.OptionalUInt32
	if g := o.UID; g != nil {
		gid = &api.OptionalUInt32{
			Value: *g,
		}
	}

	return &api.LinuxDevice{
		Path:     o.Path,
		Type:     o.Type,
		Major:    o.Major,
		Minor:    o.Minor,
		FileMode: filemode,
		Uid:      uid,
		Gid:      gid,
	}
}

type ociLinuxDeviceCgroup oci.LinuxDeviceCgroup

func (o ociLinuxDeviceCgroup) toNRI() *api.LinuxDeviceCgroup {
	var major *api.OptionalInt64
	if m := o.Major; m != nil {
		major = &api.OptionalInt64{
			Value: *m,
		}
	}
	var minor *api.OptionalInt64
	if m := o.Minor; m != nil {
		minor = &api.OptionalInt64{
			Value: *m,
		}
	}

	return &api.LinuxDeviceCgroup{
		Allow:  o.Allow,
		Type:   o.Type,
		Major:  major,
		Minor:  minor,
		Access: o.Access,
	}
}

type ociMount oci.Mount

func (o ociMount) toNRI() *api.Mount {
	return &api.Mount{
		Destination: o.Destination,
		Type:        o.Type,
		Source:      o.Source,
		Options:     o.Options,
		// TODO: We don't handle the following fields:
		// UIDMappings []LinuxIDMapping `json:"uidMappings,omitempty" platform:"linux"`
		// GIDMappings []LinuxIDMapping `json:"gidMappings,omitempty" platform:"linux"`
	}
}

type ociHooks oci.Hooks

func (o *ociHooks) toNRI() *api.Hooks {
	hooks := &api.Hooks{}
	for _, h := range o.Prestart {
		hooks.Prestart = append(hooks.Prestart, (ociHook)(h).toNRI())
	}
	for _, h := range o.CreateRuntime {
		hooks.Prestart = append(hooks.CreateRuntime, (ociHook)(h).toNRI())
	}
	for _, h := range o.CreateContainer {
		hooks.Prestart = append(hooks.CreateContainer, (ociHook)(h).toNRI())
	}
	for _, h := range o.StartContainer {
		hooks.Prestart = append(hooks.StartContainer, (ociHook)(h).toNRI())
	}
	for _, h := range o.Poststart {
		hooks.Prestart = append(hooks.Poststart, (ociHook)(h).toNRI())
	}
	for _, h := range o.Poststop {
		hooks.Prestart = append(hooks.Poststop, (ociHook)(h).toNRI())
	}
	return hooks
}

type ociHook oci.Hook

func (o ociHook) toNRI() *api.Hook {
	var timeout *api.OptionalInt
	if t := o.Timeout; t != nil {
		timeout = &api.OptionalInt{
			Value: (int64)(*t),
		}
	}
	return &api.Hook{
		Path:    o.Path,
		Args:    o.Args,
		Env:     o.Env,
		Timeout: timeout,
	}
}
