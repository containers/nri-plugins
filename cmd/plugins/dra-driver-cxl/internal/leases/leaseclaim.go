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
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	cdiSpecs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
)

// LeaseClaim: a claim on the lease device "lease" of cxl.nri.io, prepared
// for the containers that reference it: OpenClaim creates
// <data dir>/leases/<uid>/cxl.sock and <data dir>/lease-fs/<uid>, and the
// CDI device lease-<uid> that cxl.nri.io writes with CDIEdits bind-mounts
// the socket directory at /run/cxl, the lease-fs directory read-only as a
// slave at /run/cxl/fs (the driver of the device binds the filesystems of
// fs leases below it, and they appear in the running containers) and the
// cxl-request CLI at /usr/local/bin.
//
// Identity: the record state.LeaseClaim: UID, the claim UID (the socket
// directory and the CDI device name); Namespace and Name of the claim.
//
// States: state.LeaseClaimState; CLOSED and REMOVING have no record: the
// removal of the directories of a claim that is not in the store
// (removeClaimDirs, with the claim's entry in removing while it runs) goes
// through them in memory: REMOVING once the lease-fs directory is gone,
// while the socket directory is removed.
//
// Transitions: state.LeaseClaimTable, applied by applyClaim.
//
// Invariants: a lease claim OPEN or CLOSING has its socket listened, but
// for a window after a commit that failed or ended late: the next
// reconcile listens again on an OPEN claim without a socket (listenAgain),
// and CloseClaim of a claim without a record stops a socket left by a late
// removal; the directories of a claim without a record are removed unless
// a bind is mounted in its lease-fs directory, then by the release of its
// last lease or a later sweep.
//
// Persistence: the record in the store, its only copy, committed before its
// socket and CDI device, and its removal committed before the socket stops
// (closed) and the CDI device goes; lease claims survive a reboot (kubelet
// prepares them again). A lost store keeps every directory.
//
// Locks and waits: the commits of the store; the broker's mutex for the
// sockets, without it for the removal of the directories (the removal in
// progress of a claim is waited for, and its socket is not started
// meanwhile); CloseClaim waits up to Timeouts.Close for the leases of the
// claim.

const (
	// leaseDirName is the directory of the socket directories of the
	// lease claims in the plugin data directory.
	leaseDirName = "leases"
	// leaseSocketName is the socket in the directory of a lease claim.
	leaseSocketName = "cxl.sock"
	// leaseSocketTmpDirName is the directory in the plugin data directory
	// where the sockets are created before they are renamed into the
	// directories of the claims: no container has it.
	leaseSocketTmpDirName = ".lease-sockets"
	// leaseFsDirName is the directory of the lease-fs directories of the
	// lease claims in the plugin data directory. Only the plugin writes
	// them; containers have them read-only.
	leaseFsDirName = "lease-fs"
	// leaseFsMountPoint is the mount point of api.FSDir in the socket
	// directory of a lease claim, as runc would create it.
	leaseFsMountPoint = "fs"
	// leaseContainerDir is where the directory of a lease claim is
	// mounted: the directory of api.DefaultSocket.
	leaseContainerDir = "/run/cxl"
	// leaseCLIContainerPath is where the CLI is mounted.
	leaseCLIContainerPath = "/usr/local/bin/cxl-request"
	// selinuxContainerFileLabel lets container processes use the socket
	// directory on SELinux hosts (best effort, ignored elsewhere).
	selinuxContainerFileLabel = "system_u:object_r:container_file_t:s0"
)

// leaseDir returns the host directory of a lease claim.
func (b *Broker) leaseDir(claimUID string) string {
	return filepath.Join(b.opts.DataDir, leaseDirName, claimUID)
}

// LeaseFsRoot returns the directory of the lease-fs directories of the
// lease claims in the plugin data directory dataDir.
func LeaseFsRoot(dataDir string) string {
	return filepath.Join(dataDir, leaseFsDirName)
}

// leaseFsDir returns the host directory of the mounts of the fs leases of
// a lease claim.
func (b *Broker) leaseFsDir(claimUID string) string {
	return filepath.Join(LeaseFsRoot(b.opts.DataDir), claimUID)
}

