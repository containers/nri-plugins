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

package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// The persistent chain of a grant of mode block or fs: the pmem region of
// the memdev -> its nd region -> the fsdax namespace (map mem) -> its pfn ->
// the block device /dev/pmemK -> its read-only flag -> the superblock of
// its filesystem -> one node mount per memdev and access that the grants of
// the node share (<mounts dir of the source>/<serial hex>-<access>). The
// state of a superblock (read-write while a grant of the memdev writes) and
// the read-only flags of the block devices are derived from the grants of
// the memdev, never stored apart from them.

const (
	// fsMountFlags are the flags of the node mounts of read-only
	// filesystem types (mount(2)).
	fsMountFlags = syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV
	// daxAlways is the super option of a DAX mount.
	daxAlways = "dax=always"
	// maxUnbinds bounds the unmounts of one lease bind (stacked mounts).
	maxUnbinds = 8
)

// pmemGrant is a grant of mode block or fs of a persistent memdev, as the
// rules of one node see it: the engine's one view of such a grant, also of
// the grant being prepared before its records have a node mount.
type pmemGrant struct {
	ref    state.GrantRef
	source state.Source
	serial state.Serial
	mode   state.Mode
	// readOnly is the effective read-only setting: a grant with readOnly
	// false writes the device.
	readOnly bool
	// access and fsType are those of the node mount of mode fs.
	access string
	fsType string
	// blockDevice is the block device as prepared, nodeMount and loop the
	// node mount of mode fs and the loop device of a cached one.
	blockDevice string
	nodeMount   string
	loop        string
}

// writer tells whether the grant writes the device.
func (u *pmemGrant) writer() bool {
	return !u.readOnly
}

// pmemGrantOf returns the pmemGrant of a grant.
func pmemGrantOf(r *state.Records, g *state.Grant) pmemGrant {
	u := pmemGrant{ref: g.Ref, source: g.Asked.Source, serial: g.Serial, mode: g.Asked.Mode, readOnly: g.Asked.ReadOnly}
	if m := r.MountOf(g); m != nil {
		u.access, u.fsType, u.nodeMount, u.loop = string(m.Access), m.FSType, m.Path, m.Loop
	}
	if g.Got.BlockDevice != nil {
		u.blockDevice = g.Got.BlockDevice.Name
	}
	return u
}

// mountPathOf returns the path of the node mount that a grant uses, "" if
// none.
func mountPathOf(r *state.Records, g *state.Grant) string {
	if m := r.MountOf(g); m != nil {
		return m.Path
	}
	return ""
}

// prepared tells whether a grant counts on the node: realised, or realised
// before a start that did not verify it.
func prepared(g *state.Grant) bool {
	return g.State == state.GrantRealised || g.State == state.GrantUnrestored
}

// pmemGrants returns the prepared grants of the memdev with the serial
// other than except, and the grants of it whose release in an Unprepare
// failed (RELEASING, of an UNPREPARING claim): they keep what
// they still have until kubelet's retry of the Unprepare, one of mode fs
// only while its node mount lists it. The grants of claims of another boot
// (PREPARED_STALE) have nothing of this boot.
func pmemGrants(r *state.Records, serial state.Serial, except ...state.GrantRef) []pmemGrant {
	var out []pmemGrant
	for i := range r.Grants {
		g := &r.Grants[i]
		if g.Serial != serial || (!prepared(g) && !unpreparing(r, g)) || slices.Contains(except, g.Ref) ||
			(g.Asked.Mode != state.ModeBlock && g.Asked.Mode != state.ModeFS) || stale(r, g) ||
			unpreparing(r, g) && g.Asked.Mode == state.ModeFS && !mounted(r, g) {
			continue
		}
		out = append(out, pmemGrantOf(r, g))
	}
	return out
}

// mounted tells whether the node mount of a grant lists it as a user.
func mounted(r *state.Records, g *state.Grant) bool {
	m := r.MountOf(g)
	return m != nil && slices.Contains(m.Users, g.Ref)
}

// stale tells whether a grant is of a claim of another boot
// (PREPARED_STALE).
func stale(r *state.Records, g *state.Grant) bool {
	p := r.PreparationOf(g.Ref)
	return p != nil && p.State == state.PreparationStale
}

// unpreparing tells whether a grant is one whose release in an Unprepare
// failed: RELEASING, of an UNPREPARING preparation.
func unpreparing(r *state.Records, g *state.Grant) bool {
	if g.State != state.GrantReleasing {
		return false
	}
	p := r.PreparationOf(g.Ref)
	return p != nil && p.State == state.PreparationUnpreparing
}

// firstWriter returns the first grant that writes, nil if none does.
func firstWriter(grants []pmemGrant) *pmemGrant {
	for i := range grants {
		if grants[i].writer() {
			return &grants[i]
		}
	}
	return nil
}

// fsAccesses returns the accesses of the grants of mode fs, sorted.
func fsAccesses(grants []pmemGrant) []string {
	var out []string
	for _, u := range grants {
		if u.mode == state.ModeFS && !slices.Contains(out, u.access) {
			out = append(out, u.access)
		}
	}
	slices.Sort(out)
	return out
}

// nodeMounts returns the node mounts of the grants, sorted.
func nodeMounts(grants []pmemGrant) []string {
	var out []string
	for _, u := range grants {
		if u.nodeMount != "" && !slices.Contains(out, u.nodeMount) {
			out = append(out, u.nodeMount)
		}
	}
	slices.Sort(out)
	return out
}

// firstBlockDevice returns the first block device of the grants, "" if none.
func firstBlockDevice(grants []pmemGrant) string {
	for _, u := range grants {
		if u.blockDevice != "" {
			return u.blockDevice
		}
	}
	return ""
}

// devRoot returns the directory of the device nodes.
func (e *Engine) devRoot() string {
	if e.k.DevRoot != "" {
		return e.k.DevRoot
	}
	return "/dev"
}

// loopOf returns the loop device that a mount is of, "" if none.
func (e *Engine) loopOf(mi *kernel.MountInfo) string {
	if strings.HasPrefix(mi.Source, e.devRoot()+"/loop") {
		return mi.Source
	}
	return ""
}

// nodeMountPath returns the node mount of the memdev with the serial and
// the access for a source: <mounts dir>/<serial hex>-<access>.
func (e *Engine) nodeMountPath(source state.Source, serial state.Serial, access string) string {
	return filepath.Join(e.opts.MountsDirs[source], fmt.Sprintf("%x-%s", uint64(serial), access))
}

