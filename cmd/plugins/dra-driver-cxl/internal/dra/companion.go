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

package dra

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/lend"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// Companion claims: a claim with the label cxl.nri.io/lease=<id> is the
// companion of a lease of the lease broker, whose running container gets
// the memory of the claim's devices (docs/memory/cxl/leases.md). Either
// driver prepares it like any claim, but without CDI devices, after the
// companion gate:
//   - the broker has the lease, the claim is cxl-lease-<id> in the
//     requester's namespace, owned by the requester pod, and the lease is
//     CREATING, PENDING, LENDING or READY (LeaseHooks.OnLending);
//   - on the first Prepare of the lease (CREATING or PENDING), the spec of
//     the claim has the hash that the broker recorded when it made the
//     claim, if it recorded one (SpecHasher);
//   - lending is enabled and NRI is connected;
//   - cxl.nri.io: every result of the claim is a local memdev.
// The NUMA nodes of its devices in mode numa are lent to the requester (a
// grant records its lend before it is lent), its device of mode fs is bound
// into the lease claim's directory by the engine, and the broker gets the
// memdevs (state.DeviceInfo) when the claim is prepared.

// NUMALender lends NUMA nodes to running containers.
type NUMALender interface {
	// LendNUMANode lends the NUMA node to the container for the lease.
	// Idempotent per NUMA node and lease.
	LendNUMANode(ctx context.Context, podUID, name string, numaNode int, lease string) error
	// RevokeNUMANode drops the lease of the NUMA node; the NUMA node leaves
	// the container's cpuset.mems when it has no lease left. Idempotent; a
	// container that is gone is not an error.
	RevokeNUMANode(ctx context.Context, podUID, name string, numaNode int, lease string) error
}

var (
	// errLendingStarting is the error of a revoke before the lender is set
	// up: kubelet retries the Unprepare.
	errLendingStarting = errors.New("lending is not set up yet (the driver is starting)")
)

// leaseOf returns the lease id of a companion claim (its label
// device.LeaseLabel), "" for other claims.
func leaseOf(claim *resourcev1.ResourceClaim) string {
	return claim.Labels[device.LeaseLabel]
}

// lender lends the companion claims of one driver to running containers.
// A nil or zero lender has lending disabled.
//
// Identity: one per DRA driver.
//
// States: a two-state in-memory latch isSet, not a State field: unset (Set
// not called: NRI may connect yet, lent nodes are not revoked) and set;
// lending disabled (no hooks) or enabled, NRI connected (nodes) or not.
//
// Transitions: unset -> set by Set, once at the start; never back.
//
// Invariants: a revoke before Set fails.
//
// Persistence: none; the lends are in the grants of the companion claims.
//
// Locks and waits: mu guards the fields, never held across a hook or a lend.
type lender struct {
	driver string
	// mu guards the fields below: a leaf lock.
	mu    sync.Mutex
	nodes NUMALender      // nil: NRI not connected
	hooks lend.LeaseHooks // nil: lending disabled
	isSet bool
}

// newLender returns the lender of the companion claims of driver, with
// lending disabled until Set is called.
func newLender(driver string) *lender {
	return &lender{driver: driver}
}

// Set enables lending: nodes lends NUMA nodes (nil when NRI is not
// connected, then companion claims fail), hooks is the lease broker (nil
// disables lending).
func (c *lender) Set(nodes NUMALender, hooks lend.LeaseHooks) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes = nodes
	c.hooks = hooks
	c.isSet = true
}

// get returns the node lender and the hooks.
func (c *lender) get() (NUMALender, lend.LeaseHooks) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nodes, c.hooks
}

// Check is the companion gate of the lease broker for a companion claim of
// the lease id, before any device is touched: it marks the lease Lending and
// returns the lend of the lease's container.
func (c *lender) Check(id string, claim *resourcev1.ResourceClaim) (*lend.Companion, error) {
	nodes, hooks := c.get()
	if hooks == nil {
		return nil, fmt.Errorf("lease %s: lending to running containers is disabled (--leases=false)", id)
	}
	if nodes == nil {
		return nil, fmt.Errorf("lease %s: %w", id, lend.ErrLendingUnavailable)
	}
	if err := checkSpecHash(hooks, id, claim); err != nil {
		return nil, err
	}
	return hooks.OnLending(id, claim)
}

// checkSpecHash returns an error if the hooks recorded the hash of the spec
// of the companion claim of the lease id, the lease waits for its first
// Prepare, and the claim's spec has another hash.
func checkSpecHash(hooks lend.LeaseHooks, id string, claim *resourcev1.ResourceClaim) error {
	h, ok := hooks.(lend.SpecHasher)
	if !ok {
		return nil
	}
	want, first := h.CompanionSpecHash(id)
	if !first || want == "" {
		return nil
	}
	if got := lend.SpecHash(&claim.Spec); got != want {
		return fmt.Errorf("claim %s/%s: not the companion of lease %s: its spec is not the one made for the lease", claim.Namespace, claim.Name, id)
	}
	return nil
}

