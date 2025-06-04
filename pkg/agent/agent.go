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

package agent

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/containers/nri-plugins/pkg/kubernetes/client"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	nrtapi "github.com/containers/nri-plugins/pkg/agent/nrtapi"
	"github.com/containers/nri-plugins/pkg/agent/podresapi"
	"github.com/containers/nri-plugins/pkg/agent/watch"
	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"

	logger "github.com/containers/nri-plugins/pkg/log"
)

// Option is an option for the agent.
type Option func(*Agent) error

// WithKubeConfig sets the location of the config file to use for K8s cluster access.
// An unset config file implies in-cluster configuration or lack of need for cluster
// access.
func WithKubeConfig(kc string) Option {
	return func(a *Agent) error {
		a.kubeConfig = kc
		return nil
	}
}

// WithConfigNamespace sets the namespace used for config custom resources.
func WithConfigNamespace(ns string) Option {
	return func(a *Agent) error {
		a.namespace = ns
		return nil
	}
}

// WithConfigFile sets up the agent to monitor a configuration file instead of custom
// resources.
func WithConfigFile(file string) Option {
	return func(a *Agent) error {
		a.configFile = file
		return nil
	}
}

// WithConfigGroupLabel sets the key used to label nodes into config groups.
func WithConfigGroupLabel(label string) Option {
	return func(a *Agent) error {
		a.groupLabel = label
		return nil
	}
}

// ConfigInterface is used by the agent to access config custom resources.
type ConfigInterface interface {
	// Set the preferred client and configuration for cluster/apiserver access.
	SetKubeClient(cli *http.Client, cfg *rest.Config) error
	// Create a watch for monitoring the named custom resource.
	CreateWatch(ctx context.Context, namespace, name string) (watch.Interface, error)
	// Patch the status subresource of the named custom resource.
	PatchStatus(ctx context.Context, namespace, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) error
	// Unmarshal custom resource which was read from the given file.
	Unmarshal(data []byte, file string) (runtime.Object, error)
}

// BalloonsConfigInterface returns a ConfigInterface for the balloons policy.
func BalloonsConfigInterface() ConfigInterface {
	return newConfigIf(balloonsConfig)
}

// TopologyAwareConfigInterface returns a ConfigInterface for the topology-aware policy.
func TopologyAwareConfigInterface() ConfigInterface {
	return newConfigIf(topologyAwareConfig)
}

// TemplateConfigInterface returns a ConfigInterface for the template policy.
func TemplateConfigInterface() ConfigInterface {
	return newConfigIf(templateConfig)
}

// NotifyFn is a function to call when the effective configuration changes.
type NotifyFn func(cfg interface{}) (bool, error)

var (
	// Our logger instance for the agent.
	log = logger.Get("agent")
)

// Agent provides access to configuration stored as custom resources.
//
// Configuration custom resources can be defined per node, per group or as a
// default resource. These are named 'node.$NODE_NAME', 'group.$GROUP_NAME',
// and 'default' respectively. If a node-specific configuration exists, it is
// always used for the node. Otherwise either a group-specific or the default
// configuration is used depending on whether the node belongs to a group. A
// node can be assigned to a group by setting the group label on the node. By
// default this group label is 'config.nri/group'.
type Agent struct {
	nodeName   string // kubernetes node name, defaults to $NODE_NAME
	namespace  string // config resource namespace
	groupLabel string // config resource node grouping label key
	kubeConfig string // kubeconfig path
	configFile string // configuration file to use instead of custom resource

	cfgIf     ConfigInterface   // custom resource access interface
	k8sCli    *client.Client    // kubernetes client
	nrtCli    *nrtapi.Client    // NRT custom resources client
	nrtLock   sync.Mutex        // serialize NRT custom resource updates
	podResCli *podresapi.Client // pod resources API client

	notifyFn      NotifyFn        // config resource change notification callback
	nodeWatch     watch.Interface // kubernetes node watch
	group         string          // current config group
	nodeCfgWatch  watch.Interface // per-node config watch
	nodeCfg       metav1.Object   // node specific config resource
	groupCfgWatch watch.Interface // group-specific/default config watch
	groupCfg      metav1.Object   // group-specific/default config resource
	currentCfg    metav1.Object

	stopLock sync.Mutex
	stopC    chan struct{}
	doneC    chan struct{}
}

// New creates an agent with the given options.
func New(cfgIf ConfigInterface, options ...Option) (*Agent, error) {
	if cfgIf == nil {
		return nil, fmt.Errorf("failed to create agent: nil config interface")
	}

	a := &Agent{
		nodeName:   os.Getenv("NODE_NAME"),
		kubeConfig: defaultKubeConfig,
		configFile: defaultConfigFile,
		namespace:  defaultNamespace,
		groupLabel: defaultGroupLabel,
		cfgIf:      cfgIf,
		stopC:      make(chan struct{}),
	}

	for _, o := range options {
		if err := o(a); err != nil {
			return nil, fmt.Errorf("failed to create agent: %w", err)
		}
	}

	if a.nodeName == "" && a.configFile == "" {
		return nil, fmt.Errorf("failed to create agent: neither node name nor config file set")
	}

	return a, nil
}

