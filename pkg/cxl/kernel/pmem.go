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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// NdRegion, Namespace and BlockDevice: the persistent memory chain of an
// enabled pmem region: the nd region regionN on the nd bus (the id of the
// CXL region) -> its namespaces namespaceN.M (defined by the labels of the
// memdev) -> the holder of an fsdax namespace (pfnN.M) -> the block device
// /dev/pmemK.
//
// Identity: names are given out anew at every probe and boot; a block
// device is the same one only with the same namespace, name and device
// numbers. The labels outlive the region: a reader finds the namespace
// that a writer created.
// States: NdRegionState ABSENT (it appears asynchronously after the
// creation or the enabling of the region) | PROBING (no driver, or fewer
// namespaces registered than init_namespaces) | SETTLED; NamespaceState
// PROBING (about eight partial sysfs states: no size, a holder without a
// driver, a disk without dev, a pfn mode unreadable) | SEED (size 0) | RAW
// | FSDAX | SECTOR | DEVDAX. Transitions: NdRegionTransitions,
// NamespaceTransitions.
// Invariants: only CreateFsdaxNamespace writes labels, and it never runs
// while the nd region probes or when an fsdax namespace exists (it destroys
// what the namespace it reconfigures had). A release never touches labels
// or namespaces.
// Waits: Namespaces (until settled), WaitNdRegion (until it appears).

// Namespace modes and fsdax maps, as ndctl names them.
const (
	NamespaceFsdax  = "fsdax"
	NamespaceRaw    = "raw"
	NamespaceDevdax = "devdax"
	NamespaceSector = "sector"
	// MapMem keeps the struct pages of an fsdax namespace in system RAM.
	MapMem = "mem"
	// MapDev keeps them in the namespace itself.
	MapDev = "dev"
)

// NdRegionState is the state of an nd region.
type NdRegionState int

const (
	// NdAbsent: the nd region does not exist.
	NdAbsent NdRegionState = iota
	// NdProbing: the nd region or one of its namespaces is probing.
	NdProbing
	// NdSettled: the nd region and its namespaces have been probed.
	NdSettled
)

var ndRegionStateNames = []string{"ABSENT", "PROBING", "SETTLED"}

func (s NdRegionState) String() string { return stateText(ndRegionStateNames, int(s)) }

// MarshalText returns the name of the state.
func (s NdRegionState) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// NdRegionTransitions are the changes of the state of an nd region.
var NdRegionTransitions = fsm.Table[NdRegionState]{
	Object:   "NdRegion",
	Field:    "State",
	Observed: true,
	States:   fsm.List(NdAbsent, NdProbing, NdSettled),
	Rows: []fsm.Transition[NdRegionState]{
		{From: fsm.List(NdAbsent), To: NdProbing, When: "CreateRegion (pmem), EnableRegion", Effects: "the namespaces of the labels are registered"},
		{From: fsm.List(NdProbing), To: NdSettled, When: "the probe of the region and of its namespaces finished"},
		{From: fsm.List(NdAbsent), To: NdSettled, When: "CreateRegion (pmem), observed after the probe finished"},
		{From: fsm.List(NdSettled), To: NdProbing, When: "CreateFsdaxNamespace (ndctl create-namespace)",
			Guard: "SETTLED, no fsdax namespace", Effects: "the labels written; a pfn device and a block device probed"},
		{From: fsm.List(NdProbing), To: NdAbsent, When: "DisableRegion, DestroyRegion, hot-remove of its memdev"},
		{From: fsm.List(NdSettled), To: NdAbsent, When: "DisableRegion, DestroyRegion, hot-remove of its memdev", Effects: "labels and data stay"},
	},
}

// NamespaceState is the state of a namespace.
type NamespaceState int

const (
	// NsAbsent: the namespace does not exist.
	NsAbsent NamespaceState = iota
	// NsProbing: the namespace or its holder is probing.
	NsProbing
	// NsSeed: the seed of new namespaces (size 0).
	NsSeed
	// NsRaw: a namespace without a holder, with a block device of its own.
	NsRaw
	// NsFsdax: held by a pfn device, with a block device.
	NsFsdax
	// NsSector: held by a btt device, with a block device.
	NsSector
	// NsDevdax: held by a dax device.
	NsDevdax
)

var namespaceStateNames = []string{"ABSENT", "PROBING", "SEED", "RAW", "FSDAX", "SECTOR", "DEVDAX"}

func (s NamespaceState) String() string { return stateText(namespaceStateNames, int(s)) }

