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

// mempolicy package provides low-level functions to set and get
// default memory policyfor a process using the Linux kernel's
// set_mempolicy and get_mempolicy syscalls.
package mempolicy

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	MPOL_DEFAULT = iota
	MPOL_PREFERRED
	MPOL_BIND
	MPOL_INTERLEAVE
	MPOL_LOCAL
	MPOL_PREFERRED_MANY
	MPOL_WEIGHTED_INTERLEAVE

	MPOL_F_STATIC_NODES   uint = (1 << 15)
	MPOL_F_RELATIVE_NODES uint = (1 << 14)
	MPOL_F_NUMA_BALANCING uint = (1 << 13)

	SYS_SET_MEMPOLICY = 238
	SYS_GET_MEMPOLICY = 239

	MAX_NUMA_NODES = 1024
)

var Modes = map[string]uint{
	"MPOL_DEFAULT":             MPOL_DEFAULT,
	"MPOL_PREFERRED":           MPOL_PREFERRED,
	"MPOL_BIND":                MPOL_BIND,
	"MPOL_INTERLEAVE":          MPOL_INTERLEAVE,
	"MPOL_LOCAL":               MPOL_LOCAL,
	"MPOL_PREFERRED_MANY":      MPOL_PREFERRED_MANY,
	"MPOL_WEIGHTED_INTERLEAVE": MPOL_WEIGHTED_INTERLEAVE,
}

var Flags = map[string]uint{
	"MPOL_F_STATIC_NODES":   MPOL_F_STATIC_NODES,
	"MPOL_F_RELATIVE_NODES": MPOL_F_RELATIVE_NODES,
	"MPOL_F_NUMA_BALANCING": MPOL_F_NUMA_BALANCING,
}

var ModeNames map[uint]string

var FlagNames map[uint]string

func nodesToMask(nodes []int) ([]uint64, error) {
	maxNode := 0
	for _, node := range nodes {
		if node > maxNode {
			maxNode = node
		}
		if node < 0 {
			return nil, fmt.Errorf("node %d out of range", node)
		}
	}
	if maxNode >= MAX_NUMA_NODES {
		return nil, fmt.Errorf("node %d out of range", maxNode)
	}
	mask := make([]uint64, (maxNode/64)+1)
	for _, node := range nodes {
		mask[node/64] |= (1 << (node % 64))
	}
	return mask, nil
}

// SetMempolicy calls set_mempolicy syscall
func SetMempolicy(mpol uint, nodes []int) error {
	nodeMask, err := nodesToMask(nodes)
	if err != nil {
		return err
	}
	nodeMaskPtr := unsafe.Pointer(&nodeMask[0])
	_, _, errno := syscall.Syscall(SYS_SET_MEMPOLICY, uintptr(mpol), uintptr(nodeMaskPtr), uintptr(len(nodeMask)*64))
	if errno != 0 {
		return syscall.Errno(errno)
	}
	return nil
}

// GetMempolicy calls get_mempolicy syscall
func GetMempolicy() (uint, []int, error) {
	var mpol uint
	maxNode := uint64(MAX_NUMA_NODES)
	nodeMask := make([]uint64, maxNode/64)
	nodeMaskPtr := unsafe.Pointer(&nodeMask[0])
	_, _, errno := syscall.Syscall(SYS_GET_MEMPOLICY, uintptr(unsafe.Pointer(&mpol)), uintptr(nodeMaskPtr), uintptr(maxNode))
	if errno != 0 {
		return 0, []int{}, syscall.Errno(errno)
	}

	nodes := make([]int, 0)
	for i := range int(maxNode) {
		if (nodeMask[i/64] & (1 << (i % 64))) != 0 {
			nodes = append(nodes, i)
		}
	}
	return mpol, nodes, nil
}

func init() {
	ModeNames = make(map[uint]string)
	for k, v := range Modes {
		ModeNames[v] = k
	}
	FlagNames = make(map[uint]string)
	for k, v := range Flags {
		FlagNames[v] = k
	}
}
