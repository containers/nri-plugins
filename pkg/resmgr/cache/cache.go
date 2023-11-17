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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	nri "github.com/containerd/nri/pkg/api"
	v1 "k8s.io/api/core/v1"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"

	"github.com/containers/nri-plugins/pkg/kubernetes"
	logger "github.com/containers/nri-plugins/pkg/log"
	resmgr "github.com/containers/nri-plugins/pkg/resmgr/apis"
	"github.com/containers/nri-plugins/pkg/topology"
)

const (
	// CPU marks changes that can be applied by the CPU controller.
	CPU = "cpu"
	// NRI marks changes that can be applied by NRI.
	NRI = "nri"
	// RDT marks changes that can be applied by the RDT controller.
	RDT = "rdt"
	// BlockIO marks changes that can be applied by the BlockIO controller.
	BlockIO = "blockio"
	// E2ETest marks changes that can be applied by the e2e test controller.
	E2ETest = "e2e-test"

	// RDTClassKey is the pod annotation key for specifying a container RDT class.
	RDTClassKey = "rdtclass" + "." + kubernetes.ResmgrKeyNamespace
	// BlockIOClassKey is the pod annotation key for specifying a container Block I/O class.
	BlockIOClassKey = "blockioclass" + "." + kubernetes.ResmgrKeyNamespace
	// ToptierLimitKey is the pod annotation key for specifying container top tier memory limits.
	ToptierLimitKey = "toptierlimit" + "." + kubernetes.ResmgrKeyNamespace

	// RDTClassPodQoS denotes that the RDTClass should be taken from PodQosClass
	RDTClassPodQoS = "/PodQos"

	// ToptierLimitUnset is the reserved value for indicating unset top tier limits.
	ToptierLimitUnset int64 = -1

	// TopologyHintsKey can be used to opt out from automatic topology hint generation.
	TopologyHintsKey = "topologyhints" + "." + kubernetes.ResmgrKeyNamespace
)

// allControllers is a slice of all controller domains.
var allControllers = []string{CPU, NRI, RDT, BlockIO, E2ETest}

// PodState is the pod state in the runtime.
type PodState int32

// Pod is the exposed interface from a cached pod.
type Pod interface {
	// GetContainers returns the containers of the pod.
	GetContainers() []Container
	// GetId returns the pod id of the pod.
	GetID() string
	// GetUID returns the (kubernetes) unique id of the pod.
	GetUID() string
	// GetName returns the name of the pod.
	GetName() string
	// GetNamespace returns the namespace of the pod.
	GetNamespace() string
	// GetQOSClass returns the PodQOSClass of the pod.
	GetQOSClass() v1.PodQOSClass
	// GetLabel returns the value of the given label and whether it was found.
	GetLabel(string) (string, bool)
	// GetAnnotation returns the value of the given annotation and whether it was found.
	GetAnnotation(key string) (string, bool)
	// GetCgroupParent returns the pods cgroup parent directory.
	GetCgroupParent() string

	// PrettyName returns $namespace/$name as the pretty name for the pod.
	PrettyName() string

	// GetResmgrLabel returns the value of a pod label from the
	// nri-resource-policy namespace.
	GetResmgrLabel(string) (string, bool)
	// GetAnnotation returns the value of a pod annotation from the
	// nri-resource-policy namespace and whether it was found.
	GetResmgrAnnotation(key string) (string, bool)
	// GetEffectiveAnnotation returns the effective annotation for a container.
	// For any given key $K and container $C it will look for annotations in
	// this order and return the first one found:
	//     $K/container.$C
	//     $K/pod
	//     $K
	// and return the value of the first key found.
	GetEffectiveAnnotation(key, container string) (string, bool)

	// GetContainerAffinity returns the affinity expressions for the named container.
	GetContainerAffinity(string) ([]*Affinity, error)
	// ScopeExpression returns an affinity expression for defining this pod as the scope.
	ScopeExpression() *resmgr.Expression

	// Pods can be subject for expression evaluation.
	resmgr.Evaluable

	// We have String() for pods.
	fmt.Stringer

	// GetProcesses returns the pids of all processes in the pod either excluding
	// container processes, if called with false, or including those if called with true.
	GetProcesses(bool) ([]string, error)
	// GetTasks returns the pids of all threads in the pod either excluding cotnainer
	// processes, if called with false, or including those if called with true.
	GetTasks(bool) ([]string, error)
}

// A cached pod.
type pod struct {
	cache      *cache                // our cache of object
	Pod        *nri.PodSandbox       // pod data from NRI
	QOSClass   v1.PodQOSClass        // pod QOS class
	Affinity   *podContainerAffinity // annotated container affinity
	prettyName string                // cached PrettyName()
}

