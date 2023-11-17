// Copyright 2022 Intel Corporation. All Rights Reserved.
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

package cpu

import (
	"fmt"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control"
	cfgcpu "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control/cpu"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/control"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/intel/goresctrl/pkg/utils"
)

const (
	// ConfigModuleName is the configuration section for the CPU controller.
	ConfigModuleName = "cpu"

	// CPUController is the name of the CPU controller.
	CPUController = cache.CPU
)

// cpuctl encapsulates the runtime state of our CPU enforcement/controller.
type cpuctl struct {
	cache         cache.Cache      // resource manager cache
	system        sysfs.System     // system topology
	classes       map[string]Class // configured CPU classes
	uncoreEnabled bool             // whether we need to care about uncore
	started       bool
}

type Class = cfgcpu.Class

var log logger.Logger = logger.NewLogger(CPUController)

// Ccontroller singleton instance.
var singleton *cpuctl

// getCPUController returns the (singleton) CPU controller instance.
func getCPUController() *cpuctl {
	if singleton == nil {
		singleton = &cpuctl{}
	}
	return singleton
}

// Check if our configuration is effectively empty.
func isEmptyConfig(cfg *cfgapi.Config) bool {
	return cfg == nil || cfg.CPU == nil || len(cfg.CPU.Classes) == 0
}

// Start initializes the controller for enforcing decisions.
func (ctl *cpuctl) Start(cache cache.Cache, cfg *cfgapi.Config) (bool, error) {
	if isEmptyConfig(cfg) {
		log.Info("empty configuration, disabling controller")
		return false, nil
	}

	sys, err := sysfs.DiscoverSystem()
	if err != nil {
		return false, fmt.Errorf("failed to discover system topology: %w", err)
	}

	ctl.system = sys
	ctl.cache = cache

	// DEBUG: dump the class assignments we have stored in the cache
	log.Debug("retrieved cpu class assignments from cache:\n%s", utils.DumpJSON(getClassAssignments(ctl.cache)))

	if err := ctl.configure(cfg); err != nil {
		// Just print an error. A config update later on may be valid.
		log.Error("failed apply /cpuinitial configuration: %v", err)
	}

	ctl.started = true

	return true, nil
}

// Stop shuts down the controller.
func (ctl *cpuctl) Stop() {
}

// PreCreateHook handler for the CPU controller.
func (ctl *cpuctl) PreCreateHook(c cache.Container) error {
	return nil
}

// PreStartHook handler for the CPU controller.
func (ctl *cpuctl) PreStartHook(c cache.Container) error {
	return nil
}

// PostStartHook handler for the CPU controller.
func (ctl *cpuctl) PostStartHook(c cache.Container) error {
	return nil
}

// PostUpdateHook handler for the CPU controller.
func (ctl *cpuctl) PostUpdateHook(c cache.Container) error {
	return nil
}

// PostStopHook handler for the CPU controller.
func (ctl *cpuctl) PostStopHook(c cache.Container) error {
	return nil
}

// enforceCpufreq enforces a class-specific cpufreq configuration to a cpuset
func (ctl *cpuctl) enforceCpufreq(class string, cpus ...int) error {
	if _, ok := ctl.classes[class]; !ok {
		return fmt.Errorf("non-existent cpu class %q", class)
	}

	min := int(ctl.classes[class].MinFreq)
	max := int(ctl.classes[class].MaxFreq)
	log.Debug("enforcing cpu frequency limits {%d, %d} from class %q on %v", min, max, class, cpus)

	if err := utils.SetCPUsScalingMinFreq(cpus, min); err != nil {
		return fmt.Errorf("Cannot set min freq %d: %w", min, err)
	}

	if err := utils.SetCPUsScalingMaxFreq(cpus, max); err != nil {
		return fmt.Errorf("Cannot set max freq %d: %w", max, err)
	}

	return nil
}

