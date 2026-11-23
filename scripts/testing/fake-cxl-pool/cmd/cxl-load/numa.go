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
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// errNotPermitted is the error of a NUMA system call that the host does
// not permit or have, for instance a seccomp filter of a container or a
// kernel without NUMA support.
var errNotPermitted = errors.New("not permitted here")

// cpuSetBits is the number of CPUs that a unix.CPUSet can hold.
const cpuSetBits = int(unsafe.Sizeof(unix.CPUSet{})) * 8

// nodeMask returns the bit mask of the nodes for mbind(2): bit N of the
// array of 64-bit words is set for node N.
func nodeMask(nodes []int) []uint64 {
	highest := 0
	for _, n := range nodes {
		highest = max(highest, n)
	}
	mask := make([]uint64, highest/64+1)
	for _, n := range nodes {
		mask[n/64] |= 1 << (n % 64)
	}
	return mask
}

// bindMemory sets the memory policy of the mapping buf to MPOL_BIND on the
// given nodes: the pages of buf that are touched after it are taken only
// from the memory of those nodes.
func bindMemory(buf []byte, nodes []int) error {
	mask := nodeMask(nodes)
	// The kernel counts maxnode - 1 bits of the mask.
	maxnode := len(mask)*64 + 1
	_, _, errno := unix.Syscall6(unix.SYS_MBIND, uintptr(unsafe.Pointer(unsafe.SliceData(buf))), uintptr(len(buf)),
		unix.MPOL_BIND, uintptr(unsafe.Pointer(&mask[0])), uintptr(maxnode), 0)
	switch {
	case errno == 0:
		return nil
	case errors.Is(errno, unix.EPERM) || errors.Is(errno, unix.ENOSYS):
		return fmt.Errorf("mbind to nodes %s: %w: %v", formatList(nodes), errNotPermitted, errno)
	case errors.Is(errno, unix.EINVAL):
		return fmt.Errorf("mbind to nodes %s: %w (is every node online?)", formatList(nodes), errno)
	}
	return fmt.Errorf("mbind to nodes %s: %w", formatList(nodes), errno)
}

// pageNodes returns the node of the page at each address, or a negative
// errno for a page that has none (-ENOENT: it is not in memory).
func pageNodes(addrs []uintptr) ([]int32, error) {
	status := make([]int32, len(addrs))
	if len(addrs) == 0 {
		return status, nil
	}
	_, _, errno := unix.Syscall6(unix.SYS_MOVE_PAGES, 0, uintptr(len(addrs)),
		uintptr(unsafe.Pointer(&addrs[0])), 0, uintptr(unsafe.Pointer(&status[0])), 0)
	switch {
	case errno == 0:
		return status, nil
	case errors.Is(errno, unix.EPERM) || errors.Is(errno, unix.ENOSYS):
		return nil, fmt.Errorf("move_pages status query: %w: %v", errNotPermitted, errno)
	}
	return nil, fmt.Errorf("move_pages status query: %w", errno)
}

// pinThread pins the calling thread to the CPU cpu.
func pinThread(cpu int) error {
	var set unix.CPUSet
	if cpu >= cpuSetBits {
		return fmt.Errorf("CPU %d is above the %d CPUs that a CPU set holds", cpu, cpuSetBits)
	}
	set.Set(cpu)
	if err := unix.SchedSetaffinity(0, &set); err != nil {
		return fmt.Errorf("cannot pin the thread to CPU %d: %w", cpu, err)
	}
	return nil
}

// allowedCPUs returns the CPUs that the calling thread may run on.
func allowedCPUs() ([]int, error) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return nil, fmt.Errorf("cannot read the CPU affinity: %w", err)
	}
	var cpus []int
	for cpu := 0; cpu < cpuSetBits; cpu++ {
		if set.IsSet(cpu) {
			cpus = append(cpus, cpu)
		}
	}
	return cpus, nil
}

// memoryNodes returns the NUMA nodes with memory, read from
// devices/system/node/has_memory under the sysfs directory sysfs.
func memoryNodes(sysfs string) ([]int, error) {
	path := filepath.Join(sysfs, "devices/system/node/has_memory")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the NUMA nodes with memory: %w", err)
	}
	nodes, err := parseList(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return nodes, nil
}

// checkSubset returns an error naming the first of wanted that is not in
// have; what names the wanted numbers ("CPU", "node") and where has the
// description of have.
func checkSubset(what string, wanted, have []int, where string) error {
	ok := map[int]bool{}
	for _, n := range have {
		ok[n] = true
	}
	for _, n := range wanted {
		if !ok[n] {
			return fmt.Errorf("%s %d is not available (%s: %s)", what, n, where, formatList(have))
		}
	}
	return nil
}

// freeMemory returns the free memory in bytes (MemFree) of each of the
// nodes, read from devices/system/node/nodeN/meminfo under the sysfs
// directory sysfs.
func freeMemory(sysfs string, nodes []int) (int64, error) {
	var total int64
	for _, n := range nodes {
		path := filepath.Join(sysfs, fmt.Sprintf("devices/system/node/node%d/meminfo", n))
		f, err := os.Open(path)
		if err != nil {
			return 0, fmt.Errorf("cannot read the free memory of node %d: %w", n, err)
		}
		kb, err := memFreeKB(f)
		_ = f.Close()
		if err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		total += kb << 10
	}
	return total, nil
}

// memFreeKB returns the MemFree of a node's meminfo file in KiB, from its
// line like "Node 2 MemFree:     41871234 kB".
func memFreeKB(f *os.File) (int64, error) {
	s := bufio.NewScanner(f)
	for s.Scan() {
		_, rest, found := strings.Cut(s.Text(), "MemFree:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("MemFree %q: %w", fields[0], err)
		}
		return kb, nil
	}
	if err := s.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("no MemFree line")
}
