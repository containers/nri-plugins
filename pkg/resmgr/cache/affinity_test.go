// Copyright 2020 Intel Corporation. All Rights Reserved.
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

package cache

import (
	"testing"

	nri "github.com/containerd/nri/pkg/api"
)

func TestSimpleParsingSymmetry(t *testing.T) {
	c1, c2, c3, c4, c5 := "c1", "c2", "c3", "c4", "c5"

	tcases := []struct {
		name   string
		source string
		result map[string][]string
	}{
		{
			name:   "trivial 2 by 2",
			source: `c1: [ c2 ]`,
			result: map[string][]string{
				c1: {c2},
				c2: {c1},
			},
		},
		{
			name:   "simple",
			source: `c1: [ c2, c3, c4, c5 ]`,
			result: map[string][]string{
				c1: {c2, c3, c4, c5},
				c2: {c1},
				c3: {c1},
				c4: {c1},
				c5: {c1},
			},
		},
		{
			name: "a bit more complex",
			source: `
c1: [ c2 ]
c2: [ c3, c4, c5 ]
c4: [ c5 ]
`,
			result: map[string][]string{
				c1: {c2},
				c2: {c1, c3, c4, c5},
				c3: {c2},
				c4: {c2, c5},
				c5: {c2, c4},
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			pca := podContainerAffinity{}
			pod := &pod{
				Pod: &nri.PodSandbox{
					Name: "testpod",
				},
			}
			if !pca.parseSimple(pod, tc.source, 1) {
				t.Errorf("failed to parse simple container affinity %q", tc.source)
				return
			}

			found := map[string]map[string]struct{}{}
			for name, affinities := range pca {
				for _, a := range affinities {
					for _, o := range a.Match.Values {
						forw, ok := found[name]
						if !ok {
							forw = map[string]struct{}{}
							found[name] = forw
						}
						back, ok := found[o]
						if !ok {
							back = map[string]struct{}{}
							found[o] = back
						}
						forw[o] = struct{}{}
						back[name] = struct{}{}
					}
				}
			}

			for name, others := range tc.result {
				for _, o := range others {
					if _, ok := found[name][o]; !ok {
						t.Errorf("simple affinity %q did not produce %s: %s",
							tc.source, name, o)
					} else {
						delete(found[name], o)
						if len(found[name]) == 0 {
							delete(found, name)
						}
					}
				}
			}
			for name, others := range found {
				val := ""
				sep := ""
				for o := range others {
					val += sep + o
					sep = ", "
				}
				t.Errorf("simple affinity %q produced unexpected %s: [ %s ]", tc.source, name, val)
			}
		})
	}
}

func TestStrictParsing(t *testing.T) {
	tcases := []struct {
		name    string
		source  string
		invalid bool
	}{
		{
			name: "invalid annotation",
			source: `
  memtier-benchmark:
    - scope:
      key: pod/name
      operator: Matches
      values:
        - redis-*
      match:
        key: name
        operator: Equals
        values:
          - redis
      weight: 10
`,
			invalid: true,
		},
		{
			name: "valid annotation",
			source: `
  memtier-benchmark:
    - scope:
        key: pod/name
        operator: Matches
        values:
          - redis-*
      match:
        key: name
        operator: Equals
        values:
          - redis
      weight: 10
`,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			pca := podContainerAffinity{}
			pod := &pod{
				Pod: &nri.PodSandbox{
					Name: "testpod",
				},
			}
			err := pca.parseFull(pod, tc.source, 1)
			if tc.invalid && err == nil {
				t.Errorf("parsing invalid affinity expression should have failed")
				return
			}
			if !tc.invalid && err != nil {
				t.Errorf("parsing valid affinity expression should not fail")
			}
		})
	}
}
