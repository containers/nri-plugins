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
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	resourceapi "k8s.io/api/resource/v1"
	sigsjson "sigs.k8s.io/json"

	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// The origins of values that do not come from a config entry.
const (
	// OriginDefault is the origin of a built-in default.
	OriginDefault = "default"
	// OriginNodeDefault is the origin of a value of the defaults of the
	// driver configuration.
	OriginNodeDefault = "node default"
)

// Resolved is the MemoryConfig of one allocation result.
type Resolved struct {
	// Config is the merged and validated configuration, apiVersion and
	// kind set, without the sections and objects that set nothing.
	Config MemoryConfig
	// Origins has the origin of each set field of the container section
	// by field path (Field*): "claim NS/NAME request R" or "class NAME".
	Origins map[string]string
}

// Settings returns the container settings of the result for Combine.
func (r *Resolved) Settings() ResultSettings {
	return ResultSettings{Container: r.Config.Container, Origins: r.Origins}
}

// entry is a decoded config entry of an allocation.
type entry struct {
	origin string
	// params are the parameters without apiVersion and kind.
	params map[string]json.RawMessage
}

// Resolve returns the MemoryConfig of the allocation result of the
// request of the claim for the driver: the opaque config entries of the
// driver that target the request (no requests, the request, or its main
// request "r" of a subrequest "r/s"), DeviceClass entries first, then
// claim entries, in list order, merged as JSON merge patches (RFC 7386).
// Every entry is decoded strictly and checked alone; the merge is
// decoded strictly and its values that need no device facts are
// validated. The device section is checked by the Device methods.
func Resolve(claim *resourceapi.ResourceClaim, driver, request string) (*Resolved, error) {
	list, err := applicableEntries(claim, driver, request)
	if err != nil {
		return nil, err
	}
	merged := []byte("{}")
	for _, e := range list {
		patch, err := json.Marshal(e.params)
		if err == nil {
			merged, err = jsonpatch.MergePatch(merged, patch)
		}
		if err != nil {
			return nil, fmt.Errorf("%s%s: %w", errPrefix, e.origin, err)
		}
	}
	res := &Resolved{}
	if err := strictUnmarshal(merged, &res.Config); err != nil {
		return nil, fmt.Errorf("%smerged config: %w", errPrefix, err)
	}
	res.Config.APIVersion, res.Config.Kind = APIVersion, Kind
	res.Config.normalize()
	if err := res.Config.validate(); err != nil {
		return nil, err
	}
	res.Origins = origins(list, res.Config.Container)
	return res, nil
}

// CheckEntries returns an error if an opaque config entry of the driver in
// the allocation of the claim, whatever requests it targets, is not a
// valid MemoryConfig entry by itself: its kind, apiVersion, fields and
// the sources of its fields. The values are checked by Resolve.
func CheckEntries(claim *resourceapi.ResourceClaim, driver string) error {
	if claim.Status.Allocation == nil {
		return noAllocation(claim)
	}
	index := map[resourceapi.AllocationConfigSource]int{}
	for _, c := range claim.Status.Allocation.Devices.Config {
		i := index[c.Source]
		index[c.Source]++
		if c.Opaque == nil || c.Opaque.Driver != driver {
			continue
		}
		if _, err := decodeEntry(c.Opaque.Parameters.Raw, c.Source, i, driver); err != nil {
			return err
		}
	}
	return nil
}

// applicableEntries returns the decoded config entries of the driver that
// target the request, class entries first.
func applicableEntries(claim *resourceapi.ResourceClaim, driver, request string) ([]entry, error) {
	if claim.Status.Allocation == nil {
		return nil, noAllocation(claim)
	}
	main, _, _ := strings.Cut(request, "/")
	var out []entry
	for _, source := range []resourceapi.AllocationConfigSource{resourceapi.AllocationConfigSourceClass, resourceapi.AllocationConfigSourceClaim} {
		i := -1
		for _, c := range claim.Status.Allocation.Devices.Config {
			if c.Source != source {
				continue
			}
			i++
			if c.Opaque == nil || c.Opaque.Driver != driver {
				continue
			}
			if len(c.Requests) > 0 && !slices.Contains(c.Requests, request) && !slices.Contains(c.Requests, main) {
				continue
			}
			params, err := decodeEntry(c.Opaque.Parameters.Raw, source, i, driver)
			if err != nil {
				return nil, err
			}
			origin := fmt.Sprintf("claim %s/%s request %s", claim.Namespace, claim.Name, request)
			if source == resourceapi.AllocationConfigSourceClass {
				origin = classOrigin(claim, request)
			}
			out = append(out, entry{origin: origin, params: params})
		}
	}
	return out, nil
}

