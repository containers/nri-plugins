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

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/containers"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/engine"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/leases"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/provenance"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

type driver struct {
	// nri is the NRI plugin, nil when NRI did not start.
	nri *containers.Plugin
	// local is cxl.nri.io, pool the pool driver (nil when it is
	// disabled), publisher the publisher of the node's ResourceSlices,
	// watcher the watcher of the pool devices (nil without a client).
	local     *dra.Plugin
	pool      *dra.Plugin
	publisher *dra.Publisher
	watcher   *provenance.Watcher
	// stopBroker stops the lease broker, nil when lending is disabled.
	stopBroker func()
	// closeStore writes the pending commits of the state store and
	// closes it.
	closeStore func()
	// cancelRun ends the context of the parts that run until Shutdown:
	// the NRI stub, the kubelet helpers, the publisher, the watcher and
	// the periodic passes.
	cancelRun func()
	// stop ends the periodic passes of the driver.
	stop chan struct{}
	// fatal gets the first fatal error of the kubelet plugins: run stops
	// the driver and the process exits non-zero.
	fatal chan error
	// events are the uevents of the kernel objects, nil without them;
	// stopEvents stops them.
	events     kernelEvents
	stopEvents func()
}

// loadDriverConfig returns the configuration of the file of --config, and
// logs the container defaults and the filesystem types.
func loadDriverConfig(cxlFlags *flags) (*DriverConfig, error) {
	configStr, err := readConfig(cxlFlags.Config)
	if err != nil {
		return nil, err
	}
	driverConfig, err := newDriverConfigFromString(configStr)
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	driverConfig.logDefaults()
	driverConfig.logFSTypes()
	return driverConfig, nil
}

// newBroker returns the lease broker of the lease claims and the leases of
// the store st, nil when lending is disabled; cxl.nri.io of node tells it
// of its lease claims.
func newBroker(config *pluginConfig, cxlFlags *flags, driverConfig *DriverConfig, st *store.Store, node *dra.Node, k *kernel.Kernel) (*leases.Broker, error) {
	if !cxlFlags.Leases {
		return nil, nil
	}
	b, err := leases.New(leases.Options{
		Store:           st,
		Client:          config.LeaseClient,
		NodeName:        config.Flags.NodeName,
		DataDir:         config.KubeletPluginDir,
		CLI:             cxlFlags.LeaseCLI,
		Image:           cxlFlags.LeaseCompanionImage,
		WaitDefault:     cxlFlags.LeaseWait,
		PoolDriver:      node.Pool.Name(),
		Limits:          cxlFlags.LeaseLimits,
		MountPoints:     func() ([]string, error) { return mountPoints(k) },
		OpenDevice:      k.OpenDevice,
		PublishedDevice: publishedDevice(node),
	})
	if err != nil {
		return nil, err
	}
	node.Local.SetLeases(b)
	return b, nil
}

// mountPoints returns the mount points of the mount namespace that k reads.
func mountPoints(k *kernel.Kernel) ([]string, error) {
	mounts, err := k.Mounts()
	if err != nil {
		return nil, err
	}
	points := make([]string, 0, len(mounts))
	for _, m := range mounts {
		points = append(points, m.Target)
	}
	return points, nil
}

// publishedDevice returns the function that finds the driver and the
// device of a serial: the local memdev that node publishes, else a device
// in the last list of the pool ResourceSlices; nil if there is none.
func publishedDevice(node *dra.Node) func(serial uint64) (string, *resourcev1.Device) {
	return func(serial uint64) (string, *resourcev1.Device) {
		if d, ok := node.Publisher.PublishedDevice(serial); ok {
			return device.DriverName, &d
		}
		if pool := node.Pool.Name(); pool != "" {
			if d, ok := node.Watcher.PoolDevice(serial); ok {
				return pool, &d
			}
		}
		return "", nil
	}
}

