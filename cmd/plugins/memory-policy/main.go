// Copyright 2025 Inter Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//  http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/sirupsen/logrus"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"

	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

type plugin struct {
	stub   stub.Stub
	config *Config
}

type Config struct {
	InjectMpolset bool                 `json:"injectMpolset,omitempty"`
	Classes       []*MemoryPolicyClass `json:"classes,omitempty"`
}

type MemoryPolicyClass struct {
	Name   string        `json:"name"`
	Policy *MemoryPolicy `json:"policy"`
}

type MemoryPolicy struct {
	Mode  string   `json:"mode"`
	Nodes string   `json:"nodes"`
	Flags []string `json:"flags,omitempty"`
}

const (
	annotationSuffix = ".memory-policy.nri.io"
	mpolsetInjectDir = "/mnt/nri-memory-policy-mpolset"
)

var (
	sys system.System
	log *logrus.Logger

	verbose     bool
	veryVerbose bool
)

func (mpol *MemoryPolicy) String() string {
	if mpol == nil {
		return "nil"
	}
	modeFlags := strings.Join(append([]string{mpol.Mode}, mpol.Flags...), "|")
	return fmt.Sprintf("%s:%s", modeFlags, mpol.Nodes)
}

// onClose handles losing connection to container runtime.
func (p *plugin) onClose() {
	log.Infof("Connection to the runtime lost, exiting...")
	os.Exit(0)
}

// Configure handles connecting to container runtime's NRI server.
func (p *plugin) Configure(ctx context.Context, config, runtime, version string) (stub.EventMask, error) {
	log.Infof("Connected to %s %s...", runtime, version)
	if config != "" {
		log.Debugf("loading configuration from NRI server")
		if err := p.setConfig([]byte(config)); err != nil {
			return 0, err
		}
		return 0, nil
	}
	// If we are to use mpolset injection, prepare /mnt/nri-memory-policy-mpolset
	// to contain mpolset so that it can be injected into containers
	if p.config != nil && p.config.InjectMpolset {
		if err := prepareMpolset(); err != nil {
			log.Errorf("failed to prepare mpolset: %v", err)
			return 0, fmt.Errorf("configuration option injectMpolset preparation failed: %v", err)
		}
	}
	return 0, nil
}

// prepareMpolset prepares mpolset for injection into containers.
func prepareMpolset() error {
	// copy mpolset to /mnt/nri-memory-policy-mpolset
	if err := os.MkdirAll(mpolsetInjectDir, 0755); err != nil {
		log.Debugf("failed to create %q: %v", mpolsetInjectDir, err)
	}
	// mpolset is expected to be located in the same directory as this plugin
	mpolsetTarget := filepath.Join(mpolsetInjectDir, "mpolset")
	// read the directory of this plugin and replace plugin's name (for example nri-memory-policy) with mpolset
	// to get the path to mpolset
	pluginPath, err := os.Executable()
	if err != nil {
		log.Debugf("failed to get plugin path: %v", err)
	}
	pluginDir := filepath.Dir(pluginPath)
	mpolsetSource := filepath.Join(pluginDir, "mpolset")
	// check that mpolset exists
	if _, err := os.Stat(mpolsetSource); os.IsNotExist(err) {
		log.Errorf("mpolset not found in %q: %v", pluginDir, err)
		return fmt.Errorf("configuration injectMpolset requires mpolset, but it was not found in %q: %v", pluginDir, err)
	}
	// copy mpolset to /mnt/nri-memory-policy-mpolset which is located on the host
	mpolsetData, err := os.ReadFile(mpolsetSource)
	if err != nil {
		return fmt.Errorf("failed to read mpolset contents from %q: %v", mpolsetSource, err)
	}
	if err := os.WriteFile(mpolsetTarget, mpolsetData, 0755); err != nil {
		return fmt.Errorf("failed to %q mpolset: %v", mpolsetTarget, err)
	}
	return nil
}

// setConfig applies new plugin configuration.
func (p *plugin) setConfig(config []byte) error {
	cfg := &Config{}
	if err := yaml.Unmarshal(config, cfg); err != nil {
		return fmt.Errorf("failed to unmarshal configuration: %w", err)
	}
	for _, class := range cfg.Classes {
		if class.Name == "" {
			return fmt.Errorf("name missing in class definition")
		}
		if class.Policy == nil {
			return fmt.Errorf("class %q has no policy", class.Name)
		}
		if class.Policy.Mode == "" {
			return fmt.Errorf("class %q has no mode", class.Name)
		}
	}
	p.config = cfg
	log.Debugf("plugin configuration: %+v", p.config)
	return nil
}

// pprintCtr() returns unique human readable container name.
func pprintCtr(pod *api.PodSandbox, ctr *api.Container) string {
	return fmt.Sprintf("%s/%s:%s", pod.GetNamespace(), pod.GetName(), ctr.GetName())
}

