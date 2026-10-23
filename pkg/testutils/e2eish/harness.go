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

// Package e2eish runs a policy against a synthetic machine, the way the
// resource manager drives it, to replay e2e tests as unit tests. See
// "Replaying test logs as unit tests" in test/e2e/README.md.
package e2eish

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"sync"
	"testing"

	nri "github.com/containerd/nri/pkg/api"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"

	"github.com/containers/nri-plugins/pkg/kubernetes"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// NoInterrupts is a controllable interrupt pattern which matches no
// interrupt, to keep a policy off the IRQs of the host.
const NoInterrupts = "e2eish: no interrupt matches this"

// Plugin is what the harness needs to know of a policy.
type Plugin struct {
	// Name is the name of its helm chart, in deployment/helm.
	Name string
	// CRD is the file of its configuration CRD in the crds of the chart.
	CRD string
	// New creates a policy.
	New func() policyapi.Backend
	// Config returns a new, empty configuration, for a launch to unmarshal
	// the configuration into.
	Config func() any
	// OffHost returns a copy of a configuration with NoInterrupts as its
	// only controllable interrupts.
	OffHost func(cfg any) any
	// Unsupported tells why the replay cannot follow a configuration or a
	// pod, if it cannot. Either can be nil.
	UnsupportedConfig func(cfg any) string
	UnsupportedPod    func(pod *corev1.Pod) string
}

// Harness is one e2e test run of a policy: a machine, and the policy and
// cache of the policy last launched.
type Harness struct {
	T       *testing.T
	Plugin  *Plugin
	Machine system.System
	Cache   cache.Cache
	Policy  policyapi.Backend
	Owner   *Owner
	Pods    map[string]cache.Pod       // live pods by name
	Ctrs    map[string]cache.Container // live containers by name
	specs   map[string]CtrSpec         // containers by name, for restarts
	ctrCnt  int                        // makes container IDs unique
}

// Resources are the resources of a container, as the kubelet gives them.
type Resources struct {
	CPUReq, CPULim int64 // in milli-CPUs, 0 for unset
	MemReq, MemLim int64 // in bytes, 0 for unset
}

// CtrSpec is what a container is created from. QoS is "" for guaranteed,
// burstable or besteffort.
type CtrSpec struct {
	Pod cache.Pod
	QoS string
	Res Resources
}

// Owner is the owner of the policy, the resource manager. It keeps the DRA
// devices the policy publishes.
type Owner struct {
	sync.Mutex
	devices []resourceapi.Device
}

// UpdateContainers implements policyapi.Owner.
func (*Owner) UpdateContainers() error { return nil }

// DRAEnabled implements policyapi.Owner. It is true, so the policy publishes
// its devices to the harness.
func (*Owner) DRAEnabled() bool { return true }

// PublishDRADevices implements policyapi.Owner.
func (o *Owner) PublishDRADevices(devices []resourceapi.Device) error {
	o.devices = slices.Clone(devices)
	return nil
}

// DRAListAttributesSupported implements policyapi.Owner. It is true, as the
// harness has no API server which could reject a list attribute.
func (*Owner) DRAListAttributesSupported() bool { return true }

// Devices returns the DRA devices the policy published last.
func (o *Owner) Devices() []resourceapi.Device {
	return o.devices
}

// New returns a harness for the plugin on the machine of the topology.
func New(t *testing.T, plugin *Plugin, topo Topology, host Host) *Harness {
	captureLogs(t)
	return &Harness{
		T:       t,
		Plugin:  plugin,
		Machine: NewMachine(t, topo, host),
	}
}

// offHost keeps a policy off the host the tests run on: no interrupt is
// controllable, and CPU classes only log what they would write.
func (h *Harness) offHost(cfg any) any {
	cpuclass.SetDryRun(true)
	return h.Plugin.OffHost(cfg)
}

// Launch is helm-launch: it starts a fresh policy with the configuration.
// Like helm-terminate, it drops the state of any earlier policy.
func (h *Harness) Launch(cfg any) {
	h.T.Helper()
	require.NoError(h.T, h.launch(h.offHost(cfg)), "failed to launch policy")
}