// decodeEntry returns the parameters of a config entry without apiVersion
// and kind. It returns an error if the entry is not a MemoryConfig of
// APIVersion or has unknown fields. Fencing is an error in every entry,
// and the pool section in an entry of the CXL driver.
//   - index: the position of the entry among the entries of its source
func decodeEntry(raw []byte, source resourceapi.AllocationConfigSource, index int, driver string) (map[string]json.RawMessage, error) {
	where := fmt.Sprintf("%s%s config %d", errPrefix, source, index)
	if v := bytes.TrimSpace(raw); len(v) == 0 || v[0] != '{' {
		return nil, fmt.Errorf("%s: not a JSON object", where)
	}
	var head struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := sigsjson.UnmarshalCaseSensitivePreserveInts(raw, &head); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	if head.Kind != Kind {
		return nil, fmt.Errorf("%s: kind %q, expected %q", where, head.Kind, Kind)
	}
	if head.APIVersion != APIVersion {
		return nil, fmt.Errorf("%s: apiVersion %q, expected %q", where, head.APIVersion, APIVersion)
	}
	if err := strictUnmarshal(raw, &MemoryConfig{}); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	params := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	delete(params, "apiVersion")
	delete(params, "kind")
	if lookup(params, strings.Split(FieldFencing, ".")) == valueSet {
		return nil, fmt.Errorf("%s%s: set by the driver configuration only (defaults.%s)", errPrefix, FieldFencing, FieldFencing)
	}
	if driver == device.DriverName && lookup(params, []string{"pool"}) == valueSet {
		return nil, fmt.Errorf("%spool: only for devices of %s", errPrefix, device.PoolDriverName)
	}
	return params, nil
}

// strictUnmarshal decodes data into out with JSON keys matched case
// sensitively, and returns an error for unknown and duplicate fields.
func strictUnmarshal(data []byte, out any) error {
	strict, err := sigsjson.UnmarshalStrict(data, out)
	if err != nil {
		return err
	}
	if len(strict) > 0 {
		return fmt.Errorf("json: %w", strict[0])
	}
	return nil
}

// The states of a field in a config entry.
const (
	valueAbsent = iota
	valueNull
	valueSet
)

// lookup returns whether the field at path is absent, null (also when an
// object on the path is null) or set in the parameters.
func lookup(params map[string]json.RawMessage, path []string) int {
	obj := params
	for i, key := range path {
		raw, ok := obj[key]
		if !ok {
			return valueAbsent
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return valueNull
		}
		if i == len(path)-1 {
			return valueSet
		}
		obj = nil
		if err := json.Unmarshal(raw, &obj); err != nil {
			return valueAbsent
		}
	}
	return valueAbsent
}

// origins returns the origin of each set field of the container section:
// the last entry that set it, unless a later one removed it.
func origins(entries []entry, c *Container) map[string]string {
	if c == nil {
		return nil
	}
	out := map[string]string{}
	for _, f := range fields {
		if !hasField(f, c) {
			continue
		}
		path := strings.Split(f.path, ".")
		for _, e := range entries {
			switch lookup(e.params, path) {
			case valueSet:
				out[f.path] = e.origin
			case valueNull:
				delete(out, f.path)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// classOrigin returns the origin of a DeviceClass entry of the request:
// "class NAME" of the device class of the request or subrequest.
func classOrigin(claim *resourceapi.ResourceClaim, request string) string {
	main, sub, isSub := strings.Cut(request, "/")
	for _, r := range claim.Spec.Devices.Requests {
		if r.Name != main {
			continue
		}
		switch {
		case !isSub && r.Exactly != nil && r.Exactly.DeviceClassName != "":
			return "class " + r.Exactly.DeviceClassName
		case isSub:
			for _, s := range r.FirstAvailable {
				if s.Name == sub && s.DeviceClassName != "" {
					return "class " + s.DeviceClassName
				}
			}
		}
	}
	return "class of request " + request
}

// noAllocation returns the error of a claim without an allocation.
func noAllocation(claim *resourceapi.ResourceClaim) error {
	return fmt.Errorf("%sclaim %s/%s has no allocation", errPrefix, claim.Namespace, claim.Name)
}
