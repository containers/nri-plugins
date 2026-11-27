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

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// bindGrant binds the node mount of a companion grant of mode fs at its
// bind target, or finds it bound there, its intent named first.
func (e *Engine) bindGrant(ctx context.Context, req *GrantRequest, u pmemGrant) error {
	if !api.ValidName(filepath.Base(req.BindTarget)) {
		return fmt.Errorf("grant %s: invalid name %q", req.Ref, filepath.Base(req.BindTarget))
	}
	found, err := e.bindLease(&u, req.BindTarget, func(target string) bool { return e.bindUsed(target, req.Ref) })
	if err != nil {
		return err
	}
	what := "grant " + req.Ref.String()
	if req.Lend != nil {
		what = "lease " + req.Lend.Lease
	}
	if found {
		klog.Infof("engine: %s: %s is bound at %s already", what, u.nodeMount, req.BindTarget)
	} else {
		klog.Infof("engine: %s: bound %s at %s", what, u.nodeMount, req.BindTarget)
	}
	return nil
}

// BindError is the error of the lease bind of a grant of a companion
// claim of mode fs: of the bind, or of its target that the Prepare checks
// before.
type BindError struct {
	Err error
}

// Error returns the error of the bind.
func (e *BindError) Error() string {
	return e.Err.Error()
}

// Unwrap returns the error of the bind.
func (e *BindError) Unwrap() error {
	return e.Err
}

// bindUsed tells whether a prepared grant other than except has the lease
// bind target.
func (e *Engine) bindUsed(target string, except state.GrantRef) bool {
	r := e.records()
	for i := range r.Grants {
		g := &r.Grants[i]
		if g.Ref != except && g.Got.Bind != nil && g.Got.Bind.Target == target && prepared(g) {
			return true
		}
	}
	return false
}

// bindLease binds the node mount of a grant of mode fs at target and
// reports whether the bind was there already: read-only binds of writable
// types by a clone with the read-only attribute, others by MS_BIND, so that
// the bind and its propagated copies have the flags of the node mount. The
// bind must be a mount of the root of the node mount of its type with the
// flags ro (rw for a grant that writes), nosuid and nodev; a new bind
// without them is unmounted again; another mount at the target, or one that
// otherBind says is the bind of another prepared grant, is neither used nor
// unmounted. A new bind that fails the check and cannot be unmounted is
// recorded as stale.
func (e *Engine) bindLease(u *pmemGrant, target string, otherBind func(string) bool) (bool, error) {
	s := u.serial.String()
	node, err := e.k.Mounted(u.nodeMount)
	switch {
	case err != nil:
		return false, err
	case node == nil:
		return false, fmt.Errorf("memdev %s: %s is not mounted", s, u.nodeMount)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return false, err
	}
	mi, err := e.k.Mounted(target)
	if err != nil {
		return false, err
	}
	if mi != nil {
		return true, checkExistingBind(u, target, mi, node, otherBind)
	}
	err = e.locked(func() error {
		if u.readOnly && !config.FSTypeReadOnly(node.FSType) {
			return e.k.BindReadOnly(u.nodeMount, target)
		}
		return e.k.Mount(u.nodeMount, target, "", syscall.MS_BIND, "")
	})
	if err != nil {
		e.removeMountDir(target)
		return false, fmt.Errorf("memdev %s: binding %s at %s: %w", s, u.nodeMount, target, err)
	}
	return false, e.verifyBind(u, target, node)
}

// checkExistingBind returns an error unless the mount mi at target is the
// lease bind of the node mount node of the grant, and of no other prepared
// grant (otherBind).
func checkExistingBind(u *pmemGrant, target string, mi, node *kernel.MountInfo, otherBind func(string) bool) error {
	s := u.serial.String()
	switch {
	case !isLeaseBind(mi, node, u.readOnly):
		return fmt.Errorf("memdev %s: %s is a %s mount of %s, not of %s", s, target, mi.FSType, mi.Source, u.nodeMount)
	case otherBind != nil && otherBind(target):
		return fmt.Errorf("memdev %s: %s is the bind of another lease", s, target)
	}
	return nil
}

