/*
Copyright The NRI Plugins Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dra

import (
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// DeviceLister provides the DRA device list for a given driver name.
type DeviceLister interface {
	DRADevices(driverName string) ([]resourceapi.Device, error)
}

// CDIDevice holds the CDI device information for a single allocation result.
// Name is the CDI device name, precomputed by the caller via cdiDeviceName.
// WriteClaim uses Name and ClassName directly to build the CDI spec entry.
type CDIDevice struct {
	// Name is the CDI device name (precomputed by the caller via cdiDeviceName).
	Name string
	// ClassName is the nri/cpuClass attribute value for the allocated device.
	ClassName string
	// CPUs is the set of CPUs allocated for this device.
	CPUs cpuset.CPUSet
}

// ClaimAllocator provides HP CPU pick/release/account operations needed
// during PrepareResourceClaims, UnprepareResourceClaims, and restart
// reconciliation.
type ClaimAllocator interface {
	// PickHpCpus selects n HP-eligible CPUs from the punit identified by
	// (pkgID, punitID), excluding CPUs in held and those already tracked
	// in internal accounting.
	PickHpCpus(pkgID, punitID, n int, held cpuset.CPUSet) (cpuset.CPUSet, error)
	// ReleaseHpCpus removes cpus from DRA HP accounting on the given punit.
	ReleaseHpCpus(pkgID, punitID int, cpus cpuset.CPUSet)
	// AccountHpCpus records cpus as DRA HP-held on the given punit. Used
	// during restart reconciliation to rebuild HP accounting without
	// re-allocating CPUs.
	AccountHpCpus(pkgID, punitID int, cpus cpuset.CPUSet) error
	// IsHPClass reports whether className is currently classified as PCT
	// high priority.
	IsHPClass(className string) bool
}

// CDIWriter manages CDI spec files on behalf of the DRA plugin.
type CDIWriter interface {
	// WriteClaim writes a CDI spec file for the claim identified by uid,
	// containing one CDI device entry per element of devices.
	WriteClaim(uid types.UID, devices []CDIDevice) error
	// RemoveClaim removes the CDI spec file for the claim identified by uid.
	// Returns nil if the spec does not exist (idempotent).
	RemoveClaim(uid types.UID) error
	// ClaimSpecExists reports whether the CDI spec file for uid is present
	// on disk.
	ClaimSpecExists(uid types.UID) bool
	// ListClaims returns the UIDs of all claims for which a CDI spec file
	// exists under the managed CDI directory.
	ListClaims() ([]types.UID, error)
}

// ClaimStore persists and loads claim state via the resmgr cache.
// Save is a no-op when the cache is in a BlockSave window; data is still
// in-memory policyData and will persist on the next unblocked Save.
// Callers of Save must tolerate this.
type ClaimStore interface {
	// Save persists the given claims map to the backing store.
	Save(claims map[types.UID]*ClaimState) error
	// Load reads the claims map from the backing store. Returns nil, nil
	// when no claim state has been saved yet.
	Load() (map[types.UID]*ClaimState, error)
}

// Deps holds the dependencies a policy binary must supply when constructing
// a Plugin.
type Deps struct {
	// KubeClient is the Kubernetes client used to publish ResourceSlice objects.
	KubeClient kubernetes.Interface
	// NodeName is the name of the node this plugin runs on.
	NodeName string
	// RegistrarDir is the directory where the plugin registrar socket is created.
	// Defaults to kubeletplugin.KubeletRegistryDir when empty.
	RegistrarDir string
	// PluginDataDir is the directory where the plugin data socket is created.
	// Defaults to kubeletplugin.KubeletPluginsDir+"/"+driverName when empty.
	PluginDataDir string
	// ValidateClasses is a closure that validates the current cpuClass
	// configuration for DRA compatibility.
	ValidateClasses func() error
	// DeviceLister returns the list of DRA devices to publish.
	DeviceLister DeviceLister
	// ClaimAllocator provides HP CPU pick/release/account operations.
	ClaimAllocator ClaimAllocator
	// CDIWriter manages CDI spec files for prepared claims.
	CDIWriter CDIWriter
	// ClaimStore persists and loads claim state via the resmgr cache.
	ClaimStore ClaimStore
	// WithLock executes f while holding the resmgr write lock. All accesses
	// to Handler state (ValidateClasses, DRADevices, Prepare, Unprepare, and
	// RestoreClaims) must run inside WithLock.
	WithLock func(func())
	// Logger is the logger used for all plugin log output.
	Logger log.Logger
}