func (a *Agent) Start(notifyFn NotifyFn) error {
	a.notifyFn = notifyFn

	err := a.setupClients()
	if err != nil {
		return err
	}

	if err = a.setupNodeWatch(); err != nil {
		return err
	}

	if err = a.setupNodeConfigWatch(); err != nil {
		a.cleanupWatches()
		return err
	}

	eventChanOf := func(w watch.Interface) <-chan watch.Event {
		if w == nil {
			return nil
		}
		return w.ResultChan()
	}

	for {
		select {
		case <-a.stopC:
			a.cleanupWatches()
			return nil

		case e, ok := <-eventChanOf(a.nodeWatch):
			if !ok {
				break
			}
			if e.Type == watch.Added || e.Type == watch.Modified {
				group := e.Object.(*corev1.Node).Labels[a.groupLabel]
				if group == "" {
					for _, l := range deprecatedGroupLabels {
						group = e.Object.(*corev1.Node).Labels[l]
						if group != "" {
							log.Warnf("Using DEPRECATED config group label %q", l)
							log.Warnf("Please switch to using label %q instead", a.groupLabel)
							break
						}
					}
				}
				if err = a.setupGroupConfigWatch(group); err != nil {
					log.Errorf("%v", err)
				}
			}

		case e, ok := <-eventChanOf(a.nodeCfgWatch):
			if !ok {
				break
			}
			switch e.Type {
			case watch.Added, watch.Modified:
				a.updateNodeConfig(e.Object)
			case watch.Deleted:
				a.updateNodeConfig(nil)
			}

		case e, ok := <-eventChanOf(a.groupCfgWatch):
			if !ok {
				break
			}
			switch e.Type {
			case watch.Added, watch.Modified:
				a.updateGroupConfig(e.Object)
			case watch.Deleted:
				a.updateGroupConfig(nil)
			}
		}
	}
}

func (a *Agent) Stop() {
	a.stopLock.Lock()
	defer a.stopLock.Unlock()

	if a.stopC != nil {
		close(a.stopC)
		<-a.doneC
		a.stopC = nil
	}
}

func (a *Agent) NodeName() string {
	return a.nodeName
}

func (a *Agent) KubeClient() *client.Client {
	return a.k8sCli
}

func (a *Agent) KubeConfig() string {
	return a.kubeConfig
}

var (
	defaultConfig = &cfgapi.AgentConfig{
		NodeResourceTopology: true,
	}
)

func getAgentConfig(newConfig metav1.Object) *cfgapi.AgentConfig {
	cfg := cfgapi.GetAgentConfig(newConfig)
	if cfg == nil {
		return defaultConfig
	}
	return cfg
}

func (a *Agent) configure(newConfig metav1.Object) {
	if a.hasLocalConfig() {
		log.Warn("running with local configuration, skipping client setup...")
		return
	}

	cfg := getAgentConfig(newConfig)

	// Failure to create a client is not a fatal error.
	switch {
	case cfg.NodeResourceTopology && a.nrtCli == nil:
		log.Info("enabling NRT client")
		cfg, err := a.getRESTConfig()
		if err != nil {
			log.Error("failed to setup NRT client: %w", err)
			break
		}

		cli, err := nrtapi.NewForConfigAndClient(cfg, a.k8sCli.HttpClient())
		if err != nil {
			log.Error("failed to setup NRT client: %w", err)
			break
		}
		a.nrtCli = cli

	case !cfg.NodeResourceTopology && a.nrtCli != nil:
		log.Info("disabling NRT client")
		a.nrtCli = nil
	}

	// Reconfigure pod resource client, both on initial startup and reconfiguration.
	// Failure to create a client is not a fatal error.
	switch {
	case cfg.PodResourceAPI && a.podResCli == nil:
		log.Info("enabling PodResourceAPI client")
		cli, err := podresapi.NewClient()
		if err != nil {
			log.Error("failed to setup PodResourceAPI client: %v", err)
			break
		}
		a.podResCli = cli

	case !cfg.PodResourceAPI && a.podResCli != nil:
		log.Info("disabling PodResourceAPI client")
		a.podResCli.Close()
		a.podResCli = nil
	}
}

func (a *Agent) hasLocalConfig() bool {
	return a.configFile != ""
}

