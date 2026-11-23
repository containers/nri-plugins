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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
)

// Process is a qemu process found from /proc, or parsed from a command line.
type Process struct {
	PID       int
	StartTime uint64 // /proc/PID/stat starttime: detects pid reuse/restarts
	Cwd       string
	// Name is the VM name: <name> of .vagrant/machines/<name>/ in a
	// -drive file path, else -name guest=<name>, else "".
	Name string
	// ProjectDir is the directory that contains .vagrant/ (the e2e test
	// output directory where monitor.sock and qmp.sock are created).
	ProjectDir string
	UUID       string
	// QMPSockets and HMPSockets are monitor socket paths in order of
	// preference, as given on the command line (maybe relative).
	QMPSockets []string
	HMPSockets []string
	Objects    []Object
}

// Object is a -object argument.
type Object struct {
	Type  string
	ID    string
	Props map[string]string
}

var vagrantMachineRe = regexp.MustCompile(`^(.*)/\.vagrant/machines/([^/]+)/`)

// options that take a value and that the parser looks at
var qemuValueOptions = map[string]bool{
	"drive": true, "qmp": true, "monitor": true, "chardev": true,
	"mon": true, "object": true, "name": true, "uuid": true,
}

// ParseCmdline parses a qemu command line. Socket paths are not resolved.
func ParseCmdline(args []string) *Process {
	p := &Process{}
	type chardev struct{ path string }
	chardevs := map[string]chardev{}
	type mon struct{ chardev, mode string }
	var mons []mon
	nameOpt := ""
	for i := 1; i < len(args); i++ {
		opt := strings.TrimLeft(args[i], "-")
		if !strings.HasPrefix(args[i], "-") || !qemuValueOptions[opt] || i+1 >= len(args) {
			continue
		}
		val := args[i+1]
		i++
		switch opt {
		case "drive":
			o := ParseOpts(val, "")
			if m := vagrantMachineRe.FindStringSubmatch(o.Get("file")); m != nil && p.Name == "" {
				p.ProjectDir, p.Name = m[1], m[2]
			}
		case "name":
			o := ParseOpts(val, "guest")
			nameOpt = o.First
		case "uuid":
			p.UUID = strings.ToLower(strings.TrimSpace(val))
		case "qmp":
			if path, ok := unixSocketPath(val); ok {
				p.QMPSockets = append(p.QMPSockets, path)
			}
		case "monitor":
			if path, ok := unixSocketPath(val); ok {
				p.HMPSockets = append(p.HMPSockets, path)
			}
		case "chardev":
			o := ParseOpts(val, "backend")
			if o.First == "socket" && o.Get("path") != "" && isOn(o, "server") {
				chardevs[o.Get("id")] = chardev{path: o.Get("path")}
			}
		case "mon":
			o := ParseOpts(val, "chardev")
			cd := o.Get("chardev")
			if cd == "" {
				cd = o.First
			}
			mode := o.Get("mode")
			if mode == "" {
				mode = "readline"
			}
			mons = append(mons, mon{chardev: cd, mode: mode})
		case "object":
			o := ParseOpts(val, "qom-type")
			p.Objects = append(p.Objects, Object{Type: o.First, ID: o.Get("id"), Props: o.Props})
		}
	}
	for _, m := range mons {
		cd, ok := chardevs[m.chardev]
		if !ok {
			continue
		}
		if m.mode == "control" {
			p.QMPSockets = append(p.QMPSockets, cd.path)
		} else {
			p.HMPSockets = append(p.HMPSockets, cd.path)
		}
	}
	if p.Name == "" {
		p.Name = nameOpt
	}
	return p
}

func isOn(o *Opts, key string) bool {
	if !o.Has(key) {
		return false
	}
	switch o.Get(key) {
	case "", "on", "yes", "true":
		return true
	}
	return false
}

// unixSocketPath returns the path of "unix:PATH[,opts]" if it is a server.
func unixSocketPath(val string) (string, bool) {
	rest, ok := strings.CutPrefix(val, "unix:")
	if !ok {
		return "", false
	}
	o := ParseOpts(rest, "path")
	if o.First == "" {
		return "", false
	}
	if o.Has("server") && !isOn(o, "server") {
		return "", false
	}
	return o.First, true
}

