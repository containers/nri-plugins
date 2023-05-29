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
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/containers/nri-plugins/pkg/cgroups"
	"github.com/containers/nri-plugins/pkg/kubernetes"
	resmgr "github.com/containers/nri-plugins/pkg/resmgr/apis"
	"github.com/containers/nri-plugins/pkg/topology"

	nri "github.com/containerd/nri/pkg/api"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	resapi "k8s.io/apimachinery/pkg/api/resource"
)

// Create and initialize a cached container.
func (cch *cache) createContainer(nriCtr *nri.Container) (*container, error) {
	podID := nriCtr.GetPodSandboxId()
	_, ok := cch.Pods[podID]
	if !ok {
		return nil, cacheError("can't find cached pod %s for container %s (%s)",
			podID, nriCtr.GetId(), nriCtr.GetName())
	}

	c := &container{
		cache: cch,
		Ctr:   nriCtr,
		State: nriCtr.GetState(),
		Tags:  make(map[string]string),
	}

	c.generateTopologyHints()
	c.estimateResourceRequirements()

	if err := c.setDefaults(); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *container) generateTopologyHints() {
	if preference, ok := c.GetEffectiveAnnotation(TopologyHintsKey); ok {
		if genHints, err := strconv.ParseBool(preference); err != nil {
			log.Error("ignoring invalid annotation '%s=%s': %v", TopologyHintsKey, preference, err)
		} else {
			if !genHints {
				log.Info("automatic topology hint generation disabled for %q", c.PrettyName)
				return
			}
		}
	}

	for _, m := range c.Ctr.GetMounts() {
		readOnly := isReadOnlyMount(m)
		if hints := getTopologyHintsForMount(m.Destination, m.Source, readOnly); len(hints) > 0 {
			c.TopologyHints = topology.MergeTopologyHints(c.TopologyHints, hints)
		}
	}

	for _, d := range c.Ctr.GetLinux().GetDevices() {
		if !isReadOnlyDevice(c.Ctr.GetLinux().GetResources().GetDevices(), d) {
			if hints := getTopologyHintsForDevice(d.Type, d.Major, d.Minor); len(hints) > 0 {
				c.TopologyHints = topology.MergeTopologyHints(c.TopologyHints, hints)
			}
		}
	}
}

func isReadOnlyMount(m *nri.Mount) bool {
	for _, o := range m.Options {
		if o == "ro" {
			return true
		}
	}
	return false
}

func isReadOnlyDevice(rules []*nri.LinuxDeviceCgroup, d *nri.LinuxDevice) bool {
	readOnly := true

	for _, r := range rules {
		rType, rMajor, rMinor := r.Type, r.GetMajor().GetValue(), r.GetMinor().GetValue()
		switch {
		case rType == "" && rMajor == 0 && rMinor == 0:
			if strings.IndexAny(r.Access, "w") > -1 {
				readOnly = false
			}
		case d.Type == rType && d.Major == rMajor && d.Minor == rMinor:
			if strings.IndexAny(r.Access, "w") > -1 {
				readOnly = false
			}
			return readOnly
		}
	}

	return readOnly
}

// Estimate resource requirements using the containers cgroup parameters and QoS class.
func (c *container) estimateResourceRequirements() {
	qosClass := c.GetQOSClass()

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}

	lnx := c.Ctr.GetLinux().GetResources()
	cpu := lnx.GetCpu()
	shares := int64(cpu.GetShares().GetValue())

	// calculate CPU request
	if value := SharesToMilliCPU(shares); value > 0 {
		qty := resapi.NewMilliQuantity(value, resapi.DecimalSI)
		resources.Requests[corev1.ResourceCPU] = *qty
	}

	// get memory limit
	if value := lnx.GetMemory().GetLimit().GetValue(); value > 0 {
		qty := resapi.NewQuantity(value, resapi.DecimalSI)
		resources.Limits[corev1.ResourceMemory] = *qty
	}

	// calculate CPU limit, set memory request if known
	switch qosClass {
	case corev1.PodQOSGuaranteed:
		resources.Limits[corev1.ResourceCPU] = resources.Requests[corev1.ResourceCPU]
		resources.Requests[corev1.ResourceMemory] = resources.Limits[corev1.ResourceMemory]
	default:
		fallthrough
	case corev1.PodQOSBestEffort, corev1.PodQOSBurstable:
		quota := cpu.GetQuota().GetValue()
		period := int64(cpu.GetPeriod().GetValue())
		if value := QuotaToMilliCPU(quota, period); value > 0 {
			qty := resapi.NewMilliQuantity(value, resapi.DecimalSI)
			resources.Limits[corev1.ResourceCPU] = *qty
		}
	}

	c.Requirements = resources
}

