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

package metrics

import (
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/prometheus/client_golang/prometheus"
	model "github.com/prometheus/client_model/go"
)

var (
	log  = logger.Get("metrics")
	clog = logger.Get("collector")
)

type (
	// State represents the configuration of a collector or a group of collectors.
	State int

	// Collector is a registered prometheus.Collector.
	Collector struct {
		collector prometheus.Collector
		name      string
		group     string
		State
		lastpoll []prometheus.Metric
	}

	// CollectorOption is an option for a Collector.
	CollectorOption func(*Collector)
)

const (
	// Enabled marks a collector as enabled.
	Enabled State = (1 << iota)
	// Polled marks a collector as polled. Polled collectors return cached metrics
	// collected during the last polling cycle. This is useful for computationally
	// expensive metrics that should not be collected during normal collection.
	Polled
	// NamespacePrefix causes a collector's metrics to be prefixed with a common
	// namespace.
	NamespacePrefix
	// SubsystemPrefix causes a collecor's metrics to be prefixed with the name
	// of the group the collector belongs to.
	SubsystemPrefix

	// DefaultName is the name of the default group. An alias for "".
	DefaultName = "default"
)

// WithoutNamespace is an option to disable namespace prefixing for a collector.
func WithoutNamespace() CollectorOption {
	return func(c *Collector) {
		c.State &^= NamespacePrefix
	}
}

// WithoutSubsystem is an option to disable group prefixing for a collector.
func WithoutSubsystem() CollectorOption {
	return func(c *Collector) {
		c.State &^= SubsystemPrefix
	}
}

// WithPolled is an option to mark a collector polled.
func WithPolled() CollectorOption {
	return func(c *Collector) {
		c.State |= Polled
	}
}

// IsEnabled returns true if the collector is enabled.
func (s State) IsEnabled() bool {
	return s&Enabled != 0
}

// IsPolled returns true if the collector is polled.
func (s State) IsPolled() bool {
	return s&Polled != 0
}

// NeedsNamespace returns true if the collector needs a namespace prefix.
func (s State) NeedsNamespace() bool {
	return s&NamespacePrefix != 0
}

// NeedsSubsystem returns true if the collector needs a group prefix.
func (s State) NeedsSubsystem() bool {
	return s&SubsystemPrefix != 0
}

// String returns a string representation of the collector state.
func (s State) String() string {
	var (
		str = ""
		sep = ""
	)

	if s.IsEnabled() {
		str += sep + "enabled"
		sep = ","
	} else {
		str += sep + "disabled"
		sep = ","
	}
	if s.IsPolled() {
		str += sep + "polled"
		sep = ","
	}
	if s.NeedsNamespace() {
		str += sep + "namespace-prefixed"
		sep = ","
	}
	if s.NeedsSubsystem() {
		str += sep + "subsystem-prefixed"
	}

	return str
}

// NewCollector creates a new collector with the given name and collector.
func NewCollector(name string, collector prometheus.Collector, options ...CollectorOption) *Collector {
	c := &Collector{
		name:      name,
		collector: collector,
		State:     Enabled | NamespacePrefix | SubsystemPrefix,
	}

	for _, o := range options {
		o(c)
	}

	return c
}

// Name returns the name of the collector.
func (c *Collector) Name() string {
	return c.group + "/" + c.name
}

// Matches returns true if the collector matches the given glob pattern.
func (c *Collector) Matches(glob string) bool {
	if glob == c.group || glob == c.name || glob == c.Name() {
		return true
	}

	ok, err := path.Match(glob, c.group)
	if err != nil {
		log.Warnf("invalid glob pattern %q (group %s): %v", glob, c.group, err)
	}
	if ok {
		return true
	}

	ok, err = path.Match(glob, c.name)
	if err != nil {
		log.Warnf("invalid glob pattern %q (name %s): %v", glob, c.name, err)
	}
	if ok {
		return true
	}

	ok, err = path.Match(glob, c.Name())
	if ok {
		return true
	}

	if err != nil {
		log.Error("invalid glob pattern %q (name %s): %v", glob, c.Name(), err)
	}

	return false
}

