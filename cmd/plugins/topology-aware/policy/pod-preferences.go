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
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/kubernetes"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
)

const (
	// annotation key for reserved pools
	keyReservedCPUsPreference = "prefer-reserved-cpus"
	// annotation key for CPU Priority preference
	keyCpuPriorityPreference = "prefer-cpu-priority"
	// annotation key for hiding hyperthreads from allocated CPU sets
	keyHideHyperthreads = "hide-hyperthreads"
	// annotation key for picking individual resources by topology hints
	keyPickResourcesByHints = "pick-resources-by-hints"

	// effective annotation key for isolated CPU preference
	preferIsolatedCPUsKey = "prefer-isolated-cpus" + "." + kubernetes.ResmgrKeyNamespace
	// effective annotation key for shared CPU preference
	preferSharedCPUsKey = "prefer-shared-cpus" + "." + kubernetes.ResmgrKeyNamespace
	// effective annotation key for memory type preference
	preferMemoryTypeKey = "memory-type" + "." + kubernetes.ResmgrKeyNamespace
	// effective annotation key for "cold start" preference
	preferColdStartKey = "cold-start" + "." + kubernetes.ResmgrKeyNamespace
	// annotation key for reserved pools
	preferReservedCPUsKey = keyReservedCPUsPreference + "." + kubernetes.ResmgrKeyNamespace
	// effective annotation key for CPU priority preference
	preferCpuPriorityKey = keyCpuPriorityPreference + "." + kubernetes.ResmgrKeyNamespace
	// effective annotation key for hiding hyperthreads
	hideHyperthreadsKey = keyHideHyperthreads + "." + kubernetes.ResmgrKeyNamespace
	// effective annotation key for picking resources by topology hints
	pickResourcesByHints = keyPickResourcesByHints + "." + kubernetes.ResmgrKeyNamespace

	unlimitedCPU = math.MaxInt // 'unlimited' burstable CPU limit
)

type prefKind int

const (
	// prefImplicit denotes an implicit default preference
	prefImplicit prefKind = iota
	// prefConfig denotes a preference from the global configuration
	prefConfig
	// prefAnnotated denotes an explicitly annotated preference
	prefAnnotated
)

// cpuClass is a type of CPU to allocate
type cpuClass int

// names by cpu class
var cpuClassNames = map[cpuClass]string{
	cpuNormal:   "normal",
	cpuReserved: "reserved",
	cpuPreserve: "preserve",
}

const (
	cpuNormal cpuClass = iota
	cpuReserved
	cpuPreserve
)

// types by memory type name
var memoryNamedTypes = map[string]memoryType{
	"dram":  memoryDRAM,
	"pmem":  memoryPMEM,
	"hbm":   memoryHBM,
	"mixed": memoryAll,
}

// memoryType is bitmask of types of memory to allocate
type memoryType libmem.TypeMask

// memoryType bits
const (
	memoryUnspec   = memoryType(libmem.TypeMask(0))
	memoryDRAM     = memoryType(libmem.TypeMaskDRAM)
	memoryPMEM     = memoryType(libmem.TypeMaskPMEM)
	memoryHBM      = memoryType(libmem.TypeMaskHBM)
	memoryPreserve = memoryType(libmem.TypeMaskHBM << 1)
	memoryAll      = memoryType(memoryDRAM | memoryPMEM | memoryHBM)

	// type of memory to use if none specified
	defaultMemoryType = memoryAll
)

// boolConfigPreference returns the configured boolean preference and
// its preference kind (configured or implicit).
func boolConfigPreference(ptr *bool) (bool, prefKind) {
	if ptr != nil {
		return *ptr, prefConfig
	}
	return false, prefImplicit
}

