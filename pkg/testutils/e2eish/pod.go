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
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	resapi "k8s.io/apimachinery/pkg/api/resource"
)

// Pod is a pod of a Script step, built with options.
type Pod struct {
	name           string
	namespace      string
	labels         map[string]string
	annotations    map[string]string
	initContainers []*Container
	containers     []*Container
	running        []string // nil for all containers
}

// PodOption is an option of a Pod.
type PodOption func(*Pod)

// NewPod returns a pod in the default namespace with the options.
func NewPod(name string, options ...PodOption) *Pod {
	p := &Pod{name: name, namespace: "default"}
	for _, o := range options {
		o(p)
	}
	return p
}

// InNamespace puts a pod in a namespace.
func InNamespace(ns string) PodOption {
	return func(p *Pod) { p.namespace = canonicalNamespace(ns) }
}

// WithLabels gives a pod labels.
func WithLabels(labels map[string]string) PodOption {
	return func(p *Pod) { p.labels = labels }
}

// WithAnnotations gives a pod annotations.
func WithAnnotations(annotations map[string]string) PodOption {
	return func(p *Pod) { p.annotations = annotations }
}

// WithContainers gives a pod containers.
func WithContainers(ctrs ...*Container) PodOption {
	return func(p *Pod) { p.containers = append(p.containers, ctrs...) }
}

// WithInitContainers gives a pod init containers.
func WithInitContainers(ctrs ...*Container) PodOption {
	return func(p *Pod) { p.initContainers = append(p.initContainers, ctrs...) }
}

// WithRunning tells which containers of a runtime pod run. By default all
// of them do.
func WithRunning(names ...string) PodOption {
	return func(p *Pod) { p.running = append([]string{}, names...) }
}

// Container is a container of a Pod, built with options.
type Container struct {
	name           string
	cpuReq, memReq string
	cpuLim, memLim string
	extended       map[string]string // extended resources by name
}

// ContainerOption is an option of a Container.
type ContainerOption func(*Container)

// NewContainer returns a container with the options.
func NewContainer(name string, options ...ContainerOption) *Container {
	c := &Container{name: name}
	for _, o := range options {
		o(c)
	}
	return c
}

// WithRequests sets the CPU and memory requests of a container, as resource
// quantities. "" leaves one unset.
func WithRequests(cpu, mem string) ContainerOption {
	return func(c *Container) { c.cpuReq, c.memReq = cpu, mem }
}

// WithLimits sets the CPU and memory limits of a container, as resource
// quantities. "" leaves one unset.
func WithLimits(cpu, mem string) ContainerOption {
	return func(c *Container) { c.cpuLim, c.memLim = cpu, mem }
}

// WithExtendedResource gives a container an extended resource, like
// example.com/foo. Its request is its limit, as kubernetes requires.
func WithExtendedResource(name, quantity string) ContainerOption {
	return func(c *Container) {
		if c.extended == nil {
			c.extended = map[string]string{}
		}
		c.extended[name] = quantity
	}
}

// k8s gives the pod as a kubernetes object.
func (p *Pod) k8s() (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	pod.Name, pod.Namespace = p.name, p.namespace
	pod.Labels, pod.Annotations = p.labels, p.annotations
	for _, list := range []struct {
		from []*Container
		to   *[]corev1.Container
	}{
		{p.initContainers, &pod.Spec.InitContainers},
		{p.containers, &pod.Spec.Containers},
	} {
		for _, c := range list.from {
			ctr, err := c.k8s()
			if err != nil {
				return nil, fmt.Errorf("pod %s: %w", p.name, err)
			}
			*list.to = append(*list.to, ctr)
		}
	}
	return pod, nil
}

// syncPod gives the pod as a pod running in the runtime.
func (p *Pod) syncPod() (*syncPod, error) {
	pod, err := p.k8s()
	if err != nil {
		return nil, err
	}
	running := p.running
	if running == nil {
		for _, c := range p.containers {
			running = append(running, c.name)
		}
	}
	return &syncPod{Pod: *pod, Running: running}, nil
}