// realisePersistent realises a grant of mode block or fs: the pmem region
// of the memdev with its nd region, the fsdax namespace (which only a
// writable block grant creates), then the grant on the device next to the
// other grants on the node, and the lease bind of a companion grant.
func (e *Engine) realisePersistent(ctx context.Context, req *GrantRequest, memdev string) error {
	region, ns, err := e.pmemChain(ctx, req, memdev)
	if err != nil {
		return err
	}
	others := pmemGrants(e.records(), req.Serial, req.Ref)
	u, err := e.preparePmemGrant(ctx, req, ns.BlockDevice, others)
	if err != nil {
		return err
	}
	got := state.Got{Region: state.RegionRef(region), Namespace: ns.Name, BlockDevice: blockDevice(ns), Size: ns.Size,
		Mount: state.Access(u.access)}
	if err := e.recordNodeMount(ctx, req, region, u, u.writer() || firstWriter(others) != nil); err != nil {
		return err
	}
	if err := e.bindCompanion(ctx, req, u, &got); err != nil {
		return err
	}
	return e.realised(ctx, req, got, state.DevNumbers{})
}

// pmemChain returns the pmem region of the memdev of a grant and its fsdax
// namespace: the region made or reused, its nd region and namespaces waited
// for, the namespace made for a writable block grant; the region's Pmem
// and the grant's namespace and block device recorded.
func (e *Engine) pmemChain(ctx context.Context, req *GrantRequest, memdev string) (string, *kernel.Namespace, error) {
	ok, err := e.k.Persistent(memdev)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, fmt.Errorf("memdev %s: %s has no persistent memory", req.Serial.String(), memdev)
	}
	wctx, cancel := context.WithTimeout(ctx, e.settle(req.Asked.Source))
	defer cancel()
	if reg := ownRegion(e.records(), req.Serial, state.RegionRAM); reg != nil {
		return "", nil, fmt.Errorf("%s: %s is not a region of persistent memory", memdev, reg.Ref())
	}
	reg := ownRegion(e.records(), req.Serial, state.RegionPmem)
	created, again := reg == nil, reg != nil && !reg.SetUp
	start := time.Now()
	region, err := e.ensureRegion(wctx, req, memdev, state.RegionPmem, 0)
	if err != nil {
		return "", nil, err
	}
	nd, err := e.k.WaitNdRegion(wctx, region)
	if err != nil {
		return "", nil, err
	}
	nss, err := e.k.Namespaces(wctx, nd)
	if err != nil {
		return "", nil, err
	}
	if req.Asked.Source == state.SourceLocal {
		switch {
		case created:
			klog.Infof("engine: local memdev %s (%s): created %s (pmem, nvdimm %s) in %s", req.Serial.String(), memdev, region, nd, since(start))
		case again:
			klog.Infof("engine: local memdev %s (%s): set up kept %s (pmem, nvdimm %s)", req.Serial.String(), memdev, region, nd)
		}
	}
	ns, err := e.pmemNamespace(wctx, req, nd, nss)
	if err != nil {
		return "", nil, err
	}
	dev := blockDevice(ns)
	return region, ns, e.commit(ctx, func(r *state.Records) error {
		reg := r.Region(state.RegionRef(region))
		reg.Pmem = &state.Pmem{NdRegion: nd, Namespace: ns.Name, BlockDevice: dev}
		g := r.Grant(req.Ref)
		g.Got.Region, g.Got.Namespace, g.Got.BlockDevice = state.RegionRef(region), ns.Name, dev
		if !reg.SetUp {
			return fsm.Apply(&state.RegionSetUpTable, &reg.SetUp, true, state.TriggerPrepare)
		}
		return nil
	})
}

// blockDevice returns the block device of a namespace as the records hold
// it.
func blockDevice(ns *kernel.Namespace) *state.BlockDevice {
	return &state.BlockDevice{Name: ns.BlockDevice, Dev: state.DevNumbers{Major: ns.Major, Minor: ns.Minor}}
}

// bindCompanion binds the lease of a companion grant of mode fs at its
// target and sets what got records of the bind and the lend.
func (e *Engine) bindCompanion(ctx context.Context, req *GrantRequest, u pmemGrant, got *state.Got) error {
	if req.BindTarget == "" || req.Asked.Mode != state.ModeFS {
		return nil
	}
	if err := e.bindGrant(ctx, req, u); err != nil {
		return &BindError{Err: err}
	}
	got.Bind = &state.Bind{Target: req.BindTarget, State: state.BindAbsent}
	if err := fsm.Apply(&state.BindTable, &got.Bind.State, state.BindBound, state.TriggerPrepare); err != nil {
		return err
	}
	if req.Lend != nil {
		lend := *req.Lend
		got.Lend = &lend
	}
	return nil
}

// pmemNamespace returns the fsdax namespace (map mem) of the nd region for
// a grant: the existing one, else a new one for a writable block grant,
// whose intent names the nd region first (ndctl under the engine lock, the
// wait for its block device outside it); every other grant needs one that
// a writer made.
func (e *Engine) pmemNamespace(ctx context.Context, req *GrantRequest, nd string, nss []kernel.Namespace) (*kernel.Namespace, error) {
	if ns := kernel.FsdaxNamespace(nss); ns != nil {
		return ns, nil
	}
	if req.Asked.Mode == state.ModeFS || req.Asked.ReadOnly {
		return nil, fmt.Errorf("memdev %s: no fsdax namespace in %s: has a writer written the device?", req.Serial.String(), nd)
	}
	existing := ""
	if len(nss) > 0 {
		existing = nss[0].Name
	}
	if err := e.commit(ctx, func(r *state.Records) error {
		r.Grant(req.Ref).Intent.NdRegion = nd
		return nil
	}); err != nil {
		return nil, err
	}
	var name string
	if err := e.locked(func() error {
		var err error
		name, err = e.k.StartFsdaxNamespace(ctx, nd, existing)
		return err
	}); err != nil {
		return nil, err
	}
	return e.k.WaitFsdaxNamespace(ctx, nd, name)
}

// preparePmemGrant returns a grant of the block device dev prepared next
// to others, the other grants of the device on the node: mode block
// gets the read-only flag of the block device, mode fs the node mount of
// its type and access, mounted, reused or made read-write. On error nothing
// of the device changed but what cannot be undone, which is logged.
func (e *Engine) preparePmemGrant(ctx context.Context, req *GrantRequest, dev string, others []pmemGrant) (pmemGrant, error) {
	u := pmemGrant{ref: req.Ref, source: req.Asked.Source, serial: req.Serial, mode: req.Asked.Mode, readOnly: req.Asked.ReadOnly, blockDevice: dev}
	s := req.Serial.String()
	blockWriter := slices.ContainsFunc(others, func(o pmemGrant) bool { return o.mode == state.ModeBlock && o.writer() })
	if blockWriter || (req.Asked.Mode == state.ModeBlock && !req.Asked.ReadOnly && len(others) > 0) {
		return u, fmt.Errorf("memdev %s: mode block read-write needs the device alone on this node", s)
	}
	if req.Asked.Mode != state.ModeFS {
		if u.writer() {
			// others is empty: every superblock of the device is foreign
			if err := e.checkUnexplained(req.Serial, dev, nil); err != nil {
				return u, err
			}
		}
		writer := u.writer() || firstWriter(others) != nil
		return u, e.setReadOnly(req.Asked.Source, req.Serial, dev, !writer)
	}
	return e.prepareFs(ctx, req, u, others)
}

