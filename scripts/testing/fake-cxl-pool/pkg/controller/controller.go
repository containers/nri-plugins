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

// Package controller implements fake-cxl-pool-controller: the cluster side
// of the cxl-pool.nri.io DRA driver for the fake-cxl-pool server. It
// publishes the pool devices as one cluster-scoped ResourceSlice for the
// nodes whose VMs the server reaches, under the name of the server as the
// pool name, attaches a device to the VM of the node the scheduler picked,
// writes the binding condition and the device data into the claim status,
// and detaches the device when the claim is deallocated. Each server has a
// controller of its own. The controller is a stateless,
// level-triggered reconciler: desired attachments come from ResourceClaims,
// actual ones from the server. The contract with the pool driver is
// docs/memory/cxl/pool.md.
package controller

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	resourcelisters "k8s.io/client-go/listers/resource/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/resourceslice"

	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/client"
)

// Defaults.
const (
	DefaultSyncInterval  = 10 * time.Second
	DefaultAttachTimeout = 60 * time.Second
	DefaultDetachTimeout = 60 * time.Second
	DefaultOwnerPrefix   = "k8s:"
	DefaultDebounce      = 500 * time.Millisecond
	// DefaultControllerKey is the key of the controller's settings in
	// pool.opaque of a MemoryConfig.
	DefaultControllerKey = "fake-cxl-pool"
	// Component is the event source component.
	Component = "fake-cxl-pool-controller"
)

// Config configures the controller of the pool driver's devices of one
// server. DRAPoolName "" is the name of the server (GET /status).
type Config struct {
	DRAPoolName   string
	SyncInterval  time.Duration
	AttachTimeout time.Duration
	DetachTimeout time.Duration
	// OwnerPrefix is the prefix of the attachment owners of this
	// controller: "<prefix>resourceclaim/<claim uid>".
	OwnerPrefix string
	// ControllerKey is the key of the controller's settings (PoolSettings)
	// in pool.opaque of a MemoryConfig.
	ControllerKey string
	// Debounce delays a reconcile after an informer or server event.
	Debounce time.Duration
	Verbose  bool
	Logger   *log.Logger
}

func (c *Config) complete() {
	if c.SyncInterval <= 0 {
		c.SyncInterval = DefaultSyncInterval
	}
	if c.AttachTimeout <= 0 {
		c.AttachTimeout = DefaultAttachTimeout
	}
	if c.DetachTimeout <= 0 {
		c.DetachTimeout = DefaultDetachTimeout
	}
	if c.OwnerPrefix == "" {
		// never "": that would claim the attachments of every owner
		c.OwnerPrefix = DefaultOwnerPrefix
	}
	if c.ControllerKey == "" {
		c.ControllerKey = DefaultControllerKey
	}
	if c.Debounce <= 0 {
		c.Debounce = DefaultDebounce
	}
	if c.Logger == nil {
		c.Logger = log.New(os.Stderr, "fake-cxl-pool-controller: ", log.LstdFlags|log.Lmicroseconds)
	}
}

// Controller is the pool controller.
type Controller struct {
	cfg      Config
	kube     kubernetes.Interface
	pool     *client.Client
	log      *log.Logger
	recorder record.EventRecorder

	factory informers.SharedInformerFactory
	claims  resourcelisters.ResourceClaimLister
	nodes   corelisters.NodeLister
	synced  []cache.InformerSynced

	trigger chan struct{}

	// Owned by the goroutine of Run.
	resourceSlices *resourceslice.Controller
	published      *resourceslice.DriverResources // last given to slices
	devices        map[string]api.Device          // published device name -> server device
	nameWarned     map[string]bool
	noLabel        map[string]bool // nodes without a hostname label (warned once)
	hostMapStr     string
	attLogged      map[string]string // attachment id -> last logged state of undesired attachments
	cfgLogged      map[string]string // claim uid/request -> last logged configuration error
}

// New creates a controller. recorder may be nil (no events).
func New(cfg Config, kube kubernetes.Interface, pool *client.Client, recorder record.EventRecorder) *Controller {
	cfg.complete()
	if recorder == nil {
		recorder = &record.FakeRecorder{}
	}
	c := &Controller{
		cfg:        cfg,
		kube:       kube,
		pool:       pool,
		log:        cfg.Logger,
		recorder:   recorder,
		factory:    informers.NewSharedInformerFactory(kube, 0),
		trigger:    make(chan struct{}, 1),
		devices:    map[string]api.Device{},
		nameWarned: map[string]bool{},
		noLabel:    map[string]bool{},
		attLogged:  map[string]string{},
		cfgLogged:  map[string]string{},
	}
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.requestReconcile() },
		UpdateFunc: func(any, any) { c.requestReconcile() },
		DeleteFunc: func(any) { c.requestReconcile() },
	}
	ci := c.factory.Resource().V1().ResourceClaims()
	ni := c.factory.Core().V1().Nodes()
	_, _ = ci.Informer().AddEventHandler(handler)
	_, _ = ni.Informer().AddEventHandler(handler)
	c.claims = ci.Lister()
	c.nodes = ni.Lister()
	c.synced = []cache.InformerSynced{ci.Informer().HasSynced, ni.Informer().HasSynced}
	return c
}

