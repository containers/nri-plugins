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

package cache_test

import (
	"fmt"

	nri "github.com/containerd/nri/pkg/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
)

var _ = Describe("Cache", func() {
	It("can be created without errors", func() {
		makeCache()
	})

	It("can insert pods", func() {
		var (
			c       cache.Cache
			nriPods = []*nri.PodSandbox{
				makePod(),
				makePod(),
				makePod(),
			}
		)

		c, _, _ = makePopulatedCache(nriPods, nil)

		for _, nriPod := range nriPods {
			pod := c.InsertPod(nriPod, nil)
			Expect(pod).ToNot(BeNil())
		}
	})

	It("can look up inserted pods", func() {
		var (
			c       cache.Cache
			nriPods = []*nri.PodSandbox{
				makePod(),
				makePod(),
				makePod(),
			}
		)

		c, _, _ = makePopulatedCache(nriPods, nil)

		for _, nriPod := range nriPods {
			pod := c.InsertPod(nriPod, nil)
			Expect(pod).ToNot(BeNil())

			chk, ok := c.LookupPod(pod.GetID())
			Expect(chk).ToNot(BeNil())
			Expect(ok).To(BeTrue())
		}
	})

	It("properly indicates pod lookup failure", func() {
		var (
			c       cache.Cache
			nriPods = []*nri.PodSandbox{
				makePod(),
				makePod(),
				makePod(),
			}
		)

		c, _, _ = makePopulatedCache(nriPods, nil)

		pod, ok := c.LookupPod("xyzzy-foobar")
		Expect(pod).To(BeNil())
		Expect(ok).To(BeFalse())
	})
})

func makeCache() cache.Cache {
	c, err := cache.NewCache(cache.Options{CacheDir: GinkgoT().TempDir()})
	Expect(c).ToNot(BeNil())
	Expect(err).To(BeNil())
	if err != nil {
		panic(fmt.Errorf("failed to create cache: %w", err))
	}
	return c
}

func makePopulatedCache(nriPods []*nri.PodSandbox, nriCtrs []*nri.Container) (cache.Cache, []cache.Pod, []cache.Container) {
	var (
		c    = makeCache()
		pods []cache.Pod
		ctrs []cache.Container
	)

	for _, nriPod := range nriPods {
		pod := c.InsertPod(nriPod, nil)
		Expect(pod).ToNot(BeNil())
		pods = append(pods, pod)
	}
	for _, nriCtr := range nriCtrs {
		ctr, err := c.InsertContainer(nriCtr)
		Expect(ctr).ToNot(BeNil())
		Expect(err).To(BeNil())
		ctrs = append(ctrs, ctr)
	}

	return c, pods, ctrs
}
