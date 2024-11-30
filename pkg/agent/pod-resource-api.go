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
	"time"

	"github.com/containers/nri-plugins/pkg/agent/podresapi"
)

// GetPodResources queries the given pod's resources.
func (a *Agent) GetPodResources(ns, pod string, timeout time.Duration) (*podresapi.PodResources, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return a.podResCli.Get(ctx, ns, pod)
}

// GetPodResources queries the given pod's resources asynchronously.
func (a *Agent) GoGetPodResources(ns, pod string, timeout time.Duration) <-chan *podresapi.PodResources {
	if !a.podResCli.HasClient() {
		return nil
	}

	ch := make(chan *podresapi.PodResources, 1)

	go func() {
		defer close(ch)
		p, err := a.GetPodResources(ns, pod, timeout)
		if err != nil {
			log.Error("failed to get pod resources for %s/%s: %v", ns, pod, err)
			return
		}
		ch <- p
	}()

	return ch
}

// ListPodResources lists all pods' resources.
func (a *Agent) ListPodResources(timeout time.Duration) (podresapi.PodResourcesList, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return a.podResCli.List(ctx)
}

// ListPodResources lists all pods' resources asynchronously.
func (a *Agent) GoListPodResources(timeout time.Duration) <-chan podresapi.PodResourcesList {
	if !a.podResCli.HasClient() {
		return nil
	}

	ch := make(chan podresapi.PodResourcesList, 1)

	go func() {
		defer close(ch)
		l, err := a.ListPodResources(timeout)
		if err != nil {
			log.Error("failed to list pod resources: %v", err)
			return
		}
		ch <- l
	}()

	return ch
}