// hasPod tells whether the pod with the uid is a pod of the lease claim.
func hasPod(c *state.LeaseClaim, uid string) bool {
	return slices.ContainsFunc(c.Pods, func(p state.PodRef) bool { return p.UID == uid })
}

// reservedPods returns the pods in the reservedFor of a claim.
func reservedPods(claim *resourcev1.ResourceClaim) []state.PodRef {
	var pods []state.PodRef
	for _, c := range claim.Status.ReservedFor {
		if c.Resource == "pods" && c.APIGroup == "" {
			pods = append(pods, state.PodRef{Namespace: claim.Namespace, Name: c.Name, UID: string(c.UID)})
		}
	}
	return pods
}

// OpenClaim records a claim on the lease device and starts its socket.
// Idempotent: an open claim gets the pods of its reservedFor.
func (b *Broker) OpenClaim(claim *resourcev1.ResourceClaim) error {
	uid := string(claim.UID)
	var lc state.LeaseClaim
	err := b.commit(func(r *state.Records) error {
		cur := r.LeaseClaim(uid)
		if cur == nil {
			r.LeaseClaims = append(r.LeaseClaims, state.LeaseClaim{UID: uid, State: state.LeaseClaimNone})
			cur = &r.LeaseClaims[len(r.LeaseClaims)-1]
		}
		was := cur.State
		cur.Namespace, cur.Name, cur.Pods = claim.Namespace, claim.Name, reservedPods(claim)
		if !applyClaim(cur, state.LeaseClaimOpen, state.TriggerPrepare) {
			return fmt.Errorf("lease claim %s is %s", uid, was)
		}
		lc = cur.Clone()
		return nil
	})
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.listen(uid); err != nil {
		return err
	}
	klog.Infof("lease broker: claim %s/%s (%s): lease socket %s for pods %v",
		claim.Namespace, claim.Name, uid, filepath.Join(b.leaseDir(uid), leaseSocketName), lc.Pods)
	return nil
}

// recoverClaims starts the sockets of the stored lease claims at the
// start: an OPEN claim and a CLOSING one whose close a stop cut listen
// again; a failed listen is logged and an open claim is OPEN_NO_LISTENER
// until it is prepared again. A CLOSING claim stays CLOSING (no new
// leases) for kubelet's Unprepare retry.
func (b *Broker) recoverClaims() {
	r := b.records()
	to := map[string]state.LeaseClaimState{}
	b.mu.Lock()
	for i := range r.LeaseClaims {
		lc := &r.LeaseClaims[i]
		to[lc.UID] = state.LeaseClaimOpen
		if lc.State == state.LeaseClaimClosing {
			to[lc.UID] = state.LeaseClaimClosing
		}
		if err := b.listen(lc.UID); err != nil {
			klog.Errorf("lease broker: lease claim %s: %v", lc.UID, err)
			if to[lc.UID] != state.LeaseClaimClosing {
				to[lc.UID] = state.LeaseClaimOpenNoListener
			}
		}
	}
	b.mu.Unlock()
	if len(to) == 0 {
		return
	}
	b.change(func(r *state.Records) {
		for uid, next := range to {
			if lc := r.LeaseClaim(uid); lc != nil {
				applyClaim(lc, next, state.TriggerRecover)
			}
		}
	})
}

