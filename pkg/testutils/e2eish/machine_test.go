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

package e2eish

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadTopology(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topology.var.json")
	require.NoError(t, os.WriteFile(path, []byte(`[
    {"mem": "2G", "cores": 2, "nodes": 2, "packages": 2},
    {"mem": "4G", "node-dist": {"0": 21}}
]`), 0o644))
	topo, err := readTopology(path)
	require.NoError(t, err)
	require.Equal(t, Topology{
		{Mem: "2G", Cores: 2, Nodes: 2, Packages: 2},
		{Mem: "4G", NodeDist: map[int]int{0: 21}},
	}, topo)

	require.NoError(t, os.WriteFile(path, []byte(`[{"cpus": 2}]`), 0o644))
	_, err = readTopology(path)
	require.ErrorContains(t, err, `"cpus"`)
}
