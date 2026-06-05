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
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// extendedResourcesLock serializes concurrent node.status PATCHes
// emitted by the policy on container events. Last writer wins.
var extendedResourcesLock sync.Mutex

// lastPublishedExtendedResources tracks the resources we currently
// own on this node, so that we can issue 'remove' patches for
// resources that the policy stops reporting.
var lastPublishedExtendedResources = map[string]int64{}

// extendedResourcesSynced is set after the first successful
// node-status scan. Until then, every publish will first try to
// seed lastPublishedExtendedResources from the node so that
// resources left over by a prior plugin process (helm reinstall,
// pod crash, switch to a different policy, etc.) get pruned by
// the regular diff logic on the next publish.
var extendedResourcesSynced bool

// extendedResourceDomain is the per-domain prefix the agent owns.
// Only resources whose name starts with this prefix are touched
// by the agent (other extended resources advertised by other
// controllers are left alone).
const extendedResourceDomain = "cpuclass.balloons.nri.io/"

// UpdateNodeExtendedResources publishes the given resource map
// to Node.status.capacity using a JSON patch. Resources previously
// owned by the agent but absent from 'resources' are removed.
// Runs asynchronously to avoid stalling NRI request paths.
func (a *Agent) UpdateNodeExtendedResources(resources map[string]int64) error {
	if a.hasLocalConfig() {
		return nil
	}
	if a.k8sCli == nil || a.nodeName == "" {
		return nil
	}
	// Snapshot inputs and run in the background; node-status
	// PATCHes can be slow under apiserver load and we never
	// want NRI hooks to block on them.
	snapshot := make(map[string]int64, len(resources))
	for k, v := range resources {
		snapshot[k] = v
	}
	go func() {
		if err := a.updateNodeExtendedResources(snapshot); err != nil {
			log.Errorf("failed to publish extended resources: %v", err)
		}
	}()
	return nil
}

func (a *Agent) updateNodeExtendedResources(resources map[string]int64) error {
	extendedResourcesLock.Lock()
	defer extendedResourcesLock.Unlock()

	// First call after process start: scan the node for keys we
	// already own (from a prior plugin process), so the diff
	// below can prune any that the current policy no longer
	// publishes. Failure is non-fatal -- we just fall back to
	// "trust our in-memory state".
	if !extendedResourcesSynced {
		if err := a.syncExtendedResourcesFromNode(); err != nil {
			log.Warnf("extended-resource startup sync failed (orphans from a prior plugin process may persist): %v", err)
		}
		extendedResourcesSynced = true
	}

	// Compute the patch: add/replace keys present in 'resources',
	// remove keys we owned before but are now gone.
	type jsonPatchOp struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value,omitempty"`
	}

	ops := []jsonPatchOp{}
	for name, qty := range resources {
		if !strings.HasPrefix(name, extendedResourceDomain) {
			log.Warnf("refusing to publish resource %q: not in domain %q",
				name, extendedResourceDomain)
			continue
		}
		q := resource.NewQuantity(qty, resource.DecimalSI)
		ops = append(ops, jsonPatchOp{
			Op:    "add",
			Path:  "/status/capacity/" + escapeJSONPointer(name),
			Value: q.String(),
		})
	}
	for name := range lastPublishedExtendedResources {
		if _, kept := resources[name]; kept {
			continue
		}
		ops = append(ops, jsonPatchOp{
			Op:   "remove",
			Path: "/status/capacity/" + escapeJSONPointer(name),
		})
	}

	if len(ops) == 0 {
		return nil
	}

	body, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	ctx := context.Background()
	_, err = a.k8sCli.CoreV1().Nodes().Patch(
		ctx, a.nodeName, types.JSONPatchType, body,
		metav1.PatchOptions{}, "status")
	if err != nil {
		// JSON patch "add" on a missing path fails when the
		// node has no prior resource of that name -- 'add'
		// requires the parent to exist, but for a map value
		// it should create the key. In practice apiservers
		// behave correctly here. If we ever hit issues, fall
		// back to a strategic merge patch.
		return fmt.Errorf("patch node %s status: %w", a.nodeName, err)
	}

	// Record current set for next diff.
	lastPublishedExtendedResources = make(map[string]int64, len(resources))
	for k, v := range resources {
		lastPublishedExtendedResources[k] = v
	}

	publishedSummary := summarizeExtendedResources(resources)
	if publishedSummary != "" {
		log.Infof("published node extended resources: %s", publishedSummary)
	}
	return nil
}

