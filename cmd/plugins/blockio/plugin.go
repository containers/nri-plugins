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
	"path/filepath"
	"sync"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/intel/goresctrl/pkg/blockio"
	oci "github.com/opencontainers/runtime-spec/specs-go"
	"sigs.k8s.io/yaml"
)

type plugin struct {
	configFile string
	classes    map[string]map[string]string
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
	if err := yaml.UnmarshalStrict(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing block I/O configuration %q: %w", filename, err)
	}
	if err := validateClassDevices(cfg); err != nil {
		return nil, fmt.Errorf("validating block I/O configuration %q: %w", filename, err)
	}
	classConfigMu.Lock()
	defer classConfigMu.Unlock()
	if err := blockio.SetConfig(cfg, false); err != nil {
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

// goresctrl only logs device discovery errors, even with force disabled.
// Validate every match so a class cannot silently lose device settings.
func validateClassDevices(cfg *blockio.Config) error {
	for class, entries := range cfg.Classes {
		for i, entry := range entries {
			if entry.Devices == nil {
				continue
			}
			if len(entry.Devices) == 0 {
				return fmt.Errorf("class %q entry %d: Devices must not be empty", class, i+1)
			}
			for _, pattern := range entry.Devices {
				matches, err := filepath.Glob(pattern)
				if err != nil {
					return fmt.Errorf("class %q entry %d: invalid device pattern %q: %w", class, i+1, pattern, err)
				}
				if len(matches) == 0 {
					return fmt.Errorf("class %q entry %d: device pattern %q matches no devices", class, i+1, pattern)
				}
				for _, path := range matches {
					info, err := os.Stat(path)
					if err != nil {
						return fmt.Errorf("class %q entry %d: checking device %q: %w", class, i+1, path, err)
					}
					if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
						return fmt.Errorf("class %q entry %d: %q is not a block device", class, i+1, path)
					}
				}
			}
		}
	}
	return nil
}

// loadConfig loads the immutable class snapshot before the plugin starts.
func (p *plugin) loadConfig() error {
	classes, err := readClasses(p.configFile)
	if err != nil {
		return err
	}
	resources := make(map[string]map[string]string, len(classes))
	for name, parameters := range classes {
		resources[name] = linuxBlockIOToUnified(parameters)
	}
	p.classes = resources
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
	unified := p.classResources(pod, ctr, class)
	if len(unified) == 0 {
		return nil, nil, nil
	}
	adjustment := &api.ContainerAdjustment{}
	for key, value := range unified {
		adjustment.AddLinuxUnified(key, value)
	}
	return adjustment, nil, nil
}

// classResources resolves a class from the startup snapshot.
func (p *plugin) classResources(pod *api.PodSandbox, ctr *api.Container, class string) map[string]string {
	unified, ok := p.classes[class]
	if !ok {
		log.Warnf("unknown block I/O class %q for %s/%s:%s", class, pod.GetNamespace(), pod.GetName(), ctr.GetName())
		return nil
	}
	log.Debugf("block I/O class %q for %s/%s:%s: %v", class, pod.GetNamespace(), pod.GetName(), ctr.GetName(), unified)
	return unified
}
