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

package mpolinject

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	helperEnv     = "MPOLINJECT_TEST_HELPER"
	helperThreads = 4
)

// TestMain runs the test helper process when requested, otherwise the
// tests.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		runHelper()
		return
	}
	os.Exit(m.Run())
}

// runHelper starts threads that report their memory policy on
// request. Each thread prints "ready <tid>", then waits for a line on
// stdin and prints "policy <tid> <mode> <nodes>" where nodes is a
// comma-separated list of the nodes in the policy nodemask.
func runHelper() {
	var ready, done sync.WaitGroup
	check := make(chan struct{})
	for range helperThreads {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			runtime.LockOSThread()
			fmt.Printf("ready %d\n", unix.Gettid())
			ready.Done()
			<-check
			var mode int
			mask := make([]uint64, maxNodes/64)
			_, _, errno := unix.Syscall6(unix.SYS_GET_MEMPOLICY,
				uintptr(unsafe.Pointer(&mode)), uintptr(unsafe.Pointer(&mask[0])), maxNodes, 0, 0, 0)
			if errno != 0 {
				fmt.Printf("policy %d error %v\n", unix.Gettid(), errno)
				return
			}
			var nodes []string
			for node := range maxNodes {
				if mask[node/64]&(1<<(node%64)) != 0 {
					nodes = append(nodes, strconv.Itoa(node))
				}
			}
			fmt.Printf("policy %d %d %s\n", unix.Gettid(), mode, strings.Join(nodes, ","))
		}()
	}
	ready.Wait()
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		os.Exit(1)
	}
	close(check)
	done.Wait()
}

// allowedMemoryNode returns the first node in Mems_allowed_list of the
// test process.
func allowedMemoryNode(t *testing.T) int {
	data, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Mems_allowed_list:") {
			continue
		}
		list := strings.TrimSpace(strings.TrimPrefix(line, "Mems_allowed_list:"))
		first, _, _ := strings.Cut(list, ",")
		first, _, _ = strings.Cut(first, "-")
		node, err := strconv.Atoi(first)
		require.NoError(t, err)
		return node
	}
	t.Fatal("Mems_allowed_list not found in /proc/self/status")
	return 0
}

func TestSetMemoryPolicyOnChild(t *testing.T) {
	node := allowedMemoryNode(t)

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	reader := bufio.NewReader(stdout)
	readyThreads := 0
	for readyThreads < helperThreads {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if strings.HasPrefix(line, "ready ") {
			readyThreads++
		}
	}

	err = SetMemoryPolicy([]int{cmd.Process.Pid}, unix.MPOL_INTERLEAVE, []int{node})
	if errors.Is(err, unix.EPERM) {
		t.Skipf("ptrace not permitted: %v", err)
	}
	require.NoError(t, err)

	_, err = io.WriteString(stdin, "check\n")
	require.NoError(t, err)

	policies := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if !strings.HasPrefix(line, "policy ") {
			continue
		}
		policies++
		fields := strings.Fields(line)
		require.Len(t, fields, 4, "unexpected helper output %q", line)
		require.Equal(t, strconv.Itoa(unix.MPOL_INTERLEAVE), fields[2], "tid %s", fields[1])
		require.Equal(t, strconv.Itoa(node), fields[3], "tid %s", fields[1])
	}
	require.Equal(t, helperThreads, policies)
	require.NoError(t, cmd.Wait())
}