// MarshalText returns the name of the state.
func (s NamespaceState) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// NamespaceTransitions are the changes of the state of a namespace.
var NamespaceTransitions = fsm.Table[NamespaceState]{
	Object:   "Namespace",
	Field:    "State",
	Observed: true,
	States:   fsm.List(NsAbsent, NsProbing, NsSeed, NsRaw, NsFsdax, NsSector, NsDevdax),
	Rows: []fsm.Transition[NamespaceState]{
		{From: fsm.List(NsAbsent), To: NsProbing, When: "the probe of the nd region registers it (from the labels, or the seed)"},
		{From: fsm.List(NsProbing), To: NsSeed, When: "its probe finished without labels"},
		{From: fsm.List(NsProbing), To: NsRaw, When: "its probe finished without a holder"},
		{From: fsm.List(NsProbing), To: NsFsdax, When: "its pfn device probed with its block device and mode"},
		{From: fsm.List(NsProbing), To: NsSector, When: "its btt device probed with its block device"},
		{From: fsm.List(NsProbing), To: NsDevdax, When: "its dax device probed"},
		{From: fsm.List(NsSeed), To: NsProbing, When: "CreateFsdaxNamespace", Effects: "labels written"},
		{From: fsm.List(NsRaw), To: NsProbing, When: "CreateFsdaxNamespace of the existing namespace", Effects: "its content destroyed"},
		{From: fsm.List(NsProbing), To: NsAbsent, When: "DisableRegion, DestroyRegion"},
		{From: fsm.List(NsSeed), To: NsAbsent, When: "DisableRegion, DestroyRegion"},
		{From: fsm.List(NsRaw), To: NsAbsent, When: "DisableRegion, DestroyRegion"},
		{From: fsm.List(NsFsdax), To: NsAbsent, When: "DisableRegion, DestroyRegion", Effects: "its labels stay"},
		{From: fsm.List(NsSector), To: NsAbsent, When: "DisableRegion, DestroyRegion"},
		{From: fsm.List(NsDevdax), To: NsAbsent, When: "DisableRegion, DestroyRegion"},
		// changes that one call makes between two observations
		{From: fsm.List(NsSeed), To: NsFsdax, When: "CreateFsdaxNamespace (it returns with the block device), observed across it"},
		{From: fsm.List(NsRaw), To: NsFsdax, When: "CreateFsdaxNamespace of the existing namespace, observed across it", Effects: "its content destroyed"},
		{From: fsm.List(NsAbsent), To: NsSeed, When: "the probe of a new nd region without labels, observed after it finished (Namespaces)"},
		{From: fsm.List(NsAbsent), To: NsRaw, When: "the probe of a new nd region, observed after it finished"},
		{From: fsm.List(NsAbsent), To: NsFsdax, When: "the probe of a new nd region with the labels of an fsdax namespace, observed after it finished"},
		{From: fsm.List(NsAbsent), To: NsSector, When: "the probe of a new nd region, observed after it finished"},
		{From: fsm.List(NsAbsent), To: NsDevdax, When: "the probe of a new nd region, observed after it finished"},
	},
}

// Namespace is the observation of a namespace of an nd region:
//   - Name: namespaceN.M
//   - State: its probe state
//   - Why: why its probe has not finished, "" when it has
//   - Mode: NamespaceFsdax, NamespaceRaw, NamespaceDevdax or NamespaceSector
//   - Map: MapMem or MapDev of fsdax namespaces
//   - Size: bytes of its block device, of the namespace without one
//   - Holder: its pfn, btt or dax device, "" for none
//   - BlockDevice: /dev/pmemK of fsdax, raw and sector namespaces
//   - Major, Minor: device numbers of the block device
type Namespace struct {
	Name        string         `json:"name"`
	State       NamespaceState `json:"state"`
	Why         string         `json:"why,omitempty"`
	Mode        string         `json:"mode,omitempty"`
	Map         string         `json:"map,omitempty"`
	Size        uint64         `json:"size"`
	Holder      string         `json:"holder,omitempty"`
	BlockDevice string         `json:"blockDevice,omitempty"`
	Major       int            `json:"major,omitempty"`
	Minor       int            `json:"minor,omitempty"`
}

// NdRegion is the observation of an nd region.
type NdRegion struct {
	// Name is regionN on the nd bus.
	Name string `json:"name"`
	// Region is the CXL region of the same id.
	Region string        `json:"region"`
	State  NdRegionState `json:"state"`
	// Why is why the region or a namespace is probing, "" when settled.
	Why string `json:"why,omitempty"`
	// Namespaces are its namespaces in natural order, the seed too.
	Namespaces []Namespace `json:"namespaces,omitempty"`
}

