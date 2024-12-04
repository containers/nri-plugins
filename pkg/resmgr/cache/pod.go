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

package cache

import (
	"strings"
	"time"

	nri "github.com/containerd/nri/pkg/api"
	v1 "k8s.io/api/core/v1"

	"github.com/containers/nri-plugins/pkg/agent/podresapi"
	resmgr "github.com/containers/nri-plugins/pkg/apis/resmgr/v1alpha1"
	"github.com/containers/nri-plugins/pkg/cgroups"
	"github.com/containers/nri-plugins/pkg/kubernetes"
)

// Create and initialize a cached pod.
func (cch *cache) createPod(nriPod *nri.PodSandbox, ch <-chan *podresapi.PodResources) *pod {
	p := &pod{
		cache: cch,
		Pod:   nriPod,
		ctime: time.Now(),
	}

	p.goFetchPodResources(ch)

	if err := p.parseCgroupForQOSClass(); err != nil {
		log.Error("pod %s: %v", p.PrettyName(), err)
	}

	return p
}

func (p *pod) GetContainers() []Container {
	containers := []Container{}

	for _, c := range p.cache.Containers {
		if c.GetPodID() == p.GetID() {
			containers = append(containers, c)
		}
	}

	return containers
}

func (p *pod) GetID() string {
	return p.Pod.GetId()
}

func (p *pod) GetUID() string {
	return p.Pod.GetUid()
}

func (p *pod) GetName() string {
	return p.Pod.GetName()
}

func (p *pod) GetNamespace() string {
	return p.Pod.GetNamespace()
}

func (p *pod) GetCtime() time.Time {
	return p.ctime
}

func (p *pod) GetLabel(key string) (string, bool) {
	value, ok := p.Pod.GetLabels()[key]
	return value, ok
}

func (p *pod) GetAnnotation(key string) (string, bool) {
	value, ok := p.Pod.GetAnnotations()[key]
	return value, ok
}

func (p *pod) GetCgroupParent() string {
	return p.Pod.GetLinux().GetCgroupParent()
}

func (p *pod) PrettyName() string {
	if p.prettyName != "" {
		return p.prettyName
	}

	namespace := p.GetNamespace()
	if namespace == "" {
		p.prettyName = "<unknown-namespace>/"
	} else {
		p.prettyName = namespace + "/"
	}

	name := p.GetName()
	if name == "" {
		name = "<unknown-pod>"
	}

	p.prettyName += name
	return p.prettyName
}

func (p *pod) GetResmgrLabel(key string) (string, bool) {
	value, ok := p.GetLabel(kubernetes.ResmgrKey(key))
	return value, ok
}

func (p *pod) GetResmgrAnnotation(key string) (string, bool) {
	return p.GetAnnotation(kubernetes.ResmgrKey(key))
}

func (p *pod) GetEffectiveAnnotation(key, container string) (string, bool) {
	annotations := p.Pod.GetAnnotations()
	if v, ok := annotations[key+"/container."+container]; ok {
		return v, true
	}
	if v, ok := annotations[key+"/pod"]; ok {
		return v, true
	}
	v, ok := annotations[key]
	return v, ok
}

func (p *pod) GetQOSClass() v1.PodQOSClass {
	return p.QOSClass
}

func (p *pod) goFetchPodResources(ch <-chan *podresapi.PodResources) {
	go func() {
		p.podResCh = ch
		p.waitResCh = make(chan struct{})
		defer close(p.waitResCh)

		if p.podResCh != nil {
			p.PodResources = <-p.podResCh
			log.Debug("fetched pod resources %+v for %s", p.PodResources, p.GetName())
		}
	}()
}

func (p *pod) setPodResources(podRes *podresapi.PodResources) {
	p.PodResources = podRes
	log.Debug("set pod resources %+v for %s", p.PodResources, p.GetName())
}

func (p *pod) GetPodResources() *podresapi.PodResources {
	if p.waitResCh != nil {
		log.Debug("waiting for pod resources fetch to complete...")
		_ = <-p.waitResCh
	}
	return p.PodResources
}