// LaunchMayFail is helm-launch with expect_error=1. It tells if the policy
// started.
func (h *Harness) LaunchMayFail(cfg any) bool {
	h.T.Helper()
	cfg = h.offHost(cfg)
	if err := h.launch(cfg); err != nil {
		Log.Infof("===== launch failed, as it may: %v", err)
		return false
	}
	return true
}

// cacheDir is a directory for the cache of a policy.
func (h *Harness) cacheDir() string {
	h.T.Helper()

	// The cache refuses a directory anyone else may write to, and t.TempDir()
	// honours the umask, which is 0002 on a Fedora host.
	dir := h.T.TempDir()
	require.NoError(h.T, os.Chmod(dir, 0o700), "failed to restrict cache directory %s", dir)

	return dir
}

func (h *Harness) launch(cfg any) error {
	h.T.Helper()
	if data, err := json.Marshal(cfg); err == nil {
		Log.Infof("===== launch %s", data)
	}

	cch, err := cache.NewCache(cache.Options{CacheDir: h.cacheDir()})
	require.NoError(h.T, err, "failed to create cache")

	owner := &Owner{}
	p := h.Plugin.New()
	if err := p.Setup(&policyapi.BackendOptions{
		System: h.Machine,
		Cache:  cch,
		Owner:  owner,
		Config: cfg,
	}); err != nil {
		return fmt.Errorf("failed to set up policy: %w", err)
	}
	if err := p.Start(); err != nil {
		return fmt.Errorf("failed to start policy: %w", err)
	}

	h.Cache = cch
	h.Policy = p
	h.Owner = owner
	h.Pods = map[string]cache.Pod{}
	h.Ctrs = map[string]cache.Container{}
	h.specs = map[string]CtrSpec{}
	return nil
}

// LaunchFails is expect-launch-failure.
func (h *Harness) LaunchFails(cfg any) {
	h.T.Helper()
	cfg = h.offHost(cfg)

	cch, err := cache.NewCache(cache.Options{CacheDir: h.cacheDir()})
	require.NoError(h.T, err, "failed to create cache")

	err = h.Plugin.New().Setup(&policyapi.BackendOptions{
		System: h.Machine,
		Cache:  cch,
		Owner:  &Owner{},
		Config: cfg,
	})
	require.Error(h.T, err, "policy setup succeeded, expected it to fail")
	Log.Infof("===== launch failed as expected: %v", err)
}

// InsertPod does what the resource manager does for RunPodSandbox. qos is ""
// for guaranteed, burstable or besteffort.
func (h *Harness) InsertPod(ns, name, qos string, labels, annotations map[string]string) cache.Pod {
	h.T.Helper()

	uid := fmt.Sprintf("%s-%s-uid", ns, name)
	parent := "/kubepods/pod" + uid
	if qos != "" {
		parent = "/kubepods/" + qos + "/pod" + uid
	}

	h.Cache.InsertPod(&nri.PodSandbox{
		Id:          uid + "-sandbox",
		Uid:         uid,
		Name:        name,
		Namespace:   ns,
		Labels:      labels,
		Annotations: annotations,
		Linux:       &nri.LinuxPodSandbox{CgroupParent: parent},
	}, nil)
	pod, ok := h.Cache.LookupPod(uid + "-sandbox")
	require.True(h.T, ok, "failed to look up pod %s", name)
	h.Pods[name] = pod

	return pod
}

// nriResources are the resources of a container as the kubelet gives them to
// the runtime.
func nriResources(res Resources) *nri.LinuxResources {
	r := &nri.LinuxResources{
		Cpu:    &nri.LinuxCPU{},
		Memory: &nri.LinuxMemory{},
	}
	// The kubelet sets the minimal shares for no request, and the limit
	// of a guaranteed container for its request.
	r.Cpu.Shares = nri.UInt64(cache.MilliCPUToShares(res.CPUReq))
	if res.CPULim > 0 {
		quota, period := cache.MilliCPUToQuota(res.CPULim)
		r.Cpu.Quota = nri.Int64(quota)
		r.Cpu.Period = nri.UInt64(period)
	}
	if res.MemLim > 0 {
		r.Memory.Limit = nri.Int64(res.MemLim)
	}
	return r
}

