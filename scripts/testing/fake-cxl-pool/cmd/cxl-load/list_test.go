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

package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
		err  string
	}{
		{in: "5", want: []int{5}},
		{in: "64-67", want: []int{64, 65, 66, 67}},
		{in: " 1-2,5-6\n", want: []int{1, 2, 5, 6}},
		{in: "6,5,5-6", want: []int{5, 6}},
		{in: "0-3,128-129", want: []int{0, 1, 2, 3, 128, 129}},
		{in: "", err: "empty list"},
		{in: "a", err: `"a" is not a number`},
		{in: "-1", err: `"" is not a number`},
		{in: "6-5", err: "range 6-5 ends before it starts"},
		{in: "1,,2", err: `"" is not a number`},
		{in: "65536", err: `"65536" is not a number`},
	} {
		got, err := parseList(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("parseList(%q): error %v, want one with %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseList(%q) = %v, %v, want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestFormatList(t *testing.T) {
	for _, tc := range []struct {
		in   []int
		want string
	}{
		{in: nil, want: ""},
		{in: []int{3}, want: "3"},
		{in: []int{2, 3}, want: "2-3"},
		{in: []int{1, 3, 4, 5, 9}, want: "1,3-5,9"},
		{in: []int{64, 65, 66, 192}, want: "64-66,192"},
	} {
		if got := formatList(tc.in); got != tc.want {
			t.Errorf("formatList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		err  string
	}{
		{in: "4096", want: 4096},
		{in: "256K", want: 256 << 10},
		{in: "64KiB", want: 64 << 10},
		{in: "512M", want: 512 << 20},
		{in: "512MB", want: 512 << 20},
		{in: "2g", want: 2 << 30},
		{in: " 1GiB ", want: 1 << 30},
		{in: "", err: "is not a size"},
		{in: "M", err: "is not a size"},
		{in: "0", err: "is not a size"},
		{in: "-4K", err: "is not a size"},
		{in: "1.5G", err: "is not a size"},
		{in: "12T", err: "is not a size"},
		{in: "9999999999999G", err: "too large"},
	} {
		got, err := parseSize(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("parseSize(%q): error %v, want one with %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseSize(%q) = %d, %v, want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{in: 4096, want: "4 KiB"},
		{in: 256 << 10, want: "256 KiB"},
		{in: 512 << 20, want: "512 MiB"},
		{in: 3 << 30, want: "3 GiB"},
		{in: 1536 << 20, want: "1536 MiB"},
		{in: 100, want: "100 B"},
	} {
		if got := formatSize(tc.in); got != tc.want {
			t.Errorf("formatSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{in: 0, want: "0s"},
		{in: 20 * time.Microsecond, want: "20us"},
		{in: 1500 * time.Microsecond, want: "1.5ms"},
		{in: 5 * time.Second, want: "5s"},
	} {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
