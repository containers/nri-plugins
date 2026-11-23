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

package leases

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
)

// Containers is what the broker needs of the NRI plugin of the node:
// the running containers, the NUMA nodes lent to them and the events of
// their life.
type Containers interface {
	// ContainerByPid returns the pod and the name of the container whose
	// cgroup has the process pid, false for none.
	ContainerByPid(pid int) (pod state.PodRef, container string, ok bool)
	// Running tells whether an instance of the container name of the pod
	// is created or started.
	Running(podUID, container string) bool
	// LendNUMANode lends the NUMA node to the container for the lease (its
	// id). Idempotent per NUMA node and lease.
	LendNUMANode(ctx context.Context, podUID, container string, numaNode int, lease string) error
	// RevokeNUMANode drops the lease of the NUMA node of the container; a
	// container that is gone is not an error. Idempotent.
	RevokeNUMANode(ctx context.Context, podUID, container string, numaNode int, lease string) error
	// OnContainerGone registers fn, called in a goroutine of its own for
	// a container that stopped or was removed.
	OnContainerGone(fn func(pod state.PodRef, container string))
	// OnContainerStarted registers fn, called in a goroutine of its own
	// once a container has started.
	OnContainerStarted(fn func(pod state.PodRef, container string))
	// WaitSynchronized returns once the NRI plugin has synchronized with the
	// runtime, or the error of ctx.
	WaitSynchronized(ctx context.Context) error
	// LendsRestored tells that the lends of the leases are restored.
	LendsRestored()
}

// Requester is the container that asks on a lease socket.
type Requester struct {
	Pod       state.PodRef
	Container string
}

// Identity tells which container a process that connected to a lease
// socket is in.
type Identity interface {
	// Resolve returns the container of the process pid, an error that is
	// errUnknownProcess when the process is in no known container.
	Resolve(pid int) (Requester, error)
}

// errUnknownProcess is the error of an Identity for a process that is in no
// known container.
var errUnknownProcess = errors.New("in no known container")

// tableIdentity is the Identity of the container table of the container
// plugin.
type tableIdentity struct {
	containers Containers
}

// Resolve returns the container of the table whose cgroup has the process.
func (t tableIdentity) Resolve(pid int) (Requester, error) {
	pod, name, ok := t.containers.ContainerByPid(pid)
	if !ok {
		return Requester{}, errUnknownProcess
	}
	return Requester{Pod: pod, Container: name}, nil
}

// connProcess is the process that connected to a lease socket, fixed at connect:
// a pid alone names whichever process holds it now.
type connProcess struct {
	pid int
	// start is the start time of the process at connect, "" when unknown.
	start string
	// pidfd refers to the process itself (SO_PEERPIDFD), -1 when the
	// kernel has no SO_PEERPIDFD.
	pidfd int
}

// processStartTime returns the start time of the process pid, field 22 of
// /proc/<pid>/stat.
var processStartTime = func(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// the command name in parentheses may hold spaces and parentheses
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return "", fmt.Errorf("/proc/%d/stat: no command name", pid)
	}
	// the fields after the command name start at field 3
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("/proc/%d/stat: %d fields", pid, len(fields)+2)
	}
	return fields[19], nil
}

// openProcess returns the process that connected: its pid from SO_PEERCRED,
// its start time and, where the kernel has SO_PEERPIDFD, its pidfd.
func openProcess(c *net.UnixConn) (*connProcess, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	p := &connProcess{pidfd: -1}
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		var cred *unix.Ucred
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if credErr != nil {
			return
		}
		p.pid = int(cred.Pid)
		if pidfd, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD); err == nil {
			p.pidfd = pidfd
		}
	}); err != nil {
		return nil, err
	}
	if credErr != nil {
		return nil, credErr
	}
	p.start, _ = processStartTime(p.pid)
	return p, nil
}

// running tells whether the process that connected still runs: a process
// that exited, or a pid whose start time differs, is gone.
func (p *connProcess) running() bool {
	if p.pidfd >= 0 && unix.PidfdSendSignal(p.pidfd, 0, nil, 0) != nil {
		return false
	}
	start, err := processStartTime(p.pid)
	return err == nil && p.start != "" && start == p.start
}

// close releases the pidfd of the process.
func (p *connProcess) close() {
	if p != nil && p.pidfd >= 0 {
		_ = unix.Close(p.pidfd)
		p.pidfd = -1
	}
}