// Describe implements the prometheus.Collector interface.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	c.collector.Describe(ch)
}

// Collect implements the prometheus.Collector interface.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	switch {
	case !c.IsEnabled():
		return

	case !c.IsPolled():
		clog.Debug("collecting %q", c.Name())
		c.collector.Collect(ch)

	default: // c.IsEnabled() && c.IsPolled():
		clog.Debug("collecting (polled) %q", c.Name())
		for _, m := range c.lastpoll {
			ch <- m
		}
	}
}

// Poll collects metrics from the collector if it is polled.
func (c *Collector) Poll() {
	if !c.IsEnabled() || !c.IsPolled() {
		return
	}

	clog.Debug("polling %q", c.Name())

	ch := make(chan prometheus.Metric, 32)
	go func() {
		c.collector.Collect(ch)
		close(ch)
	}()

	polled := make([]prometheus.Metric, 0, 16)
	for m := range ch {
		polled = append(polled, m)
	}

	c.lastpoll = polled[:]
}

// Enable enables or disables the collector.
func (c *Collector) Enable(state bool) {
	if state {
		c.State |= Enabled
	} else {
		c.State &^= Enabled
	}
}

// Polled marks the collector polled or non-polled.
func (c *Collector) Polled(state bool) {
	if state {
		c.State |= Polled
	} else {
		c.State &^= Polled
	}
}

func (c *Collector) state() State {
	return c.State
}

type (
	// Group is a collection of collectors.
	Group struct {
		name       string
		collectors []*Collector
	}
)

func newGroup(name string) *Group {
	return &Group{name: name}
}

// Describe implements the prometheus.Collector interface.
func (g *Group) Describe(ch chan<- *prometheus.Desc) {
	for _, c := range g.collectors {
		c.Describe(ch)
	}
}

// Collect implements the prometheus.Collector interface.
func (g *Group) Collect(ch chan<- prometheus.Metric) {
	clog.Debug("collecting group %s", g.name)
	for _, c := range g.collectors {
		c.Collect(ch)
	}
}

func (g *Group) poll() {
	if !g.state().IsPolled() {
		return
	}

	clog.Debug("polling group %s", g.name)
	wg := sync.WaitGroup{}
	for _, c := range g.collectors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Poll()
		}()
	}
	wg.Wait()
}

func (g *Group) state() State {
	var state State
	for _, c := range g.collectors {
		state |= c.state()
	}
	return state
}

func (g *Group) add(c *Collector) {
	c.group = g.name
	g.collectors = append(g.collectors, c)
	log.Info("registered collector %q", c.Name())
}

func (g *Group) register(plain, ns prometheus.Registerer) error {
	var (
		plainGrp = prefixedRegisterer(g.name, plain)
		nsGrp    = prefixedRegisterer(g.name, ns)
	)

	for _, c := range g.collectors {
		var reg prometheus.Registerer

		if c.NeedsNamespace() {
			if c.NeedsSubsystem() {
				reg = nsGrp
			} else {
				reg = ns
			}
		} else {
			if c.NeedsSubsystem() {
				reg = plainGrp
			} else {
				reg = plain
			}
		}

		if err := reg.Register(c); err != nil {
			return err
		}
	}

	return nil
}

func (g *Group) configure(enabled, polled []string, match map[string]struct{}) State {
	for _, c := range g.collectors {
		c.Enable(false)
	}

	state := State(0)
	for _, c := range g.collectors {
		for _, glob := range enabled {
			if c.Matches(glob) {
				match[glob] = struct{}{}
				c.Enable(true)
				log.Info("collector %q now %s", c.Name(), c.state())
			}
			state |= c.state()
		}
		for _, glob := range polled {
			if c.Matches(glob) {
				match[glob] = struct{}{}
				c.Enable(true)
				// TODO(klihub): Note that this is currently a one-way street.
				// Once we force a collector to be polled we never reset it to
				// be normally collected. So let's give a warning about it...
				if !c.IsPolled() {
					log.Warn("permanently forcing collector %q to be polled", c.Name())
				}
				c.Polled(true)
				log.Info("collector %q now %s", c.Name(), c.state())
			}
			state |= c.state()
		}
	}

	log.Info("group %q now %s", g.name, state)

	return state
}

