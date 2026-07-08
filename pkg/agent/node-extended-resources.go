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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// extResResyncInterval bounds how often the reconciler re-checks
// the Node against the desired state without an explicit request.
// It retries transient API failures and heals external drift.
const extResResyncInterval = 30 * time.Second

// extResRetrySyncInterval is time between reconciliation rounds
// when previous round failed. Avoids flooding the API server.
const extResRetrySyncInterval = 5 * time.Second

// nriExtendedResourceGlob restricts which extended resources this
// agent is ever allowed to publish or remove. Regardless of what a
// policy claims to own or wants to publish, only resources whose
// name matches this pattern can be changed.
const nriExtendedResourceGlob = "*.nri.io/*"

// isNRIExtendedResource reports whether 'name' is an extended
// resource this agent is allowed to manage.
func isNRIExtendedResource(name string) bool {
	return globMatch(nriExtendedResourceGlob, name)
}

// UpdateNodeExtendedResources records 'resources' as the desired
// state of the local Node's status.capacity and wakes the
// reconciler to take care of status change.
//
// 'resources' must describe the *complete* desired state:
//
//   - A non-nil value publishes (adds/replaces) the named resource
//     with the given capacity.
//   - A nil value whose key contains no '*' removes that exact
//     resource if it is currently present on the Node.
//   - A nil value whose key contains '*' is an ownership pattern:
//     every resource currently on the Node that matches the
//     pattern but is not being published is removed.
//
// If the resources map is empty or nil, nothing is to be published or
// removed, and therefore querying and modifying Node status/capacity
// is omitted.
//
// Because every request carries the full desired state, a newer
// request fully supersedes older ones; the reconciler therefore
// coalesces bursts and only ever drives the Node towards the most
// recent request.
func (a *Agent) UpdateNodeExtendedResources(resources map[string]*resource.Quantity) error {
	if a.hasLocalConfig() {
		return nil
	}
	if a.k8sCli == nil || a.nodeName == "" {
		return nil
	}

	snapshot := make(map[string]*resource.Quantity, len(resources))
	for k, v := range resources {
		if v == nil {
			snapshot[k] = nil
			continue
		}
		q := v.DeepCopy()
		snapshot[k] = &q
	}

	a.extResLock.Lock()
	a.extResWant = snapshot
	a.extResLock.Unlock()

	// Coalescing wakeup: if a signal is already pending, the
	// reconciler will pick up the latest desired state anyway.
	select {
	case a.extResWake <- struct{}{}:
	default:
	}
	return nil
}

// reconcileExtendedResourcesLoop is the single long-lived worker
// that drives the Node's status.capacity towards the most recently
// requested desired state. Running as a single goroutine, it
// guarantees in-order, non-overlapping reconciles (no last-writer
// race) and at most one in-flight GET+PATCH. It reconciles on every
// wakeup and periodically, so transient API failures and external
// drift are corrected even without new requests.
func (a *Agent) reconcileExtendedResourcesLoop() {
	timer := time.NewTimer(extResResyncInterval)
	defer timer.Stop()

	for {
		select {
		case <-a.stopC:
			return
		case <-a.extResWake:
		case <-timer.C:
		}

		// We are about to reconcile, so cancel any pending timer
		// fire and recompute the next deadline from now below. This
		// drains the channel if the timer already fired but we were
		// woken by something else, keeping the single-fire invariant.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		nextInterval := extResResyncInterval

		a.extResLock.Lock()
		resources := a.extResWant
		a.extResLock.Unlock()
		if len(resources) > 0 {
			// Only touch the API server when there is a desired
			// state to reconcile towards.
			if err := a.reconcileNodeExtendedResources(); err != nil {
				log.Errorf("failed to reconcile extended resources: %v", err)
				nextInterval = extResRetrySyncInterval
			}
		}

		timer.Reset(nextInterval)
	}
}

