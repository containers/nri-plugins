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

// Package api is what containers see of the CXL DRA driver: the lease
// protocol of the socket $CXL_LEASE_SOCKET that a lease client speaks, and
// the names of the environment variables of the memory that claims and
// leases give a container. It uses the standard library only. See
// docs/memory/cxl/leases.md (protocol) and docs/memory/cxl/usage.md
// (environment).
package api

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// SocketEnv names the lease socket in containers of lease claims.
	SocketEnv = "CXL_LEASE_SOCKET"
	// DefaultSocket is the path of the lease socket in containers.
	DefaultSocket = "/run/cxl/cxl.sock"
	// FSDir is the directory of the mounts of fs leases in containers of
	// lease claims.
	FSDir = "/run/cxl/fs"
	// LeaseEnv is the lease id in the environment of a command that a lease
	// client runs with the memory of a lease.
	LeaseEnv = "CXL_LEASE"
	// ClaimEnvPrefix is the prefix of the claim marker
	// CXL_CLAIM_<uid with underscores>=1 of the claims of both drivers.
	ClaimEnvPrefix = "CXL_CLAIM_"

	// SourceLocal is memory of a local memdev of the node.
	SourceLocal = "local"
	// SourcePool is memory of a device of a memory pool.
	SourcePool = "pool"

	// ModeNUMA gives memory as system RAM of a NUMA node.
	ModeNUMA = "numa"
	// ModeDax gives memory as a dax device (claims) or its fd
	// (leases).
	ModeDax = "dax"
	// ModeBlock gives persistent memory as the block device of its fsdax
	// namespace (claims) or its fd (leases).
	ModeBlock = "block"
	// ModeFS gives persistent memory as a filesystem mounted from its
	// fsdax namespace, read-only unless the claim or lease opts in to
	// read-write.
	ModeFS = "fs"

	// AccessDirect is a mode fs mount whose file pages are the device's
	// memory (dax=always).
	AccessDirect = "direct"
	// AccessCached is a mode fs mount whose file pages are in the page
	// cache of the node (dax=never).
	AccessCached = "cached"

	// The filesystem types that the drivers know (KnownFSTypes).
	FSTypeErofs    = "erofs"
	FSTypeSquashfs = "squashfs"
	FSTypeExt4     = "ext4"
	FSTypeXfs      = "xfs"
	FSTypeBtrfs    = "btrfs"
	FSTypeGfs2     = "gfs2"

	// Operations of a request.
	OpRequest = "request"
	OpRelease = "release"
	OpList    = "list"
	OpFd      = "fd"
	OpEnv     = "env"

	// States of a lease.
	StateCreating  = "Creating"  // stored, companion objects not created yet
	StatePending   = "Pending"   // companion claim and pod exist
	StateLending   = "Lending"   // companion claim being prepared
	StateReady     = "Ready"     // memory lent, dax or block fd available, or filesystem mounted
	StateReleasing = "Releasing" // companion being deleted
	StateFailed    = "Failed"    // see Error
	StateReleased  = "Released"  // gone (response state only)

	// DRAMKey is the env key of the DRAM device.
	DRAMKey = "DRAM"
)

// MaxWait is the longest wait of a request.
const MaxWait = time.Hour

// maxRequestID is the longest requestID in bytes.
const maxRequestID = 64