// ContainerState is the container state in the runtime.
type ContainerState = nri.ContainerState

const (
	// ContainerStateCreating marks a container being created.
	ContainerStateCreating = ContainerState(nri.ContainerState_CONTAINER_UNKNOWN - 1)
	// ContainerStateUnknown marks a container to be in an unknown state.
	ContainerStateUnknown = nri.ContainerState_CONTAINER_UNKNOWN
	// ContainerStateCreated marks a container created, not running.
	ContainerStateCreated = nri.ContainerState_CONTAINER_CREATED
	// ContainerStateRunning marks a container created, running.
	ContainerStateRunning = nri.ContainerState_CONTAINER_RUNNING
	// ContainerStateExited marks a container exited.
	ContainerStateExited = nri.ContainerState_CONTAINER_STOPPED
	// ContainerStateStale marks a container removed.
	ContainerStateStale = ContainerState(nri.ContainerState_CONTAINER_STOPPED + 1)
)

// Container is the exposed interface from a cached container.
type Container interface {
	// GetPod returns the pod of the container and a boolean indicating if there was one.
	GetPod() (Pod, bool)
	// GetID returns the ID of the container.
	GetID() string
	// GetPodID returns the pod ID of the container.
	GetPodID() string
	// GetName returns the name of the container.
	GetName() string
	// GetNamespace returns the namespace of the container.
	GetNamespace() string
	// UpdateState updates the state of the container.
	UpdateState(ContainerState)
	// GetState returns the ContainerState of the container.
	GetState() ContainerState
	// GetQOSClass returns the QoS class the pod would have if this was its only container.
	GetQOSClass() v1.PodQOSClass
	// GetArgs returns the container command arguments.
	GetArgs() []string
	// GetLabel returns the value of a container label.
	GetLabel(string) (string, bool)
	// GetAnnotation returns the value of a container annotation.
	GetAnnotation(key string, objPtr interface{}) (string, bool)
	// GetEnv returns the value of a container environment variable.
	GetEnv(string) (string, bool)
	// GetMounts returns all the mounts of the container.
	GetMounts() []*Mount
	// GetDevices returns all the linux devices of the container.
	GetDevices() []*Device

	// PrettyName returns the user-friendly $namespace/$pod/$container for the container.
	PrettyName() string

	// GetResmgrLabel returns the value of a container label from the
	// nri-resource-policy namespace.
	GetResmgrLabel(string) (string, bool)
	// GetResmgrAnnotation returns the value of a container annotation from the
	// nri-resource-policy namespace.
	GetResmgrAnnotation(key string, objPtr interface{}) (string, bool)
	// GetEffectiveAnnotation returns the effective annotation for the container from the pod.
	GetEffectiveAnnotation(key string) (string, bool)

	// Containers can be subject for expression evaluation.
	resmgr.Evaluable

	// We have String() for containers.
	fmt.Stringer

	// GetResourceRequirements returns the resource requirements for this container.
	// The requirements are calculated from the containers cgroup parameters.
	GetResourceRequirements() v1.ResourceRequirements

	// SetResourceUpdates sets updated resources for a container. Returns true if the
	// resources were really updated.
	SetResourceUpdates(*nri.LinuxResources) bool
	// GetResourceUpdates() returns any updated resource requirements for this container.
	// The updates are calculated from the cgroups parameters in the resource update.
	GetResourceUpdates() (v1.ResourceRequirements, bool)

	// InsertMount inserts a mount into the container.
	InsertMount(*Mount)

	// Get any attached topology hints.
	GetTopologyHints() topology.Hints

	// SetCPUShares sets the CFS CPU shares of the container.
	SetCPUShares(int64)
	// SetCPUQuota sets the CFS CPU quota of the container.
	SetCPUQuota(int64)
	// SetCPUPeriod sets the CFS CPU period of the container.
	SetCPUPeriod(int64)
	// SetCpusetCpu sets the cgroup cpuset.cpus of the container.
	SetCpusetCpus(string)
	// SetCpusetMems sets the cgroup cpuset.mems of the container.
	SetCpusetMems(string)
	// SetmemoryLimit sets the memory limit in bytes for the container.
	SetMemoryLimit(int64)

	// GetPendingAdjusmentn clears and returns any pending adjustment for the container.
	GetPendingAdjustment() *nri.ContainerAdjustment
	// GetPendingUpdate clears and returns any pending update for the container.
	GetPendingUpdate() *nri.ContainerUpdate

	// GetAffinity returns the annotated affinity expressions for this container.
	GetAffinity() ([]*Affinity, error)

	// GetCgroupDir returns the relative path of the cgroup directory for the container.
	GetCgroupDir() string

	// SetRDTClass assigns this container to the given RDT class.
	SetRDTClass(string)
	// GetRDTClass returns the RDT class for this container.
	GetRDTClass() string

	// SetBlockIOClass assigns this container to the given BlockIO class.
	SetBlockIOClass(string)
	// GetBlockIOClass returns the BlockIO class for this container.
	GetBlockIOClass() string

	// GetProcesses returns the pids of processes in the container.
	GetProcesses() ([]string, error)
	// GetTasks returns the pids of threads in the container.
	GetTasks() ([]string, error)

	// GetPending gets the names of the controllers with pending changes.
	GetPending() []string
	// HasPending checks if the container has pending chanhes for the given controller.
	HasPending(string) bool
	// ClearPending clears the pending change marker for the given controller.
	ClearPending(string)

	// GetTag gets the value of the given tag.
	GetTag(string) (string, bool)
	// SetTag sets the value of the given tag and returns its previous value..
	SetTag(string, string) (string, bool)
	// DeleteTag deletes the given tag, returning its deleted value.
	DeleteTag(string) (string, bool)
}

