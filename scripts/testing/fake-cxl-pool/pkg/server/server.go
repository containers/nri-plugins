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
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
)

// Options are the dependencies of a Server. Zero values select the real
// implementations; unit tests inject fakes.
type Options struct {
	// Logger for lifecycle messages (default: log.Default()).
	Logger *log.Logger
	// Verbose logs every qemu command and its result.
	Verbose bool
	// Discover lists qemu processes (default: qemu.Discover("/proc")).
	Discover func() ([]*qemu.Process, error)
	// ProcessAlive tells if a qemu process still runs (default: /proc).
	ProcessAlive func(pid int, startTime uint64) bool
	// NewMonitor creates a monitor for a socket (default: qemu.NewQMP or
	// qemu.NewHMP).
	NewMonitor func(proto, path string) qemu.Monitor
	// SocketExists tells if a monitor socket exists (default: qemu.IsSocket).
	SocketExists func(path string) bool
	// BackgroundPoll is the interval of checking detaching devices in the
	// background (default 2s).
	BackgroundPoll time.Duration
}

// backgroundMax is how long a detaching device is checked in the
// background.
const backgroundMax = time.Hour

// Server is the fake-cxl-pool server state.
type Server struct {
	cfg     *Config
	opts    Options
	log     *log.Logger
	started time.Time

	mu      sync.RWMutex
	serials *serials
	devices map[string]*device
	hosts   map[string]*host
	atts    map[string]*attachment

	// adoptWarned remembers the qemu devices that were not adopted because
	// their serial is not the device's, or the device is unshared and
	// attached elsewhere (warn once).
	adoptWarned map[string]bool

	events *broker
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// refreshMu serializes host refreshes.
	refreshMu sync.Mutex
}

type device struct {
	api.Device
	serial  uint64
	dynamic bool // created over the API (persisted)
	// local is the pre-declared memory backend of a local device, nil
	// while the qemu command line of its host has none. An attach reuses
	// the backend object.
	local *binding
}

type binding struct {
	host     string
	ObjectID string
	Bus      string
	Serial   uint64
}

// localTo returns the binding of a local device to the host, nil if it
// has none.
func (d *device) localTo(hostName string) *binding {
	if d.local == nil || d.local.host != hostName {
		return nil
	}
	return d.local
}

type host struct {
	name      string
	uuid      string
	proc      *qemu.Process
	pid       int
	startTime uint64
	qmpPath   string
	hmpPath   string
	control   string
	mon       qemu.Monitor
	monKey    string
	version   string
	state     string
	lastErr   string
	tree      *qemu.Tree
	// gen counts attachment changes of the host. treeGen is the gen at the
	// time tree was read: a tree read while attachments changed may miss a
	// device that was just plugged (or show one just deleted), so reconcile
	// trusts the tree only if treeGen == gen.
	gen       uint64
	treeGen   uint64
	hotRemove bool
	fmw       map[string]int64 // host bridge -> fixed memory window size (from qemu)
	counter   int              // hotplug counter for qemu device ids
	// unplugged are the qemu device ids that got device_del in this qemu
	// instance. They are never adopted: on stock qemu such a device stays
	// in the tree as a zombie after the guest has removed it.
	unplugged map[string]bool
	fromState bool // known only from the state file so far
	opMu      sync.Mutex
}

type attachment struct {
	api.Attachment
	finalizing    bool
	bgWaiting     bool
	unplugPending bool          // device_del accepted, qemu has not deleted the device yet
	leaked        bool          // device gone from qemu, backend still mapped in the guest
	done          chan struct{} // closed when attaching ends
}