// prepareFs prepares the grant u of mode fs (preparePmemGrant): the type
// of the filesystem, the rules of the type and of one node, then the node
// mount of the access mounted, reused, made read-write or read-only.
func (e *Engine) prepareFs(ctx context.Context, req *GrantRequest, u pmemGrant, others []pmemGrant) (pmemGrant, error) {
	source, serial, dev := req.Asked.Source, req.Serial, u.blockDevice
	sig := &signature{e: e, serial: serial.String(), dev: dev}
	typ, err := e.fsTypeOf(req, dev, sig)
	if err != nil {
		return u, err
	}
	access, err := e.checkFSType(req, typ, sig)
	if err != nil {
		return u, err
	}
	if err := checkNodeRules(req, access, others); err != nil {
		return u, err
	}
	writer := u.writer() || firstWriter(others) != nil
	mnt := e.nodeMountPath(source, serial, access)
	if err := e.commit(ctx, func(r *state.Records) error {
		r.Grant(req.Ref).Intent.MountPath = mnt
		return nil
	}); err != nil {
		return u, err
	}
	loop, err := e.nodeMount(req, &u, typ, access, mnt, writer, others, sig)
	if err != nil {
		return u, err
	}
	u.access, u.fsType, u.nodeMount, u.loop = access, typ, mnt, loop
	return u, nil
}

// signature reads the signature of a block device once, when a rule needs
// it.
type signature struct {
	e           *Engine
	serial, dev string
	sig         *kernel.FSSignature
}

// get returns the signature of the block device.
func (s *signature) get() (*kernel.FSSignature, error) {
	if s.sig == nil {
		sg, err := s.e.k.ReadFSSignature(s.dev)
		if err != nil {
			return nil, fmt.Errorf("memdev %s: reading %s: %w", s.serial, s.dev, err)
		}
		s.sig = &sg
	}
	return s.sig, nil
}

// fsTypeOf returns the type of the filesystem of the block device dev of a
// grant: that of a node mount of the device, else of its signature.
func (e *Engine) fsTypeOf(req *GrantRequest, dev string, sig *signature) (string, error) {
	var mounted *kernel.MountInfo
	for _, a := range []string{api.AccessDirect, api.AccessCached} {
		mi, err := e.k.Mounted(e.nodeMountPath(req.Asked.Source, req.Serial, a))
		if err != nil {
			return "", err
		}
		if mi == nil || mounted != nil {
			continue
		}
		if ok, err := e.isDeviceMount(mi, dev, a); err != nil {
			return "", err
		} else if ok {
			mounted = mi
		}
	}
	if mounted != nil {
		if req.Asked.FSType != "" && req.Asked.FSType != mounted.FSType {
			return "", fmt.Errorf("memdev %s: %s has a filesystem of type %s, not %s", sig.serial, dev, mounted.FSType, req.Asked.FSType)
		}
		return mounted.FSType, nil
	}
	sg, err := sig.get()
	if err != nil {
		return "", err
	}
	return fsType(*sg, sig.serial, dev, req.Asked.FSType)
}

// checkFSType returns the access of a grant of the filesystem type (the
// asked one, else direct for a DAX type, cached for the others), or an
// error if the node does not allow the type, the type mounts no block
// device, a read-only type is asked read-write, a type without DAX is asked
// direct, or a writer on several hosts has no cluster filesystem on
// coherent memory.
func (e *Engine) checkFSType(req *GrantRequest, typ string, sig *signature) (string, error) {
	fail := func(format string, args ...any) (string, error) {
		return "", fmt.Errorf("memdev "+sig.serial+": "+format, args...)
	}
	allowed := strings.Join(e.opts.FSTypes, ", ")
	if allowed == "" {
		allowed = "none"
	}
	switch {
	case !slices.Contains(e.opts.FSTypes, typ):
		return fail("filesystem type %s is not allowed on this node (fsTypes: %s)", typ, allowed)
	case e.nodevType(typ):
		return fail("filesystem type %s does not mount a block device", typ)
	case !req.Asked.ReadOnly && config.FSTypeReadOnly(typ):
		return fail("%s is a read-only filesystem: set readOnly: true", typ)
	case req.Asked.Access == state.AccessDirect && !config.FSTypeDax(typ):
		return fail("%s has no DAX: use access cached", typ)
	}
	access := string(req.Asked.Access)
	if access == "" {
		access = api.AccessCached
		if config.FSTypeDax(typ) {
			access = api.AccessDirect
		}
	}
	if !req.Asked.ReadOnly && !req.SingleHost {
		if err := checkCluster(req, typ, sig); err != nil {
			return "", err
		}
	}
	return access, nil
}

// checkCluster returns an error unless a writer of the type on several
// hosts has a cluster filesystem on coherent memory.
func checkCluster(req *GrantRequest, typ string, sig *signature) error {
	sg, err := sig.get()
	if err != nil {
		return err
	}
	ok, why := cluster(*sg, typ)
	if ok && !req.Facts.Coherent {
		ok, why = false, "the device is not coherent between hosts"
	}
	if !ok {
		return fmt.Errorf("memdev %s: read-write on several hosts needs a cluster filesystem on coherent memory: %s", sig.serial, why)
	}
	return nil
}

// checkNodeRules returns an error if a grant of the access breaks the rules
// of one node next to the other grants of the device: one access with a
// writer, a writer only with one access.
func checkNodeRules(req *GrantRequest, access string, others []pmemGrant) error {
	s := req.Serial.String()
	other := firstWriter(others)
	accesses := fsAccesses(others)
	switch {
	case other != nil && other.access != access:
		return fmt.Errorf("memdev %s: it is mounted read-write with access %s on this node: access %s would mount it a second time",
			s, other.access, access)
	case !req.Asked.ReadOnly && len(accesses) == 2:
		return fmt.Errorf("memdev %s: it is mounted with access %s and %s on this node: a read-write claim needs one access only",
			s, api.AccessDirect, api.AccessCached)
	case !req.Asked.ReadOnly && len(accesses) == 1 && accesses[0] != access:
		return fmt.Errorf("memdev %s: it is mounted with access %s only on this node: a read-write claim with access %s would mount it a second time",
			s, accesses[0], access)
	}
	return nil
}