type (
	// Registry is a collection of groups.
	Registry struct {
		groups map[string]*Group
		state  State
	}

	// RegisterOptions are options for registering collectors.
	RegisterOptions struct {
		group string
		copts []CollectorOption
	}

	// RegisterOption is an option for registering collectors.
	RegisterOption func(*RegisterOptions)
)

// WithGroup is an option to register a collector in a specific group.
func WithGroup(name string) RegisterOption {
	return func(o *RegisterOptions) {
		if name == "" {
			name = DefaultName
		}
		o.group = name
	}
}

// WithCollectorOptions is an option to register a collector with options.
func WithCollectorOptions(opts ...CollectorOption) RegisterOption {
	return func(o *RegisterOptions) {
		o.copts = append(o.copts, opts...)
	}
}

// NewRegistry creates a new registry.
func NewRegistry() *Registry {
	return &Registry{
		groups: make(map[string]*Group),
	}
}

// Register registers a collector with the registry.
func (r *Registry) Register(name string, collector prometheus.Collector, opts ...RegisterOption) error {
	options := &RegisterOptions{group: DefaultName}
	for _, o := range opts {
		o(options)
	}

	grp, ok := r.groups[options.group]
	if !ok {
		grp = newGroup(options.group)
		r.groups[grp.name] = grp
	}

	grp.add(NewCollector(name, collector, options.copts...))
	r.state = 0

	return nil
}

// Configure enables the collectors matching any of the given globs. Any
// collector matching any glob in polled is forced to polled mode.
func (r *Registry) Configure(enabled []string, polled []string) (State, error) {
	log.Info("configuring registry with collectors enabled=[%s], polled=[%s]",
		strings.Join(enabled, ","), strings.Join(polled, ","))

	match := make(map[string]struct{})
	r.state = 0
	for _, g := range r.groups {
		r.state |= g.configure(enabled, polled, match)
	}

	unmatched := []string{}
	for _, glob := range enabled {
		if _, ok := match[glob]; !ok {
			unmatched = append(unmatched, glob)
		}
	}
	for _, glob := range polled {
		if _, ok := match[glob]; !ok {
			unmatched = append(unmatched, glob)
		}
	}

	if len(unmatched) > 0 {
		return r.state, fmt.Errorf("no collectors match globs %s", strings.Join(unmatched, ", "))
	}

	return r.state, nil
}

// Poll all collectors with are enabled and in polled mode.
func (r *Registry) Poll() {
	wg := sync.WaitGroup{}
	for _, g := range r.groups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.poll()
		}()
	}
	wg.Wait()
}

// State returns the collective state of all collectors in the registry.
func (r *Registry) State() State {
	if r.state == 0 {
		for _, g := range r.groups {
			r.state |= g.state()
		}
	}
	return r.state
}

// Getherer returns a gatherer for the registry, configured with the given options.
func (r *Registry) Gatherer(opts ...GathererOption) (*Gatherer, error) {
	return r.NewGatherer(opts...)
}

func prefixedRegisterer(prefix string, reg prometheus.Registerer) prometheus.Registerer {
	if prefix != "" {
		return prometheus.WrapRegistererWithPrefix(prefix+"_", reg)
	}
	return reg
}

type (
	// Gatherer is a prometheus gatherer for our registry.
	Gatherer struct {
		*prometheus.Registry
		r            *Registry
		namespace    string
		ticker       *time.Ticker
		pollInterval time.Duration
		stopCh       chan chan struct{}
		lock         sync.Mutex
		enabled      []string
		polled       []string
	}

	// GathererOption is an option for the gatherer.
	GathererOption func(*Gatherer)
)

const (
	// MinPollInterval is the most frequent allowed polling interval.
	MinPollInterval = 5 * time.Second
	// DefaultPollInterval is the default interval for polling collectors.
	DefaultPollInterval = 30 * time.Second
)

