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

package libmem_test

import (
	"github.com/containers/nri-plugins/pkg/lib/hardware"
	. "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestTypes(t *testing.T) {
	type testCase struct {
		name    string
		kind    hardware.MemoryKind
		memType Type
	}

	for _, tc := range []*testCase{
		{
			name:    "DRAM",
			kind:    hardware.MemoryKindDRAM,
			memType: TypeDRAM,
		},
		{
			name:    "PMEM",
			kind:    hardware.MemoryKindPMEM,
			memType: TypePMEM,
		},
		{
			name:    "HBM",
			kind:    hardware.MemoryKindHBM,
			memType: TypeHBM,
		},
	} {
		t.Run(tc.name+" TypeForKind", func(t *testing.T) {
			require.Equal(t, tc.memType, TypeForKind(tc.kind))
		})
		t.Run(tc.name+" Kind", func(t *testing.T) {
			require.Equal(t, tc.kind, tc.memType.Kind())
		})
		t.Run(tc.name+" MustParseType", func(t *testing.T) {
			require.Equal(t, tc.memType, MustParseType(tc.name))
		})
	}
}

func TestTypeMasks(t *testing.T) {
	type testCase struct {
		name  string
		types []Type
		mask  TypeMask
	}

	for _, tc := range []*testCase{
		{
			name:  "DRAM",
			types: []Type{TypeDRAM},
			mask:  TypeMaskDRAM,
		},
		{
			name:  "PMEM",
			types: []Type{TypePMEM},
			mask:  TypeMaskPMEM,
		},
		{
			name:  "HBM",
			types: []Type{TypeHBM},
			mask:  TypeMaskHBM,
		},
		{
			name:  "DRAM,PMEM",
			types: []Type{TypeDRAM, TypePMEM},
			mask:  TypeMaskDRAM | TypeMaskPMEM,
		},
		{
			name:  "DRAM,HBM",
			types: []Type{TypeDRAM, TypeHBM},
			mask:  TypeMaskDRAM | TypeMaskHBM,
		},
		{
			name:  "PMEM,HBM",
			types: []Type{TypePMEM, TypeHBM},
			mask:  TypeMaskPMEM | TypeMaskHBM,
		},
		{
			name:  "DRAM,PMEM,HBM",
			types: []Type{TypeDRAM, TypePMEM, TypeHBM},
			mask:  TypeMaskDRAM | TypeMaskPMEM | TypeMaskHBM,
		},
	} {
		t.Run(tc.name+" NewTypeMask", func(t *testing.T) {
			require.Equal(t, tc.mask, NewTypeMask(tc.types...))
		})
		t.Run(tc.name+" MustParseTypeMask", func(t *testing.T) {
			require.Equal(t, tc.mask, MustParseTypeMask(tc.name))
		})
		t.Run(tc.name+" Slice", func(t *testing.T) {
			require.Equal(t, tc.types, tc.mask.Slice())
		})
		t.Run(tc.name+" Contains", func(t *testing.T) {
			require.True(t, tc.mask.Contains(tc.types...))
		})
		t.Run(tc.name+" ContainsAny", func(t *testing.T) {
			require.True(t, tc.mask.Contains(tc.types...))
		})
		t.Run(tc.name+" !ContainsAny", func(t *testing.T) {
			if others := TypeMaskAll &^ tc.mask; others != TypeMask(0) {
				require.True(t, !tc.mask.ContainsAny(others.Slice()...))
			}
		})
	}
}