// verifyBind returns an error, after it unmounted the new bind at target
// again (or recorded it stale when that fails), when the bind is not the
// lease bind of the node mount node of the grant.
func (e *Engine) verifyBind(u *pmemGrant, target string, node *kernel.MountInfo) error {
	s := u.serial.String()
	mi, err := e.k.Mounted(target)
	if err == nil && (mi == nil || !isLeaseBind(mi, node, u.readOnly)) {
		err = fmt.Errorf("memdev %s: the bind of %s at %s is not a %s nosuid nodev %s mount", s, u.nodeMount, target, kernel.RoWord(u.readOnly), node.FSType)
	}
	if err == nil {
		return nil
	}
	if uerr := e.locked(func() error { return e.k.Unmount(target) }); uerr != nil {
		klog.Errorf("engine: %s memdev %s: unmounting %s: %v; trying again at the next release", u.source, s, target, uerr)
		e.addStale(u.serial, target)
	} else {
		e.removeMountDir(target)
	}
	return err
}

// isLeaseBind tells whether a mount is a bind of the node mount node with
// the flags of a lease bind, ro or rw by readOnly.
func isLeaseBind(mi, node *kernel.MountInfo, readOnly bool) bool {
	if mi.FSType != node.FSType || mi.Source != node.Source || mi.Root != node.Root {
		return false
	}
	for _, f := range []string{roWord(readOnly), "nosuid", "nodev"} {
		if !slices.Contains(mi.MountFlags, f) {
			return false
		}
	}
	return true
}

// leaseBindType tells whether a mount may be a lease bind: of a block
// device, of a filesystem type that mode fs mounts.
func (e *Engine) leaseBindType(mi *kernel.MountInfo) bool {
	return strings.HasPrefix(mi.Source, e.devRoot()+"/") && (api.KnownFSType(mi.FSType) || slices.Contains(e.opts.FSTypes, mi.FSType))
}

// unbindGrant unbinds the lease bind of a companion grant. A bind that is
// busy (a file of it mapped) becomes STALE, recorded with its memdev, and
// the release fails.
func (e *Engine) unbindGrant(ctx context.Context, g *state.Grant, trigger state.Trigger) error {
	b := g.Got.Bind
	if b == nil {
		return e.unbindCut(g)
	}
	if b.State != state.BindBound && b.State != state.BindStale {
		return nil
	}
	if e.bindUsed(b.Target, g.Ref) {
		klog.Warningf("engine: %s: %s is the bind of another prepared claim, not unmounting it", bindWhat(g), b.Target)
		return e.commit(ctx, func(r *state.Records) error { return boundTo(r, g.Ref, b.Target, state.BindAbsent, trigger) })
	}
	err := e.unbind(g.Asked.Source, bindWhat(g), b.Target)
	to := state.BindAbsent
	if err != nil {
		if b.State == state.BindStale || !errors.Is(err, unix.EBUSY) {
			return err
		}
		to = state.BindStale
	}
	cerr := e.commit(ctx, func(r *state.Records) error { return boundTo(r, g.Ref, b.Target, to, trigger) })
	return errors.Join(err, cerr)
}

// bindWhat returns what the log lines of the lease bind of a grant name: its
// lease, else the grant.
func bindWhat(g *state.Grant) string {
	if l := g.Got.Lend; l != nil {
		return "lease " + l.Lease
	}
	if l := g.Intent.Lend; l != nil {
		return "lease " + l.Lease
	}
	return "grant " + g.Ref.String()
}

// unbindCut unbinds the lease bind that the intent of a grant of a cut
// Prepare names, when a lease bind is mounted there: the bind is made
// before the grant records it.
func (e *Engine) unbindCut(g *state.Grant) error {
	target := g.Intent.BindTarget
	if target == "" || (g.State != state.GrantJournaled && g.State != state.GrantRealising) {
		return nil
	}
	mi, err := e.k.Mounted(target)
	if err != nil || mi == nil || !e.leaseBindType(mi) {
		return err
	}
	return e.unbind(g.Asked.Source, bindWhat(g), target)
}

// boundTo records the lease bind of a grant in state to (ABSENT, or STALE
// with its memdev's StaleBinds entry).
func boundTo(r *state.Records, ref state.GrantRef, target string, to state.BindState, trigger state.Trigger) error {
	rg := r.Grant(ref)
	if rg == nil || rg.Got.Bind == nil {
		return nil
	}
	if err := fsm.Apply(&state.BindTable, &rg.Got.Bind.State, to, trigger); err != nil {
		return err
	}
	if m := r.Memdev(rg.Serial); m != nil {
		if to == state.BindStale {
			m.StaleBinds = addRef(m.StaleBinds, target)
		} else {
			m.StaleBinds = removeRef(m.StaleBinds, target)
		}
	}
	return nil
}

