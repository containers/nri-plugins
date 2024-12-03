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

package podresapi

import (
	"strconv"
	"strings"

	"github.com/containers/nri-plugins/pkg/topology"
	api "k8s.io/kubelet/pkg/apis/podresources/v1"
)

// PodResources contains resources for a pod.
type PodResources struct {
	*api.PodResources
}

// ContainerResources contains resources for a single container.
type ContainerResources struct {
	*api.ContainerResources
}

// PodResourcesList is a list of PodResources.
type PodResourcesList []*api.PodResources

// PodResourceMap is a map representation of PodResourcesList.
type PodResourcesMap map[string]map[string]*PodResources

// GetContainer returns resources for the given container.
func (p *PodResources) GetContainer(ctr string) *ContainerResources {
	if p == nil {
		return nil
	}

	for _, c := range p.GetContainers() {
		if c.GetName() == ctr {
			return &ContainerResources{c}
		}
	}

	return nil
}

// GetPodResources returns resources for the given pod.
func (l PodResourcesList) GetPodResources(ns, pod string) *PodResources {
	for _, p := range l {
		if p.GetNamespace() == ns && p.GetName() == pod {
			return &PodResources{p}
		}
	}

	return nil
}

// Map returns a PodResourcesMap for the pod resources list.
func (l PodResourcesList) Map() PodResourcesMap {
	m := make(PodResourcesMap)

	for _, p := range l {
		podMap, ok := m[p.GetNamespace()]
		if !ok {
			podMap = make(map[string]*PodResources)
			m[p.GetNamespace()] = podMap
		}
		podMap[p.GetName()] = &PodResources{p}
	}

	return m
}

// GetPod returns resources for the given pod.
func (m PodResourcesMap) GetPod(ns, pod string) *PodResources {
	return m[ns][pod]
}

// GetContainer returns resources for the given container.
func (m PodResourcesMap) GetContainer(ns, pod, ctr string) *ContainerResources {
	return m.GetPod(ns, pod).GetContainer(ctr)
}

// GetDeviceTopologyHints returns topology hints for the given container. checkDenied
// is used to filter out hints that are disallowed.
func (r *ContainerResources) GetDeviceTopologyHints(checkDenied func(string) bool) topology.Hints {
	if r == nil {
		return nil
	}

	hints := make(topology.Hints)

	for _, dev := range r.GetDevices() {
		name := "podresourceapi:" + dev.GetResourceName()

		if checkDenied(name) {
			log.Info("filtering hints for disallowed device %s", name)
			continue
		}

		var (
			nodes = dev.GetTopology().GetNodes()
			numas = &strings.Builder{}
			sep   = ""
		)

		if len(nodes) == 0 {
			continue
		}

		for i, n := range nodes {
			numas.WriteString(sep)
			numas.WriteString(strconv.FormatInt(n.GetID(), 10))
			if i == 0 {
				sep = ","
			}
		}

		hints[name] = topology.Hint{
			NUMAs: numas.String(),
		}
	}

	return hints
}
