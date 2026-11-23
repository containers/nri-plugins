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
	"bytes"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// errPrefix starts the errors of a MemoryConfig.
	errPrefix = "MemoryConfig: "
	// defaultsPrefix starts the field paths of the errors of the
	// defaults of the driver configuration.
	defaultsPrefix = "defaults."
)

// validate returns an error if a value of a resolved configuration that
// needs no device facts is invalid.
func (c *MemoryConfig) validate() error {
	if c.Container != nil {
		if err := validateSteering(errPrefix, c.Container.Steering, false, nil); err != nil {
			return err
		}
	}
	return c.Pool.validate()
}

// validate returns an error if the pool section has an empty controller
// key or a value that is not a JSON object.
func (p *Pool) validate() error {
	if p == nil {
		return nil
	}
	for key, value := range p.Opaque {
		if key == "" {
			return fmt.Errorf("%spool.opaque: empty controller key", errPrefix)
		}
		if v := bytes.TrimSpace(value); len(v) == 0 || v[0] != '{' {
			return fmt.Errorf("%spool.opaque.%s: not a JSON object", errPrefix, key)
		}
	}
	return nil
}

// validateSteering returns an error if the steering is not usable.
//   - prefix: the start of the messages before the field path
//   - s: the steering, nil: none
//   - effective: s is the effective steering of a container: no policy
//     is none, and policy waypoints needs waypoints
//   - origins: the origins of the fields by field path for the messages,
//     nil: the messages name no origins
func validateSteering(prefix string, s *Steering, effective bool, origins map[string]string) error {
	if s == nil {
		return nil
	}
	policy := s.Policy
	if policy != "" && !slices.Contains(policies, policy) {
		return fmt.Errorf("%s%s: invalid policy %q (%s)", prefix, FieldPolicy, policy, joinList(policies, "or"))
	}
	minStep, err := parseQuantity(prefix+FieldMinStep, s.MinStep, true)
	if err != nil {
		return err
	}
	maxStep, err := parseQuantity(prefix+FieldMaxStep, s.MaxStep, true)
	if err != nil {
		return err
	}
	if err := validateWaypoints(prefix+FieldWaypoints, s.Waypoints); err != nil {
		return err
	}
	if policy == "" && effective {
		policy = PolicyNone
	}
	from := func(fields ...string) string {
		return fieldOrigins(s, policy, origins, fields...)
	}
	if policy == PolicyNone {
		for _, f := range []struct {
			field string
			set   bool
		}{
			{FieldWaypoints, len(s.Waypoints) > 0},
			{FieldMinStep, s.MinStep != ""},
			{FieldMaxStep, s.MaxStep != ""},
		} {
			if f.set {
				return fmt.Errorf("%s%s needs a policy other than %s%s", prefix, f.field, PolicyNone, from(FieldPolicy, f.field))
			}
		}
	}
	switch {
	case policy != "" && policy != PolicyWaypoints && len(s.Waypoints) > 0:
		return fmt.Errorf("%s%s needs policy %s%s", prefix, FieldWaypoints, PolicyWaypoints, from(FieldPolicy, FieldWaypoints))
	case effective && policy == PolicyWaypoints && len(s.Waypoints) == 0:
		return fmt.Errorf("%s%s %s needs waypoints%s", prefix, FieldPolicy, PolicyWaypoints, from(FieldPolicy))
	case minStep > 0 && maxStep > 0 && minStep > maxStep:
		return fmt.Errorf("%s%s %s exceeds maxStep %s%s", prefix, FieldMinStep, s.MinStep, s.MaxStep, from(FieldMinStep, FieldMaxStep))
	}
	return nil
}

// fieldOrigins returns " (<field> <value> <origin>, ...)" of the steering
// fields, "" without origins.
//   - policy: the value of the policy field to show
func fieldOrigins(s *Steering, policy string, origins map[string]string, fields ...string) string {
	if origins == nil {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		name := field[strings.LastIndex(field, ".")+1:]
		switch field {
		case FieldPolicy:
			name += " " + policy
		case FieldMinStep:
			name += " " + s.MinStep
		case FieldMaxStep:
			name += " " + s.MaxStep
		}
		parts = append(parts, name+originText(origins[field]))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// originText returns how a value of the origin is told in messages:
// " (default)", " (node default)" or " from <origin>".
func originText(origin string) string {
	switch origin {
	case OriginDefault, "":
		return " (" + OriginDefault + ")"
	case OriginNodeDefault:
		return " (" + OriginNodeDefault + ")"
	}
	return " from " + origin
}

// validateWaypoints returns an error if a waypoint has no target usages,
// an invalid memory type or usage, a memory type twice, or a usage that is
// less than in the previous waypoint.
//   - path: the field path of the waypoints in the messages
func validateWaypoints(path string, wps []Waypoint) error {
	types := []string{MemoryTypeDRAM, MemoryTypeCXL}
	prev := make([]int64, len(types))
	prevText := []string{"0", "0"}
	for i, wp := range wps {
		at := fmt.Sprintf("%s[%d]", path, i)
		if len(wp.TargetUsages) == 0 {
			return fmt.Errorf("%s.targetUsages: empty", at)
		}
		usage := slices.Clone(prev)
		usageText := slices.Clone(prevText)
		seen := make([]bool, len(types))
		for j, tu := range wp.TargetUsages {
			tat := fmt.Sprintf("%s.targetUsages[%d]", at, j)
			k := slices.Index(types, tu.MemoryType)
			if k < 0 {
				return fmt.Errorf("%s.memoryType: invalid memory type %q (%s)", tat, tu.MemoryType, joinList(types, "or"))
			}
			if seen[k] {
				return fmt.Errorf("%s.memoryType: %s twice in the waypoint", tat, tu.MemoryType)
			}
			seen[k] = true
			if tu.Usage == "" {
				return fmt.Errorf("%s.usage: missing", tat)
			}
			v, err := parseQuantity(tat+".usage", tu.Usage, false)
			if err != nil {
				return err
			}
			usage[k], usageText[k] = v, tu.Usage
		}
		for k := range types {
			if usage[k] < prev[k] {
				return fmt.Errorf("%s: %s usage %s is less than %s of waypoint %d", at, types[k], usageText[k], prevText[k], i-1)
			}
		}
		prev, prevText = usage, usageText
	}
	return nil
}

// parseQuantity returns the bytes of a quantity, 0 for "".
//   - path: the field path of the quantity in the messages
//   - positive: the quantity must be positive, else not negative
func parseQuantity(path, s string, positive bool) (int64, error) {
	if s == "" {
		return 0, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid quantity %q", path, s)
	}
	switch {
	case positive && q.Sign() <= 0:
		return 0, fmt.Errorf("%s: invalid quantity %q (must be positive)", path, s)
	case q.Sign() < 0:
		return 0, fmt.Errorf("%s: invalid quantity %q (must not be negative)", path, s)
	}
	return q.Value(), nil
}

// joinList returns the values as "a, b <last> c", with last "or" or
// "and".
func joinList(values []string, last string) string {
	if len(values) < 2 {
		return strings.Join(values, "")
	}
	return strings.Join(values[:len(values)-1], ", ") + " " + last + " " + values[len(values)-1]
}