// New creates a server from a completed configuration.
func New(cfg *Config, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Discover == nil {
		opts.Discover = func() ([]*qemu.Process, error) { return qemu.Discover("/proc") }
	}
	if opts.ProcessAlive == nil {
		opts.ProcessAlive = func(pid int, st uint64) bool { return qemu.ProcessAlive("/proc", pid, st) }
	}
	if opts.SocketExists == nil {
		opts.SocketExists = qemu.IsSocket
	}
	if opts.BackgroundPoll == 0 {
		opts.BackgroundPoll = 2 * time.Second
	}
	s := &Server{
		cfg:     cfg,
		opts:    opts,
		log:     opts.Logger,
		started: time.Now(),
		serials: newSerials(uint64(*cfg.SharedSerialBase), uint64(*cfg.SerialBase)),
		devices: map[string]*device{},
		hosts:   map[string]*host{},
		atts:    map[string]*attachment{},
		events:  newBroker(),

		adoptWarned: map[string]bool{},
	}
	if opts.NewMonitor == nil {
		s.opts.NewMonitor = s.newMonitor
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	for _, dc := range cfg.Devices {
		if err := s.addStaticDevice(dc); err != nil {
			return nil, err
		}
	}
	if err := s.loadState(); err != nil {
		return nil, err
	}
	return s, nil
}

// filePath returns the data file of a pool device.
func (s *Server) filePath(name string) string {
	return filepath.Join(s.cfg.Dir, name+".raw")
}

// localSerialLocked returns true if a local device has the serial.
func (s *Server) localSerialLocked(sn uint64) bool {
	return s.localDeviceWithSerialLocked(sn, "") != nil
}

// localDeviceWithSerialLocked returns a local device (bound to hostName, or
// to any host if "") that has the serial.
func (s *Server) localDeviceWithSerialLocked(sn uint64, hostName string) *device {
	for _, d := range s.devices {
		if b := d.local; b != nil && b.Serial == sn && (hostName == "" || b.host == hostName) {
			return d
		}
	}
	return nil
}

func (s *Server) newMonitor(proto, path string) qemu.Monitor {
	var logf qemu.Logf
	if s.opts.Verbose {
		logf = func(format string, args ...any) { s.log.Printf(format, args...) }
	}
	if proto == qemu.ProtoQMP {
		return qemu.NewQMP(path, logf)
	}
	return qemu.NewHMP(path, logf)
}

func (s *Server) logf(format string, args ...any) { s.log.Printf(format, args...) }

func (s *Server) debugf(format string, args ...any) {
	if s.opts.Verbose {
		s.log.Printf(format, args...)
	}
}

func (s *Server) addStaticDevice(dc DeviceConfig) error {
	if err := validName(dc.Name); err != nil {
		return fmt.Errorf("device %q: %w", dc.Name, err)
	}
	if int64(dc.Size)%CapacityMultiplier != 0 {
		return fmt.Errorf("device %q: size must be a multiple of %s", dc.Name, api.FormatSize(CapacityMultiplier))
	}
	path := s.filePath(dc.Name)
	sn := uint64(dc.Serial)
	if err := s.serials.Use(sn, dc.Name); err != nil {
		return fmt.Errorf("device %q: %w", dc.Name, err)
	}
	d := &device{
		Device: api.Device{
			Name:       dc.Name,
			Serial:     cxlapi.FormatSerial(sn),
			Size:       int64(dc.Size),
			Shared:     dc.Shared,
			Persistent: dc.Persistent,
			Backend:    api.BackendFile,
			Path:       path,
			Scope:      api.ScopePool,
			Static:     true,
			Created:    time.Now().UTC(),
		},
		serial: sn,
	}
	if dc.Persistent {
		d.LSAPath = lsaPath(path)
	}
	if err := ensureFiles(d.Path, d.LSAPath, d.Size); err != nil {
		return fmt.Errorf("device %q: %w", dc.Name, err)
	}
	s.devices[d.Name] = d
	return nil
}

// Start runs the initial host refresh and the periodic discovery loop.
func (s *Server) Start() {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	s.Refresh(ctx)
	cancel()
	s.mu.Lock()
	for id, a := range s.atts {
		if s.devices[a.Device] == nil {
			// a config device that was removed, or a local device of a VM
			// that is gone: nothing can detach it through the server
			s.logf("WARNING: attachment %s: device %s is not known any more (removed from the config?); "+
				"record dropped, qemu device %s of host %s left as it is", id, a.Device, a.QemuDeviceID, a.Host)
			delete(s.atts, id)
			s.saveStateLocked()
		}
	}
	for _, a := range s.atts {
		if a.State == api.AttachmentDetaching || a.State == api.AttachmentFailed {
			// after a restart: device_del was sent before, or the state is
			// unknown; completes when qemu has deleted the device
			a.unplugPending = true
			s.startBackgroundWaitLocked(a, time.Duration(s.cfg.DetachTimeout))
		}
	}
	s.mu.Unlock()
	s.wg.Go(func() {
		t := time.NewTicker(time.Duration(s.cfg.Discovery.Interval))
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(s.ctx, time.Duration(s.cfg.Discovery.Interval))
				s.Refresh(ctx)
				cancel()
			}
		}
	})
}

// Stop stops background work and closes monitors.
func (s *Server) Stop() {
	s.cancel()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hosts {
		if h.mon != nil {
			h.mon.Close()
		}
	}
	s.saveStateLocked()
}

