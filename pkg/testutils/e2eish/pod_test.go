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

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resapi "k8s.io/apimachinery/pkg/api/resource"
)

func TestPod(t *testing.T) {
	p := NewPod("pod0",
		InNamespace("ns"),
		WithLabels(map[string]string{"app": "pod0"}),
		WithInitContainers(NewContainer("init", WithRequests("1", ""))),
		WithContainers(
			NewContainer("pod0c0", WithRequests("500m", "100M"), WithLimits("1", "100M")),
			NewContainer("pod0c1", WithExtendedResource("example.com/foo", "2")),
		),
		WithRunning("pod0c1"),
	)

	pod, err := p.k8s()
	require.NoError(t, err)
	require.Equal(t, "ns", pod.Namespace)
	require.Equal(t, map[string]string{"app": "pod0"}, pod.Labels)
	require.Equal(t, corev1.ResourceList{corev1.ResourceCPU: resapi.MustParse("1")},
		pod.Spec.InitContainers[0].Resources.Requests)
	require.Equal(t, Resources{CPUReq: 500, CPULim: 1000, MemReq: 100e6, MemLim: 100e6},
		ResourcesOf(&pod.Spec.Containers[0]))
	foo := corev1.ResourceList{"example.com/foo": resapi.MustParse("2")}
	require.Equal(t, foo, pod.Spec.Containers[1].Resources.Requests)
	require.Equal(t, foo, pod.Spec.Containers[1].Resources.Limits)
	require.Equal(t, "burstable", PodQOS(pod))

	sp, err := p.syncPod()
	require.NoError(t, err)
	require.Equal(t, []string{"pod0c1"}, sp.Running)
	sp, err = NewPod("pod1", WithContainers(NewContainer("a"), NewContainer("b"))).syncPod()
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, sp.Running)
	require.Equal(t, "default", sp.Namespace)

	_, err = NewPod("pod2", WithContainers(NewContainer("a", WithLimits("x", "")))).k8s()
	require.Error(t, err)
}

func TestPodSpec(t *testing.T) {
	pod := func(spec PodSpec, qos string) *corev1.Pod {
		t.Helper()
		p, err := NewPod(spec.Name, spec.pod(qos)...).k8s()
		require.NoError(t, err)
		return p
	}

	p := pod(PodSpec{Name: "pod0", Namespace: "ns", ContCount: 2, IContCount: 1, CPU: "2"}, Guaranteed)
	require.Equal(t, "ns", p.Namespace)
	require.Equal(t, map[string]string{"app": "pod0"}, p.Labels)
	require.Equal(t, "pod0c0i", p.Spec.InitContainers[0].Name)
	require.Equal(t, []string{"pod0c0", "pod0c1"}, []string{p.Spec.Containers[0].Name, p.Spec.Containers[1].Name})
	require.Equal(t, Resources{CPUReq: 2000, CPULim: 2000, MemReq: 100e6, MemLim: 100e6}, ResourcesOf(&p.Spec.Containers[1]))
	require.Equal(t, "", PodQOS(p))

	p = pod(PodSpec{Name: "pod1", CPULim: "0", MemLim: "300M", Labels: map[string]string{"a": "b"}}, Burstable)
	require.Equal(t, map[string]string{"app": "pod1", "a": "b"}, p.Labels)
	require.Equal(t, Resources{CPUReq: 1000, MemReq: 100e6, MemLim: 300e6}, ResourcesOf(&p.Spec.Containers[0]))
	require.Equal(t, "burstable", PodQOS(p))

	p = pod(PodSpec{Name: "pod2"}, BestEffort)
	require.Equal(t, Resources{}, ResourcesOf(&p.Spec.Containers[0]))
	require.Equal(t, "besteffort", PodQOS(p))
}