// nodeMount returns the loop device of the node mount mnt of a grant after
// it mounted it, or checked and reused the mount there: made read-write for
// a grant that writes, read-only when no grant writes (a read-write
// superblock that a failed release left).
func (e *Engine) nodeMount(req *GrantRequest, u *pmemGrant, typ, access, mnt string, writer bool, others []pmemGrant, sig *signature) (string, error) {
	source, serial, dev := req.Asked.Source, req.Serial, u.blockDevice
	mi, err := e.k.Mounted(mnt)
	if err != nil {
		return "", err
	}
	if mi == nil {
		return e.mountFresh(source, serial, dev, access, typ, mnt, writer, nodeMounts(others), sig)
	}
	loop, err := e.checkNodeMount(serial, dev, access, typ, mi)
	if err != nil {
		return "", err
	}
	switch {
	case u.writer() && mi.SuperReadOnly() && !config.FSTypeRemountable(typ):
		return "", fmt.Errorf("memdev %s: %s is mounted read-only on this node and cannot be remounted read-write: a read-write claim waits until the read-only claims of the node are gone",
			sig.serial, typ)
	case u.writer() && mi.SuperReadOnly():
		if err := e.remountReadWrite(source, serial, dev, loop, mnt, append(nodeMounts(others), mnt)); err != nil {
			return "", err
		}
	case !writer && !mi.SuperReadOnly():
		// a rw superblock without a writer (a release that failed)
		if err := e.remountReadOnly(source, serial, dev, loop, mnt); err != nil {
			klog.Errorf("engine: %v", err)
		}
	}
	return loop, nil
}

// mountFresh mounts the node mount mnt that is not mounted (mountNode),
// refusing an ext4 that needs recovery when no grant writes.
func (e *Engine) mountFresh(source state.Source, serial state.Serial, dev, access, typ, mnt string, writer bool, explained []string, sig *signature) (string, error) {
	if !writer && typ == api.FSTypeExt4 {
		sg, err := sig.get()
		if err != nil {
			return "", err
		}
		if sg.Ext4NeedsRecovery {
			return "", errNeedsRecovery(sig.serial, typ, dev)
		}
	}
	return e.mountNode(source, serial, dev, access, typ, mnt, writer, explained)
}

// errNeedsRecovery returns the error of a filesystem that a read-only
// mount cannot recover.
func errNeedsRecovery(serial, fsType, dev string) error {
	return fmt.Errorf("memdev %s: %s on %s needs recovery (it was not unmounted cleanly): a read-only mount cannot recover it, a read-write claim on one node does",
		serial, fsType, dev)
}

// nodevType tells whether the kernel lists the filesystem type as one
// without a block device.
func (e *Engine) nodevType(fsType string) bool {
	nodev, err := e.k.NodevFSType(fsType)
	if err != nil {
		klog.V(3).Infof("engine: %v", err)
	}
	return nodev
}

// recordNodeMount records the node mount of a grant of mode fs in its
// region: a new mount (NONE -> MOUNTED_RO or MOUNTED_RW), the grant a
// user of an existing one, read-write iff writer (a grant of the device
// writes).
func (e *Engine) recordNodeMount(ctx context.Context, req *GrantRequest, region string, u pmemGrant, writer bool) error {
	if u.nodeMount == "" {
		return nil
	}
	return e.commit(ctx, func(r *state.Records) error {
		reg := r.Region(state.RegionRef(region))
		m := reg.Mount(state.Access(u.access))
		if m == nil {
			reg.Mounts = append(reg.Mounts, state.Mount{Access: state.Access(u.access), Path: u.nodeMount, State: state.MountNone})
			m = &reg.Mounts[len(reg.Mounts)-1]
		}
		want := state.MountedRO
		if writer {
			want = state.MountedRW
		}
		if err := fsm.Apply(&state.MountTable, &m.State, want, state.TriggerPrepare); err != nil {
			return err
		}
		m.Path, m.Loop, m.FSType = u.nodeMount, u.loop, u.fsType
		m.Users = addRef(m.Users, req.Ref)
		g := r.Grant(req.Ref)
		g.Got.Mount = state.Access(u.access)
		return remountedUsers(r, m, req.Ref)
	})
}

// remountedUsers makes the other REALISED_UNRESTORED grants that use the
// node mount m, mounted again by the grant ref, REALISED with its loop
// device in m, and their preparations PREPARED when all their grants are.
func remountedUsers(r *state.Records, m *state.Mount, ref state.GrantRef) error {
	for _, user := range m.Users {
		g := r.Grant(user)
		if user == ref || g == nil || g.State != state.GrantUnrestored {
			continue
		}
		if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantRealised, state.TriggerPrepare); err != nil {
			return err
		}
		p := r.PreparationOf(user)
		if p != nil && p.State == state.PreparationUnrestored && r.AllRealised(p) {
			if err := fsm.Apply(&state.PreparationTable, &p.State, state.PreparationPrepared, state.TriggerPrepare); err != nil {
				return err
			}
		}
	}
	return nil
}

// mountNode returns the loop device of a cached node mount after it
// mounted the filesystem of the type on the block device dev at the node
// mount mnt for the access: read-write for a writer, else read-only on a
// read-only block device; access direct on the block device, cached on a
// direct-I/O loop device over it. Read-only types are mounted with
// mount(2) on a read-only loop device. A writable type is not mounted
// while the device has a superblock that the node mounts explained do not
// have.
func (e *Engine) mountNode(source state.Source, serial state.Serial, dev, access, fsType, mnt string, writer bool, explained []string) (string, error) {
	readOnlyType := config.FSTypeReadOnly(fsType)
	if !readOnlyType {
		if err := e.checkUnexplained(serial, dev, explained); err != nil {
			return "", err
		}
	}
	ro := !writer || readOnlyType
	data := config.FSMountData(fsType, access, ro)
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return "", err
	}
	if err := e.setReadOnly(source, serial, dev, ro); err != nil {
		e.removeMountDir(mnt)
		return "", err
	}
	src := dev
	var loop kernel.Loop
	if access == api.AccessCached {
		l, err := e.configureCachedLoop(source, serial, dev, mnt, readOnlyType, ro)
		if err != nil {
			return "", err
		}
		loop, src = l, l.Path()
	}
	if err := e.mountFs(source, serial, dev, src, fsType, mnt, ro, data, loop); err != nil {
		return "", err
	}
	if access == api.AccessDirect && config.FSTypeDax(fsType) {
		if err := e.verifyNodeDax(source, serial, dev, fsType, mnt); err != nil {
			return "", err
		}
	}
	what := fsType + ", " + roWord(ro)
	if len(data) > 0 {
		what += ", " + strings.Join(data, ",")
	}
	klog.Infof("engine: %s memdev %s: mounted %s at %s (%s)", source, serial.String(), src, mnt, what)
	if loop != nil {
		return loop.Path(), nil
	}
	return "", nil
}

