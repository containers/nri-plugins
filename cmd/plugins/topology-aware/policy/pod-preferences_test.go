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

package topologyaware

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resapi "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func boolPtr(v *bool) bool {
	if v != nil {
		return *v
	}
	return false
}

func TestPodIsolationPreference(t *testing.T) {
	tcases := []struct {
		name            string
		pod             *mockPod
		container       *mockContainer
		expectedIsolate bool
		expectedKind    prefKind
		optValue        *bool
		disabled        bool
	}{
		{
			name:     "podIsolationPreference() should handle nil pod arg gracefully",
			disabled: true,
		},
		{
			name:            "return defaults",
			pod:             &mockPod{},
			container:       &mockContainer{},
			expectedIsolate: boolPtr(opt.PreferIsolated),
		},
		{
			name: "prefer resmgr's annotation value",
			pod: &mockPod{
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.c0": "true",
				},
			},
			container:       &mockContainer{name: "c0"},
			expectedIsolate: true,
			expectedKind:    prefAnnotated,
		},
		{
			name: "return defaults for unparsable",
			pod: &mockPod{
				returnValue1FotGetResmgrAnnotation: "UNPARSABLE",
				returnValue2FotGetResmgrAnnotation: true,
			},
			container:       &mockContainer{},
			expectedIsolate: boolPtr(opt.PreferIsolated),
		},
		{
			name: "podIsolationPreference() should handle nil container arg gracefully",
			pod: &mockPod{
				returnValue1FotGetResmgrAnnotation: "key: true",
				returnValue2FotGetResmgrAnnotation: true,
			},
			container: &mockContainer{},
			disabled:  true,
		},
		{
			name: "return defaults for missing preferences",
			pod: &mockPod{
				returnValue1FotGetResmgrAnnotation: "key: true",
				returnValue2FotGetResmgrAnnotation: true,
			},
			container:       &mockContainer{},
			expectedIsolate: boolPtr(opt.PreferIsolated),
		},
		{
			name: "return defined preferences",
			pod: &mockPod{
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.testcontainer": "false",
				},
			},
			container: &mockContainer{
				name: "testcontainer",
			},
			expectedKind: prefAnnotated,
		},
		// effective annotation tests
		{
			name: "prefer resmgr's annotation value",
			pod: &mockPod{
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.c0": "true",
				},
			},
			container:       &mockContainer{name: "c0"},
			expectedIsolate: true,
			expectedKind:    prefAnnotated,
		},
		{
			name: "prefer resmgr's annotation value",
			pod: &mockPod{
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.c0": "false",
				},
			},
			container:       &mockContainer{name: "c0"},
			expectedIsolate: false,
			expectedKind:    prefAnnotated,
		},
		{
			name: "return defaults for unparsable annotation value",
			pod: &mockPod{
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.c0": "blah",
				},
			},
			container:       &mockContainer{name: "c0"},
			expectedIsolate: boolPtr(opt.PreferIsolated),
		},
		{
			name: "return defaults for missing preferences",
			pod: &mockPod{
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.c0": "true",
				},
			},
			container:       &mockContainer{name: "c1"},
			expectedIsolate: boolPtr(opt.PreferIsolated),
		},
	}

	opt.PreferIsolated = nil
	opt.PreferShared = nil

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.disabled {
				t.Skipf("The case '%s' is skipped", tc.name)
			}
			isolate, kind := isolatedCPUsPreference(tc.pod, tc.container)
			require.Equal(t, tc.expectedIsolate, isolate, "isolation preference")
			require.Equal(t, tc.expectedKind, kind, "preference kind")
		})
	}
}

