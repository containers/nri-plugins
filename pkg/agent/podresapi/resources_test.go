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

package podresapi_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	api "k8s.io/kubelet/pkg/apis/podresources/v1"

	. "github.com/containers/nri-plugins/pkg/agent/podresapi"
)

func TestGetContainer(t *testing.T) {
	type testCase struct {
		name              string
		podResources      *api.PodResources
		containerName     string
		expectedContainer *ContainerResources
	}

	for _, tc := range []*testCase{
		{
			name:              "no containers",
			podResources:      &api.PodResources{},
			containerName:     "test",
			expectedContainer: nil,
		},
		{
			name: "container not found",
			podResources: &api.PodResources{
				Containers: []*api.ContainerResources{
					{
						Name: "test1",
					},
				},
			},
			containerName:     "test",
			expectedContainer: nil,
		},
		{
			name: "the only container found",
			podResources: &api.PodResources{
				Containers: []*api.ContainerResources{
					{
						Name: "test",
					},
				},
			},
			containerName: "test",
			expectedContainer: &ContainerResources{
				&api.ContainerResources{
					Name: "test",
				},
			},
		},
		{
			name: "one of many containers found",
			podResources: &api.PodResources{
				Containers: []*api.ContainerResources{
					{
						Name: "test1",
					},
					{
						Name: "test2",
					},
					{
						Name: "test3",
					},
				},
			},
			containerName: "test2",
			expectedContainer: &ContainerResources{
				&api.ContainerResources{
					Name: "test2",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &PodResources{tc.podResources}

			ct := p.GetContainer(tc.containerName)
			require.Equal(t, tc.expectedContainer, ct)
		})
	}
}

func TestPodResourcesList(t *testing.T) {
	type lookup struct {
		Namespace string
		Pod       string
	}
	type testCase struct {
		name         string
		podResources []*api.PodResources
		lookup       []*lookup
		expect       []*PodResources
	}

	for _, tc := range []*testCase{
		{
			name:         "no pod resources",
			podResources: []*api.PodResources{},
			lookup: []*lookup{
				{
					Namespace: "test",
					Pod:       "pod1",
				},
			},
			expect: []*PodResources{
				nil,
			},
		},
		{
			name: "pod not found",
			podResources: []*api.PodResources{
				{
					Namespace: "test",
					Name:      "pod1",
				},
			},
			lookup: []*lookup{
				{
					Namespace: "test",
					Pod:       "pod2",
				},
			},
			expect: []*PodResources{
				nil,
			},
		},
		{
			name: "the only pod found",
			podResources: []*api.PodResources{
				{
					Namespace: "test",
					Name:      "pod1",
				},
			},
			lookup: []*lookup{
				{
					Namespace: "test",
					Pod:       "pod1",
				},
			},
			expect: []*PodResources{
				{
					&api.PodResources{
						Namespace: "test",
						Name:      "pod1",
					},
				},
			},
		},
		{
			name: "one of many pods found",
			podResources: []*api.PodResources{
				{
					Namespace: "test",
					Name:      "pod1",
				},
				{
					Namespace: "test",
					Name:      "pod2",
				},
				{
					Namespace: "test",
					Name:      "pod3",
				},
			},
			lookup: []*lookup{
				{
					Namespace: "test",
					Pod:       "pod2",
				},
			},
			expect: []*PodResources{
				{
					&api.PodResources{
						Namespace: "test",
						Name:      "pod2",
					},
				},
			},
		},
		{
			name: "all of many pods found",
			podResources: []*api.PodResources{
				{
					Namespace: "test1",
					Name:      "pod1",
				},
				{
					Namespace: "test1",
					Name:      "pod2",
				},
				{
					Namespace: "test1",
					Name:      "pod3",
				},
				{
					Namespace: "test2",
					Name:      "pod4",
				},
				{
					Namespace: "test3",
					Name:      "pod5",
				},
				{
					Namespace: "test3",
					Name:      "pod6",
				},
				{
					Namespace: "test1",
					Name:      "pod7",
				},
			},
			lookup: []*lookup{
				{
					Namespace: "test3",
					Pod:       "pod5",
				},
				{
					Namespace: "test1",
					Pod:       "pod7",
				},
				{
					Namespace: "test3",
					Pod:       "pod6",
				},
				{
					Namespace: "test2",
					Pod:       "pod4",
				},
				{
					Namespace: "test1",
					Pod:       "pod3",
				},

				{
					Namespace: "test1",
					Pod:       "pod2",
				},

				{
					Namespace: "test1",
					Pod:       "pod1",
				},
			},
			expect: []*PodResources{
				{
					&api.PodResources{
						Namespace: "test3",
						Name:      "pod5",
					},
				},
				{
					&api.PodResources{
						Namespace: "test1",
						Name:      "pod7",
					},
				},
				{
					&api.PodResources{
						Namespace: "test3",
						Name:      "pod6",
					},
				},
				{
					&api.PodResources{
						Namespace: "test2",
						Name:      "pod4",
					},
				},
				{
					&api.PodResources{
						Namespace: "test1",
						Name:      "pod3",
					},
				},
				{
					&api.PodResources{
						Namespace: "test1",
						Name:      "pod2",
					},
				},
				{
					&api.PodResources{
						Namespace: "test1",
						Name:      "pod1",
					},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewPodResourcesList(tc.podResources)

			for i, l := range tc.lookup {
				p := r.GetPodResources(l.Namespace, l.Pod)
				require.Equal(t, tc.expect[i], p)
			}
		})
	}
}