// wireLenders sets the lenders of the companion claims of both drivers of
// node: the NRI plugin nri and the broker, none when lending is disabled (a
// lend recorded before is not revoked then). It returns the containers of
// the broker's requests: nri, nil when NRI did not start, which makes lease
// requests and companion claims fail with errLendingUnavailable.
func wireLenders(nri *containers.Plugin, broker *leases.Broker, node *dra.Node) leases.Containers {
	var lender leases.Containers
	var nodes dra.NUMALender
	if nri != nil {
		lender, nodes = nri, nri
	}
	if broker != nil {
		node.SetLenders(nodes, broker)
	} else {
		node.SetLenders(nil, nil)
	}
	return lender
}

// startPlugins registers the kubelet plugins of cxl.nri.io and of the pool
// driver, starts the publisher of the node's ResourceSlices and the watcher
// of the pool devices.
func (d *driver) startPlugins(ctx context.Context, config *pluginConfig, node *dra.Node) error {
	klog.Infof(`Starting DRA driver kubelet plugin
RegistrarDirectoryPath: %v
PluginDataDirectoryPath: %v`,
		config.KubeletPluginsRegistryDir,
		config.KubeletPluginDir)

	if err := node.Local.Start(ctx,
		kubeletplugin.KubeClient(config.Client),
		kubeletplugin.NodeName(config.Flags.NodeName),
		kubeletplugin.DriverName(device.DriverName),
		kubeletplugin.RegistrarDirectoryPath(config.KubeletPluginsRegistryDir),
		kubeletplugin.PluginDataDirectoryPath(config.KubeletPluginDir),
	); err != nil {
		return err
	}
	if err := startPoolDriver(ctx, config, node.Pool); err != nil {
		node.Local.Stop()
		return err
	}
	node.Publisher.Start(ctx)
	go node.Watcher.Watch(ctx, node.Publisher.Trigger, node.Publisher.LastScan)
	return nil
}

// staleBindPeriod is how often the stale lease binds are unmounted again
// between releases.
const staleBindPeriod = 60 * time.Second

// newKernel returns the kernel of the node. It logs what it does, the
// details of its lookups at klog V(3) and what its system calls find
// unexpected as warnings.
func newKernel() *kernel.Kernel {
	k := kernel.New()
	k.Logf = func(format string, args ...any) { klog.InfofDepth(1, "kernel: "+format, args...) }
	k.Debugf = func(format string, args ...any) { klog.V(3).InfofDepth(1, "kernel: "+format, args...) }
	if sys, ok := k.Sys.(*kernel.SysCalls); ok {
		sys.Logf = func(format string, args ...any) { klog.WarningfDepth(1, "kernel: "+format, args...) }
	}
	return k
}

// kernelEvents are the uevents of the kernel objects: they wake the waits
// of the kernel package, and their stable signal the rescans.
type kernelEvents interface {
	kernel.Events
	Start()
	Stop()
	Stable() <-chan struct{}
}

// newKernelEvents returns the uevents of the kernel objects whose stable
// signal comes after stableDuration of quiet (a variable for tests).
var newKernelEvents = func(stableDuration time.Duration) (kernelEvents, error) {
	ev, err := kernel.NewUdevEvents(stableDuration)
	if err != nil {
		return nil, err
	}
	ev.Logf = func(format string, args ...any) { klog.Infof("kernel uevents: "+format, args...) }
	return ev, nil
}

// udevStableDuration is the quiet time after the last uevent before the
// ResourceSlice is rescanned.
const udevStableDuration = 200 * time.Millisecond

// wakeOnUevents starts the one watch of the uevents of the kernel objects,
// and makes them wake the waits of the kernel k early (they still poll).
// It returns the events, nil without them (the waits only poll), and their
// stop.
func wakeOnUevents(k *kernel.Kernel) (kernelEvents, func()) {
	ev, err := newKernelEvents(udevStableDuration)
	if err != nil {
		klog.Errorf("kernel uevents: %v: the waits for kernel objects only poll, and the ResourceSlice is not rescanned on uevents", err)
		return nil, func() {}
	}
	ev.Start()
	k.Events = ev
	return ev, ev.Stop
}

// repairPeriod is how often the fence pass of the NRI plugin tries the
// failed updates of the cpuset.mems of the fenced containers again.
const repairPeriod = 60 * time.Second