// A cached container.
type container struct {
	cache *cache         // our cache of objects
	Ctr   *nri.Container // container data from NRI
	State ContainerState // current state of the container

	Requirements    v1.ResourceRequirements
	ResourceUpdates *v1.ResourceRequirements
	request         interface{}

	Resources *nri.LinuxResources

	TopologyHints topology.Hints    // Set of topology hints for all containers within Pod
	Tags          map[string]string // container tags (local dynamic labels)

	CgroupDir    string // cgroup directory relative to a(ny) controller.
	RDTClass     string // RDT class this container is assigned to.
	BlockIOClass string // Block I/O class this container is assigned to.
	ToptierLimit int64  // Top tier memory limit.

	pending map[string]struct{} // controllers with pending changes for this container

	prettyName string // cached PrettyName()
}

type Mount = nri.Mount
type Device = nri.LinuxDevice

type Cachable interface {
	// Set value (via a pointer receiver) to the object.
	Set(value interface{})
	// Get the object that should be cached.
	Get() interface{}
}

// Cache is the primary interface exposed for tracking pods and containers.
//
// Cache tracks pods and containers in the runtime, mostly by processing CRI
// requests and responses which the cache is fed as these are being procesed.
// Cache also saves its state upon changes to secondary storage and restores
// itself upon startup.
type Cache interface {
	// InsertPod inserts a pod into the cache, using a runtime request or reply.
	InsertPod(pod *nri.PodSandbox) (Pod, error)
	// DeletePod deletes a pod from the cache.
	DeletePod(id string) Pod
	// LookupPod looks up a pod in the cache.
	LookupPod(id string) (Pod, bool)
	// InsertContainer inserts a container into the cache, using a runtime request or reply.
	InsertContainer(*nri.Container) (Container, error)
	// DeleteContainer deletes a container from the cache.
	DeleteContainer(id string) Container
	// LookupContainer looks up a container in the cache.
	LookupContainer(id string) (Container, bool)
	// LookupContainerByCgroup looks up a container for the given cgroup path.
	LookupContainerByCgroup(path string) (Container, bool)

	// GetPendingContainers returs all containers with pending changes.
	GetPendingContainers() []Container

	// GetPods returns all the pods known to the cache.
	GetPods() []Pod
	// GetContainers returns all the containers known to the cache.
	GetContainers() []Container

	// GetContaineIds return the ids of all containers.
	GetContainerIds() []string

	// FilterScope returns the containers selected by the scope expression.
	FilterScope(*resmgr.Expression) []Container
	// EvaluateAffinity evaluates the given affinity against all known in-scope containers
	EvaluateAffinity(*Affinity) map[string]int32
	// AddImplicitAffinities adds a set of implicit affinities (added to all containers).
	AddImplicitAffinities(map[string]ImplicitAffinity) error

	// GetActivePolicy returns the name of the active policy stored in the cache.
	GetActivePolicy() string
	// SetActivePolicy updates the name of the active policy stored in the cache.
	SetActivePolicy(string) error

	// ResetActivePolicy clears the active policy any any policy-specific data from the cache.
	ResetActivePolicy() error

	// SetPolicyEntry sets the policy entry for a key.
	SetPolicyEntry(string, interface{})
	// GetPolicyEntry gets the policy entry for a key.
	GetPolicyEntry(string, interface{}) bool

	// Save requests a cache save.
	Save() error

	// RefreshPods purges/inserts stale/new pods/containers using a pod sandbox list response.
	RefreshPods([]*nri.PodSandbox) ([]Pod, []Pod, []Container)
	// RefreshContainers purges/inserts stale/new containers using a container list response.
	RefreshContainers([]*nri.Container) ([]Container, []Container)

	// Get the container (data) directory for a container.
	ContainerDirectory(string) string
	// OpenFile opens the names container data file, creating it if necessary.
	OpenFile(string, string, os.FileMode) (*os.File, error)
	// WriteFile writes a container data file, creating it if necessary.
	WriteFile(string, string, os.FileMode, []byte) error
}