// enforceUncore enforces uncore frequency limits
func (ctl *cpuctl) enforceUncore(assignments cpuClassAssignments, affectedCPUs ...int) error {
	if !ctl.uncoreEnabled {
		return nil
	}

	cpus := cpuset.New(affectedCPUs...)

	for _, cpuPkgID := range ctl.system.PackageIDs() {
		cpuPkg := ctl.system.Package(cpuPkgID)
		for _, cpuDieID := range cpuPkg.DieIDs() {
			dieCPUs := cpuPkg.DieCPUSet(cpuDieID)

			// Check if this die is affected by the specified cpuset
			if cpus.Size() == 0 || dieCPUs.Intersection(cpus).Size() > 0 {
				min, max, minCls, maxCls := effectiveUncoreFreqs(utils.NewIDSet(dieCPUs.List()...), ctl.classes, assignments)

				if min == 0 && max == 0 {
					log.Debug("no uncore frequency limits for cpu package/die %d/%d", cpuPkgID, cpuDieID)
					continue
				}

				log.Debug("enforcing uncore min freq to %d (class %q), max freq to %d (class %q) on cpu package/die %d/%d", min, minCls, max, maxCls, cpuPkgID, cpuDieID)
				if min > 0 {
					if max > 0 && min > max {
						log.Warn("uncore frequency limit min > max (%d > %d) on cpu package/die %d/%d", min, max, cpuPkgID, cpuDieID)
					}

					if err := utils.SetUncoreMinFreq(cpuPkgID, cpuDieID, int(min)); err != nil {
						return err
					}
				}
				if max > 0 {
					if err := utils.SetUncoreMaxFreq(cpuPkgID, cpuDieID, int(max)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// effectiveUncoreClasses resolves the effective classes for setting the uncore
// frequency limits for a cpu package/die. It has "performance preference" so
// that the highest value (for both min and max) of the cpu classes effective
// on the die is selected.
func effectiveUncoreFreqs(cpus utils.IDSet, classes map[string]Class, assignments cpuClassAssignments) (minFreq, maxFreq uint, minCls, maxCls string) {
	for className, assignedCPUs := range assignments {
		// Check if this class is "effective" on the specified cpuset
		if idSetIntersects(cpus, assignedCPUs) {
			class := classes[className]
			if class.UncoreMinFreq > minFreq {
				minCls = className
				minFreq = class.UncoreMinFreq
			}
			if class.UncoreMaxFreq > maxFreq {
				maxCls = className
				maxFreq = class.UncoreMaxFreq
			}
		}
	}
	return minFreq, maxFreq, minCls, maxCls
}

func idSetIntersects(a, b utils.IDSet) bool {
	// Try to optimize the search for unbalanced idsets
	if len(a) < len(b) {
		for id := range a {
			if _, ok := b[id]; ok {
				return true
			}
		}
	} else {
		for id := range b {
			if _, ok := a[id]; ok {
				return true
			}
		}
	}
	return false
}

func (ctl *cpuctl) configure(cfg *cfgapi.Config) error {
	ctl.classes = nil
	ctl.uncoreEnabled = false

	if cfg != nil && cfg.CPU != nil {
		ctl.classes = cfg.CPU.Classes
	}

	// Re-configure CPUs that are assigned to some known class
	assignments := *getClassAssignments(ctl.cache)

	// DEBUG: dump the class assignments we have stored in the cache
	log.Debug("applying cpu controller configuration:\n%s", utils.DumpJSON(ctl.classes))

	// Sanity check
	uncoreAvailable := utils.UncoreFreqAvailable()
	for name, conf := range ctl.classes {
		if conf.UncoreMinFreq != 0 || conf.UncoreMaxFreq != 0 {
			if !uncoreAvailable {
				return fmt.Errorf("uncore limits set in cpu class %q but uncore driver not available in the system, make sure that the intel_uncore_frequency driver is loaded", name)
			}
			ctl.uncoreEnabled = true
			break
		}
	}

	// Configure the system
	for class, cpus := range assignments {
		if _, ok := ctl.classes[class]; ok {
			// Re-configure cpus (sysfs) according to new class parameters
			if err := ctl.enforceCpufreq(class, cpus.SortedMembers()...); err != nil {
				log.Error("cpufreq enforcement on re-configure failed: %v", err)
			}
		} else {
			// TODO: what should we really do with classes that do not exist in
			// the configuration anymore? Now we remember the CPUs assigned to
			// them. A further config update might re-introduce the class in
			// which case the CPUs will be reconfigured.
			log.Warn("class %q with cpus %v missing from the configuration", class, cpus)
		}
	}
	if err := ctl.enforceUncore(assignments); err != nil {
		log.Error("uncore frequency enforcement on re-configure failed: %v", err)
	}

	log.Debug("cpu controller configured")

	return nil
}

func (ctl *cpuctl) getClasses() map[string]Class {
	ret := make(map[string]Class, len(ctl.classes))
	for k, v := range ctl.classes {
		ret[k] = v
	}
	return ret
}

// Register us as a controller.
func init() {
	control.Register(CPUController, "CPU controller", getCPUController())
}