// isolatedCPUsPreference returns whether isolated CPU allocation is preferred
// for the given container. If an effective annotation is not found, it uses
// the global configuration for isolated CPU preference.
func isolatedCPUsPreference(pod cache.Pod, container cache.Container) (bool, prefKind) {
	key := preferIsolatedCPUsKey
	value, ok := pod.GetEffectiveAnnotation(key, container.GetName())
	if !ok {
		return boolConfigPreference(opt.PreferIsolated)
	}

	preference, err := strconv.ParseBool(value)
	if err != nil {
		log.Error("invalid CPU isolation preference annotation (%q, %q): %v",
			key, value, err)
		return boolConfigPreference(opt.PreferIsolated)
	}

	log.Debug("%s: effective CPU isolation preference %v", container.PrettyName(), preference)

	return preference, prefAnnotated
}

// sharedCPUsPreference returns whether shared CPU allocation is preferred for
// the given container. If an effective annotation is not found, it uses the
// global configuration for shared CPU preference.
func sharedCPUsPreference(pod cache.Pod, container cache.Container) (bool, prefKind) {
	key := preferSharedCPUsKey
	value, ok := pod.GetEffectiveAnnotation(key, container.GetName())
	if !ok {
		return boolConfigPreference(opt.PreferShared)
	}

	preference, err := strconv.ParseBool(value)
	if err != nil {
		log.Error("invalid shared CPU preference annotation (%q, %q): %v",
			key, value, err)
		return boolConfigPreference(opt.PreferShared)
	}

	log.Debug("%s: effective shared CPU preference %v", container.PrettyName(), preference)

	return preference, prefAnnotated
}

// cpuPrioPreference returns the CPU priority preference for the given container
// and whether the container was explicitly annotated with this setting.
func cpuPrioPreference(pod cache.Pod, container cache.Container, fallback cpuPrio) cpuPrio {
	key := preferCpuPriorityKey
	value, ok := pod.GetEffectiveAnnotation(key, container.GetName())

	if !ok {
		prio := fallback
		log.Debug("%s: implicit CPU priority preference %q", container.PrettyName(), prio)
		return prio
	}

	if value == "default" {
		prio := defaultPrio
		log.Debug("%s: explicit default CPU priority preference %q", container.PrettyName(), prio)
		return prio
	}

	prio, ok := cpuPrioByName[value]
	if !ok {
		log.Error("%s: invalid CPU priority preference %q", container.PrettyName(), value)
		prio := fallback
		log.Debug("%s: implicit CPU priority preference %q", container.PrettyName(), prio)
		return prio
	}

	log.Debug("%s: explicit CPU priority preference %q", container.PrettyName(), prio)
	return prio
}

// hideHyperthreadsPreference returns if a container should run using
// only single hyperthread from each physical core.
func hideHyperthreadsPreference(pod cache.Pod, container cache.Container) bool {
	value, ok := container.GetEffectiveAnnotation(hideHyperthreadsKey)
	if !ok {
		return false
	}
	hide, err := strconv.ParseBool(value)
	if err != nil {
		return false
	}
	return hide
}

// memoryTypePreference returns what type of memory should be allocated for
// the container.
func memoryTypePreference(pod cache.Pod, container cache.Container) memoryType {
	if container.PreserveMemoryResources() {
		return memoryPreserve
	}
	key := preferMemoryTypeKey
	value, ok := pod.GetEffectiveAnnotation(key, container.GetName())
	if !ok {
		log.Debug("pod %s has no memory preference annotations", pod.GetName())
		return memoryUnspec
	}

	mtype, err := parseMemoryType(value)
	if err != nil {
		log.Error("invalid memory type preference (%q, %q): %v", key, value, err)
		return memoryUnspec
	}

	log.Debug("%s: effective memory type preference %v", container.PrettyName(), mtype)

	return mtype
}

