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

package kernel

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// The kinds of the errors of the operations. An error of an operation
// matches its kind with errors.Is, and wraps its cause (a syscall error, the
// error of a command) when it has one.
var (
	// ErrNotFound: no memdev has the serial, or the object named does not
	// exist.
	ErrNotFound = errors.New("not found")
	// ErrEndpointNotReady: the memdev has no endpoint port (yet): it is
	// not enabled, or its probe has not finished ("add_dev() failed").
	ErrEndpointNotReady = errors.New("memdev endpoint not ready")
	// ErrBusyOnline: memory blocks are online that the operation needs
	// offline (kmem keeps online memory until a reboot).
	ErrBusyOnline = errors.New("memory blocks online")
	// ErrNoRoom: the dax region, the root decoder or the port has no room
	// for the object.
	ErrNoRoom = errors.New("no room")
)

// kernelError is an error of a kind with the message of the operation.
type kernelError struct {
	kind error
	msg  string
	err  error
}

func (e *kernelError) Error() string { return e.msg }

// Is tells whether target is the kind of the error.
func (e *kernelError) Is(target error) bool { return target == e.kind }

// Unwrap returns the cause of the error.
func (e *kernelError) Unwrap() error { return e.err }

// errorf returns an error of the kind with the message of format. A %w verb
// in format makes its operand the cause.
func errorf(kind error, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	return &kernelError{kind: kind, msg: err.Error(), err: errors.Unwrap(err)}
}

// ErrDecoderHole is the error of a region creation that a port refused
// because a region below newer regions was destroyed in it (see
// DecoderHole). It wraps the error of the creation.
type ErrDecoderHole struct {
	// Port is the port nearest to the root with a hole (portN, or the
	// endpoint port endpointN of the memdev).
	Port string
	// Decoder is the lowest decoder of Port without a region.
	Decoder string
	// Blocking are the regions on decoders above the holes of every port:
	// the ports take new regions again once all of them are destroyed.
	Blocking []string
	// Holes are the ports with a hole, nearest to the root first.
	Holes []DecoderHole
	// Err is the error of the creation.
	Err error
}

func (e *ErrDecoderHole) Error() string {
	var s []string
	for _, h := range e.Holes {
		s = append(s, h.String())
	}
	until := "the decoders above them are released"
	switch len(e.Blocking) {
	case 0:
	case 1:
		until = e.Blocking[0] + " is destroyed"
	default:
		until = strings.Join(e.Blocking, ", ") + " are destroyed"
	}
	return fmt.Sprintf("%v (CXL decoders released out of order: %s; the kernel creates no region through these ports until %s)",
		e.Err, strings.Join(s, "; "), until)
}

// Unwrap returns the error of the creation.
func (e *ErrDecoderHole) Unwrap() error { return e.Err }

// newErrDecoderHole returns the error of a creation refused by the ports
// with holes, nil if there are none.
func newErrDecoderHole(holes []DecoderHole, err error) error {
	if len(holes) == 0 {
		return nil
	}
	e := &ErrDecoderHole{Port: holes[0].Port, Holes: holes, Err: err}
	if len(holes[0].Free) > 0 {
		e.Decoder = holes[0].Free[0]
	}
	for _, h := range holes {
		for _, r := range h.Blocking {
			if !slices.Contains(e.Blocking, r) {
				e.Blocking = append(e.Blocking, r)
			}
		}
	}
	return e
}

// isGone tells whether err says that a device or a file does not exist.
func isGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENXIO) || errors.Is(err, unix.ENODEV)
}
