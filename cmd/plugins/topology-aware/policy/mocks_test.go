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
	"os"
	"time"

	nri "github.com/containerd/nri/pkg/api"
	"github.com/containers/nri-plugins/pkg/agent/podresapi"
	resmgr "github.com/containers/nri-plugins/pkg/apis/resmgr/v1alpha1"
	"github.com/containers/nri-plugins/pkg/cpuallocator"
	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	"github.com/containers/nri-plugins/pkg/topology"
	v1 "k8s.io/api/core/v1"
)

type mockContainer struct {
	name                                  string
	namespace                             string
	returnValueForGetResourceRequirements v1.ResourceRequirements
	returnValueForGetID                   string
	returnValueForQOSClass                v1.PodQOSClass
	pod                                   cache.Pod
	cdiDeviceNames                        []string
	cpusetCpus                            string
	setCpusetCpusCalls                    []string
}

func (m *mockContainer) GetPod() (cache.Pod, bool) {
	if m.pod == nil {
		return &mockPod{}, false
	}
	return m.pod, true
}
func (m *mockContainer) GetID() string {
	if len(m.returnValueForGetID) == 0 {
		return "0"
	}

	return m.returnValueForGetID
}
func (m *mockContainer) GetPodID() string {
	panic("unimplemented")
}
func (m *mockContainer) GetName() string {
	return m.name
}
func (m *mockContainer) GetNamespace() string {
	return m.namespace
}
func (m *mockContainer) UpdateState(cache.ContainerState) {
	panic("unimplemented")
}
func (m *mockContainer) GetState() cache.ContainerState {
	panic("unimplemented")
}
func (m *mockContainer) GetQOSClass() v1.PodQOSClass {
	if len(m.returnValueForQOSClass) == 0 {
		return v1.PodQOSGuaranteed
	}

	return m.returnValueForQOSClass
}
func (m *mockContainer) GetArgs() []string {
	panic("unimplemented")
}
func (m *mockContainer) GetLabel(string) (string, bool) {
	panic("unimplemented")
}
func (m *mockContainer) GetResmgrLabelKeys() []string {
	panic("unimplemented")
}
func (m *mockContainer) GetAnnotation(string, any) (string, bool) {
	panic("unimplemented")
}
func (m *mockContainer) GetEnv(string) (string, bool) {
	panic("unimplemented")
}
func (m *mockContainer) GetAnnotations() map[string]string {
	panic("unimplemented")
}
func (m *mockContainer) GetMounts() []*cache.Mount {
	panic("unimplemented")
}
func (m *mockContainer) GetDevices() []*cache.Device {
	panic("unimplemented")
}
func (m *mockContainer) GetCDIDeviceNames() []string {
	return m.cdiDeviceNames
}
func (m *mockContainer) PrettyName() string {
	return m.name
}
func (m *mockContainer) GetResmgrLabel(string) (string, bool) {
	panic("unimplemented")
}
func (m *mockContainer) GetResmgrAnnotation(string, any) (string, bool) {
	panic("unimplemented")
}
func (m *mockContainer) GetEffectiveAnnotation(key string) (string, bool) {
	pod, ok := m.GetPod()
	if !ok {
		return "", false
	}
	return pod.GetEffectiveAnnotation(key, m.name)
}
func (m *mockContainer) QueryEffectiveAnnotation(key string) (string, cache.AnnotationScope, bool) {
	pod, ok := m.GetPod()
	if !ok {
		return "", cache.UnscopedAnnotation, false
	}
	return pod.QueryEffectiveAnnotation(key, m.name)
}
func (m *mockContainer) EvalKey(string) any {
	panic("unimplemented")
}
func (m *mockContainer) EvalRef(string) (string, bool) {
	panic("unimplemented")
}
func (m *mockContainer) Expand(string, bool) (string, error) {
	panic("unimplemented")
}