const (
	// CacheVersion is the running version of the cache.
	CacheVersion = "1"
)

// permissions describe preferred/expected ownership and permissions for a file or directory.
type permissions struct {
	prefer os.FileMode // permissions to create file/directory with
	reject os.FileMode // bits that cause rejection to use an existing entry
}

// permissions to create with/check against
var (
	cacheDirPerm  = &permissions{prefer: 0710, reject: 0022}
	cacheFilePerm = &permissions{prefer: 0644, reject: 0022}
	dataDirPerm   = &permissions{prefer: 0755, reject: 0022}
	dataFilePerm  = &permissions{prefer: 0644, reject: 0022}
	log           = logger.Get("cache")
)

// Our cache of objects.
type cache struct {
	sync.Mutex `json:"-"` // we're lockable
	filePath   string     // where to store to/load from
	dataDir    string     // container data directory

	Pods       map[string]*pod       // known/cached pods
	Containers map[string]*container // known/cache containers
	NextID     uint64                // next container cache id to use

	PolicyName string                 // name of the active policy
	policyData map[string]interface{} // opaque policy data
	PolicyJSON map[string]string      // ditto in raw, unmarshaled form

	pending map[string]struct{} // cache IDs of containers with pending changes

	implicit map[string]ImplicitAffinity // implicit affinities
}

// Make sure cache implements Cache.
var _ Cache = &cache{}

// Options contains the configurable cache options.
type Options struct {
	// CacheDir is the directory the cache should save its state in.
	CacheDir string
}

// NewCache instantiates a new cache. Load it from the given path if it exists.
func NewCache(options Options) (Cache, error) {
	cch := &cache{
		filePath:   filepath.Join(options.CacheDir, "cache"),
		dataDir:    filepath.Join(options.CacheDir, "containers"),
		Pods:       make(map[string]*pod),
		Containers: make(map[string]*container),
		NextID:     1,
		policyData: make(map[string]interface{}),
		PolicyJSON: make(map[string]string),
		implicit:   make(map[string]ImplicitAffinity),
	}

	if _, err := cch.checkPerm("cache", cch.filePath, false, cacheFilePerm); err != nil {
		return nil, cacheError("refusing to use existing cache file: %v", err)
	}
	if err := cch.mkdirAll("cache", options.CacheDir, cacheDirPerm); err != nil {
		return nil, err
	}
	if err := cch.mkdirAll("container", cch.dataDir, dataDirPerm); err != nil {
		return nil, err
	}
	if err := cch.Load(); err != nil {
		return nil, err
	}

	return cch, nil
}

// GetActivePolicy returns the name of the active policy stored in the cache.
func (cch *cache) GetActivePolicy() string {
	return cch.PolicyName
}

// SetActivePolicy updaes the name of the active policy stored in the cache.
func (cch *cache) SetActivePolicy(policy string) error {
	cch.PolicyName = policy
	return cch.Save()
}

// ResetActivePolicy clears the active policy any any policy-specific data from the cache.
func (cch *cache) ResetActivePolicy() error {
	log.Warn("clearing all data for active policy (%q) from cache...",
		cch.PolicyName)

	cch.PolicyName = ""
	cch.policyData = make(map[string]interface{})
	cch.PolicyJSON = make(map[string]string)

	return cch.Save()
}

// Insert a pod into the cache.
func (cch *cache) InsertPod(nriPod *nri.PodSandbox) (Pod, error) {
	p := cch.createPod(nriPod)
	cch.Pods[nriPod.GetId()] = p
	cch.Save()

	return p, nil
}

// Delete a pod from the cache.
func (cch *cache) DeletePod(id string) Pod {
	p, ok := cch.Pods[id]
	if !ok {
		return nil
	}

	log.Debug("removing pod %s (%s)", p.PrettyName(), p.GetID())
	delete(cch.Pods, id)

	cch.Save()

	return p
}

