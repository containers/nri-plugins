// Copyright 2026 Intel Corporation. All Rights Reserved.
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

// Tests for unexported sysfs utility functions.
package sysfs

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	idset "github.com/intel/goresctrl/pkg/utils"
)

// writeSysfsFile creates base/entry containing content for use in read tests.
func writeSysfsFile(t *testing.T, base, entry, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(base, entry), []byte(content), 0o600); err != nil {
		t.Fatalf("writeSysfsFile: %v", err)
	}
}

// --------------------------------------------------------------------------
// parseIntTo
// --------------------------------------------------------------------------

func TestParseIntTo(t *testing.T) {
	t.Run("int", func(t *testing.T) {
		for _, tc := range []struct {
			in      string
			want    int
			wantErr bool
		}{
			{"0", 0, false},
			{"42", 42, false},
			{"-1", -1, false},
			{"0xff", 255, false},
			{"abc", 0, true},
		} {
			v, err := parseIntTo[int](tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("parseIntTo[int](%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			}
			if err == nil && v != tc.want {
				t.Errorf("parseIntTo[int](%q) = %d, want %d", tc.in, v, tc.want)
			}
		}
	})

	t.Run("int8_overflow", func(t *testing.T) {
		if _, err := parseIntTo[int8]("128"); err == nil {
			t.Error("expected overflow error for int8(128), got nil")
		}
		if _, err := parseIntTo[int8]("-129"); err == nil {
			t.Error("expected overflow error for int8(-129), got nil")
		}
		v, err := parseIntTo[int8]("127")
		if err != nil || v != 127 {
			t.Errorf("parseIntTo[int8](127) = %d, %v; want 127, nil", v, err)
		}
	})

	t.Run("int16_overflow", func(t *testing.T) {
		if _, err := parseIntTo[int16]("32768"); err == nil {
			t.Error("expected overflow error for int16(32768), got nil")
		}
		v, err := parseIntTo[int16]("32767")
		if err != nil || v != 32767 {
			t.Errorf("parseIntTo[int16](32767) = %d, %v; want 32767, nil", v, err)
		}
	})

	t.Run("int32_overflow", func(t *testing.T) {
		if _, err := parseIntTo[int32]("2147483648"); err == nil {
			t.Error("expected overflow error for int32(2147483648), got nil")
		}
	})

	t.Run("int64_max", func(t *testing.T) {
		v, err := parseIntTo[int64]("9223372036854775807")
		if err != nil || v != 9223372036854775807 {
			t.Errorf("parseIntTo[int64](max) = %d, %v", v, err)
		}
	})

	// Named types whose underlying type is int should parse at strconv.IntSize bits,
	// not silently truncate into a smaller type.
	t.Run("named_int_type", func(t *testing.T) {
		type myID int
		v, err := parseIntTo[myID]("100")
		if err != nil || v != 100 {
			t.Errorf("parseIntTo[myID](100) = %d, %v; want 100, nil", v, err)
		}
	})

	// Named types with a small underlying type (e.g. int8) must also be range-checked
	// at int8 width, not int width. This was the regression case from PR #676.
	t.Run("named_int8_type_overflow", func(t *testing.T) {
		type myInt8 int8
		if _, err := parseIntTo[myInt8]("128"); err == nil {
			t.Error("expected overflow error for myInt8(128), got nil")
		}
		v, err := parseIntTo[myInt8]("127")
		if err != nil || v != 127 {
			t.Errorf("parseIntTo[myInt8](127) = %d, %v; want 127, nil", v, err)
		}
	})
}

// --------------------------------------------------------------------------
// parseUintTo
// --------------------------------------------------------------------------

func TestParseUintTo(t *testing.T) {
	t.Run("uint", func(t *testing.T) {
		v, err := parseUintTo[uint]("255")
		if err != nil || v != 255 {
			t.Errorf("parseUintTo[uint](255) = %d, %v; want 255, nil", v, err)
		}
		if _, err := parseUintTo[uint]("-1"); err == nil {
			t.Error("expected error for negative uint, got nil")
		}
	})

	t.Run("uint8_overflow", func(t *testing.T) {
		if _, err := parseUintTo[uint8]("256"); err == nil {
			t.Error("expected overflow error for uint8(256), got nil")
		}
		v, err := parseUintTo[uint8]("255")
		if err != nil || v != 255 {
			t.Errorf("parseUintTo[uint8](255) = %d, %v; want 255, nil", v, err)
		}
	})

	t.Run("uint16_overflow", func(t *testing.T) {
		if _, err := parseUintTo[uint16]("65536"); err == nil {
			t.Error("expected overflow error for uint16(65536), got nil")
		}
	})

	t.Run("uint32_overflow", func(t *testing.T) {
		if _, err := parseUintTo[uint32]("4294967296"); err == nil {
			t.Error("expected overflow error for uint32(4294967296), got nil")
		}
	})

	t.Run("uint64_max", func(t *testing.T) {
		v, err := parseUintTo[uint64]("18446744073709551615")
		if err != nil || v != 18446744073709551615 {
			t.Errorf("parseUintTo[uint64](max) = %d, %v", v, err)
		}
	})

	t.Run("hex", func(t *testing.T) {
		v, err := parseUintTo[uint32]("0xff")
		if err != nil || v != 255 {
			t.Errorf("parseUintTo[uint32](0xff) = %d, %v; want 255, nil", v, err)
		}
	})

	// Named types with a small underlying type (e.g. uint8) must be range-checked
	// at uint8 width. Without the reflect-based fix this would silently wrap.
	t.Run("named_uint8_type_overflow", func(t *testing.T) {
		type myUint8 uint8
		if _, err := parseUintTo[myUint8]("256"); err == nil {
			t.Error("expected overflow error for myUint8(256), got nil")
		}
		v, err := parseUintTo[myUint8]("255")
		if err != nil || v != 255 {
			t.Errorf("parseUintTo[myUint8](255) = %d, %v; want 255, nil", v, err)
		}
	})
}

// --------------------------------------------------------------------------
// parseIntSlice
// --------------------------------------------------------------------------

func TestParseIntSlice(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		got, err := parseIntSlice[int]("10 20 30", " ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []int{10, 20, 30}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("got[%d]=%d, want %d", i, got[i], want[i])
			}
		}
	})

	// Trailing separator must be skipped (continue), not stop parsing (break).
	t.Run("trailing_separator_skipped", func(t *testing.T) {
		got, err := parseIntSlice[int]("1 2 3 ", " ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("expected 3 elements, got %d: %v", len(got), got)
		}
	})

	// Empty string produces nil slice, not an error.
	t.Run("empty_string", func(t *testing.T) {
		got, err := parseIntSlice[int]("", " ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty slice, got %v", got)
		}
	})

	// Overflow in one element should propagate an error.
	t.Run("overflow_returns_error", func(t *testing.T) {
		if _, err := parseIntSlice[int8]("1 200 3", " "); err == nil {
			t.Error("expected error for int8 overflow in list, got nil")
		}
	})

	t.Run("negative_values", func(t *testing.T) {
		got, err := parseIntSlice[int32]("-3 0 7", " ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got[0] != -3 || got[1] != 0 || got[2] != 7 {
			t.Errorf("unexpected values: %v", got)
		}
	})
}