// CloseClaim releases every lease of a lease claim, waits up to
// Timeouts.Close until their nodes are revoked and the deletion of their
// companions is requested, then stops the socket, removes the claim and
// its directories; a lease-fs directory with a mount in it is removed
// later. The leases are removed when their companions are gone. A timeout
// is an error (kubelet retries the Unprepare). Unknown claims are not an
// error.
func (b *Broker) CloseClaim(ctx context.Context, uid string) error {
	if b.records().LeaseClaim(uid) == nil {
		// a socket left by a removal committed after its wait ended
		b.mu.Lock()
		b.stopListening(uid)
		b.mu.Unlock()
		return nil
	}
	var ns, name string
	err := b.commit(func(r *state.Records) error {
		if lc := r.LeaseClaim(uid); lc != nil {
			ns, name = lc.Namespace, lc.Name
			applyClaim(lc, state.LeaseClaimClosing, state.TriggerUnprepare)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var ids []string
	for _, l := range leasesOf(b.records(), uid) {
		ids = append(ids, l.ID)
	}
	for _, id := range ids {
		b.startRelease(id, "lease claim unprepared (pod done)", state.TriggerUnprepare)
	}
	ctx, cancel := context.WithTimeout(ctx, b.t.Close)
	defer cancel()
	remaining := func() int {
		n := 0
		for _, l := range leasesOf(b.records(), uid) {
			if !l.DeletionRequested {
				n++
			}
		}
		return n
	}
	if err := b.waitFor(ctx, func() bool { return remaining() == 0 }); err != nil {
		return fmt.Errorf("claim %s/%s: still releasing %d leases, retry", ns, name, remaining())
	}
	switch closed, err := b.closed(uid); {
	case err != nil:
		return err
	case !closed:
		return fmt.Errorf("claim %s/%s: prepared again while it was closed, retry", ns, name)
	}
	sock := filepath.Join(b.leaseDir(uid), leaseSocketName)
	err = os.Remove(sock)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if left := b.removeClaimDirs(uid, state.TriggerUnprepare); len(left) > 0 {
		klog.Infof("lease broker: claim %s/%s (%s): left %v, removed later", ns, name, uid, left)
	}
	klog.Infof("lease broker: claim %s/%s (%s): lease socket closed", ns, name, uid)
	return err
}

// closed removes the record of a closing lease claim, then stops its
// socket; it reports whether the claim is closed (false: it was opened
// again meanwhile, or the commit failed) and returns the error of the
// commit.
func (b *Broker) closed(uid string) (bool, error) {
	closed := true
	err := b.commit(func(r *state.Records) error {
		lc := r.LeaseClaim(uid)
		switch {
		case lc == nil:
		case !applyClaim(lc, state.LeaseClaimClosed, state.TriggerUnprepare):
			closed = false
		default:
			r.LeaseClaims = slices.DeleteFunc(r.LeaseClaims, func(c state.LeaseClaim) bool { return c.UID == uid })
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !closed || b.records().LeaseClaim(uid) != nil {
		return false, nil
	}
	b.stopListening(uid)
	return true, nil
}

// stopListening stops the socket of a lease claim: it closes the listener,
// so that no new connection is accepted, and the open connections
// (closeConnsLocked), so that their requests end. It drops the claim's
// request bucket and warning counts. Called with b.mu held.
func (b *Broker) stopListening(uid string) {
	if l := b.listeners[uid]; l != nil {
		_ = l.Close()
		delete(b.listeners, uid)
	}
	b.closeConnsLocked(uid)
	delete(b.requests, uid)
	delete(b.warnings, uid)
}

// removeClaimDirs removes the directories of a lease claim that is not in
// the store and returns the paths that are left: lease-fs/<uid>
// (removeFsDir, os.Remove only), then, once it is gone, the entries of
// leases/<uid> by name (with whatever a container of before its read-only
// mount wrote into its /run/cxl; never following a symlink) and the
// directory. While a bind is in lease-fs/<uid>, leases/<uid> stays with
// the mount point of api.FSDir in it: an rmdir of that mount point would
// detach the bind lazily in a container that is still there. Nothing is
// mounted below leases/<uid> in the driver's mount namespace. Called
// without b.mu: the removal of what a container wrote may take long; the
// claim's socket is not started meanwhile (listen). A removal of the
// claim's directories in progress is waited for, then it is decided again.
func (b *Broker) removeClaimDirs(uid string, trigger state.Trigger) []string {
	b.mu.Lock()
	for {
		if b.records().LeaseClaim(uid) != nil {
			b.mu.Unlock()
			return nil
		}
		busy, ok := b.removing[uid]
		if !ok {
			break
		}
		b.mu.Unlock()
		<-busy
		b.mu.Lock()
	}
	done := make(chan struct{})
	b.removing[uid] = done
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.removing, uid)
		close(done)
		b.mu.Unlock()
	}()
	if !b.removeFsDir(uid) {
		return []string{b.leaseFsDir(uid)}
	}
	gone := &state.LeaseClaim{UID: uid, State: state.LeaseClaimClosed}
	applyClaim(gone, state.LeaseClaimRemoving, trigger)
	dir := b.leaseDir(uid)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		applyClaim(gone, state.LeaseClaimNone, trigger)
		return nil
	}
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for _, e := range entries {
		// RemoveAll removes a symlink itself, never its target
		errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
	}
	if err := errors.Join(errs...); err == nil {
		err = os.Remove(dir)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			applyClaim(gone, state.LeaseClaimNone, trigger)
			return nil
		}
		errs = append(errs, err)
	}
	klog.Warningf("lease broker: removing %s: %v", dir, errors.Join(errs...))
	return []string{dir}
}

// removeFsDir removes the lease-fs directory of a lease claim and its
// entries, except entries that are mount points or have mounts below them:
// those and the directory stay. It reports whether the directory is gone.
// Errors are logged.
func (b *Broker) removeFsDir(uid string) bool {
	dir := b.leaseFsDir(uid)
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return true
	case err != nil:
		klog.Errorf("lease broker: %v", err)
		return false
	}
	mounts, err := b.opts.MountPoints()
	if err != nil {
		klog.Errorf("lease broker: not removing %s: %v", dir, err)
		return false
	}
	left := 0
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if mountedAtOrBelow(mounts, p) {
			klog.V(3).Infof("lease broker: %s is mounted, keeping it and %s", p, dir)
			left++
			continue
		}
		// os.Remove only: RemoveAll could descend into a mount
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			klog.Warningf("lease broker: removing %s: %v", p, err)
			left++
		}
	}
	if left > 0 {
		return false
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		klog.Warningf("lease broker: removing %s: %v", dir, err)
		return false
	}
	klog.V(3).Infof("lease broker: removed %s", dir)
	return true
}

