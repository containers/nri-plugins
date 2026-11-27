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

package qemu

import (
	"encoding/json"
	"strings"
)

// Opts is a parsed qemu option string "first,key=value,...": First is the
// implicit first value (driver, qom-type, backend), Props the key=value
// pairs in order, Flags the keys given without a value.
type Opts struct {
	First string
	Keys  []string
	Props map[string]string
}

// Get returns the value of a key.
func (o *Opts) Get(key string) string {
	if o == nil {
		return ""
	}
	return o.Props[key]
}

// Has returns true if the key was given.
func (o *Opts) Has(key string) bool {
	if o == nil {
		return false
	}
	_, ok := o.Props[key]
	return ok
}

// ParseOpts parses a qemu option string. A ",," is an escaped comma. The
// first element is implicit (First) if it does not contain '='; firstKey,
// if not empty, is also accepted as an explicit key for it (for instance
// "driver" or "qom-type"). A JSON object argument ("{...}") is parsed as
// JSON, with firstKey as the First value.
func ParseOpts(s, firstKey string) *Opts {
	o := &Opts{Props: map[string]string{}}
	if strings.HasPrefix(strings.TrimSpace(s), "{") {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			for k, v := range m {
				var str string
				switch vv := v.(type) {
				case string:
					str = vv
				case bool:
					if vv {
						str = "on"
					} else {
						str = "off"
					}
				default:
					b, _ := json.Marshal(vv)
					str = string(b)
				}
				if k == firstKey {
					o.First = str
					continue
				}
				o.Keys = append(o.Keys, k)
				o.Props[k] = str
			}
			return o
		}
	}
	for i, part := range splitOpts(s) {
		k, v, hasEq := strings.Cut(part, "=")
		if !hasEq {
			if i == 0 {
				o.First = part
				continue
			}
			v = "on"
		}
		if firstKey != "" && k == firstKey && o.First == "" {
			o.First = v
			continue
		}
		if _, dup := o.Props[k]; !dup {
			o.Keys = append(o.Keys, k)
		}
		o.Props[k] = v
	}
	return o
}

// splitOpts splits at single commas, turning ",," into ",".
func splitOpts(s string) []string {
	var (
		parts []string
		cur   strings.Builder
	)
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if i+1 < len(s) && s[i+1] == ',' {
				cur.WriteByte(',')
				i++
				continue
			}
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(s[i])
	}
	parts = append(parts, cur.String())
	return parts
}

// EscapeOptValue escapes commas in an option value.
func EscapeOptValue(v string) string {
	return strings.ReplaceAll(v, ",", ",,")
}