// coldStartPreference figures out 'cold start' preferences for the container, IOW
// if the container memory should be allocated for an initial 'cold start' period
// from PMEM, and how long this initial period should be.
func coldStartPreference(pod cache.Pod, container cache.Container) (ColdStartPreference, error) {
	key := preferColdStartKey
	value, ok := pod.GetEffectiveAnnotation(key, container.GetName())
	if !ok {
		return ColdStartPreference{}, nil
	}

	preference := ColdStartPreference{}
	if err := yaml.Unmarshal([]byte(value), &preference); err != nil {
		log.Error("failed to parse cold start preference (%q, %q): %v",
			key, value, err)
		return ColdStartPreference{}, policyError("invalid cold start preference %q: %v",
			value, err)
	}

	if preference.Duration.Duration < 0 || time.Duration(preference.Duration.Duration) > time.Hour {
		return ColdStartPreference{}, policyError("cold start duration %s out of range",
			preference.Duration.String())
	}

	log.Debug("%s: effective cold start preference %v",
		container.PrettyName(), preference.Duration.Duration.String())

	return preference, nil
}

// ColdStartPreference lists the various ways the container can be configured to trigger
// cold start. Currently, only timer is supported. If the "duration" is set to a duration
// greater than 0, cold start is enabled and the DRAM controller is added to the container
// after the duration has passed.
type ColdStartPreference struct {
	Duration metav1.Duration // `json:"duration,omitempty"`
}

func checkReservedPoolNamespaces(namespace string) bool {
	if namespace == metav1.NamespaceSystem {
		return true
	}

	for _, str := range opt.ReservedPoolNamespaces {
		ret, err := filepath.Match(str, namespace)
		if err != nil {
			return false
		}

		if ret {
			return true
		}
	}

	return false
}

func checkReservedCPUsAnnotations(c cache.Container) (bool, bool) {
	hintSetting, ok := c.GetEffectiveAnnotation(preferReservedCPUsKey)
	if !ok {
		return false, false
	}

	preference, err := strconv.ParseBool(hintSetting)
	if err != nil {
		log.Error("failed to parse reserved CPU preference %s = '%s': %v",
			keyReservedCPUsPreference, hintSetting, err)
		return false, false
	}

	return preference, true
}

