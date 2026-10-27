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
	"maps"
	"testing"

	nri "github.com/containerd/nri/pkg/api"
	"github.com/containers/nri-plugins/pkg/kubernetes"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestPodID(t *testing.T) {
	nriPods := []*nri.PodSandbox{
		makePod(),
	}

	_, pods, _ := makePopulatedCache(t, nriPods, nil)
	require.Equal(t, nriPods[0].GetId(), pods[0].GetID())
}

func TestPodUID(t *testing.T) {
	nriPods := []*nri.PodSandbox{
		makePod(),
	}

	_, pods, _ := makePopulatedCache(t, nriPods, nil)
	require.Equal(t, nriPods[0].GetUid(), pods[0].GetUID())
}

func TestPodName(t *testing.T) {
	nriPods := []*nri.PodSandbox{
		makePod(),
	}

	_, pods, _ := makePopulatedCache(t, nriPods, nil)
	require.Equal(t, nriPods[0].GetName(), pods[0].GetName())
}

func TestPodNamespace(t *testing.T) {
	nriPods := []*nri.PodSandbox{
		makePod(),
	}

	_, pods, _ := makePopulatedCache(t, nriPods, nil)
	require.Equal(t, nriPods[0].GetNamespace(), pods[0].GetNamespace())
}

func TestPodLabels(t *testing.T) {
	var (
		labels = map[string]string{
			"key1": "value1",
			"key2": "value2",
		}
		nriPods = []*nri.PodSandbox{
			makePod(WithPodLabels(labels)),
		}
	)

	_, pods, _ := makePopulatedCache(t, nriPods, nil)
	for key, val := range labels {
		chk, ok := pods[0].GetLabel(key)
		require.Equal(t, val, chk)
		require.True(t, ok)
	}
}

func TestPodAnnotations(t *testing.T) {
	var (
		annotations = map[string]string{
			"key1": "value1",
			"key2": "value2",
		}
		nriPods = []*nri.PodSandbox{
			makePod(WithPodAnnotations(annotations)),
		}
	)

	_, pods, _ := makePopulatedCache(t, nriPods, nil)
	for key, val := range annotations {
		chk, ok := pods[0].GetAnnotation(key)
		require.Equal(t, val, chk)
		require.True(t, ok)
	}
}

func TestPodCgroupParent(t *testing.T) {
	var (
		cgroupParent = "/cgroup/parent/dir"
		nriPods      = []*nri.PodSandbox{
			makePod(WithCgroupParent(cgroupParent)),
		}
	)

	_, pods, _ := makePopulatedCache(t, nriPods, nil)
	require.Equal(t, cgroupParent, pods[0].GetCgroupParent())
}

func TestPodQOSClass(t *testing.T) {
	nriPods := []*nri.PodSandbox{
		makePod(WithCgroupParent("/a/besteffort/pod")),
		makePod(WithCgroupParent("/a/burstable/pod")),
		makePod(WithCgroupParent("/a/guaranteed/pod")),
	}

	_, pods, _ := makePopulatedCache(t, nriPods, nil)
	require.Equal(t, corev1.PodQOSBestEffort, pods[0].GetQOSClass())
	require.Equal(t, corev1.PodQOSBurstable, pods[1].GetQOSClass())
	require.Equal(t, corev1.PodQOSGuaranteed, pods[2].GetQOSClass())
}

func TestPodResmgrAnnotations(t *testing.T) {
	var (
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

	_, pods, _ := makePopulatedCache(t, nriPods, nil)
	for key, val := range annotations {
		chk, ok := pods[0].GetResmgrAnnotation(key)
		require.Equal(t, val, chk)
		require.True(t, ok)
	}
}

func TestPodEffectiveAnnotations(t *testing.T) {
	var (
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

	_, pods, _ := makePopulatedCache(t, nriPods, nil)

	for _, tc := range tests {
		result, ok := pods[0].GetEffectiveAnnotation(tc.key, tc.name)
		require.Equal(t, tc.result, result)
		require.Equal(t, tc.ok, ok)
	}
}

func TestPodPrettyName(t *testing.T) {
	nriPods := []*nri.PodSandbox{
		makePod(
			WithNamespace("default"),
			WithPodName("test-pod1"),
		),
		makePod(
			WithNamespace("non-default"),
			WithPodName("test-pod2"),
		),
	}

	_, pods, _ := makePopulatedCache(t, nriPods, nil)

	require.Equal(t, pods[0].GetNamespace()+"/"+pods[0].GetName(), pods[0].PrettyName())
	require.Equal(t, pods[1].GetNamespace()+"/"+pods[1].GetName(), pods[1].PrettyName())
}

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
		maps.Copy(nriPod.Labels, labels)
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
		maps.Copy(nriPod.Annotations, annotations)
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