// --------------------------------------------------------------------------
// parseIDSet
// --------------------------------------------------------------------------

func TestParseIDSet(t *testing.T) {
	parse := func(s, sep string) (idset.IDSet, error) {
		var ids idset.IDSet
		return ids, parseIDSet(s, sep, &ids)
	}

	t.Run("single_ids", func(t *testing.T) {
		ids, err := parse("0 2 4", " ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, id := range []int{0, 2, 4} {
			if !ids.Has(idset.ID(id)) {
				t.Errorf("expected id %d in set", id)
			}
		}
		if ids.Size() != 3 {
			t.Errorf("expected size 3, got %d", ids.Size())
		}
	})

	t.Run("range", func(t *testing.T) {
		ids, err := parse("0-3", " ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids.Size() != 4 {
			t.Errorf("expected 4 elements, got %d: %v", ids.Size(), ids)
		}
		for i := 0; i <= 3; i++ {
			if !ids.Has(idset.ID(i)) {
				t.Errorf("missing id %d", i)
			}
		}
	})

	t.Run("mixed_ids_and_ranges", func(t *testing.T) {
		ids, err := parse("0-1 4 8-9", " ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, id := range []int{0, 1, 4, 8, 9} {
			if !ids.Has(idset.ID(id)) {
				t.Errorf("expected id %d in set %v", id, ids)
			}
		}
		if ids.Size() != 5 {
			t.Errorf("expected size 5, got %d", ids.Size())
		}
	})

	t.Run("comma_separator", func(t *testing.T) {
		ids, err := parse("0,1,2", ",")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids.Size() != 3 {
			t.Errorf("expected 3, got %d", ids.Size())
		}
	})

	// Typical sysfs cpulist format: comma-separated ranges, e.g. "0-7,15-23".
	t.Run("comma_separated_ranges", func(t *testing.T) {
		ids, err := parse("0-7,15-23", ",")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids.Size() != 17 {
			t.Errorf("expected 17, got %d: %v", ids.Size(), ids)
		}
		for _, id := range []int{0, 3, 7, 15, 19, 23} {
			if !ids.Has(idset.ID(id)) {
				t.Errorf("expected id %d in set %v", id, ids)
			}
		}
	})

	// Trailing separator should be skipped (continue not break) — same as for parseIntSlice.
	t.Run("trailing_separator_skipped", func(t *testing.T) {
		ids, err := parse("0 1 2 ", " ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids.Size() != 3 {
			t.Errorf("expected 3 ids, got %d: %v", ids.Size(), ids)
		}
	})

	t.Run("empty_string", func(t *testing.T) {
		ids, err := parse("", ",")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids.Size() != 0 {
			t.Errorf("expected empty set, got %v", ids)
		}
	})

	t.Run("invalid_id", func(t *testing.T) {
		if _, err := parse("0 abc 2", " "); err == nil {
			t.Error("expected error for non-numeric id, got nil")
		}
	})

	t.Run("invalid_range_end", func(t *testing.T) {
		if _, err := parse("0-abc", " "); err == nil {
			t.Error("expected error for invalid range end, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// readSysfsRaw
// --------------------------------------------------------------------------

func TestReadSysfsRaw(t *testing.T) {
	t.Run("reads_and_trims_newline", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "val", "hello\n")
		got, err := readSysfsRaw(dir, "val")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("trims_multiple_trailing_newlines", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "val", "42\n\n")
		got, err := readSysfsRaw(dir, "val")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "42" {
			t.Errorf("got %q, want %q", got, "42")
		}
	})

	t.Run("missing_file_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := readSysfsRaw(dir, "nonexistent"); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// readSysfsInt
// --------------------------------------------------------------------------

func TestReadSysfsInt(t *testing.T) {
	t.Run("reads_int", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "id", "7\n")
		var v int
		if err := readSysfsInt(dir, "id", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 7 {
			t.Errorf("got %d, want 7", v)
		}
	})

	t.Run("reads_named_int_type", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "id", "42\n")
		var v idset.ID
		if err := readSysfsInt(dir, "id", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 42 {
			t.Errorf("got %d, want 42", v)
		}
	})

	t.Run("overflow_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "id", "200\n")
		var v int8
		if err := readSysfsInt(dir, "id", &v); err == nil {
			t.Error("expected overflow error for int8(200), got nil")
		}
	})

	t.Run("missing_file_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		var v int
		if err := readSysfsInt(dir, "nonexistent", &v); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// readSysfsUint
// --------------------------------------------------------------------------

func TestReadSysfsUint(t *testing.T) {
	t.Run("reads_uint64", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "freq", "3200000\n")
		var v uint64
		if err := readSysfsUint(dir, "freq", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 3200000 {
			t.Errorf("got %d, want 3200000", v)
		}
	})

	t.Run("overflow_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "val", "256\n")
		var v uint8
		if err := readSysfsUint(dir, "val", &v); err == nil {
			t.Error("expected overflow error for uint8(256), got nil")
		}
	})

	t.Run("negative_value_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "val", "-1\n")
		var v uint64
		if err := readSysfsUint(dir, "val", &v); err == nil {
			t.Error("expected error for negative uint, got nil")
		}
	})

	t.Run("missing_file_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		var v uint64
		if err := readSysfsUint(dir, "nonexistent", &v); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// readSysfsString
// --------------------------------------------------------------------------

func TestReadSysfsString(t *testing.T) {
	t.Run("reads_string", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "type", "Unified\n")
		var v string
		if err := readSysfsString(dir, "type", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != "Unified" {
			t.Errorf("got %q, want %q", v, "Unified")
		}
	})

	t.Run("missing_file_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		var v string
		if err := readSysfsString(dir, "nonexistent", &v); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// readSysfsIDSet
// --------------------------------------------------------------------------

func TestReadSysfsIDSet(t *testing.T) {
	t.Run("reads_idset_with_range", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "cpulist", "0-3,8\n")
		var ids idset.IDSet
		if err := readSysfsIDSet(dir, "cpulist", ",", &ids); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids.Size() != 5 {
			t.Errorf("expected 5 ids, got %d: %v", ids.Size(), ids)
		}
		for _, id := range []int{0, 1, 2, 3, 8} {
			if !ids.Has(idset.ID(id)) {
				t.Errorf("expected id %d in set", id)
			}
		}
	})

	t.Run("missing_file_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		var ids idset.IDSet
		if err := readSysfsIDSet(dir, "nonexistent", ",", &ids); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})

	t.Run("malformed_content_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "cpulist", "0,abc,2\n")
		var ids idset.IDSet
		if err := readSysfsIDSet(dir, "cpulist", ",", &ids); err == nil {
			t.Error("expected error for malformed content, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// readSysfsIntList
// --------------------------------------------------------------------------

func TestReadSysfsIntList(t *testing.T) {
	t.Run("reads_distance_list", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "distance", "10 20 30\n")
		var v []int
		if err := readSysfsIntList(dir, "distance", " ", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []int{10, 20, 30}
		if len(v) != len(want) {
			t.Fatalf("got %v, want %v", v, want)
		}
		for i, w := range want {
			if v[i] != w {
				t.Errorf("v[%d]=%d, want %d", i, v[i], w)
			}
		}
	})

	t.Run("overflow_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, dir, "val", "1 200 3\n")
		var v []int8
		if err := readSysfsIntList(dir, "val", " ", &v); err == nil {
			t.Error("expected overflow error for int8 element, got nil")
		}
	})

	t.Run("missing_file_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		var v []int
		if err := readSysfsIntList(dir, "nonexistent", " ", &v); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// readSysfsEPP
// --------------------------------------------------------------------------

func TestReadSysfsEPP(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    EPP
	}{
		{"performance\n", EPPPerformance},
		{"balance_performance\n", EPPBalancePerformance},
		{"balance_power\n", EPPBalancePower},
		{"power\n", EPPPower},
		{"unknown_value\n", EPPUnknown},
	} {
		tc := tc
		t.Run(tc.content[:len(tc.content)-1], func(t *testing.T) {
			dir := t.TempDir()
			writeSysfsFile(t, dir, "epp", tc.content)
			var v EPP
			if err := readSysfsEPP(dir, "epp", &v); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}

	t.Run("missing_file_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		var v EPP
		if err := readSysfsEPP(dir, "nonexistent", &v); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// writeSysfsRaw
// --------------------------------------------------------------------------

func TestWriteSysfsRaw(t *testing.T) {
	t.Run("writes_and_reads_back", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "val")
		if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := writeSysfsRaw(path, "new"); err != nil {
			t.Fatalf("writeSysfsRaw: %v", err)
		}
		got, err := readSysfsRaw(dir, "val")
		if err != nil {
			t.Fatalf("readSysfsRaw: %v", err)
		}
		if got != "new" {
			t.Errorf("got %q, want %q", got, "new")
		}
	})

	t.Run("missing_file_returns_error", func(t *testing.T) {
		if err := writeSysfsRaw("/nonexistent/path/val", "x"); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// writeSysfsInt
// --------------------------------------------------------------------------

func TestWriteSysfsInt(t *testing.T) {
	setup := func(t *testing.T, initial string) (base, entry string) {
		t.Helper()
		base = t.TempDir()
		entry = "val"
		if err := os.WriteFile(filepath.Join(base, entry), []byte(initial+"\n"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		return base, entry
	}

	t.Run("writes_value", func(t *testing.T) {
		base, entry := setup(t, "0")
		if err := writeSysfsInt(base, entry, int(99)); err != nil {
			t.Fatalf("writeSysfsInt: %v", err)
		}
		var got int
		if err := readSysfsInt(base, entry, &got); err != nil {
			t.Fatalf("readSysfsInt: %v", err)
		}
		if got != 99 {
			t.Errorf("got %d, want 99", got)
		}
	})

	t.Run("captures_old_value", func(t *testing.T) {
		base, entry := setup(t, "5")
		var old int
		if err := writeSysfsInt(base, entry, int(9), &old); err != nil {
			t.Fatalf("writeSysfsInt: %v", err)
		}
		if old != 5 {
			t.Errorf("old = %d, want 5", old)
		}
		var got int
		if err := readSysfsInt(base, entry, &got); err != nil {
			t.Fatalf("readSysfsInt: %v", err)
		}
		if got != 9 {
			t.Errorf("new = %d, want 9", got)
		}
	})

	t.Run("negative_value", func(t *testing.T) {
		base, entry := setup(t, "0")
		if err := writeSysfsInt(base, entry, int32(-7)); err != nil {
			t.Fatalf("writeSysfsInt: %v", err)
		}
		var got int32
		if err := readSysfsInt(base, entry, &got); err != nil {
			t.Fatalf("readSysfsInt: %v", err)
		}
		if got != -7 {
			t.Errorf("got %d, want -7", got)
		}
	})

	t.Run("missing_file_returns_error", func(t *testing.T) {
		if err := writeSysfsInt(t.TempDir(), "nonexistent", int(1)); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// writeSysfsUint
// --------------------------------------------------------------------------

func TestWriteSysfsUint(t *testing.T) {
	setup := func(t *testing.T, initial string) (base, entry string) {
		t.Helper()
		base = t.TempDir()
		entry = "freq"
		if err := os.WriteFile(filepath.Join(base, entry), []byte(initial+"\n"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		return base, entry
	}

	t.Run("writes_value", func(t *testing.T) {
		base, entry := setup(t, "0")
		if err := writeSysfsUint(base, entry, uint64(3200000)); err != nil {
			t.Fatalf("writeSysfsUint: %v", err)
		}
		var got uint64
		if err := readSysfsUint(base, entry, &got); err != nil {
			t.Fatalf("readSysfsUint: %v", err)
		}
		if got != 3200000 {
			t.Errorf("got %d, want 3200000", got)
		}
	})

	t.Run("captures_old_value", func(t *testing.T) {
		base, entry := setup(t, "1000000")
		var old uint64
		if err := writeSysfsUint(base, entry, uint64(2000000), &old); err != nil {
			t.Fatalf("writeSysfsUint: %v", err)
		}
		if old != 1000000 {
			t.Errorf("old = %d, want 1000000", old)
		}
		var got uint64
		if err := readSysfsUint(base, entry, &got); err != nil {
			t.Fatalf("readSysfsUint: %v", err)
		}
		if got != 2000000 {
			t.Errorf("new = %d, want 2000000", got)
		}
	})

	t.Run("missing_file_returns_error", func(t *testing.T) {
		if err := writeSysfsUint(t.TempDir(), "nonexistent", uint64(1)); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// strconv.IntSize consistency check
// --------------------------------------------------------------------------

// TestPlatformIntSize verifies that parseIntTo[int] accepts the platform's max int value.
func TestPlatformIntSize(t *testing.T) {
	// Use uint64 shift to avoid overflow in constant expressions, then cast.
	maxUint64 := uint64(1)<<(strconv.IntSize-1) - 1
	max := int64(maxUint64)
	v, err := parseIntTo[int](strconv.FormatInt(max, 10))
	if err != nil {
		t.Errorf("parseIntTo[int](%d) unexpected error: %v", max, err)
	}
	if int64(v) != max {
		t.Errorf("parseIntTo[int](%d) = %d", max, v)
	}
}
