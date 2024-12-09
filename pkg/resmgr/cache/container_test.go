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
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/topology"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	nri "github.com/containerd/nri/pkg/api"
	"golang.org/x/sys/unix"
	"sigs.k8s.io/yaml"
)

var (
	testdataDir     string
	enableDebugging bool = false

	cpulist  string = "0-7"
	numanode string = "0"
)

var _ = BeforeSuite(func() {
	pwd, err := os.Getwd()
	if err != nil {
		panic(fmt.Errorf("unable to get current directory: %w", err))
	}

	if path, err := filepath.EvalSymlinks(pwd); err == nil {
		pwd = path
	}

	l := log.Get("topology-hints")
	topology.SetLogger(l)

	c := log.Get("cache")

	if enableDebugging {
		l.EnableDebug(true)
		c.EnableDebug(true)
	}

	testdataDir = pwd + "/testdata"
	topology.SetSysRoot(testdataDir)
})

var _ = AfterSuite(func() {
	topology.SetSysRoot("")
})

var _ = Describe("Container", func() {
	It("can return its ID", func() {
		var (
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId())),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		Expect(ctrs[0].GetID()).To(Equal(nriCtrs[0].GetId()))
	})

	It("can return its Pod ID", func() {
		var (
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId())),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		Expect(ctrs[0].GetPodID()).To(Equal(nriPods[0].GetId()))
	})

	It("can return its Pod", func() {
		var (
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId())),
			}
		)

		_, pods, ctrs := makePopulatedCache(nriPods, nriCtrs)

		pod, ok := ctrs[0].GetPod()
		Expect(pod).To(Equal(pods[0]))
		Expect(ok).To(BeTrue())
	})

	It("can return its name", func() {
		var (
			name    = "test-ctr"
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId()), WithCtrName(name)),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		Expect(ctrs[0].GetName()).To(Equal(name))
	})

	It("can return its Kubernetes namespace", func() {
		var (
			namespace = "test-namespace"
			nriPods   = []*nri.PodSandbox{
				makePod(WithNamespace(namespace)),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId())),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		Expect(ctrs[0].GetNamespace()).To(Equal(namespace))
	})

	It("can return its state", func() {
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		Expect(ctrs[0].GetState()).To(Equal(state))
	})

	It("can return its QoS class", func() {
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		Expect(ctrs[0].GetQOSClass()).To(Equal(corev1.PodQOSBestEffort))
		Expect(ctrs[1].GetQOSClass()).To(Equal(corev1.PodQOSBurstable))
		Expect(ctrs[2].GetQOSClass()).To(Equal(corev1.PodQOSGuaranteed))
	})

	It("can return its args", func() {
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		Expect(ctrs[0].GetArgs()).To(Equal(args))
	})

	It("can look up its labels", func() {
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		for key, val := range labels {
			chk, ok := ctrs[0].GetLabel(key)
			Expect(chk).To(Equal(val))
			Expect(ok).To(BeTrue())
		}
	})

	It("can look up annotations", func() {
		var (
			objects = map[string]interface{}{
				"key1": true,
				"key2": "foobar",
				"key3": 3.141,
			}
			annotations = map[string]string{
				"key1": jsonEncode(objects["key1"]),
				"key2": jsonEncode(objects["key2"]),
				"key3": jsonEncode(objects["key3"]),
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		for key, val := range objects {
			var ok bool
			switch objects[key].(type) {
			case bool:
				var bln bool
				_, ok = ctrs[0].GetAnnotation(key, &bln)
				Expect(bln).To(Equal(val))
			case string:
				var str string
				_, ok = ctrs[0].GetAnnotation(key, &str)
				Expect(str).To(Equal(val))
			case float64:
				var flt float64
				_, ok = ctrs[0].GetAnnotation(key, &flt)
				Expect(flt).To(Equal(val))
			}
			Expect(ok).To(BeTrue())
		}
	})

	It("can look up its environment variables", func() {
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		for _, v := range vars {
			chk, ok := ctrs[0].GetEnv(v.Key)
			Expect(chk).To(Equal(v.Value))
			Expect(ok).To(BeTrue())
		}
	})

	It("can return its mounts", func() {
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		Expect(ctrs[0].GetMounts()).To(Equal(mounts))
	})

	It("can return its devices", func() {
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		Expect(ctrs[0].GetDevices()).To(Equal(devices))
	})

})

