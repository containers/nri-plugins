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

package qemu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

const hmpPrompt = "(qemu) "

// HMP is a Monitor that talks to the human monitor (readline mode) over a
// unix socket. Every command uses a new connection, like vm-monitor in
// test/e2e/lib/vm.bash does.
type HMP struct {
	Path         string
	PollInterval time.Duration
	Log          Logf // logs every command and its output
	mu           sync.Mutex
	closed       bool
}

// NewHMP returns an HMP monitor for the socket path.
func NewHMP(path string, log Logf) *HMP {
	return &HMP{Path: path, PollInterval: DefaultPollInterval, Log: log}
}

// Close implements Monitor. It is final, like QMP.Close.
func (h *HMP) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
}

// Command runs one HMP command line and returns its output with the
// readline echo, terminal escapes, carriage returns and the prompt removed.
// An output line starting with "Error:" makes it return an *Error.
func (h *HMP) Command(ctx context.Context, line string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return "", ErrClosed
	}
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	out, err := h.command(ctx, line)
	if err != nil {
		h.Log.printf("hmp %s: %q: error: %v", h.Path, line, err)
		return "", err
	}
	if !strings.HasPrefix(line, "info ") {
		h.Log.printf("hmp %s: %q: %q", h.Path, line, out)
	}
	return out, hmpError(out, line)
}

// hmpError returns an *Error for the first line of the output of an HMP
// command line that starts with "Error: ", nil if there is none.
func hmpError(out, line string) error {
	for _, l := range strings.Split(out, "\n") {
		if desc, ok := strings.CutPrefix(l, "Error: "); ok {
			return &Error{Desc: desc, Command: line}
		}
	}
	return nil
}

func (h *HMP) command(ctx context.Context, line string) (string, error) {
	conn, err := dialUnix(ctx, h.Path)
	if err != nil {
		return "", fmt.Errorf("hmp connect %s: %w", h.Path, err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	var buf bytes.Buffer
	readUntil := func(done func([]byte) bool) error {
		chunk := make([]byte, 64*1024)
		for !done(buf.Bytes()) {
			n, err := conn.Read(chunk)
			buf.Write(chunk[:n])
			if err != nil {
				if ne, ok := errors.AsType[net.Error](err); ctx.Err() != nil || (ok && ne.Timeout()) {
					return fmt.Errorf("hmp %s: no prompt: %w (is another client connected?)", h.Path, context.DeadlineExceeded)
				}
				return fmt.Errorf("hmp %s: read: %w", h.Path, err)
			}
		}
		return nil
	}
	// greeting and the first prompt
	if err := readUntil(func(b []byte) bool { return bytes.HasSuffix(b, []byte(hmpPrompt)) }); err != nil {
		return "", err
	}
	buf.Reset()
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		return "", fmt.Errorf("hmp %s: write: %w", h.Path, err)
	}
	// echo line, output, prompt
	if err := readUntil(func(b []byte) bool {
		i := bytes.IndexByte(b, '\n')
		return i >= 0 && bytes.HasSuffix(b[i+1:], []byte(hmpPrompt))
	}); err != nil {
		return "", err
	}
	return CleanHMPOutput(buf.String()), nil
}

var ansiEscapeRe = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

// CleanHMPOutput turns the raw bytes that HMP sends after a command line
// into the command output: it drops the readline echo (everything up to
// the first newline), terminal escape sequences, carriage returns and the
// trailing prompt.
func CleanHMPOutput(raw string) string {
	s := raw
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, hmpPrompt)
	s = ansiEscapeRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.TrimRight(s, "\n ")
}

// Version implements Monitor.
func (h *HMP) Version(ctx context.Context) (string, error) {
	out, err := h.Command(ctx, "info version")
	if err != nil {
		return "", err
	}
	return NormalizeVersion(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])), nil
}

var versionRe = regexp.MustCompile(`^(\d+\.\d+\.\d+)\s*(.*)$`)

// NormalizeVersion formats "11.1.1openSUSE Slowroll" (HMP) as
// "11.1.1 openSUSE Slowroll".
func NormalizeVersion(v string) string {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return strings.TrimSpace(v)
	}
	if pkg := strings.Trim(strings.TrimSpace(m[2]), "()"); pkg != "" {
		return m[1] + " " + pkg
	}
	return m[1]
}

