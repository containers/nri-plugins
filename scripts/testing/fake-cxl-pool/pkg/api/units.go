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

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
)

// Size is a byte count that unmarshals from a JSON number or from a string
// like "256Mi", "256M", "1GiB" or "268435456". Suffixes are binary
// (K=Ki=1024). It marshals as a JSON number.
type Size int64

// UnmarshalJSON implements json.Unmarshaler.
func (s *Size) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		v, err := ParseSize(str)
		if err != nil {
			return err
		}
		*s = Size(v)
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("invalid size %s: %w", string(b), err)
	}
	*s = Size(v)
	return nil
}

// ParseSize parses a size string: a decimal or 0x-hex number of bytes,
// optionally followed by K, M, G, T (also Ki, KiB, MB, ...; all binary).
func ParseSize(s string) (int64, error) {
	str := strings.TrimSpace(s)
	if str == "" {
		return 0, fmt.Errorf("empty size")
	}
	if strings.HasPrefix(str, "0x") || strings.HasPrefix(str, "0X") {
		v, err := strconv.ParseUint(str[2:], 16, 63)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q", s)
		}
		return int64(v), nil
	}
	up := strings.ToUpper(str)
	up = strings.TrimSuffix(up, "B")
	up = strings.TrimSuffix(up, "I")
	mult := int64(1)
	if n := len(up); n > 0 {
		switch up[n-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult != 1 {
			up = up[:n-1]
		}
	}
	v, err := strconv.ParseInt(strings.TrimSpace(up), 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if v > (1<<63-1)/mult {
		return 0, fmt.Errorf("size %q overflows", s)
	}
	return v * mult, nil
}

// FormatSize returns bytes in the largest binary unit that divides them,
// as a Kubernetes quantity: "256Mi", "1Gi", "1536" ("0" for 0).
func FormatSize(v int64) string {
	units := []struct {
		s string
		m int64
	}{{"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}}
	if v == 0 {
		return "0"
	}
	for _, u := range units {
		if v%u.m == 0 {
			return strconv.FormatInt(v/u.m, 10) + u.s
		}
	}
	return strconv.FormatInt(v, 10)
}

// Serial is a serial number that unmarshals from a JSON number or string
// (YAML configs may have unquoted 0x... numbers). It marshals as a string.
type Serial uint64

// UnmarshalJSON implements json.Unmarshaler.
func (s *Serial) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		v, err := cxlapi.ParseSerial(str)
		if err != nil {
			return err
		}
		*s = Serial(v)
		return nil
	}
	var v uint64
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("invalid serial %s: %w", string(b), err)
	}
	*s = Serial(v)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (s Serial) MarshalJSON() ([]byte, error) {
	return json.Marshal(cxlapi.FormatSerial(uint64(s)))
}

// Duration is a time.Duration that (un)marshals as a string like "30s".
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		v, err := time.ParseDuration(str)
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return fmt.Errorf("invalid duration %s", string(b))
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}