// ResolvePath resolves a socket path of the command line. Relative paths
// are relative to the cwd of qemu when it started; a daemonized qemu has
// cwd "/" by now, so the vagrant project dir is tried, too. The first
// candidate for which exists returns true is returned, else the most
// likely candidate.
func (p *Process) ResolvePath(path string, exists func(string) bool) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	var cands []string
	if p.Cwd != "" && p.Cwd != "/" {
		cands = append(cands, filepath.Join(p.Cwd, path))
	}
	if p.ProjectDir != "" {
		cands = append(cands, filepath.Join(p.ProjectDir, path))
	}
	if p.Cwd == "/" {
		cands = append(cands, filepath.Join("/", path))
	}
	for _, c := range cands {
		if exists != nil && exists(c) {
			return c
		}
	}
	if len(cands) > 0 {
		return cands[0]
	}
	return path
}

// IsSocket returns true if path is a unix socket.
func IsSocket(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// LocalBackend is a memory-backend-ram object pre-declared on the qemu
// command line for a CXL memory device (test/e2e/lib/topology2qemuopts.py
// naming: beram_<memdev>__bus_<bus>__sn_<serial>).
type LocalBackend struct {
	ObjectID  string
	Memdev    string // e.g. cxl_memdev3
	Bus       string
	Serial    uint64
	HasSerial bool
	Size      int64
}

var localBackendRe = regexp.MustCompile(`^beram_(.+?)__bus_(.+?)__sn_(.+)$`)

// ParseLocalBackendID parses a beram_ object id.
func ParseLocalBackendID(id string) (LocalBackend, bool) {
	m := localBackendRe.FindStringSubmatch(id)
	if m == nil {
		return LocalBackend{}, false
	}
	lb := LocalBackend{ObjectID: id, Memdev: m[1], Bus: m[2]}
	if sn, err := cxlapi.ParseSerial(m[3]); err == nil {
		lb.Serial, lb.HasSerial = sn, true
	}
	return lb, true
}

// LocalBackends returns the pre-declared CXL memory backends.
func (p *Process) LocalBackends() []LocalBackend {
	var out []LocalBackend
	for _, o := range p.Objects {
		lb, ok := ParseLocalBackendID(o.ID)
		if !ok || o.Type != MemoryBackendRAM {
			continue
		}
		if v, err := api.ParseSize(o.Props["size"]); err == nil {
			lb.Size = v
		}
		out = append(out, lb)
	}
	return out
}

// Discover scans procRoot (normally "/proc") for qemu-system-* processes.
func Discover(procRoot string) ([]*Process, error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var out []*Process
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if !strings.HasPrefix(filepath.Base(args[0]), "qemu-system-") {
			continue
		}
		p := ParseCmdline(args)
		p.PID = pid
		p.Cwd, _ = os.Readlink(filepath.Join(dir, "cwd"))
		_, p.StartTime = readStat(filepath.Join(dir, "stat"))
		for i, s := range p.QMPSockets {
			p.QMPSockets[i] = p.ResolvePath(s, IsSocket)
		}
		for i, s := range p.HMPSockets {
			p.HMPSockets[i] = p.ResolvePath(s, IsSocket)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// readStat returns the state and the start time of a process from its
// stat file, 0 if it cannot be read.
func readStat(statPath string) (string, uint64) {
	b, err := os.ReadFile(statPath)
	if err != nil {
		return "", 0
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", 0
	}
	f := strings.Fields(string(b[i+1:]))
	// fields after comm: state(3) ... starttime is field 22 overall
	if len(f) < 20 {
		return "", 0
	}
	v, _ := strconv.ParseUint(f[19], 10, 64)
	return f[0], v
}

// ProcessAlive returns true if the process exists, has the start time and
// is not a zombie (an exited qemu that is not reaped yet has closed its
// monitor sockets; Discover does not list it either).
func ProcessAlive(procRoot string, pid int, startTime uint64) bool {
	if pid <= 0 {
		return false
	}
	state, st := readStat(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if st == 0 || state == "Z" || state == "X" {
		return false
	}
	return startTime == 0 || st == startTime
}
