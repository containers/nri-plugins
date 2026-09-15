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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	cdilib "tags.cncf.io/container-device-interface/pkg/cdi"
	"tags.cncf.io/container-device-interface/pkg/parser"
	specs "tags.cncf.io/container-device-interface/specs-go"

	logger "github.com/containers/nri-plugins/pkg/log"
)

var cdiLog = logger.NewLogger("dra")

const (
	// defaultCDIDir is the default directory for CDI spec files.
	defaultCDIDir = "/var/run/cdi"
	// cdiClass is the CDI class name used for all DRA claim specs.
	cdiClass = "device"
)

// cdiWriter implements CDIWriter using the upstream CDI library.
type cdiWriter struct {
	cache  *cdilib.Cache
	vendor string
	class  string
	cdiDir string
}

// NewCDIWriter creates a CDIWriter that writes CDI specs under cdiDir.
// If cdiDir is empty, it defaults to /var/run/cdi.
// driverName is validated as a CDI vendor name. Returns an error if
// driverName is not a valid CDI vendor name or if the CDI cache cannot
// be created.
func NewCDIWriter(driverName, cdiDir string) (CDIWriter, error) {
	if err := parser.ValidateVendorName(driverName); err != nil {
		return nil, fmt.Errorf("dra cdi: invalid driverName %q: %w", driverName, err)
	}
	if cdiDir == "" {
		cdiDir = defaultCDIDir
	}
	if err := os.MkdirAll(cdiDir, 0750); err != nil {
		return nil, fmt.Errorf("dra cdi: create CDI dir %q: %w", cdiDir, err)
	}
	cache, err := cdilib.NewCache(
		cdilib.WithAutoRefresh(false),
		cdilib.WithSpecDirs(cdiDir),
	)
	if err != nil {
		return nil, fmt.Errorf("dra cdi: create CDI cache: %w", err)
	}
	return &cdiWriter{
		cache:  cache,
		vendor: driverName,
		class:  cdiClass,
		cdiDir: cdiDir,
	}, nil
}

// WriteClaim writes a CDI spec file for the claim identified by uid.
// Each element of devices becomes one CDI device entry in the spec.
// Returns an error if devices is empty, if spec assembly fails, or if
// writing the spec file fails.
func (w *cdiWriter) WriteClaim(uid types.UID, devices []CDIDevice) error {
	if len(devices) == 0 {
		return fmt.Errorf("dra cdi: WriteClaim %s: devices must not be empty", uid)
	}

	cdiDevices := make([]specs.Device, 0, len(devices))
	for _, d := range devices {
		env := []string{fmt.Sprintf("NRI_CLASS=%s", d.ClassName)}
		for _, cpu := range d.CPUs.List() {
			env = append(env, fmt.Sprintf("NRI_CPU%d=1", cpu))
		}
		cdiDevices = append(cdiDevices, specs.Device{
			Name: d.Name,
			ContainerEdits: specs.ContainerEdits{
				Env: env,
			},
		})
	}

	spec := specs.Spec{
		Kind:    w.vendor + "/" + w.class,
		Devices: cdiDevices,
	}
	// MinimumRequiredVersion must be called after spec is fully assembled,
	// including Kind which it reads to determine requirements.
	v, err := specs.MinimumRequiredVersion(&spec)
	if err != nil {
		return fmt.Errorf("dra cdi: WriteClaim %s: determine CDI version: %w", uid, err)
	}
	spec.Version = v

	name := cdilib.GenerateTransientSpecName(w.vendor, w.class, string(uid))
	if err := w.cache.WriteSpec(&spec, name); err != nil {
		return fmt.Errorf("dra cdi: WriteClaim %s: write spec: %w", uid, err)
	}
	return nil
}

// RemoveClaim removes the CDI spec file for the claim identified by uid.
// Returns nil if the spec does not exist (idempotent).
func (w *cdiWriter) RemoveClaim(uid types.UID) error {
	name := cdilib.GenerateTransientSpecName(w.vendor, w.class, string(uid))
	if err := w.cache.RemoveSpec(name); err != nil {
		return fmt.Errorf("dra cdi: RemoveClaim %s: %w", uid, err)
	}
	return nil
}

// ClaimSpecExists reports whether the CDI spec file for uid is present on disk.
// The file path is <cdiDir>/<vendor>-<class>_<uid>.yaml, consistent with
// how WriteSpec names files for transient specs.
func (w *cdiWriter) ClaimSpecExists(uid types.UID) bool {
	name := cdilib.GenerateTransientSpecName(w.vendor, w.class, string(uid))
	path := filepath.Join(w.cdiDir, name+".yaml")
	_, err := os.Stat(path)
	return err == nil
}

// ListClaims returns the UIDs of all claims for which a CDI spec exists in
// the managed CDI directory. Refresh errors are logged at Warn level but do
// not abort the listing — foreign or malformed specs in the directory must not
// prevent our claims from being returned.
func (w *cdiWriter) ListClaims() ([]types.UID, error) {
	if err := w.cache.Refresh(); err != nil {
		cdiLog.Warnf("dra cdi: ListClaims: cache refresh: %v", err)
	}
	prefix := w.vendor + "-" + w.class + "_"
	suffix := ".yaml"
	var uids []types.UID
	for _, s := range w.cache.GetVendorSpecs(w.vendor) {
		base := filepath.Base(s.GetPath())
		if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, suffix) {
			continue
		}
		uidStr := strings.TrimPrefix(base, prefix)
		uidStr = strings.TrimSuffix(uidStr, suffix)
		if uidStr == "" {
			continue
		}
		uids = append(uids, types.UID(uidStr))
	}
	return uids, nil
}

// CDIDeviceName builds a valid CDI device name for the given allocation result.
// The format is:
//
//	"claim-<uid>-<sanitize(request)>-<device>-<idx>"
//
// where sanitize replaces '/' and any character that is not alphanumeric,
// '_', '-', '.', or ':' with '-', then trims any leading or trailing
// non-alphanumeric characters.
//
// The result can be validated with parser.ValidateDeviceName. Using a per-
// result idx ensures that two results that share the same Request+Device
// (e.g. AllowMultipleAllocations with count > 1) produce distinct names.
func CDIDeviceName(uid types.UID, request, device string, idx int) string {
	sanitized := sanitizeCDIName(request)
	sanitizedDev := sanitizeCDIName(device)
	return "claim-" + string(uid) + "-" + sanitized + "-" + sanitizedDev + "-" + strconv.Itoa(idx)
}

// sanitizeCDIName replaces any character that is invalid in a CDI device name
// middle position with '-', then trims leading and trailing non-alphanumeric
// characters. '/' is always replaced (it is not a valid CDI device name char).
func sanitizeCDIName(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteRune(c)
		case c == '_' || c == '-' || c == '.' || c == ':':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	result := b.String()
	// Trim leading/trailing non-alphanumeric characters.
	start := strings.IndexFunc(result, func(r rune) bool {
		return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
	})
	if start < 0 {
		// All characters were replaced by dashes or the string is empty.
		// Return a safe placeholder so the caller still gets a non-empty segment.
		return "x"
	}
	end := strings.LastIndexFunc(result, func(r rune) bool {
		return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
	})
	return result[start : end+1]
}
