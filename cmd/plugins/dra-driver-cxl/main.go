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

// Command nri-dra-driver-cxl is the CXL DRA driver of a node: the kubelet
// plugins of cxl.nri.io (the memory of the node) and cxl-pool.nri.io (the
// devices of a CXL memory pool), the NRI plugin that keeps containers off
// the CXL memory that they did not claim and steers the memory of the
// containers that opt in, and the lease broker of running containers.
// main.go has the flags and the start and stop of the process, driver.go
// puts the parts together, config.go reads the configuration file. The
// parts are the packages of internal: dra (the kubelet plugins and the
// publisher), engine (the kernel objects), store and state (the records),
// containers, steering and mpolinject (the NRI plugin), leases and lend
// (the lease broker), provenance (pool devices or local memdevs). The
// design is in docs/memory/cxl/design.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/containers"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/leases"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/kubernetes/client"
	"github.com/containers/nri-plugins/pkg/version"
)

const (
	// kubeletPluginsDir is the parent of the plugin data directories of
	// the drivers (<dir>/<driver name>).
	kubeletPluginsDir = "/var/lib/kubelet/plugins"
	// kubeletPluginsRegistryDir is where kubelet finds the registration
	// sockets of the drivers.
	kubeletPluginsRegistryDir = "/var/lib/kubelet/plugins_registry"

	defaultCDIRoot = cdiapi.DefaultDynamicDir
	defaultNRIName = "dra-driver-cxl"
	defaultNRIIdx  = "30"
	// defaultPoolPrepareTimeout is how long a Prepare of the pool driver
	// waits for the memdev to appear.
	defaultPoolPrepareTimeout = 30 * time.Second
)

// flags are the command line flags of the driver.
type flags struct {
	NodeName   string
	KubeConfig string
	CDIRoot    string
	// Config is the configuration file, "" for none.
	Config  string
	NRIName string
	NRIIdx  string

	// Pool enables the pool driver.
	Pool bool
	// PoolPrepareTimeout is how long the pool driver waits for a
	// memdev to appear.
	PoolPrepareTimeout time.Duration

	// Leases enables lending local and pool memory to running
	// containers: the lease device, lease sockets and companion pods.
	Leases bool
	// LeaseCLI is the host path of the static cxl-request CLI that is
	// bind-mounted into containers of lease claims.
	LeaseCLI string
	// LeaseCLISource is the path of a cxl-request CLI that is copied to
	// LeaseCLI at start, empty for none.
	LeaseCLISource string
	// LeaseWait is how long a lease request waits for Ready when the
	// request does not say.
	LeaseWait time.Duration
	// LeaseCompanionImage is the image of the companion pods.
	LeaseCompanionImage string
	// LeaseLimits are the limits of the lease sockets and of the leases of
	// a pod and of the node.
	LeaseLimits leases.Limits

	// NewState starts with a new state.json when neither state.json nor
	// state.json.prev can be read.
	NewState bool
	// ForgetProvenance are the serials whose provenance records are
	// removed at start.
	ForgetProvenance []uint64
}

// poolDriver returns the DRA driver name of the pool driver, "" when it is
// disabled.
func (f *flags) poolDriver() string {
	if !f.Pool {
		return ""
	}
	return device.PoolDriverName
}

// serialsFlag is a list of serials as a flag value.
type serialsFlag []uint64

// String returns the serials, comma-separated.
func (s *serialsFlag) String() string {
	if s == nil {
		return ""
	}
	var out []string
	for _, v := range *s {
		out = append(out, api.FormatSerial(v))
	}
	return strings.Join(out, ",")
}

// Set adds the serials of a comma-separated list.
func (s *serialsFlag) Set(value string) error {
	for _, text := range strings.Split(value, ",") {
		if text = strings.TrimSpace(text); text == "" {
			continue
		}
		v, err := api.ParseSerial(text)
		if err != nil {
			return err
		}
		*s = append(*s, v)
	}
	return nil
}

// pluginConfig is the configuration of the kubelet plugins of the driver.
type pluginConfig struct {
	Flags  *flags
	Client coreclientset.Interface
	// LeaseClient is the Kubernetes client of the lease broker. Its rate
	// limiter is not shared with Client.
	LeaseClient coreclientset.Interface
	// Kernel is the kernel of the node.
	Kernel *kernel.Kernel
	// NRIStub, if not nil, connects the NRI plugin to the runtime instead
	// of the NRI socket of the flags.
	NRIStub                   containers.Stub
	KubeletPluginDir          string
	KubeletPluginsRegistryDir string
}