// mountedAtOrBelow tells whether p or a path below p is one of the mount
// points.
func mountedAtOrBelow(mounts []string, p string) bool {
	for _, t := range mounts {
		if t == p || strings.HasPrefix(t, p+"/") {
			return true
		}
	}
	return false
}

// sweepFsDirs removes the lease-fs and socket directories of lease claims
// that are not in the store (removeClaimDirs), unless the store is lost:
// then the lease claims of the node are unknown, and nothing is removed.
// Called without b.mu.
func (b *Broker) sweepFsDirs(trigger state.Trigger) {
	if b.st.Lost() != nil {
		klog.V(3).Infof("lease broker: the lease table was not decoded, keeping the directories of lease claims")
		return
	}
	r := b.records()
	var uids []string
	seen := map[string]bool{}
	for _, root := range []string{LeaseFsRoot(b.opts.DataDir), filepath.Join(b.opts.DataDir, leaseDirName)} {
		entries, err := os.ReadDir(root)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			klog.Errorf("lease broker: %v", err)
		}
		for _, e := range entries {
			uid := e.Name()
			if !e.IsDir() || seen[uid] || r.LeaseClaim(uid) != nil {
				continue
			}
			seen[uid] = true
			uids = append(uids, uid)
		}
	}
	for _, uid := range uids {
		b.removeClaimDirs(uid, trigger)
	}
}