func (m *mockContainer) String() string {
	return "mockContainer"
}
func (m *mockContainer) GetResourceRequirements() v1.ResourceRequirements {
	return m.returnValueForGetResourceRequirements
}
func (m *mockContainer) SetResourceUpdates(*nri.LinuxResources) bool {
	return false
}
func (m *mockContainer) GetResourceUpdates() (v1.ResourceRequirements, bool) {
	return v1.ResourceRequirements{}, false
}
func (m *mockContainer) InsertMount(*cache.Mount) {
	panic("unimplemented")
}
func (m *mockContainer) GetTopologyHints() topology.Hints {
	return topology.Hints{}
}
func (m *mockContainer) StrictTopologyHints() bool {
	return false
}
func (m *mockContainer) SetCPUShares(int64) {
}
func (m *mockContainer) SetCPUPeriod(int64) {
	panic("unimplemented")
}
func (m *mockContainer) SetCPUQuota(int64) {
	panic("unimplemented")
}
func (m *mockContainer) SetCpusetCpus(cpus string) {
	m.setCpusetCpusCalls = append(m.setCpusetCpusCalls, cpus)
	m.cpusetCpus = cpus
}
func (m *mockContainer) SetCpusetMems(string) {
}
func (m *mockContainer) SetMemoryLimit(int64) {
	panic("unimplemented")
}
func (m *mockContainer) SetMemorySwap(int64) {
	panic("unimplemented")
}
func (m *mockContainer) SetSchedulingPolicy(nri.LinuxSchedulerPolicy) {
	panic("unimplemented")
}
func (m *mockContainer) SetSchedulingNice(int32) {
	panic("unimplemented")
}
func (m *mockContainer) SetSchedulingPriority(int32) {
	panic("unimplemented")
}
func (m *mockContainer) SetSchedulingFlags([]nri.LinuxSchedulerFlag) {
	panic("unimplemented")
}
func (m *mockContainer) SetSchedulingRuntime(uint64) {
	panic("unimplemented")
}
func (m *mockContainer) SetSchedulingDeadline(uint64) {
	panic("unimplemented")
}
func (m *mockContainer) SetSchedulingPeriod(uint64) {
	panic("unimplemented")
}
func (m *mockContainer) SetSchedulingIOClass(nri.IOPrioClass) {
	panic("unimplemented")
}
func (m *mockContainer) SetSchedulingIOPriority(int32) {
	panic("unimplemented")
}
func (m *mockContainer) GetPendingAdjustment() *nri.ContainerAdjustment {
	panic("unimplemented")
}
func (m *mockContainer) GetPendingUpdate() *nri.ContainerUpdate {
	panic("unimplemented")
}
func (m *mockContainer) GetAffinity() ([]*cache.Affinity, error) {
	return nil, nil
}
func (m *mockContainer) GetCgroupDir() string {
	panic("unimplemented")
}
func (m *mockContainer) SetRDTClass(string) {
	panic("unimplemented")
}
func (m *mockContainer) GetRDTClass() string {
	panic("unimplemented")
}
func (m *mockContainer) SetBlockIOClass(string) {
	panic("unimplemented")
}
func (m *mockContainer) GetBlockIOClass() string {
	panic("unimplemented")
}
func (m *mockContainer) GetPending() []string {
	panic("unimplemented")
}
func (m *mockContainer) HasPending(string) bool {
	panic("unimplemented")
}
func (m *mockContainer) ClearPending(string) {
	panic("unimplemented")
}
func (m *mockContainer) GetTag(string) (string, bool) {
	panic("unimplemented")
}
func (m *mockContainer) SetTag(string, string) (string, bool) {
	panic("unimplemented")
}
func (m *mockContainer) DeleteTag(string) (string, bool) {
	panic("unimplemented")
}
func (m *mockContainer) GetProcesses() ([]string, error) {
	panic("unimplemented")
}
func (m *mockContainer) GetTasks() ([]string, error) {
	panic("unimplemented")
}
func (m *mockContainer) GetCPUShares() int64 {
	panic("unimplemented")
}
func (m *mockContainer) GetCPUQuota() int64 {
	panic("unimplemented")
}
func (m *mockContainer) GetCPUPeriod() int64 {
	panic("unimplemented")
}
func (m *mockContainer) GetCpusetCpus() string {
	return m.cpusetCpus
}
func (m *mockContainer) GetCpusetMems() string {
	panic("unimplemented")
}
func (m *mockContainer) GetMemoryLimit() int64 {
	panic("unimplemented")
}
func (m *mockContainer) GetMemorySwap() int64 {
	panic("unimplemented")
}
func (m *mockContainer) GetCtime() time.Time {
	panic("unimplemented")
}
func (m *mockContainer) GetCreatedAt() int64 {
	return 0
}
func (m *mockContainer) PreserveCpuResources() bool {
	return false
}
func (m *mockContainer) PreserveMemoryResources() bool {
	return false
}
func (m *mockContainer) MemoryTypes() (libmem.TypeMask, error) {
	return libmem.TypeMaskDRAM, nil
}
func (m *mockContainer) GetPodResources() *podresapi.ContainerResources {
	return nil
}