// reconcileNodeExtendedResources computes and applies the JSON
// patch that brings Node.status.capacity in line with the most
// recently requested desired state.
func (a *Agent) reconcileNodeExtendedResources() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node, err := a.k8sCli.CoreV1().Nodes().Get(ctx, a.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get extended resources: get node %s: %w", a.nodeName, err)
	}

	// Snapshot the latest desired state now that we hold fresh
	// node status.
	a.extResLock.Lock()
	resources := a.extResWant
	a.extResLock.Unlock()
	if len(resources) == 0 {
		return nil
	}

	// Split the input into the things we want published, the
	// exact keys we want gone, and the ownership patterns that
	// tell us which additional (possibly unknown) keys on the
	// Node we are responsible for pruning. Names outside our
	// ownership domain are refused here and never acted upon.
	publish := map[string]string{}
	exactRemove := map[string]bool{}
	ownGlobs := []string{}
	for name, qty := range resources {
		if qty != nil {
			if strings.Contains(name, "*") {
				log.Errorf("ignoring extended resource %q: wildcards are only allowed on owned (nil-valued) keys", name)
				continue
			}
			if !isNRIExtendedResource(name) {
				log.Errorf("refusing to publish extended resource %q: name does not match %q", name, nriExtendedResourceGlob)
				continue
			}
			publish[name] = qty.String()
			continue
		}
		if strings.Contains(name, "*") {
			ownGlobs = append(ownGlobs, name)
			continue
		}
		if !isNRIExtendedResource(name) {
			log.Errorf("refusing to remove extended resource %q: name does not match %q", name, nriExtendedResourceGlob)
			continue
		}
		exactRemove[name] = true
	}

	current := map[string]string{}
	for name, q := range node.Status.Capacity {
		current[string(name)] = q.String()
	}

	// Determine which currently-present keys must be removed:
	// exact removals plus every key matched by an ownership glob
	// that we are not publishing. Regardless of what the policy
	// asked for, never touch a key outside our ownership domain.
	removeSet := map[string]bool{}
	for name := range current {
		if !isNRIExtendedResource(name) {
			continue
		}
		if _, keep := publish[name]; keep {
			continue
		}
		if exactRemove[name] {
			removeSet[name] = true
			continue
		}
		for _, glob := range ownGlobs {
			if globMatch(glob, name) {
				removeSet[name] = true
				break
			}
		}
	}

	// Now we have the current state, the desired state, and the
	// set of keys to remove. Build a single JSON merge patch that
	// will bring the Node into compliance.
	updatedCapacity := map[string]any{}
	for name, newValue := range publish {
		if curValue, ok := current[name]; ok && curValue == newValue {
			continue
		}
		updatedCapacity[name] = newValue
	}
	for name := range removeSet {
		updatedCapacity[name] = nil
	}
	if len(updatedCapacity) == 0 {
		return nil
	}

	body, err := json.Marshal(map[string]any{
		"status": map[string]any{
			"capacity": updatedCapacity,
		},
	})
	if err != nil {
		log.Warnf("failed to marshal update-resources patch: %v", err)
		return err
	}

	mergeCtx, mergeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer mergeCancel()
	_, err = a.k8sCli.CoreV1().Nodes().Patch(
		mergeCtx, a.nodeName, types.MergePatchType, body,
		metav1.PatchOptions{}, "status")
	if err != nil {
		return fmt.Errorf("patch node %s status: %w", a.nodeName, err)
	}

	if s := summarizeResources(publish); s != "" {
		log.Infof("published node extended resources: %s", s)
	}
	if len(removeSet) > 0 {
		log.Infof("removed node extended resources: %s", strings.Join(slices.Sorted(maps.Keys(removeSet)), ", "))
	}
	return nil
}

func globMatch(pattern, name string) bool {
	matched, err := path.Match(pattern, name)
	if err != nil {
		log.Errorf("invalid extended resources glob pattern %q: %v", pattern, err)
	}
	return matched
}

// summarizeResources formats a name->value map deterministically
// for logs: "name1=v1, name2=v2, ...".
func summarizeResources(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s=%s", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