// mountFs mounts the filesystem of the type on src (the block device dev,
// or the loop device loop over it) at mnt: mount(2) for a read-only type,
// the new mount API read-only (ro) or read-write for the others. On an
// error the loop device is detached and the directory removed; then the
// mount keeps the autoclear loop device.
func (e *Engine) mountFs(source state.Source, serial state.Serial, dev, src, fsType, mnt string, ro bool, data []string, loop kernel.Loop) error {
	s := serial.String()
	err := e.locked(func() error {
		if config.FSTypeReadOnly(fsType) {
			return e.k.Mount(src, mnt, fsType, fsMountFlags, strings.Join(data, ","))
		}
		return e.k.FSMount(src, mnt, fsType, !ro, data)
	})
	if err != nil {
		if loop != nil {
			e.clearLoop(source, serial, loop)
		}
		e.removeMountDir(mnt)
		if ro && errors.Is(err, syscall.EROFS) {
			return errNeedsRecovery(s, fsType, dev)
		}
		return fmt.Errorf("memdev %s: %s has no %s filesystem: %w", s, dev, fsType, err)
	}
	if loop != nil {
		if err := loop.Close(); err != nil {
			klog.Warningf("engine: %s memdev %s: closing %s: %v", source, s, loop.Path(), err)
		}
	}
	return nil
}

// configureCachedLoop returns a new direct-I/O loop device over the block
// device dev for a cached node mount at mnt, read-only for a read-only type,
// else with the read-only flag ro; on an error nothing of it is left.
func (e *Engine) configureCachedLoop(source state.Source, serial state.Serial, dev, mnt string, readOnlyType, ro bool) (kernel.Loop, error) {
	var l kernel.Loop
	err := e.locked(func() error {
		var err error
		l, err = e.k.ConfigureLoop(dev, !readOnlyType)
		return err
	})
	if err != nil {
		e.removeMountDir(mnt)
		return nil, fmt.Errorf("memdev %s: a loop device over %s: %w", serial.String(), dev, err)
	}
	if !readOnlyType {
		if err := e.setReadOnly(source, serial, l.Path(), ro); err != nil {
			e.clearLoop(source, serial, l)
			e.removeMountDir(mnt)
			return nil, err
		}
	}
	return l, nil
}

// verifyNodeDax returns an error, after it unmounted the node mount mnt
// again, when the mount of the DAX type is without dax=always: another
// mount of the block device on the node has the superblock, and its
// options win.
func (e *Engine) verifyNodeDax(source state.Source, serial state.Serial, dev, fsType, mnt string) error {
	s := serial.String()
	mi, err := e.k.Mounted(mnt)
	if err == nil && (mi == nil || !mi.HasSuperOpt(daxAlways)) {
		err = fmt.Errorf("memdev %s: %s on %s was mounted without DAX", s, fsType, dev)
	}
	if err == nil {
		return nil
	}
	if uerr := e.locked(func() error { return e.k.Unmount(mnt) }); uerr != nil {
		klog.Errorf("engine: %s memdev %s: unmounting %s: %v", source, s, mnt, uerr)
	} else {
		e.removeMountDir(mnt)
	}
	return err
}

// clearLoop detaches a loop device that no mount uses.
func (e *Engine) clearLoop(source state.Source, serial state.Serial, loop kernel.Loop) {
	if err := e.locked(func() error { return loop.Clear() }); err != nil {
		klog.Errorf("engine: %s memdev %s: detaching %s: %v", source, serial.String(), loop.Path(), err)
	}
}

// remountReadWrite makes the read-only superblock of the node mount mnt read-write:
// the block device, then the loop device of a cached mount, then the
// superblock. On error both are read-only again. It refuses while the
// device has a superblock that the node mounts explained do not have.
func (e *Engine) remountReadWrite(source state.Source, serial state.Serial, dev, loop, mnt string, explained []string) error {
	s := serial.String()
	if err := e.checkUnexplained(serial, dev, explained); err != nil {
		return err
	}
	fail := func(err error) error {
		if loop != "" {
			if rerr := e.setReadOnly(source, serial, loop, true); rerr != nil {
				klog.Errorf("engine: %v", rerr)
			}
		}
		if rerr := e.setReadOnly(source, serial, dev, true); rerr != nil {
			klog.Errorf("engine: %v", rerr)
		}
		return fmt.Errorf("memdev %s: remounting %s read-write: %w", s, mnt, err)
	}
	if err := e.setReadOnly(source, serial, dev, false); err != nil {
		return fail(err)
	}
	if loop != "" {
		if err := e.setReadOnly(source, serial, loop, false); err != nil {
			return fail(err)
		}
	}
	if err := e.locked(func() error { return e.k.Remount(mnt, true) }); err != nil {
		return fail(err)
	}
	klog.Infof("engine: %s memdev %s: remounted %s read-write", source, s, mnt)
	return nil
}

// remountReadOnly makes the read-write superblock of the node mount mnt
// read-only: its data written, the superblock, then the loop device of a
// cached mount, then the block device.
func (e *Engine) remountReadOnly(source state.Source, serial state.Serial, dev, loop, mnt string) error {
	s := serial.String()
	if err := e.locked(func() error { return e.k.Syncfs(mnt) }); err != nil {
		return fmt.Errorf("memdev %s: remounting %s read-only: %w", s, mnt, err)
	}
	if err := e.locked(func() error { return e.k.Remount(mnt, false) }); err != nil {
		return fmt.Errorf("memdev %s: remounting %s read-only: %w", s, mnt, err)
	}
	klog.Infof("engine: %s memdev %s: remounted %s read-only", source, s, mnt)
	if loop != "" {
		if err := e.setReadOnly(source, serial, loop, true); err != nil {
			return err
		}
	}
	return e.setReadOnly(source, serial, dev, true)
}

// setReadOnly sets the read-only flag of the block device dev of the
// memdev with the serial, when it differs, and logs the change. A block
// device is never made read-only while a read-write superblock is on it,
// or on a loop device over it. A block device that is gone is not an error.
func (e *Engine) setReadOnly(source state.Source, serial state.Serial, dev string, readOnly bool) error {
	if dev == "" {
		return nil
	}
	s := serial.String()
	changed := false
	if err := e.locked(func() error {
		var err error
		changed, err = e.k.SetBlockDeviceReadOnly(dev, readOnly)
		return err
	}); err != nil {
		return fmt.Errorf("memdev %s: %w", s, err)
	}
	if changed {
		klog.Infof("engine: %s memdev %s: %s %s", source, s, dev, kernel.RoWord(readOnly))
	}
	return nil
}