// everyPeriod runs pass every period until stop is closed.
func everyPeriod(period time.Duration, stop <-chan struct{}, pass func()) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			pass()
		}
	}
}

// startRescans starts the rescans of the publisher on the stable signal of
// the uevents, if there are uevents.
func (d *driver) startRescans(ctx context.Context) {
	if d.events == nil {
		return
	}
	go rescanLoop(d.stop, d.events.Stable(), func() { d.publisher.Trigger(ctx, false) })
}

// fenceOf returns the fence of the NUMA nodes of the NRI plugin nri that
// the device engine calls before memory goes online, nil with fencing none.
func fenceOf(nri *containers.Plugin) func(ctx context.Context, node int) {
	if !nri.Fencing() {
		return nil
	}
	return nri.Fence
}

// startNRI connects the NRI plugin nri to the runtime and returns it, nil
// when it did not start: then the driver runs without NRI. A Prepare of
// either driver waits for its first Synchronize; without the lease broker
// (withBroker false) there are no lends to restore.
func startNRI(ctx context.Context, nri *containers.Plugin, config *pluginConfig, node *dra.Node, withBroker bool) *containers.Plugin {
	f := config.Flags
	c := containers.Connection{Name: f.NRIName, Idx: f.NRIIdx, Stub: config.NRIStub}
	if err := nri.Start(ctx, c); err != nil {
		klog.Warningf("Failed to start NRI plugin (continuing without it): %v", err)
		return nil
	}
	wait := func(ctx context.Context) error { return waitNRI(ctx, nri) }
	node.Local.SetWaitNRI(wait)
	node.Pool.SetWaitNRI(wait)
	if !withBroker {
		// no lends to restore
		nri.LendsRestored()
	}
	return nri
}

// storeLockWait is how long the driver waits at start for the lock of the
// state store that another driver process holds.
const storeLockWait = 2 * time.Minute

// openDriverStore opens the state store in the plugin data directory of
// cxl.nri.io, and returns it and its close.
func openDriverStore(ctx context.Context, config *pluginConfig, cxlFlags *flags, bootID string) (*store.Store, func(), error) {
	// a driver pod that still runs holds the lock: wait for its exit
	ctx, cancel := context.WithTimeout(ctx, storeLockWait)
	defer cancel()
	st, err := store.Open(ctx, store.Options{Dir: config.KubeletPluginDir, BootID: bootID, NewState: cxlFlags.NewState})
	if err != nil {
		return nil, nil, fmt.Errorf("state store: %w", err)
	}
	return st, func() {
		if err := st.Close(); err != nil {
			klog.Errorf("closing the state store: %v", err)
		}
	}, nil
}

// nodeOptions returns the options of the DRA drivers of the node over the
// kernel k and the store st. Fence and Shutdown are not set.
func nodeOptions(config *pluginConfig, driverConfig *DriverConfig, k *kernel.Kernel, st *store.Store) dra.NodeOptions {
	f := config.Flags
	return dra.NodeOptions{
		Kernel:          k,
		Store:           st,
		Dir:             config.KubeletPluginDir,
		PoolDriver:      f.poolDriver(),
		LeaseFsRoot:     leases.LeaseFsRoot(config.KubeletPluginDir),
		CDIDir:          f.CDIRoot,
		NodeName:        f.NodeName,
		Client:          config.Client,
		Local:           driverConfig.localConfig(),
		Publish:         driverConfig.publishConfig(config.Flags.Leases),
		FSTypes:         driverConfig.fsTypes(),
		ProbeAddressing: driverConfig.verifyAddressing(),
		Timeouts:        engine.Timeouts{Memdev: f.PoolPrepareTimeout, Settle: f.PoolPrepareTimeout},
		Forget:          f.ForgetProvenance,
	}
}