// effectiveAnnotations returns map of annotation key prefixes and
// values that are effective for a container. It checks for
// container-specific annotations first, and if not found, it
// returns pod-level annotations. "policy" and "class" annotations
// are mutually exclusive.
//
// Example annotations:
//
// class.memory-policy.nri.io: default-class-for-containers-in-pod
//
// class.memory-policy.nri.io/container.my-special-container: special-class
//
// policy.memory-policy.nri.io/container.my-special-container2: |+
//
//	mode: MPOL_INTERLEAVE
//	nodes: max-dist:19
//	flags: [MPOL_F_STATIC_NODES]
func effectiveAnnotations(pod *api.PodSandbox, ctr *api.Container) map[string]string {
	effAnn := map[string]string{}
	for key, value := range pod.GetAnnotations() {
		annPrefix, hasSuffix := strings.CutSuffix(key, annotationSuffix+"/container."+ctr.Name)
		if hasSuffix {
			// Override possibly already found pod-level annotation.
			log.Tracef("- found container-specific annotation %q", key)
			if annPrefix == "class" || annPrefix == "policy" {
				delete(effAnn, "class")
				delete(effAnn, "policy")
			}
			effAnn[annPrefix] = value
			continue
		}
		annPrefix, hasSuffix = strings.CutSuffix(key, annotationSuffix)
		if hasSuffix {
			if annPrefix == "class" || annPrefix == "policy" {
				_, hasClass := effAnn["class"]
				_, hasPolicy := effAnn["policy"]
				if hasClass || hasPolicy {
					log.Tracef("- ignoring pod-level annotation %q due to a container-specific annotation", key)
					continue
				}
			}
			log.Tracef("- found pod-level annotation %q", key)
			effAnn[annPrefix] = value
			continue
		}
	}
	return effAnn
}

// takePolicyAnnotation() takes the policy annotation from the
// annotations map. It returns the policy and removes the
// annotation from the map.
func takePolicyAnnotation(ann map[string]string) (*MemoryPolicy, error) {
	if value, ok := ann["policy"]; ok {
		delete(ann, "policy")
		if value == "" {
			return nil, nil
		}
		policy := &MemoryPolicy{}
		if err := yaml.Unmarshal([]byte(value), policy); err != nil {
			return nil, fmt.Errorf("failed to unmarshal policy: %w", err)
		}
		return policy, nil
	}
	return nil, nil
}

// takeClassAnnotation() takes the class annotation from the
// annotations map. It returns the class and removes the
// annotation from the map.
func (p *plugin) takeClassAnnotation(ann map[string]string) (*MemoryPolicyClass, error) {
	if value, ok := ann["class"]; ok {
		delete(ann, "class")
		if value == "" {
			return nil, nil
		}
		for _, class := range p.config.Classes {
			if class.Name == value {
				return class, nil
			}
		}
		return nil, fmt.Errorf("class %q not found in configuration", value)
	}
	return nil, nil
}

// getPolicy() returns the memory policy for a container.
func (p *plugin) getPolicy(pod *api.PodSandbox, ctr *api.Container) (*MemoryPolicy, error) {
	effAnn := effectiveAnnotations(pod, ctr)

	policy, err := takePolicyAnnotation(effAnn)
	if err != nil {
		return nil, fmt.Errorf("invalid 'policy' annotation: %w", err)
	}
	if policy != nil {
		log.Tracef("- effective policy annotation: %+v", policy)
		return policy, nil
	}

	class, err := p.takeClassAnnotation(effAnn)
	if err != nil {
		return nil, fmt.Errorf("invalid 'class' annotation: %w", err)
	}
	if class != nil {
		log.Tracef("effective class annotation: %+v", class)
		if class.Policy == nil {
			return nil, fmt.Errorf("class %q has no policy", class.Name)
		}
		return class.Policy, nil
	}

	// Check for unknown annotations.
	for ann := range effAnn {
		return nil, fmt.Errorf("unknown annotation %s%s", ann, annotationSuffix)
	}

	log.Tracef("- no memory policy found in annotations")
	return nil, nil
}

