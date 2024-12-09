// Copyright 2023 Inter Corporation. All Rights Reserved.
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
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/sirupsen/logrus"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
)

type plugin struct {
	stub   stub.Stub
	config *pluginConfig
}

type pluginConfig struct {
	// UnifiedAnnotations lists keys whose values are written
	// directly from annotations to the OCI Linux unified
	// object. Example:
	//     UnifiedAnnotations: ["memory.high", "memory.swap.max"]
	// allows using pod annotation
	//     memory.swap.max.memory-qos.nri.io: max
	// that will add unified["memory.swap.max"] = "max"
	UnifiedAnnotations []string

	// Classes define how memory of all workloads in each QoS
	// class should be managed.
	Classes []QoSClass
}

type QoSClass struct {
	// Name of the QoS class, matches to annotations in
	// pods. Examples:
	// Set the default class for containers in the pod:
	//     class.memory-qos.nri.io: "swap"
	// Override the default class of CONTAINERNAME:
	//     class.memory-qos.nri.io/CONTAINERNAME: "noswap"
	Name string

	// SwapLimitRatio sets memory.high based on memory limit.
	// 1.0 means no throttling before getting OOM-killed.
	// 0.75 throttle (reclaim pages) when usage reaches 75 % of memory limit.
	SwapLimitRatio float32
}

const (
	annotationSuffix = ".memory-qos.nri.io"
)

var (
	log *logrus.Logger
)

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
	return 0, nil
}

// onClose handles losing connection to container runtime.
func (p *plugin) onClose() {
	log.Infof("Connection to the runtime lost, exiting...")
	os.Exit(0)
}

// setConfig applies new plugin configuration.
func (p *plugin) setConfig(config []byte) error {
	log.Tracef("setConfig: parsing\n---8<---\n%s\n--->8---", config)
	cfg := pluginConfig{}
	err := yaml.Unmarshal(config, &cfg)
	if err != nil {
		errWithContext := fmt.Errorf("setConfig: cannot parse configuration: %w", err)
		log.Debugf("%s", errWithContext)
		return errWithContext
	}
	p.config = &cfg
	log.Tracef("new configuration has %d classes:", len(p.config.Classes))
	for _, cls := range p.config.Classes {
		log.Tracef("- %s", cls.Name)
	}
	return nil
}

// pprintCtr() returns unique human readable container name.
func pprintCtr(pod *api.PodSandbox, ctr *api.Container) string {
	return fmt.Sprintf("%s/%s:%s", pod.GetNamespace(), pod.GetName(), ctr.GetName())
}

// applyQosClass applies QoS class to a container, updates unified values.
func (p *plugin) applyQosClass(pod *api.PodSandbox, ctr *api.Container, cls string, unified map[string]string) error {
	if p.config == nil {
		return fmt.Errorf("missing plugin configuration")
	}
	for _, class := range p.config.Classes {
		log.Tracef("comparing configuration class %q to annotation %q", class.Name, cls)
		if class.Name == cls {
			log.Tracef("applying SwapLimitRatio=%.2f on unified=%v", class.SwapLimitRatio, unified)
			if class.SwapLimitRatio > 0 {
				memLimitp := ctr.Linux.Resources.Memory.Limit
				if memLimitp == nil {
					return fmt.Errorf("missing container memory limit")
				}
				// memory.high and memory.swap.max
				// values defined by the QoS class do
				// not override these values if set by
				// specifically with unified annotations.
				associate(unified, "memory.high", strconv.FormatInt(int64(float32(memLimitp.Value)*(1.0-class.SwapLimitRatio)), 10), false)
				associate(unified, "memory.swap.max", "max", false)
			}
			log.Tracef("resulted unified=%v", unified)
			return nil
		}
	}
	return fmt.Errorf("class not found in plugin configuration")
}

