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

package udev

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	logger "github.com/containers/nri-plugins/pkg/log"
)

// Event represents a udev event.
type Event struct {
	Header     string
	Subsystem  string
	Action     string
	Devpath    string
	Seqnum     string
	Properties map[string]string
}

const (
	// PropertyAction is the key for the ACTION property.
	PropertyAction = "ACTION"
	// PropertyDevpath is the key for the DEVPATH property.
	PropertyDevpath = "DEVPATH"
	// PropertySubsystem is the key for the SUBSYSTEM property.
	PropertySubsystem = "SUBSYSTEM"
	// PropertySeqnum is the key for the SEQNUM property.
	PropertySeqnum = "SEQNUM"
)

var (
	log = logger.Get("udev")
)

// Reader implements an io.ReadCloser for reading raw event data from
// the udev netlink socket.
type Reader struct {
	sock   int
	closed bool
}

// NewReader creates a new io.ReadCloser for reading raw udev event data.
func NewReader() (*Reader, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return nil, fmt.Errorf("failed to create udev reader: %w", err)
	}

	addr := syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Pid:    uint32(os.Getpid()),
		Groups: 1,
	}

	if err := syscall.Bind(fd, &addr); err != nil {
		syscall.Close(fd) // nolint:errcheck
		return nil, fmt.Errorf("failed to bind udev reader: %w", err)
	}

	return &Reader{sock: fd}, nil
}

// Read implements the io.Reader interface.
func (r *Reader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.EOF
	}

	n, err := syscall.Read(r.sock, p)

	// allow wrapping Reader in a bufio.Reader, which would panic on n < 0
	if n == -1 {
		n = 0
	}

	// TODO(klihub): make this controllable using an option ?
	if err == syscall.ENOBUFS {
		log.Warn("udev ran out of buffer space (was dropping events)")
		err = nil
	}

	return n, err
}

// Close implements the io.Closer interface.
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}

	r.closed = true
	return syscall.Close(r.sock)
}

// EventReader reads udev events.
type EventReader struct {
	r io.ReadCloser
	b *bufio.Reader
}

// NewEventReader creates a new udev event reader.
func NewEventReader() (*EventReader, error) {
	r, err := NewReader()
	if err != nil {
		return nil, err
	}

	return &EventReader{
		r: r,
		b: bufio.NewReader(r),
	}, nil
}

// NewEventReaderFromReader creates a new udev event reader from an existing
// io.ReadCloser. This can be used to generate synthetic events for testing.
func NewEventReaderFromReader(r io.ReadCloser) *EventReader {
	return &EventReader{
		r: r,
		b: bufio.NewReader(r),
	}
}

// Read reads a udev event, blocking until one is available.
func (r *EventReader) Read() (*Event, error) {
	e := &Event{
		Properties: map[string]string{},
	}

	hdr, err := r.b.ReadString(0)
	if err != nil {
		return nil, err
	}
	if len(hdr) > 0 {
		hdr = hdr[:len(hdr)-1]
	}

	e.Header = hdr
	for {
		next, err := r.b.ReadString(0)
		if err != nil {
			return nil, err
		}

		kv := strings.SplitN(next, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("failed to read udev event: unknown format")
		}

		k, v := kv[0], kv[1]
		if len(v) > 0 {
			v = v[:len(v)-1]
		}
		e.Properties[k] = v

		switch k {
		case PropertyAction:
			e.Action = v
		case PropertyDevpath:
			e.Devpath = v
		case PropertySubsystem:
			e.Subsystem = v
		case PropertySeqnum:
			e.Seqnum = v
			return e, nil
		}
	}
}

// Close closes the reader.
func (r *EventReader) Close() error {
	return r.r.Close()
}