// Look up a pod in the cache.
func (cch *cache) LookupPod(id string) (Pod, bool) {
	p, ok := cch.Pods[id]
	return p, ok
}

// Insert a container into the cache.
func (cch *cache) InsertContainer(ctr *nri.Container) (Container, error) {
	var err error

	c := &container{
		cache: cch,
	}

	c, err = cch.createContainer(ctr)
	if err != nil {
		return nil, cacheError("failed to insert container %s: %v", c.GetID(), err)
	}

	cch.Containers[c.GetID()] = c
	cch.createContainerDirectory(c.GetID())
	cch.Save()

	return c, nil
}

// Delete a pod from the cache.
func (cch *cache) DeleteContainer(id string) Container {
	c, ok := cch.Containers[id]
	if !ok {
		return nil
	}

	log.Debug("removing container %s", c.PrettyName())
	cch.removeContainerDirectory(c.GetID())
	delete(cch.Containers, c.GetID())

	cch.Save()

	return c
}

// Look up a pod in the cache.
func (cch *cache) LookupContainer(id string) (Container, bool) {
	c, ok := cch.Containers[id]
	return c, ok
}

// LookupContainerByCgroup looks up the container for the given cgroup path.
func (cch *cache) LookupContainerByCgroup(path string) (Container, bool) {
	log.Debug("resolving %s to a container...", path)

	for _, c := range cch.Containers {
		parent := ""
		if pod, ok := c.GetPod(); ok {
			parent = pod.GetCgroupParent()
		}
		if parent == "" {
			continue
		}

		if !strings.HasPrefix(path, parent+"/") {
			continue
		}

		if strings.Index(path, c.GetID()) != -1 {
			return c, true
		}
	}

	return nil, false
}

// RefreshPods purges/inserts stale/new pods/containers into the cache.
func (cch *cache) RefreshPods(pods []*nri.PodSandbox) ([]Pod, []Pod, []Container) {
	valid := make(map[string]struct{})

	add := []Pod{}
	del := []Pod{}
	containers := []Container{}

	for _, item := range pods {
		valid[item.Id] = struct{}{}
		if _, ok := cch.Pods[item.Id]; !ok {
			log.Debug("inserting discovered pod %s...", item.Id)
			pod, err := cch.InsertPod(item)
			if err != nil {
				log.Error("failed to insert discovered pod %s to cache: %v",
					item.Id, err)
			} else {
				add = append(add, pod)
			}
		}
	}
	for _, pod := range cch.Pods {
		if _, ok := valid[pod.GetID()]; !ok {
			log.Debug("purging stale pod %s...", pod.GetID())
			del = append(del, cch.DeletePod(pod.GetID()))
		}
	}
	for _, c := range cch.Containers {
		if _, ok := valid[c.GetPodID()]; !ok {
			log.Debug("purging container %s of stale pod %s...", c.GetID(), c.GetPodID())
			cch.DeleteContainer(c.GetID())
			c.State = ContainerStateStale
			containers = append(containers, c)
		}
	}

	return add, del, containers
}

// RefreshContainers purges/inserts stale/new containers using a container list response.
func (cch *cache) RefreshContainers(containers []*nri.Container) ([]Container, []Container) {
	valid := make(map[string]struct{})

	add := []Container{}
	del := []Container{}

	for _, c := range containers {
		valid[c.Id] = struct{}{}
		if _, ok := cch.Containers[c.Id]; !ok {
			log.Debug("inserting discovered container %s...", c.Id)
			inserted, err := cch.InsertContainer(c)
			if err != nil {
				log.Error("failed to insert discovered container %s to cache: %v",
					c.Id, err)
			} else {
				add = append(add, inserted)
			}
		}
	}

	for _, c := range cch.Containers {
		if _, ok := valid[c.GetID()]; !ok {
			log.Debug("purging stale container %s (state: %v)...", c.GetID(), c.GetState())
			cch.DeleteContainer(c.GetID())
			c.State = ContainerStateStale
			del = append(del, c)
		}
	}

	return add, del
}

// Mark a container as having pending changes.
func (cch *cache) markPending(c *container) {
	if cch.pending == nil {
		cch.pending = make(map[string]struct{})
	}
	cch.pending[c.GetID()] = struct{}{}
}

// Get all containers with pending changes.
func (cch *cache) GetPendingContainers() []Container {
	pending := make([]Container, 0, len(cch.pending))
	for id := range cch.pending {
		c, ok := cch.LookupContainer(id)
		if ok {
			pending = append(pending, c)
		}
	}
	return pending
}