// InsertContainer adds a container to the cache, as the resource manager does
// first for CreateContainer, with the resources the kubelet would give it.
func (h *Harness) InsertContainer(spec CtrSpec, name string) cache.Container {
	h.T.Helper()

	res := spec.Res
	r := nriResources(res)

	// The cache estimates a burstable memory request from the OOM score
	// adjustment, at a resolution of 1/1000 of the memory of the host.
	var oomAdj int64
	switch spec.QoS {
	case "":
		oomAdj = kubernetes.GuaranteedOOMScoreAdj
	case "besteffort":
		oomAdj = kubernetes.BestEffortOOMScoreAdj
	default:
		oomAdj = min(max(kubernetes.MemReqToOomAdj(res.MemReq),
			kubernetes.MinBurstableOOMScoreAdj), kubernetes.MaxBurstableOOMScoreAdj)
	}

	h.ctrCnt++
	c, err := h.Cache.InsertContainer(&nri.Container{
		Id:           fmt.Sprintf("%s-%d", name, h.ctrCnt),
		PodSandboxId: spec.Pod.GetID(),
		Name:         name,
		Linux: &nri.LinuxContainer{
			Resources:   r,
			OomScoreAdj: nri.Int(int(oomAdj)),
		},
	}, cache.WithContainerState(cache.ContainerStateCreating))
	require.NoError(h.T, err, "failed to insert container %s", name)
	h.specs[name] = spec

	return c
}

// CreateContainer does what the resource manager does for CreateContainer
// and StartContainer. A container which fails allocation is left stale.
func (h *Harness) CreateContainer(spec CtrSpec, name string) (cache.Container, error) {
	h.T.Helper()

	c := h.InsertContainer(spec, name)
	h.Owner.Lock()
	defer h.Owner.Unlock()
	if err := h.Policy.AllocateResources(c); err != nil {
		c.UpdateState(cache.ContainerStateStale)
		return c, err
	}
	c.UpdateState(cache.ContainerStateCreated)
	c.UpdateState(cache.ContainerStateRunning)
	require.NoError(h.T, h.Policy.ContainerStarted(c), "failed to start %s", name)
	h.Ctrs[name] = c

	return c, nil
}

// Spec returns what a container was created from.
func (h *Harness) Spec(name string) CtrSpec {
	return h.specs[name]
}

// ResizeContainer does what the resource manager does for UpdateContainer: it
// gives a container of a pod the resources of an in-place resize, and has the
// policy update what it allocated when they really changed.
func (h *Harness) ResizeContainer(pod, name string, res Resources) {
	h.T.Helper()

	p, ok := h.Pods[pod]
	require.True(h.T, ok, "no pod %s to resize", pod)
	var c cache.Container
	for _, ctr := range p.GetContainers() {
		if ctr.GetName() == name {
			c = ctr
			break
		}
	}
	require.NotNil(h.T, c, "no container %s of pod %s to resize", name, pod)

	Log.Infof("===== resize %s/%s", pod, name)
	h.Owner.Lock()
	defer h.Owner.Unlock()
	if !c.SetResourceUpdates(nriResources(res)) {
		return
	}
	require.NoError(h.T, h.Policy.UpdateResources(c), "failed to update %s", name)
}

// StopContainer does what the resource manager does for StopContainer and
// RemoveContainer.
func (h *Harness) StopContainer(c cache.Container) {
	h.T.Helper()

	h.Owner.Lock()
	defer h.Owner.Unlock()
	require.NoError(h.T, h.Policy.ReleaseResources(c), "failed to release %s", c.GetName())
	c.UpdateState(cache.ContainerStateExited)
	h.Cache.DeleteContainer(c.GetID())
	maps.DeleteFunc(h.Ctrs, func(_ string, ctr cache.Container) bool { return ctr == c })
}

// DeletePods is kubectl delete pods.
func (h *Harness) DeletePods(names ...string) {
	h.T.Helper()

	for _, name := range names {
		pod, ok := h.Pods[name]
		require.True(h.T, ok, "no pod %s to delete", name)
		Log.Infof("===== delete %s", name)
		ctrs := pod.GetContainers()
		sort.Slice(ctrs, func(i, j int) bool { return ctrs[i].GetName() < ctrs[j].GetName() })
		for _, c := range ctrs {
			h.StopContainer(c)
		}
		h.Cache.DeletePod(pod.GetID())
		delete(h.Pods, name)
	}
}