type mockPod struct {
	name                               string
	returnValueFotGetQOSClass          v1.PodQOSClass
	returnValue1FotGetResmgrAnnotation string
	returnValue2FotGetResmgrAnnotation bool
	annotations                        map[string]string
}

func (m *mockPod) GetContainers() []cache.Container {
	panic("unimplemented")
}
func (m *mockPod) GetID() string {
	panic("unimplemented")
}
func (m *mockPod) GetUID() string {
	panic("unimplemented")
}
func (m *mockPod) GetName() string {
	return m.name
}
func (m *mockPod) GetNamespace() string {
	panic("unimplemented")
}
func (m *mockPod) GetQOSClass() v1.PodQOSClass {
	return m.returnValueFotGetQOSClass
}
func (m *mockPod) GetLabel(string) (string, bool) {
	panic("unimplemented")
}
func (m *mockPod) GetAnnotation(string) (string, bool) {
	panic("unimplemented")
}
func (m *mockPod) GetCgroupParent() string {
	panic("unimplemented")
}
func (m *mockPod) PrettyName() string {
	return m.name
}
func (m *mockPod) GetResmgrLabel(string) (string, bool) {
	panic("unimplemented")
}
func (m *mockPod) GetResmgrAnnotation(key string) (string, bool) {
	return m.returnValue1FotGetResmgrAnnotation, m.returnValue2FotGetResmgrAnnotation
}
func (m *mockPod) GetEffectiveAnnotation(key, container string) (string, bool) {
	if v, ok := m.annotations[key+"/container."+container]; ok {
		return v, true
	}
	if v, ok := m.annotations[key+"/pod"]; ok {
		return v, true
	}
	v, ok := m.annotations[key]
	return v, ok
}
func (m *mockPod) QueryEffectiveAnnotation(key, container string) (string, cache.AnnotationScope, bool) {
	if v, ok := m.annotations[key+"/container."+container]; ok {
		return v, cache.ContainerScopedAnnotation, true
	}
	if v, ok := m.annotations[key+"/pod"]; ok {
		return v, cache.PodScopedAnnotation, true
	}
	v, ok := m.annotations[key]
	return v, cache.UnscopedAnnotation, ok
}
func (m *mockPod) GetContainerAffinity(string) ([]*cache.Affinity, error) {
	panic("unimplemented")
}
func (m *mockPod) ScopeExpression() *resmgr.Expression {
	panic("unimplemented")
}
func (m *mockPod) String() string {
	return "mockPod"
}
func (m *mockPod) EvalKey(string) any {
	panic("unimplemented")
}
func (m *mockPod) EvalRef(string) (string, bool) {
	panic("unimplemented")
}
func (m *mockPod) Expand(string, bool) (string, error) {
	panic("unimplemented")
}
func (m *mockPod) GetProcesses(bool) ([]string, error) {
	panic("unimplemented")
}
func (m *mockPod) GetTasks(bool) ([]string, error) {
	panic("unimplemented")
}
func (m *mockPod) GetCtime() time.Time {
	panic("unimplemented")
}
func (m *mockPod) GetPodResources() *podresapi.PodResources {
	return nil
}

type mockCache struct {
	returnValueForGetPolicyEntry   bool
	returnValue1ForLookupContainer cache.Container
	returnValue2ForLookupContainer bool
	containers                     []cache.Container
}