// parseFlags returns the flags of args registered in fs. The defaults of
// --node-name and --kubeconfig are the environment variables NODE_NAME and
// KUBECONFIG.
func parseFlags(fs *flag.FlagSet, args []string, lookupEnv func(string) (string, bool)) (*flags, error) {
	f := &flags{}
	nodeName, _ := lookupEnv("NODE_NAME")
	kubeConfig, _ := lookupEnv("KUBECONFIG")
	fs.StringVar(&f.NodeName, "node-name", nodeName, "name of the node of the driver (required; default $NODE_NAME)")
	fs.StringVar(&f.KubeConfig, "kubeconfig", kubeConfig, "kubeconfig file, empty for the in-cluster configuration (default $KUBECONFIG)")
	fs.StringVar(&f.CDIRoot, "cdi-root", defaultCDIRoot, "directory of the generated CDI specs")
	fs.StringVar(&f.Config, "config", "", "configuration file, JSON or YAML")
	fs.StringVar(&f.NRIName, "nri-name", defaultNRIName, "NRI plugin name to register")
	fs.StringVar(&f.NRIIdx, "nri-idx", defaultNRIIdx, "NRI plugin index (place in the plugin chain, higher is later)")
	fs.BoolVar(&f.Pool, "pool", true, "run the pool driver "+device.PoolDriverName)
	fs.DurationVar(&f.PoolPrepareTimeout, "pool-prepare-timeout", defaultPoolPrepareTimeout, "how long the pool driver waits for the memdev of a pool device to appear")
	fs.BoolVar(&f.Leases, "leases", true, "lend memory to running containers: publish the lease device, serve lease sockets, create companions (needs NRI; pool memory needs the pool driver)")
	fs.StringVar(&f.LeaseCLI, "lease-cli", leases.DefaultCLI, "host path of the static cxl-request CLI, mounted into containers of lease claims")
	fs.StringVar(&f.LeaseCLISource, "lease-cli-source", "", "cxl-request CLI that is copied to --lease-cli at start (empty for none)")
	fs.DurationVar(&f.LeaseWait, "lease-wait", leases.DefaultWait, "default wait of a lease request for the lease to become Ready")
	fs.StringVar(&f.LeaseCompanionImage, "lease-companion-image", leases.DefaultCompanionImage, "container image of the companion pods that hold the devices of leases")
	fs.IntVar(&f.LeaseLimits.MaxConnections, "lease-max-connections", leases.DefaultMaxConnections, "open connections of the lease socket of a lease claim")
	fs.DurationVar(&f.LeaseLimits.IdleTimeout, "lease-idle-timeout", leases.DefaultIdle, "how long a connection of a lease socket may stay without a request")
	fs.IntVar(&f.LeaseLimits.MaxPerPod, "lease-max-per-pod", leases.DefaultMaxLeasesPerPod, "leases of the containers of a pod")
	fs.IntVar(&f.LeaseLimits.LeasesPerNode, "lease-max-per-node", leases.DefaultMaxLeasesPerNode, "leases of the node")
	fs.IntVar(&f.LeaseLimits.RequestsPerMinute, "lease-requests-per-minute", leases.DefaultRequestsPerMinute, "lease requests of a lease claim per minute, also their burst")
	fs.BoolVar(&f.NewState, "new-state", false, "start with a new state.json when neither state.json nor state.json.prev can be read")
	fs.Var((*serialsFlag)(&f.ForgetProvenance), "forget-provenance",
		"serial (0x<hex> or decimal) whose remembered provenance is removed at start, so that a memdev of it is new again; repeatable, or a comma-separated list")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("arguments not supported: %v", fs.Args())
	}
	if f.NodeName == "" {
		return nil, errors.New("--node-name or NODE_NAME is required")
	}
	if f.LeaseCLISource != "" && f.LeaseCLI == "" {
		return nil, errors.New("--lease-cli-source needs --lease-cli")
	}
	if f.LeaseWait < 0 || f.LeaseWait > api.MaxWait {
		return nil, fmt.Errorf("--lease-wait %s: not between 0s and %s", f.LeaseWait, api.MaxWait)
	}
	return f, nil
}