func (c *container) setDefaults() error {
	class, ok := c.GetEffectiveAnnotation(RDTClassKey)
	if !ok {
		class = RDTClassPodQoS
	}
	c.SetRDTClass(class)

	class, ok = c.GetEffectiveAnnotation(BlockIOClassKey)
	if !ok {
		class = string(c.GetQOSClass())
	}
	c.SetBlockIOClass(class)

	limit, ok := c.GetEffectiveAnnotation(ToptierLimitKey)
	if !ok {
		c.ToptierLimit = ToptierLimitUnset
	} else {
		qty, err := resapi.ParseQuantity(limit)
		if err != nil {
			return cacheError("%q: failed to parse top tier limit annotation %q (%q): %v",
				c.PrettyName(), ToptierLimitKey, limit, err)
		}
		c.SetToptierLimit(qty.Value())
	}

	return nil
}

func (c *container) PrettyName() string {
	if c.prettyName != "" {
		return c.prettyName
	}

	if pod, ok := c.GetPod(); ok {
		c.prettyName = pod.PrettyName()
	} else {
		c.prettyName = fmt.Sprintf("<unknown-pod %s>", c.GetPodID())
	}
	c.prettyName += "/" + c.GetName()

	return c.prettyName
}

func (c *container) GetPod() (Pod, bool) {
	if pod, ok := c.cache.Pods[c.GetPodID()]; ok {
		return pod, ok
	}
	return nil, false
}

func (c *container) GetID() string {
	return c.Ctr.GetId()
}

func (c *container) GetPodID() string {
	return c.Ctr.GetPodSandboxId()
}

func (c *container) GetName() string {
	return c.Ctr.GetName()
}

func (c *container) GetNamespace() string {
	if pod, ok := c.GetPod(); ok {
		return pod.GetNamespace()
	}
	return ""
}

func (c *container) UpdateState(state ContainerState) {
	c.State = state
}

func (c *container) GetState() ContainerState {
	return c.State
}

func (c *container) GetQOSClass() v1.PodQOSClass {
	if pod, ok := c.GetPod(); ok {
		return pod.GetQOSClass()
	}
	return ""
}

func (c *container) GetArgs() []string {
	args := make([]string, len(c.Ctr.GetArgs()))
	copy(args, c.Ctr.GetArgs())
	return args
}

func (c *container) GetLabel(key string) (string, bool) {
	value, ok := c.Ctr.GetLabels()[key]
	return value, ok
}

func (c *container) GetAnnotation(key string, objPtr interface{}) (string, bool) {
	jsonStr, ok := c.Ctr.GetAnnotations()[key]
	if !ok {
		return "", false
	}

	if objPtr != nil {
		if err := json.Unmarshal([]byte(jsonStr), objPtr); err != nil {
			log.Error("failed to unmarshal annotation %s (%s) of pod %s into %T",
				key, jsonStr, c.GetID(), objPtr)
			return "", false
		}
	}

	return jsonStr, true
}

func (c *container) GetEnv(key string) (string, bool) {
	for _, env := range c.Ctr.GetEnv() {
		if idx := strings.IndexRune(env, '='); 0 < idx {
			k, v := env[0:idx], ""
			if idx < len(env)-1 {
				v = env[idx+1:]
			}
			if k == key {
				return v, true
			}
		}
	}
	return "", false
}

func (c *container) GetMounts() []*Mount {
	var mounts []*Mount

	for _, m := range c.Ctr.GetMounts() {
		var options []string
		for _, o := range m.Options {
			options = append(options, o)
		}
		mounts = append(mounts, &Mount{
			Destination: m.Destination,
			Source:      m.Source,
			Type:        m.Type,
			Options:     options,
		})
	}

	return mounts
}