func (m *mockCache) InsertPod(*nri.PodSandbox, <-chan *podresapi.PodResources) cache.Pod {
	panic("unimplemented")
}
func (m *mockCache) DeletePod(string) cache.Pod {
	panic("unimplemented")
}
func (m *mockCache) LookupPod(string) (cache.Pod, bool) {
	panic("unimplemented")
}
func (m *mockCache) InsertContainer(*nri.Container, ...cache.InsertContainerOption) (cache.Container, error) {
	panic("unimplemented")
}
func (m *mockCache) DeleteContainer(string) cache.Container {
	panic("unimplemented")
}
func (m *mockCache) LookupContainer(string) (cache.Container, bool) {
	return m.returnValue1ForLookupContainer, m.returnValue2ForLookupContainer
}
func (m *mockCache) LookupContainerByCgroup(path string) (cache.Container, bool) {
	panic("unimplemented")
}
func (m *mockCache) GetPendingContainers() []cache.Container {
	panic("unimplemented")
}
func (m *mockCache) GetPods() []cache.Pod {
	panic("unimplemented")
}
func (m *mockCache) GetContainers() []cache.Container {
	return m.containers
}
func (m *mockCache) GetContainerIds() []string {
	panic("unimplemented")
}
func (m *mockCache) FilterScope(*resmgr.Expression) []cache.Container {
	panic("unimplemented")
}
func (m *mockCache) EvaluateAffinity(*cache.Affinity) map[string]int32 {
	return map[string]int32{
		"fake key": 1,
	}
}
func (m *mockCache) AddImplicitAffinities(map[string]cache.ImplicitAffinity) error {
	return nil
}
func (m *mockCache) DeleteImplicitAffinities(...string) {
}

func (m *mockCache) ConfigureRDTControl(bool) {
}
func (m *mockCache) ConfigureBlockIOControl(bool) {
}
func (m *mockCache) GetActivePolicy() string {
	panic("unimplemented")
}
func (m *mockCache) SetActivePolicy(string) error {
	panic("unimplemented")
}
func (m *mockCache) ResetActivePolicy() error {
	panic("unimplemented")
}
func (m *mockCache) SetPolicyEntry(string, any) {
}
func (m *mockCache) GetPolicyEntry(string, any) bool {
	return m.returnValueForGetPolicyEntry
}
func (m *mockCache) Save() error {
	return nil
}
func (m *mockCache) BlockSave() {
}
func (m *mockCache) UnblockSave() error {
	return nil
}

func (m *mockCache) RefreshPods([]*nri.PodSandbox, <-chan *podresapi.PodResourcesList) ([]cache.Pod, []cache.Pod, []cache.Container) {
	panic("unimplemented")
}
func (m *mockCache) RefreshContainers([]*nri.Container) ([]cache.Container, []cache.Container) {
	panic("unimplemented")
}
func (m *mockCache) ContainerDirectory(string) string {
	panic("unimplemented")
}
func (m *mockCache) OpenFile(string, string, os.FileMode) (*os.File, error) {
	panic("unimplemented")
}
func (m *mockCache) WriteFile(string, string, os.FileMode, []byte) error {
	panic("unimplemented")
}

type mockCPUAllocator struct{}

func (m *mockCPUAllocator) AllocateCpus(from *libcpu.CpuMask, cnt int, options ...cpuallocator.Option) (*libcpu.CpuMask, error) {
	return libcpu.NewCpuMask(0), nil
}

func (m *mockCPUAllocator) ReleaseCpus(from *libcpu.CpuMask, cnt int, options ...cpuallocator.Option) (*libcpu.CpuMask, error) {
	return libcpu.NewCpuMask(0), nil
}

func (m *mockCPUAllocator) GetCPUPriorities() map[cpuallocator.CPUPriority]*libcpu.CpuMask {
	// An entry per priority, as the real allocator promises. A caller is entitled
	// to index this without checking.
	prios := map[cpuallocator.CPUPriority]*libcpu.CpuMask{}
	for prio := range cpuallocator.NumCPUPriorities {
		prios[prio] = libcpu.NewCpuMask()
	}
	return prios
}

var (
	_ cpuallocator.CPUAllocator = &mockCPUAllocator{}
)