// clear the pending state of the given container.
func (cch *cache) clearPending(c *container) {
	delete(cch.pending, c.GetID())
}

// Get the ids of all cached containers.
func (cch *cache) GetContainerIds() []string {
	ids := make([]string, len(cch.Containers))

	idx := 0
	for _, c := range cch.Containers {
		ids[idx] = c.GetID()
		idx++
	}

	return ids[0:idx]
}

// GetPods returns all pods present in the cache.
func (cch *cache) GetPods() []Pod {
	pods := make([]Pod, 0, len(cch.Pods))
	for _, pod := range cch.Pods {
		pods = append(pods, pod)
	}
	return pods
}

// GetContainers returns all the containers present in the cache.
func (cch *cache) GetContainers() []Container {
	containers := make([]Container, 0, len(cch.Containers))
	for _, container := range cch.Containers {
		containers = append(containers, container)
	}
	return containers
}

// Set the policy entry for a key.
func (cch *cache) SetPolicyEntry(key string, obj interface{}) {
	cch.policyData[key] = obj

	if log.DebugEnabled() {
		if data, err := marshalEntry(obj); err != nil {
			log.Error("marshalling of policy entry '%s' failed: %v", key, err)
		} else {
			log.Debug("policy entry '%s' set to '%s'", key, string(data))
		}
	}
}

// Get the policy entry for a key.
func (cch *cache) GetPolicyEntry(key string, ptr interface{}) bool {

	//
	// Notes:
	//     We try to serve requests from the demarshaled cache (policyData).
	//     If that fails (may be a first access since load) we look for the
	//     entry in the unmarshaled cache (PolicyJSON), demarshal, and cache
	//     the entry if found.
	//     Note the quirk: in the latter case we first directly unmarshal to
	//     the pointer provided by the caller, only then Get() and cache the
	//     result.
	//

	obj, ok := cch.policyData[key]
	if !ok {
		entry, ok := cch.PolicyJSON[key]
		if !ok {
			return false
		}

		// first access to key since startup
		if err := unmarshalEntry([]byte(entry), ptr); err != nil {
			log.Fatal("failed to unmarshal '%s' policy entry for key '%s' (%T): %v",
				cch.PolicyName, key, ptr, err)
		}

		if err := cch.cacheEntry(key, ptr); err != nil {
			log.Fatal("failed to cache '%s' policy entry for key '%s': %v",
				cch.PolicyName, key, err)
		}
	} else {
		// subsequent accesses to key
		if err := cch.setEntry(key, ptr, obj); err != nil {
			log.Fatal("failed use cached entry for key '%s' of policy '%s': %v",
				key, cch.PolicyName, err)
		}
	}

	return true
}

// Marshal an opaque policy entry, special-casing cpusets and maps of cpusets.
func marshalEntry(obj interface{}) ([]byte, error) {
	switch obj.(type) {
	case cpuset.CPUSet:
		return []byte("\"" + obj.(cpuset.CPUSet).String() + "\""), nil
	case map[string]cpuset.CPUSet:
		dst := make(map[string]string)
		for key, cset := range obj.(map[string]cpuset.CPUSet) {
			dst[key] = cset.String()
		}
		return json.Marshal(dst)

	default:
		return json.Marshal(obj)
	}
}

// Unmarshal an opaque policy entry, special-casing cpusets and maps of cpusets.
func unmarshalEntry(data []byte, ptr interface{}) error {
	switch ptr.(type) {
	case *cpuset.CPUSet:
		cset, err := cpuset.Parse(string(data[1 : len(data)-1]))
		if err != nil {
			return err
		}
		*ptr.(*cpuset.CPUSet) = cset
		return nil

	case *map[string]cpuset.CPUSet:
		src := make(map[string]string)
		if err := json.Unmarshal([]byte(data), &src); err != nil {
			return cacheError("failed to unmarshal map[string]cpuset.CPUSet: %v", err)
		}

		dst := make(map[string]cpuset.CPUSet)
		for key, str := range src {
			cset, err := cpuset.Parse(str)
			if err != nil {
				return cacheError("failed to unmarshal cpuset.CPUSet '%s': %v", str, err)
			}
			dst[key] = cset
		}

		*ptr.(*map[string]cpuset.CPUSet) = dst
		return nil

	default:
		err := json.Unmarshal(data, ptr)
		return err
	}
}