// unbind unmounts a lease bind target, never detaching it, and removes its
// directory. A bind that cannot be unmounted (busy while the requester has
// files of it open or mapped), or that has another mount on it than a
// lease bind, is an error.
func (e *Engine) unbind(source state.Source, what, target string) error {
	for i := 0; ; i++ {
		mi, err := e.k.Mounted(target)
		if err == nil && mi == nil {
			break
		}
		if err == nil && !e.leaseBindType(mi) {
			return fmt.Errorf("%s: %s is a %s mount of %s, not a lease bind", what, target, mi.FSType, mi.Source)
		}
		if err == nil && i >= maxUnbinds {
			err = errors.New("still mounted")
		}
		if err == nil {
			err = e.locked(func() error { return e.k.Unmount(target) })
		}
		if err != nil {
			return fmt.Errorf("%s: unmounting %s: %w", what, target, err)
		}
		klog.Infof("engine: %s: unbound %s", what, target)
	}
	e.removeMountDir(target)
	return nil
}

// unbindStale unmounts a stale lease bind once, when it is one.
func (e *Engine) unbindStale(source state.Source, target string) error {
	mi, err := e.k.Mounted(target)
	switch {
	case err != nil, mi == nil:
	case !e.leaseBindType(mi):
		err = fmt.Errorf("a %s mount of %s is on it", mi.FSType, mi.Source)
	default:
		if err = e.locked(func() error { return e.k.Unmount(target) }); err == nil {
			klog.Infof("engine: unbound stale lease bind %s", target)
		}
	}
	if err != nil {
		return err
	}
	e.removeMountDir(target)
	return nil
}

// addStale records a node mount or lease bind of the memdev with the
// serial whose unmount failed, once, in a lazy commit; a memdev without a
// record keeps nothing.
func (e *Engine) addStale(serial state.Serial, path string) {
	err := e.commitAs(context.Background(), store.Lazy, func(r *state.Records) error {
		if m := r.Memdev(serial); m != nil {
			m.StaleBinds = addRef(m.StaleBinds, path)
		}
		return nil
	})
	if err != nil {
		klog.Errorf("engine: memdev %s: recording stale lease bind %s: %v", serial, path, err)
	}
}

// RetryStaleBinds unmounts the stale lease binds of every memdev and the lease
// binds of no memdev again, as every release does first, and finishes the
// releases that they kept, with the transitions of the trigger: an
// Unprepare for the periodic pass, a Prepare for a refused one. Errors are
// logged.
func (e *Engine) RetryStaleBinds(ctx context.Context, trigger state.Trigger) {
	ctx, end := e.Request(ctx)
	defer end()
	e.retryAllStale(ctx, trigger)
}

// retryAllStale unmounts the stale lease binds of every memdev of the
// sources of the engine that no prepared grant lists again, in the queue of
// each memdev that no other request holds, and forgets those that are
// gone; then the grants of the memdev that a stale bind left RELEASING are
// released again when no other grant of a claim uses the memdev, and a
// pool memdev without grants is released (its holder wait, its region,
// disabled). Errors are logged.
func (e *Engine) retryAllStale(ctx context.Context, trigger state.Trigger) {
	for _, m := range e.records().Memdevs {
		if len(m.StaleBinds) == 0 || !e.owns(m.Source) {
			continue
		}
		e.retryStale(ctx, m.Serial, trigger)
	}
	e.retryUnownedBinds()
}

// addUnownedBind keeps a lease bind of no memdev whose unmount failed, once,
// for the next release.
func (e *Engine) addUnownedBind(target string) {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	if !slices.Contains(e.unownedBinds, target) {
		e.unownedBinds = append(e.unownedBinds, target)
	}
}

// retryUnownedBinds unmounts again the lease binds of no memdev that a start
// could not unmount, and forgets those that are gone or that a prepared
// grant lists again; the others are tried again at the next release.
func (e *Engine) retryUnownedBinds() {
	e.qmu.Lock()
	binds := e.unownedBinds
	e.unownedBinds = nil
	e.qmu.Unlock()
	var left []string
	for _, target := range binds {
		if e.bindUsed(target, state.GrantRef{}) {
			continue
		}
		if err := e.unbindStale(state.SourceLocal, target); err != nil {
			klog.Errorf("engine: unmounting stale lease bind %s: %v", target, err)
			left = append(left, target)
		}
	}
	for _, target := range left {
		e.addUnownedBind(target)
	}
}