// checkUnexplained returns an error if the block device dev of the memdev
// with the serial, or a loop device over it, has a superblock anywhere on
// the node that is not that of one of the node mounts explained (the node
// mounts of the grants of the device), or if its superblocks cannot be
// read.
func (e *Engine) checkUnexplained(serial state.Serial, dev string, explained []string) error {
	s := serial.String()
	sbs, err := e.k.Superblocks(dev)
	if err != nil {
		return fmt.Errorf("memdev %s: the superblocks of %s: %w", s, dev, err)
	}
	known := map[string]bool{}
	for _, mnt := range explained {
		mi, err := e.k.Mounted(mnt)
		if err != nil {
			return err
		}
		if mi != nil {
			known[mi.Source] = true
		}
	}
	for _, sb := range sbs {
		if !known[sb.Device] {
			return fmt.Errorf("memdev %s: %s has a superblock that no claim of this node explains (%s mounted %s in %s): it is not used until that mount is gone",
				s, dev, sb.Source, roWord(sb.ReadOnly), sb.Where)
		}
	}
	return nil
}

// isDeviceMount tells whether a mount is one of the block device dev for
// the access: of dev itself (direct), of a loop device over it (cached).
func (e *Engine) isDeviceMount(mi *kernel.MountInfo, dev, access string) (bool, error) {
	if access != api.AccessCached {
		return mi.Source == dev, nil
	}
	if e.loopOf(mi) == "" {
		return false, nil
	}
	backing, err := e.k.LoopBackingFile(mi.Source)
	if err != nil {
		return false, err
	}
	return kernel.SameDevice(backing, dev), nil
}

// checkNodeMount returns the loop device of an existing cached node mount,
// or an error if the mount at the node mount is not the filesystem of the
// type ("": any) of the block device for the access: on the block device
// (direct, with dax=always for the DAX types), on a loop device over it
// (cached).
func (e *Engine) checkNodeMount(serial state.Serial, dev, access, fsType string, mi *kernel.MountInfo) (string, error) {
	s := serial.String()
	ok, err := e.isDeviceMount(mi, dev, access)
	if err != nil {
		return "", err
	}
	if !ok {
		want := dev
		if access == api.AccessCached {
			want = "a loop device over " + dev
		}
		return "", fmt.Errorf("memdev %s: %s is a %s mount of %s, not of %s", s, mi.Target, mi.FSType, mi.Source, want)
	}
	if fsType != "" && mi.FSType != fsType {
		return "", fmt.Errorf("memdev %s: %s is a %s mount of %s, not %s", s, mi.Target, mi.FSType, mi.Source, fsType)
	}
	if access == api.AccessCached {
		return mi.Source, nil
	}
	if config.FSTypeDax(mi.FSType) && !mi.HasSuperOpt(daxAlways) {
		return "", fmt.Errorf("memdev %s: %s on %s was mounted without DAX", s, mi.FSType, dev)
	}
	return "", nil
}

// removeMountDir removes the directory of a mount that is not mounted.
func (e *Engine) removeMountDir(mnt string) {
	if err := os.Remove(mnt); err != nil && !errors.Is(err, os.ErrNotExist) {
		klog.Warningf("engine: removing %s: %v", mnt, err)
	}
}

// unmountNode unmounts a node mount if it is mounted, never detaching it,
// and removes its directory. A busy mount is an error.
func (e *Engine) unmountNode(source state.Source, serial state.Serial, mnt string) error {
	mi, err := e.k.Mounted(mnt)
	if err != nil {
		return err
	}
	if mi != nil {
		if err := e.locked(func() error { return e.k.Unmount(mnt) }); err != nil {
			return fmt.Errorf("memdev %s: unmounting %s: %w", serial.String(), mnt, err)
		}
		klog.Infof("engine: %s memdev %s: unmounted %s", source, serial.String(), mnt)
	}
	e.removeMountDir(mnt)
	return nil
}

// releasePmemGrants releases the grants gone of the memdev with the serial
// while the grants remaining stay: the node mounts that no remaining grant has
// are unmounted, the superblock of a node mount whose remaining grants have no
// writer is made read-only, and so are the block devices when no writer
// remains (the block device of the remaining grants only when it is the one of
// the region now). An error (a busy unmount or remount read-only) stops it.
func (e *Engine) releasePmemGrants(source state.Source, serial state.Serial, gone, remaining []pmemGrant) error {
	used := nodeMounts(remaining)
	for _, mnt := range nodeMounts(gone) {
		if slices.Contains(used, mnt) {
			continue
		}
		if err := e.unmountNode(source, serial, mnt); err != nil {
			return err
		}
	}
	if len(remaining) == 0 || firstWriter(remaining) != nil {
		return nil
	}
	// the recorded block device may be another device's now
	dev := e.verified(source, serial, firstBlockDevice(remaining))
	var loops []string
	for _, mnt := range used {
		mi, err := e.k.Mounted(mnt)
		if err != nil || mi == nil {
			continue
		}
		loop := e.loopOf(mi)
		if loop != "" {
			loops = append(loops, loop)
		}
		if !mi.SuperReadOnly() {
			if err := e.remountReadOnly(source, serial, dev, loop, mnt); err != nil {
				return err
			}
		}
	}
	for _, loop := range loops {
		if err := e.setReadOnly(source, serial, loop, true); err != nil {
			return err
		}
	}
	return e.setReadOnly(source, serial, dev, true)
}

// verified returns dev when it is the block device of the fsdax namespace
// of the region of the memdev with the serial now as recorded, else ""
// (logged).
func (e *Engine) verified(source state.Source, serial state.Serial, dev string) string {
	if dev == "" {
		return dev
	}
	if err := e.checkRecordedBlockDevice(serial, dev); err != nil {
		klog.Warningf("engine: %s memdev %s: not changing the read-only flag of %s: %v", source, serial.String(), dev, err)
		return ""
	}
	return dev
}

// waitNoHolder waits until the deadline until nothing holds the block
// device of a persistent memdev: no mount, no loop device, no exclusive
// open, outside the engine lock.
//
// This check is what keeps a region with a live superblock from being
// released, and it must stay: an umount succeeds while a bind of the same
// superblock exists elsewhere, and an rmdir of a directory that is a mount
// point in another mount namespace succeeds and detaches those mounts
// lazily, so a lease bind can be gone from the driver's view and still hold
// the device.
func (e *Engine) waitNoHolder(ctx context.Context, source state.Source, serial state.Serial, dev string, deadline time.Time) error {
	s := serial.String()
	for {
		holder, err := e.k.Holder(dev)
		if err != nil {
			return err
		}
		if holder == "" {
			return nil
		}
		if !time.Now().Before(deadline) {
			klog.Warningf("engine: %s memdev %s: %s is held: %s", source, s, dev, holder)
			return fmt.Errorf("memdev %s: %s is still in use", s, dev)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("memdev %s: %s is still in use: %w", s, dev, ctx.Err())
		case <-time.After(holderPoll):
		}
	}
}