func (c *container) GetDevices() []*Device {
	var devices []*Device

	for _, d := range c.Ctr.GetLinux().GetDevices() {
		devices = append(devices, &Device{
			Path:     d.Path,
			Type:     d.Type,
			Major:    d.Major,
			Minor:    d.Minor,
			FileMode: nri.FileMode(d.GetFileMode()),
			Uid:      nri.UInt32(d.Uid),
			Gid:      nri.UInt32(d.Gid),
		})
	}

	return devices
}

func (c *container) GetResmgrLabel(key string) (string, bool) {
	value, ok := c.GetLabel(kubernetes.ResmgrKey(key))
	return value, ok
}

func (c *container) GetResmgrAnnotation(key string, objPtr interface{}) (string, bool) {
	return c.GetAnnotation(kubernetes.ResmgrKey(key), objPtr)
}

func (c *container) GetEffectiveAnnotation(key string) (string, bool) {
	pod, ok := c.GetPod()
	if !ok {
		return "", false
	}
	return pod.GetEffectiveAnnotation(key, c.GetName())
}

func (c *container) GetResourceRequirements() v1.ResourceRequirements {
	return c.Requirements
}

func (c *container) GetLinuxResources() *nri.LinuxResources {
	return c.Resources
}

func (c *container) GetTopologyHints() topology.Hints {
	return c.TopologyHints
}

func (c *container) getPendingRequest() interface{} {
	if c.request == nil {
		if c.GetState() == ContainerStateCreating {
			c.request = &nri.ContainerAdjustment{}
		} else {
			c.request = &nri.ContainerUpdate{
				ContainerId: c.GetID(),
			}
		}
	}
	return c.request
}

func (c *container) GetPendingAdjustment() *nri.ContainerAdjustment {
	if c.request == nil {
		return nil
	}

	req, ok := c.request.(*nri.ContainerAdjustment)
	if !ok {
		log.Error("%s: queried pending adjustment has mismatching type %T",
			c.PrettyName(), c.request)
		req = nil
	}

	c.request = nil
	return req
}

func (c *container) GetPendingUpdate() *nri.ContainerUpdate {
	if c.request == nil {
		return nil
	}

	req, ok := c.request.(*nri.ContainerUpdate)
	if !ok {
		log.Error("%s: queried pending update has mismatching type %T",
			c.PrettyName(), c.request)
		req = nil
	}

	c.request = nil
	return req
}

func (c *container) InsertMount(m *Mount) {
	var adjust *nri.ContainerAdjustment

	adjust, ok := c.getPendingRequest().(*nri.ContainerAdjustment)
	if !ok {
		log.Error("%s: can't insert mount %s -> %s, container is not being created",
			c.PrettyName(), m.Source, m.Destination)
		return
	}

	adjust.AddMount(m)
	c.markPending(NRI)
}

func (c *container) SetCPUShares(value int64) {
	switch req := c.getPendingRequest().(type) {
	case *nri.ContainerAdjustment:
		req.SetLinuxCPUShares(uint64(value))
	case *nri.ContainerUpdate:
		req.SetLinuxCPUShares(uint64(value))
	default:
		log.Error("%s: can't set CPU shares (%d): incorrect pending request type %T",
			c.PrettyName(), value, c.request)
		return
	}
	c.markPending(NRI)
}

func (c *container) SetCPUQuota(value int64) {
	switch req := c.getPendingRequest().(type) {
	case *nri.ContainerAdjustment:
		req.SetLinuxCPUQuota(value)
	case *nri.ContainerUpdate:
		req.SetLinuxCPUQuota(value)
	default:
		log.Error("%s: can't set CPU quota (%d): incorrect pending request type %T",
			c.PrettyName(), value, c.request)
		return
	}
	c.markPending(NRI)
}

func (c *container) SetCPUPeriod(value int64) {
	switch req := c.getPendingRequest().(type) {
	case *nri.ContainerAdjustment:
		req.SetLinuxCPUPeriod(value)
	case *nri.ContainerUpdate:
		req.SetLinuxCPUPeriod(value)
	default:
		log.Error("%s: can't set CPU period (%d): incorrect pending request type %T",
			c.PrettyName(), value, c.request)
		return
	}
	c.markPending(NRI)
}