func (p *pod) GetContainerAffinity(name string) ([]*Affinity, error) {
	if p.Affinity != nil {
		return (*p.Affinity)[name], nil
	}

	affinity := &podContainerAffinity{}

	value, ok := p.GetResmgrAnnotation(keyAffinity)
	if ok {
		weight := DefaultWeight
		if !affinity.parseSimple(p, value, weight) {
			if err := affinity.parseFull(p, value, weight); err != nil {
				log.Error("%v", err)
				return nil, err
			}
		}
	}
	value, ok = p.GetResmgrAnnotation(keyAntiAffinity)
	if ok {
		weight := -DefaultWeight
		if !affinity.parseSimple(p, value, weight) {
			if err := affinity.parseFull(p, value, weight); err != nil {
				log.Error("%v", err)
				return nil, err
			}
		}
	}

	if log.DebugEnabled() {
		log.Debug("Pod container affinity for %s:", p.GetName())
		for id, ca := range *affinity {
			log.Debug("  - container %s:", id)
			for _, a := range ca {
				log.Debug("    * %s", a.String())
			}
		}
	}

	p.Affinity = affinity

	return (*p.Affinity)[name], nil
}

func (p *pod) ScopeExpression() *resmgr.Expression {
	return &resmgr.Expression{
		Key:    "pod/name",
		Op:     resmgr.Equals,
		Values: []string{p.GetName()},
	}
}

// EvalKey returns the value of a key for expression evaluation.
func (p *pod) EvalKey(key string) interface{} {
	switch key {
	case resmgr.KeyName:
		return p.GetName()
	case resmgr.KeyNamespace:
		return p.GetNamespace()
	case resmgr.KeyQOSClass:
		return p.GetQOSClass()
	case resmgr.KeyLabels:
		return p.Pod.GetLabels()
	case resmgr.KeyID:
		return p.GetID()
	case resmgr.KeyUID:
		return p.GetUID()
	default:
		return cacheError("Pod cannot evaluate of %q", key)
	}
}

// EvalRef evaluates the value of a key reference for this pod.
func (p *pod) EvalRef(key string) (string, bool) {
	return resmgr.KeyValue(key, p)
}

// Expand a string with possible key references.
func (p *pod) Expand(src string, mustResolve bool) (string, error) {
	return resmgr.Expand(src, p, mustResolve)
}

func (p *pod) String() string {
	return p.PrettyName()
}

func (p *pod) GetProcesses(recursive bool) ([]string, error) {
	return p.getTasks(recursive, true)
}

func (p *pod) GetTasks(recursive bool) ([]string, error) {
	return p.getTasks(recursive, false)
}

func (p *pod) getTasks(recursive, processes bool) ([]string, error) {
	var pids, childPids []string
	var err error

	dir := p.GetCgroupParent()
	if dir == "" {
		return nil, cacheError("%s: unknown cgroup parent directory", p.PrettyName())
	}

	if processes {
		pids, err = cgroups.Cpu.Group(dir).GetProcesses()
	} else {
		pids, err = cgroups.Cpu.Group(dir).GetTasks()
	}
	if err != nil {
		return nil, cacheError("%s: failed to read pids: %v", p.PrettyName(), err)
	}

	if !recursive {
		return pids, nil
	}

	for _, c := range p.GetContainers() {
		if c.GetState() == ContainerStateRunning {
			if processes {
				childPids, err = c.GetProcesses()
			} else {
				childPids, err = c.GetTasks()
			}
			if err == nil {
				pids = append(pids, childPids...)
				continue
			}

			log.Error("%s: failed to read pids of %s: %v", p.PrettyName(), c.GetName(), err)
		}
	}

	return pids, nil
}

func (p *pod) parseCgroupForQOSClass() error {
	dir := p.GetCgroupParent()
	switch {
	case strings.Contains(dir, "besteffort"):
		p.QOSClass = v1.PodQOSBestEffort
	case strings.Contains(dir, "burstable"):
		p.QOSClass = v1.PodQOSBurstable
	default:
		p.QOSClass = v1.PodQOSGuaranteed
	}

	if dir == "" {
		return cacheError("unknown cgroup parent/QoS class")
	}

	return nil
}
