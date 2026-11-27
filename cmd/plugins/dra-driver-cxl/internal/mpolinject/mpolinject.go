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

// Package mpolinject sets the NUMA memory policy of running processes
// by injecting a set_mempolicy system call into every thread of the
// process with ptrace. The processes must be ptraceable by the
// caller. Injection is implemented for linux/amd64 only.
//
// The injection is unsafe for threaded programs. It writes a syscall
// and an int3 instruction at the instruction pointer of each stopped
// thread, runs them and writes the code back. A task that shares the
// memory of the process and runs meanwhile can execute the written
// code and die of SIGTRAP: a thread that the injector could not stop,
// a thread that appeared after its last listing, or a child made with
// vfork, as posix_spawn and Go's os/exec make. The int3 resets a SIGTRAP handler or ignore of the target to
// the default. A signal that arrives while a thread is stopped can be
// lost. A 32-bit process gets SIGILL or another system call. The
// nodemask is written below the stack pointer of each thread and stays
// there; on a stack that a runtime allocates, such as a goroutine
// stack, that can be other memory of the process. The wait for a thread
// to stop has no time limit: a thread in an uninterruptible sleep
// stalls the injection.
package mpolinject

import (
	"errors"
	"fmt"
)

// ErrUnsupported is returned by SetMemoryPolicy on platforms without
// an injection implementation.
var ErrUnsupported = errors.New("memory policy injection is not supported on this platform")

// maxNodes is the limit of the node IDs in a nodemask.
const maxNodes = 1024

// nodeMask returns the set_mempolicy nodemask of the nodes as 64-bit
// words.
func nodeMask(nodes []int) ([]uint64, error) {
	if len(nodes) == 0 {
		return nil, errors.New("no nodes")
	}
	maxNode := 0
	for _, node := range nodes {
		if node < 0 || node >= maxNodes {
			return nil, fmt.Errorf("invalid NUMA node %d", node)
		}
		maxNode = max(maxNode, node)
	}
	mask := make([]uint64, maxNode/64+1)
	for _, node := range nodes {
		mask[node/64] |= 1 << (node % 64)
	}
	return mask, nil
}
