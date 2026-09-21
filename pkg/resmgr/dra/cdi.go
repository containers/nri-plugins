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

package dra

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"k8s.io/apimachinery/pkg/types"

	"tags.cncf.io/container-device-interface/pkg/cdi"
	"tags.cncf.io/container-device-interface/pkg/parser"
	specs "tags.cncf.io/container-device-interface/specs-go"
)

// cdiClass is the CDI class of every device we write.
const cdiClass = "device"

// cdiStore writes the CDI specs of prepared claims, one spec file per claim.
// A claim's spec is what grants its devices to the containers using it, and it
// is derived state: the policy's accounting of the claim is the source of truth,
// and a spec is written only once that accounting is safe.
//
// The store names the CDI devices it writes and knows nothing else about them:
// what a device grants a container is in the edits the policy hands us, and
// those are written out verbatim.
type cdiStore struct {
	cache  *cdi.Cache
	dir    string
	vendor string
}

// newCDIStore creates a store writing its specs to dir. An empty dir selects
// the CDI directory runtimes watch for dynamically generated specs.
func newCDIStore(driverName, dir string) (*cdiStore, error) {
	// The driver name becomes the vendor of every spec we write. An invalid one
	// would otherwise surface much later, as a spec file no runtime can use.
	if err := parser.ValidateVendorName(driverName); err != nil {
		return nil, fmt.Errorf("dra: driver name %q is not a valid CDI vendor: %w", driverName, err)
	}

	if dir == "" {
		dir = cdi.DefaultDynamicDir
	}

	// Auto-refresh watches the directory for changes made by others. We are the
	// only writer of our own specs and nothing here reads them back, so there is
	// nothing to watch for.
	cache, err := cdi.NewCache(cdi.WithSpecDirs(dir), cdi.WithAutoRefresh(false))
	if err != nil {
		return nil, fmt.Errorf("dra: failed to create CDI cache for %q: %w", dir, err)
	}

	return &cdiStore{cache: cache, dir: dir, vendor: driverName}, nil
}

// WriteClaim writes the CDI spec of a prepared claim. Each of the given edits
// becomes one CDI device of the claim's spec, in the order they were given.
// The spec is written atomically, replacing an existing one for the same claim.
//
// It returns the qualified names of the devices written, positionally matching
// edits, for the caller to hand to the kubelet.
func (s *cdiStore) WriteClaim(uid types.UID, edits []specs.ContainerEdits) ([]string, error) {
	switch {
	case uid == "":
		// Everything we write is named after the claim, so a claim with no UID
		// would leave behind a spec RemoveClaim can never name again.
		return nil, fmt.Errorf("dra: refusing to write a CDI spec for a claim with no UID")
	case len(edits) == 0:
		return nil, fmt.Errorf("dra: refusing to write a CDI spec with no devices for claim %s", uid)
	}

	spec := &specs.Spec{
		Kind:    s.vendor + "/" + cdiClass,
		Devices: make([]specs.Device, 0, len(edits)),
	}
	names := make([]string, 0, len(edits))

	for i, e := range edits {
		name := cdiDeviceName(uid, i)
		spec.Devices = append(spec.Devices, specs.Device{Name: name, ContainerEdits: e})
		names = append(names, parser.QualifiedName(s.vendor, cdiClass, name))
	}

	// Only once the spec is complete, Kind included: which version it needs
	// depends on the edits the policy used, so a policy reaching for a newer
	// kind of edit gets the version bump it requires without us knowing about
	// that kind of edit at all.
	version, err := specs.MinimumRequiredVersion(spec)
	if err != nil {
		return nil, fmt.Errorf("dra: failed to determine CDI version of claim %s: %w", uid, err)
	}
	spec.Version = version

	if err := s.cache.WriteSpec(spec, s.specName(uid)); err != nil {
		return nil, fmt.Errorf("dra: failed to write CDI spec of claim %s: %w", uid, err)
	}

	return names, nil
}

// RemoveClaim removes the CDI spec of a claim. A claim with no spec is not an
// error: the kubelet may ask to unprepare the same claim more than once.
func (s *cdiStore) RemoveClaim(uid types.UID) error {
	if err := s.cache.RemoveSpec(s.specName(uid)); err != nil {
		return fmt.Errorf("dra: failed to remove CDI spec of claim %s: %w", uid, err)
	}
	return nil
}

// HasClaim tells whether a claim has a spec. A claim which has one has been
// prepared successfully at least once, because the spec is the last thing a
// prepare writes, and its devices may be in use by a container.
func (s *cdiStore) HasClaim(uid types.UID) (bool, error) {
	switch _, err := os.Stat(s.specPath(uid)); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("dra: failed to look for the CDI spec of claim %s: %w", uid, err)
	}
}

// specPath is the spec file of a claim.
func (s *cdiStore) specPath(uid types.UID) string {
	return filepath.Join(s.dir, s.specName(uid))
}

// specName is the name of the spec file of a claim, extension included. The CDI
// library appends one of its own to a name which has none, and both the format
// it writes and the path it writes to follow from it, so naming the extension
// here is what lets specPath name the same file the library does.
func (s *cdiStore) specName(uid types.UID) string {
	return cdi.GenerateTransientSpecName(s.vendor, cdiClass, string(uid)) + ".yaml"
}

// cdiDeviceName names one CDI device of a claim. The claim UID is what upstream
// recommends naming devices after, and the index tells apart the several devices
// a single claim can have, one per allocation result. The index is stable across
// a re-prepare of the same claim, because a claim's allocation never changes
// once it has been made.
func cdiDeviceName(uid types.UID, index int) string {
	return "claim-" + string(uid) + "-" + strconv.Itoa(index)
}