// CloseEvents ends every event stream, and every later one.
func (s *Server) CloseEvents() {
	s.events.close()
}

// Refresh rediscovers qemu processes, queries the device tree of every
// running host, and reconciles attachments with what qemu has.
func (s *Server) Refresh(ctx context.Context) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	procs, err := s.opts.Discover()
	discovered := err == nil
	if err != nil {
		s.logf("discovery failed: %v", err)
	} else {
		procs = s.filterProcs(procs)
	}
	s.mu.Lock()
	s.updateHostsLocked(procs, discovered)
	var hs []*host
	for _, h := range s.hosts {
		if h.mon != nil && h.state != api.HostStopped {
			hs = append(hs, h)
		}
	}
	s.mu.Unlock()
	s.queryHosts(ctx, hs)
	s.reconcile(ctx, hs)
}

// refreshTrees queries the trees of running hosts (all, or the named
// ones) and reconciles them.
func (s *Server) refreshTrees(ctx context.Context, names ...string) {
	s.mu.RLock()
	var hs []*host
	for _, h := range s.hosts {
		if h.mon == nil || h.state == api.HostStopped {
			continue
		}
		if len(names) > 0 && !slices.Contains(names, h.name) {
			continue
		}
		hs = append(hs, h)
	}
	s.mu.RUnlock()
	s.queryHosts(ctx, hs)
	s.reconcile(ctx, hs)
}

// queryHosts reads version and tree of the hosts in parallel.
func (s *Server) queryHosts(ctx context.Context, hs []*host) {
	var wg sync.WaitGroup
	for _, h := range hs {
		s.mu.RLock()
		mon, needVersion, gen := h.mon, h.version == "", h.gen
		s.mu.RUnlock()
		wg.Go(func() {
			qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var (
				version string
				err     error
			)
			if needVersion {
				version, err = mon.Version(qctx)
			}
			var (
				tree         *qemu.Tree
				fmw          map[string]int64
				hotRemove    bool
				hotRemoveErr error
				memdevs      []qemu.Memdev
			)
			if err == nil {
				tree, err = mon.QueryTree(qctx)
			}
			if err == nil && needVersion {
				if ws, ferr := qemu.QueryFMW(qctx, mon); ferr == nil {
					fmw = map[string]int64{}
					for _, w := range ws {
						for _, t := range w.Targets {
							// an interleaved window is shared by its targets
							fmw[t] += w.Size / int64(len(w.Targets))
						}
					}
				} else {
					s.debugf("host %s: cannot read cxl-fmw: %v", h.name, ferr)
				}
				hotRemove, hotRemoveErr = qemu.HotRemoveCapable(qctx, mon, tree)
				if hotRemoveErr != nil {
					s.logf("host %s: cannot check hot-remove capability: %v", h.name, hotRemoveErr)
				}
				// leftover fcp_* objects (object-del failed) seed the counter
				if mds, merr := mon.QueryMemdevs(qctx); merr == nil {
					memdevs = mds
				}
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if h.mon != mon {
				return // monitor replaced meanwhile
			}
			if s.qemuGoneLocked(h) {
				return
			}
			if err != nil {
				if h.state != api.HostUnreachable {
					s.logf("host %s: monitor %s unreachable: %v", h.name, h.control, err)
				}
				h.state = api.HostUnreachable
				h.lastErr = err.Error()
				return
			}
			if version != "" {
				h.version = version
				h.fmw = fmw
				h.hotRemove = hotRemove
				if !hotRemove && hotRemoveErr == nil {
					s.logf("host %s: WARNING: qemu %s cannot complete hot-remove from cxl-downstream slots "+
						"(no power_controller_present): a detached device stays in qemu, its slot and backend "+
						"cannot be reused until the VM restarts", h.name, version)
				}
			}
			if h.state != api.HostRunning {
				s.logf("host %s: running, qemu %s, control %s", h.name, h.version, h.control)
				s.events.publish(api.EventHostUpdated, s.hostViewLocked(h))
			}
			h.state = api.HostRunning
			h.lastErr = ""
			for _, md := range memdevs {
				s.seedCounterLocked(h, md.ID)
			}
			s.storeTreeLocked(h, tree, gen, false)
		})
	}
	wg.Wait()
}

var fcpIDRe = regexp.MustCompile(`^fcp_.+\.hp([0-9]+)(-lsa)?$`)

// seedCounterLocked raises the hotplug counter of a host above the N of a
// qemu id fcp_<device>.hp<N> (or its label storage area backend
// fcp_<device>.hp<N>-lsa) that exists in qemu, so that a new attachment
// never reuses an id (state file lost or disabled, stock qemu zombies,
// objects whose object-del failed).
func (s *Server) seedCounterLocked(h *host, id string) {
	if m := fcpIDRe.FindStringSubmatch(id); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > h.counter {
			h.counter = n
		}
	}
}