func (c *container) SetCpusetCpus(value string) {
	switch req := c.getPendingRequest().(type) {
	case *nri.ContainerAdjustment:
		req.SetLinuxCPUSetCPUs(value)
	case *nri.ContainerUpdate:
		req.SetLinuxCPUSetCPUs(value)
	default:
		log.Error("%s: can't set cpuset CPUs (%s): incorrect pending request type %T",
			c.PrettyName(), value, c.request)
		return
	}
	c.markPending(NRI)
}

func (c *container) SetCpusetMems(value string) {
	switch req := c.getPendingRequest().(type) {
	case *nri.ContainerAdjustment:
		req.SetLinuxCPUSetMems(value)
	case *nri.ContainerUpdate:
		req.SetLinuxCPUSetMems(value)
	default:
		log.Error("%s: can't set cpuset memory (%s): incorrect pending request type %T",
			c.PrettyName(), value, c.request)
		return
	}
	c.markPending(NRI)
}

func (c *container) SetMemoryLimit(value int64) {
	switch req := c.getPendingRequest().(type) {
	case *nri.ContainerAdjustment:
		req.SetLinuxMemoryLimit(value)
	case *nri.ContainerUpdate:
		req.SetLinuxMemoryLimit(value)
	default:
		log.Error("%s: can't set memory limit (%d): incorrect pending request type %T",
			c.PrettyName(), value, c.request)
		return
	}
	c.markPending(NRI)
}

func getTopologyHintsForMount(hostPath, containerPath string, readOnly bool) topology.Hints {

	if readOnly {
		// if device or path is read-only, assume it as non-important for now
		// TODO: determine topology hint, but use it with low priority
		return topology.Hints{}
	}

	log.Debug("getting topology hints for mount %s (at %s)", hostPath, containerPath)

	// ignore topology information for small files in /etc, service files in /var/lib/kubelet and host libraries mounts
	ignoredTopologyPaths := []string{"/.nri-resource-policy", "/etc/", "/dev/termination-log", "/lib/", "/lib64/", "/usr/lib/", "/usr/lib32/", "/usr/lib64/"}

	for _, path := range ignoredTopologyPaths {
		if strings.HasPrefix(hostPath, path) || strings.HasPrefix(containerPath, path) {
			return topology.Hints{}
		}
	}

	// More complext rules, for Kubelet secrets and config maps
	ignoredTopologyPathRegexps := []*regexp.Regexp{
		// Kubelet directory can be different, but we can detect it by structure inside of it.
		// For now, we can safely ignore exposed config maps and secrets for topology hints.
		regexp.MustCompile(`(kubelet)?/pods/[[:xdigit:]-]+/volumes/kubernetes.io~(configmap|secret)/`),
	}
	for _, re := range ignoredTopologyPathRegexps {
		if re.MatchString(hostPath) || re.MatchString(containerPath) {
			return topology.Hints{}
		}
	}

	if devPath, err := topology.FindSysFsDevice(hostPath); err == nil {
		// errors are ignored
		if hints, err := topology.NewTopologyHints(devPath); err == nil && len(hints) > 0 {
			return hints
		}
	}

	return topology.Hints{}
}

func getTopologyHintsForDevice(devType string, major, minor int64) topology.Hints {
	log.Debug("getting topology hints for device <%s %d,%d>", devType, major, minor)

	if devPath, err := topology.FindGivenSysFsDevice(devType, major, minor); err == nil {
		// errors are ignored
		if hints, err := topology.NewTopologyHints(devPath); err == nil && len(hints) > 0 {
			return hints
		}
	}

	return topology.Hints{}
}

func getKubeletHint(cpus, mems string) (ret topology.Hints) {
	if cpus != "" || mems != "" {
		ret = topology.Hints{
			topology.ProviderKubelet: topology.Hint{
				Provider: topology.ProviderKubelet,
				CPUs:     cpus,
				NUMAs:    mems}}
	}
	return
}

func (c *container) GetAffinity() ([]*Affinity, error) {
	pod, ok := c.GetPod()
	if !ok {
		log.Error("internal error: can't find Pod for container %s", c.PrettyName())
	}
	affinity, err := pod.GetContainerAffinity(c.GetName())
	if err != nil {
		return nil, err
	}
	affinity = append(affinity, c.implicitAffinities(len(affinity) > 0)...)
	log.Debug("affinity for container %s:", c.PrettyName())
	for _, a := range affinity {
		log.Debug("  - %s", a.String())
	}

	return affinity, nil
}