func TestPodSharedCPUPreference(t *testing.T) {
	tcases := []struct {
		name           string
		pod            *mockPod
		container      *mockContainer
		expectedShared bool
		disabled       bool
	}{
		{
			name:     "podSharedCPUPreference() should handle nil pod arg gracefully",
			disabled: true,
		},
		{
			name:           "return defaults",
			pod:            &mockPod{},
			container:      &mockContainer{},
			expectedShared: boolPtr(opt.PreferShared),
		},
		{
			name: "prefer resmgr's annotation value",
			pod: &mockPod{
				annotations: map[string]string{
					preferSharedCPUsKey + "/container.c0": "true",
				},
			},
			container:      &mockContainer{name: "c0"},
			expectedShared: true,
		},
		{
			name: "return defaults for unparsable",
			pod: &mockPod{
				returnValue1FotGetResmgrAnnotation: "UNPARSABLE",
				returnValue2FotGetResmgrAnnotation: true,
			},
			container:      &mockContainer{},
			expectedShared: boolPtr(opt.PreferShared),
		},
		{
			name: "podSharedCPUPreference() should handle nil container arg gracefully",
			pod: &mockPod{
				returnValue1FotGetResmgrAnnotation: "key: true",
				returnValue2FotGetResmgrAnnotation: true,
			},
			container: &mockContainer{},
			disabled:  true,
		},
		{
			name: "return defaults for missing preferences",
			pod: &mockPod{
				returnValue1FotGetResmgrAnnotation: "key: true",
				returnValue2FotGetResmgrAnnotation: true,
			},
			container:      &mockContainer{},
			expectedShared: boolPtr(opt.PreferShared),
		},
		{
			name: "return defined preferences",
			pod: &mockPod{
				returnValue1FotGetResmgrAnnotation: "testcontainer: \"false\"",
				returnValue2FotGetResmgrAnnotation: true,
			},
			container: &mockContainer{
				name: "testcontainer",
			},
		},
		{
			name: "return defaults for unparsable annotation value",
			pod: &mockPod{
				returnValue1FotGetResmgrAnnotation: "testcontainer: UNPARSABLE",
				returnValue2FotGetResmgrAnnotation: true,
			},
			container: &mockContainer{
				name: "testcontainer",
			},
			expectedShared: boolPtr(opt.PreferShared),
		},
		// effective annotation tests
		{
			name: "prefer resmgr's annotation value",
			pod: &mockPod{
				annotations: map[string]string{
					preferSharedCPUsKey + "/container.c0": "true",
				},
			},
			container:      &mockContainer{name: "c0"},
			expectedShared: true,
		},
		{
			name: "prefer resmgr's annotation value",
			pod: &mockPod{
				annotations: map[string]string{
					preferSharedCPUsKey + "/container.c0": "false",
				},
			},
			container:      &mockContainer{name: "c0"},
			expectedShared: false,
		},
		{
			name: "return defaults for unparsable annotation value",
			pod: &mockPod{
				annotations: map[string]string{
					preferSharedCPUsKey + "/container.c0": "blah",
				},
			},
			container:      &mockContainer{name: "c0"},
			expectedShared: boolPtr(opt.PreferShared),
		},
		{
			name: "return defaults for missing preferences",
			pod: &mockPod{
				annotations: map[string]string{
					preferSharedCPUsKey + "/container.c0": "true",
				},
			},
			container:      &mockContainer{name: "c1"},
			expectedShared: boolPtr(opt.PreferShared),
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.disabled {
				t.Skipf("The case '%s' is skipped", tc.name)
			}
			shared, _ := sharedCPUsPreference(tc.pod, tc.container)
			require.Equal(t, tc.expectedShared, shared, "shared preference")
		})
	}
}