var _ = Describe("Container", func() {
	It("properly records CPU shares adjustment", func() {
		var (
			shares  = 999
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId())),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetCPUShares(int64(shares))

		pending := ctrs[0].GetPendingAdjustment()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetCpu().GetShares().GetValue()
		Expect(value).To(Equal(uint64(shares)))
	})

	It("properly records CPU quota adjustment", func() {
		var (
			quota   = 998
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId())),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetCPUQuota(int64(quota))

		pending := ctrs[0].GetPendingAdjustment()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetCpu().GetQuota().GetValue()
		Expect(value).To(Equal(int64(quota)))
	})

	It("properly records CPU period adjustment", func() {
		var (
			period  = 997
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId())),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetCPUPeriod(int64(period))

		pending := ctrs[0].GetPendingAdjustment()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetCpu().GetPeriod().GetValue()
		Expect(value).To(Equal(uint64(period)))
	})

	It("properly records cpuset CPU adjustment", func() {
		var (
			cpus    = "1-5,7"
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId())),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetCpusetCpus(cpus)

		pending := ctrs[0].GetPendingAdjustment()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetCpu().GetCpus()
		Expect(value).To(Equal(cpus))
	})

	It("properly records cpuset memory adjustment", func() {
		var (
			mems    = "0-2,4"
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId())),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetCpusetMems(mems)

		pending := ctrs[0].GetPendingAdjustment()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetCpu().GetMems()
		Expect(value).To(Equal(mems))
	})

	It("properly records memory limit adjustment", func() {
		var (
			limit   int64 = 123456789
			nriPods       = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(WithCtrPodID(nriPods[0].GetId())),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetMemoryLimit(limit)

		pending := ctrs[0].GetPendingAdjustment()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetMemory().GetLimit().GetValue()
		Expect(value).To(Equal(limit))
	})
})

var _ = Describe("Container", func() {
	It("properly records CPU shares update", func() {
		var (
			shares  = 999
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetCPUShares(int64(shares))

		pending := ctrs[0].GetPendingUpdate()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetCpu().GetShares().GetValue()
		Expect(value).To(Equal(uint64(shares)))
	})

	It("properly records CPU quota update", func() {
		var (
			quota   = 998
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetCPUQuota(int64(quota))

		pending := ctrs[0].GetPendingUpdate()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetCpu().GetQuota().GetValue()
		Expect(value).To(Equal(int64(quota)))
	})

	It("properly records CPU period update", func() {
		var (
			period  = 997
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetCPUPeriod(int64(period))

		pending := ctrs[0].GetPendingUpdate()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetCpu().GetPeriod().GetValue()
		Expect(value).To(Equal(uint64(period)))
	})

	It("properly records cpuset CPU update", func() {
		var (
			cpus    = "1-5,7"
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetCpusetCpus(cpus)

		pending := ctrs[0].GetPendingUpdate()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetCpu().GetCpus()
		Expect(value).To(Equal(cpus))
	})

	It("properly records cpuset memory update", func() {
		var (
			mems    = "0-2,4"
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

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetCpusetMems(mems)

		pending := ctrs[0].GetPendingUpdate()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetCpu().GetMems()
		Expect(value).To(Equal(mems))
	})

	It("properly records memory limit update", func() {
		var (
			limit   int64 = 123456789
			nriPods       = []*nri.PodSandbox{
				makePod(),
			}
			nriCtrs = []*nri.Container{
				makeCtr(
					WithCtrPodID(nriPods[0].GetId()),
					WithCtrState(cache.ContainerStateRunning),
				),
			}
		)

		_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)

		ctrs[0].SetMemoryLimit(limit)

		pending := ctrs[0].GetPendingUpdate()
		Expect(pending).ToNot(BeNil())
		value := pending.GetLinux().GetResources().GetMemory().GetLimit().GetValue()
		Expect(value).To(Equal(limit))
	})
})

var _ = Describe("Container", func() {
	It("properly filters topology hints for mounts", func() {
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
			}
		)

		Expect(checkAllowedHints(annotations1, 1)).To(Equal(true))
		Expect(checkAllowedHints(annotations2, 0)).To(Equal(true))
		Expect(checkAllowedHints(annotations3, 1)).To(Equal(true))
		Expect(checkAllowedHints(annotations4, 0)).To(Equal(true))
		Expect(checkAllowedHints(annotations5, 1)).To(Equal(true))
		Expect(checkAllowedHints(annotations6, 0)).To(Equal(false))
	})
})

func checkAllowedHints(annotations map[string]string, expectedHints int) bool {
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

	// We must setup the test-data directory so that the container.go:generateTopologyHints()
	// can do its magic and resolve the mount directory to proper sysfs device path.
	// This will work properly on non virtual devices and we are relaying here that /boot
	// directory is mounted to one. That is why the /boot directory is used as a mount path
	// above. So we first try to figure out where the host /boot directory is located, if
	// it is not pointing to real device, then this test cannot pass as topology.go will
	// check that (major number is 0 which indicates a virtual device node) and we just give a
	// warning message.
	err := setupSysFsDevice(hostPath)
	Expect(err).To(BeNil())

	_, _, ctrs := makePopulatedCache(nriPods, nriCtrs)
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
		Expect(len(hints)).To(Equal(expectedHints))

		for _, hint := range hints {
			Expect(hint.CPUs).To(Equal(cpulist))
			Expect(hint.NUMAs).To(Equal(numanode))
		}
	}

	return true
}

func createSysFsDevice(devType string, major, minor int64) error {
	devPath := fmt.Sprintf("/sys/dev/%s/%d:%d", devType, major, minor)
	if err := os.MkdirAll(testdataDir+"/"+devPath, 0770); err != nil {
		return err
	}

	realDevPath, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(testdataDir+"/"+realDevPath, 0770); err != nil {
		return err
	}

	realDevPath = testdataDir + "/" + realDevPath

	// Then create two files that topology.go expects to find somewhere there in the sysfs
	f, err := os.Create(realDevPath + "/local_cpulist")
	if err != nil {
		return err
	}

	if _, err := f.Write([]byte(cpulist)); err != nil {
		log.Get("cache").Errorf("unable to write to %s: %v", realDevPath+"/local_cpulist", err)
	}
	f.Close()

	f, err = os.Create(realDevPath + "/numa_node")
	if err != nil {
		return err
	}

	if _, err := f.Write([]byte(numanode)); err != nil {
		log.Get("cache").Errorf("unable to write to %s: %v", realDevPath+"/numa_node", err)
	}
	f.Close()

	return nil
}

func setupSysFsDevice(dev string) error {
	fi, err := os.Stat(dev)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no such file %s: %w", dev, err)
		}
		return fmt.Errorf("unable to get stat for %s: %w", dev, err)
	}

	devType := "block"
	rdev := fi.Sys().(*syscall.Stat_t).Dev
	if mode := fi.Mode(); mode&os.ModeDevice != 0 {
		rdev = fi.Sys().(*syscall.Stat_t).Rdev
		if mode&os.ModeCharDevice != 0 {
			devType = "char"
		}
	}

	major := int64(unix.Major(rdev))
	minor := int64(unix.Minor(rdev))
	if major == 0 {
		return fmt.Errorf("%s is a virtual device node", dev)
	}

	err = createSysFsDevice(devType, major, minor)
	if err != nil {
		return fmt.Errorf("failed to find sysfs device for %s: %w", dev, err)
	}

	return nil
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
		for k, v := range labels {
			nriCtr.Labels[k] = v
		}
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
		for k, v := range annotations {
			nriCtr.Annotations[k] = v
		}
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

