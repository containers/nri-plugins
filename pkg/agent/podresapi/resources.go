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

const (
	HintProvider = "podresourceapi:"
)

// PodResources contains resources for a pod.
type PodResources struct {
	*api.PodResources
}

// ContainerResources contains resources for a single container.
type ContainerResources struct {
	*api.ContainerResources
}

// PodResourcesList containers the result of a pod resources list query.
type PodResourcesList struct {
	l []*api.PodResources
	m map[string]map[string]*PodResources
}

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

func NewPodResourcesList(l []*api.PodResources) *PodResourcesList {
	return &PodResourcesList{
		l: l,
		m: make(map[string]map[string]*PodResources),
	}
}

func (l *PodResourcesList) Len() int {
	if l == nil {
		return 0
	}

	cnt := len(l.l)
	for _, m := range l.m {
		cnt += len(m)
	}

	return cnt
}

// GetPodResources returns resources for the given pod.
func (l *PodResourcesList) GetPodResources(ns, pod string) *PodResources {
	if l == nil {
		return nil
	}

	if p, ok := l.m[ns][pod]; ok {
		return p
	}

	for i, p := range l.l {
		var (
			podNs   = p.GetNamespace()
			podName = p.GetName()
		)

		podMap, ok := l.m[podNs]
		if !ok {
			podMap = make(map[string]*PodResources)
			l.m[podNs] = podMap
		}

		r := &PodResources{p}
		podMap[podName] = r

		if podNs == ns && podName == pod {
			l.l = l.l[i+1:]
			return r
		}
	}

	l.l = nil

	return nil
}

// GetContainer returns resources for the given container.
func (l *PodResourcesList) GetContainer(ns, pod, ctr string) *ContainerResources {
	return l.GetPodResources(ns, pod).GetContainer(ctr)
}

func (l *PodResourcesList) PurgePodResources(ns, pod string) {
	if l == nil {
		return
	}

	if podMap, ok := l.m[ns]; ok {
		delete(podMap, pod)
	}
}

// GetDeviceTopologyHints returns topology hints for the given container. checkDenied
// is used to filter out hints that are disallowed.
func (r *ContainerResources) GetDeviceTopologyHints(checkDenied func(string) bool) topology.Hints {
	if r == nil {
		return nil
	}

	hints := make(topology.Hints)

	for _, dev := range r.GetDevices() {
		name := HintProvider + dev.GetResourceName()

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

func IsPodResourceHint(provider string) bool {
	return strings.HasPrefix(provider, HintProvider)
}