// Cache an unmarshaled opaque policy entry, special-casing some simple/common types.
func (cch *cache) cacheEntry(key string, ptr interface{}) error {
	if cachable, ok := ptr.(Cachable); ok {
		cch.policyData[key] = cachable.Get()
		return nil
	}

	switch ptr.(type) {
	case *cpuset.CPUSet:
		cch.policyData[key] = *ptr.(*cpuset.CPUSet)
	case *map[string]cpuset.CPUSet:
		cch.policyData[key] = *ptr.(*map[string]cpuset.CPUSet)
	case *map[string]string:
		cch.policyData[key] = *ptr.(*map[string]string)

	case *string:
		cch.policyData[key] = *ptr.(*string)
	case *bool:
		cch.policyData[key] = *ptr.(*bool)

	case *int32:
		cch.policyData[key] = *ptr.(*int32)
	case *uint32:
		cch.policyData[key] = *ptr.(*uint32)
	case *int64:
		cch.policyData[key] = *ptr.(*int64)
	case *uint64:
		cch.policyData[key] = *ptr.(*uint64)

	case *int:
		cch.policyData[key] = *ptr.(*int)
	case *uint:
		cch.policyData[key] = *ptr.(*uint)

	default:
		return cacheError("can't handle policy data of type %T", ptr)
	}

	return nil
}

// Serve an unmarshaled opaque policy entry, special-casing some simple/common types.
func (cch *cache) setEntry(key string, ptr, obj interface{}) error {
	if cachable, ok := ptr.(Cachable); ok {
		cachable.Set(obj)
		return nil
	}

	switch ptr.(type) {
	case *cpuset.CPUSet:
		*ptr.(*cpuset.CPUSet) = obj.(cpuset.CPUSet)
	case *map[string]cpuset.CPUSet:
		*ptr.(*map[string]cpuset.CPUSet) = obj.(map[string]cpuset.CPUSet)
	case *map[string]string:
		*ptr.(*map[string]string) = obj.(map[string]string)

	case *string:
		*ptr.(*string) = obj.(string)
	case *bool:
		*ptr.(*bool) = obj.(bool)

	case *int32:
		*ptr.(*int32) = obj.(int32)
	case *uint32:
		*ptr.(*uint32) = obj.(uint32)
	case *int64:
		*ptr.(*int64) = obj.(int64)
	case *uint64:
		*ptr.(*uint64) = obj.(uint64)

	case *int:
		*ptr.(*int) = obj.(int)
	case *uint:
		*ptr.(*uint) = obj.(uint)

	default:
		return cacheError("can't handle policy data of type %T", ptr)
	}

	return nil
}

// checkPerm checks permissions of an already existing file or directory.
func (cch *cache) checkPerm(what, path string, isDir bool, p *permissions) (bool, error) {
	if isDir {
		what += " directory"
	}

	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return true, cacheError("failed to os.Stat() %s %q: %v", what, path, err)
		}
		return false, nil
	}

	if (info.Mode() & os.ModeType) == os.ModeSymlink {
		return true, cacheError("%s %q exists, but is a symbolic link", what, path)
	}

	// check expected file type
	if isDir {
		if !info.IsDir() {
			return true, cacheError("%s %q exists, but is not a directory", what, path)
		}
	} else {
		if info.Mode()&os.ModeType != 0 {
			return true, cacheError("%s %q exists, but is not a regular file", what, path)
		}
	}

	existing := info.Mode().Perm()
	expected := p.prefer
	rejected := p.reject
	if ((expected | rejected) &^ os.ModePerm) != 0 {
		log.Panic("internal error: current permissions check only handles permission bits (rwx)")
	}

	// check that we don't have any of the rejectable permission bits set
	if existing&rejected != 0 {
		return true, cacheError("existing %s %q has disallowed permissions set: %v",
			what, path, existing&rejected)
	}

	// warn if permissions are less strict than the preferred defaults
	if (existing | expected) != expected {
		log.Warn("existing %s %q has less strict permissions %v than expected %v",
			what, path, existing, expected)
	}

	return true, nil
}

// mkdirAll creates a directory, checking permissions if it already exists.
func (cch *cache) mkdirAll(what, path string, p *permissions) error {
	exists, err := cch.checkPerm(what, path, true, p)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	if err := os.MkdirAll(path, p.prefer); err != nil {
		return cacheError("failed to create %s directory %q: %v", what, path, err)
	}

	return nil
}

// snapshot is used to serialize the cache into a saveable/loadable state.
type snapshot struct {
	Version    string
	Pods       map[string]*pod
	Containers map[string]*container
	NextID     uint64
	PolicyName string
	PolicyJSON map[string]string
}

