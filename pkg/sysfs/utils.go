// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package sysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// Get the trailing enumeration part of a name.
func getEnumeratedID(name string) idset.ID {
	id := 0
	base := 1
	for idx := len(name) - 1; idx > 0; idx-- {
		d := name[idx]

		if '0' <= d && d <= '9' {
			id += base * (int(d) - '0')
			base *= 10
		} else {
			if base > 1 {
				return idset.ID(id)
			}

			return idset.ID(-1)
		}
	}

	return idset.ID(-1)
}

// Read content of a sysfs entry and convert it according to the type of a given pointer.
func readSysfsEntry(base, entry string, ptr interface{}, args ...interface{}) (string, error) {
	var buf string

	path := filepath.Join(base, entry)

	blob, err := os.ReadFile(path)
	if err != nil {
		return "", sysfsError(path, "failed to read sysfs entry: %w", err)
	}
	buf = strings.Trim(string(blob), "\n")

	if ptr == interface{}(nil) {
		return buf, nil
	}

	switch ptr := ptr.(type) {
	case *string, *int, *uint, *int8, *uint8, *int16, *uint16, *int32, *uint32, *int64, *uint64:
		err := parseValue(buf, ptr)
		if err != nil {
			return "", sysfsError(path, "%w", err)
		}
		return buf, nil

	case *idset.IDSet, *[]int, *[]uint, *[]int8, *[]uint8, *[]int16, *[]uint16, *[]int32, *[]uint32, *[]int64, *[]uint64:
		sep, err := getSeparator(" ", args)
		if err != nil {
			return "", sysfsError(path, "%w", err)
		}
		err = parseValueList(buf, sep, ptr)
		if err != nil {
			return "", sysfsError(path, "%w", err)
		}
		return buf, nil
	case *EPP:
		*ptr = EPPFromString(buf)
		return buf, nil
	}

	return "", sysfsError(path, "unsupported sysfs entry type %T", ptr)
}

// Write a value to a sysfs entry. An optional item separator can be specified for slice values.
func writeSysfsEntry(base, entry string, val, oldp interface{}, args ...interface{}) (string, error) {
	var buf, old string
	var err error

	if oldp != nil {
		if old, err = readSysfsEntry(base, entry, oldp, args...); err != nil {
			return "", err
		}
	}

	path := filepath.Join(base, entry)

	switch val.(type) {
	case string, int, uint, int8, uint8, int16, uint16, int32, uint32, int64, uint64:
		buf, err = formatValue(val)
		if err != nil {
			return "", sysfsError(path, "%w", err)
		}

	case idset.IDSet, []int, []uint, []int8, []uint8, []int16, []uint16, []int32, []uint32, []int64, []uint64:
		sep, err := getSeparator(" ", args)
		if err != nil {
			return "", sysfsError(path, "%w", err)
		}
		buf, err = formatValueList(sep, val)
		if err != nil {
			return "", sysfsError(path, "%w", err)
		}

	default:
		return "", sysfsError(path, "unsupported sysfs entry type %T", val)
	}

	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return "", sysfsError(path, "cannot open: %w", err)
	}
	defer f.Close()

	if _, err = f.Write([]byte(buf + "\n")); err != nil {
		return "", sysfsError(path, "cannot write: %w", err)
	}

	return old, nil
}

// Determine list separator string, given an optional separator variadic argument.
func getSeparator(defaultVal string, args []interface{}) (string, error) {
	switch len(args) {
	case 0:
		return defaultVal, nil
	case 1:
		return args[0].(string), nil
	}

	return "", fmt.Errorf("invalid separator (%v), 1 expected, %d given", args, len(args))
}

// Parse a value from a string.
func parseValue(str string, value interface{}) error {
	switch value := value.(type) {
	case *string:
		*value = str

	case *int, *int8, *int16, *int32, *int64:
		v, err := strconv.ParseInt(str, 0, 0)
		if err != nil {
			return fmt.Errorf("invalid entry '%s': %w", str, err)
		}

		switch value := value.(type) {
		case *int:
			*value = int(v)
		case *int8:
			*value = int8(v)
		case *int16:
			*value = int16(v)
		case *int32:
			*value = int32(v)
		case *int64:
			*value = v
		}

	case *uint, *uint8, *uint16, *uint32, *uint64:
		v, err := strconv.ParseUint(str, 0, 0)
		if err != nil {
			return fmt.Errorf("invalid entry: '%s': %w", str, err)
		}

		switch value := value.(type) {
		case *uint:
			*value = uint(v)
		case *uint8:
			*value = uint8(v)
		case *uint16:
			*value = uint16(v)
		case *uint32:
			*value = uint32(v)
		case *uint64:
			*value = v
		}
	}

	return nil
}

