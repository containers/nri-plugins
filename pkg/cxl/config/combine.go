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

package config

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// ResultSettings are the container settings of one allocation result that
// a container uses.
type ResultSettings struct {
	// Container is the resolved container section, nil: none.
	Container *Container `json:"container,omitempty"`
	// Origins has the origin of each set field of Container by field path.
	Origins map[string]string `json:"origins,omitempty"`
}

// Effective are the settings of a container.
type Effective struct {
	// Container has the effective settings: Steering and its Policy, and
	// Fencing are always set.
	Container Container
	// Origins has the origin of every set field by field path: the origin
	// of a result, OriginNodeDefault or OriginDefault.
	Origins map[string]string
}

// field is a field of the container section for Combine.
type field struct {
	path string
	// value returns the value of the field as text, and whether it is set.
	value func(c *Container) (string, bool)
	// equal tells whether two values of the field are the same.
	equal func(a, b string) bool
	// copy sets the field of dst to that of src.
	copy func(dst, src *Container)
	// steeringParameter: the field belongs to the steering policy.
	steeringParameter bool
}

// fields are the fields of the container section in the order of checks.
var fields = []field{
	{
		path:  FieldPolicy,
		value: func(c *Container) (string, bool) { return steeringOf(c).Policy, steeringOf(c).Policy != "" },
		equal: sameText,
		copy:  func(dst, src *Container) { steeringFor(dst).Policy = src.Steering.Policy },
	},
	{
		path: FieldWaypoints,
		value: func(c *Container) (string, bool) {
			wps := steeringOf(c).Waypoints
			if len(wps) == 0 {
				return "", false
			}
			data, err := json.Marshal(wps)
			if err != nil {
				return fmt.Sprint(wps), true
			}
			return string(data), true
		},
		equal:             sameText,
		copy:              func(dst, src *Container) { steeringFor(dst).Waypoints = src.Steering.Waypoints },
		steeringParameter: true,
	},
	{
		path:              FieldMinStep,
		value:             func(c *Container) (string, bool) { return steeringOf(c).MinStep, steeringOf(c).MinStep != "" },
		equal:             sameQuantity,
		copy:              func(dst, src *Container) { steeringFor(dst).MinStep = src.Steering.MinStep },
		steeringParameter: true,
	},
	{
		path:              FieldMaxStep,
		value:             func(c *Container) (string, bool) { return steeringOf(c).MaxStep, steeringOf(c).MaxStep != "" },
		equal:             sameQuantity,
		copy:              func(dst, src *Container) { steeringFor(dst).MaxStep = src.Steering.MaxStep },
		steeringParameter: true,
	},
	{
		path:  FieldFencing,
		value: func(c *Container) (string, bool) { return c.Fencing, c.Fencing != "" },
		equal: sameText,
		copy:  func(dst, src *Container) { dst.Fencing = src.Fencing },
	},
}

// Combine returns the effective settings of a container that uses the
// results:
//   - every field that several results set has one value (lists and
//     objects compared as JSON, quantities by value), else the error is
//     "<field path>: <origin> says <value>, <origin> says <value>";
//   - the fields of the defaults that no result sets are taken, except
//     the steering parameters (waypoints, minStep, maxStep) of the
//     defaults when the results set another policy;
//   - an absent policy is none, an absent fencing allowed-mems;
//   - the effective steering is validated, the error names the origins.
//
// Containers without results get the defaults.
func Combine(results []ResultSettings, defaults Container) (*Effective, error) {
	combined := &Container{}
	combinedOrigins := map[string]string{}
	for _, f := range fields {
		first := -1
		var firstValue string
		for i, r := range results {
			if r.Container == nil {
				continue
			}
			v, ok := f.value(r.Container)
			if !ok {
				continue
			}
			if first < 0 {
				first, firstValue = i, v
				f.copy(combined, r.Container)
				combinedOrigins[f.path] = resultOrigin(r, f.path, i)
				continue
			}
			if !f.equal(firstValue, v) {
				return nil, fmt.Errorf("%s: %s says %s, %s says %s", f.path,
					resultOrigin(results[first], f.path, first), firstValue, resultOrigin(r, f.path, i), v)
			}
		}
	}

	eff := &Effective{Origins: map[string]string{}}
	defaultPolicy, combinedPolicy := steeringOf(&defaults).Policy, steeringOf(combined).Policy
	if defaultPolicy == "" {
		defaultPolicy = PolicyNone
	}
	for _, f := range fields {
		switch {
		case hasField(f, combined):
			f.copy(&eff.Container, combined)
			eff.Origins[f.path] = combinedOrigins[f.path]
		case f.steeringParameter && combinedPolicy != "" && combinedPolicy != defaultPolicy:
			// the parameters of the defaults go with their policy
		case hasField(f, &defaults):
			f.copy(&eff.Container, &defaults)
			eff.Origins[f.path] = OriginNodeDefault
		}
	}
	if steeringFor(&eff.Container).Policy == "" {
		eff.Container.Steering.Policy = PolicyNone
		eff.Origins[FieldPolicy] = OriginDefault
	}
	if eff.Container.Fencing == "" {
		eff.Container.Fencing = FencingAllowedMems
		eff.Origins[FieldFencing] = OriginDefault
	}
	if err := validateSteering(errPrefix, eff.Container.Steering, true, eff.Origins); err != nil {
		return nil, err
	}
	return eff, nil
}

// Steers tells whether the effective steering policy is not none.
func (e *Effective) Steers() bool {
	return e.Container.Steering != nil && e.Container.Steering.Policy != PolicyNone
}

// PolicyText returns the effective policy and its origin, such as
// "none (default)", "auto from claim ns/a request r" or "first-dram (node
// default)".
func (e *Effective) PolicyText() string {
	return steeringOf(&e.Container).Policy + originText(e.Origins[FieldPolicy])
}

// FencingText returns the effective fencing and its origin, such as
// "allowed-mems (default)" or "none (node default)".
func (e *Effective) FencingText() string {
	return e.Container.Fencing + originText(e.Origins[FieldFencing])
}

// hasField tells whether the field is set in the container section.
func hasField(f field, c *Container) bool {
	_, ok := f.value(c)
	return ok
}

// resultOrigin returns the origin of a field of a result, "result <i>"
// when the result has none.
func resultOrigin(r ResultSettings, path string, i int) string {
	if o := r.Origins[path]; o != "" {
		return o
	}
	return fmt.Sprintf("result %d", i)
}

// steeringOf returns the steering of the container section, the zero
// Steering when there is none.
func steeringOf(c *Container) *Steering {
	if c == nil || c.Steering == nil {
		return &Steering{}
	}
	return c.Steering
}

// steeringFor returns the steering of the container section, created if
// there is none.
func steeringFor(c *Container) *Steering {
	if c.Steering == nil {
		c.Steering = &Steering{}
	}
	return c.Steering
}

// sameText tells whether two values are the same text.
func sameText(a, b string) bool {
	return a == b
}

// sameQuantity tells whether two quantities have the same value; values
// that are not quantities are compared as text.
func sameQuantity(a, b string) bool {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return qa.Cmp(qb) == 0
}
