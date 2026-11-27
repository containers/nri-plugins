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

// Observations (contract.txt 6). Decoding is lenient: unknown fields
// are ignored, a part that bash could not read is null (and listed in
// "errors"), and the oracle waits for it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Observation is one settle observation (6.1).
type Observation struct {
	Step        int                        `json:"step"`
	Poll        int                        `json:"poll"`
	Pods        []ObsPod                   `json:"pods"`
	Companions  []ObsCompanion             `json:"companions"`
	Others      []string                   `json:"others"`
	Attachments map[string][]ObsAttachment `json:"attachments"`
	Drivers     map[string]*ObsDriver      `json:"drivers"`
	Controllers map[string]string          `json:"controllers"`
	VMs         map[string]*ObsVM          `json:"vms"`
	Inspect     map[string]*ObsInspect     `json:"inspect"`
	Slices      map[string][]ObsSliceDev   `json:"slices"`     // A13: cxl.nri.io devices per node
	PoolSlices  map[string][]ObsPoolSlice  `json:"poolSlices"` // A18: cxl-pool.nri.io slices per pool
	// PDevs: the backing files of the persistent devices (A24), raw:
	// absent = not checked, null = not read
	PDevs  json.RawMessage `json:"pdevs,omitempty"`
	Errors []string        `json:"errors,omitempty"`
}

// ObsPDev is the content of a persistent device's backing files (A24).
type ObsPDev struct {
	LSA      string  `json:"lsa"` // zero or labels
	LSAHash  string  `json:"lsaHash"`
	HeadHash string  `json:"headHash"`
	PFN      bool    `json:"pfn"`
	DataOff  *uint64 `json:"dataOff"`
	Erofs    bool    `json:"erofs"`
	Token    *string `json:"token"`
	Raw      *string `json:"raw"`
}

type ObsPod struct {
	Slot          string     `json:"slot"`
	Name          string     `json:"name"`
	UID           string     `json:"uid"`
	Gen           int        `json:"gen"`
	Node          *string    `json:"node"`
	Phase         string     `json:"phase"`
	Ready         bool       `json:"ready"`
	Deleting      bool       `json:"deleting"`
	Restarts      int        `json:"restarts"`
	Waiting       *string    `json:"waiting"`
	Unschedulable bool       `json:"unschedulable"`
	Claim         *ObsClaim  `json:"claim"`
	Event         *ObsEvent  `json:"event"`  // A10: the newest Warning event
	Events        []ObsEvent `json:"events"` // A12: Warning events, newest first
	// the containers of the pod status (executor): their state
	// running, waiting or terminated; nil: not observed
	Containers []ObsPodContainer `json:"containers"`
	// Claims: every claim of the pod in the order of its spec (A39);
	// Claim is the first
	Claims []ObsClaim `json:"claims"`
	// the local claim of a dnuma pod (dualClaims; its pool claim is
	// then Claim, allocated when both are)
	localClaim *ObsClaim
}

// dualClaims returns the pool claim and the local claim of a dnuma pod
// (A39) from "claims": by the driver of their devices once allocated
// (cxl.nri.io the local one), by their order before (c0 local, c1 pool).
func (p *ObsPod) dualClaims() (pool, local *ObsClaim, ok bool) {
	if len(p.Claims) != 2 {
		return nil, nil, false
	}
	isLocal := func(c *ObsClaim) bool {
		for _, d := range c.Devices {
			if d.Driver != "" {
				return d.Driver == localDriver
			}
		}
		return false
	}
	a, b := &p.Claims[0], &p.Claims[1]
	switch {
	case isLocal(b) || (len(a.Devices) > 0 && !isLocal(a)):
		return a, b, true
	}
	return b, a, true
}

// localDriver is the DRA driver of local claims, poolDriver that of pool
// claims. claimsCDISpec is the CDI spec file of localDriver in cdiDir,
// poolCDISpec that of poolDriver.
const (
	localDriver   = "cxl.nri.io"
	poolDriver    = "cxl-pool.nri.io"
	cdiDir        = "/var/run/cdi"
	claimsCDISpec = "cxl-claims.json"
	poolCDISpec   = "cxl-pool-claims.json"
)

// claimAllocated: a claim of the pod is allocated (a dnuma pod: one of
// its two).
func (p *ObsPod) claimAllocated() bool {
	return (p.Claim != nil && p.Claim.Allocated) || (p.localClaim != nil && p.localClaim.Allocated)
}