// associate adds new key-value pair to a map, or updates existing
// pair if called with override. Returns true if added/updated.
func associate(m map[string]string, key, value string, override bool) bool {
	if _, exists := m[key]; override || !exists {
		m[key] = value
		return true
	}
	return false
}

// sliceContains returns true if and only if haystack contains
// needle. Note: go 1.21+ will enable using slices.Contains().
func sliceContains(haystack []string, needle string) bool {
	for _, hay := range haystack {
		if hay == needle {
			return true
		}
	}
	return false
}

// effectiveAnnotations returns map of annotation key prefixes and
// values that are effective for a container.
// Example: a container-specific pod annotation
//
//	memory.high.memory-qos.nri.io/CTRNAME: 10000000
//
// shows up as
//
//	effAnn["memory.high"] = "10000000"
func effectiveAnnotations(pod *api.PodSandbox, ctr *api.Container) map[string]string {
	effAnn := map[string]string{}
	for key, value := range pod.GetAnnotations() {
		annPrefix, hasSuffix := strings.CutSuffix(key, annotationSuffix+"/"+ctr.Name)
		if hasSuffix {
			// Override possibly already found pod-level annotation.
			log.Tracef("- found container-specific annotation %q", key)
			associate(effAnn, annPrefix, value, true)
			effAnn[annPrefix] = value
			continue
		}
		annPrefix, hasSuffix = strings.CutSuffix(key, annotationSuffix)
		if hasSuffix {
			// Do not override if there already is a
			// container-level annotation.
			if associate(effAnn, annPrefix, value, false) {
				log.Tracef("- found pod-level annotation %q", key)
			} else {
				log.Tracef("- ignoring pod-level annotation %q due to a container-level annotation", key)
			}
			continue
		}
		log.Tracef("- ignoring annotation %q", key)
	}
	return effAnn
}

// CreateContainer modifies container when it is being created.
func (p *plugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	ppName := pprintCtr(pod, ctr)
	log.Tracef("CreateContainer %s", ppName)
	unified := map[string]string{}
	class := ""
	for annPrefix, value := range effectiveAnnotations(pod, ctr) {
		switch {
		case annPrefix == "class":
			if err := p.applyQosClass(pod, ctr, value, unified); err != nil {
				errWithContext := fmt.Errorf("cannot apply memory QoS class %q: %w", value, err)
				log.Errorf("CreateContainer %s: %s", ppName, errWithContext)
				return nil, nil, errWithContext
			}
			class = value
		case sliceContains(p.config.UnifiedAnnotations, annPrefix):
			unified[annPrefix] = value
			log.Tracef("applying unified annotation %q resulted in unified=%v", annPrefix, unified)
		default:
			err := fmt.Errorf("CreateContainer %s: invalid annotation: %q", ppName, annPrefix)
			log.Errorf("%s", err)
			return nil, nil, err
		}
	}
	if len(unified) == 0 {
		log.Debugf("CreateContainer %s: no adjustments", ppName)
		return nil, nil, nil
	}
	ca := api.ContainerAdjustment{
		Linux: &api.LinuxContainerAdjustment{
			Resources: &api.LinuxResources{
				Unified: unified,
			},
		},
	}
	log.Debugf("CreateContainer %s: class %q, LinuxResources.Unified=%v", ppName, class, ca.Linux.Resources.Unified)
	return &ca, nil, nil
}

func main() {
	var (
		pluginName  string
		pluginIdx   string
		configFile  string
		err         error
		verbose     bool
		veryVerbose bool
	)

	log = logrus.StandardLogger()
	log.SetFormatter(&logrus.TextFormatter{
		PadLevelText: true,
	})

	flag.StringVar(&pluginName, "name", "", "plugin name to register to NRI")
	flag.StringVar(&pluginIdx, "idx", "", "plugin index to register to NRI")
	flag.StringVar(&configFile, "config", "", "configuration file name")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.BoolVar(&veryVerbose, "vv", false, "very verbose output")
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