// HMPObjectAdd returns the HMP object_add command line for the backend.
func HMPObjectAdd(b MemoryBackend) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "object_add %s,id=%s,size=%d", b.QomType, EscapeOptValue(b.ID), b.Size)
	if b.Share {
		sb.WriteString(",share=on")
	}
	if b.QomType == MemoryBackendFile {
		sb.WriteString(",mem-path=" + EscapeOptValue(b.MemPath))
		if b.Align != 0 {
			fmt.Fprintf(&sb, ",align=%d", b.Align)
		}
	}
	return sb.String()
}

// HMPDeviceAdd returns the HMP device_add command line for the device.
func HMPDeviceAdd(d CXLType3) string {
	var sb strings.Builder
	sb.WriteString("device_add cxl-type3,bus=" + EscapeOptValue(d.Bus))
	if d.VolatileMemdev != "" {
		sb.WriteString(",volatile-memdev=" + EscapeOptValue(d.VolatileMemdev))
	}
	if d.PersistentMemdev != "" {
		sb.WriteString(",persistent-memdev=" + EscapeOptValue(d.PersistentMemdev))
	}
	if d.LSA != "" {
		sb.WriteString(",lsa=" + EscapeOptValue(d.LSA))
	}
	fmt.Fprintf(&sb, ",id=%s,sn=0x%x", EscapeOptValue(d.ID), d.Serial)
	return sb.String()
}

// ObjectAdd implements Monitor.
func (h *HMP) ObjectAdd(ctx context.Context, b MemoryBackend) error {
	_, err := h.Command(ctx, HMPObjectAdd(b))
	return err
}

// ObjectDel implements Monitor.
func (h *HMP) ObjectDel(ctx context.Context, id string) error {
	_, err := h.Command(ctx, "object_del "+id)
	return err
}

// DeviceAdd implements Monitor.
func (h *HMP) DeviceAdd(ctx context.Context, d CXLType3) error {
	_, err := h.Command(ctx, HMPDeviceAdd(d))
	return err
}

// DeviceDel implements Monitor.
func (h *HMP) DeviceDel(ctx context.Context, id string) error {
	_, err := h.Command(ctx, "device_del "+id)
	return err
}

// QueryMemdevs implements Monitor.
func (h *HMP) QueryMemdevs(ctx context.Context) ([]Memdev, error) {
	out, err := h.Command(ctx, "info memdev")
	if err != nil {
		return nil, err
	}
	return ParseInfoMemdev(out), nil
}

// QueryTree implements Monitor.
func (h *HMP) QueryTree(ctx context.Context) (*Tree, error) {
	out, err := h.Command(ctx, "info qtree")
	if err != nil {
		return nil, err
	}
	return ParseQtree(out), nil
}

// DeviceExists returns true if the device id is in the device tree.
func (h *HMP) DeviceExists(ctx context.Context, id string) (bool, error) {
	out, err := h.Command(ctx, "info qtree -b")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, fmt.Sprintf(`, id "%s"`, id)), nil
}

// BackendMapped implements Monitor.
func (h *HMP) BackendMapped(ctx context.Context, objID string) (bool, error) {
	out, err := h.Command(ctx, "info mtree")
	if err != nil {
		return false, err
	}
	return BackendMappedIn(out, objID), nil
}

// QomGet implements Monitor.
func (h *HMP) QomGet(ctx context.Context, path, property string) (json.RawMessage, error) {
	out, err := h.Command(ctx, "qom-get "+path+" "+property)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if !json.Valid([]byte(out)) {
		return nil, &Error{Command: "qom-get", Desc: "invalid output: " + out}
	}
	return json.RawMessage(out), nil
}

// WaitDeviceDeleted implements Monitor by polling the device tree.
func (h *HMP) WaitDeviceDeleted(ctx context.Context, id string) error {
	return pollDeviceDeleted(ctx, id, h.PollInterval, h.DeviceExists)
}

func pollDeviceDeleted(ctx context.Context, id string, interval time.Duration,
	exists func(context.Context, string) (bool, error)) error {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	for {
		cctx, cancel := context.WithTimeout(ctx, DefaultCommandTimeout)
		ok, err := exists(cctx, id)
		cancel()
		if err == nil && !ok {
			return nil
		}
		if errors.Is(err, ErrClosed) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