// ObsPodContainer is a container of a pod's status.
type ObsPodContainer struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// runningContainer tells if a pod has a running container that must be
// observed in its VM (A11): by the containers' states of the pod status
// when observed (a Terminating companion whose container ended),
// else by the pod's phase.
func runningContainer(cs []ObsPodContainer, phase string) bool {
	if cs == nil {
		return phase == "Running"
	}
	for _, c := range cs {
		if c.State == "running" {
			return true
		}
	}
	return false
}

// ObsEvent is a Kubernetes event of a pod.
type ObsEvent struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type ObsClaim struct {
	Name      string           `json:"name"`
	UID       string           `json:"uid"`
	Allocated bool             `json:"allocated"`
	Devices   []ObsClaimDevice `json:"devices"`
	Attached  *bool            `json:"attached"`
	// AttachFailed: the claim's AttachFailed condition is True (A44
	// note: a pool claim of a VM whose qemu restarts, for a moment)
	AttachFailed *bool `json:"attachFailed"`
}

type ObsClaimDevice struct {
	Driver string `json:"driver"`
	Pool   string `json:"pool"`
	Device string `json:"device"`
}

type ObsCompanion struct {
	Name      string  `json:"name"`
	Node      string  `json:"node"`
	Phase     string  `json:"phase"`
	Deleting  bool    `json:"deleting"`
	Lease     string  `json:"lease"`
	Requester *string `json:"requester"`
	// the containers of its pod status (as ObsPod's)
	Containers []ObsPodContainer `json:"containers"`
}

type ObsAttachment struct {
	Device     string `json:"device"`
	Host       string `json:"host"`
	State      string `json:"state"`
	SingleHost bool   `json:"singleHost"`
	Adopted    bool   `json:"adopted"`
	Owner      string `json:"owner"`
}

type ObsDriver struct {
	Pod      string `json:"pod"`
	Ready    bool   `json:"ready"`
	Restarts int    `json:"restarts"`
}

// ObsVM is the observation of mbt-observe.py in a VM (6.3).
type ObsVM struct {
	BootID     string                 `json:"bootID"`
	Memdevs    []ObsMemdev            `json:"memdevs"`
	Regions    []ObsRegion            `json:"regions"`
	Nodes      map[string]ObsNUMANode `json:"numaNodes"`
	State      *ObsDriverState        `json:"state"`
	CDI        map[string][]string    `json:"cdi"`
	Containers []ObsContainer         `json:"containers"`
	Ports      []ObsPort              `json:"ports"`     // A10: committed decoders
	DriverLog  *ObsDriverLog          `json:"driverLog"` // A13
	Missing    json.RawMessage        `json:"missing"`   // A11 (executor): running containers not observed
	// persistent devices (A24), raw: absent = not checked (noted), null
	// = not read (wait)
	Pmem       json.RawMessage `json:"pmem,omitempty"`
	FSMounts   json.RawMessage `json:"fsMounts,omitempty"`
	LeaseBinds json.RawMessage `json:"leaseBinds,omitempty"`
	LeaseDirs  json.RawMessage `json:"leaseDirs,omitempty"`
	Loops      json.RawMessage `json:"loops,omitempty"`
	CtrMounts  json.RawMessage `json:"ctrMounts,omitempty"`
	PoolClaims json.RawMessage `json:"poolClaims,omitempty"`
	CDIPool    json.RawMessage `json:"cdiPool,omitempty"`
	Kernel     json.RawMessage `json:"kernel,omitempty"`
	KillAt     json.RawMessage `json:"killAt,omitempty"`
	// CXLDevs: the dax and pmem devices of the VM (A44 "cxlDevices"),
	// raw: absent = not checked (noted), null = not read (wait)
	CXLDevs json.RawMessage `json:"cxlDevices,omitempty"`
	Errors  []string        `json:"errors,omitempty"`
}

// ObsCXLDev is a dax device of "regions" or the block device of a
// namespace of "pmem" of a VM (A44 cxlDevices, EXEC): its name, path and
// kind (dax, pmem), the major:minor of its device node (dev, null if
// sysfs has none), its driver (dax: kmem, device_dax), its region and the
// serial of the memdev of the region's first target (serials: all).
type ObsCXLDev struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Kind    string   `json:"kind"`
	Major   *int     `json:"major"`
	Minor   *int     `json:"minor"`
	Dev     *string  `json:"dev"`
	Driver  *string  `json:"driver"`
	Region  string   `json:"region"`
	Serial  string   `json:"serial"`
	Serials []string `json:"serials"`
}