// FsdaxNamespace returns the first fsdax namespace with map mem and a block
// device, nil if there is none.
func FsdaxNamespace(namespaces []Namespace) *Namespace {
	for i := range namespaces {
		if ns := &namespaces[i]; ns.Mode == NamespaceFsdax && ns.Map == MapMem && ns.BlockDevice != "" {
			return ns
		}
	}
	return nil
}

// NdRegionOf returns the nd region of a CXL region of persistent memory:
// the nd region with the id of the CXL region.
func (k *Kernel) NdRegionOf(region string) (string, error) {
	id, ok := strings.CutPrefix(region, "region")
	if _, err := strconv.Atoi(id); !ok || err != nil {
		return "", fmt.Errorf("invalid region name %q", region)
	}
	nd := "region" + id
	if _, err := os.Stat(k.ndDev(nd)); err != nil {
		return "", errorf(ErrNotFound, "%s has no nvdimm region: %w", region, err)
	}
	return nd, nil
}

// WaitNdRegion waits until the nd region of a CXL region of persistent
// memory exists (the kernel makes it asynchronously) and returns its name.
func (k *Kernel) WaitNdRegion(ctx context.Context, region string) (string, error) {
	var nd string
	err := k.poll(ctx, func() (bool, error) {
		var err error
		nd, err = k.NdRegionOf(region)
		return err == nil, nil
	})
	if err != nil {
		return "", fmt.Errorf("%s: no nvdimm region appeared: %w", region, err)
	}
	return nd, nil
}

// blockDeviceOf returns the name of the block device (pmemK) of a device
// directory, "" if it has none.
func blockDeviceOf(dir string) string {
	m, _ := filepath.Glob(filepath.Join(dir, "block", "*"))
	slices.Sort(m)
	if len(m) == 0 {
		return ""
	}
	return filepath.Base(m[0])
}

// probeNamespace returns a namespace of an nd region; its Why tells why
// its probing has not finished, "" if it has: it has no memory (a seed), a
// block device of its own (raw), or a holder (pfn, btt or dax device) with
// a driver and, for pfn and btt, a block device; a pfn device also has a
// mode that is ram or pmem, read last. An attribute that cannot be read is
// a probe that has not finished.
func (k *Kernel) probeNamespace(name string) (Namespace, error) {
	ns, why, err := k.probeNamespaceWhy(name)
	ns.Why = why
	switch {
	case err != nil:
	case why != "":
		ns.State = NsProbing
	case ns.Size == 0:
		ns.State = NsSeed
	case ns.Mode == NamespaceRaw:
		ns.State = NsRaw
	case ns.Mode == NamespaceFsdax:
		ns.State = NsFsdax
	case ns.Mode == NamespaceSector:
		ns.State = NsSector
	case ns.Mode == NamespaceDevdax:
		ns.State = NsDevdax
	}
	return ns, err
}