// cpuAllocationPreferences figures out the amount and kind of CPU to allocate.
// Returned values:
// 1. full: number of full CPUs
// 2. fraction: amount of fractional CPU in milli-CPU
// 3. limit: CPU limit for this container
// 4. isolate: (bool) whether to prefer isolated full CPUs
// 5. cpuType: (cpuClass) class of CPU to allocate (reserved vs. normal)
// 6. cpuPrio: preferred CPU allocator priority for CPU allocation.
func cpuAllocationPreferences(pod cache.Pod, container cache.Container) (int, int, int, bool, cpuClass, cpuPrio) {
	//
	// CPU allocation preferences for a container consist of
	//
	//   - the number of exclusive cores to allocate
	//   - the amount of fractional cores to allocate (in milli-CPU)
	//   - whether kernel-isolated cores are preferred for exclusive allocation
	//   - cpu class IOW, whether reserved or normal cores should be allocated
	//
	// The rules for determining these preferences are:
	//
	//   - reserved cores are only and always preferred for kube-system namespace containers
	//   - kube-system namespace containers:
	//       => fractional/shared (reserved) cores
	//   - BestEffort QoS class containers:
	//       => fractional/shared cores
	//   - Burstable QoS class containers:
	//       => fractional/shared cores
	//   - Guaranteed QoS class containers:
	//      - 1 full core > CPU request
	//          => fractional/shared cores
	//      - 1 full core <= CPU request < 2 full cores:
	//          a. fractional allocation:
	//            - shared preference explicitly annotated/configured false:
	//              => mixed cores, prefer isolated, unless annotated/configured otherwise (*)
	//            - shared preference explicitly annotated/configured true:
	//              => shared cores
	//          b. non-fractional allocation:
	//            - shared preference explicitly annotated true:
	//              => shared cores
	//            - isolated default preference false or explicitly annotated false:
	//              => exclusive cores
	//            - isolated default preference true or explicitly annotated true:
	//              => exclusive cores, prefer isolated (*)
	//      - 2 full cores <= CPU request
	//          a. fractional allocation:
	//            - shared preference explicitly annotated false:
	//              => mixed cores, prefer isolated only if explicitly annotated (**)
	//            - otherwise (no shared annotation):
	//              => shared cores
	//          b. non-fractional allocation:
	//            - shared preference explicitly annotated true:
	//              => shared cores
	//            - otherwise (no shared annotation):
	//              => exclusive cores, prefer isolated only if explicitly annotated (**)
	//
	//   - Rationale for isolation defaults:
	//     *)
	//        In the single core case, a workload does not need to do anything extra to
	//        benefit from running on isolated vs. ordinary exclusive cores. Therefore,
	//        allocating isolated cores is a safe default choice.
	//     **)
	//        In the multiple cores case, a workload needs to be 'isolation-aware' to
	//        benefit (or actually to not even get hindered) by running on isolated vs.
	//        ordinary exclusive cores. If it gets isolated cores allocated, it needs
	//        to actively spread itself/its correct processes over the cores, because
	//        the scheduler is not going to do load-balancing for it. Therefore, the
	//        safe choice in this case is to not allocate isolated cores by default.
	//

	namespace := container.GetNamespace()

	reqs, ok := container.GetResourceUpdates()
	if !ok {
		reqs = container.GetResourceRequirements()
	}
	request := reqs.Requests[corev1.ResourceCPU]
	qosClass := pod.GetQOSClass()
	fraction := int(request.MilliValue())
	prio := defaultPrio // ignored for fractional allocations
	limit := 0

	switch qosClass {
	case corev1.PodQOSBestEffort:
	case corev1.PodQOSBurstable:
		if lim, ok := reqs.Limits[corev1.ResourceCPU]; ok {
			limit = int(lim.MilliValue())
		} else {
			limit = unlimitedCPU
		}
	case corev1.PodQOSGuaranteed:
		if lim, ok := reqs.Limits[corev1.ResourceCPU]; ok {
			limit = int(lim.MilliValue())
		}
	}

	// easy cases: kube-system namespace, Burstable or BestEffort QoS class containers
	preferReserved, explicitReservation := checkReservedCPUsAnnotations(container)
	switch {
	case container.PreserveCpuResources():
		return 0, fraction, limit, false, cpuPreserve, prio
	case preferReserved:
		return 0, fraction, limit, false, cpuReserved, prio
	case checkReservedPoolNamespaces(namespace) && !explicitReservation:
		return 0, fraction, limit, false, cpuReserved, prio
	case qosClass == corev1.PodQOSBurstable:
		return 0, fraction, limit, false, cpuNormal, prio
	case qosClass == corev1.PodQOSBestEffort:
		return 0, 0, 0, false, cpuNormal, prio
	}

	// complex case: Guaranteed QoS class containers
	cores := fraction / 1000
	fraction = fraction % 1000
	limit = 1000*cores + fraction
	preferIsolated, isolPrefKind := isolatedCPUsPreference(pod, container)
	preferShared, sharedPrefKind := sharedCPUsPreference(pod, container)
	prio = cpuPrioPreference(pod, container, defaultPrio) // ignored for fractional allocations

	switch {
	case cores == 0: // sub-core CPU request
		return 0, fraction, limit, false, cpuNormal, prio
	case cores < 2: // 1 <= CPU request < 2
		if preferShared {
			return 0, 1000*cores + fraction, limit, false, cpuNormal, prio
		}
		// potentially mixed allocation (1 core + some fraction)
		return cores, fraction, limit, preferIsolated, cpuNormal, prio
	default: // CPU request >= 2
		// fractional allocation, only mixed if explicitly annotated as unshared
		if fraction > 0 {
			if !preferShared && sharedPrefKind == prefAnnotated {
				return cores, fraction, limit, preferIsolated, cpuNormal, prio
			}
			return 0, 1000*cores + fraction, limit, false, cpuNormal, prio
		}
		// non-fractional allocation
		if preferShared {
			return 0, 1000 * cores, limit, false, cpuNormal, prio
		}
		// for multiple cores, isolated preference must be explicitly annotated
		return cores, 0, limit, preferIsolated && isolPrefKind == prefAnnotated, cpuNormal, prio
	}
}

