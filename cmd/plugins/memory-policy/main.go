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
	Name       string            `json:"name"`
	PolicySpec *MemoryPolicySpec `json:"policy"`
}

type MemoryPolicySpec struct {
	Mode  string   `json:"mode"`
	Nodes string   `json:"nodes"`
	Flags []string `json:"flags,omitempty"`
}

type LinuxMemoryPolicy struct {
	Mode  string
	Nodes string
	Flags []string
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

	// When NRI API supports memory policies, we will switch to using
	// api.MpolMode and api.MpolFlag instead of these maps.
	MpolMode_value = map[string]int32{
		"MPOL_DEFAULT":             0,
		"MPOL_PREFERRED":           1,
		"MPOL_BIND":                2,
		"MPOL_INTERLEAVE":          3,
		"MPOL_LOCAL":               4,
		"MPOL_PREFERRED_MANY":      5,
		"MPOL_WEIGHTED_INTERLEAVE": 6,
	}
	MpolFlag_value = map[string]int32{
		"MPOL_F_STATIC_NODES":   0,
		"MPOL_F_RELATIVE_NODES": 1,
		"MPOL_F_NUMA_BALANCING": 2,
	}
)

func (mpol *MemoryPolicySpec) String() string {
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
		if class.PolicySpec == nil {
			return fmt.Errorf("class %q has no policy", class.Name)
		}
		if class.PolicySpec.Mode == "" {
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
func takePolicyAnnotation(ann map[string]string) (*MemoryPolicySpec, error) {
	if value, ok := ann["policy"]; ok {
		delete(ann, "policy")
		if value == "" {
			return nil, nil
		}
		policy := &MemoryPolicySpec{}
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

// getPolicySpec() returns the memory policy for a container.
func (p *plugin) getPolicySpec(pod *api.PodSandbox, ctr *api.Container) (*MemoryPolicySpec, error) {
	effAnn := effectiveAnnotations(pod, ctr)

	policySpec, err := takePolicyAnnotation(effAnn)
	if err != nil {
		return nil, fmt.Errorf("invalid 'policy' annotation: %w", err)
	}
	if policySpec != nil {
		log.Tracef("- effective policy annotation: %+v", policySpec)
		return policySpec, nil
	}

	class, err := p.takeClassAnnotation(effAnn)
	if err != nil {
		return nil, fmt.Errorf("invalid 'class' annotation: %w", err)
	}
	if class != nil {
		log.Tracef("- effective class annotation: %+v", class)
		if class.PolicySpec == nil {
			return nil, fmt.Errorf("class %q has no policy", class.Name)
		}
		return class.PolicySpec, nil
	}

	// Check for unknown annotations.
	for ann := range effAnn {
		return nil, fmt.Errorf("unknown annotation %s%s", ann, annotationSuffix)
	}

	log.Tracef("- no memory policy found in annotations")
	return nil, nil
}

// ToLinuxMemoryPolicy() converts the memory policy specification into
// valid mode, nodes and flags. It is responsible for:
//   - validating the mode and flags. Passing invalid mode or flags into
//     injected command would be dangerous.
//   - calculating exact node numbers based on node specification,
//     container's cpuset and allowed memory nodes.
func (policySpec *MemoryPolicySpec) ToLinuxMemoryPolicy(ctr *api.Container) (*LinuxMemoryPolicy, error) {
	var err error
	var nodeMask libmem.NodeMask

	if policySpec == nil {
		return nil, nil
	}

	// Validate mode.
	_, ok := MpolMode_value[policySpec.Mode]
	if !ok {
		return nil, fmt.Errorf("invalid memory policy mode %q", policySpec.Mode)
	}

	// Resolve nodes based on the policy specification.
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
	case policySpec.Nodes == "all":
		nodeMask = libmem.NewNodeMask(sys.NodeIDs()...)
		log.Tracef("- nodes %q (all)", nodeMask.MemsetString())

	// "allowed-mems" includes only allowed memory nodes into the mask.
	case policySpec.Nodes == "allowed-mems":
		nodeMask = allowedMemsMask
		log.Tracef("- nodes: %q (allowed-mems)", nodeMask.MemsetString())

	// "cpu-packages" includes all nodes that are in the same package
	// as the CPUs in the container's cpuset.
	case policySpec.Nodes == "cpu-packages":
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
	case policySpec.Nodes == "cpu-nodes":
		nodeIds := sys.IDSetForCPUs(ctrCpuset, func(cpu system.CPU) idset.ID {
			return cpu.NodeID()
		})
		nodeMask = libmem.NewNodeMask(nodeIds.Members()...)
		log.Tracef("- nodes: %q (cpu-nodes)", nodeMask.MemsetString())

	// "max-dist:<int>" includes all nodes that are within the
	// specified distance from the CPUs in the container's cpuset.
	case strings.HasPrefix(policySpec.Nodes, "max-dist:"):
		maxDist := policySpec.Nodes[len("max-dist:"):]
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
	case policySpec.Nodes[0] >= '0' && policySpec.Nodes[0] <= '9':
		nodeMask, err = libmem.ParseNodeMask(policySpec.Nodes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse nodes %q: %v", policySpec.Nodes, err)
		}
		log.Tracef("- nodes %q (hardcoded)", nodeMask.MemsetString())

	default:
		return nil, fmt.Errorf("invalid nodes: %q", policySpec.Nodes)
	}

	nodes := nodeMask.MemsetString()
	if (nodeMask & allowedMemsMask) != nodeMask {
		log.Debugf("some memory policy nodes (%s) are not allowed (%s)", nodes, allowedMemsMask.MemsetString())
	}

	// Copy and validate flags.
	flags := []string{}
	for _, flag := range policySpec.Flags {
		if _, ok := MpolFlag_value[flag]; !ok {
			return nil, fmt.Errorf("invalid memory policy flag %q", flag)
		}
		flags = append(flags, flag)
	}

	linuxMemoryPolicy := &LinuxMemoryPolicy{
		Mode:  policySpec.Mode,
		Nodes: nodes,
		Flags: flags,
	}

	return linuxMemoryPolicy, nil
}

// ToMemoryPolicyAdjustment() returns a ContainerAdjustment with
// corresponding LinuxMemoryPolicy adjustment.
func (policy *LinuxMemoryPolicy) ToMemoryPolicyAdjustment() (*api.ContainerAdjustment, error) {
	if policy == nil {
		return nil, nil
	}
	return nil, fmt.Errorf("memory policy adjustment is not implemented yet")

	// // Uncomment this to use memory policy in NRI API
	// ca := &api.ContainerAdjustment{}
	// mode, ok := api.MpolMode_value[policy.Mode]
	// if !ok {
	// 	return nil, fmt.Errorf("invalid memory policy mode %q", policy.Mode)
	// }
	//
	// flags := []api.MpolFlag{}
	// for _, flag := range policy.Flags {
	// 	if flagValue, ok := api.MpolFlag_value[flag]; ok {
	// 		flags = append(flags, api.MpolFlag(flagValue))
	// 	} else {
	// 		return nil, fmt.Errorf("invalid memory policy flag %q", flag)
	// 	}
	// }
	// ca.SetLinuxMemoryPolicy(api.MpolMode(mode), policy.Nodes, flags...)
	// return ca, nil
}

// ToCommandInjectionAdjustment() converts the memory policy into a
// command injection adjustment that mounts the mpolset binary, too.
func (policy *LinuxMemoryPolicy) ToCommandInjectionAdjustment(ctr *api.Container) (*api.ContainerAdjustment, error) {
	ca := &api.ContainerAdjustment{}
	ca.AddMount(&api.Mount{
		Source:      mpolsetInjectDir,
		Destination: mpolsetInjectDir,
		Type:        "bind",
		Options:     []string{"bind", "ro", "rslave"},
	})
	mpolsetArgs := []string{
		filepath.Join(mpolsetInjectDir, "mpolset"),
		"--mode", policy.Mode,
		"--nodes", policy.Nodes,
	}
	if len(policy.Flags) > 0 {
		mpolsetArgs = append(mpolsetArgs, "--flags", strings.Join(policy.Flags, ","))
	}
	if veryVerbose {
		mpolsetArgs = append(mpolsetArgs, "-vv")
	}
	mpolsetArgs = append(mpolsetArgs, "--")

	ca.SetArgs(append(mpolsetArgs, ctr.GetArgs()...))
	return ca, nil
}

// CreateContainer modifies container when it is being created.
func (p *plugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	var ca *api.ContainerAdjustment
	var err error
	ppName := pprintCtr(pod, ctr)
	log.Tracef("CreateContainer %s", ppName)

	policySpec, err := p.getPolicySpec(pod, ctr)
	if err != nil {
		log.Errorf("CreateContainer %s: failed to get policy: %v", ppName, err)
		return nil, nil, err
	}
	if policySpec == nil || policySpec.Mode == "" {
		log.Tracef("- no memory policy")
		return nil, nil, nil
	}

	policy, err := policySpec.ToLinuxMemoryPolicy(ctr)
	if err != nil || policy == nil {
		log.Errorf("CreateContainer %s: failed to convert policy to LinuxMemoryPolicy: %v", ppName, err)
		return nil, nil, err
	}

	if p.config.InjectMpolset {
		if ca, err = policy.ToCommandInjectionAdjustment(ctr); err != nil {
			log.Errorf("CreateContainer %s: failed to convert adjustment into mpolset command: %v", ppName, err)
			return nil, nil, err
		}
	} else {
		if ca, err = policy.ToMemoryPolicyAdjustment(); err != nil {
			log.Errorf("CreateContainer %s: failed to convert policy to ContainerAdjustment: %v", ppName, err)
			return nil, nil, err
		}
	}
	log.Debugf("CreateContainer %s: memory policy %s (resolved nodes: %s)", ppName, policySpec, policy.Nodes)
	log.Tracef("- adjustment: %+v", ca)
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