func (k *Kernel) probeNamespaceWhy(name string) (Namespace, string, error) {
	dir := k.ndDev(name)
	ns := Namespace{Name: name}
	sizeFile := filepath.Join(dir, "size")
	size, err := readUint(sizeFile)
	if errors.Is(err, os.ErrNotExist) {
		return ns, "no " + sizeFile, nil // being added
	}
	if err != nil {
		return ns, "", err
	}
	ns.Size = size
	if size == 0 {
		return ns, "", nil // the seed of new namespaces
	}
	// An attribute that is not there yet is a probe in progress.
	holderFile := filepath.Join(dir, "holder")
	holder, err := readTrim(holderFile)
	if err != nil {
		return ns, fmt.Sprintf("cannot read %s: %v", holderFile, err), nil
	}
	ns.Holder = holder
	blockDir := dir
	switch {
	case holder == "":
		ns.Mode = NamespaceRaw
	case strings.HasPrefix(holder, "pfn"):
		ns.Mode = NamespaceFsdax
		blockDir = k.ndDev(holder)
	case strings.HasPrefix(holder, "btt"):
		ns.Mode = NamespaceSector
		blockDir = k.ndDev(holder)
	case strings.HasPrefix(holder, "dax"):
		ns.Mode = NamespaceDevdax
		if d := k.ndDev(holder); !hasDriver(d) {
			return ns, "no " + filepath.Join(d, "driver"), nil
		}
		return ns, "", nil
	default:
		return ns, "", fmt.Errorf("%s: unknown holder %q", name, holder)
	}
	blk := blockDeviceOf(blockDir)
	if blk == "" {
		return ns, "no block device in " + filepath.Join(blockDir, "block"), nil
	}
	if holder != "" && !hasDriver(blockDir) {
		return ns, "no " + filepath.Join(blockDir, "driver"), nil
	}
	// The driver link of a pfn or btt device appears before its probe
	// adds the disk, and the disk directory before its class link and its
	// dev and size attributes: a missing one is a probe in progress.
	major, minor, bytes, err := k.BlockDeviceInfo(blk)
	if errors.Is(err, os.ErrNotExist) {
		if pe := (*os.PathError)(nil); errors.As(err, &pe) {
			return ns, "no " + pe.Path, nil
		}
		return ns, err.Error(), nil
	}
	if err != nil {
		return ns, "", err
	}
	ns.BlockDevice = filepath.Join(k.devRoot(), blk)
	ns.Major, ns.Minor, ns.Size = major, minor, bytes
	// The mode of a pfn device is read last, from a device known to be
	// probed: read before, it could be missing (the holder of the
	// namespace is set before the pfn device is registered) while the
	// probe finishes before the checks above. A mode that cannot be read
	// is not settled either: an fsdax namespace with an unknown map would
	// look like no fsdax namespace with map mem (FsdaxNamespace), and a
	// writer's namespace could be reconfigured.
	if ns.Mode == NamespaceFsdax {
		modeFile := filepath.Join(blockDir, "mode")
		switch m, err := readTrim(modeFile); {
		case err != nil:
			return ns, fmt.Sprintf("cannot read %s: %v", modeFile, err), nil
		case m == "ram":
			ns.Map = MapMem
		case m == "pmem":
			ns.Map = MapDev
		default:
			return ns, fmt.Sprintf("unknown mode %q in %s", m, modeFile), nil
		}
	}
	return ns, "", nil
}

// BlockDeviceInfo returns the device numbers and the size in bytes of a
// block device (/sys/class/block/<name>).
func (k *Kernel) BlockDeviceInfo(name string) (major, minor int, size uint64, err error) {
	v, err := readTrim(k.blockDev(name, "dev"))
	if err != nil {
		return 0, 0, 0, err
	}
	if major, minor, err = parseDevNumbers(name, v); err != nil {
		return 0, 0, 0, err
	}
	sectors, err := readUint(k.blockDev(name, "size"))
	if err != nil {
		return 0, 0, 0, err
	}
	return major, minor, sectors * 512, nil
}

// NdRegion returns the observation of an nd region and its namespaces.
func (k *Kernel) NdRegion(ndregion string) (NdRegion, error) {
	r := NdRegion{Name: ndregion, Region: ndregion, State: NdAbsent}
	if !exists(k.ndDev(ndregion)) {
		return r, nil
	}
	nss, why, err := k.probeNamespaces(ndregion)
	if err != nil {
		return r, err
	}
	r.Namespaces, r.Why, r.State = nss, why, NdSettled
	if why != "" {
		r.State = NdProbing
	}
	return r, nil
}

// probeNamespaces returns the namespaces of an nd region, the seed too, and
// why the region or a namespace has not finished probing, "" if all have.
func (k *Kernel) probeNamespaces(ndregion string) ([]Namespace, string, error) {
	dir := k.ndDev(ndregion)
	if !hasDriver(dir) {
		return nil, "no " + filepath.Join(dir, "driver"), nil
	}
	// The kernel registers the namespaces that it found at the probe of
	// the region asynchronously; init_namespaces counts them.
	initFile := filepath.Join(dir, "init_namespaces")
	v, err := readTrim(initFile)
	if err != nil {
		return nil, fmt.Sprintf("cannot read %s: %v", initFile, err), nil
	}
	a, _, _ := strings.Cut(v, "/")
	active, _ := strconv.Atoi(a)
	id := strings.TrimPrefix(ndregion, "region")
	m, _ := filepath.Glob(filepath.Join(dir, "namespace"+id+".*"))
	names := make([]string, 0, len(m))
	for _, p := range m {
		names = append(names, filepath.Base(p))
	}
	sortNat(names)
	why := ""
	if len(names) < active {
		why = fmt.Sprintf("%d of %d namespaces registered", len(names), active)
	}
	var out []Namespace
	for _, name := range names {
		ns, err := k.probeNamespace(name)
		if err != nil {
			return nil, "", err
		}
		if why == "" && ns.Why != "" {
			why = name + ": " + ns.Why
		}
		out = append(out, ns)
	}
	return out, why, nil
}

