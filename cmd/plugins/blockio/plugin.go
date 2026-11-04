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
	"os"
	"sync"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/intel/goresctrl/pkg/blockio"
	oci "github.com/opencontainers/runtime-spec/specs-go"
	"sigs.k8s.io/yaml"
)

type plugin struct {
	mu         sync.Mutex
	configFile string
	reload     bool
	weights    weightSupport
	classes    map[string]*oci.LinuxBlockIO
}

// goresctrl stores class definitions globally. Protect configuration and the
// complete snapshot operation, including when more than one plugin is tested.
var classConfigMu sync.Mutex

func readClasses(filename string) (map[string]*oci.LinuxBlockIO, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("reading block I/O configuration %q: %w", filename, err)
	}
	cfg := &blockio.Config{}
	// CRI-O intentionally accepts unknown keys and forces class configuration.
	// SetConfigFromData would use strict YAML validation instead.
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing block I/O configuration %q: %w", filename, err)
	}
	classConfigMu.Lock()
	defer classConfigMu.Unlock()
	if err := blockio.SetConfig(cfg, true); err != nil {
		return nil, fmt.Errorf("configuring block I/O: %w", err)
	}
	classes := make(map[string]*oci.LinuxBlockIO)
	for _, name := range blockio.GetClasses() {
		parameters, err := blockio.OciLinuxBlockIO(name)
		if err != nil {
			return nil, err
		}
		classes[name] = parameters
	}
	return classes, nil
}

// loadConfig is called at startup or with p.mu held. A read/parse failure leaves
// the previous snapshot intact, but it must not be applied for that request.
func (p *plugin) loadConfig() error {
	classes, err := readClasses(p.configFile)
	if err != nil {
		return err
	}
	p.classes = classes
	log.Debugf("loaded %d block I/O classes from %q", len(classes), p.configFile)
	return nil
}

func (p *plugin) Configure(_ context.Context, config, runtime, version string) (stub.EventMask, error) {
	log.Infof("Connected to %s %s", runtime, version)
	if config != "" {
		return 0, fmt.Errorf("runtime-supplied configuration is not supported; provide class definitions with --config")
	}
	return 0, nil
}

func (p *plugin) CreateContainer(_ context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	if p.configFile == "" {
		return nil, nil, nil
	}
	class, err := blockio.ContainerClassFromAnnotations(ctr.GetName(), ctr.GetAnnotations(), pod.GetAnnotations())
	if err != nil || class == "" {
		return nil, nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reload {
		if err := p.loadConfig(); err != nil {
			// CRI-O logs specSetBlockioClass reload errors and continues
			// creation without setting BlockIO for this container.
			log.Warnf("reloading block I/O configuration for %s/%s:%s: %v; skipping adjustment", pod.GetNamespace(), pod.GetName(), ctr.GetName(), err)
			return nil, nil, nil
		}
	}
	parameters, ok := p.classes[class]
	if !ok {
		log.Warnf("unknown block I/O class %q for %s/%s:%s", class, pod.GetNamespace(), pod.GetName(), ctr.GetName())
		return nil, nil, nil
	}
	unified := linuxBlockIOToUnified(parameters, p.weights)
	if len(unified) == 0 {
		return nil, nil, nil
	}
	adjustment := &api.ContainerAdjustment{}
	for key, value := range unified {
		adjustment.AddLinuxUnified(key, value)
	}
	log.Debugf("block I/O class %q for %s/%s:%s: %v", class, pod.GetNamespace(), pod.GetName(), ctr.GetName(), unified)
	return adjustment, nil, nil
}