// memoryAllocationPreference returns the amount and kind of memory to allocate.
func memoryAllocationPreference(pod cache.Pod, c cache.Container) (int64, int64, memoryType) {
	var (
		req int64
		lim int64
	)

	resources, ok := c.GetResourceUpdates()
	if !ok {
		resources = c.GetResourceRequirements()
	}
	mtype := memoryTypePreference(pod, c)

	if memReq, ok := resources.Requests[corev1.ResourceMemory]; ok {
		req = memReq.Value()
	}
	if memLim, ok := resources.Limits[corev1.ResourceMemory]; ok {
		lim = memLim.Value()
	}

	return req, lim, mtype
}

func pickByHintsPreference(pod cache.Pod, container cache.Container) bool {
	value, ok := pod.GetEffectiveAnnotation(pickResourcesByHints, container.GetName())
	if !ok {
		return false
	}

	pick, err := strconv.ParseBool(value)
	if err != nil {
		log.Error("failed to parse pick resources by hints preference %s = '%s': %v",
			pickResourcesByHints, value, err)
		return false
	}

	log.Debug("%s: effective pick resources by hints preference %v",
		container.PrettyName(), pick)

	return pick
}

// unlimitedBurstablePreference returns the preferred unlimited burstable topology level.
func (p *policy) unlimitedBurstablePreference(container cache.Container) cfgapi.CPUTopologyLevel {
	prefer, ok := container.GetEffectiveAnnotation(cache.UnlimitedBurstableKey)
	if !ok {
		return opt.UnlimitedBurstable
	}

	level := cfgapi.CPUTopologyLevel(prefer)
	if level.Value() == cfgapi.CPUTopologyLevelUndefined.Value() {
		log.Errorf("ignoring invalid annotated burstable preference %q", prefer)
		level = opt.UnlimitedBurstable
	} else {
		level = p.findExistingTopologyLevel(level)
	}

	return level
}

// String stringifies a cpuClass.
func (t cpuClass) String() string {
	if cpuClassName, ok := cpuClassNames[t]; ok {
		return cpuClassName
	}
	return fmt.Sprintf("#UNNAMED-CPUCLASS(%d)", int(t))
}

// String stringifies a memoryType.
func (t memoryType) String() string {
	return libmem.TypeMask(t).String()
}

// parseMemoryType parses a memory type string, ideally produced by String()
func parseMemoryType(value string) (memoryType, error) {
	if value == "" {
		return memoryUnspec, nil
	}
	mtype := 0
	for _, typestr := range strings.Split(value, ",") {
		t, ok := memoryNamedTypes[strings.ToLower(typestr)]
		if !ok {
			return memoryUnspec, policyError("unknown memory type value '%s'", typestr)
		}
		mtype |= int(t)
	}
	return memoryType(mtype), nil
}

// MarshalJSON is the JSON marshaller for memoryType.
func (t memoryType) MarshalJSON() ([]byte, error) {
	value := t.String()
	return json.Marshal(value)
}

// UnmarshalJSON is the JSON unmarshaller for memoryType
func (t *memoryType) UnmarshalJSON(data []byte) error {
	ival := 0
	if err := json.Unmarshal(data, &ival); err == nil {
		*t = memoryType(ival)
		return nil
	}

	value := ""
	if err := json.Unmarshal(data, &value); err != nil {
		return policyError("failed to unmarshal memoryType '%s': %v",
			string(data), err)
	}

	mtype, err := parseMemoryType(value)
	if err != nil {
		return policyError("failed parse memoryType '%s': %v", value, err)
	}

	*t = mtype
	return nil
}

func (t memoryType) TypeMask() libmem.TypeMask {
	return libmem.TypeMask(t)
}