// Request is one request line on a lease socket. Only Op is required.
type Request struct {
	Op     string `json:"op"`
	Source string `json:"source,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Share  bool   `json:"share,omitempty"`
	// ReadOnly absent is the mode's default: read-only for fs, read-write
	// for dax and block (EffectiveReadOnly).
	ReadOnly  *bool  `json:"readOnly,omitempty"`
	Access    string `json:"access,omitempty"`
	FSType    string `json:"fsType,omitempty"`
	Name      string `json:"name,omitempty"`
	Size      string `json:"size,omitempty"`
	Serial    string `json:"serial,omitempty"`
	Wait      string `json:"wait,omitempty"`
	RequestID string `json:"requestID,omitempty"`
	Lease     string `json:"lease,omitempty"`
	All       bool   `json:"all,omitempty"`
}

// Response is one response line on a lease socket. The response of a
// Ready dax or block lease to request and fd carries the open fd of its
// device (SCM_RIGHTS). Path is the directory of the filesystem of a Ready
// fs lease in the container, FSType its filesystem type. Dax and Block are the host paths of the
// devices of Ready dax and block leases, informational: the container has
// no such device nodes.
type Response struct {
	Lease    string            `json:"lease,omitempty"`
	State    string            `json:"state,omitempty"`
	Source   string            `json:"source,omitempty"`
	Mode     string            `json:"mode,omitempty"`
	Share    bool              `json:"share"`
	ReadOnly bool              `json:"readOnly"`
	Access   string            `json:"access,omitempty"`
	FSType   string            `json:"fsType,omitempty"`
	Path     string            `json:"path,omitempty"`
	Size     uint64            `json:"size,omitempty"`
	Serial   string            `json:"serial,omitempty"`
	NUMANode *int              `json:"numaNode,omitempty"`
	Dax      string            `json:"dax,omitempty"`
	Block    string            `json:"block,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Error    string            `json:"error,omitempty"`
	Leases   *[]Response       `json:"leases,omitempty"`
}

// EffectiveMode returns the mode of a request: Mode, or dax for a share
// and numa otherwise when Mode is empty.
func (r *Request) EffectiveMode() string {
	switch {
	case r.Mode != "":
		return r.Mode
	case r.Share:
		return ModeDax
	}
	return ModeNUMA
}

// EffectiveReadOnly returns whether a request asks for memory that allows
// reads only: ReadOnly, or by default an fs lease.
func (r *Request) EffectiveReadOnly() bool {
	if r.ReadOnly != nil {
		return *r.ReadOnly
	}
	return r.EffectiveMode() == ModeFS
}

// Validate returns an error if the fields of the request do not fit
// together or have invalid values.
func (r *Request) Validate() error {
	if r.Wait != "" {
		d, err := time.ParseDuration(r.Wait)
		if err != nil || d < 0 {
			return fmt.Errorf("invalid wait %q", r.Wait)
		}
		if d > MaxWait {
			return fmt.Errorf("invalid wait %q (at most %s)", r.Wait, MaxWait)
		}
	}
	if r.RequestID != "" && !portableName(r.RequestID, maxRequestID) {
		return fmt.Errorf(`invalid requestID %q (1 to %d letters, digits, ".", "_" and "-")`, r.RequestID, maxRequestID)
	}
	if r.Lease != "" && !validLease(r.Lease) {
		return fmt.Errorf("invalid lease %q (8 lower-case hex digits)", r.Lease)
	}
	switch r.Op {
	case OpRequest:
		return r.validateRequest()
	case OpRelease:
		if (r.Lease == "") == !r.All {
			return errors.New("release takes lease or all")
		}
	case OpFd, OpEnv:
		if r.Lease == "" {
			return fmt.Errorf("%s needs lease", r.Op)
		}
	case OpList:
	default:
		return fmt.Errorf("unknown op %q (%s, %s, %s, %s or %s)", r.Op, OpRequest, OpRelease, OpList, OpFd, OpEnv)
	}
	return nil
}

// validateRequest returns an error if the fields of an OpRequest do not
// fit together or have invalid values.
func (r *Request) validateRequest() error {
	if err := r.validateValues(); err != nil {
		return err
	}
	mode, readOnly := r.EffectiveMode(), r.EffectiveReadOnly()
	switch {
	case r.Share && r.Mode == ModeNUMA:
		return errors.New("shared memory is never onlined as RAM: use mode dax")
	case r.Access != "" && mode != ModeFS:
		return errors.New("access needs mode fs")
	case r.Name != "" && mode != ModeFS:
		return errors.New("name needs mode fs")
	case r.Name != "" && !ValidName(r.Name):
		return fmt.Errorf(`invalid name %q (letters, digits, ".", "_" and "-", not "." or "..")`, r.Name)
	case r.FSType != "" && mode != ModeFS:
		return errors.New("fsType needs mode fs")
	case r.FSType != "" && !ValidFSType(r.FSType):
		return fmt.Errorf(`invalid fsType %q (lower-case letters, digits, "_" and "-")`, r.FSType)
	case mode == ModeBlock && r.Share && !readOnly:
		return errors.New("mode block with share needs readOnly")
	case readOnly && mode != ModeDax && mode != ModeFS && mode != ModeBlock:
		return errors.New("readOnly needs mode dax, fs or block")
	case !r.Share && r.Serial == "" && r.Size == "":
		return errors.New("size or serial is required")
	}
	if r.Serial != "" {
		if _, err := ParseSerial(r.Serial); err != nil {
			return err
		}
	}
	return nil
}