// escapeJSONPointer escapes '~' and '/' per RFC 6901 so that a
// resource name containing slashes survives as a single JSON
// Pointer segment.
func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// summarizeExtendedResources formats the map deterministically
// for logs: "name1=N1, name2=N2, ...".
func summarizeExtendedResources(m map[string]int64) string {
	if len(m) == 0 {
		return ""
	}
	sortedKeys := slices.Sorted(maps.Keys(m))
	parts := make([]string, 0, len(sortedKeys))
	for _, k := range sortedKeys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// syncExtendedResourcesFromNode reads Node.status.capacity and
// seeds lastPublishedExtendedResources with every entry whose
// key carries extendedResourceDomain. Caller must hold
// extendedResourcesLock.
func (a *Agent) syncExtendedResourcesFromNode() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node, err := a.k8sCli.CoreV1().Nodes().Get(ctx, a.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", a.nodeName, err)
	}
	owned := map[string]int64{}
	for name, q := range node.Status.Capacity {
		key := string(name)
		if !strings.HasPrefix(key, extendedResourceDomain) {
			continue
		}
		v, ok := q.AsInt64()
		if !ok {
			v = q.Value()
		}
		owned[key] = v
		if _, ours := lastPublishedExtendedResources[key]; !ours {
			lastPublishedExtendedResources[key] = v
		}
	}
	if len(owned) > 0 {
		log.Infof("extended-resource startup sync: found %d existing key(s) on node %s: %s",
			len(owned), a.nodeName, summarizeExtendedResources(owned))
	}
	return nil
}

// ClearNodeExtendedResources removes every node-status key the
// agent currently owns (every key in lastPublishedExtendedResources
// plus, for safety, every key currently present on the node that
// carries our domain prefix). Best-effort and synchronous, with a
// short timeout; intended for Agent.Stop() so a graceful shutdown
// does not leave orphan capacity entries behind.
func (a *Agent) ClearNodeExtendedResources() {
	if a.hasLocalConfig() {
		return
	}
	if a.k8sCli == nil || a.nodeName == "" {
		return
	}

	extendedResourcesLock.Lock()
	defer extendedResourcesLock.Unlock()

	toRemove := map[string]struct{}{}
	for k := range lastPublishedExtendedResources {
		toRemove[k] = struct{}{}
	}

	// Also fold in anything currently on the node under our
	// domain that we may not be tracking (e.g., startup sync
	// never ran because no publish happened before Stop). Best
	// effort: ignore the read error and fall back to the
	// in-memory set.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if node, err := a.k8sCli.CoreV1().Nodes().Get(ctx, a.nodeName, metav1.GetOptions{}); err == nil {
		for name := range node.Status.Capacity {
			key := string(name)
			if strings.HasPrefix(key, extendedResourceDomain) {
				toRemove[key] = struct{}{}
			}
		}
	}
	cancel()

	if len(toRemove) == 0 {
		return
	}

	type jsonPatchOp struct {
		Op   string `json:"op"`
		Path string `json:"path"`
	}
	ops := make([]jsonPatchOp, 0, len(toRemove))
	keys := make([]string, 0, len(toRemove))
	for k := range toRemove {
		ops = append(ops, jsonPatchOp{
			Op:   "remove",
			Path: "/status/capacity/" + escapeJSONPointer(k),
		})
		keys = append(keys, k)
	}

	body, err := json.Marshal(ops)
	if err != nil {
		log.Warnf("ClearNodeExtendedResources: marshal patch: %v", err)
		return
	}

	pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pcancel()
	_, err = a.k8sCli.CoreV1().Nodes().Patch(
		pctx, a.nodeName, types.JSONPatchType, body,
		metav1.PatchOptions{}, "status")
	if err != nil {
		log.Warnf("ClearNodeExtendedResources: patch node %s: %v", a.nodeName, err)
		return
	}

	// Stable order in the log
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	log.Infof("cleared node extended resources on shutdown: %s", strings.Join(keys, ", "))

	lastPublishedExtendedResources = map[string]int64{}
}