// WithNamespace defines the common namespace prefix for gathered collectors.
func WithNamespace(namespace string) GathererOption {
	return func(g *Gatherer) {
		g.namespace = namespace
	}
}

// WithPollInterval defines the polling interval for the gatherer.
func WithPollInterval(interval time.Duration) GathererOption {
	return func(g *Gatherer) {
		if interval < MinPollInterval {
			g.pollInterval = MinPollInterval
		} else {
			g.pollInterval = interval
		}
	}
}

// WithoutPolling disables internally triggered polling for the gatherer.
func WithoutPolling() GathererOption {
	return func(g *Gatherer) {
		g.pollInterval = 0
	}
}

// WithMetrics defines which groups or collectors will be enabled, and
// and polled if any.
func WithMetrics(enabled, polled []string) GathererOption {
	return func(g *Gatherer) {
		g.enabled = enabled
		g.polled = polled
	}
}

// NewGatherer creates a new gatherer for the registry, with the given options.
func (r *Registry) NewGatherer(opts ...GathererOption) (*Gatherer, error) {
	g := &Gatherer{
		r:            r,
		Registry:     prometheus.NewPedanticRegistry(),
		pollInterval: DefaultPollInterval,
	}

	for _, o := range opts {
		o(g)
	}

	if _, err := r.Configure(g.enabled, g.polled); err != nil {
		return nil, err
	}

	nsg := prefixedRegisterer(g.namespace, g.Registry)

	for _, grp := range r.groups {
		if err := grp.register(g.Registry, nsg); err != nil {
			return nil, err
		}
	}

	g.start()

	return g, nil
}

// Gather implements the prometheus.Gatherer interface.
func (g *Gatherer) Gather() ([]*model.MetricFamily, error) {
	g.Block()
	defer g.Unblock()

	mfs, err := g.Registry.Gather()
	if err != nil {
		return nil, err
	}

	return mfs, nil
}

// Block the gatherer from polling collectors.
func (g *Gatherer) Block() {
	g.lock.Lock()
}

// Allow the gatherer to poll collectors.
func (g *Gatherer) Unblock() {
	g.lock.Unlock()
}

// Poll all enabled collectors in poll mode in the registry.
func (g *Gatherer) Poll() {
	g.Block()
	g.r.Poll()
	g.Unblock()
}

func (g *Gatherer) start() {
	g.Block()
	defer g.Unblock()

	if !g.r.State().IsPolled() {
		log.Info("no polling (no collectors in polled mode)")
		return
	}

	if g.pollInterval == 0 {
		log.Info("no polling (internally triggered polling disabled)")
		return
	}

	log.Info("will do periodic polling (some collectors in polled mode)")

	g.stopCh = make(chan chan struct{})
	g.ticker = time.NewTicker(g.pollInterval)

	g.r.Poll()
	go g.poller()
}

func (g *Gatherer) poller() {
	for {
		select {
		case doneCh := <-g.stopCh:
			g.ticker.Stop()
			g.ticker = nil
			close(doneCh)
			return
		case <-g.ticker.C:
			g.Poll()
		}
	}
}

func (g *Gatherer) Stop() {
	g.Block()
	defer g.Unblock()

	if g.stopCh == nil {
		return
	}

	doneCh := make(chan struct{})
	g.stopCh <- doneCh
	<-doneCh

	g.stopCh = nil
}

var (
	defaultRegistry *Registry
)

// Default returns the default registry.
func Default() *Registry {
	if defaultRegistry == nil {
		defaultRegistry = NewRegistry()
	}
	return defaultRegistry
}

// Register registers a collector with the default registry.
func Register(name string, collector prometheus.Collector, opts ...RegisterOption) error {
	return Default().Register(name, collector, opts...)
}

// MustRegister registers a collector with the default registry, panicking on error.
func MustRegister(name string, collector prometheus.Collector, opts ...RegisterOption) {
	if err := Register(name, collector, opts...); err != nil {
		panic(err)
	}
}

// NewGatherer creates a new gatherer for the default registry, with the given options.
func NewGatherer(opts ...GathererOption) (*Gatherer, error) {
	return Default().Gatherer(opts...)
}