// ObsDevNode is a device node of a container (A44 "devices", EXEC: from
// /proc/PID/root/dev, the top level and one directory below, runc's
// defaults left out): its path there, type (c, b) and major:minor.
type ObsDevNode struct {
	Path  string  `json:"path"`
	Type  string  `json:"type"`
	Major *int    `json:"major"`
	Minor *int    `json:"minor"`
	Dev   *string `json:"dev"`
}

// devNumbers is major:minor of a device ("" if not known).
func devNumbers(dev *string, major, minor *int) string {
	switch {
	case dev != nil && *dev != "":
		return *dev
	case major != nil && minor != nil:
		return fmt.Sprintf("%d:%d", *major, *minor)
	}
	return ""
}

// ObsPmemRegion is a CXL region of mode pmem with its nvdimm region and
// namespaces (A24 pmem).
type ObsPmemRegion struct {
	Region     string         `json:"region"`
	Serials    []string       `json:"serials"`
	Size       *uint64        `json:"size"`
	NdRegion   *string        `json:"ndRegion"`
	Namespaces []ObsNamespace `json:"namespaces"`
}

type ObsNamespace struct {
	Name     string  `json:"name"`
	Mode     *string `json:"mode"` // memory (fsdax)
	Size     *uint64 `json:"size"`
	UUID     *string `json:"uuid"`
	Holder   *string `json:"holder"`
	Map      *string `json:"map"` // the pfn's mode: ram (map mem)
	Blockdev *string `json:"blockdev"`
}

// ObsMount is a mount of fsMounts, leaseBinds (A24) or a container's.
type ObsMount struct {
	Target    string  `json:"target"`
	Source    string  `json:"source"`
	Fstype    string  `json:"fstype"`
	Root      string  `json:"root"`
	Superopts string  `json:"superopts"`
	Vfsopts   string  `json:"vfsopts"`
	Count     int     `json:"count"`
	Backing   *string `json:"backing"` // a loop device source
	Dio       *bool   `json:"dio"`
	RO        *bool   `json:"ro"`
}

// ObsLoop is a loop device over /dev/pmem* (A24 loops).
type ObsLoop struct {
	Name    string  `json:"name"`
	Backing *string `json:"backing"`
	Dio     *bool   `json:"dio"`
	Ro      *bool   `json:"ro"`
}

// ObsLeaseDirs are the names in lease-fs/, leases/ and the pool's
// mounts/ (A24 leaseDirs).
type ObsLeaseDirs struct {
	LeaseFS map[string][]string `json:"leaseFs"`
	Leases  []string            `json:"leases"`
	Mounts  []string            `json:"mounts"`
}

// ObsCtrMounts are the erofs mounts of a container (A24 ctrMounts).
type ObsCtrMounts struct {
	Pod    string     `json:"pod"`
	Name   string     `json:"name"`
	Mounts []ObsMount `json:"mounts"`
}

// ObsPoolClaim is a prepared claim of the pool driver in state.json (A24
// poolClaims).
type ObsPoolClaim struct {
	ClaimName string            `json:"claimName"`
	Devices   []ObsPoolClaimDev `json:"devices"`
	Lend      *json.RawMessage  `json:"lend"`
}

type ObsPoolClaimDev struct {
	Serial      *string `json:"serial"`
	Mode        *string `json:"mode"`
	ReadOnly    *bool   `json:"readOnly"`
	Persistent  *bool   `json:"persistent"`
	BlockDevice *string `json:"blockDevice"`
	Access      *string `json:"access"`
	Path        *string `json:"path"`
	NodeMount   *string `json:"nodeMount"`
	LoopDevice  *string `json:"loopDevice"`
}

// ObsCDIDevice is a device of the pool driver's CDI spec (A24 cdiPool).
type ObsCDIDevice struct {
	DeviceNodes []struct {
		Path        string  `json:"path"`
		Type        *string `json:"type"`
		Permissions *string `json:"permissions"`
	} `json:"deviceNodes"`
	Mounts []struct {
		HostPath      string   `json:"hostPath"`
		ContainerPath string   `json:"containerPath"`
		Options       []string `json:"options"`
	} `json:"mounts"`
}

// ObsKernel are the kernel log records since the last observation (A24).
type ObsKernel struct {
	Lines []string `json:"lines"`
	Bad   int      `json:"bad"`
}