// withMemory returns the namespaces that have memory.
func withMemory(nss []Namespace) []Namespace {
	var out []Namespace
	for _, ns := range nss {
		if ns.Size > 0 {
			out = append(out, ns)
		}
	}
	return out
}

// ProbeNamespaces returns the namespaces with memory of an nd region, and
// why the region or a namespace has not finished probing, "" if all have.
func (k *Kernel) ProbeNamespaces(ndregion string) ([]Namespace, string, error) {
	nss, why, err := k.probeNamespaces(ndregion)
	return withMemory(nss), why, err
}

// Namespaces returns the namespaces with memory of an nd region, after
// waiting until the region and its namespaces have finished probing (an
// error when the context ends first).
func (k *Kernel) Namespaces(ctx context.Context, ndregion string) ([]Namespace, error) {
	var out []Namespace
	var why string
	err := k.poll(ctx, func() (bool, error) {
		var err error
		out, why, err = k.ProbeNamespaces(ndregion)
		return why == "", err
	})
	if err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("%s: namespaces still probing (%s): %w", ndregion, why, err)
	}
	return out, err
}

var namespaceNameRe = regexp.MustCompile(`"dev"\s*:\s*"(namespace\d+\.\d+)"`)

// CreateFsdaxNamespace creates an fsdax namespace with map mem in an nd
// region, or reconfigures the namespace existing ("" for a new one) as
// such, with ndctl create-namespace, and returns it when its block device
// is there (StartFsdaxNamespace, then WaitFsdaxNamespace).
func (k *Kernel) CreateFsdaxNamespace(ctx context.Context, ndregion, existing string) (*Namespace, error) {
	name, err := k.StartFsdaxNamespace(ctx, ndregion, existing)
	if err != nil {
		return nil, err
	}
	return k.WaitFsdaxNamespace(ctx, ndregion, name)
}

// StartFsdaxNamespace runs ndctl create-namespace for an fsdax namespace
// with map mem in an nd region, of the namespace existing ("" for a new
// one), and returns the name of the namespace; its block device appears
// asynchronously (WaitFsdaxNamespace). This writes the labels of the
// region, and destroys what the reconfigured namespace had: it is refused
// unless the region has finished probing and has no fsdax namespace, which
// a writer may have formatted.
func (k *Kernel) StartFsdaxNamespace(ctx context.Context, ndregion, existing string) (string, error) {
	nss, why, err := k.ProbeNamespaces(ndregion)
	if err != nil {
		return "", err
	}
	if why != "" {
		return "", fmt.Errorf("%s: not creating an fsdax namespace: the namespaces are still probing (%s)", ndregion, why)
	}
	for _, ns := range nss {
		if ns.Mode == NamespaceFsdax {
			return "", fmt.Errorf("%s: not creating an fsdax namespace: %s is one already (map %q)", ndregion, ns.Name, ns.Map)
		}
	}
	args := []string{"create-namespace", "-f", "-r", ndregion, "-m", NamespaceFsdax, "-M", MapMem}
	if existing != "" {
		args = append(args, "-e", existing)
	}
	out, err := k.exec(ctx, "ndctl", args...)
	if err != nil {
		return "", err
	}
	m := namespaceNameRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("ndctl create-namespace did not report a namespace: %s", out)
	}
	return m[1], nil
}

// WaitFsdaxNamespace waits until the namespace name of an nd region is an
// fsdax namespace with its block device, and returns it.
func (k *Kernel) WaitFsdaxNamespace(ctx context.Context, ndregion, name string) (*Namespace, error) {
	var found *Namespace
	var nsErr error
	err := k.poll(ctx, func() (bool, error) {
		nss, err := k.Namespaces(ctx, ndregion)
		if err != nil {
			nsErr = err
			return false, err
		}
		for i := range nss {
			if ns := &nss[i]; ns.Name == name && ns.Mode == NamespaceFsdax && ns.BlockDevice != "" {
				found = ns
				return true, nil
			}
		}
		return false, nil
	})
	switch {
	case nsErr != nil:
		return nil, nsErr
	case err != nil:
		return nil, fmt.Errorf("%s: %s has no fsdax block device: %w", ndregion, name, err)
	}
	k.logf("%s: created %s (fsdax, map %s, %s, %d bytes)", ndregion, found.Name, found.Map, found.BlockDevice, found.Size)
	return found, nil
}