// validateValues returns an error if the source, the mode or the access of
// a request is invalid.
func (r *Request) validateValues() error {
	switch {
	case r.Source != "" && r.Source != SourceLocal && r.Source != SourcePool:
		return fmt.Errorf("invalid source %q (%s or %s)", r.Source, SourceLocal, SourcePool)
	case r.Mode != "" && r.Mode != ModeNUMA && r.Mode != ModeDax && r.Mode != ModeFS && r.Mode != ModeBlock:
		return fmt.Errorf("invalid mode %q (%s, %s, %s or %s)", r.Mode, ModeNUMA, ModeDax, ModeFS, ModeBlock)
	case r.Access != "" && r.Access != AccessDirect && r.Access != AccessCached:
		return fmt.Errorf("invalid access %q (%s or %s)", r.Access, AccessDirect, AccessCached)
	}
	return nil
}

// ValidName returns true if name is a valid mount point name of an fs
// lease: 1 to 255 bytes of letters, digits, ".", "_" and "-", not "." and
// not "..".
func ValidName(name string) bool {
	return portableName(name, 255) && name != "." && name != ".."
}

// portableName returns true if s has 1 to maxLen bytes of letters, digits,
// ".", "_" and "-".
func portableName(s string, maxLen int) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// validLease returns true if id is 8 lower-case hex digits, the form of a
// lease id.
func validLease(id string) bool {
	if len(id) != 8 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// KnownFSTypes are the filesystem types that the drivers know.
var KnownFSTypes = []string{FSTypeErofs, FSTypeSquashfs, FSTypeExt4, FSTypeXfs, FSTypeBtrfs, FSTypeGfs2}

// KnownFSType returns true if the drivers know the filesystem type.
func KnownFSType(name string) bool {
	return slices.Contains(KnownFSTypes, name)
}

// PersistentMode tells whether a mode is a mode of persistent memory
// (ModeBlock, ModeFS).
func PersistentMode(mode string) bool {
	return mode == ModeBlock || mode == ModeFS
}

// ValidFSType returns true if name is a valid filesystem type name: 1 to
// 32 bytes of lower-case letters, digits, "_" and "-", starting with a
// letter or a digit.
func ValidFSType(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// Memory is what a claim or a lease gives a container of one device:
//   - Key: the env key (Key, RegionKey, DRAMKey)
//   - Serial: 0x<hex> of a memdev, "" for regions and DRAM
//   - Shared: the memdev is a shared device, which several claims may map
//     at a time
//   - Mode: ModeNUMA, ModeDax, ModeBlock or ModeFS
//   - Size: bytes that the claim or lease got; of block and fs memory the
//     size of the fsdax namespace
//   - NUMANode: NUMA node of numa memory without NUMANodes
//   - NUMANodes: NUMA nodes of DRAM (cpuset syntax)
//   - Dax: /dev/daxX.Y of dax memory
//   - Path: the device node (/dev/pmemK) of block memory (of a lease: the
//     host path), the directory in the container of fs memory
//   - Access: AccessDirect or AccessCached of fs memory
//   - FSType: the filesystem type of fs memory
//   - ReadOnly: dax and block memory allows reads only, fs memory is
//     mounted read-only
type Memory struct {
	Key       string
	Serial    string
	Shared    bool
	Mode      string
	Size      uint64
	NUMANode  int
	NUMANodes string
	Dax       string
	Path      string
	Access    string
	FSType    string
	ReadOnly  bool
}

// Key returns the env key of the n-th memory of the memdev with the serial
// in one claim: the serial in upper-case hex, with "_<n>" for n > 1.
func Key(serial uint64, n int) string {
	return Nth(strings.ToUpper(strconv.FormatUint(serial, 16)), n)
}

// RegionKey returns the env key of a region: its name in upper case
// (REGION0).
func RegionKey(region string) string {
	return strings.Map(func(c rune) rune {
		switch {
		case c >= 'a' && c <= 'z':
			return c - 'a' + 'A'
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			return c
		}
		return '_'
	}, region)
}

// Nth returns the env key of the n-th memory with the key in one claim:
// key for n <= 1, else key_<n>.
func Nth(key string, n int) string {
	if n <= 1 {
		return key
	}
	return key + "_" + strconv.Itoa(n)
}

// Env returns the environment variables CXL_<KEY>_<FIELD> of a memory:
// SERIAL and SHARED of memdevs, MODE and SIZE always, NUMA_NODE or
// NUMA_NODES of numa memory, DAX and READ_ONLY of dax memory, PATH and
// READ_ONLY of block memory, PATH, ACCESS, READ_ONLY and FS_TYPE (when the
// type is known) of fs memory.
func Env(m Memory) map[string]string {
	p := "CXL_" + m.Key + "_"
	env := map[string]string{
		p + "MODE": m.Mode,
		p + "SIZE": strconv.FormatUint(m.Size, 10),
	}
	if m.Serial != "" {
		env[p+"SERIAL"] = m.Serial
		env[p+"SHARED"] = boolEnv(m.Shared)
	}
	switch {
	case m.Mode == ModeDax:
		if m.Dax != "" {
			env[p+"DAX"] = m.Dax
		}
		env[p+"READ_ONLY"] = boolEnv(m.ReadOnly)
	case m.Mode == ModeBlock:
		env[p+"PATH"] = m.Path
		env[p+"READ_ONLY"] = boolEnv(m.ReadOnly)
	case m.Mode == ModeFS:
		env[p+"PATH"] = m.Path
		env[p+"ACCESS"] = m.Access
		env[p+"READ_ONLY"] = boolEnv(m.ReadOnly)
		if m.FSType != "" {
			env[p+"FS_TYPE"] = m.FSType
		}
	case m.NUMANodes != "":
		env[p+"NUMA_NODES"] = m.NUMANodes
	default:
		env[p+"NUMA_NODE"] = strconv.Itoa(m.NUMANode)
	}
	return env
}

// EnvList returns env as sorted KEY=VALUE strings.
func EnvList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// ExecEnv returns the environment of a command that runs with a lease, as
// sorted KEY=VALUE strings: env with every CXL_<KEY>_DAX, and the
// CXL_<KEY>_PATH of every key whose CXL_<KEY>_MODE is block, replaced by
// CXL_<KEY>_FD=<fd> when fd >= 0, and CXL_LEASE=<lease>.
func ExecEnv(env map[string]string, lease string, fd int) []string {
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		if base, ok := fdVarBase(env, k); ok && fd >= 0 {
			out[base+"_FD"] = strconv.Itoa(fd)
			continue
		}
		out[k] = v
	}
	out[LeaseEnv] = lease
	return EnvList(out)
}

// fdVarBase returns CXL_<KEY> of the variable k of env whose place
// CXL_<KEY>_FD takes in the environment of a command with the fd of a
// lease: CXL_<KEY>_DAX, or CXL_<KEY>_PATH when CXL_<KEY>_MODE is block.
func fdVarBase(env map[string]string, k string) (string, bool) {
	if !strings.HasPrefix(k, "CXL_") {
		return "", false
	}
	if base, ok := strings.CutSuffix(k, "_DAX"); ok {
		return base, true
	}
	if base, ok := strings.CutSuffix(k, "_PATH"); ok && env[base+"_MODE"] == ModeBlock {
		return base, true
	}
	return "", false
}

// ParseSerial returns the value of a serial in 0x hex or decimal.
func ParseSerial(s string) (uint64, error) {
	var v uint64
	var err error
	if h, ok := strings.CutPrefix(strings.ToLower(s), "0x"); ok {
		v, err = strconv.ParseUint(h, 16, 64)
	} else {
		v, err = strconv.ParseUint(s, 10, 64)
	}
	if err != nil {
		return 0, fmt.Errorf("invalid serial %q", s)
	}
	return v, nil
}

// FormatSerial returns a serial as 0x<lower-case hex>.
func FormatSerial(v uint64) string {
	return "0x" + strconv.FormatUint(v, 16)
}

// boolEnv returns "1" for true and "0" for false.
func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
