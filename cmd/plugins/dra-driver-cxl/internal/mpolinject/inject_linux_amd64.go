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
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Supported tells whether SetMemoryPolicy is implemented on this
// platform.
const Supported = true

const (
	// maxWorkers is the number of processes handled in parallel.
	// Workers are mostly blocked in wait4, so this does not need to
	// follow the number of CPUs.
	maxWorkers = 8
	// stackOffset is the distance below the stack pointer where the
	// nodemask is written. The 128 bytes right below the stack
	// pointer are the red zone of the ABI and must not be touched.
	stackOffset = 256
	// maxStopRounds bounds the number of thread listings done while
	// stopping the threads of a process.
	maxStopRounds = 10
)

// SetMemoryPolicy sets the memory policy of every thread of the
// processes. Processes are handled in parallel and all threads of a
// process are stopped while its threads are updated. Failures of
// individual processes and threads are collected into the returned
// error, the others are still updated.
//
// Parameters:
//   - pids: process IDs, for example from cgroup.procs.
//   - mode: set_mempolicy mode, unix.MPOL_PREFERRED or unix.MPOL_INTERLEAVE.
//   - nodes: NUMA node IDs, at least one.
func SetMemoryPolicy(pids []int, mode int, nodes []int) error {
	if len(pids) == 0 {
		return errors.New("no processes")
	}
	mask, err := nodeMask(nodes)
	if err != nil {
		return err
	}

	pidCh := make(chan int, len(pids))
	for _, pid := range pids {
		pidCh <- pid
	}
	close(pidCh)

	errCh := make(chan error, len(pids))
	var wg sync.WaitGroup
	for range min(len(pids), maxWorkers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// All ptrace requests on a tracee must come from
			// the OS thread that seized it.
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			for pid := range pidCh {
				if err := setProcessPolicy(pid, mode, mask); err != nil {
					errCh <- fmt.Errorf("pid %d: %w", pid, err)
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// stoppedThread is a thread in ptrace-stop.
type stoppedThread struct {
	tid int
	// sig is a signal that was about to be delivered when the
	// thread stopped. It is delivered when the thread is detached.
	sig unix.Signal
}

// setProcessPolicy stops all threads of the process, injects the
// set_mempolicy call into each of them and resumes them.
func setProcessPolicy(pid int, mode int, mask []uint64) error {
	threads, errs := stopThreads(pid)
	defer resumeThreads(threads)
	for i := range threads {
		sig, err := injectSetMempolicy(threads[i].tid, mode, mask)
		if err != nil {
			errs = append(errs, fmt.Errorf("tid %d: %w", threads[i].tid, err))
		}
		if sig != 0 {
			threads[i].sig = sig
		}
	}
	return errors.Join(errs...)
}

// stopThreads seizes and interrupts the threads of the process until
// no new threads appear. Returns the stopped threads and errors for
// threads that could not be stopped.
func stopThreads(pid int) ([]stoppedThread, []error) {
	var threads []stoppedThread
	var errs []error
	seen := make(map[int]bool)
	for range maxStopRounds {
		tids, err := threadIDs(pid)
		if err != nil {
			errs = append(errs, err)
			break
		}
		newThreads := false
		for _, tid := range tids {
			if seen[tid] {
				continue
			}
			seen[tid] = true
			newThreads = true
			thread, err := stopThread(tid)
			if errors.Is(err, unix.ESRCH) {
				// The thread exited before it was stopped.
				continue
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("tid %d: %w", tid, err))
				continue
			}
			threads = append(threads, thread)
		}
		if !newThreads {
			break
		}
	}
	return threads, errs
}

// stopThread seizes the thread and brings it into ptrace-stop.
func stopThread(tid int) (stoppedThread, error) {
	if err := unix.PtraceSeize(tid); err != nil {
		return stoppedThread{}, fmt.Errorf("seize: %w", err)
	}
	if err := unix.PtraceInterrupt(tid); err != nil {
		_ = ptraceDetach(tid, 0)
		return stoppedThread{}, fmt.Errorf("interrupt: %w", err)
	}
	var ws unix.WaitStatus
	if _, err := unix.Wait4(tid, &ws, unix.WALL, nil); err != nil {
		_ = ptraceDetach(tid, 0)
		return stoppedThread{}, fmt.Errorf("wait: %w", err)
	}
	if !ws.Stopped() {
		return stoppedThread{}, unix.ESRCH
	}
	thread := stoppedThread{tid: tid}
	if ws>>16 == 0 {
		// Signal-delivery-stop instead of the interrupt stop:
		// the signal must be delivered when the thread resumes.
		thread.sig = ws.StopSignal()
	}
	return thread, nil
}

// resumeThreads detaches the threads, delivering their pending signals.
func resumeThreads(threads []stoppedThread) {
	for _, thread := range threads {
		_ = ptraceDetach(thread.tid, thread.sig)
	}
}

// ptraceDetach detaches the thread and delivers sig to it if non-zero.
func ptraceDetach(tid int, sig unix.Signal) error {
	if _, _, errno := unix.Syscall6(unix.SYS_PTRACE, unix.PTRACE_DETACH, uintptr(tid), 0, uintptr(sig), 0, 0); errno != 0 {
		return errno
	}
	return nil
}

// threadIDs returns the thread IDs of the process from /proc.
func threadIDs(pid int) ([]int, error) {
	entries, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	if err != nil {
		return nil, err
	}
	tids := make([]int, 0, len(entries))
	for _, entry := range entries {
		tid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		tids = append(tids, tid)
	}
	return tids, nil
}

// injectSetMempolicy executes set_mempolicy(mode, mask) in the thread,
// which must be in ptrace-stop, and restores its registers and code.
// Returns a signal that interrupted the injected call, if any.
func injectSetMempolicy(tid int, mode int, mask []uint64) (sig unix.Signal, err error) {
	var regs unix.PtraceRegs
	if err := unix.PtraceGetRegs(tid, &regs); err != nil {
		return 0, fmt.Errorf("get registers: %w", err)
	}
	origRegs := regs

	var origCode [8]byte
	if _, err := unix.PtracePeekData(tid, uintptr(regs.Rip), origCode[:]); err != nil {
		return 0, fmt.Errorf("read code: %w", err)
	}

	// Write the nodemask on the stack below the red zone.
	var maskAddr, maxnode uint64
	if len(mask) > 0 {
		maskAddr = (regs.Rsp - stackOffset) &^ 7
		maxnode = uint64(len(mask) * 64)
		var word [8]byte
		for i, value := range mask {
			binary.LittleEndian.PutUint64(word[:], value)
			if _, err := unix.PtracePokeData(tid, uintptr(maskAddr)+uintptr(i*8), word[:]); err != nil {
				return 0, fmt.Errorf("write nodemask: %w", err)
			}
		}
	}

	// Replace the next instructions with "syscall; int3", keeping
	// the rest of the 8 bytes as they are.
	code := origCode
	code[0], code[1], code[2] = 0x0f, 0x05, 0xcc
	if _, err := unix.PtracePokeData(tid, uintptr(regs.Rip), code[:]); err != nil {
		return 0, fmt.Errorf("write code: %w", err)
	}
	defer func() {
		if _, e := unix.PtracePokeData(tid, uintptr(origRegs.Rip), origCode[:]); e != nil {
			err = errors.Join(err, fmt.Errorf("restore code: %w", e))
		}
		if e := unix.PtraceSetRegs(tid, &origRegs); e != nil {
			err = errors.Join(err, fmt.Errorf("restore registers: %w", e))
		}
	}()

	regs.Rax = unix.SYS_SET_MEMPOLICY
	regs.Rdi = uint64(mode)
	regs.Rsi = maskAddr
	regs.Rdx = maxnode
	if err := unix.PtraceSetRegs(tid, &regs); err != nil {
		return 0, fmt.Errorf("set registers: %w", err)
	}
	if err := unix.PtraceCont(tid, 0); err != nil {
		return 0, fmt.Errorf("continue: %w", err)
	}
	var ws unix.WaitStatus
	if _, err := unix.Wait4(tid, &ws, unix.WALL, nil); err != nil {
		return 0, fmt.Errorf("wait: %w", err)
	}
	if !ws.Stopped() {
		return 0, errors.New("thread exited during injection")
	}
	if ws>>16 != 0 || ws.StopSignal() != unix.SIGTRAP {
		return ws.StopSignal(), fmt.Errorf("injection interrupted by signal %d", ws.StopSignal())
	}

	var result unix.PtraceRegs
	if err := unix.PtraceGetRegs(tid, &result); err != nil {
		return 0, fmt.Errorf("get result: %w", err)
	}
	if result.Rax > 0xfffffffffffff000 {
		return 0, fmt.Errorf("set_mempolicy: %w", syscall.Errno(-result.Rax))
	}
	return 0, nil
}