// fenceSeed returns the NUMA nodes that the NRI plugin fences from the
// start: those of the prepared claims of the drivers. A lost store does not
// know the claims, so then they are the NUMA nodes of every online memory
// block of a CXL region.
func fenceSeed(ctx context.Context, st *store.Store, k *kernel.Kernel, drivers ...string) ([]int, error) {
	if st.Lost() == nil {
		return dra.NUMANodes(&st.Snapshot().Records, drivers...), nil
	}
	snap, err := k.ObserveTopology(ctx)
	if err != nil {
		return nil, fmt.Errorf("the NUMA nodes of CXL memory to fence: %w", err)
	}
	var nodes []int
	for _, b := range snap.Blocks {
		if b.State == kernel.BlockOnline && b.NUMANode >= 0 && !slices.Contains(nodes, b.NUMANode) {
			nodes = append(nodes, b.NUMANode)
		}
	}
	slices.Sort(nodes)
	klog.Warningf("the driver state is lost: fencing the NUMA nodes %v of online CXL memory", nodes)
	return nodes, nil
}

// startPoolDriver registers the pool driver as a second kubelet plugin
// (nothing if pool is nil).
func startPoolDriver(ctx context.Context, config *pluginConfig, pool *dra.Plugin) error {
	if pool == nil {
		return nil
	}
	return pool.Start(ctx,
		kubeletplugin.KubeClient(config.Client),
		kubeletplugin.NodeName(config.Flags.NodeName),
		kubeletplugin.DriverName(pool.Name()),
		kubeletplugin.RegistrarDirectoryPath(config.KubeletPluginsRegistryDir),
		kubeletplugin.PluginDataDirectoryPath(filepath.Join(filepath.Dir(config.KubeletPluginDir), pool.Name())),
	)
}

// nriPrepareWait bounds how long a Prepare waits for the first NRI
// Synchronize (waitNRI): well within kubelet's 45 s timeout of the call,
// so that kubelet gets the error and retries (a variable for tests).
var nriPrepareWait = 30 * time.Second

// waitNRI waits before a Prepare of claims until the NRI plugin has
// synchronized with the runtime, at most nriPrepareWait. kubelet creates
// the containers of a pod after its claims are prepared, also after the
// Prepare of a claim that is prepared already. A container created before
// the NRI plugin is in the runtime is only found by Synchronize, and its
// CreateContainer is never seen: a container that its claims steer is
// never steered, a fenced one runs with the fenced nodes until the first
// fence pass. Once Synchronize has returned, the runtime creates no
// container before the NRI plugin gets its CreateContainer. No wait without an
// NRI plugin (nil: it failed to start, and the driver runs without it). The
// lends do not need to be restored: a new container gets the kept lends of
// its leases at its creation.
func waitNRI(ctx context.Context, nri *containers.Plugin) error {
	if nri == nil {
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, nriPrepareWait)
	defer cancel()
	if err := nri.WaitSynchronized(wctx); err != nil {
		return fmt.Errorf("%w: the containers of the claims would be created before the NRI plugin sees them; kubelet retries the Prepare", err)
	}
	return nil
}

// rescanLoop runs rescan at every signal of stable until stop is closed.
func rescanLoop(stop, stable <-chan struct{}, rescan func()) {
	for {
		select {
		case <-stop:
			return
		case <-stable:
			klog.V(3).Info("kernel uevents: sysfs stabilized, signaling rescan")
			rescan()
		}
	}
}

// Shutdown stops the driver: NRI marked stopping, the periodic passes and
// the uevents, the kubelet plugins of both drivers, the broker, NRI, the
// publisher, the watcher and the store.
func (d *driver) Shutdown() {
	klog.V(5).Info("Shutting down driver")

	// First: the runtime may close the NRI connection while the rest
	// stops (the broker waits for its reconciles), which is no loss now.
	if d.nri != nil {
		d.nri.MarkStopping()
	}
	if d.stop != nil {
		close(d.stop)
	}
	if d.stopEvents != nil {
		d.stopEvents()
	}
	// The kubelet plugins before NRI: kubelet creates the containers of a
	// claim that a Prepare finished, and NRI would never see them.
	d.pool.Stop()
	d.local.Stop()
	if d.stopBroker != nil {
		d.stopBroker()
	}
	if d.nri != nil {
		d.nri.Stop()
	}
	d.publisher.Stop()
	if d.cancelRun != nil {
		d.cancelRun()
	}
	if d.closeStore != nil {
		d.closeStore()
	}
}