func (c *Controller) logf(format string, args ...any) {
	c.log.Printf(format, args...)
}

func (c *Controller) debugf(format string, args ...any) {
	if c.cfg.Verbose {
		c.log.Printf(format, args...)
	}
}

// requestReconcile asks the run loop for a (debounced) reconcile.
func (c *Controller) requestReconcile() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// start starts the informers and waits for their caches. A reconcile on a
// cache that is not synced would detach everything.
func (c *Controller) start(ctx context.Context) error {
	c.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), c.synced...) {
		return errors.New("informer caches did not sync")
	}
	return nil
}

// resolvePoolName sets the pool name to the name of the server unless one
// is configured. It returns true if the name came from the server.
func (c *Controller) resolvePoolName(ctx context.Context) (bool, error) {
	if c.cfg.DRAPoolName != "" {
		return false, nil
	}
	st, err := c.pool.Status(ctx)
	if err != nil {
		return false, err
	}
	c.cfg.DRAPoolName = st.Name
	return true, nil
}

// Run runs the controller until ctx is done. On return the pool
// ResourceSlices of the controller are deleted.
func (c *Controller) Run(ctx context.Context) error {
	fromServer, err := c.resolvePoolName(ctx)
	for err != nil {
		if ctx.Err() != nil {
			return nil
		}
		c.logf("ERROR: pool name: server status: %v (retrying in %s)", err, c.cfg.SyncInterval)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(c.cfg.SyncInterval):
		}
		fromServer, err = c.resolvePoolName(ctx)
	}
	if fromServer {
		c.logf("driver %s, pool %s (from the server), server %s, sync interval %s, config key %s", device.PoolDriverName, c.cfg.DRAPoolName, c.pool.BaseURL, c.cfg.SyncInterval, c.cfg.ControllerKey)
	} else {
		c.logf("driver %s, pool %s, server %s, sync interval %s, config key %s", device.PoolDriverName, c.cfg.DRAPoolName, c.pool.BaseURL, c.cfg.SyncInterval, c.cfg.ControllerKey)
	}
	if err := c.start(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer c.factory.Shutdown()
	go c.watchServerEvents(ctx)

	ticker := time.NewTicker(c.cfg.SyncInterval)
	defer ticker.Stop()
	c.sync(ctx)
	for {
		select {
		case <-ctx.Done():
			c.shutdown()
			return nil
		case <-c.trigger:
			select {
			case <-ctx.Done():
				continue
			case <-time.After(c.cfg.Debounce):
			}
			select {
			case <-c.trigger:
			default:
			}
			c.sync(ctx)
		case <-ticker.C:
			c.sync(ctx)
		}
	}
}

// sync maps the nodes to the hosts of the server, publishes the pool
// devices for those nodes and reconciles attachments.
func (c *Controller) sync(ctx context.Context) {
	hm, err := c.mapNodes(ctx)
	if err != nil {
		c.logf("ERROR: sync: %v", err)
		return
	}
	if err := c.publish(ctx, hm); err != nil {
		c.logf("ERROR: publish: %v", err)
		return
	}
	if err := c.reconcile(ctx, hm); err != nil {
		c.logf("ERROR: reconcile: %v", err)
	}
}

// watchServerEvents triggers a reconcile on every server event. The
// stream is reopened with backoff; the sync ticker keeps polling meanwhile.
func (c *Controller) watchServerEvents(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.pool.Events(ctx, func(ev client.Event) error {
			c.debugf("server event %s", ev.Type)
			c.requestReconcile()
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		c.logf("server event stream: %v (polling every %s, reconnecting in %s)", err, c.cfg.SyncInterval, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
		c.requestReconcile()
	}
}

// shutdown stops publishing and deletes the pool slices: they have no
// owner, nothing else garbage collects them.
func (c *Controller) shutdown() {
	if c.resourceSlices != nil {
		c.resourceSlices.Stop()
		c.resourceSlices = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.deleteResourceSlices(ctx); err != nil {
		c.logf("ERROR: delete ResourceSlices: %v", err)
	}
}
