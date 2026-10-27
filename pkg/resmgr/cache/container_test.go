// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package cache_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/utils"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	nri "github.com/containerd/nri/pkg/api"
	"sigs.k8s.io/yaml"
)

func TestContainerID(t *testing.T) {
	var (
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(WithCtrPodID(nriPods[0].GetId())),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	require.Equal(t, nriCtrs[0].GetId(), ctrs[0].GetID())
}

func TestContainerPodID(t *testing.T) {
	var (
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(WithCtrPodID(nriPods[0].GetId())),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	require.Equal(t, nriPods[0].GetId(), ctrs[0].GetPodID())
}

func TestContainerPod(t *testing.T) {
	var (
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(WithCtrPodID(nriPods[0].GetId())),
		}
	)

	_, pods, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	pod, ok := ctrs[0].GetPod()
	require.Equal(t, pods[0], pod)
	require.True(t, ok)
}

func TestContainerName(t *testing.T) {
	var (
		name    = "test-ctr"
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(WithCtrPodID(nriPods[0].GetId()), WithCtrName(name)),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	require.Equal(t, name, ctrs[0].GetName())
}

func TestContainerNamespace(t *testing.T) {
	var (
		namespace = "test-namespace"
		nriPods   = []*nri.PodSandbox{
			makePod(WithNamespace(namespace)),
		}
		nriCtrs = []*nri.Container{
			makeCtr(WithCtrPodID(nriPods[0].GetId())),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	require.Equal(t, namespace, ctrs[0].GetNamespace())
}

func TestContainerState(t *testing.T) {
	var (
		state   = cache.ContainerStateRunning
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(
				WithCtrPodID(nriPods[0].GetId()),
				WithCtrState(state),
			),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	require.Equal(t, state, ctrs[0].GetState())
}

func TestContainerQOSClass(t *testing.T) {
	var (
		nriPods = []*nri.PodSandbox{
			makePod(WithCgroupParent("/a/besteffort/pod")),
			makePod(WithCgroupParent("/a/burstable/pod")),
			makePod(WithCgroupParent("/a/pod")),
		}
		nriCtrs = []*nri.Container{
			makeCtr(WithCtrPodID(nriPods[0].GetId())),
			makeCtr(WithCtrPodID(nriPods[1].GetId())),
			makeCtr(WithCtrPodID(nriPods[2].GetId())),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	require.Equal(t, corev1.PodQOSBestEffort, ctrs[0].GetQOSClass())
	require.Equal(t, corev1.PodQOSBurstable, ctrs[1].GetQOSClass())
	require.Equal(t, corev1.PodQOSGuaranteed, ctrs[2].GetQOSClass())
}

func TestContainerArgs(t *testing.T) {
	var (
		args    = []string{"a", "b", "c"}
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(
				WithCtrPodID(nriPods[0].GetId()),
				WithCtrArgs(args),
			),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	require.Equal(t, args, ctrs[0].GetArgs())
}

func TestContainerLabels(t *testing.T) {
	var (
		labels = map[string]string{
			"key1": "value1",
			"key2": "value2",
		}
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(
				WithCtrPodID(nriPods[0].GetId()),
				WithCtrLabels(labels),
			),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	for key, val := range labels {
		chk, ok := ctrs[0].GetLabel(key)
		require.Equal(t, val, chk)
		require.True(t, ok)
	}
}

func TestContainerAnnotations(t *testing.T) {
	var (
		objects = map[string]any{
			"key1": true,
			"key2": "foobar",
			"key3": 3.141,
		}
		annotations = map[string]string{
			"key1": jsonEncode(t, objects["key1"]),
			"key2": jsonEncode(t, objects["key2"]),
			"key3": jsonEncode(t, objects["key3"]),
		}
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(
				WithCtrPodID(nriPods[0].GetId()),
				WithCtrAnnotations(annotations),
			),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	for key, val := range objects {
		var ok bool
		switch objects[key].(type) {
		case bool:
			var bln bool
			_, ok = ctrs[0].GetAnnotation(key, &bln)
			require.Equal(t, val, bln)
		case string:
			var str string
			_, ok = ctrs[0].GetAnnotation(key, &str)
			require.Equal(t, val, str)
		case float64:
			var flt float64
			_, ok = ctrs[0].GetAnnotation(key, &flt)
			require.Equal(t, val, flt)
		}
		require.True(t, ok)
	}
}

func TestContainerEnv(t *testing.T) {
	var (
		vars = []*nri.KeyValue{
			{
				Key:   "key1",
				Value: "value1",
			},
			{
				Key:   "key2",
				Value: "value2",
			},
			{
				Key:   "key3",
				Value: "value3",
			},
		}
		env = []string{
			vars[0].Key + "=" + vars[0].Value,
			vars[1].Key + "=" + vars[1].Value,
			vars[2].Key + "=" + vars[2].Value,
		}
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(
				WithCtrPodID(nriPods[0].GetId()),
				WithCtrEnv(env),
			),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	for _, v := range vars {
		chk, ok := ctrs[0].GetEnv(v.Key)
		require.Equal(t, v.Value, chk)
		require.True(t, ok)
	}
}

func TestContainerMounts(t *testing.T) {
	var (
		mounts = []*nri.Mount{
			{
				Source:      "/foo",
				Destination: "/host/foo",
				Type:        "bind",
				Options:     []string{"bind", "ro"},
			},
			{
				Source:      "/bar",
				Destination: "/host/bar",
				Type:        "bind",
				Options:     []string{"bind", "ro"},
			},
		}
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(
				WithCtrPodID(nriPods[0].GetId()),
				WithCtrMounts(mounts),
			),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	require.Equal(t, mounts, ctrs[0].GetMounts())
}

func TestContainerDevices(t *testing.T) {
	var (
		devices = []*nri.LinuxDevice{
			{
				Path:     "/dev/null",
				Type:     "c",
				Major:    1,
				Minor:    3,
				FileMode: nri.FileMode(0644),
			},
			{
				Path:     "/dev/zero",
				Type:     "c",
				Major:    1,
				Minor:    5,
				FileMode: nri.FileMode(0644),
			},
			{
				Path:     "/dev/foo",
				Type:     "c",
				Major:    3,
				Minor:    45,
				Uid:      nri.UInt32(15),
				Gid:      nri.UInt32(16),
				FileMode: nri.FileMode(0755),
			},
		}
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(
				WithCtrPodID(nriPods[0].GetId()),
				WithCtrDevices(devices),
			),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	require.Equal(t, devices, ctrs[0].GetDevices())
}

func TestContainerAdjustments(t *testing.T) {
	const limit int64 = 123456789

	tcases := []struct {
		name  string
		set   func(cache.Container)
		check func(*testing.T, *nri.ContainerAdjustment)
	}{
		{
			name: "CPU shares",
			set:  func(c cache.Container) { c.SetCPUShares(999) },
			check: func(t *testing.T, a *nri.ContainerAdjustment) {
				require.Equal(t, uint64(999), a.GetLinux().GetResources().GetCpu().GetShares().GetValue())
			},
		},
		{
			name: "CPU quota",
			set:  func(c cache.Container) { c.SetCPUQuota(998) },
			check: func(t *testing.T, a *nri.ContainerAdjustment) {
				require.Equal(t, int64(998), a.GetLinux().GetResources().GetCpu().GetQuota().GetValue())
			},
		},
		{
			name: "CPU period",
			set:  func(c cache.Container) { c.SetCPUPeriod(997) },
			check: func(t *testing.T, a *nri.ContainerAdjustment) {
				require.Equal(t, uint64(997), a.GetLinux().GetResources().GetCpu().GetPeriod().GetValue())
			},
		},
		{
			name: "cpuset CPU",
			set:  func(c cache.Container) { c.SetCpusetCpus("1-5,7") },
			check: func(t *testing.T, a *nri.ContainerAdjustment) {
				require.Equal(t, "1-5,7", a.GetLinux().GetResources().GetCpu().GetCpus())
			},
		},
		{
			name: "cpuset memory",
			set:  func(c cache.Container) { c.SetCpusetMems("0-2,4") },
			check: func(t *testing.T, a *nri.ContainerAdjustment) {
				require.Equal(t, "0-2,4", a.GetLinux().GetResources().GetCpu().GetMems())
			},
		},
		{
			name: "memory limit",
			set:  func(c cache.Container) { c.SetMemoryLimit(limit) },
			check: func(t *testing.T, a *nri.ContainerAdjustment) {
				require.Equal(t, limit, a.GetLinux().GetResources().GetMemory().GetLimit().GetValue())
			},
		},
		{
			name: "memory policy",
			set: func(c cache.Container) {
				c.SetMemoryPolicy(nri.MpolMode_MPOL_INTERLEAVE, "0-1", nri.MpolFlag_MPOL_F_STATIC_NODES)
			},
			check: func(t *testing.T, a *nri.ContainerAdjustment) {
				mpol := a.GetLinux().GetMemoryPolicy()
				require.Equal(t, nri.MpolMode_MPOL_INTERLEAVE, mpol.GetMode())
				require.Equal(t, "0-1", mpol.GetNodes())
				require.Equal(t, []nri.MpolFlag{nri.MpolFlag_MPOL_F_STATIC_NODES}, mpol.GetFlags())
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				nriPods = []*nri.PodSandbox{
					makePod(),
				}
				nriCtrs = []*nri.Container{
					makeCtr(WithCtrPodID(nriPods[0].GetId())),
				}
			)

			_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

			tc.set(ctrs[0])

			pending := ctrs[0].GetPendingAdjustment()
			require.NotNil(t, pending)
			tc.check(t, pending)
		})
	}
}

func TestContainerRefusesMemoryPolicyOfRunningContainer(t *testing.T) {
	var (
		nriPods = []*nri.PodSandbox{
			makePod(),
		}
		nriCtrs = []*nri.Container{
			makeCtr(
				WithCtrPodID(nriPods[0].GetId()),
				WithCtrState(cache.ContainerStateRunning),
			),
		}
	)

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

	ctrs[0].SetMemoryPolicy(nri.MpolMode_MPOL_INTERLEAVE, "0-1")

	require.Empty(t, ctrs[0].GetPending())
}

func TestContainerUpdates(t *testing.T) {
	const limit int64 = 123456789

	tcases := []struct {
		name  string
		set   func(cache.Container)
		check func(*testing.T, *nri.ContainerUpdate)
	}{
		{
			name: "CPU shares",
			set:  func(c cache.Container) { c.SetCPUShares(999) },
			check: func(t *testing.T, u *nri.ContainerUpdate) {
				require.Equal(t, uint64(999), u.GetLinux().GetResources().GetCpu().GetShares().GetValue())
			},
		},
		{
			name: "CPU quota",
			set:  func(c cache.Container) { c.SetCPUQuota(998) },
			check: func(t *testing.T, u *nri.ContainerUpdate) {
				require.Equal(t, int64(998), u.GetLinux().GetResources().GetCpu().GetQuota().GetValue())
			},
		},
		{
			name: "CPU period",
			set:  func(c cache.Container) { c.SetCPUPeriod(997) },
			check: func(t *testing.T, u *nri.ContainerUpdate) {
				require.Equal(t, uint64(997), u.GetLinux().GetResources().GetCpu().GetPeriod().GetValue())
			},
		},
		{
			name: "cpuset CPU",
			set:  func(c cache.Container) { c.SetCpusetCpus("1-5,7") },
			check: func(t *testing.T, u *nri.ContainerUpdate) {
				require.Equal(t, "1-5,7", u.GetLinux().GetResources().GetCpu().GetCpus())
			},
		},
		{
			name: "cpuset memory",
			set:  func(c cache.Container) { c.SetCpusetMems("0-2,4") },
			check: func(t *testing.T, u *nri.ContainerUpdate) {
				require.Equal(t, "0-2,4", u.GetLinux().GetResources().GetCpu().GetMems())
			},
		},
		{
			name: "memory limit",
			set:  func(c cache.Container) { c.SetMemoryLimit(limit) },
			check: func(t *testing.T, u *nri.ContainerUpdate) {
				require.Equal(t, limit, u.GetLinux().GetResources().GetMemory().GetLimit().GetValue())
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				nriPods = []*nri.PodSandbox{
					makePod(),
				}
				nriCtrs = []*nri.Container{
					makeCtr(
						WithCtrPodID(nriPods[0].GetId()),
						WithCtrState(cache.ContainerStateRunning),
					),
				}
			)

			_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)

			tc.set(ctrs[0])

			pending := ctrs[0].PeekPendingUpdate()
			require.NotNil(t, pending)
			tc.check(t, pending)
		})
	}
}

func TestContainerTopologyHintFiltering(t *testing.T) {
	var (
		annotations1 = map[string]string{
			"allow.topologyhints.resource-policy.nri.io": `type: prefix
paths:
  - /boot
  - /path/allowed
`,
			"deny.topologyhints.resource-policy.nri.io": `type: prefix
paths:
  - /path/denied
  - /denied
`,
			"test.topologyhints.resource-policy.nri.io": `test:
  Provider: /boot
  CPUs: ` + cpulist + `
  NUMAs: ` + numanode,
		}
		annotations2 = map[string]string{
			"allow.topologyhints.resource-policy.nri.io": `type: prefix
paths:
  - /bar2
  - /allowedpath2
`,
			"deny.topologyhints.resource-policy.nri.io": `type: prefix
paths:
  - /deniedpath2
  - /boot
`,
			"test.topologyhints.resource-policy.nri.io": `test:
  Provider: /boot
  CPUs: ` + cpulist + `
  NUMAs: ` + numanode,
		}
		annotations3 = map[string]string{
			"allow.topologyhints.resource-policy.nri.io/pod": `type: glob
paths:
  - /boo*
  - /anotherglobbedpath*2
`,
			"deny.topologyhints.resource-policy.nri.io/pod": `type: glob
paths:
  - /*
  - /yet*anotherglobbedpath2
`,
			"test.topologyhints.resource-policy.nri.io": `test:
  Provider: /boot
  CPUs: ` + cpulist + `
  NUMAs: ` + numanode,
		}
		annotations4 = map[string]string{
			"allow.topologyhints.resource-policy.nri.io/pod": `type: glob
paths:
  - /a*
  - /anotherglobbedpath*2
`,
			"deny.topologyhints.resource-policy.nri.io/pod": `type: glob
paths:
  - /b*
  - /yet*anotherglobbedpath2
`,
			"test.topologyhints.resource-policy.nri.io": `test:
  Provider: /boot
  CPUs: ` + cpulist + `
  NUMAs: ` + numanode,
		}
		annotations5 = map[string]string{
			"allow.topologyhints.resource-policy.nri.io/pod": `paths:
  - /boot
  - /anotherglobbedpath*2
`,
			"deny.topologyhints.resource-policy.nri.io/pod": `type: glob
paths:
  - /*
  - /yet*anotherglobbedpath2
`,
			"test.topologyhints.resource-policy.nri.io": `test:
  Provider: /boot
  CPUs: ` + cpulist + `
  NUMAs: ` + numanode,
		}
		annotations6 = map[string]string{
			"allow.topologyhints.resource-policy.nri.io/pod": `this will fail unmarshaling
  - /boot
  - /anotherglobbedpath*2
`,
			"deny.topologyhints.resource-policy.nri.io/pod": `type: glob
paths:
  - /*
  - /yet*anotherglobbedpath2
`,
			"test.topologyhints.resource-policy.nri.io": `test:
  Provider: /boot
  CPUs: ` + cpulist + `
  NUMAs: ` + numanode,
		}
	)

	require.True(t, checkAllowedHints(t, annotations1, 1))
	require.True(t, checkAllowedHints(t, annotations2, 0))
	require.True(t, checkAllowedHints(t, annotations3, 1))
	require.True(t, checkAllowedHints(t, annotations4, 0))
	require.True(t, checkAllowedHints(t, annotations5, 1))
	require.False(t, checkAllowedHints(t, annotations6, 0))
}

func checkAllowedHints(t *testing.T, annotations map[string]string, expectedHints int) bool {
	t.Helper()

	var (
		// Note that we assume here that /boot is installed in a non virtual
		// device node so that the code in topology.go can resolve it to a real device.
		hostPath = "/boot"
		mounts   = []*nri.Mount{
			{
				Source:      "/dir",
				Destination: hostPath,
				Type:        "bind",
				Options:     []string{"bind", "rw"},
			},
		}
		nriPods = []*nri.PodSandbox{
			makePod(WithPodAnnotations(annotations)),
		}
		nriCtrs = []*nri.Container{
			makeCtr(WithCtrPodID(nriPods[0].GetId()),
				WithCtrState(cache.ContainerStateRunning),
				WithCtrMounts(mounts),
			),
		}
		pathList cache.PathList
	)

	// Instead of trying to go through the complex and fragile process of setting up
	// a fake sysfs for pkg/topology to generate the desired topology hint for /boot,
	// we now have allow/deny-list filtering in place for annotated test hints and use
	// annotated hints here (for testing the filtering logic).
	if err := os.Setenv(utils.EnvVarEnableTestAPIs, "true"); err != nil {
		log.Get("cache").Errorf("Failed to set environment variable %s: %v",
			utils.EnvVarEnableTestAPIs, err)
		return false
	}

	_, _, ctrs := makePopulatedCache(t, nriPods, nriCtrs)
	for _, ctr := range ctrs {
		// First part of the checks make sure that the yaml
		// above is properly set. Not strictly necessary but
		// make sure we do not have wrong stuff there.
		ann := "allow" + "." + cache.TopologyHintsKey
		allow, ok := ctr.GetEffectiveAnnotation(ann)
		if !ok {
			log.Get("cache").Errorf("unable to get annotation %s (%s)", ann, allow)
			return false
		}

		if err := yaml.Unmarshal([]byte(allow), &pathList); err != nil {
			log.Get("cache").Errorf("Error (%v) when trying to parse \"%s\"", err, allow)
			return false
		}

		ann = "deny" + "." + cache.TopologyHintsKey
		deny, ok := ctr.GetEffectiveAnnotation(ann)
		if !ok {
			log.Get("cache").Errorf("unable to get annotation %s (%s)", ann, deny)
			return false
		}

		if err := yaml.Unmarshal([]byte(deny), &pathList); err != nil {
			log.Get("cache").Errorf("Error (%v) when trying to parse \"%s\"", err, deny)
			return false
		}

		// Then we check that we get proper hints from the APIs
		hints := ctr.GetTopologyHints()
		require.Len(t, hints, expectedHints)

		for _, hint := range hints {
			require.Equal(t, cpulist, hint.CPUs)
			require.Equal(t, numanode, hint.NUMAs)
		}
	}

	return true
}

type CtrOption func(*nri.Container) error

func WithCtrName(name string) CtrOption {
	return func(nriCtr *nri.Container) error {
		nriCtr.Name = name
		return nil
	}
}

func WithCtrPodID(id string) CtrOption {
	return func(nriCtr *nri.Container) error {
		nriCtr.PodSandboxId = id
		return nil
	}
}

func WithCtrState(state cache.ContainerState) CtrOption {
	return func(nriCtr *nri.Container) error {
		nriCtr.State = state
		return nil
	}
}

func WithCtrArgs(args []string) CtrOption {
	return func(nriCtr *nri.Container) error {
		if args == nil {
			nriCtr.Args = nil
			return nil
		}
		nriCtr.Args = slices.Clone(args)
		return nil
	}
}

func WithCtrEnv(env []string) CtrOption {
	return func(nriCtr *nri.Container) error {
		if env == nil {
			nriCtr.Env = nil
			return nil
		}
		nriCtr.Env = slices.Clone(env)
		return nil
	}
}

func WithCtrMounts(mounts []*nri.Mount) CtrOption {
	return func(nriCtr *nri.Container) error {
		if mounts == nil {
			nriCtr.Mounts = nil
			return nil
		}
		nriCtr.Mounts = make([]*nri.Mount, len(mounts))
		for i, m := range mounts {
			nriCtr.Mounts[i] = &nri.Mount{
				Destination: m.Destination,
				Source:      m.Source,
				Type:        m.Type,
				Options:     slices.Clone(m.Options),
			}
		}
		return nil
	}
}

func WithCtrDevices(devices []*nri.LinuxDevice) CtrOption {
	return func(nriCtr *nri.Container) error {
		if devices == nil {
			if nriCtr.Linux != nil {
				nriCtr.Linux.Devices = nil
			}
			return nil
		}
		if nriCtr.Linux == nil {
			nriCtr.Linux = &nri.LinuxContainer{}
		}
		nriCtr.Linux.Devices = make([]*nri.LinuxDevice, len(devices))
		for i, d := range devices {
			nriCtr.Linux.Devices[i] = &nri.LinuxDevice{
				Path:     d.Path,
				Type:     d.Type,
				Major:    d.Major,
				Minor:    d.Minor,
				Uid:      nri.UInt32(d.Uid),
				Gid:      nri.UInt32(d.Gid),
				FileMode: nri.FileMode(d.FileMode),
			}
		}
		return nil
	}
}

func WithCtrLabels(labels map[string]string) CtrOption {
	return func(nriCtr *nri.Container) error {
		if labels == nil {
			nriCtr.Labels = nil
			return nil
		}
		nriCtr.Labels = make(map[string]string)
		maps.Copy(nriCtr.Labels, labels)
		return nil
	}
}

func WithCtrAnnotations(annotations map[string]string) CtrOption {
	return func(nriCtr *nri.Container) error {
		if annotations == nil {
			nriCtr.Annotations = nil
			return nil
		}
		nriCtr.Annotations = make(map[string]string)
		maps.Copy(nriCtr.Annotations, annotations)
		return nil
	}
}

func makeCtr(options ...CtrOption) *nri.Container {
	id := ctrID.Generate()
	ctr := &nri.Container{
		Id:    id,
		Name:  "ctrName-" + id,
		State: cache.ContainerStateCreating,
	}
	for _, o := range options {
		if err := o(ctr); err != nil {
			panic(fmt.Errorf("failed to make Container: %w", err))
		}
	}
	return ctr
}

func jsonEncode(t *testing.T, o any) string {
	t.Helper()

	bytes, err := json.Marshal(o)
	require.NoError(t, err)
	return string(bytes)
}