// retryStale unmounts the stale lease binds of the memdev with the serial
// in its queue, when no other request of the memdev is in progress (its
// next release tries again), and forgets those that are gone or that a
// prepared grant lists again; when none is left, the memdev is released
// after them.
func (e *Engine) retryStale(ctx context.Context, serial state.Serial, trigger state.Trigger) {
	leave := e.tryAcquire(serial)
	if leave == nil {
		return
	}
	defer leave()
	m := e.records().Memdev(serial)
	if m == nil {
		return
	}
	source := m.Source
	var forget []string
	gone, left := false, false
	for _, path := range m.StaleBinds {
		if e.bindUsed(path, state.GrantRef{}) {
			// a Prepare again bound it for its grant
			forget = append(forget, path)
			continue
		}
		if err := e.unbindStale(source, path); err != nil {
			klog.Errorf("engine: unmounting stale lease bind %s: %v", path, err)
			left = true
			continue
		}
		forget = append(forget, path)
		gone = true
	}
	if len(forget) == 0 {
		return
	}
	if err := e.commit(ctx, func(r *state.Records) error { return forgetStale(r, serial, forget, trigger) }); err != nil {
		klog.Errorf("engine: %s: %v", memdevText(source, serial), err)
		return
	}
	if gone && !left {
		e.releaseAfterStale(ctx, serial, trigger)
	}
}

// releaseAfterStale releases the memdev with the serial after its stale
// lease binds are gone, its queue held, when no grant of a claim uses it
// any more: its RELEASING grants, each as the release that it was in, else
// the memdev itself; the record of a memdev of which nothing is left goes.
// Errors are logged.
func (e *Engine) releaseAfterStale(ctx context.Context, serial state.Serial, trigger state.Trigger) {
	r := e.records()
	m := r.Memdev(serial)
	if m == nil {
		return
	}
	var refs []state.GrantRef
	for _, ref := range m.Grants {
		if g := r.Grant(ref); g == nil || g.State != state.GrantReleasing {
			return
		}
		refs = append(refs, ref)
	}
	source := m.Source
	call := newReleaseCall(e, ReleaseUnprepare, refs)
	call.trigger = trigger
	var errs []error
	for _, ref := range refs {
		call.release = releaseOf(r.PreparationOf(ref))
		errs = append(errs, e.releaseGrant(ctx, ref, call))
	}
	switch {
	case len(refs) == 0 && source == state.SourcePool:
		errs = append(errs, e.releaseIdle(ctx, m, e.idleKind(m, false), call.holderDeadline(""), trigger))
	case len(refs) == 0 && m.Facts.Persistent:
		errs = append(errs, e.releaseIdleQueued(ctx, m, trigger))
	}
	e.logAfterStale(source, serial, errs)
	if err := e.commit(ctx, func(r *state.Records) error {
		if m := r.Memdev(serial); m != nil && unused(m) {
			return dropMemdev(r, serial, trigger)
		}
		return nil
	}); err != nil {
		klog.Errorf("engine: %s: %v", memdevText(source, serial), err)
	}
}

// releaseOf returns the release that a RELEASING grant of the preparation p
// was in: a rollback for a claim whose Prepare did not finish, else an
// Unprepare.
func releaseOf(p *state.Preparation) Release {
	if p != nil && (p.State == state.PreparationPreparing || p.State == state.PreparationKept) {
		return ReleaseRollback
	}
	return ReleaseUnprepare
}

// logAfterStale logs the errors of the release of the memdev with the
// serial after its stale lease binds: a block device that stayed held, and
// the other errors.
func (e *Engine) logAfterStale(source state.Source, serial state.Serial, errs []error) {
	var held, failed []error
	for _, err := range errs {
		if err == nil {
			continue
		}
		cause := err
		var re *releaseError
		if errors.As(err, &re) && re.serial == serial {
			cause = re.err
		}
		if errors.Is(err, errHolder) {
			held = append(held, cause)
		} else {
			failed = append(failed, cause)
		}
	}
	if len(held) > 0 {
		klog.Errorf("engine: not releasing %s after its stale lease bind: %v", memdevText(source, serial), errors.Join(held...))
	}
	if len(failed) > 0 {
		klog.Errorf("engine: releasing %s after its stale lease bind: %v", memdevText(source, serial), errors.Join(failed...))
	}
}

// forgetStale removes the stale lease binds gone of the memdev with the
// serial, the binds of their grants that are STALE ABSENT (a BOUND one of
// a grant that lists it again stays).
func forgetStale(r *state.Records, serial state.Serial, gone []string, trigger state.Trigger) error {
	m := r.Memdev(serial)
	if m == nil {
		return nil
	}
	for _, path := range gone {
		m.StaleBinds = removeRef(m.StaleBinds, path)
		for i := range r.Grants {
			if b := r.Grants[i].Got.Bind; b != nil && b.Target == path && b.State == state.BindStale {
				if err := fsm.Apply(&state.BindTable, &b.State, state.BindAbsent, trigger); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