func (a *Agent) setupClients() error {
	var err error

	if a.hasLocalConfig() {
		log.Warn("running with local configuration, skipping cluster access client setup...")
		return nil
	}

	a.k8sCli, err = client.New(client.WithKubeOrInClusterConfig(a.kubeConfig))
	if err != nil {
		return err
	}

	a.nrtCli, err = nrtapi.NewForConfigAndClient(a.k8sCli.RestConfig(), a.k8sCli.HttpClient())
	if err != nil {
		a.cleanupClients()
		return fmt.Errorf("failed to setup NRT client: %w", err)
	}

	err = a.cfgIf.SetKubeClient(a.k8sCli.HttpClient(), a.k8sCli.RestConfig())
	if err != nil {
		a.cleanupClients()
		return fmt.Errorf("failed to setup kubernetes config resource client: %w", err)
	}

	a.configure(a.currentCfg)

	return nil
}

func (a *Agent) cleanupClients() {
	if a.k8sCli != nil {
		a.k8sCli.Close()
	}
	a.k8sCli = nil
	a.nrtCli = nil
}

func (a *Agent) getRESTConfig() (*rest.Config, error) {
	var (
		cfg *rest.Config
		err error
	)

	if a.kubeConfig == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", a.kubeConfig)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get kubernetes REST client config: %w", err)
	}

	return cfg, err
}

