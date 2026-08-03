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

package topologyaware

import (
	"fmt"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

func (p *policy) validateCpuClasses() error {
	// currently cfgapi.Config.Validate() does enough validation for us.
	return nil
}

func (p *policy) setReservedCpuClass() {
	if p.cpuClasses == nil {
		return
	}
	if opt.ReservedCpuClass == "" {
		return
	}
	if err := p.cpuClasses.UseClass(opt.ReservedCpuClass, p.reserved); err != nil {
		log.Errorf("failed to set reserved CPU class for %s: %v", p.reserved, err)
	}
}

func (p *policy) resolveCpuClass(ctr cache.Container) (string, error) {
	if p.cpuClasses == nil {
		return "", nil
	}

	class, err := cpuClassPreference(ctr)
	if err != nil {
		return "", err
	}

	if !p.cpuClasses.IsKnownClass(class) {
		return "", fmt.Errorf("unknown CPU class %q", class)
	}

	return class, nil
}

func (p *policy) resetCpuClass(subject string, cpus cpuset.CPUSet) {
	if p.cpuClasses == nil {
		return
	}
	if opt.SharedCpuClass == "" {
		return
	}
	if err := p.cpuClasses.UseClass(opt.SharedCpuClass, cpus); err != nil {
		log.Errorf("%s: failed to reset CPU class for %s: %v", subject, cpus, err)
	}
}

func (p *policy) commitCpuClasses(subject string) {
	if p.cpuClasses == nil {
		return
	}
	if err := p.cpuClasses.Commit(); err != nil {
		log.Errorf("%s: failed to commit CPU class changes: %v", subject, err)
	}
}