// Snapshot takes a restorable snapshot of the current state of the cache.
func (cch *cache) Snapshot() ([]byte, error) {
	s := snapshot{
		Version:    CacheVersion,
		Pods:       make(map[string]*pod),
		Containers: make(map[string]*container),
		NextID:     cch.NextID,
		PolicyName: cch.PolicyName,
		PolicyJSON: cch.PolicyJSON,
	}

	for id, p := range cch.Pods {
		s.Pods[id] = p
	}

	for _, c := range cch.Containers {
		s.Containers[c.GetID()] = c
	}

	for key, obj := range cch.policyData {
		data, err := marshalEntry(obj)
		if err != nil {
			return nil, cacheError("failed to marshal policy entry '%s': %v", key, err)
		}

		s.PolicyJSON[key] = string(data)
	}

	data, err := json.Marshal(s)
	if err != nil {
		return nil, cacheError("failed to marshal cache: %v", err)
	}

	return data, nil
}

// Restore restores a previously takes snapshot of the cache.
func (cch *cache) Restore(data []byte) error {
	s := snapshot{
		Pods:       make(map[string]*pod),
		Containers: make(map[string]*container),
		PolicyJSON: make(map[string]string),
	}

	if err := json.Unmarshal(data, &s); err != nil {
		return cacheError("failed to unmarshal snapshot data: %v", err)
	}

	if s.Version != CacheVersion {
		return cacheError("can't restore snapshot, version '%s' != running version %s",
			s.Version, CacheVersion)
	}

	cch.Pods = s.Pods
	cch.Containers = s.Containers
	cch.NextID = s.NextID
	cch.PolicyJSON = s.PolicyJSON
	cch.PolicyName = s.PolicyName
	cch.policyData = make(map[string]interface{})

	for _, p := range cch.Pods {
		p.cache = cch
	}
	for _, c := range cch.Containers {
		c.cache = cch
		cch.Containers[c.GetID()] = c
	}

	return nil
}

// Save the state of the cache.
func (cch *cache) Save() error {
	log.Debug("saving cache to file '%s'...", cch.filePath)

	data, err := cch.Snapshot()
	if err != nil {
		return cacheError("failed to save cache: %v", err)
	}

	tmpPath := cch.filePath + ".saving"
	if err = os.WriteFile(tmpPath, data, cacheFilePerm.prefer); err != nil {
		return cacheError("failed to write cache to file %q: %v", tmpPath, err)
	}
	if err := os.Rename(tmpPath, cch.filePath); err != nil {
		return cacheError("failed to rename %q to %q: %v",
			tmpPath, cch.filePath, err)
	}

	return nil
}

// Load loads the last saved state of the cache.
func (cch *cache) Load() error {
	log.Debug("loading cache from file '%s'...", cch.filePath)

	data, err := os.ReadFile(cch.filePath)

	switch {
	case os.IsNotExist(err):
		log.Debug("no cache file '%s', nothing to restore", cch.filePath)
		return nil
	case len(data) == 0:
		log.Debug("empty cache file '%s', nothing to restore", cch.filePath)
		return nil
	case err != nil:
		return cacheError("failed to load cache from file '%s': %v", cch.filePath, err)
	}

	return cch.Restore(data)
}

func (cch *cache) ContainerDirectory(id string) string {
	c, ok := cch.Containers[id]
	if !ok {
		return ""
	}
	return filepath.Join(cch.dataDir, c.GetID())
}

func (cch *cache) createContainerDirectory(id string) error {
	dir := cch.ContainerDirectory(id)
	if dir == "" {
		return cacheError("failed to determine container directory path for container %s", id)
	}
	return cch.mkdirAll("container directory", dir, dataDirPerm)
}

func (cch *cache) removeContainerDirectory(id string) error {
	dir := cch.ContainerDirectory(id)
	if dir == "" {
		return cacheError("failed to delete directory for container %s", id)
	}
	return os.RemoveAll(dir)
}

func (cch *cache) OpenFile(id string, name string, perm os.FileMode) (*os.File, error) {
	dir := cch.ContainerDirectory(id)
	if dir == "" {
		return nil, cacheError("failed to determine data directory for container %s", id)
	}
	if err := cch.mkdirAll("container directory", dir, dataDirPerm); err != nil {
		return nil, cacheError("container %s: can't create data file %q: %v", id, name, err)
	}

	path := filepath.Join(dir, name)
	if _, err := cch.checkPerm("container", path, false, dataFilePerm); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return nil, cacheError("container %s: can't open data file %q: %v", id, path, err)
	}

	return file, nil
}

func (cch *cache) WriteFile(id string, name string, perm os.FileMode, data []byte) error {
	file, err := cch.OpenFile(id, name, perm)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(data)

	return err
}
