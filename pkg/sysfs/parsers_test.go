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

package sysfs_test

import (
	"os"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/pkg/sysfs"
)

// colonPickFn splits lines of the form "key: value" into key and value.
func colonPickFn(line string) (string, string, error) {
	parts := strings.SplitN(line, ": ", 2)
	if len(parts) != 2 {
		return "", "", nil
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "parsers_test_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close temp file: %v", err)
	}
	return f.Name()
}

func TestParseFileEntries_Int(t *testing.T) {
	path := writeTempFile(t, "count: 42\n")
	var count int
	if err := sysfs.ParseFileEntries(path, map[string]interface{}{"count": &count}, colonPickFn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 42 {
		t.Errorf("expected 42, got %d", count)
	}
}

func TestParseFileEntries_IntWithUnit(t *testing.T) {
	path := writeTempFile(t, "size: 4 kB\n")
	var size int64
	if err := sysfs.ParseFileEntries(path, map[string]interface{}{"size": &size}, colonPickFn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := int64(4 * 1024); size != want {
		t.Errorf("expected %d, got %d", want, size)
	}
}

func TestParseFileEntries_StringAndBool(t *testing.T) {
	path := writeTempFile(t, "name: hello\nenabled: true\n")
	var name string
	var enabled bool
	err := sysfs.ParseFileEntries(path, map[string]interface{}{
		"name":    &name,
		"enabled": &enabled,
	}, colonPickFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "hello" {
		t.Errorf("expected name %q, got %q", "hello", name)
	}
	if !enabled {
		t.Errorf("expected enabled=true, got false")
	}
}

func TestParseFileEntries_MissingFile(t *testing.T) {
	var count int
	err := sysfs.ParseFileEntries("/nonexistent/path/file", map[string]interface{}{"count": &count}, colonPickFn)
	if err == nil {
		t.Error("expected an error for missing file, got nil")
	}
}

// parseVal creates a temp file with "v: <content>" and parses field "v" into dest.
func parseVal(t *testing.T, fieldContent string, dest interface{}) error {
	t.Helper()
	path := writeTempFile(t, "v: "+fieldContent+"\n")
	return sysfs.ParseFileEntries(path, map[string]interface{}{"v": dest}, colonPickFn)
}

// TestParseNumericUnits_Positive tests every recognized unit string against every
// integer and float type that can hold the resulting value without overflow.
func TestParseNumericUnits_Positive(t *testing.T) {
	t.Run("int", func(t *testing.T) {
		// Limit to k/kB: int may be 32-bit on some platforms.
		for _, tc := range []struct {
			unit string
			want int
		}{
			{"k", 1 << 10}, {"kB", 1 << 10},
		} {
			t.Run(tc.unit, func(t *testing.T) {
				var v int
				if err := parseVal(t, "1 "+tc.unit, &v); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if v != tc.want {
					t.Errorf("got %d, want %d", v, tc.want)
				}
			})
		}
	})

	t.Run("int8_plain", func(t *testing.T) {
		var v int8
		if err := parseVal(t, "100", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 100 {
			t.Errorf("got %d, want 100", v)
		}
	})

	t.Run("int16", func(t *testing.T) {
		for _, tc := range []struct {
			unit string
			want int16
		}{
			{"k", 1 << 10}, {"kB", 1 << 10},
		} {
			t.Run(tc.unit, func(t *testing.T) {
				var v int16
				if err := parseVal(t, "1 "+tc.unit, &v); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if v != tc.want {
					t.Errorf("got %d, want %d", v, tc.want)
				}
			})
		}
	})

	t.Run("int32", func(t *testing.T) {
		// T = 2^40 overflows int32; stop at G.
		for _, tc := range []struct {
			unit string
			want int32
		}{
			{"k", 1 << 10}, {"kB", 1 << 10},
			{"M", 1 << 20}, {"MB", 1 << 20},
			{"G", 1 << 30}, {"GB", 1 << 30},
		} {
			t.Run(tc.unit, func(t *testing.T) {
				var v int32
				if err := parseVal(t, "1 "+tc.unit, &v); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if v != tc.want {
					t.Errorf("got %d, want %d", v, tc.want)
				}
			})
		}
	})

	t.Run("int64", func(t *testing.T) {
		for _, tc := range []struct {
			unit string
			want int64
		}{
			{"k", 1 << 10}, {"kB", 1 << 10},
			{"M", 1 << 20}, {"MB", 1 << 20},
			{"G", 1 << 30}, {"GB", 1 << 30},
			{"T", 1 << 40}, {"TB", 1 << 40},
		} {
			t.Run(tc.unit, func(t *testing.T) {
				var v int64
				if err := parseVal(t, "1 "+tc.unit, &v); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if v != tc.want {
					t.Errorf("got %d, want %d", v, tc.want)
				}
			})
		}
	})

	t.Run("uint", func(t *testing.T) {
		// Limit to k/kB: uint may be 32-bit on some platforms.
		for _, tc := range []struct {
			unit string
			want uint
		}{
			{"k", 1 << 10}, {"kB", 1 << 10},
		} {
			t.Run(tc.unit, func(t *testing.T) {
				var v uint
				if err := parseVal(t, "1 "+tc.unit, &v); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if v != tc.want {
					t.Errorf("got %d, want %d", v, tc.want)
				}
			})
		}
	})

	t.Run("uint8_plain", func(t *testing.T) {
		var v uint8
		if err := parseVal(t, "100", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 100 {
			t.Errorf("got %d, want 100", v)
		}
	})

	t.Run("uint16", func(t *testing.T) {
		for _, tc := range []struct {
			unit string
			want uint16
		}{
			{"k", 1 << 10}, {"kB", 1 << 10},
		} {
			t.Run(tc.unit, func(t *testing.T) {
				var v uint16
				if err := parseVal(t, "1 "+tc.unit, &v); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if v != tc.want {
					t.Errorf("got %d, want %d", v, tc.want)
				}
			})
		}
	})

	t.Run("uint32", func(t *testing.T) {
		// T = 2^40 overflows uint32; stop at G.
		for _, tc := range []struct {
			unit string
			want uint32
		}{
			{"k", 1 << 10}, {"kB", 1 << 10},
			{"M", 1 << 20}, {"MB", 1 << 20},
			{"G", 1 << 30}, {"GB", 1 << 30},
		} {
			t.Run(tc.unit, func(t *testing.T) {
				var v uint32
				if err := parseVal(t, "1 "+tc.unit, &v); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if v != tc.want {
					t.Errorf("got %d, want %d", v, tc.want)
				}
			})
		}
	})

	t.Run("uint64", func(t *testing.T) {
		for _, tc := range []struct {
			unit string
			want uint64
		}{
			{"k", 1 << 10}, {"kB", 1 << 10},
			{"M", 1 << 20}, {"MB", 1 << 20},
			{"G", 1 << 30}, {"GB", 1 << 30},
			{"T", 1 << 40}, {"TB", 1 << 40},
		} {
			t.Run(tc.unit, func(t *testing.T) {
				var v uint64
				if err := parseVal(t, "1 "+tc.unit, &v); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if v != tc.want {
					t.Errorf("got %d, want %d", v, tc.want)
				}
			})
		}
	})

	// All powers of 2 used here are exactly representable in both float32 and float64.
	allFloatUnits := []struct {
		unit string
		want float64
	}{
		{"k", 1 << 10}, {"kB", 1 << 10},
		{"M", 1 << 20}, {"MB", 1 << 20},
		{"G", 1 << 30}, {"GB", 1 << 30},
		{"T", 1 << 40}, {"TB", 1 << 40},
	}

	t.Run("float32", func(t *testing.T) {
		for _, tc := range allFloatUnits {
			t.Run(tc.unit, func(t *testing.T) {
				var v float32
				if err := parseVal(t, "1 "+tc.unit, &v); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if want := float32(tc.want); v != want {
					t.Errorf("got %g, want %g", v, want)
				}
			})
		}
	})

	t.Run("float64", func(t *testing.T) {
		for _, tc := range allFloatUnits {
			t.Run(tc.unit, func(t *testing.T) {
				var v float64
				if err := parseVal(t, "1 "+tc.unit, &v); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if v != tc.want {
					t.Errorf("got %g, want %g", v, tc.want)
				}
			})
		}
	})
}

// TestParseNumericUnits_Negative tests inputs that must produce an error.
func TestParseNumericUnits_Negative(t *testing.T) {
	// Unrecognized unit strings (the map keys are case-sensitive).
	t.Run("invalid_units", func(t *testing.T) {
		for _, unit := range []string{"KB", "kb", "Kb", "mb", "gb", "tb", "xyz", "MiB", "GiB"} {
			t.Run(unit, func(t *testing.T) {
				var v int64
				if err := parseVal(t, "1 "+unit, &v); err == nil {
					t.Errorf("expected error for unit %q, got nil", unit)
				}
			})
		}
	})

	// Values whose base number exceeds the range that strconv.ParseInt accepts
	// for the given bitSize, surfacing a parse error before any multiplication.
	t.Run("range_overflow", func(t *testing.T) {
		// int8 / uint8: ParseInt bitSize=8 accepts only –128..127.
		t.Run("int8", func(t *testing.T) {
			var v int8
			if err := parseVal(t, "200", &v); err == nil {
				t.Error("expected error for int8 base value 200 (> 127), got nil")
			}
		})
		t.Run("uint8", func(t *testing.T) {
			var v uint8
			if err := parseVal(t, "200", &v); err == nil {
				t.Error("expected error for uint8 base value 200 (> 127 signed), got nil")
			}
		})
		// int16 / uint16: ParseInt bitSize=16 accepts only –32768..32767.
		t.Run("int16", func(t *testing.T) {
			var v int16
			if err := parseVal(t, "40000", &v); err == nil {
				t.Error("expected error for int16 base value 40000 (> 32767), got nil")
			}
		})
		t.Run("uint16", func(t *testing.T) {
			var v uint16
			if err := parseVal(t, "40000", &v); err == nil {
				t.Error("expected error for uint16 base value 40000 (> 32767 signed), got nil")
			}
		})
	})

	t.Run("malformed_input", func(t *testing.T) {
		// Non-numeric base value.
		t.Run("non_numeric_base", func(t *testing.T) {
			var v int64
			if err := parseVal(t, "abc", &v); err == nil {
				t.Error("expected error for non-numeric value, got nil")
			}
		})
		// Non-numeric base with a valid unit.
		t.Run("non_numeric_base_with_unit", func(t *testing.T) {
			var v int64
			if err := parseVal(t, "abc k", &v); err == nil {
				t.Error("expected error for non-numeric base with unit, got nil")
			}
		})
		// Three whitespace-separated tokens are rejected by splitNumericAndUnit.
		t.Run("three_tokens", func(t *testing.T) {
			var v int64
			if err := parseVal(t, "1 k extra", &v); err == nil {
				t.Error("expected error for three-token input, got nil")
			}
		})
	})
}