// installLeaseCLI copies the cxl-request CLI src to dst atomically: dst is
// replaced by an executable copy, labelled for containers on SELinux hosts.
func installLeaseCLI(src, dst string) (retErr error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("lease CLI source: %w", err)
	}
	defer func() { _ = in.Close() }()
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("lease CLI directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".*")
	if err != nil {
		return fmt.Errorf("lease CLI: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return fmt.Errorf("lease CLI: copy %s: %w", src, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("lease CLI: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0755); err != nil {
		return fmt.Errorf("lease CLI: %w", err)
	}
	leases.SELinuxLabel(tmp.Name())
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("lease CLI: %w", err)
	}
	klog.Infof("Installed lease CLI %s from %s", dst, src)
	return nil
}

// newDriver returns the driver of the configuration of config, started in
// the order of its start-up below. The waits of the start end with ctx;
// the parts that run until Shutdown do not: Shutdown stops them in its
// order and ends their context last.
//  1. the boot id and the state store;
//  2. the device engine and its recovery, with the first list of the pool
//     devices and the first scan;
//  3. the fence of the NRI plugin seeded with the NUMA nodes of the claims;
//  4. the lease broker loaded, the sockets of its lease claims made again,
//     the CDI specs of the prepared claims and the lease claims written in
//     place, the other CXL specs removed;
//  5. the NRI plugin started and the lenders wired;
//  6. the kubelet plugins of cxl.nri.io and of the pool driver registered,
//     the first publication of the node's ResourceSlices, the watchers, the
//     periodic pass over the stale lease binds and the periodic repair pass
//     of the cpuset.mems of the fenced containers.
func newDriver(ctx context.Context, config *pluginConfig) (*driver, error) {
	cxlFlags := config.Flags
	driverConfig, err := loadDriverConfig(cxlFlags)
	if err != nil {
		return nil, err
	}

	// 1. The prepared claims, the slices of local memdevs and the leases of
	// another boot are not prepared: nothing of what their Prepares made
	// survives a reboot. The records of both drivers are kept in the state
	// store (state.json); the store drops or marks stale what is of another
	// boot before they are read. Without the boot id the driver does not
	// start: it could not tell the records of another boot.
	k := config.Kernel
	bootID, err := k.BootID()
	if err != nil {
		return nil, fmt.Errorf("the boot id of the running kernel: %w", err)
	}
	klog.Infof("boot id %s", bootID)
	st, closeStore, err := openDriverStore(ctx, config, cxlFlags, bootID)
	if err != nil {
		return nil, err
	}
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	started := false
	defer func() {
		if !started {
			cancelRun()
			closeStore()
		}
	}()

	// 2. The NRI plugin with the fenced nodes exists before any Prepare can
	// online memory, and without NRI too. One device engine owns the CXL
	// kernel objects of both drivers; its records are recovered before the
	// first scan, so that the regions of the drivers are never published as
	// region devices. The provenance of the serials is remembered across
	// restarts: a pool device on the node is never a local memdev, also
	// while the pool controller has deleted its ResourceSlices.
	nri, err := containers.New(containers.Options{Defaults: driverConfig.containerDefaults(), Store: st, Leases: cxlFlags.Leases,
		SysRoot: filepath.Dir(k.SysRoot)})
	if err != nil {
		return nil, err
	}
	events, stopEvents := wakeOnUevents(k)
	fatal := make(chan error, 1)
	opts := nodeOptions(config, driverConfig, k, st)
	opts.Fence = fenceOf(nri)
	opts.Shutdown = func(err error) {
		select {
		case fatal <- err:
		default: // the first one stops the driver
		}
	}
	node, err := dra.NewNode(opts)
	if err != nil {
		return nil, err
	}
	if _, err := node.Recover(ctx); err != nil {
		return nil, err
	}

	// 3. Not the nodes of the claims of another boot: they have no memory
	// online, their Prepares again fence the nodes before they online it.
	// A lost store fences every node of online CXL memory.
	seed, err := fenceSeed(ctx, st, k, node.Local.Name(), node.Pool.Name())
	if err != nil {
		return nil, err
	}
	nri.Seed(seed)
	// node.Pool is a nil *dra.Plugin without the pool driver: its readers
	// are nil-receiver safe (no pool results)
	nri.SetResults(node.Local, node.Pool)

	d := &driver{
		local:      node.Local,
		pool:       node.Pool,
		publisher:  node.Publisher,
		watcher:    node.Watcher,
		closeStore: closeStore,
		cancelRun:  cancelRun,
		stop:       make(chan struct{}),
		fatal:      fatal,
		events:     events,
		stopEvents: stopEvents,
	}

	// 4. The lease broker re-creates the sockets of prepared lease claims
	// before kubelet can call Prepare or Unprepare.
	broker, err := newBroker(config, cxlFlags, driverConfig, st, node, k)
	if err != nil {
		return nil, err
	}
	if broker != nil {
		d.stopBroker = broker.Stop
	}
	// The specs of the claims prepared before the start are rewritten, not
	// removed first: a container created meanwhile finds its devices.
	if err := node.WriteCDI(); err != nil {
		return nil, err
	}

	// 5. The NRI plugin starts before the kubelet plugins: kubelet creates
	// the containers of a pod after its claims are prepared, and a container
	// created before the NRI plugin is in the runtime is only found by
	// Synchronize: its CreateContainer is never seen. Its Synchronize may
	// use the prepared claims of both drivers and the lease table, loaded
	// above. A Prepare still waits for the first Synchronize (waitNRI): the
	// stub starts when the runtime has configured it, before it
	// synchronizes.
	d.nri = startNRI(runCtx, nri, config, node, broker != nil)
	lender := wireLenders(d.nri, broker, node)

	// 6. The kubelet plugins, the publisher and the watchers.
	if err := d.startPlugins(runCtx, config, node); err != nil {
		return nil, err
	}
	if broker != nil {
		broker.Start(lender)
	}
	go everyPeriod(staleBindPeriod, d.stop, func() { node.RetryStaleBinds(runCtx) })
	if d.nri != nil {
		go everyPeriod(repairPeriod, d.stop, func() { d.nri.Repair(runCtx) })
	}
	d.startRescans(runCtx)

	klog.V(3).Info("Finished creating new driver")
	started = true
	return d, nil
}