// Lend lends the NUMA nodes of the numa devices of a companion claim to the
// container of the lend, held by its lease, and records them in rec. Dax
// and block devices are passed as an fd by the broker. On error rec lists
// the nodes lent so far.
func (c *lender) Lend(ctx context.Context, rec *lend.Companion, devs []state.DeviceInfo) error {
	nodes, _ := c.get()
	for i := range devs {
		node := deviceNUMANode(&devs[i])
		if node < 0 {
			continue
		}
		if nodes == nil {
			return fmt.Errorf("lease %s: %w", rec.Lease, lend.ErrLendingUnavailable)
		}
		rec.NUMANodes = append(rec.NUMANodes, node)
		if err := nodes.LendNUMANode(ctx, rec.Pod.UID, rec.Container, node, rec.Lease); err != nil {
			return fmt.Errorf("lease %s: lending NUMA node %d to %s/%s/%s: %w", rec.Lease, node, rec.Pod.Namespace, rec.Pod.Name, rec.Container, err)
		}
		klog.Infof("%s: lease %s: lent NUMA node %d to %s/%s/%s", c.driver, rec.Lease, node, rec.Pod.Namespace, rec.Pod.Name, rec.Container)
	}
	return nil
}

// Revoke revokes the lent nodes of a lend for its lease: a node that
// another lease of the container holds stays lent. Without NRI the nodes
// cannot be revoked: the release then migrates the pages anyway, but the
// stale node stays in the container's cpuset.mems. Before the lender is set
// up it is an error.
func (c *lender) Revoke(ctx context.Context, rec *lend.Companion, claimName string) error {
	if rec == nil || len(rec.NUMANodes) == 0 {
		return nil
	}
	if !c.ready() {
		return fmt.Errorf("lease %s: %w: nodes %v of %s/%s/%s not revoked yet", rec.Lease, errLendingStarting, rec.NUMANodes, rec.Pod.Namespace, rec.Pod.Name, rec.Container)
	}
	nodes, _ := c.get()
	if nodes == nil {
		klog.Warningf("%s: claim %s: lease %s: %v, cannot revoke nodes %v from %s/%s/%s",
			c.name(), claimName, rec.Lease, lend.ErrLendingUnavailable, rec.NUMANodes, rec.Pod.Namespace, rec.Pod.Name, rec.Container)
		return nil
	}
	var errs []error
	for _, n := range rec.NUMANodes {
		if err := nodes.RevokeNUMANode(ctx, rec.Pod.UID, rec.Container, n, rec.Lease); err != nil {
			errs = append(errs, fmt.Errorf("lease %s: revoking NUMA node %d from %s/%s/%s: %w", rec.Lease, n, rec.Pod.Namespace, rec.Pod.Name, rec.Container, err))
		}
	}
	return errors.Join(errs...)
}

// preparedStep returns the change of the records that tells the broker that
// the companion claim of a lease is prepared and lent, with its memdevs of
// the source, to commit with its preparation PREPARED, and what follows the
// commit: Prepared, when the broker takes no change.
func (c *lender) preparedStep(id string, source state.Source, devs []state.DeviceInfo) (func(r *state.Records), func()) {
	if _, hooks := c.get(); hooks != nil {
		if s, ok := hooks.(lend.PreparedStepper); ok {
			return s.PreparedStep(id, source, devs)
		}
	}
	return func(*state.Records) {}, func() { c.Prepared(id, source, devs) }
}

// Prepared tells the broker that the companion claim of a lease is prepared
// and lent, with its memdevs of the source.
func (c *lender) Prepared(id string, source state.Source, devs []state.DeviceInfo) {
	if _, hooks := c.get(); hooks != nil {
		hooks.OnPrepared(id, source, devs)
	}
}

// PreparedAgain tells the broker again that a stored companion claim is
// prepared and lent, as kubelet asks to prepare a prepared claim again.
func (c *lender) PreparedAgain(rec *lend.Companion, claim *resourcev1.ResourceClaim, source state.Source, devs []state.DeviceInfo) {
	_, hooks := c.get()
	if hooks == nil || rec == nil {
		return
	}
	if _, err := hooks.OnLending(rec.Lease, claim); err == nil {
		hooks.OnPrepared(rec.Lease, source, devs)
	}
}

// Failed tells the broker that lending the companion claim of a lease
// failed.
func (c *lender) Failed(id string, err error) {
	if _, hooks := c.get(); hooks != nil {
		hooks.OnLendFailed(id, err)
	}
}

// Releasing tells the broker that the companion claim of a lease is being
// unprepared.
func (c *lender) Releasing(id string) {
	if _, hooks := c.get(); hooks != nil {
		hooks.OnReleasing(id)
	}
}

// ready tells whether the lender has been set up (Set). A nil lender is.
func (c *lender) ready() bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isSet
}

// name returns the driver name for logs.
func (c *lender) name() string {
	if c == nil {
		return ""
	}
	return c.driver
}

// RevokeGrant revokes the NUMA node that one grant of a companion claim
// lends, for the engine's rollback of a later Prepare of the claim.
func (c *lender) RevokeGrant(ctx context.Context, gl state.GrantLend, claimName string) error {
	rec := &lend.Companion{Lease: gl.Lease, Pod: gl.Pod, Container: gl.Container}
	if gl.NUMANode != nil {
		rec.NUMANodes = []int{*gl.NUMANode}
	}
	return c.Revoke(ctx, rec, claimName)
}

// leaseBindTarget returns the lease bind of the device of mode fs of a
// companion claim, <FSDir>/<Name> of its lend.
func leaseBindTarget(lend *lend.Companion) (string, error) {
	switch {
	case lend.Name == "":
		return "", fmt.Errorf("lease %s: the device is of mode %s, the lease is not", lend.Lease, api.ModeFS)
	case lend.FSDir == "":
		return "", fmt.Errorf("lease %s: its lease claim has no filesystem directory", lend.Lease)
	case !api.ValidName(lend.Name):
		return "", fmt.Errorf("lease %s: invalid name %q", lend.Lease, lend.Name)
	}
	return filepath.Join(lend.FSDir, lend.Name), nil
}
