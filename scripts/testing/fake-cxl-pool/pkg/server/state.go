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

package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
)

// persistedState is the content of the state file.
type persistedState struct {
	Saved       time.Time         `json:"saved"`
	Devices     []persistedDevice `json:"devices,omitempty"`
	Hosts       []persistedHost   `json:"hosts,omitempty"`
	Attachments []api.Attachment  `json:"attachments,omitempty"`
}

// persistedDevice is a device created over the API.
type persistedDevice struct {
	Name       string    `json:"name"`
	Serial     string    `json:"serial"`
	Size       int64     `json:"size"`
	Shared     bool      `json:"shared,omitempty"`
	Persistent bool      `json:"persistent,omitempty"`
	Path       string    `json:"path"`
	Created    time.Time `json:"created"`
}

type persistedHost struct {
	Name      string `json:"name"`
	UUID      string `json:"uuid,omitempty"`
	PID       int    `json:"pid,omitempty"`
	StartTime uint64 `json:"startTime,omitempty"`
	Counter   int    `json:"counter,omitempty"`
	// Unplugged are qemu device ids that got device_del (never adopted).
	Unplugged []string `json:"unplugged,omitempty"`
}

func (s *Server) persistenceEnabled() bool {
	return s.cfg.StateFile != "" && s.cfg.StateFile != "-"
}

// loadState restores dynamic devices, hosts and attachments. Devices are
// never dropped silently: problems are logged and the device is kept.
func (s *Server) loadState() error {
	if !s.persistenceEnabled() {
		return nil
	}
	b, err := os.ReadFile(s.cfg.StateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("state file: %w", err)
	}
	var st persistedState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("state file %s: %w", s.cfg.StateFile, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pd := range st.Devices {
		if _, ok := s.devices[pd.Name]; ok {
			s.logf("state: device %s is also in the config, using the config", pd.Name)
			continue
		}
		sn, err := cxlapi.ParseSerial(pd.Serial)
		if err != nil {
			sn = s.serials.Next(pd.Name, pd.Shared)
			s.logf("ERROR: state: device %s: %v; kept with the new serial 0x%x", pd.Name, err, sn)
		} else if err := s.serials.Use(sn, pd.Name); err != nil {
			s.logf("ERROR: state: device %s: %v; kept with the duplicate serial", pd.Name, err)
		}
		d := &device{
			Device: api.Device{
				Name:       pd.Name,
				Serial:     cxlapi.FormatSerial(sn),
				Size:       pd.Size,
				Shared:     pd.Shared,
				Persistent: pd.Persistent,
				Backend:    api.BackendFile,
				Path:       pd.Path,
				Scope:      api.ScopePool,
				Created:    pd.Created,
			},
			serial:  sn,
			dynamic: true,
		}
		if pd.Persistent {
			d.LSAPath = lsaPath(pd.Path)
		}
		if err := ensureFiles(d.Path, d.LSAPath, d.Size); err != nil {
			s.logf("state: device %s: %v", pd.Name, err)
		}
		s.devices[pd.Name] = d
	}
	for _, ph := range st.Hosts {
		unplugged := map[string]bool{}
		for _, id := range ph.Unplugged {
			unplugged[id] = true
		}
		s.hosts[ph.Name] = &host{
			unplugged: unplugged,
			name:      ph.Name,
			uuid:      ph.UUID,
			pid:       ph.PID,
			startTime: ph.StartTime,
			counter:   ph.Counter,
			state:     api.HostStopped,
			fromState: true,
		}
	}
	for _, a := range st.Attachments {
		if a.State == api.AttachmentAttaching {
			// interrupted attach: the tree decides at reconcile
			a.State = api.AttachmentAttached
		}
		s.atts[a.ID] = &attachment{Attachment: a}
	}
	s.logf("state: loaded %s: %d devices, %d hosts, %d attachments",
		s.cfg.StateFile, len(st.Devices), len(st.Hosts), len(st.Attachments))
	return nil
}

// saveStateLocked writes the state file atomically.
func (s *Server) saveStateLocked() {
	if !s.persistenceEnabled() {
		return
	}
	st := persistedState{Saved: time.Now().UTC()}
	for _, d := range sortedDevices(s.devices) {
		if d.dynamic {
			st.Devices = append(st.Devices, persistedDevice{
				Name: d.Name, Serial: d.Serial, Size: d.Size, Shared: d.Shared, Persistent: d.Persistent, Path: d.Path,
				Created: d.Created,
			})
		}
	}
	for _, h := range sortedHosts(s.hosts) {
		ph := persistedHost{Name: h.name, UUID: h.uuid, PID: h.pid, StartTime: h.startTime, Counter: h.counter}
		for id := range h.unplugged {
			ph.Unplugged = append(ph.Unplugged, id)
		}
		sort.Strings(ph.Unplugged)
		st.Hosts = append(st.Hosts, ph)
	}
	for _, a := range sortedAttachments(s.atts) {
		st.Attachments = append(st.Attachments, a.Attachment)
	}
	b, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		s.logf("state: marshal: %v", err)
		return
	}
	dir := filepath.Dir(s.cfg.StateFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.logf("state: %v", err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".fake-cxl-pool-state-*")
	if err != nil {
		s.logf("state: %v", err)
		return
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		s.logf("state: write %s: %v %v", tmp.Name(), werr, cerr)
		return
	}
	if err := os.Rename(tmp.Name(), s.cfg.StateFile); err != nil {
		_ = os.Remove(tmp.Name())
		s.logf("state: %v", err)
	}
}