func (c *container) GetCgroupDir() string {
	if c.CgroupDir != "" {
		return c.CgroupDir
	}
	if pod, ok := c.GetPod(); ok {
		parent, podID := pod.GetCgroupParent(), pod.GetID()
		ID := c.GetID()
		c.CgroupDir = findContainerDir(parent, podID, ID)
	}
	return c.CgroupDir
}

func (c *container) SetRDTClass(class string) {
	c.RDTClass = class
	c.markPending(RDT)
}

func (c *container) GetRDTClass() string {
	return c.RDTClass
}

func (c *container) SetBlockIOClass(class string) {
	c.BlockIOClass = class
	c.markPending(BlockIO)
}

func (c *container) GetBlockIOClass() string {
	return c.BlockIOClass
}

func (c *container) SetToptierLimit(limit int64) {
	c.ToptierLimit = limit
	c.markPending(Memory)
}

func (c *container) GetToptierLimit() int64 {
	return c.ToptierLimit
}

func (c *container) SetPageMigration(p *PageMigrate) {
	c.PageMigrate = p
	c.markPending(PageMigration)
}

func (c *container) GetPageMigration() *PageMigrate {
	return c.PageMigrate
}

func (c *container) GetProcesses() ([]string, error) {
	dir := c.GetCgroupDir()
	if dir == "" {
		return nil, cacheError("%s: unknown cgroup directory", c.PrettyName())
	}
	return cgroups.Cpu.Group(dir).GetProcesses()
}

func (c *container) GetTasks() ([]string, error) {
	dir := c.GetCgroupDir()
	if dir == "" {
		return nil, cacheError("%s: unknown cgroup directory", c.PrettyName())
	}
	return cgroups.Cpu.Group(dir).GetTasks()
}

func (c *container) markPending(controllers ...string) {
	if c.pending == nil {
		c.pending = make(map[string]struct{})
	}
	for _, ctrl := range controllers {
		c.pending[ctrl] = struct{}{}
		c.cache.markPending(c)
	}
}

func (c *container) ClearPending(controller string) {
	delete(c.pending, controller)
	if len(c.pending) == 0 {
		c.cache.clearPending(c)
	}
}

func (c *container) GetPending() []string {
	if c.pending == nil {
		return nil
	}
	pending := make([]string, 0, len(c.pending))
	for controller := range c.pending {
		pending = append(pending, controller)
	}
	sort.Strings(pending)
	return pending
}

func (c *container) HasPending(controller string) bool {
	if c.pending == nil {
		return false
	}
	_, pending := c.pending[controller]
	return pending
}

func (c *container) GetTag(key string) (string, bool) {
	value, ok := c.Tags[key]
	return value, ok
}

func (c *container) SetTag(key string, value string) (string, bool) {
	prev, ok := c.Tags[key]
	c.Tags[key] = value
	return prev, ok
}

func (c *container) DeleteTag(key string) (string, bool) {
	value, ok := c.Tags[key]
	delete(c.Tags, key)
	return value, ok
}

func (c *container) implicitAffinities(hasExplicit bool) []*Affinity {
	affinities := []*Affinity{}
	for name, generate := range c.cache.implicit {
		implicit := generate(c, hasExplicit)
		if implicit == nil {
			log.Debug("no implicit affinity %s for container %s",
				name, c.PrettyName())
			continue
		}

		log.Debug("using implicit affinity %s for %s", name, c.PrettyName())
		affinities = append(affinities, implicit)
	}
	return affinities
}

func (c *container) String() string {
	return c.PrettyName()
}

func (c *container) Eval(key string) interface{} {
	switch key {
	case resmgr.KeyPod:
		pod, ok := c.GetPod()
		if !ok {
			return cacheError("%s: failed to find pod %s", c.PrettyName(), c.GetPodID())
		}
		return pod
	case resmgr.KeyName:
		return c.GetName()
	case resmgr.KeyNamespace:
		return c.GetNamespace()
	case resmgr.KeyQOSClass:
		return c.GetQOSClass()
	case resmgr.KeyLabels:
		return c.Ctr.GetLabels()
	case resmgr.KeyTags:
		return c.Tags
	case resmgr.KeyID:
		return c.GetID()
	default:
		return cacheError("%s: Container cannot evaluate of %q", c.PrettyName(), key)
	}
}