// applyPolicy() applies the memory policy to the container. It
// returns the container adjustment that should be applied to the
// container.
func applyPolicy(ctr *api.Container, policy *MemoryPolicy) (*api.ContainerAdjustment, error) {
	var err error
	if policy == nil {
		return nil, nil
	}
	mode, ok := api.MpolMode_value[policy.Mode]
	if !ok {
		return nil, fmt.Errorf("invalid memory policy mode %q", policy.Mode)
	}

	nodeMask := libmem.NewNodeMask()
	ctrCpuset := sys.OnlineCPUs()
	if ctrCpus := ctr.GetLinux().GetResources().GetCpu().GetCpus(); ctrCpus != "" {
		ctrCpuset, err = cpuset.Parse(ctrCpus)
		if err != nil {
			return nil, fmt.Errorf("failed to parse allowed CPUs %q: %v", ctrCpus, err)
		}
	}
	allowedMemsMask := libmem.NewNodeMask(sys.NodeIDs()...)
	ctrMems := ctr.GetLinux().GetResources().GetCpu().GetMems()
	if ctrMems != "" {
		if parsedMask, err := libmem.ParseNodeMask(ctrMems); err == nil {
			allowedMemsMask = parsedMask
		} else {
			return nil, fmt.Errorf("failed to parse allowed mems %q: %v", ctrMems, err)
		}
	}
	log.Tracef("- allowed mems: %s, cpus %s", ctrMems, ctrCpuset)

	switch {
	// "all" includes all nodes into the mask.
	case policy.Nodes == "all":
		nodeMask = libmem.NewNodeMask(sys.NodeIDs()...)
		log.Tracef("- nodes %q (all)", nodeMask.MemsetString())

	// "allowed-mems" includes only allowed memory nodes into the mask.
	case policy.Nodes == "allowed-mems":
		nodeMask = allowedMemsMask
		log.Tracef("- nodes: %q (allowed-mems)", nodeMask.MemsetString())

	// "cpu-packages" includes all nodes that are in the same package
	// as the CPUs in the container's cpuset.
	case policy.Nodes == "cpu-packages":
		pkgs := sys.IDSetForCPUs(ctrCpuset, func(cpu system.CPU) idset.ID {
			return cpu.PackageID()
		})
		nodeMask = libmem.NewNodeMask()
		for _, nodeId := range sys.NodeIDs() {
			nodePkgId := sys.Node(nodeId).PackageID()
			if pkgs.Has(nodePkgId) {
				nodeMask = nodeMask.Set(nodeId)
			}
		}
		log.Tracef("- nodes: %q (cpu-packages %q)", nodeMask.MemsetString(), pkgs)

	// "cpu-nodes" includes all nodes in the cpuset of the container.
	case policy.Nodes == "cpu-nodes":
		nodeIds := sys.IDSetForCPUs(ctrCpuset, func(cpu system.CPU) idset.ID {
			return cpu.NodeID()
		})
		nodeMask = libmem.NewNodeMask(nodeIds.Members()...)
		log.Tracef("- nodes: %q (cpu-nodes)", nodeMask.MemsetString())

	// "max-dist:<int>" includes all nodes that are within the
	// specified distance from the CPUs in the container's cpuset.
	case strings.HasPrefix(policy.Nodes, "max-dist:"):
		maxDist := policy.Nodes[len("max-dist:"):]
		maxDistInt, err := strconv.Atoi(maxDist)
		if err != nil {
			return nil, fmt.Errorf("failed to parse max-dist %q: %v", maxDist, err)
		}
		nodeMask = libmem.NewNodeMask()
		fromNodes := sys.IDSetForCPUs(ctrCpuset, func(cpu system.CPU) idset.ID {
			return cpu.NodeID()
		})
		for _, fromNode := range fromNodes.Members() {
			for _, toNode := range sys.NodeIDs() {
				if sys.NodeDistance(fromNode, toNode) <= maxDistInt {
					nodeMask = nodeMask.Set(toNode)
				}
			}
		}
		log.Tracef("- nodes %q (max-dist %d from CPU nodes %q)", nodeMask.MemsetString(), maxDistInt, fromNodes)

	// <int>[-<int>][, ...] includes the set of nodes.
	case policy.Nodes[0] >= '0' && policy.Nodes[0] <= '9':
		nodeMask, err = libmem.ParseNodeMask(policy.Nodes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse nodes %q: %v", policy.Nodes, err)
		}
		log.Tracef("- nodes %q (hardcoded)", nodeMask.MemsetString())

	default:
		return nil, fmt.Errorf("invalid nodes: %q", policy.Nodes)
	}

	flags := []api.MpolFlag{}
	if len(policy.Flags) > 0 {
		for _, flag := range policy.Flags {
			flag = strings.TrimSpace(flag)
			if flagValue, ok := api.MpolFlag_value[flag]; ok {
				flags = append(flags, api.MpolFlag(flagValue))
			} else {
				return nil, fmt.Errorf("invalid memory policy flag %q", flag)
			}
		}
	}

	nodes := nodeMask.MemsetString()
	if (nodeMask & allowedMemsMask) != nodeMask {
		log.Debugf("some memory policy nodes (%s) are not allowed (%s)", nodes, allowedMemsMask.MemsetString())
	}

	ca := &api.ContainerAdjustment{}
	ca.SetLinuxMemoryPolicy(api.MpolMode(mode), nodes, flags...)
	return ca, nil
}