func (c *Container) k8s() (corev1.Container, error) {
	ctr := corev1.Container{Name: c.name}
	type resource struct {
		value string
		list  *corev1.ResourceList
		name  corev1.ResourceName
	}
	resources := []resource{
		{c.cpuReq, &ctr.Resources.Requests, corev1.ResourceCPU},
		{c.memReq, &ctr.Resources.Requests, corev1.ResourceMemory},
		{c.cpuLim, &ctr.Resources.Limits, corev1.ResourceCPU},
		{c.memLim, &ctr.Resources.Limits, corev1.ResourceMemory},
	}
	for _, name := range slices.Sorted(maps.Keys(c.extended)) {
		resources = append(resources,
			resource{c.extended[name], &ctr.Resources.Requests, corev1.ResourceName(name)},
			resource{c.extended[name], &ctr.Resources.Limits, corev1.ResourceName(name)})
	}
	for _, r := range resources {
		if r.value == "" {
			continue
		}
		q, err := resapi.ParseQuantity(r.value)
		if err != nil {
			return ctr, fmt.Errorf("container %s: bad %s %q: %w", c.name, r.name, r.value, err)
		}
		if *r.list == nil {
			*r.list = corev1.ResourceList{}
		}
		(*r.list)[r.name] = q
	}
	return ctr, nil
}

// PodSpec is a pod as the guaranteed, burstable and besteffort templates of
// the e2e tests create it: containers NAMEc0, NAMEc1..., init containers
// NAMEc0i..., all with the same resources, and the label app=NAME. Unset
// fields take the defaults of the templates.
type PodSpec struct {
	Name        string // pod<N> by default, with N pods created before
	Namespace   string
	ContCount   int               // 1 by default
	IContCount  int               // init containers
	CPU, Mem    string            // guaranteed, 1 and 100M by default
	CPUReq      string            // burstable, 1 by default
	CPULim      string            // burstable, 2 by default, "0" for none
	MemReq      string            // burstable, 100M by default, "0" for none
	MemLim      string            // burstable, 200M by default, "0" for none
	Labels      map[string]string // besides app
	Annotations map[string]string
}

// Template defaults of a PodSpec.
const (
	DefaultCPU    = "1"
	DefaultMem    = "100M"
	DefaultCPUReq = "1"
	DefaultCPULim = "2"
	DefaultMemReq = "100M"
	DefaultMemLim = "200M"
)

// QoS classes of the templates.
const (
	Guaranteed = "guaranteed"
	Burstable  = "burstable"
	BestEffort = "besteffort"
)

// pod gives the options of the pod of the spec, of the template of a QoS
// class.
func (spec PodSpec) pod(qos string) []PodOption {
	or := func(v, def string) string {
		switch v {
		case "":
			return def
		case "0":
			return ""
		}
		return v
	}
	var res []ContainerOption
	switch qos {
	case Guaranteed:
		cpu, mem := or(spec.CPU, DefaultCPU), or(spec.Mem, DefaultMem)
		res = []ContainerOption{WithRequests(cpu, mem), WithLimits(cpu, mem)}
	case Burstable:
		res = []ContainerOption{
			WithRequests(or(spec.CPUReq, DefaultCPUReq), or(spec.MemReq, DefaultMemReq)),
			WithLimits(or(spec.CPULim, DefaultCPULim), or(spec.MemLim, DefaultMemLim)),
		}
	}

	labels := map[string]string{"app": spec.Name}
	maps.Copy(labels, spec.Labels)
	options := []PodOption{WithLabels(labels)}
	if spec.Namespace != "" {
		options = append(options, InNamespace(spec.Namespace))
	}
	if len(spec.Annotations) > 0 {
		options = append(options, WithAnnotations(spec.Annotations))
	}
	for i := range spec.IContCount {
		options = append(options, WithInitContainers(NewContainer(fmt.Sprintf("%sc%di", spec.Name, i), res...)))
	}
	for i := range max(spec.ContCount, 1) {
		options = append(options, WithContainers(NewContainer(fmt.Sprintf("%sc%d", spec.Name, i), res...)))
	}
	return options
}