// listen starts the socket of a lease claim if it is not running, with the
// directories of its fs leases, unless the broker stops. Called with b.mu
// held.
func (b *Broker) listen(uid string) error {
	switch {
	case b.listeners[uid] != nil:
		return nil
	case b.ctx.Err() != nil:
		return errors.New("the lease broker is stopping")
	}
	if _, ok := b.removing[uid]; ok {
		return fmt.Errorf("the directories of lease claim %s are being removed, retry", uid)
	}
	dir := b.leaseDir(uid)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0755); err != nil {
		return err
	}
	if err := b.makeFsDirs(uid); err != nil {
		return err
	}
	// The socket is made and given its mode where no container can
	// replace it by a symlink, then renamed into the claim's directory
	// (which containers of before its read-only mount can write): the
	// rename replaces whatever is at the path, and follows nothing.
	tmpDir := filepath.Join(b.opts.DataDir, leaseSocketTmpDirName)
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return err
	}
	tmp := filepath.Join(tmpDir, uid+".sock")
	_ = os.Remove(tmp)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listening on %s: %w", tmp, err)
	}
	// The socket is removed explicitly (CloseClaim): a driver that stops
	// must leave the path to the next instance.
	l.SetUnlinkOnClose(false)
	sock := filepath.Join(dir, leaseSocketName)
	if err := os.Chmod(tmp, 0666); err != nil {
		_ = l.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, sock); err != nil {
		_ = l.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("placing the lease socket %s: %w", sock, err)
	}
	SELinuxLabel(dir, sock)
	b.listeners[uid] = l
	b.wg.Add(1)
	go b.accept(uid, l)
	return nil
}

// makeFsDirs creates, for the lease claim uid, the mount point of
// api.FSDir in its socket directory and its lease-fs directory.
func (b *Broker) makeFsDirs(uid string) error {
	fsDir := b.leaseFsDir(uid)
	mp := filepath.Join(b.leaseDir(uid), leaseFsMountPoint)
	if err := mkdirNoFollow(mp); err != nil {
		return err
	}
	if err := os.MkdirAll(fsDir, 0755); err != nil {
		return err
	}
	if err := os.Chmod(fsDir, 0755); err != nil {
		return err
	}
	SELinuxLabel(mp, fsDir)
	return nil
}

// mkdirNoFollow makes p a directory of mode 0755 in a directory that a
// container may write: whatever else is at p (a symlink, a file) is
// removed first, and the mode is set on the directory opened without
// following a symlink.
func mkdirNoFollow(p string) error {
	if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	if err := os.Mkdir(p, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", p, err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fchmod(fd, 0755); err != nil {
		return fmt.Errorf("chmod %s: %w", p, err)
	}
	return nil
}

// SELinuxLabel labels the socket directory and socket for containers when
// SELinux is enabled. Errors are logged only.
func SELinuxLabel(paths ...string) {
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err != nil {
		return
	}
	for _, p := range paths {
		if err := unix.Lsetxattr(p, "security.selinux", []byte(selinuxContainerFileLabel), 0); err != nil {
			klog.Warningf("lease broker: cannot label %s %s: %v", p, selinuxContainerFileLabel, err)
		}
	}
}

// CDIEdits returns the container edits of the CDI device of the lease
// claim claimUID: its socket directory, its lease-fs directory and the
// CLI.
func (b *Broker) CDIEdits(claimUID string) cdiSpecs.ContainerEdits {
	edits := cdiSpecs.ContainerEdits{
		Env: []string{api.SocketEnv + "=" + api.DefaultSocket},
		// read-only: connect(2) needs write access to the socket only,
		// and the containers never write the host directory
		Mounts: []*cdiSpecs.Mount{{
			HostPath:      b.leaseDir(claimUID),
			ContainerPath: leaseContainerDir,
			Type:          "bind",
			Options:       []string{"rbind", "ro"},
		}},
	}
	edits.Mounts = append(edits.Mounts, &cdiSpecs.Mount{
		HostPath:      b.leaseFsDir(claimUID),
		ContainerPath: api.FSDir,
		Type:          "bind",
		Options:       []string{"rbind", "ro", "rslave"},
	})
	// A missing bind mount source fails the container creation, so the
	// CLI is mounted only if it exists. The socket works without it.
	if b.opts.CLI != "" {
		if _, err := os.Stat(b.opts.CLI); err == nil {
			edits.Mounts = append(edits.Mounts, &cdiSpecs.Mount{
				HostPath:      b.opts.CLI,
				ContainerPath: leaseCLIContainerPath,
				Type:          "bind",
				Options:       []string{"bind", "ro"},
			})
		} else {
			klog.Warningf("lease broker: cxl-request CLI %s not mounted into containers: %v", b.opts.CLI, err)
		}
	}
	return edits
}