// CompareContainersFn compares two containers by some arbitrary property.
// It returns a negative integer, 0, or a positive integer, depending on
// whether the first container is considered smaller, equal, or larger than
// the second.
type CompareContainersFn func(Container, Container) int

// SortContainers sorts a slice of containers using the given comparison functions.
// If the containers are otherwise equal they are sorted by pod and container name.
// If the comparison functions are omitted, containers are compared by QoS class,
// memory and cpuset size.
func SortContainers(containers []Container, compareFns ...CompareContainersFn) {
	if len(compareFns) == 0 {
		compareFns = CompareByQOSMemoryCPU
	}
	sort.Slice(containers, func(i, j int) bool {
		ci, cj := containers[i], containers[j]
		for _, cmpFn := range compareFns {
			switch diff := cmpFn(ci, cj); {
			case diff < 0:
				return true
			case diff > 0:
				return false
			}
		}
		// If two containers are otherwise equal they are sorted by pod and container name.
		if pi, ok := ci.GetPod(); ok {
			if pj, ok := cj.GetPod(); ok {
				ni, nj := pi.GetName(), pj.GetName()
				if ni != nj {
					return ni < nj
				}
			}
		}
		return ci.GetName() < cj.GetName()
	})
}

// CompareByQOSMemoryCPU is a slice for comparing container by QOS, memory, and CPU.
var CompareByQOSMemoryCPU = []CompareContainersFn{CompareQOS, CompareMemory, CompareCPU}

// CompareQOS compares containers by QOS class.
func CompareQOS(ci, cj Container) int {
	qosi, qosj := ci.GetQOSClass(), cj.GetQOSClass()
	switch {
	case qosi == v1.PodQOSGuaranteed && qosj != v1.PodQOSGuaranteed:
		return -1
	case qosj == v1.PodQOSGuaranteed && qosi != v1.PodQOSGuaranteed:
		return +1
	case qosi == v1.PodQOSBurstable && qosj == v1.PodQOSBestEffort:
		return -1
	case qosj == v1.PodQOSBurstable && qosi == v1.PodQOSBestEffort:
		return +1
	}
	return 0
}

// CompareMemory compares containers by memory requests and limits.
func CompareMemory(ci, cj Container) int {
	var reqi, limi, reqj, limj int64

	resi := ci.GetResourceRequirements()
	if qty, ok := resi.Requests[v1.ResourceMemory]; ok {
		reqi = qty.Value()
	}
	if qty, ok := resi.Limits[v1.ResourceMemory]; ok {
		limi = qty.Value()
	}
	resj := cj.GetResourceRequirements()
	if qty, ok := resj.Requests[v1.ResourceMemory]; ok {
		reqj = qty.Value()
	}
	if qty, ok := resj.Limits[v1.ResourceMemory]; ok {
		limj = qty.Value()
	}

	switch diff := reqj - reqi; {
	case diff < 0:
		return -1
	case diff > 0:
		return +1
	}
	switch diff := limj - limi; {
	case diff < 0:
		return -1
	case diff > 0:
		return +1
	}
	return 0
}

// CompareCPU compares containers by CPU requests and limits.
func CompareCPU(ci, cj Container) int {
	var reqi, limi, reqj, limj int64

	resi := ci.GetResourceRequirements()
	if qty, ok := resi.Requests[v1.ResourceCPU]; ok {
		reqi = qty.MilliValue()
	}
	if qty, ok := resi.Limits[v1.ResourceCPU]; ok {
		limi = qty.MilliValue()
	}
	resj := cj.GetResourceRequirements()
	if qty, ok := resj.Requests[v1.ResourceCPU]; ok {
		reqj = qty.MilliValue()
	}
	if qty, ok := resj.Limits[v1.ResourceCPU]; ok {
		limj = qty.MilliValue()
	}

	switch diff := reqj - reqi; {
	case diff < 0:
		return -1
	case diff > 0:
		return +1
	}
	switch diff := limj - limi; {
	case diff < 0:
		return -1
	case diff > 0:
		return +1
	}
	return 0
}
