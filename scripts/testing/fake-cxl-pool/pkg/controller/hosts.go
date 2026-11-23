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

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
)

// hostMap maps the Nodes of this cluster to pool hosts (qemu VMs).
type hostMap struct {
	nodeHost  map[string]api.Host // node name -> host
	hostNode  map[string]string   // host name -> node name
	hostnames map[string]string   // node name -> kubernetes.io/hostname label
	// unlabeled are the nodes that have a host but no hostname label
	unlabeled []string
}

// mapHosts finds the host of each node: the host whose uuid equals the
// node's systemUUID (lower case, without dashes), else the host whose name
// is the node name. A host maps to one node at most. A node without a host
// is not a pool host, nor is a node without a kubernetes.io/hostname label:
// the node selector of the pool selects nodes by that label.
func mapHosts(hosts []api.Host, nodes []*corev1.Node) hostMap {
	hm := hostMap{nodeHost: map[string]api.Host{}, hostNode: map[string]string{}, hostnames: map[string]string{}}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	for _, n := range nodes {
		uuid := api.NormUUID(n.Status.NodeInfo.SystemUUID)
		var found *api.Host
		if uuid != "" {
			for i := range hosts {
				if api.NormUUID(hosts[i].UUID) == uuid {
					found = &hosts[i]
					break
				}
			}
		}
		if found == nil {
			for i := range hosts {
				if hosts[i].Name == n.Name {
					found = &hosts[i]
					break
				}
			}
		}
		if found == nil {
			continue
		}
		if _, taken := hm.hostNode[found.Name]; taken {
			continue
		}
		v := n.Labels[corev1.LabelHostname]
		if v == "" {
			hm.unlabeled = append(hm.unlabeled, n.Name)
			continue
		}
		hm.nodeHost[n.Name] = *found
		hm.hostNode[found.Name] = n.Name
		hm.hostnames[n.Name] = v
	}
	return hm
}

// mapNodes returns the map of the Nodes of the cluster to the hosts of the
// server, and logs it when it changed.
func (c *Controller) mapNodes(ctx context.Context) (hostMap, error) {
	hosts, err := c.pool.Hosts(ctx)
	if err != nil {
		return hostMap{}, fmt.Errorf("list hosts: %w", err)
	}
	nodes, err := c.nodes.List(labels.Everything())
	if err != nil {
		return hostMap{}, err
	}
	hm := mapHosts(hosts, nodes)
	for _, n := range hm.unlabeled {
		if !c.noLabel[n] {
			c.noLabel[n] = true
			c.logf("node %s has no %s label: not a pool host", n, corev1.LabelHostname)
		}
	}
	if s := hm.String(); s != c.hostMapStr {
		c.logf("pool hosts of nodes: %s", s)
		c.hostMapStr = s
	}
	return hm, nil
}

// String lists the mapping, sorted by node name.
func (hm hostMap) String() string {
	ns := make([]string, 0, len(hm.nodeHost))
	for n, h := range hm.nodeHost {
		ns = append(ns, n+"="+h.Name)
	}
	sort.Strings(ns)
	return "[" + strings.Join(ns, " ") + "]"
}