// toCommandInjection() converts the memory policy container
// adjustment into a command injection. It removes the memory policy
// from the container adjustment and adds a bind mount to the
// mpolsetInjectDir. It also sets the command line arguments to
// run mpolset with the memory policy options before executing the
// original command.
func toCommandInjection(ctr *api.Container, ca *api.ContainerAdjustment) error {
	if ca == nil || ca.Linux == nil || ca.Linux.MemoryPolicy == nil {
		return nil
	}
	mpol := ca.Linux.MemoryPolicy
	ca.Linux.MemoryPolicy = nil
	ca.AddMount(&api.Mount{
		Source:      mpolsetInjectDir,
		Destination: mpolsetInjectDir,
		Type:        "bind",
		Options:     []string{"bind", "ro", "rslave"},
	})
	flags := []string{}
	for _, flag := range mpol.Flags {
		if flagName, ok := api.MpolFlag_name[int32(flag)]; ok {
			flags = append(flags, flagName)
		} else {
			return fmt.Errorf("invalid memory policy flag %q", flag)
		}
	}

	mpolsetArgs := []string{
		filepath.Join(mpolsetInjectDir, "mpolset"),
		"--mode", api.MpolMode_name[int32(mpol.Mode)],
		"--nodes", mpol.Nodes,
	}
	if len(flags) > 0 {
		mpolsetArgs = append(mpolsetArgs, "--flags", strings.Join(flags, ","))
	}
	if veryVerbose {
		mpolsetArgs = append(mpolsetArgs, "-vv")
	}
	mpolsetArgs = append(mpolsetArgs, "--")

	ca.SetArgs(append(mpolsetArgs, ctr.GetArgs()...))
	return nil
}

// CreateContainer modifies container when it is being created.
func (p *plugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	var ca *api.ContainerAdjustment
	var err error
	ppName := pprintCtr(pod, ctr)
	log.Tracef("CreateContainer %s", ppName)

	policy, err := p.getPolicy(pod, ctr)
	if err != nil {
		log.Errorf("CreateContainer %s: failed to get policy: %v", ppName, err)
		return nil, nil, err
	}
	if policy == nil || policy.Mode == "" {
		log.Tracef("CreateContainer %s: no memory policy", ppName)
		return nil, nil, nil
	}

	log.Debugf("CreateContainer %s: apply memory policy %s", ppName, policy)
	ca, err = applyPolicy(ctr, policy)
	if err != nil {
		log.Errorf("CreateContainer %s failed to apply policy: %v", ppName, err)
		return nil, nil, err
	}
	log.Tracef("CreateContainer %s: memory policy adjustment: %s", ppName, ca)

	if p.config.InjectMpolset {
		if err := toCommandInjection(ctr, ca); err != nil {
			log.Errorf("CreateContainer %s: failed to converting adjustment into mpolset command: %v", ppName, err)
			return nil, nil, err
		}
		log.Tracef("CreateContainer %s: converted to command injection %s", ppName, ca)
	}
	return ca, nil, nil
}

func main() {
	var (
		pluginName string
		pluginIdx  string
		configFile string
		err        error
	)

	log = logrus.StandardLogger()
	log.SetFormatter(&logrus.TextFormatter{
		PadLevelText: true,
	})

	flag.StringVar(&pluginName, "name", "", "plugin name to register to NRI")
	flag.StringVar(&pluginIdx, "idx", "", "plugin index to register to NRI")
	flag.StringVar(&configFile, "config", "", "configuration file name")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.BoolVar(&veryVerbose, "vv", false, "very verbose output and run mpolset -vv injected in containers")
	flag.Parse()

	if verbose {
		log.SetLevel(logrus.DebugLevel)
	}
	if veryVerbose {
		log.SetLevel(logrus.TraceLevel)
	}

	p := &plugin{}

	if configFile != "" {
		log.Debugf("read configuration from %q", configFile)
		config, err := os.ReadFile(configFile)
		if err != nil {
			log.Fatalf("error reading configuration file %q: %s", configFile, err)
		}
		if err = p.setConfig(config); err != nil {
			log.Fatalf("error applying configuration from file %q: %s", configFile, err)
		}
	}

	sys, err = system.DiscoverSystem(system.DiscoverCPUTopology)
	if err != nil {
		log.Fatalf("failed to discover CPU topology: %v", err)
	}

	opts := []stub.Option{
		stub.WithOnClose(p.onClose),
	}
	if pluginName != "" {
		opts = append(opts, stub.WithPluginName(pluginName))
	}
	if pluginIdx != "" {
		opts = append(opts, stub.WithPluginIdx(pluginIdx))
	}

	if p.stub, err = stub.New(p, opts...); err != nil {
		log.Fatalf("failed to create plugin stub: %v", err)
	}

	if err = p.stub.Run(context.Background()); err != nil {
		log.Errorf("plugin exited (%v)", err)
		os.Exit(1)
	}
}
