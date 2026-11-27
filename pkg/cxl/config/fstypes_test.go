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

package config

import (
	"strings"
	"testing"

	"github.com/containers/nri-plugins/pkg/cxl/api"
)

func TestFSTypeTable(t *testing.T) {
	if len(fsTypes) != len(api.KnownFSTypes) {
		t.Fatalf("%d types in the table, %d known", len(fsTypes), len(api.KnownFSTypes))
	}
	for _, tc := range []struct {
		name        string
		known       bool
		class       string
		readOnly    bool
		dax         bool
		cluster     bool
		remountable bool
	}{
		{"erofs", true, fsClassReadOnly, true, true, false, false},
		{"squashfs", true, fsClassReadOnly, true, false, false, false},
		{"ext4", true, fsClassSingleHost, false, true, false, true},
		{"xfs", true, fsClassSingleHost, false, true, false, true},
		{"btrfs", true, fsClassSingleHost, false, false, false, false},
		{"gfs2", true, fsClassCluster, false, false, true, false},
		{"f2fs", false, fsClassSingleHost, false, false, false, false},
		{"ocfs2", false, fsClassSingleHost, false, false, false, false},
		{"EXT4", false, fsClassSingleHost, false, false, false, false},
		{"", false, fsClassSingleHost, false, false, false, false},
	} {
		_, inTable := fsTypes[tc.name]
		known, class, readOnly, dax, cluster, remountable := api.KnownFSType(tc.name), fsTypeClass(tc.name), FSTypeReadOnly(tc.name),
			FSTypeDax(tc.name), FSTypeCluster(tc.name), FSTypeRemountable(tc.name)
		if known != tc.known || inTable != tc.known || class != tc.class || readOnly != tc.readOnly || dax != tc.dax ||
			cluster != tc.cluster || remountable != tc.remountable {
			t.Errorf("%q: known %v (table %v) class %s read-only %v DAX %v cluster %v remountable %v, expected %v %s %v %v %v %v",
				tc.name, known, inTable, class, readOnly, dax, cluster, remountable, tc.known, tc.class, tc.readOnly, tc.dax, tc.cluster, tc.remountable)
		}
	}
}

func TestFSMountData(t *testing.T) {
	for _, tc := range []struct {
		name     string
		access   string
		readOnly bool
		want     string
	}{
		{"erofs", api.AccessDirect, true, "dax=always"},
		{"erofs", api.AccessCached, true, "dax=never"},
		{"squashfs", api.AccessCached, true, ""},
		{"ext4", api.AccessDirect, true, "dax=always"},
		{"ext4", api.AccessCached, true, "dax=never"},
		{"ext4", api.AccessDirect, false, "dax=always"},
		{"ext4", api.AccessCached, false, "dax=never"},
		{"xfs", api.AccessDirect, true, "dax=always,nouuid"},
		{"xfs", api.AccessCached, true, "dax=never,nouuid"},
		{"xfs", api.AccessDirect, false, "dax=always"},
		{"xfs", api.AccessCached, false, "dax=never"},
		{"btrfs", api.AccessCached, true, ""},
		{"btrfs", api.AccessCached, false, ""},
		{"gfs2", api.AccessCached, true, "spectator"},
		{"gfs2", api.AccessCached, false, ""},
		{"f2fs", api.AccessCached, true, ""},
		{"f2fs", api.AccessCached, false, ""},
	} {
		if got := strings.Join(FSMountData(tc.name, tc.access, tc.readOnly), ","); got != tc.want {
			t.Errorf("%s %s read-only %v: data %q, expected %q", tc.name, tc.access, tc.readOnly, got, tc.want)
		}
	}
}