// rawPart decodes a raw part of an observation: ok false and absent true
// when the field is not there, ok false when it is null or invalid.
func rawPart(raw json.RawMessage, into any) (ok, absent bool, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false, true, nil
	}
	if string(raw) == "null" {
		return false, false, nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// ObsPoolSlice is a ResourceSlice of a pool's controller (A18).
type ObsPoolSlice struct {
	Name    string          `json:"name"`
	Devices json.RawMessage `json:"devices"`
	Nodes   []string        `json:"nodes"`
}

// ObsSliceDev is a device of a node's cxl.nri.io ResourceSlice (A13).
type ObsSliceDev struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
	Shared bool   `json:"shared"`
}

// ObsDriverLog is the summary of the current driver container's log
// (A13).
type ObsDriverLog struct {
	Container *string  `json:"container"`
	Panics    int      `json:"panics"`
	Errors    []string `json:"errors"`
	// Invariants: the driver's "engine invariant: " lines, the first of
	// them in InvariantErrors
	Invariants      *int     `json:"invariants"`
	InvariantErrors []string `json:"invariantErrors"`
	LendsRestored   *bool    `json:"lendsRestored"` // A15: NRI and lending up
	// Fencing: the mode of the current driver container's start line
	// (null before it, A36)
	Fencing *string `json:"fencing"`
	Sockets *bool   `json:"sockets"` // A15: both dra.sock
	// A24: lines of a persistent device not mounted or bound again
	NotRemounted *int `json:"notRemounted"`
}

// ObsPort is a CXL port with its committed decoders (A10).
type ObsPort struct {
	Port      string       `json:"port"`
	Kind      string       `json:"kind"`
	HB        *string      `json:"hb"`
	Committed int          `json:"committed"`
	Decoders  []ObsDecoder `json:"decoders"`
}

type ObsDecoder struct {
	Name   string `json:"name"`
	Region string `json:"region"`
	Start  uint64 `json:"start"`
	Size   uint64 `json:"size"`
}

type ObsMemdev struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
	Size   uint64 `json:"size"`
}

type ObsRegion struct {
	Name    string   `json:"name"`
	Serials []string `json:"serials"`
	Mode    string   `json:"mode"`
	Size    uint64   `json:"size"`
	Node    *int     `json:"node"`
	Dax     []ObsDax `json:"dax"`
}

type ObsDax struct {
	Name   string  `json:"name"`
	Size   uint64  `json:"size"`
	Driver *string `json:"driver"`
	Online uint64  `json:"online"`
}

type ObsNUMANode struct {
	MemTotal uint64 `json:"memTotal"`
}

// ObsDriverState is what the observer derives from the driver's
// state.json: the boot of the records, the UIDs of the prepared claims by
// driver, the local memdevs, the dax devices of local memdevs that no
// grant uses, the lease claims and the leases. A part that is not observed
// is nil.
type ObsDriverState struct {
	BootID       string              `json:"bootID"`
	Prepared     map[string][]string `json:"prepared"`
	LocalMemdevs []ObsLocalMemdev    `json:"localMemdevs"`
	UnusedDax    []string            `json:"unusedDax"`
	LeaseClaims  []string            `json:"leaseClaims"`
	Leases       []ObsStoredLease    `json:"leases"`
}

// ObsLocalMemdev is a local memdev of state.json: its serial (0x hex text)
// and per claim UID the number of its grants that use the memdev.
type ObsLocalMemdev struct {
	Serial string         `json:"serial"`
	Claims map[string]int `json:"claims"`
}

// ObsStoredLease is a lease of state.json: its id, mode, state (the
// protocol's name) and the name of its pod.
type ObsStoredLease struct {
	ID    string `json:"id"`
	Mode  string `json:"mode"`
	State string `json:"state"`
	Pod   string `json:"pod"`
}

type ObsContainer struct {
	Ns            string `json:"ns"`
	Pod           string `json:"pod"`
	Name          string `json:"name"`
	ID            string `json:"id"`
	Mems          string `json:"mems"`
	MemsEffective string `json:"memsEffective"`
	MemoryHigh    string `json:"memoryHigh"`
	// DevNodes: its device nodes (A44 "devices"), raw: absent = not
	// checked, null = not read (wait); DevicesFrom: proc, or spec when
	// the executor took them from the runtime spec
	DevNodes    json.RawMessage `json:"devices,omitempty"`
	DevicesFrom string          `json:"devicesFrom,omitempty"`
}