// Running gives the names of the running containers, sorted.
func (h *Harness) Running() []string {
	return slices.Sorted(maps.Keys(h.Ctrs))
}

// Allowed is report allowed: the CPUs and memory nodes a container may use,
// as the e2e tests see them in /proc/self/status. The memory nodes are a
// cpuset string, "" for all of them.
func (h *Harness) Allowed(name string) (cpuset.CPUSet, string) {
	h.T.Helper()

	c, ok := h.Ctrs[name]
	require.True(h.T, ok, "no container %s", name)

	// A timer of the policy may update containers, with the owner locked.
	h.Owner.Lock()
	cpuStr, mems := c.GetCpusetCpus(), c.GetCpusetMems()
	h.Owner.Unlock()

	cpus := h.Machine.OnlineCPUs()
	if cpuStr != "" {
		var err error
		cpus, err = cpuset.Parse(cpuStr)
		require.NoError(h.T, err, "%s: bad cpuset %q", name, cpuStr)
	}
	return cpus, mems
}

// CreateFromPod creates a pod and starts its containers as the kubelet would
// for the pod object, init containers first, one at a time to completion.
// Containers which fail allocation are left stale.
func (h *Harness) CreateFromPod(ns string, pod *corev1.Pod) {
	h.T.Helper()
	Log.Infof("===== create %s/%s", ns, pod.Name)

	qos := PodQOS(pod)
	p := h.InsertPod(ns, pod.Name, qos, pod.Labels, pod.Annotations)
	for _, c := range pod.Spec.InitContainers {
		ctr, err := h.CreateContainer(CtrSpec{p, qos, ResourcesOf(&c)}, c.Name)
		if err != nil {
			Log.Warnf("===== %s failed: %v", c.Name, err)
			continue
		}
		h.StopContainer(ctr)
	}
	for _, c := range pod.Spec.Containers {
		if _, err := h.CreateContainer(CtrSpec{p, qos, ResourcesOf(&c)}, c.Name); err != nil {
			Log.Warnf("===== %s failed: %v", c.Name, err)
		}
	}
}

// PodQOS gives the QoS class of a pod, "" for guaranteed.
func PodQOS(pod *corev1.Pod) string {
	var (
		ctrs       = append(slices.Clone(pod.Spec.InitContainers), pod.Spec.Containers...)
		requested  = false
		guaranteed = true
	)
	for _, c := range ctrs {
		r := c.Resources
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			lim, hasLim := r.Limits[name]
			req, hasReq := r.Requests[name]
			requested = requested || hasLim || hasReq
			if !hasReq {
				req, hasReq = lim, hasLim
			}
			if !hasLim || !hasReq || req.Cmp(lim) != 0 {
				guaranteed = false
			}
		}
	}
	switch {
	case !requested:
		return "besteffort"
	case guaranteed:
		return ""
	default:
		return "burstable"
	}
}

// ResourcesOf gives the resources of a container. A request left out is the
// limit.
func ResourcesOf(c *corev1.Container) Resources {
	r := c.Resources
	get := func(list corev1.ResourceList, name corev1.ResourceName, milli bool) int64 {
		q, ok := list[name]
		if !ok {
			return 0
		}
		if milli {
			return q.MilliValue()
		}
		return q.Value()
	}
	res := Resources{
		CPUReq: get(r.Requests, corev1.ResourceCPU, true),
		CPULim: get(r.Limits, corev1.ResourceCPU, true),
		MemReq: get(r.Requests, corev1.ResourceMemory, false),
		MemLim: get(r.Limits, corev1.ResourceMemory, false),
	}
	if _, ok := r.Requests[corev1.ResourceCPU]; !ok {
		res.CPUReq = res.CPULim
	}
	if _, ok := r.Requests[corev1.ResourceMemory]; !ok {
		res.MemReq = res.MemLim
	}
	return res
}