// storeTreeLocked stores a device tree that was read when the attachment
// generation of the host was gen. A tree that may be stale (attachments
// changed while it was read) is stored only if force; reconcile skips it.
func (s *Server) storeTreeLocked(h *host, tree *qemu.Tree, gen uint64, force bool) {
	if tree == nil {
		return
	}
	for _, t3 := range tree.Type3 {
		s.seedCounterLocked(h, t3.ID)
		s.seedCounterLocked(h, t3.VolatileMemdev)
		s.seedCounterLocked(h, t3.PersistentMemdev)
		s.seedCounterLocked(h, t3.LSA)
	}
	if gen != h.gen && !force {
		s.debugf("host %s: attachments changed while the device tree was read, tree not used", h.name)
		return
	}
	h.tree = tree
	h.treeGen = gen
}

// filterProcs keeps the processes whose name matches discovery.names.
func (s *Server) filterProcs(procs []*qemu.Process) []*qemu.Process {
	if len(s.cfg.Discovery.Names) == 0 {
		return procs
	}
	var out []*qemu.Process
	for _, p := range procs {
		for _, pat := range s.cfg.Discovery.Names {
			if ok, _ := path.Match(pat, p.Name); ok {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// updateHostsLocked updates the hosts from the discovered processes.
func (s *Server) updateHostsLocked(procs []*qemu.Process, discovered bool) {
	byName := map[string]*qemu.Process{}
	for _, p := range procs {
		if p.Name == "" {
			p.Name = "qemu-" + strconv.Itoa(p.PID)
		}
		if prev, ok := byName[p.Name]; ok {
			s.debugf("discovery: qemu pid %d has the same name %q as pid %d, ignored", p.PID, p.Name, prev.PID)
			continue
		}
		byName[p.Name] = p
	}
	seen := map[string]bool{}
	for name, p := range byName {
		seen[name] = true
		h := s.hosts[name]
		if h == nil {
			h = &host{name: name, state: api.HostUnreachable}
			s.hosts[name] = h
			s.logf("discovered qemu pid %d: host %s", p.PID, name)
		}
		s.updateHostLocked(h, p)
	}
	for name, h := range s.hosts {
		if seen[name] {
			h.fromState = false
			continue
		}
		if h.fromState && discovered {
			// a VM of a previous server run that is gone: forget it
			s.hostGoneLocked(h, "qemu process not found")
			delete(s.hosts, name)
			s.saveStateLocked()
			continue
		}
		// a discovered host whose qemu is gone, or a host only known from
		// the state file
		if discovered || (h.pid > 0 && !s.opts.ProcessAlive(h.pid, h.startTime)) {
			s.updateHostLocked(h, nil)
		}
	}
}

// updateHostLocked updates a host from its qemu process (nil if not
// running or not found).
func (s *Server) updateHostLocked(h *host, p *qemu.Process) {
	if p == nil {
		s.processGoneLocked(h, "qemu process not found")
		return
	}
	if h.pid != 0 && (h.pid != p.PID || (h.startTime != 0 && p.StartTime != 0 && h.startTime != p.StartTime)) {
		s.hostGoneLocked(h, fmt.Sprintf("qemu restarted (pid %d -> %d)", h.pid, p.PID))
	}
	h.proc = p
	h.pid = p.PID
	h.startTime = p.StartTime
	h.uuid = p.UUID
	if h.state == api.HostStopped {
		h.state = api.HostUnreachable // until the monitor answers
	}
	qmpPath, hmpPath := "", ""
	for _, q := range p.QMPSockets {
		if reservedSockets[filepath.Base(q)] {
			continue
		}
		if s.opts.SocketExists(q) {
			qmpPath = q
			break
		}
	}
	for _, m := range p.HMPSockets {
		if s.opts.SocketExists(m) {
			hmpPath = m
			break
		}
	}
	s.setMonitorLocked(h, qmpPath, hmpPath)
	if s.cfg.Discovery.LocalDevicesEnabled() {
		s.updateLocalDevicesLocked(h, p)
	}
}

// processGoneLocked handles a host whose qemu process is not running.
func (s *Server) processGoneLocked(h *host, why string) {
	if h.pid > 0 || h.state != api.HostStopped {
		s.hostGoneLocked(h, why)
	}
}

// qemuGoneLocked checks after a monitor call that the qemu process of the
// host still runs. A monitor socket outlives its qemu: a restarted qemu
// answers on the same path before discovery has seen its new pid. Its
// tree is not the old process's, and the next discovery would drop a
// device attached into it ("qemu restarted") and adopt it without owner.
// If the process is gone, the host stops now, as if discovery had not
// found it, and the next discovery takes the new process. It returns true
// then.
func (s *Server) qemuGoneLocked(h *host) bool {
	if h.pid == 0 || s.opts.ProcessAlive(h.pid, h.startTime) {
		return false
	}
	s.processGoneLocked(h, fmt.Sprintf("qemu process %d not found", h.pid))
	return true
}

func (s *Server) setMonitorLocked(h *host, qmpPath, hmpPath string) {
	h.qmpPath, h.hmpPath = qmpPath, hmpPath
	proto, path := "", ""
	force := s.cfg.Discovery.Control
	switch {
	case force == qemu.ProtoHMP:
		if hmpPath != "" {
			proto, path = qemu.ProtoHMP, hmpPath
		}
	case force == qemu.ProtoQMP:
		if qmpPath != "" {
			proto, path = qemu.ProtoQMP, qmpPath
		}
	case qmpPath != "":
		proto, path = qemu.ProtoQMP, qmpPath
	case hmpPath != "":
		proto, path = qemu.ProtoHMP, hmpPath
	}
	key := proto + ":" + path
	if key == h.monKey {
		return
	}
	if h.mon != nil {
		h.mon.Close()
		h.mon = nil
	}
	h.monKey = key
	h.control = proto
	h.version = ""
	if proto != "" {
		h.mon = s.opts.NewMonitor(proto, path)
		s.debugf("host %s: using %s monitor %s", h.name, proto, path)
	} else {
		h.lastErr = "no monitor socket"
	}
}

// hostGoneLocked marks a host stopped and drops its attachments: a new
// qemu process has none of the hotplugged devices.
func (s *Server) hostGoneLocked(h *host, why string) {
	if h.state != api.HostStopped {
		s.logf("host %s: stopped: %s", h.name, why)
	}
	h.state = api.HostStopped
	h.pid, h.startTime, h.proc, h.tree, h.version = 0, 0, nil, nil, ""
	h.unplugged = nil
	if h.mon != nil {
		h.mon.Close()
		h.mon = nil
	}
	h.monKey = ""
	for id, a := range s.atts {
		if a.Host == h.name {
			s.logf("attachment %s: dropped: %s", id, why)
			delete(s.atts, id)
			a.State = api.AttachmentDetached
			s.publishAttachmentLocked(a)
		}
	}
	s.saveStateLocked()
	s.events.publish(api.EventHostUpdated, s.hostViewLocked(h))
}

// reservedSockets are monitor sockets of e2e VMs that belong to someone
// else: qmp-e2e.sock is for the test framework's vm-qmp (a QMP socket
// serves one client at a time; the server keeps qmp.sock connected).
var reservedSockets = map[string]bool{"qmp-e2e.sock": true}

var nonNameChars = regexp.MustCompile(`[^a-z0-9.-]+`)

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	s = nonNameChars.ReplaceAllString(s, "-")
	return strings.Trim(s, "-.")
}

// localDeviceName returns the device name of a pre-declared ram backend.
func localDeviceName(hostName, memdev string) string {
	m := strings.TrimPrefix(sanitizeName(memdev), "cxl-")
	return sanitizeName(hostName) + "." + m
}

// updateLocalDevicesLocked turns the pre-declared CXL backends of a qemu
// command line into devices bound to the host.
func (s *Server) updateLocalDevicesLocked(h *host, p *qemu.Process) {
	present := map[string]bool{}
	for _, lb := range p.LocalBackends() {
		name := localDeviceName(h.name, lb.Memdev)
		d := s.devices[name]
		if d == nil {
			d = s.newLocalDeviceLocked(name, lb)
		}
		sn := d.serial
		if lb.HasSerial {
			sn = lb.Serial
		}
		d.local = &binding{host: h.name, ObjectID: lb.ObjectID, Bus: lb.Bus, Serial: sn}
		d.LocalHost = h.name
		present[d.Name] = true
	}
	for _, d := range s.devices {
		if d.localTo(h.name) != nil && !present[d.Name] {
			d.local, d.LocalHost = nil, ""
		}
	}
}

func (s *Server) newLocalDeviceLocked(name string, lb qemu.LocalBackend) *device {
	// Local devices have a serial namespace of their own host: VMs may
	// declare the same serial. Collisions with pool devices are refused at
	// attach and create time.
	sn := lb.Serial
	if !lb.HasSerial {
		sn = s.serials.NextExcept(name, false, s.localSerialLocked)
	} else if owner, ok := s.serials.Owner(sn); ok {
		s.logf("WARNING: local device %s has the serial 0x%x of pool device %s: that device cannot be attached to the same VM", name, sn, owner)
	}
	d := &device{
		Device: api.Device{
			Name:    name,
			Serial:  cxlapi.FormatSerial(sn),
			Size:    lb.Size,
			Backend: api.BackendRAM,
			Scope:   api.ScopeLocal,
			Created: time.Now().UTC(),
		},
		serial: sn,
	}
	s.devices[name] = d
	s.debugf("local device %s: object %s", name, lb.ObjectID)
	return d
}

var fcpObjectRe = regexp.MustCompile(`^fcp_(.+)\.hp[0-9]+$`)

// reconcile compares attachments with the device trees of the hosts:
// attachments whose qemu device is gone are finalized, and cxl-type3
// devices that use a known backend but have no attachment are adopted.
func (s *Server) reconcile(ctx context.Context, hs []*host) {
	var finalize []*attachment
	s.mu.Lock()
	changed := false
	for _, h := range hs {
		if h.state != api.HostRunning || h.tree == nil || h.treeGen != h.gen {
			// no tree, or attachments changed after it was read
			continue
		}
		inTree := map[string]bool{}
		for _, t3 := range h.tree.Type3 {
			inTree[t3.ID] = true
		}
		byQemuID := map[string]*attachment{}
		for _, a := range s.atts {
			if a.Host != h.name {
				continue
			}
			byQemuID[a.QemuDeviceID] = a
			if a.State == api.AttachmentAttaching || a.leaked || a.finalizing {
				continue
			}
			if !inTree[a.QemuDeviceID] {
				if a.State == api.AttachmentAttached {
					s.logf("attachment %s: qemu device %s is gone", a.ID, a.QemuDeviceID)
				}
				finalize = append(finalize, a)
			}
		}
		for _, t3 := range h.tree.Type3 {
			if byQemuID[t3.ID] != nil || h.unplugged[t3.ID] {
				continue
			}
			d := s.deviceForObjectLocked(h.name, t3.Memdev())
			if d == nil {
				continue
			}
			id := api.AttachmentID(d.Name, h.name)
			if s.atts[id] != nil {
				continue
			}
			if fcpObjectRe.MatchString(t3.Memdev()) && t3.HasSerial && t3.Serial != d.serial {
				// a device of the same name, plugged by another pool server
				key := h.name + "/" + t3.ID
				if !s.adoptWarned[key] {
					s.adoptWarned[key] = true
					s.logf("WARNING: host %s has qemu device %s with serial %s, not the serial %s of device %s: not adopted (another pool server's device?)",
						h.name, t3.ID, cxlapi.FormatSerial(t3.Serial), d.Serial, d.Name)
				}
				continue
			}
			if !d.Shared {
				if other := s.otherHostAttachmentLocked(d, h.name); other != nil {
					key := h.name + "/" + t3.ID
					if !s.adoptWarned[key] {
						s.adoptWarned[key] = true
						s.logf("WARNING: host %s has qemu device %s of unshared device %s, which is attached to %s: not adopted",
							h.name, t3.ID, d.Name, other.Host)
					}
					continue
				}
			}
			now := time.Now().UTC()
			a := &attachment{Attachment: api.Attachment{
				ID:              id,
				Device:          d.Name,
				Host:            h.name,
				Serial:          cxlapi.FormatSerial(t3.Serial),
				Slot:            s.slotLocked(h, t3.Bus),
				QemuDeviceID:    t3.ID,
				QemuObjectID:    t3.Memdev(),
				QemuLSAObjectID: t3.LSA,
				State:           api.AttachmentAttached,
				Adopted:         true,
				Created:         now,
				Updated:         now,
			}}
			if !t3.HasSerial {
				a.Serial = d.Serial
			}
			s.atts[id] = a
			changed = true
			s.logf("attachment %s: adopted qemu device %s (bus %s, backend %s)", id, t3.ID, t3.Bus, t3.Memdev())
			s.publishAttachmentLocked(a)
		}
		// warn again about a qemu device that is gone and comes back
		for key := range s.adoptWarned {
			if id, ok := strings.CutPrefix(key, h.name+"/"); ok && !inTree[id] {
				delete(s.adoptWarned, key)
			}
		}
	}
	if changed {
		s.saveStateLocked()
	}
	s.mu.Unlock()
	for _, a := range finalize {
		s.completeDetach(ctx, a)
	}
}

// otherHostAttachmentLocked returns an attachment of the device to a host
// other than hostName.
func (s *Server) otherHostAttachmentLocked(d *device, hostName string) *attachment {
	for _, a := range s.atts {
		if a.Device == d.Name && a.Host != hostName {
			return a
		}
	}
	return nil
}

// deviceForObjectLocked finds the device of a memory backend object id in
// a host.
func (s *Server) deviceForObjectLocked(hostName, objID string) *device {
	if objID == "" {
		return nil
	}
	if m := fcpObjectRe.FindStringSubmatch(objID); m != nil {
		return s.devices[m[1]]
	}
	for _, d := range s.devices {
		if b := d.localTo(hostName); b != nil && b.ObjectID == objID {
			return d
		}
	}
	return nil
}

// completeResult is the outcome of completeDetach.
type completeResult int

const (
	completeDone    completeResult = iota // finalized: detached
	completeLeaked                        // the guest keeps the memory: failed
	completeUnknown                       // cannot tell now: left as is, retried later
)

// completeDetach handles an attachment whose qemu device is gone from the
// device tree. If an HDM decoder still maps the backend, the guest did not
// release the memory: the device is a zombie that qemu cannot finalize, and
// the backend stays mapped into that guest until qemu restarts. Such an
// attachment becomes "failed" and is kept, so that the device is not given
// to another host as if it was free. If the check cannot be done (monitor
// error), the attachment is left as is: reconcile and the background
// waiter retry. Otherwise the detach is finalized.
func (s *Server) completeDetach(ctx context.Context, a *attachment) completeResult {
	s.mu.RLock()
	h := s.hosts[a.Host]
	var mon qemu.Monitor
	if h != nil {
		mon = h.mon
	}
	s.mu.RUnlock()
	if a.QemuObjectID != "" {
		if mon == nil {
			return completeUnknown
		}
		mapped := false
		for range 3 {
			mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			m, err := mon.BackendMapped(mctx, a.QemuObjectID)
			cancel()
			if err != nil {
				s.logf("attachment %s: cannot check if backend %s is still mapped (retried later): %v", a.ID, a.QemuObjectID, err)
				return completeUnknown
			}
			if mapped = m; !mapped {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if mapped {
			s.markLeaked(a)
			return completeLeaked
		}
	}
	s.finalizeDetach(ctx, a)
	return completeDone
}

// markLeaked records that the guest kept the memory of a removed device.
func (s *Server) markLeaked(a *attachment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.atts[a.ID] != a || a.leaked {
		return
	}
	a.leaked = true
	a.unplugPending = false
	a.State = api.AttachmentFailed
	a.Error = fmt.Sprintf("leaked: qemu device %s is gone but backend %s is still mapped into the guest "+
		"(the guest did not release the memory before detach); it stays mapped until the VM restarts",
		a.QemuDeviceID, a.QemuObjectID)
	a.Updated = time.Now().UTC()
	s.logf("attachment %s: %s", a.ID, a.Error)
	s.saveStateLocked()
	s.publishAttachmentLocked(a)
}

// markTimedOutLocked marks a detach that did not complete in time.
func (s *Server) markTimedOutLocked(a *attachment, timeout time.Duration) {
	if a.State == api.AttachmentFailed {
		return
	}
	a.State = api.AttachmentFailed
	if h := s.hosts[a.Host]; h != nil && !h.hotRemove && a.Slot.Kind == api.SlotDownstream {
		a.Error = fmt.Sprintf("qemu did not delete the device within %s: this qemu (%s) cannot complete hot-remove from "+
			"cxl-downstream slots, the device stays in qemu as a zombie with its backend mapped until the VM restarts "+
			"(the guest has removed it if it was released)", timeout, h.version)
	} else {
		a.Error = fmt.Sprintf("guest did not release the device within %s; qemu keeps the backend mapped until the VM restarts "+
			"(the attachment clears itself if qemu deletes the device later)", timeout)
	}
	a.Updated = time.Now().UTC()
	s.logf("attachment %s: %s", a.ID, a.Error)
	s.saveStateLocked()
	s.publishAttachmentLocked(a)
}

// finalizeDetach completes a detach whose qemu device is gone: deletes the
// backend objects (unless pre-declared) and removes the attachment.
func (s *Server) finalizeDetach(ctx context.Context, a *attachment) {
	s.mu.Lock()
	if a.finalizing || s.atts[a.ID] != a {
		s.mu.Unlock()
		return
	}
	a.finalizing = true
	h := s.hosts[a.Host]
	var mon qemu.Monitor
	var objIDs []string
	if strings.HasPrefix(a.QemuObjectID, "fcp_") {
		objIDs = append(objIDs, a.QemuObjectID)
		if a.QemuLSAObjectID != "" {
			objIDs = append(objIDs, a.QemuLSAObjectID)
		}
	}
	if h != nil {
		mon = h.mon
	}
	s.mu.Unlock()
	if len(objIDs) > 0 && mon != nil {
		h.opMu.Lock()
		for _, id := range objIDs {
			octx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := mon.ObjectDel(octx, id)
			cancel()
			if err != nil && !qemu.IsNotFound(err) {
				// stock qemu keeps the backend mapped after unplug
				s.logf("attachment %s: object-del %s failed (left in qemu): %v", a.ID, id, err)
			}
		}
		h.opMu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.atts[a.ID] == a {
		delete(s.atts, a.ID)
	}
	a.State = api.AttachmentDetached
	a.Updated = time.Now().UTC()
	s.logf("attachment %s: detached (qemu device %s)", a.ID, a.QemuDeviceID)
	s.saveStateLocked()
	s.publishAttachmentLocked(a)
}

// startBackgroundWaitLocked keeps checking, with short monitor calls,
// whether qemu has deleted the device of a detaching (or timed out)
// attachment, and completes the detach when it has. If failAfter > 0 and
// the device is still there after it, the attachment is marked failed.
func (s *Server) startBackgroundWaitLocked(a *attachment, failAfter time.Duration) {
	if a.bgWaiting {
		return
	}
	a.bgWaiting = true
	s.wg.Go(func() {
		defer func() {
			s.mu.Lock()
			a.bgWaiting = false
			s.mu.Unlock()
		}()
		start := time.Now()
		deadline := start.Add(backgroundMax)
		for time.Now().Before(deadline) {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(s.opts.BackgroundPoll):
			}
			s.mu.RLock()
			cur := s.atts[a.ID]
			h := s.hosts[a.Host]
			active := a.State == api.AttachmentDetaching || (a.State == api.AttachmentFailed && a.unplugPending)
			leaked := a.leaked
			var mon qemu.Monitor
			if h != nil {
				mon = h.mon
			}
			s.mu.RUnlock()
			if cur != a || !active || leaked {
				return
			}
			if mon == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
			exists, err := mon.DeviceExists(ctx, a.QemuDeviceID)
			cancel()
			if err == nil && !exists {
				if s.completeDetach(s.ctx, a) != completeUnknown {
					return
				}
				continue
			}
			if failAfter > 0 && time.Since(start) > failAfter {
				s.mu.Lock()
				if s.atts[a.ID] == a {
					s.markTimedOutLocked(a, failAfter)
				}
				s.mu.Unlock()
			}
		}
		s.logf("attachment %s: stopped waiting for qemu device %s to be deleted", a.ID, a.QemuDeviceID)
	})
}

// publishAttachmentLocked is called on every change of an attachment: it
// bumps the attachment generation of the host and publishes events.
func (s *Server) publishAttachmentLocked(a *attachment) {
	if h := s.hosts[a.Host]; h != nil {
		h.gen++
	}
	s.events.publish(api.EventAttachmentUpdated, a.Attachment)
	if d := s.devices[a.Device]; d != nil {
		s.events.publish(api.EventDeviceUpdated, s.deviceViewLocked(d))
	}
}

// lookupHostLocked finds a host by name or uuid.
func (s *Server) lookupHostLocked(ref string) *host {
	if h := s.hosts[ref]; h != nil {
		return h
	}
	nref := api.NormUUID(ref)
	for _, h := range s.hosts {
		if h.uuid != "" && api.NormUUID(h.uuid) == nref {
			return h
		}
	}
	return nil
}

// resolveHostLocked implements GET /hosts/resolve.
func (s *Server) resolveHostLocked(hostname, uuid string) *host {
	if nu := api.NormUUID(uuid); nu != "" {
		for _, h := range s.hosts {
			if h.uuid != "" && api.NormUUID(h.uuid) == nu {
				return h
			}
		}
	}
	if hostname == "" {
		return nil
	}
	if h := s.hosts[hostname]; h != nil {
		return h
	}
	first, _, _ := strings.Cut(hostname, ".")
	if h := s.hosts[first]; h != nil {
		return h
	}
	return nil
}
