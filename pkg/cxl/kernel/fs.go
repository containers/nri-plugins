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

package kernel

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"

	"github.com/containers/nri-plugins/pkg/cxl/api"
)

// The filesystem of a block device of a persistent device is known by the
// signatures of the known types in its first FSProbeBytes
// (docs/memory/cxl/persistent.md "Filesystems").

const (
	// FSProbeBytes is how much of a block device the probe reads.
	FSProbeBytes = 128 << 10
	// gfs2SuperOffset is the offset of the gfs2 superblock.
	gfs2SuperOffset = 65536
	// Gfs2LockProtoDLM is the gfs2 lock protocol of a cluster.
	Gfs2LockProtoDLM = "lock_dlm"
)

// FSSignature is what the probe found in the head of a block device.
type FSSignature struct {
	// Types are the known types whose signature the head has, in the order
	// erofs, squashfs, ext4, xfs, btrfs, gfs2.
	Types []string `json:"types,omitempty"`
	// Gfs2LockProto is the lock protocol in a gfs2 superblock.
	Gfs2LockProto string `json:"gfs2LockProto,omitempty"`
	// Ext4NeedsRecovery is set when an ext4 superblock has the incompat
	// feature needs_recovery (a read-only device refuses its mount).
	Ext4NeedsRecovery bool `json:"ext4NeedsRecovery,omitempty"`
}

// ParseFSSignature returns the signatures of the known filesystem types in
// the head of a block device. A short head matches the signatures that it
// covers.
func ParseFSSignature(head []byte) FSSignature {
	le32 := func(off int) (uint32, bool) {
		if off+4 > len(head) {
			return 0, false
		}
		return binary.LittleEndian.Uint32(head[off:]), true
	}
	be32 := func(off int) (uint32, bool) {
		if off+4 > len(head) {
			return 0, false
		}
		return binary.BigEndian.Uint32(head[off:]), true
	}
	at := func(off int, magic string) bool {
		return off+len(magic) <= len(head) && string(head[off:off+len(magic)]) == magic
	}
	var s FSSignature
	if v, ok := le32(1024); ok && v == 0xE0F5E1E2 {
		s.Types = append(s.Types, api.FSTypeErofs)
	}
	if at(0, "hsqs") {
		s.Types = append(s.Types, api.FSTypeSquashfs)
	}
	if len(head) >= 1082 && binary.LittleEndian.Uint16(head[1080:]) == 0xEF53 {
		s.Types = append(s.Types, api.FSTypeExt4)
		if v, ok := le32(1024 + 0x60); ok && v&0x4 != 0 {
			s.Ext4NeedsRecovery = true
		}
	}
	if at(0, "XFSB") {
		s.Types = append(s.Types, api.FSTypeXfs)
	}
	if at(65600, "_BHRfS_M") {
		s.Types = append(s.Types, api.FSTypeBtrfs)
	}
	magic, ok1 := be32(gfs2SuperOffset)
	meta, ok2 := be32(gfs2SuperOffset + 4)
	if ok1 && ok2 && magic == 0x01161970 && meta == 1 {
		s.Types = append(s.Types, api.FSTypeGfs2)
		if off := gfs2SuperOffset + 96; off < len(head) {
			proto := head[off:min(off+64, len(head))]
			if i := bytes.IndexByte(proto, 0); i >= 0 {
				proto = proto[:i]
			}
			s.Gfs2LockProto = string(proto)
		}
	}
	return s
}

// ReadFSSignature returns the signatures in the head of the block device
// dev.
func (k *Kernel) ReadFSSignature(dev string) (FSSignature, error) {
	head, err := k.ReadHead(dev, FSProbeBytes)
	if err != nil {
		return FSSignature{}, err
	}
	return ParseFSSignature(head), nil
}

// NodevFSType tells whether the kernel lists the filesystem type as one
// that mounts no block device ("nodev" in /proc/filesystems).
func (k *Kernel) NodevFSType(fsType string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(k.procRoot(), "filesystems"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "nodev" && f[1] == fsType {
			return true, nil
		}
	}
	return false, nil
}
