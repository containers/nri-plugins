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

package cache_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	nri "github.com/containerd/nri/pkg/api"
	"github.com/containers/nri-plugins/pkg/kubernetes"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("Pod", func() {
	It("can return its ID", func() {
		var (
			pods    []cache.Pod
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)
		Expect(pods[0].GetID()).To(Equal(nriPods[0].GetId()))
	})

	It("can return its UID", func() {
		var (
			pods    []cache.Pod
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)
		Expect(pods[0].GetUID()).To(Equal(nriPods[0].GetUid()))
	})

	It("can return its name", func() {
		var (
			pods    []cache.Pod
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)
		Expect(pods[0].GetName()).To(Equal(nriPods[0].GetName()))
	})

	It("can return its Kubernetes namespace", func() {
		var (
			pods    []cache.Pod
			nriPods = []*nri.PodSandbox{
				makePod(),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)
		Expect(pods[0].GetNamespace()).To(Equal(nriPods[0].GetNamespace()))
	})

	It("can look up labels", func() {
		var (
			pods   []cache.Pod
			labels = map[string]string{
				"key1": "value1",
				"key2": "value2",
			}
			nriPods = []*nri.PodSandbox{
				makePod(WithPodLabels(labels)),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)
		for key, val := range labels {
			chk, ok := pods[0].GetLabel(key)
			Expect(chk).To(Equal(val))
			Expect(ok).To(BeTrue())
		}
	})

	It("can look up annotations", func() {
		var (
			pods        []cache.Pod
			annotations = map[string]string{
				"key1": "value1",
				"key2": "value2",
			}
			nriPods = []*nri.PodSandbox{
				makePod(WithPodAnnotations(annotations)),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)
		for key, val := range annotations {
			chk, ok := pods[0].GetAnnotation(key)
			Expect(chk).To(Equal(val))
			Expect(ok).To(BeTrue())
		}
	})

	It("can return its cgroup parent", func() {
		var (
			pods         []cache.Pod
			cgroupParent = "/cgroup/parent/dir"
			nriPods      = []*nri.PodSandbox{
				makePod(WithCgroupParent(cgroupParent)),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)
		Expect(pods[0].GetCgroupParent()).To(Equal(cgroupParent))
	})

	It("can return its QoS class", func() {
		var (
			pods    []cache.Pod
			nriPods = []*nri.PodSandbox{
				makePod(WithCgroupParent("/a/besteffort/pod")),
				makePod(WithCgroupParent("/a/burstable/pod")),
				makePod(WithCgroupParent("/a/guaranteed/pod")),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)
		Expect(pods[0].GetQOSClass()).To(Equal(corev1.PodQOSBestEffort))
		Expect(pods[1].GetQOSClass()).To(Equal(corev1.PodQOSBurstable))
		Expect(pods[2].GetQOSClass()).To(Equal(corev1.PodQOSGuaranteed))
	})

	It("can look up annotations in the resmgr key namespace", func() {
		var (
			pods        []cache.Pod
			annotations = map[string]string{
				"key1": "value1",
				"key2": "value2",
			}
			podAnnotations = map[string]string{
				kubernetes.ResmgrKey("key1"): "value1",
				kubernetes.ResmgrKey("key2"): "value2",
			}
			nriPods = []*nri.PodSandbox{
				makePod(WithPodAnnotations(podAnnotations)),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)
		for key, val := range annotations {
			chk, ok := pods[0].GetResmgrAnnotation(key)
			Expect(chk).To(Equal(val))
			Expect(ok).To(BeTrue())
		}
	})

	It("can return the correct effective annotations for container names", func() {
		var (
			pods        []cache.Pod
			annotations = map[string]string{
				"test-key":                "test-value",
				"test-key/container.ctr1": "ctr1-value",
				"test-key/container.ctr2": "ctr2-value",
				"test-key/container.ctr3": "ctr3-value",
				"test-key1/pod":           "pod-value",
			}
			tests = []struct {
				key    string
				name   string
				result string
				ok     bool
			}{
				{"test-key", "ctr0", "test-value", true},
				{"test-key", "ctr1", "ctr1-value", true},
				{"test-key", "ctr2", "ctr2-value", true},
				{"test-key", "ctr3", "ctr3-value", true},
				{"test-key", "ctr4", "test-value", true},
				{"test-key1", "ctr5", "pod-value", true},
				{"test-key2", "ctr0", "", false},
			}
			nriPods = []*nri.PodSandbox{
				makePod(WithPodAnnotations(annotations)),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)

		for _, t := range tests {
			result, ok := pods[0].GetEffectiveAnnotation(t.key, t.name)
			Expect(result).To(Equal(t.result))
			Expect(ok).To(Equal(t.ok))
		}
	})

	It("produces pretty pod names as expected", func() {
		var (
			pods    []cache.Pod
			nriPods = []*nri.PodSandbox{
				makePod(
					WithNamespace("default"),
					WithPodName("test-pod1"),
				),
				makePod(
					WithNamespace("non-default"),
					WithPodName("test-pod2"),
				),
			}
		)

		_, pods, _ = makePopulatedCache(nriPods, nil)

		Expect(pods[0].PrettyName()).To(Equal(pods[0].GetNamespace() + "/" + pods[0].GetName()))
		Expect(pods[1].PrettyName()).To(Equal(pods[1].GetNamespace() + "/" + pods[1].GetName()))
	})
})

type PodOption func(*nri.PodSandbox) error

func WithPodID(id string) PodOption {
	return func(nriPod *nri.PodSandbox) error {
		nriPod.Id = id
		return nil
	}
}

func WithPodName(name string) PodOption {
	return func(nriPod *nri.PodSandbox) error {
		nriPod.Name = name
		return nil
	}
}

func WithNamespace(ns string) PodOption {
	return func(nriPod *nri.PodSandbox) error {
		nriPod.Namespace = ns
		return nil
	}
}

func WithPodLabels(labels map[string]string) PodOption {
	return func(nriPod *nri.PodSandbox) error {
		if labels == nil {
			nriPod.Labels = nil
			return nil
		}
		nriPod.Labels = make(map[string]string)
		for k, v := range labels {
			nriPod.Labels[k] = v
		}
		return nil
	}
}

func WithPodAnnotations(annotations map[string]string) PodOption {
	return func(nriPod *nri.PodSandbox) error {
		if annotations == nil {
			nriPod.Annotations = nil
			return nil
		}
		nriPod.Annotations = make(map[string]string)
		for k, v := range annotations {
			nriPod.Annotations[k] = v
		}
		return nil
	}
}

func WithCgroupParent(cgroupParent string) PodOption {
	return func(nriPod *nri.PodSandbox) error {
		nriPod.Linux.CgroupParent = cgroupParent
		return nil
	}
}

func makePod(options ...PodOption) *nri.PodSandbox {
	id := podID.Generate()
	pod := &nri.PodSandbox{
		Id:        id,
		Uid:       "uid-" + id,
		Name:      "podName-" + id,
		Namespace: "default",
		Linux: &nri.LinuxPodSandbox{
			CgroupParent: "/maybe/a/besteffort/pod",
		},
	}
	for _, o := range options {
		if err := o(pod); err != nil {
			panic(fmt.Errorf("failed to make Pod: %w", err))
		}
	}
	return pod
}