// run runs the driver until SIGTERM, SIGINT or a fatal error of the
// kubelet plugins stops it, and returns the fatal error. A signal during
// the start ends its waits.
func run(ctx context.Context, flags *flags) error {
	klog.Infof("Starting %s version %s (build %s, %s, %s/%s), drivers %s and %q",
		filepath.Base(os.Args[0]), version.Version, version.Build,
		runtime.Version(), runtime.GOOS, runtime.GOARCH,
		device.DriverName, flags.poolDriver())

	kubeClient, err := client.New(client.WithKubeOrInClusterConfig(flags.KubeConfig))
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	leaseClient, err := client.New(client.WithKubeOrInClusterConfig(flags.KubeConfig))
	if err != nil {
		return fmt.Errorf("create lease broker client: %w", err)
	}
	config := &pluginConfig{
		Flags:                     flags,
		Client:                    kubeClient,
		LeaseClient:               leaseClient,
		Kernel:                    newKernel(),
		KubeletPluginDir:          filepath.Join(kubeletPluginsDir, device.DriverName),
		KubeletPluginsRegistryDir: kubeletPluginsRegistryDir,
	}
	if err := os.MkdirAll(config.KubeletPluginDir, 0750); err != nil {
		return err
	}
	if err := os.MkdirAll(flags.CDIRoot, 0o750); err != nil {
		return fmt.Errorf("CDI root: %w", err)
	}
	if flags.LeaseCLISource != "" {
		if err := installLeaseCLI(flags.LeaseCLISource, flags.LeaseCLI); err != nil {
			return err
		}
	}

	// SIGQUIT keeps its goroutine dump: the diagnostic of a hang
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stopSignals()
	driver, err := newDriver(ctx, config)
	if err != nil {
		if ctx.Err() != nil {
			klog.Infof("%v during the start (%v), exiting.", context.Cause(ctx), err)
			return nil
		}
		return err
	}

	var fatal error
	select {
	case <-ctx.Done():
		klog.Infof("%v, exiting.", context.Cause(ctx))
	case fatal = <-driver.fatal:
		klog.Infof("Fatal error of a kubelet plugin, exiting.")
	}
	// a second signal kills the process at once
	stopSignals()
	driver.Shutdown()
	return fatal
}

func main() {
	klog.InitFlags(nil)
	flags, err := parseFlags(flag.CommandLine, os.Args[1:], os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	if err := run(context.Background(), flags); err != nil {
		klog.Errorf("Error: %v", err)
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}
