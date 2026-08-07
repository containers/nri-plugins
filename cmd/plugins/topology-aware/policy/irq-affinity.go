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

	"github.com/containers/nri-plugins/pkg/irq"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	"sigs.k8s.io/yaml"
)

type IrqAffinity struct {
	Claim []string `json:"claim,omitempty"`
	Mask  []string `json:"mask,omitempty"`
	Mode  IrqMode  `json:"mode,omitempty"`
}

type IrqMode string

const (
	// IrqAffinityModeDefault is the default IRQ affinity mode.
	IrqClaimFirst  IrqMode = "claim-first"
	IrqMaskFirst   IrqMode = "mask-first"
	IrqModeDefault IrqMode = IrqMaskFirst
)

func parseIrqAffinity(raw []byte) (*IrqAffinity, error) {
	parsed := &IrqAffinity{}
	if err := yaml.Unmarshal(raw, parsed); err != nil {
		return nil, fmt.Errorf("invalid IRQ affinity %q: %w", string(raw), err)
	}
	switch parsed.Mode {
	case "":
		parsed.Mode = IrqModeDefault
	case IrqClaimFirst, IrqMaskFirst:
	default:
		return nil, fmt.Errorf("invalid IRQ affinity mode %q (valid modes: %v)", parsed.Mode,
			[]IrqMode{IrqMaskFirst, IrqClaimFirst})
	}

	if _, err := irq.ValidateReferencedInterrupts(
		append(append([]string{}, parsed.Claim...), parsed.Mask...),
	); err != nil {
		return nil, err
	}

	return parsed, nil
}

func (p *policy) irqCpus(hwIrq *irq.Irq) (preMask, claim, mask cpuset.CPUSet) {
	preMask, claim, mask = cpuset.New(), cpuset.New(), cpuset.New()
	for _, g := range p.allocations.grants {
		irqs := g.IrqAffinity()
		switch {
		case irqs == nil:
			continue
		case g.ExclusiveCPUs().IsEmpty():
			continue
		}
		for _, c := range irqs.Claim {
			if !hwIrq.Match(c) {
				continue
			}
			claim = claim.Union(g.ExclusiveCPUs())
			log.Debugf("irq: %s claims %s", g.GetContainer().PrettyName(), hwIrq.String())
		}
		for _, m := range irqs.Mask {
			if !hwIrq.Match(m) {
				continue
			}
			if irqs.Mode == IrqClaimFirst {
				preMask = preMask.Union(g.ExclusiveCPUs())
				log.Debugf("irq: %s pre-masks %s", g.GetContainer().PrettyName(), hwIrq.String())
			} else {
				mask = mask.Union(g.ExclusiveCPUs())
				log.Debugf("irq: %s masks %s", g.GetContainer().PrettyName(), hwIrq.String())
			}
		}
	}

	return preMask, claim, mask
}

func (p *policy) applyIrqAffinity(user string) {
	if applied, current := p.irqCnt, p.allocations.irqState(); applied != current {
		log.Debugf("%s: updating active IRQ affinity (applied: %d, current: %d)",
			user, applied, current)
	} else {
		log.Debugf("%s: no change in active IRQ affinity, skipping update...", user)
		return
	}

	hwIrqs, err := irq.Interrupts()
	if err != nil {
		log.Errorf("failed to read HW interrupts: %v", err)
		return
	}

	for _, hwIrq := range hwIrqs {
		current, err := hwIrq.AffinityCpus()
		if err != nil {
			log.Errorf("%s: failed to read affinity: %v", hwIrq.String(), err)
			continue
		}

		preMask, claim, mask := p.irqCpus(hwIrq)

		if both := claim.Intersection(mask); !both.IsEmpty() {
			log.Warnf("%s: both claimed and masked for cpus %s, will give claims priority",
				hwIrq.String(), both.String())
			mask = mask.Difference(both)
		}

		cpus := p.allowed.Difference(preMask)
		if !claim.IsEmpty() {
			cpus = cpus.Intersection(claim)
		}
		if !mask.IsEmpty() {
			cpus = cpus.Difference(mask)
		}

		if cpus.Equals(current) {
			continue
		}

		if err := hwIrq.SetAffinityCpus(cpus); err != nil {
			log.Errorf("%s: failed to set affinity to cpus %s (for %s): %v",
				hwIrq.String(), cpus.String(), user, err)
		}
	}

	p.irqCnt = p.allocations.irqState()
}