func TestCpuAllocationPreferences(t *testing.T) {
	tcases := []struct {
		name                   string
		pod                    *mockPod
		container              *mockContainer
		preferIsolated         bool
		preferShared           bool
		expectedFull           int
		expectedFraction       int
		expectedIsolate        bool
		expectedCpuType        cpuClass
		disabled               bool
		reservedPoolNamespaces []string
	}{
		{
			name:     "cpuAllocationPreferences() should handle nil container arg gracefully",
			disabled: true,
		},
		{
			name:      "no resource requirements",
			pod:       &mockPod{},
			container: &mockContainer{},
		},
		{
			name: "cpuAllocationPreferences() should handle nil pod arg gracefully",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1"),
					},
				},
			},
			disabled: true,
		},
		{
			name: "return defaults",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction: 1000,
			expectedIsolate:  false,
		},
		{
			name: "return request's value for system container",
			container: &mockContainer{
				namespace: metav1.NamespaceSystem,
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction: 2000,
			expectedCpuType:  cpuReserved,
		},
		{
			name: "return request's value for burstable QoS",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction: 2000,
		},
		{
			name: "guaranteed QoS with sub-core request",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("750m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			expectedFull:     0,
			expectedFraction: 750,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with sub-core request, prefer isolated",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("750m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferIsolated:   true,
			expectedFull:     0,
			expectedFraction: 750,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with sub-core request, prefer shared",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("750m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferShared:     true,
			expectedFull:     0,
			expectedFraction: 750,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with sub-core request, prefer isolated & shared",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("750m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferIsolated:   true,
			preferShared:     true,
			expectedFull:     0,
			expectedFraction: 750,
			expectedIsolate:  false,
		},

		{
			name: "guaranteed QoS with single full core request, prefer isolated",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferIsolated:  true,
			expectedFull:    1,
			expectedIsolate: true,
		},
		{
			name: "guaranteed QoS with single full core request, prefer no isolated",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferIsolated:  false,
			expectedFull:    1,
			expectedIsolate: false,
		},
		{
			name: "guaranteed QoS with single full core request, prefer shared",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferShared:     true,
			expectedFull:     0,
			expectedFraction: 1000,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with single full core request, prefer isolated & shared",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferIsolated:   true,
			preferShared:     true,
			expectedFull:     0,
			expectedFraction: 1000,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with single full core request, annotated shared",
			container: &mockContainer{
				name: "testcontainer",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
				annotations: map[string]string{
					preferSharedCPUsKey + "/container.testcontainer": "true",
				},
			},
			preferIsolated:   true,
			preferShared:     true,
			expectedFull:     0,
			expectedFraction: 1000,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with single full core request, annotated no isolated",
			container: &mockContainer{
				name: "testcontainer",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.testcontainer": "false",
				},
			},
			preferIsolated:   true,
			preferShared:     true,
			expectedFull:     0,
			expectedFraction: 1000,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with potential mixed request",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			expectedFull:     1,
			expectedFraction: 500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with potential mixed request, prefer isolated",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferIsolated:   true,
			expectedFull:     1,
			expectedFraction: 500,
			expectedIsolate:  true,
		},
		{
			name: "guaranteed QoS with potential mixed request, prefer shared",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferShared:     true,
			expectedFull:     0,
			expectedFraction: 1500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with potential mixed request, prefer isolated & shared",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("1500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferIsolated:   true,
			preferShared:     true,
			expectedFull:     0,
			expectedFraction: 1500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core full request",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			expectedFull:    2,
			expectedIsolate: false,
		},
		{
			name: "guaranteed QoS with multi-core full request, prefer isolated",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferIsolated:  true,
			expectedFull:    2,
			expectedIsolate: false,
		},
		{
			name: "guaranteed QoS with multi-core full request, prefer shared",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferShared:     true,
			expectedFull:     0,
			expectedFraction: 2000,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core full request, prefer isolated & shared",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			preferIsolated:   true,
			preferShared:     true,
			expectedFull:     0,
			expectedFraction: 2000,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core full request, annotate isolated",
			container: &mockContainer{
				name: "testcontainer",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.testcontainer": "true",
				},
			},
			expectedFull:    2,
			expectedIsolate: true,
		},
		{
			name: "guaranteed QoS with multi-core full request, annotate shared",
			container: &mockContainer{
				name: "testcontainer",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
				annotations: map[string]string{
					preferSharedCPUsKey + "/container.testcontainer": "true",
				},
			},
			expectedFull:     0,
			expectedFraction: 2000,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core full request, annotate isolated & shared",
			container: &mockContainer{
				name: "testcontainer",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.testcontainer": "true",
					preferSharedCPUsKey + "/container.testcontainer":   "true",
				},
			},
			expectedFull:     0,
			expectedFraction: 2000,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core mixed request",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			expectedFull:     0,
			expectedFraction: 2500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core mixed request, prefer isolated",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			expectedFull:     0,
			expectedFraction: 2500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core mixed request, prefer shared",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			expectedFull:     0,
			expectedFraction: 2500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core mixed request, prefer isolated & shared",
			container: &mockContainer{
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
			},
			expectedFull:     0,
			expectedFraction: 2500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core mixed request, annotate isolated",
			container: &mockContainer{
				name: "testcontainer",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.testcontainer": "true",
				},
			},
			expectedFull:     0,
			expectedFraction: 2500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core mixed request, annotate shared",
			container: &mockContainer{
				name: "testcontainer",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
				annotations: map[string]string{
					preferSharedCPUsKey + "/container.testcontainer": "true",
				},
			},
			expectedFull:     0,
			expectedFraction: 2500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core mixed request, annotate isolated & shared",
			container: &mockContainer{
				name: "testcontainer",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.testcontainer": "true",
					preferSharedCPUsKey + "/container.testcontainer":   "true",
				},
			},
			expectedFull:     0,
			expectedFraction: 2500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core mixed request, annotate no shared",
			container: &mockContainer{
				name: "testcontainer",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
				annotations: map[string]string{
					preferSharedCPUsKey + "/container.testcontainer": "false",
				},
			},
			expectedFull:     2,
			expectedFraction: 500,
			expectedIsolate:  false,
		},
		{
			name: "guaranteed QoS with multi-core mixed request, annotate isolated, no shared",
			container: &mockContainer{
				name: "testcontainer",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2500m"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
				annotations: map[string]string{
					preferIsolatedCPUsKey + "/container.testcontainer": "true",
					preferSharedCPUsKey + "/container.testcontainer":   "false",
				},
			},
			expectedFull:     2,
			expectedFraction: 500,
			expectedIsolate:  true,
		},
		{
			name: "return request's value for reserved pool namespace container",
			container: &mockContainer{
				namespace: "foobar",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction:       2000,
			expectedCpuType:        cpuReserved,
			reservedPoolNamespaces: []string{"foobar"},
		},
		{
			name: "return request's value for reserved pool namespace container using a glob 1",
			container: &mockContainer{
				namespace: "foobar2",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction:       2000,
			expectedCpuType:        cpuReserved,
			reservedPoolNamespaces: []string{"foobar*"},
		},
		{
			name: "return request's value for reserved pool namespace container using a glob 2",
			container: &mockContainer{
				namespace: "foobar-testing",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction:       2000,
			expectedCpuType:        cpuReserved,
			reservedPoolNamespaces: []string{"barfoo", "foobar*"},
		},
		{
			name: "return request's value for reserved pool namespace container using a glob 3",
			container: &mockContainer{
				namespace: "testing",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction:       2000,
			expectedCpuType:        cpuNormal,
			reservedPoolNamespaces: []string{"barfoo", "foobar?"},
		},
		{
			name: "return request's value for reserved pool namespace container using a glob 4",
			container: &mockContainer{
				namespace: "1foobar2",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction:       2000,
			expectedCpuType:        cpuNormal,
			reservedPoolNamespaces: []string{"barfoo", "foobar?"},
		},
		{
			name: "return request's value for reserved pool namespace container using a glob 5",
			container: &mockContainer{
				namespace: "foobar12",
				returnValueForGetResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resapi.MustParse("2"),
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction:       2000,
			expectedCpuType:        cpuNormal,
			reservedPoolNamespaces: []string{"barfoo", "foobar?", "testing"},
		},
		{
			name: "return request's value for reserved cpu annotation container",
			container: &mockContainer{
				name: "testcontainer",
				pod: &mockPod{
					returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
					annotations: map[string]string{
						preferReservedCPUsKey + "/container.special": "false",
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction: 0,
			expectedCpuType:  cpuNormal,
		},
		{
			name: "return request's value for reserved cpu annotation container",
			container: &mockContainer{
				pod: &mockPod{
					returnValueFotGetQOSClass: corev1.PodQOSGuaranteed,
					annotations: map[string]string{
						preferReservedCPUsKey + "/pod": "true",
					},
				},
			},
			pod: &mockPod{
				returnValueFotGetQOSClass: corev1.PodQOSBurstable,
			},
			expectedFraction: 0,
			expectedCpuType:  cpuReserved,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.disabled {
				t.Skipf("The case '%s' is skipped", tc.name)
			}
			opt.PreferIsolated, opt.PreferShared = &tc.preferIsolated, &tc.preferShared
			opt.ReservedPoolNamespaces = tc.reservedPoolNamespaces
			full, fraction, _, isolate, cpuType, _ := cpuAllocationPreferences(tc.pod, tc.container)
			require.Equal(t, tc.expectedFull, full, "full CPU cores")
			require.Equal(t, tc.expectedFraction, fraction, "CPU core fraction")
			require.Equal(t, tc.expectedIsolate, isolate, "isolation preference")
			require.Equal(t, tc.expectedCpuType, cpuType, "CPU type preference")
		})
	}
}