// checkRecordedBlockDevice returns an error if blockDev, as a prepared
// grant recorded it, is not the block device of the fsdax namespace of the
// region of the memdev with the serial now.
func (e *Engine) checkRecordedBlockDevice(serial state.Serial, blockDev string) error {
	r := e.records()
	for i := range r.Grants {
		g := &r.Grants[i]
		// a grant whose region is gone has the block device of that region
		if g.Serial == serial && g.Got.Region != "" && g.Got.BlockDevice != nil && g.Got.BlockDevice.Name == blockDev {
			return e.checkBlockDevice(g)
		}
	}
	return fmt.Errorf("memdev %s: no prepared device has %s", serial.String(), blockDev)
}

// checkBlockDevice returns an error if the block device of a grant is not
// the one of the fsdax namespace of the region of its memdev now: its
// name, numbers and namespace must be those that the Prepare recorded.
func (e *Engine) checkBlockDevice(g *state.Grant) error {
	s := g.Serial.String()
	nss, nd, err := e.regionNamespaces(g.Serial)
	if err != nil {
		return fmt.Errorf("memdev %s: %w", s, err)
	}
	cur := kernel.FsdaxNamespace(nss)
	bd := g.Got.BlockDevice
	switch {
	case cur == nil:
		return fmt.Errorf("memdev %s: no fsdax namespace in %s", s, nd)
	case bd == nil || cur.BlockDevice != bd.Name || cur.Name != g.Got.Namespace || cur.Major != bd.Dev.Major || cur.Minor != bd.Dev.Minor:
		var name string
		var major, minor int
		if bd != nil {
			name, major, minor = bd.Name, bd.Dev.Major, bd.Dev.Minor
		}
		return fmt.Errorf("memdev %s: its fsdax namespace is %s with %s (%d:%d), not %s with %s (%d:%d) as prepared",
			s, cur.Name, cur.BlockDevice, cur.Major, cur.Minor, g.Got.Namespace, name, major, minor)
	}
	return nil
}

// errNoRegion is the kind of the error of a memdev without a region.
var errNoRegion = errors.New("has no region")

// regionNamespaces returns the namespaces of the nd region of the region of
// the memdev with the serial and the nd region, waiting at most
// Timeouts.Restore for them to settle.
func (e *Engine) regionNamespaces(serial state.Serial) ([]kernel.Namespace, string, error) {
	memdev, err := e.k.FindMemdev(uint64(serial))
	if err != nil {
		return nil, "", err
	}
	regions := e.k.Regions(memdev)
	if len(regions) == 0 {
		return nil, "", fmt.Errorf("%s %w", memdev, errNoRegion)
	}
	nd, err := e.k.NdRegionOf(regions[0])
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.opts.Timeouts.Restore)
	defer cancel()
	nss, err := e.k.Namespaces(ctx, nd)
	return nss, nd, err
}

// currentBlockDevice returns the block device of the fsdax namespace of the
// region of the memdev with the serial, else of another namespace of it
// with a block device, "" if there is none.
func (e *Engine) currentBlockDevice(serial state.Serial) string {
	nss, _, err := e.regionNamespaces(serial)
	if err != nil {
		if !errors.Is(err, kernel.ErrNotFound) && !errors.Is(err, errNoRegion) {
			klog.Warningf("engine: memdev %s: %v", serial.String(), err)
		}
		return ""
	}
	if ns := kernel.FsdaxNamespace(nss); ns != nil {
		return ns.BlockDevice
	}
	for _, ns := range nss {
		if ns.BlockDevice != "" {
			return ns.BlockDevice
		}
	}
	return ""
}

// leaveNodeMount releases the block device or node mount of the
// persistent grants of the call on the memdev of a grant next to the other
// grants of the memdev, once, and removes those grants from the users of
// their node mount in one commit. The node mount of a grant that a cut
// Prepare left is the one of its intent when a mount of its device is
// there.
func (e *Engine) leaveNodeMount(ctx context.Context, g *state.Grant, call *releaseCall) error {
	if !g.Asked.Kind.Persistent() {
		return nil
	}
	if err, done := call.released[g.Serial]; done {
		return err
	}
	r := e.records()
	var remaining []pmemGrant
	for _, u := range pmemGrants(r, g.Serial, g.Ref) {
		if !call.refs[u.ref] {
			remaining = append(remaining, u)
		}
	}
	var refs []state.GrantRef
	for _, o := range callGrants(r, g, call) {
		refs = append(refs, o.Ref)
	}
	err := e.releasePmemGrants(g.Asked.Source, g.Serial, e.gonePmemGrants(r, g, call), remaining)
	if err != nil {
		err = fmt.Errorf("releasing %s (memdev %s): %w", e.deviceNameOf(g.Ref), g.Serial.String(), err)
	}
	call.released[g.Serial] = err
	if err != nil {
		return err
	}
	return e.commit(ctx, func(r *state.Records) error { return leaveMount(r, refs, firstWriter(remaining) != nil, call.trigger) })
}

// gonePmemGrants returns the grants of a release call on the memdev of g, g
// first, that give up a block device or node mount: none of a node mount
// that is not ours (UNRESTORED, FOREIGN).
func (e *Engine) gonePmemGrants(r *state.Records, g *state.Grant, call *releaseCall) []pmemGrant {
	var gone []pmemGrant
	for _, o := range callGrants(r, g, call) {
		u := pmemGrantOf(r, o)
		if u.nodeMount == "" {
			u = e.withCutMount(o, u)
		}
		if o.Got.BlockDevice == nil && u.nodeMount == "" {
			continue
		}
		if reg := r.Region(o.Got.Region); reg != nil && o.Got.Mount != "" {
			if m := reg.Mount(o.Got.Mount); m != nil && (m.State == state.MountUnrestored || m.State == state.MountForeign) {
				// nothing of ours mounted to release
				continue
			}
		}
		gone = append(gone, u)
	}
	return gone
}

// callGrants returns g and the other persistent grants of the release call
// on its memdev.
func callGrants(r *state.Records, g *state.Grant, call *releaseCall) []*state.Grant {
	grants := []*state.Grant{g}
	if m := r.Memdev(g.Serial); m != nil {
		for _, ref := range m.Grants {
			if o := r.Grant(ref); o != nil && ref != g.Ref && call.refs[ref] && o.Asked.Kind.Persistent() {
				grants = append(grants, o)
			}
		}
	}
	return grants
}