func jsonEncode(o interface{}) string {
	bytes, err := json.Marshal(o)
	Expect(err).To(BeNil())
	return string(bytes)
}

/*
func TestGetKubeletHint(t *testing.T) {
	type T struct {
		name        string
		cpus        string
		mems        string
		expectedLen int
	}

	cases := []T{
		{
			name:        "empty",
			cpus:        "",
			mems:        "",
			expectedLen: 0,
		},
		{
			name:        "cpus",
			cpus:        "0-9",
			mems:        "",
			expectedLen: 1,
		},
		{
			name:        "mems",
			cpus:        "",
			mems:        "0,1",
			expectedLen: 1,
		},
		{
			name:        "both",
			cpus:        "0-9",
			mems:        "0,1",
			expectedLen: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := getKubeletHint(tc.cpus, tc.mems)
			if len(output) != tc.expectedLen {
				t.Errorf("expected len of hints: %d, got: %d, hints: %+v", tc.expectedLen, len(output), output)
			}
		})
	}
}

func TestGetTopologyHints(t *testing.T) {
	type T struct {
		name          string
		hostPath      string
		containerPath string
		readOnly      bool
		expectedLen   int
	}

	cases := []T{
		{
			name:          "read-only",
			hostPath:      "/something",
			containerPath: "/something",
			readOnly:      true,
		},
		{
			name:          "host /etc",
			hostPath:      "/etc/something",
			containerPath: "/data/something",
		},
		{
			name:          "container /etc",
			hostPath:      "/var/lib/kubelet/pods/0c9bcfc4-c51b-11e9-ac9a-b8aeed7c7427/etc-hosts",
			containerPath: "/etc/hosts",
		},
		{
			name:          "ConfigMap",
			containerPath: "/var/lib/kube-proxy",
			hostPath:      "/var/lib/kubelet/pods/0c9bcfc4-c51b-11e9-ac9a-b8aeed7c7427/volumes/kubernetes.io~configmap/kube-proxy",
		},
		{
			name:          "secret",
			containerPath: "/var/run/secrets/kubernetes.io/serviceaccount",
			hostPath:      "/var/lib/kubelet/pods/0c9bcfc4-c51b-11e9-ac9a-b8aeed7c7427/volumes/kubernetes.io~secret/kube-proxy-token-d9slz",
		},
		{
			name:          "dev null",
			hostPath:      "/dev/null",
			containerPath: "/dev/null",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := getTopologyHintsForMount(tc.hostPath, tc.containerPath, tc.readOnly)
			if len(output) != tc.expectedLen {
				t.Errorf("expected len of hints: %d, got: %d, hints: %+v", tc.expectedLen, len(output), output)
			}
		})
	}
}
*/
