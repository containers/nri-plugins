// Copyright 2020 Intel Corporation. All Rights Reserved.
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

package parse

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// unit multipliers
const (
	unitK = (int64(1) << 10)
	unitM = (int64(1) << 20)
	unitG = (int64(1) << 30)
	unitT = (int64(1) << 40)
)

// unit name to multiplier mapping
var units = map[string]int64{
	"k": unitK, "kB": unitK,
	"M": unitM, "MB": unitM,
	"G": unitG, "GB": unitG,
	"T": unitT, "TB": unitT,
}

// PickEntryFn picks a given input line apart into an entry of key and value.
type PickEntryFn func(string) (string, string, error)

// fileError prefixes an error with the file which produced it.
func fileError(path, format string, args ...any) error {
	return fmt.Errorf(path+": "+format, args...)
}

// splitNumericAndUnit splits a string into a numeric and a unit part.
func splitNumericAndUnit(path string, value string) (string, int64, error) {
	fields := strings.Fields(value)

	switch len(fields) {
	case 1:
		return fields[0], 1, nil
	case 2:
		num := fields[0]
		unit, ok := units[fields[1]]
		if !ok {
			return "", -1, fileError(path, "failed to parse '%s', invalid unit '%s'",
				value, fields[1])
		}
		return num, unit, nil
	}

	return "", -1, fileError(path, "invalid numeric value %s", value)
}

// parseNumeric parses a numeric string into an integer of the right size.
func parseNumeric(path, value string, ptr any) error {
	var numstr string
	var num, unit int64
	var f float64
	var err error

	if numstr, unit, err = splitNumericAndUnit(path, value); err != nil {
		return err
	}

	switch ptr := ptr.(type) {
	case *int:
		num, err = strconv.ParseInt(numstr, 0, strconv.IntSize)
		*ptr = int(num * unit)
	case *int8:
		num, err = strconv.ParseInt(numstr, 0, 8)
		*ptr = int8(num * unit)
	case *int16:
		num, err = strconv.ParseInt(numstr, 0, 16)
		*ptr = int16(num * unit)
	case *int32:
		num, err = strconv.ParseInt(numstr, 0, 32)
		*ptr = int32(num * unit)
	case *int64:
		num, err = strconv.ParseInt(numstr, 0, 64)
		*ptr = int64(num * unit)
	case *uint:
		num, err = strconv.ParseInt(numstr, 0, strconv.IntSize)
		*ptr = uint(num * unit)
	case *uint8:
		num, err = strconv.ParseInt(numstr, 0, 8)
		*ptr = uint8(num * unit)
	case *uint16:
		num, err = strconv.ParseInt(numstr, 0, 16)
		*ptr = uint16(num * unit)
	case *uint32:
		num, err = strconv.ParseInt(numstr, 0, 32)
		*ptr = uint32(num * unit)
	case *uint64:
		num, err = strconv.ParseInt(numstr, 0, 64)
		*ptr = uint64(num * unit)
	case *float32:
		f, err = strconv.ParseFloat(numstr, 32)
		*ptr = float32(f) * float32(unit)
	case *float64:
		f, err = strconv.ParseFloat(numstr, 64)
		*ptr = f * float64(unit)

	default:
		err = fileError(path, "can't parse numeric value '%s' into type %T", value, ptr)
	}

	return err
}

// FileEntries parses the given entries out of a file of key and value lines, as
// the files under /sys and /proc are. pickFn splits one line into its key and
// value; values maps a key to a pointer to store its value in, which says how
// the value is parsed. Parsing stops once every key has been seen.
func FileEntries(path string, values map[string]any, pickFn PickEntryFn) error {
	var err error

	data, err := os.ReadFile(path)
	if err != nil {
		return fileError(path, "failed to read file: %v", err)
	}

	left := len(values)
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, err := pickFn(line)
		if err != nil {
			return err
		}

		ptr, ok := values[key]
		if !ok {
			continue
		}

		switch ptr := ptr.(type) {
		case *int, *int8, *int32, *int16, *int64, *uint, *uint8, *uint16, *uint32, *uint64:
			if err = parseNumeric(path, value, ptr); err != nil {
				return err
			}
		case *float32, *float64:
			if err = parseNumeric(path, value, ptr); err != nil {
				return err
			}
		case *string:
			*ptr = value
		case *bool:
			*ptr, err = strconv.ParseBool(value)
			if err != nil {
				return fileError(path, "failed to parse line %s, value '%s' for boolean key '%s'",
					line, value, key)
			}
		default:
			return fileError(path, "don't know how to parse key '%s' of type %T", key, ptr)

		}

		left--
		if left == 0 {
			break
		}
	}

	return nil
}
