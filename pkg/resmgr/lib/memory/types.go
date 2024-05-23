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

package libmem

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/containers/nri-plugins/pkg/sysfs"
)

// Type represents known types of memory.
type Type int

const (
	TypeDRAM Type = iota // ordinary DRAM
	TypePMEM             // 'persistent' memory, typically slower but higher capacity than DRAM
	TypeHBM              // high-bandwidth memory
)

var (
	sysToType = map[sysfs.MemoryType]Type{
		sysfs.MemoryTypeDRAM: TypeDRAM,
		sysfs.MemoryTypePMEM: TypePMEM,
		sysfs.MemoryTypeHBM:  TypeHBM,
	}
	typeToSys = map[Type]sysfs.MemoryType{
		TypeDRAM: sysfs.MemoryTypeDRAM,
		TypePMEM: sysfs.MemoryTypePMEM,
		TypeHBM:  sysfs.MemoryTypeHBM,
	}
	typeToString = map[Type]string{
		TypeDRAM: "DRAM",
		TypePMEM: "PMEM",
		TypeHBM:  "HBM",
	}
	stringToType = map[string]Type{
		"DRAM": TypeDRAM,
		"PMEM": TypePMEM,
		"HBM":  TypeHBM,
	}
)

// TypeForSysfs returns the memory type for the given sysfs memory type.
func TypeForSysfs(sysType sysfs.MemoryType) Type {
	if t, ok := sysToType[sysType]; ok {
		return t
	}

	panic(fmt.Errorf("unknown sysfs memory type %v", sysType))
}

// Sysfs returns the sysfs memory type for the given memory type.
func (t Type) Sysfs() sysfs.MemoryType {
	if sysType, ok := typeToSys[t]; ok {
		return sysType
	}

	panic(fmt.Errorf("unknown libmem memory type %d", t))
}

// ParseType parses the given string into a memory type.
func ParseType(str string) (Type, error) {
	if t, ok := stringToType[strings.ToUpper(str)]; ok {
		return t, nil
	}

	return 0, fmt.Errorf("%w: %q", ErrInvalidType, str)
}

// MustParseType parses the given string into a memory type.
// It panicks on failure.
func MustParseType(str string) Type {
	t, err := ParseType(str)
	if err == nil {
		return t
	}

	panic(err)
}

// Mask returns the memory TypeMask for the memory type.
func (t Type) Mask() TypeMask {
	return TypeMask(1 << t)
}

// IsValid returns true if the memory type is valid/known.
func (t Type) IsValid() bool {
	_, ok := typeToSys[t]
	return ok
}

// String returns a string representation of the memory type.
func (t Type) String() string {
	if str, ok := typeToString[t]; ok {
		return str
	}

	return fmt.Sprintf("%%!(libmem:Bad-Type %d)", t)
}

// MarshalJSON is the json.Marshaller for Type.
func (t Type) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.String())
}

// UnmarshalJSON is the json.Unmarshaller for Type.
func (t *Type) UnmarshalJSON(data []byte) error {
	i := 0
	if err := json.Unmarshal(data, &i); err == nil {
		if _, ok := typeToString[Type(i)]; ok {
			*t = Type(i)
			return nil
		}
		return fmt.Errorf("%w: %d", ErrInvalidType, i)
	}

	str := ""
	err := json.Unmarshal(data, &str)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidType, err)
	}

	stt, ok := stringToType[strings.ToUpper(str)]
	if !ok {
		return fmt.Errorf("%w: %s", ErrInvalidType, str)
	}

	*t = stt
	return nil
}

// TypeMask represents a set of memory types as a bit mask.
type TypeMask int

const (
	TypeMaskDRAM TypeMask = 1 << TypeDRAM          // ordinary DRAM
	TypeMaskPMEM TypeMask = 1 << TypePMEM          // 'persistent' memory
	TypeMaskHBM  TypeMask = 1 << TypeHBM           // high-bandwidth memory
	TypeMaskAll  TypeMask = (TypeMaskHBM << 1) - 1 // all types of memory
)

var (
	typeMaskToString map[TypeMask]string
)