// Parse a list of values from a string into a slice.
func parseValueList(str, sep string, valuep interface{}) error {
	var value interface{}

	switch valuep.(type) {
	case *idset.IDSet:
		value = idset.NewIDSet()
	case *[]int:
		value = []int{}
	case *[]uint:
		value = []uint{}
	case *[]int8:
		value = []int8{}
	case *[]uint8:
		value = []uint8{}
	case *[]int16:
		value = []int16{}
	case *[]uint16:
		value = []uint16{}
	case *[]int32:
		value = []int32{}
	case *[]uint32:
		value = []uint32{}
	case *[]int64:
		value = []int64{}
	case *[]uint64:
		value = []uint64{}
	default:
		return fmt.Errorf("invalid slice value type: %T", valuep)
	}

	for _, s := range strings.Split(str, sep) {
		if s == "" {
			break
		}
		switch val := value.(type) {
		case idset.IDSet:
			if rng := strings.Split(s, "-"); len(rng) == 1 {
				id, err := strconv.Atoi(s)
				if err != nil {
					return fmt.Errorf("invalid entry '%s': %w", s, err)
				}
				val.Add(idset.ID(id))
			} else {
				beg, err := strconv.Atoi(rng[0])
				if err != nil {
					return fmt.Errorf("invalid entry '%s': %w", s, err)
				}
				end, err := strconv.Atoi(rng[1])
				if err != nil {
					return fmt.Errorf("invalid entry '%s': %w", s, err)
				}
				for id := beg; id <= end; id++ {
					val.Add(idset.ID(id))
				}
			}
			value = val

		case []int, []int8, []int16, []int32, []int64:
			v, err := strconv.ParseInt(s, 0, 0)
			if err != nil {
				return fmt.Errorf("invalid entry '%s': %w", s, err)
			}
			switch val := value.(type) {
			case []int:
				value = append(val, int(v))
			case []int8:
				value = append(val, int8(v))
			case []int16:
				value = append(val, int16(v))
			case []int32:
				value = append(val, int32(v))
			case []int64:
				value = append(val, v)
			}

		case []uint, []uint8, []uint16, []uint32, []uint64:
			v, err := strconv.ParseUint(s, 0, 0)
			if err != nil {
				return fmt.Errorf("invalid entry '%s': %w", s, err)
			}
			switch val := value.(type) {
			case []uint:
				value = append(val, uint(v))
			case []uint8:
				value = append(val, uint8(v))
			case []uint16:
				value = append(val, uint16(v))
			case []uint32:
				value = append(val, uint32(v))
			case []uint64:
				value = append(val, v)
			}
		}
	}

	switch valuep := valuep.(type) {
	case *idset.IDSet:
		*valuep = value.(idset.IDSet)
	case *[]int:
		*valuep = value.([]int)
	case *[]uint:
		*valuep = value.([]uint)
	case *[]int8:
		*valuep = value.([]int8)
	case *[]uint8:
		*valuep = value.([]uint8)
	case *[]int16:
		*valuep = value.([]int16)
	case *[]uint16:
		*valuep = value.([]uint16)
	case *[]int32:
		*valuep = value.([]int32)
	case *[]uint32:
		*valuep = value.([]uint32)
	case *[]int64:
		*valuep = value.([]int64)
	case *[]uint64:
		*valuep = value.([]uint64)
	}

	return nil
}

// Format a value into a string.
func formatValue(value interface{}) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case int, uint, int8, uint8, int16, uint16, int32, uint32, int64, uint64:
		return fmt.Sprintf("%d", value), nil
	default:
		return "", fmt.Errorf("invalid value type %T", value)
	}
}

// Format a list of values from a slice into a string.
func formatValueList(sep string, value interface{}) (string, error) {
	var v []interface{}

	switch value := value.(type) {
	case idset.IDSet:
		return value.StringWithSeparator(sep), nil
	case []int, []uint, []int8, []uint8, []int16, []uint16, []int32, []uint32, []int64, []uint64:
		v = value.([]interface{})
	default:
		return "", fmt.Errorf("invalid value type %T", value)
	}

	str := ""
	t := ""
	for idx := range v {
		str = str + t + fmt.Sprintf("%d", v[idx])
		t = sep
	}

	return "", nil
}

// IDSetFromCPUSet returns an id set corresponding to a cpuset.CPUSet.
func IDSetFromCPUSet(cset cpuset.CPUSet) idset.IDSet {
	return idset.NewIDSetFromIntSlice(cset.List()...)
}

// CPUSetFromIDSet returns a cpuset.CPUSet corresponding to an id set.
func CPUSetFromIDSet(s idset.IDSet) cpuset.CPUSet {
	return cpuset.New(s.Members()...)
}

// GetMemoryCapacity parses memory capacity from /proc/meminfo (mimicking cAdvisor).
func GetMemoryCapacity() int64 {
	var (
		data []byte
		err  error
		capa int64
	)

	if data, err = os.ReadFile("/proc/meminfo"); err != nil {
		return -1
	}

	for _, line := range strings.Split(string(data), "\n") {
		keyval := strings.Split(line, ":")
		if len(keyval) != 2 || keyval[0] != "MemTotal" {
			continue
		}

		valunit := strings.Split(strings.TrimSpace(keyval[1]), " ")
		if len(valunit) != 2 || valunit[1] != "kB" {
			return -1
		}

		capa, err = strconv.ParseInt(valunit[0], 10, 64)
		if err != nil {
			return -1
		}

		capa *= 1024
		break
	}

	return capa
}
