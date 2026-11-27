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
	"encoding/json"
	"testing"
)

// FormatSize writes sizes in the largest binary unit, ParseSize reads them
// and the other spellings, and Size takes JSON numbers and strings.
func TestSize(t *testing.T) {
	for _, tc := range []struct {
		v    int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{1536, "1536"},
		{1 << 10, "1Ki"},
		{256 << 20, "256Mi"},
		{1 << 30, "1Gi"},
		{1536 << 20, "1536Mi"},
		{3 << 30, "3Gi"},
		{2 << 40, "2Ti"},
		{-(256 << 20), "-256Mi"},
	} {
		if got := FormatSize(tc.v); got != tc.want {
			t.Errorf("FormatSize(%d) = %q, expected %q", tc.v, got, tc.want)
		}
		if tc.v < 0 {
			continue
		}
		if got, err := ParseSize(tc.want); err != nil || got != tc.v {
			t.Errorf("ParseSize(%q) = %d, %v, expected %d", tc.want, got, err, tc.v)
		}
	}
	for _, tc := range []struct {
		s    string
		want int64
	}{
		{"256M", 256 << 20},
		{"256MiB", 256 << 20},
		{"256MB", 256 << 20},
		{"268435456", 256 << 20},
		{"0x10000000", 256 << 20},
		{"1g", 1 << 30},
		{"4Ki", 4 << 10},
	} {
		if got, err := ParseSize(tc.s); err != nil || got != tc.want {
			t.Errorf("ParseSize(%q) = %d, %v, expected %d", tc.s, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "M", "-1M", "256Xi", "0xzz", "9000000000T"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) accepted", bad)
		}
	}
	var v struct{ A, B Size }
	if err := json.Unmarshal([]byte(`{"A": "256Mi", "B": 268435456}`), &v); err != nil || v.A != 256<<20 || v.B != 256<<20 {
		t.Fatalf("unmarshal: %+v %v", v, err)
	}
	if b, err := json.Marshal(v); err != nil || string(b) != `{"A":268435456,"B":268435456}` {
		t.Fatalf("marshal: %s %v", b, err)
	}
}

func TestSerial(t *testing.T) {
	var v struct{ A, B Serial }
	if err := json.Unmarshal([]byte(`{"A": "0xC1EE0001", "B": 3253600257}`), &v); err != nil || v.A != 0xc1ee0001 || v.B != 0xc1ee0001 {
		t.Fatalf("unmarshal: %+v %v", v, err)
	}
	if b, err := json.Marshal(v); err != nil || string(b) != `{"A":"0xc1ee0001","B":"0xc1ee0001"}` {
		t.Fatalf("marshal: %s %v", b, err)
	}
}