// NewTypeMask returns a TypeMask containing the given memory types.
func NewTypeMask(types ...Type) TypeMask {
	m := TypeMask(0)
	for _, t := range types {
		m |= (1 << t)
	}
	return m & TypeMaskAll
}

// NewTypeMaskForSysfs returns a TypeMask containing th given sysfs memory types.
func NewTypeMaskForSysfs(sysTypes ...sysfs.MemoryType) TypeMask {
	m := TypeMask(0)
	for _, st := range sysTypes {
		m |= (1 << TypeForSysfs(st))
	}
	return m & TypeMaskAll
}

// ParseTypeMask parses the given string into a TypeMask.
func ParseTypeMask(str string) (TypeMask, error) {
	m := TypeMask(0)
	for _, s := range strings.Split(str, ",") {
		t, err := ParseType(strings.TrimSpace(s))
		if err != nil {
			return 0, fmt.Errorf("%w: %q", ErrInvalidType, str)
		}
		m |= (1 << t)
	}
	return m, nil
}

// MustParseTypeMask parses the given string into a TypeMask.
// It panicks on failure.
func MustParseTypeMask(str string) TypeMask {
	m, err := ParseTypeMask(str)
	if err == nil {
		return m
	}

	panic(err)
}

// Slice returns the memory types present in the TypeMask.
func (m TypeMask) Slice() []Type {
	var types []Type
	for _, t := range []Type{TypeDRAM, TypePMEM, TypeHBM} {
		if (m & (1 << t)) != 0 {
			types = append(types, t)
		}
	}
	return types
}

// Set returns a TypeMask with the original and the given types added.
func (m TypeMask) Set(types ...Type) TypeMask {
	for _, t := range types {
		m |= (1 << t)
	}
	return m
}

// Clear returns a TypeMask with the given types removed.
func (m TypeMask) Clear(types ...Type) TypeMask {
	for _, t := range types {
		m &^= (1 << t)
	}
	return m
}

// Contains returns true if all the given types are present in the TypeMask.
func (m TypeMask) Contains(types ...Type) bool {
	for _, t := range types {
		if (m & (1 << t)) == 0 {
			return false
		}
	}
	return true
}

// ContainsAny returns true if any of the given types are present in the TypeMask.
func (m TypeMask) ContainsAny(types ...Type) bool {
	for _, t := range types {
		if (m & (1 << t)) != 0 {
			return true
		}
	}
	return false
}

// And returns a TypeMask with all IDs which are present in both masks.
func (m TypeMask) And(o TypeMask) TypeMask {
	return m & o
}

// And returns a TypeMask with all IDs which are present at least in one of the masks.
func (m TypeMask) Or(o TypeMask) TypeMask {
	return m | o
}

// And returns a TypeMask with all IDs which are present in m but not in o.
func (m TypeMask) AndNot(o TypeMask) TypeMask {
	return m &^ o
}

// String returns a string representation of the TypeMask.
func (m TypeMask) String() string {
	str := strings.Builder{}
	sep := ""
	for _, t := range []Type{TypeDRAM, TypePMEM, TypeHBM} {
		if (m & (1 << t)) != 0 {
			str.WriteString(sep)
			str.WriteString(t.String())
			sep = ","
		}
	}
	return str.String()
}

// Foreach calls the given function for each type present in the TypeMask
// until the function returns false, or ForeachDone. Iteration continues
// if the returned value is true, or ForeachMore.
func (m TypeMask) Foreach(fn func(Type) bool) {
	for _, t := range m.Slice() {
		if !fn(t) {
			return
		}
	}
}

// MarshalJSON is the json.Marshaller for TypeMask.
func (m TypeMask) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// UnmarshalJSON is the json.Unmarshaller for TypeMask.
func (m *TypeMask) UnmarshalJSON(data []byte) error {
	i := 0
	if err := json.Unmarshal(data, &i); err == nil {
		if unknown := (TypeMask(i) &^ TypeMaskAll); unknown != 0 {
			return fmt.Errorf("%w: unknown type bits 0x%x", ErrInvalidType, unknown)
		}
		*m = TypeMask(i)
		return nil
	}

	str := ""
	err := json.Unmarshal(data, &str)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidType, err)
	}

	parsed, err := ParseTypeMask(str)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidType, err)
	}

	*m = parsed
	return nil
}