type ObsInspect struct {
	UID      string            `json:"uid"`
	Restarts int               `json:"restarts"`
	Env      map[string]string `json:"env"`
	Leases   json.RawMessage   `json:"leases"`
	// persistent kinds and plse (A24): the mount of CXL_<KEY>_PATH, the
	// mounts under /run/cxl/fs, the /dev/pmem* of the container
	Mount     *ObsMount  `json:"mount"`
	Mounts    []ObsMount `json:"mounts"`
	PmemNodes []string   `json:"pmemNodes"`
}

// ObsLease is one lease of `cxl-request list`.
type ObsLease struct {
	Lease  string `json:"lease"`
	State  string `json:"state"`
	Mode   string `json:"mode"`
	Source string `json:"source"`
}

// leases decodes the list of an inspection: an array of leases, or the
// response object {"leases": [...]}; ok is false for null or garbage.
func (in *ObsInspect) leases() ([]ObsLease, bool) {
	raw := bytes.TrimSpace(in.Leases)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var list []ObsLease
	if json.Unmarshal(raw, &list) == nil {
		return list, true
	}
	var obj struct {
		Leases *[]ObsLease `json:"leases"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.Leases == nil {
			return nil, true
		}
		return *obj.Leases, true
	}
	return nil, false
}

// DoneObs is what a non-settle step returned (6.2) with its status.
type DoneObs struct {
	Status int     `json:"status"`
	Text   *string `json:"text"`
	Stderr string  `json:"stderr"`
	Lease  *string `json:"lease"`
	// fencing-switch: the mode the drivers run now (A37)
	Fencing *string `json:"fencing"`
	// node-reboot: the boot id of the new boot (A42)
	Boot *string `json:"boot"`
}

func parseObservation(data []byte) (*Observation, error) {
	o := &Observation{}
	if err := json.Unmarshal(data, o); err != nil {
		return nil, fmt.Errorf("invalid observation: %w", err)
	}
	return o, nil
}

// parseDone parses the JSON of a done line.
func parseDone(status int, data []byte) (*DoneObs, error) {
	d := &DoneObs{}
	data = bytes.TrimSpace(data)
	if len(data) > 0 {
		if err := json.Unmarshal(data, d); err != nil {
			return nil, fmt.Errorf("invalid done JSON: %w", err)
		}
	}
	d.Status = status
	return d, nil
}

// parseObservedFile parses the file of `check --observed`: a settle
// observation (JSON or an "observe N JSON" line) or a done observation
// (a "done N STATUS JSON" line, or JSON with "status").
func parseObservedFile(data []byte) (*Observation, *DoneObs, error) {
	data = bytes.TrimSpace(data)
	switch {
	case bytes.HasPrefix(data, []byte("observe ")):
		f := strings.SplitN(string(data), " ", 3)
		if len(f) < 3 {
			return nil, nil, fmt.Errorf("invalid observe line")
		}
		o, err := parseObservation([]byte(f[2]))
		return o, nil, err
	case bytes.HasPrefix(data, []byte("done ")):
		f := strings.SplitN(string(data), " ", 4)
		if len(f) < 3 {
			return nil, nil, fmt.Errorf("invalid done line")
		}
		st, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, nil, fmt.Errorf("invalid done status %q", f[2])
		}
		js := ""
		if len(f) == 4 {
			js = f[3]
		}
		d, err := parseDone(st, []byte(js))
		return nil, d, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, nil, fmt.Errorf("invalid observation: %w", err)
	}
	if _, ok := probe["pods"]; ok {
		o, err := parseObservation(data)
		return o, nil, err
	}
	if _, ok := probe["vms"]; ok {
		o, err := parseObservation(data)
		return o, nil, err
	}
	d := &DoneObs{}
	if err := json.Unmarshal(data, d); err != nil {
		return nil, nil, fmt.Errorf("invalid done observation: %w", err)
	}
	return nil, d, nil
}

// parseNUMAList parses a cpuset list ("0-1,3").
func parseNUMAList(s string) (map[int]bool, error) {
	nodes := map[int]bool{}
	s = strings.TrimSpace(s)
	if s == "" {
		return nodes, nil
	}
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("invalid node list %q", s)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("invalid node list %q", s)
			}
		}
		for n := a; n <= b; n++ {
			nodes[n] = true
		}
	}
	return nodes, nil
}
