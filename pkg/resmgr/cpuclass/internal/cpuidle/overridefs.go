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

package cpuidle

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/intel/goresctrl/pkg/cstates"
	"github.com/intel/goresctrl/pkg/utils"
)

// cstatesEnvOverridesJson lets e2e tests inject a simulated cstates
// sysfs through the OVERRIDE_SYS_CSTATES environment variable. The
// variable is read once at process start. Production deployments
// leave it unset and use real sysfs.
var (
	cstatesEnvOverridesVar  = "OVERRIDE_SYS_CSTATES"
	cstatesEnvOverridesJson = os.Getenv(cstatesEnvOverridesVar)
)

type cstatesOverrides []cstatesOverride
type cstatesOverride struct {
	Cpus  string            `json:"cpus"`
	Names []string          `json:"names"`
	Files map[string]string `json:"files"`
}

type cstatesOverrideFs struct {
	overrides    cstatesOverrides
	stateName    map[int]string
	nameState    map[string]int
	cpuStateFile map[utils.ID]map[int]map[string]string
}

// newCstatesFromOverride builds a *cstates.Cstates backed by an
// in-memory override fs constructed from the OVERRIDE_SYS_CSTATES
// JSON. Used only when that environment variable is set.
func newCstatesFromOverride(filter cstates.Filter) (*cstates.Cstates, error) {
	cs := cstates.NewCstates()
	ofs, err := newCstatesOverrideFs()
	if err != nil {
		return nil, fmt.Errorf("failed to create override fs from %s: %v", cstatesEnvOverridesVar, err)
	}
	cs.SetFs(ofs)
	if err := cs.Read(filter); err != nil {
		return nil, fmt.Errorf("failed to refresh cstates from %s overrides: %v", cstatesEnvOverridesVar, err)
	}
	return cs, nil
}

func newCstatesOverrideFs() (*cstatesOverrideFs, error) {
	ofs := &cstatesOverrideFs{
		stateName:    make(map[int]string),
		nameState:    make(map[string]int),
		cpuStateFile: make(map[utils.ID]map[int]map[string]string),
	}
	if err := json.Unmarshal([]byte(cstatesEnvOverridesJson), &ofs.overrides); err != nil {
		return nil, err
	}
	if len(ofs.overrides) == 0 {
		return nil, fmt.Errorf("no overrides found in %s", cstatesEnvOverridesVar)
	}
	names := make(map[string]bool)
	for _, o := range ofs.overrides {
		for _, name := range o.Names {
			names[name] = true
		}
	}
	orderedNames := make([]string, 0, len(names))
	for name := range names {
		orderedNames = append(orderedNames, name)
	}
	slices.Sort(orderedNames)
	for state, name := range orderedNames {
		ofs.stateName[state] = name
		ofs.nameState[name] = state
	}

	for _, o := range ofs.overrides {
		cpus, err := utils.NewIDSetFromString(o.Cpus)
		if err != nil {
			return nil, fmt.Errorf("invalid CPU list %q in %s: %v", o.Cpus, cstatesEnvOverridesVar, err)
		}
		for cpu := range cpus {
			cpuid := utils.ID(cpu)
			if _, ok := ofs.cpuStateFile[cpuid]; !ok {
				ofs.cpuStateFile[cpuid] = make(map[int]map[string]string)
			}
			for _, name := range o.Names {
				state := ofs.nameState[name]
				if _, ok := ofs.cpuStateFile[cpuid][state]; !ok {
					ofs.cpuStateFile[cpuid][state] = make(map[string]string)
				}
				maps.Copy(ofs.cpuStateFile[cpuid][state], o.Files)
				ofs.cpuStateFile[cpuid][state]["name"] = name
			}
		}
	}
	log.Debugf("cstates override fs: loaded overrides for %d CPUs C-states: %s", len(ofs.cpuStateFile), strings.Join(orderedNames, ", "))
	return ofs, nil
}

func (fs *cstatesOverrideFs) PossibleCpus() (string, error) {
	maxCpu := utils.ID(-1)
	for cpu := range fs.cpuStateFile {
		if cpu > maxCpu {
			maxCpu = cpu
		}
	}
	if maxCpu < 0 {
		return "", nil
	}
	return "0-" + strconv.Itoa(maxCpu), nil
}

func (fs *cstatesOverrideFs) CpuidleStates(cpuID utils.ID) ([]int, error) {
	states := []int{}
	for state := range fs.stateName {
		states = append(states, state)
	}
	slices.Sort(states)
	return states, nil
}

func (fs *cstatesOverrideFs) CpuidleStateAttrRead(cpu utils.ID, state int, attribute string) (string, error) {
	if stateFiles, ok := fs.cpuStateFile[cpu]; ok {
		if files, ok := stateFiles[state]; ok {
			if val, ok := files[attribute]; ok {
				log.Debugf("cstates override fs: read cpu%d cstate=%s %s=%q", cpu, fs.stateName[state], attribute, val)
				return val, nil
			}
		}
	}
	log.Errorf("cstates override fs: cannot read cpu%d cstate=%s attribute %q", cpu, fs.stateName[state], attribute)
	return "", os.ErrNotExist
}

func (fs *cstatesOverrideFs) CpuidleStateAttrWrite(cpu utils.ID, state int, attribute string, value string) error {
	if stateFiles, ok := fs.cpuStateFile[cpu]; ok {
		if files, ok := stateFiles[state]; ok {
			files[attribute] = value
			log.Debugf("cstates override fs: wrote cpu%d cstate=%s %s=%q", cpu, fs.stateName[state], attribute, value)
			return nil
		}
	}
	log.Errorf("cstates override fs: write to non-existing cpu%d cstate=%d %s=%q ignored", cpu, state, attribute, value)
	return nil
}
