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
	"testing"

	nri "github.com/containerd/nri/pkg/api"
	"github.com/stretchr/testify/require"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

func TestCacheCreate(t *testing.T) {
	makeCache(t)
}

func TestCacheInsertPods(t *testing.T) {
	nriPods := []*nri.PodSandbox{
		makePod(),
		makePod(),
		makePod(),
	}

	c, _, _ := makePopulatedCache(t, nriPods, nil)

	for _, nriPod := range nriPods {
		pod := c.InsertPod(nriPod, nil)
		require.NotNil(t, pod)
	}
}

func TestCacheLookupPods(t *testing.T) {
	nriPods := []*nri.PodSandbox{
		makePod(),
		makePod(),
		makePod(),
	}

	c, _, _ := makePopulatedCache(t, nriPods, nil)

	for _, nriPod := range nriPods {
		pod := c.InsertPod(nriPod, nil)
		require.NotNil(t, pod)

		chk, ok := c.LookupPod(pod.GetID())
		require.NotNil(t, chk)
		require.True(t, ok)
	}
}

func TestCacheLookupPodFailure(t *testing.T) {
	nriPods := []*nri.PodSandbox{
		makePod(),
		makePod(),
		makePod(),
	}

	c, _, _ := makePopulatedCache(t, nriPods, nil)

	pod, ok := c.LookupPod("xyzzy-foobar")
	require.Nil(t, pod)
	require.False(t, ok)
}

func TestCacheResetPolicyEntries(t *testing.T) {
	dir := t.TempDir()

	c, err := cache.NewCache(cache.Options{CacheDir: dir})
	require.NoError(t, err)
	require.NoError(t, c.SetActivePolicy("test"))
	c.SetPolicyEntry("key", "value")
	require.NoError(t, c.Save())

	c, err = cache.NewCache(cache.Options{CacheDir: dir})
	require.NoError(t, err)
	c.ResetPolicyEntries()

	var value string
	require.False(t, c.GetPolicyEntry("key", &value))
	require.Equal(t, "test", c.GetActivePolicy())
}

func TestCacheSaveRestoreCPUSetMaps(t *testing.T) {
	dir := t.TempDir()
	saved := map[string][]cpuset.CPUSet{
		"a": {cpuset.MustParse("0-1"), cpuset.MustParse("4,6")},
		"b": {cpuset.MustParse("2")},
		"c": {},
	}

	c, err := cache.NewCache(cache.Options{CacheDir: dir})
	require.NoError(t, err)
	require.NoError(t, c.SetActivePolicy("test"))
	c.SetPolicyEntry("claims", saved)
	require.NoError(t, c.Save())

	c, err = cache.NewCache(cache.Options{CacheDir: dir})
	require.NoError(t, err)
	restored := map[string][]cpuset.CPUSet{}
	require.True(t, c.GetPolicyEntry("claims", &restored))
	require.Len(t, restored, 3)
	for key, csets := range saved {
		require.Len(t, restored[key], len(csets))
		for i, cset := range csets {
			require.True(t, restored[key][i].Equals(cset))
		}
	}
}

func makeCache(t *testing.T) cache.Cache {
	t.Helper()

	c, err := cache.NewCache(cache.Options{CacheDir: t.TempDir()})
	require.NoError(t, err, "failed to create cache")
	require.NotNil(t, c)
	return c
}

func makePopulatedCache(t *testing.T, nriPods []*nri.PodSandbox, nriCtrs []*nri.Container) (cache.Cache, []cache.Pod, []cache.Container) {
	t.Helper()

	var (
		c    = makeCache(t)
		pods []cache.Pod
		ctrs []cache.Container
	)

	for _, nriPod := range nriPods {
		pod := c.InsertPod(nriPod, nil)
		require.NotNil(t, pod)
		pods = append(pods, pod)
	}
	for _, nriCtr := range nriCtrs {
		ctr, err := c.InsertContainer(nriCtr)
		require.NoError(t, err)
		require.NotNil(t, ctr)
		ctrs = append(ctrs, ctr)
	}

	return c, pods, ctrs
}