// withCutMount returns u, the pmemGrant of a grant g of a cut Prepare, with
// the node mount of its intent when a mount of the block device of its
// region is there (the mount is made before the grant records it).
func (e *Engine) withCutMount(g *state.Grant, u pmemGrant) pmemGrant {
	if g.Intent.MountPath == "" {
		return u
	}
	dev := e.currentBlockDevice(g.Serial)
	if dev == "" {
		return u
	}
	mi, err := e.k.Mounted(g.Intent.MountPath)
	if err != nil || mi == nil {
		return u
	}
	for _, a := range []string{string(state.AccessDirect), string(state.AccessCached)} {
		if ok, _ := e.isDeviceMount(mi, dev, a); ok {
			u.nodeMount, u.blockDevice = g.Intent.MountPath, dev
		}
	}
	return u
}

// leaveMount removes grants from the users of the node mounts that list
// them: a mount without users is gone (NONE), one without a writer left
// read-only.
func leaveMount(r *state.Records, refs []state.GrantRef, writer bool, trigger state.Trigger) error {
	for _, ref := range refs {
		if rg := r.Grant(ref); rg != nil {
			rg.Got.Mount = ""
		}
	}
	for i := range r.Regions {
		reg := &r.Regions[i]
		for j := 0; j < len(reg.Mounts); j++ {
			m := &reg.Mounts[j]
			if !slices.ContainsFunc(m.Users, func(u state.GrantRef) bool { return slices.Contains(refs, u) }) {
				continue
			}
			for _, ref := range refs {
				m.Users = removeRef(m.Users, ref)
			}
			gone, err := leftMount(m, writer, trigger)
			if err != nil {
				return err
			}
			if gone {
				reg.Mounts = slices.Delete(reg.Mounts, j, j+1)
				j--
			}
		}
	}
	return nil
}

// leftMount applies the transition of a node mount that users left: NONE
// without users (it is gone), read-only without a writer. A FOREIGN one
// stays: it goes with its region.
func leftMount(m *state.Mount, writer bool, trigger state.Trigger) (bool, error) {
	switch {
	case m.State == state.MountForeign:
	case len(m.Users) == 0:
		return true, fsm.Apply(&state.MountTable, &m.State, state.MountNone, trigger)
	case m.State == state.MountedRW && !writer:
		return false, fsm.Apply(&state.MountTable, &m.State, state.MountedRO, trigger)
	case m.State == state.MountedRO || m.State == state.MountedRW:
		return false, fsm.Apply(&state.MountTable, &m.State, m.State, trigger)
	}
	return false, nil
}

// firstBlockDevice returns the block device whose holders the release of the
// last grant of a memdev, of the call, waits for: the recorded one unless
// the start did not mount its node mount again or it is not the one of its
// region now (checkBlockDevice: the region is gone, or its namespace has
// other numbers); then the recorded one may be another device's now, and
// the wait is for the current one of the region, none when the memdev has
// no region. The recorded one stays when the block device of the region is
// not known now: none would release it without the holder check. A grant
// of a cut Prepare waits for the current one.
func (e *Engine) firstBlockDevice(g *state.Grant, call *releaseCall) string {
	if !g.Asked.Kind.Persistent() {
		return ""
	}
	if g.Got.BlockDevice == nil {
		return e.currentBlockDevice(g.Serial)
	}
	recorded := g.Got.BlockDevice.Name
	var err error
	if path := call.unrestored[g.Ref]; path != "" {
		err = fmt.Errorf("its node mount %s was not restored at start", path)
	} else {
		err = e.checkBlockDevice(g)
	}
	if err == nil {
		return recorded
	}
	m, claim := memdevText(g.Asked.Source, g.Serial), e.claimNameOf(g.Ref)
	cur := e.currentBlockDevice(g.Serial)
	switch {
	case cur == "" && e.hasRegion(g.Serial) && e.otherBlockDevice(g.Got.BlockDevice):
		klog.Infof("engine: claim %s: %v; %s is another block device now: the release of %s waits for no holder of it",
			claim, err, recorded, m)
		return ""
	case cur == "" && e.hasRegion(g.Serial):
		klog.Warningf("engine: claim %s: %v; the block device of the region of %s is not known now: its release waits for the holders of %s as prepared",
			claim, err, m, recorded)
		return recorded
	case cur == "":
		klog.Infof("engine: claim %s: %v; %s has no region now: its release waits for no holder of %s", claim, err, m, recorded)
	default:
		klog.Infof("engine: claim %s: %v; the release of %s waits for the holders of %s, the block device of its region now, not of %s",
			claim, err, m, cur, recorded)
	}
	return cur
}

// otherBlockDevice tells whether the name of the recorded block device bd
// is a block device with other numbers now.
func (e *Engine) otherBlockDevice(bd *state.BlockDevice) bool {
	major, minor, _, err := e.k.BlockDeviceInfo(filepath.Base(bd.Name))
	return err == nil && (major != bd.Dev.Major || minor != bd.Dev.Minor)
}

// fsType returns the type of the filesystem of the block device dev of the
// device with the serial with the signatures s and the declared type ("":
// none). A type that the driver does not know is taken as declared when
// the device has no signature of a known type.
func fsType(s kernel.FSSignature, serial, dev, declared string) (string, error) {
	prefix := "memdev " + serial + ": "
	switch {
	case len(s.Types) == 1 && (declared == "" || declared == s.Types[0]):
		return s.Types[0], nil
	case len(s.Types) == 1:
		return "", fmt.Errorf("%s%s has a filesystem of type %s, not %s", prefix, dev, s.Types[0], declared)
	case len(s.Types) > 1 && slices.Contains(s.Types, declared):
		return declared, nil
	case len(s.Types) > 1:
		return "", fmt.Errorf("%s%s has signatures of several filesystems (%s): set device.fsType", prefix, dev, strings.Join(s.Types, ", "))
	case declared == "":
		return "", fmt.Errorf("%s%s has no filesystem of a type that the driver knows (%s): set device.fsType", prefix, dev, listOr(api.KnownFSTypes))
	case api.KnownFSType(declared):
		return "", fmt.Errorf("%s%s has no filesystem of type %s (no signature of a type that the driver knows)", prefix, dev, declared)
	}
	return declared, nil
}

// cluster returns true if a filesystem of the type with the signatures s
// is a cluster filesystem (gfs2 with lock_dlm), else why it is
// single-host.
func cluster(s kernel.FSSignature, fsType string) (bool, string) {
	switch {
	case !config.FSTypeCluster(fsType):
		return false, fsType + " is a single-host filesystem"
	case s.Gfs2LockProto == kernel.Gfs2LockProtoDLM:
		return true, ""
	case s.Gfs2LockProto == "":
		return false, fsType + " without a lock protocol is a single-host filesystem"
	}
	return false, fsType + " with " + s.Gfs2LockProto + " is a single-host filesystem"
}

// listOr returns the values as "a, b or c".
func listOr(values []string) string {
	if len(values) < 2 {
		return strings.Join(values, "")
	}
	return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
}