func (a *Agent) setupNodeWatch() error {
	if a.hasLocalConfig() {
		return nil
	}

	if a.nodeWatch != nil {
		a.nodeWatch.Stop()
		a.nodeWatch = nil
	}

	w, err := watch.Object(context.Background(), "", a.nodeName,
		func(ctx context.Context, _, name string) (watch.Interface, error) {
			selector := metav1.ListOptions{
				FieldSelector: "metadata.name=" + name,
			}
			return a.k8sCli.CoreV1().Nodes().Watch(context.Background(), selector)
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create node watch for %s: %w", a.nodeName, err)
	}

	a.nodeWatch = w

	return nil
}

func (a *Agent) setupNodeConfigWatch() error {
	if a.nodeCfgWatch != nil {
		a.nodeCfgWatch.Stop()
		a.nodeCfgWatch = nil
	}

	if a.hasLocalConfig() {
		w, err := watch.File(a.configFile, a.cfgIf.Unmarshal)
		if err != nil {
			return fmt.Errorf("failed to create config file watch for %s: %w", a.configFile, err)
		}
		a.nodeCfgWatch = w
		return nil
	}

	w, err := watch.Object(context.Background(), a.namespace, a.nodeConfigName(),
		func(ctx context.Context, ns, name string) (watch.Interface, error) {
			return a.cfgIf.CreateWatch(ctx, ns, name)
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create node-specific config watch for %s/%s: %w",
			a.namespace, a.nodeConfigName(), err)
	}

	a.nodeCfgWatch = w

	return nil
}

func (a *Agent) setupGroupConfigWatch(group string) error {
	if a.hasLocalConfig() {
		return nil
	}

	if group == a.group && a.groupCfgWatch != nil {
		return nil
	}

	if a.groupCfgWatch != nil {
		a.groupCfgWatch.Stop()
		a.groupCfgWatch = nil
	}

	if group != "" {
		log.Infof("node assigned to config group '%s'", group)
	} else {
		log.Infof("node removed from config group '%s'", group)
	}
	a.group = group

	w, err := watch.Object(context.Background(), a.namespace, a.groupConfigName(),
		func(ctx context.Context, ns, name string) (watch.Interface, error) {
			return a.cfgIf.CreateWatch(ctx, ns, name)
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create group-specific config watch for %s/%s: %w",
			a.namespace, a.groupConfigName(), err)
	}

	a.groupCfgWatch = w

	return nil
}

func (a *Agent) cleanupWatches() {
	if a.nodeWatch != nil {
		a.nodeWatch.Stop()
		a.nodeWatch = nil
	}
	if a.nodeCfgWatch != nil {
		a.nodeCfgWatch.Stop()
		a.nodeCfgWatch = nil
	}
	if a.groupCfgWatch != nil {
		a.groupCfgWatch.Stop()
		a.groupCfgWatch = nil
	}
}

func (a *Agent) nodeConfigName() string {
	return "node." + a.nodeName
}

func (a *Agent) groupConfigName() string {
	if a.group != "" {
		return "group." + a.group
	}
	return "default"
}

func (a *Agent) updateNodeConfig(obj runtime.Object) {
	var cfg metav1.Object

	if obj != nil {
		o, ok := obj.(metav1.Object)
		if !ok {
			log.Error("can't handle object %T, not meta/v1.Object, ignoring it", obj)
			return
		}
		cfg = o
	}

	if sameConfigVersion(cfg, a.nodeCfg) {
		log.Debug("ignoring duplicate node-specific config update")
		return
	}

	if cfg == nil {
		log.Info("node-specific config deleted")
	} else {
		log.Info("node-specific config updated")
	}

	a.nodeCfg = cfg

	//
	// switch to new configuration and notify about it
	//
	// If node-specific configuration was added or updated switch to it.
	// If it was removed switch to group-specific one if we have it.
	//

	if cfg == nil {
		cfg = a.groupCfg
	}

	a.updateConfig(cfg)
}

func (a *Agent) updateGroupConfig(obj runtime.Object) {
	var cfg metav1.Object

	if obj != nil {
		o, ok := obj.(metav1.Object)
		if !ok {
			log.Error("can't handle object %T, not meta/v1.Object, ignoring it", obj)
			return
		}
		cfg = o
	}

	if sameConfigVersion(cfg, a.groupCfg) {
		log.Debug("ignoring duplicate group-specific config update")
		return
	}

	if cfg == nil {
		log.Info("group-specific config deleted")
	} else {
		log.Info("group-specific config updated")
	}

	//
	// update group-specific configuration
	//
	// If we don't have node-specific configuration but we have group-
	// specific one, switch to it and notify about it.
	//

	a.groupCfg = cfg

	if a.nodeCfg != nil {
		return
	}

	a.updateConfig(cfg)
}

func (a *Agent) updateConfig(cfg metav1.Object) {
	if cfg == nil {
		log.Warnf("node (%s) has no effective configuration", a.nodeName)
		return
	}

	if v, ok := cfg.(cfgapi.Validator); ok {
		if err := v.Validate(); err != nil {
			log.Errorf("failed to validate configuration: %v", err)

			a.patchConfigStatus(a.currentCfg, cfg, err)
			a.currentCfg = cfg
			return
		}
	}

	fatal, err := a.notifyFn(cfg)
	a.patchConfigStatus(a.currentCfg, cfg, err)

	if err != nil {
		if fatal {
			log.Fatalf("failed to apply configuration: %v", err)
		} else {
			log.Errorf("failed to apply configuration: %v", err)
		}
	}

	a.currentCfg = cfg
	a.configure(cfg)
}

func (a *Agent) patchConfigStatus(prev, curr metav1.Object, errors error) {
	if a.cfgIf == nil {
		return
	}

	prevName := ""
	if prev != nil {
		prevName = prev.GetName()
	}

	currName := ""
	if curr != nil {
		currName = curr.GetName()
	}

	ctx := context.TODO()
	ns := a.namespace
	node := a.nodeName

	if prev != nil && prevName != currName {
		data, pt, err := cfgapi.NodeStatusPatch(node, nil)
		if err == nil {
			err = a.cfgIf.PatchStatus(ctx, ns, prevName, pt, data, metav1.PatchOptions{})
		}
		if err != nil {
			log.Errorf("failed to patch status of previous config %s/%s: %v", ns, prevName, err)
		}
	}

	if curr != nil {
		status := cfgapi.NewNodeStatus(errors, curr.GetGeneration())
		data, pt, err := cfgapi.NodeStatusPatch(node, status)
		if err == nil {
			err = a.cfgIf.PatchStatus(ctx, ns, currName, pt, data, metav1.PatchOptions{})
		}
		if err != nil {
			log.Errorf("failed to patch status of current config %s/%s: %v", ns, currName, err)
		}
	}
}

func sameConfigVersion(cfg1, cfg2 metav1.Object) bool {
	switch {
	case cfg1 == nil && cfg2 == nil:
		return true
	case cfg1 == nil && cfg2 != nil:
		return false
	case cfg1 != nil && cfg2 == nil:
		return false
	}

	switch {
	case cfg1.GetUID() != cfg2.GetUID():
		return false
	case cfg1.GetGeneration() != cfg2.GetGeneration():
		return false
	case cfg1.GetGeneration() == 0: // config from a file
		return false
	}

	return true
}

var (
	defaultNamespace  string
	defaultGroupLabel string
	defaultKubeConfig string
	defaultConfigFile string

	deprecatedGroupLabels = []string{
		"group.config.nri",
		"resource-policy.nri.io/group",
	}
)

func init() {
	groupLabel := cfgapi.SchemeGroupVersion.Group + "/group"

	flag.StringVar(&defaultNamespace, "config-namespace", "kube-system",
		"namespace for configuration CustomResources")
	flag.StringVar(&defaultGroupLabel, "config-group-label", groupLabel,
		"name of the label used to assign the node to a configuration group")
	flag.StringVar(&defaultConfigFile, "config-file", "",
		"config file to use/monitor instead of a CustomResource")
	flag.StringVar(&defaultKubeConfig, "kubeconfig", "",
		"kubeconfig file to use, empty for in-cluster configuration")
}
