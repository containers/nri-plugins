// Copyright 2023 Intel Corporation. All Rights Reserved.
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
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"sigs.k8s.io/yaml"

	"github.com/sirupsen/logrus"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
)

type plugin struct {
	stub           stub.Stub
	config         *pluginConfig
	cgroupsDir     string
	ctrMemtierdEnv map[string]*memtierdEnv
}

type pluginConfig struct {
	// Classes define how memory of all workloads in each QoS
	// class should be managed.
	Classes []qosClass
}

type qosClass struct {
	// Name of the QoS class, matches to annotations in
	// pods. Examples:
	// annotations:
	//   # The default for all containers in the pod:
	//   class.memtierd.nri.io: swap-idle-data
	//   # Override the default for CONTAINERNAME1:
	//   class.memtierd.nri.io/CONTAINERNAME1: noswap
	//   # Do not apply any class for CONTAINERNAME2:
	//   class.memtierd.nri.io/CONTAINERNAME2: ""
	Name string

	// MemtierdConfig is a string that contains full configuration
	// for memtierd. If non-empty, a separate memtierd will be
	// launched to track each container of this QoS class.
	MemtierdConfig string

	// AllowSwap: if true, set memory.swap.max to max, if false,
	// set memory.swap.max to 0. If undefined, do not touch
	// memory.swap.max. Direct annotation that defines value of
	// memory.swap.max overrides this option.
	AllowSwap *bool
}

type memtierdEnv struct {
	ctrDir     string
	configFile string
	outputFile string
	pidFile    string
	cmd        *exec.Cmd
}

type options struct {
	runDir     string
	cgroupsDir string
}

const (
	annotationSuffix = ".memtierd.nri.io"
)

var opt = options{}

var (
	log *logrus.Logger
)

// Configure handles connecting to container runtime's NRI server.
func (p *plugin) Configure(ctx context.Context, config, runtime, version string) (stub.EventMask, error) {
	log.Infof("Connected to %s %s...", runtime, version)
	if config != "" {
		if err := p.setConfig([]byte(config)); err != nil {
			return 0, loggedErrorf("Configure: loading configuration from NRI server failed: %s", err)
		}
		log.Debugf("Using configuration from NRI server")
	} else {
		log.Debugf("No configuration from NRI server")
	}
	return 0, nil
}

// setConfig applies new plugin configuration.
func (p *plugin) setConfig(config []byte) error {
	log.Tracef("setConfig: parsing\n---8<---\n%s\n--->8---", config)
	cfg := pluginConfig{}
	err := yaml.Unmarshal(config, &cfg)
	if err != nil {
		log.Tracef("setConfig: parsing failed: %s", err)
		return fmt.Errorf("setConfig: cannot parse configuration: %w", err)
	}
	p.config = &cfg
	if log.GetLevel() == logrus.TraceLevel {
		log.Tracef("new configuration has %d classes:", len(p.config.Classes))
		for _, cls := range p.config.Classes {
			log.Tracef("- %s", cls.Name)
		}
	}
	return nil
}

// pprintCtr() returns human readable container name that is
// unique to the node.
func pprintCtr(pod *api.PodSandbox, ctr *api.Container) string {
	return fmt.Sprintf("%s/%s:%s", pod.GetNamespace(), pod.GetName(), ctr.GetName())
}

// loggedErrorf formats, logs and returns an error.
func loggedErrorf(s string, args ...any) error {
	err := fmt.Errorf(s, args...)
	log.Errorf("%s", err)
	return err
}

// associate adds new key-value pair to a map, or updates existing
// pair if called with the override set. Returns true if the pair was
// added/updated.
func associate(m map[string]string, key, value string, override bool) bool {
	if _, exists := m[key]; override || !exists {
		m[key] = value
		return true
	}
	return false
}

// effectiveAnnotations returns map of annotation key prefixes and
// values that are effective for a container. Example: a
// container-specific pod annotation
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
	}
	return effAnn
}

// CreateContainer responsibilities:
//   - validate all annotations effective for a new container so that
//     validation is no more needed in StartContainer.
//   - configure cgroups unified parameters, for instance
//     memory.swap.max.
func (p *plugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	ppName := pprintCtr(pod, ctr)
	unified := map[string]string{}
	class := ""
	for annPrefix, value := range effectiveAnnotations(pod, ctr) {
		switch annPrefix {
		case "memory.swap.max":
			unified["memory.swap.max"] = value
		case "memory.high":
			unified["memory.high"] = value
		case "class":
			class = value
			if class != "" {
				qoscls, err := p.qosClass(class)
				if err != nil {
					return nil, nil, loggedErrorf("CreateContainer: cannot search for class %q: %s", class, err)
				}
				if qoscls == nil {
					return nil, nil, loggedErrorf("CreateContainer: unknown class %q", class)
				}
				if qoscls.AllowSwap != nil {
					if *qoscls.AllowSwap {
						associate(unified, "memory.swap.max", "max", false)
					} else {
						associate(unified, "memory.swap.max", "0", false)
					}
				}
			}
		default:
			log.Errorf("CreateContainer %s: pod has invalid annotation: %q", ppName, annPrefix)
		}
	}
	if len(unified) == 0 {
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

// StartContainer launches a memtierd to manage container's memory if
// the container is associated with a QoS class that has memtierd
// configuration.
func (p *plugin) StartContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	ppName := pprintCtr(pod, ctr)
	log.Tracef("StartContainer: %s", ppName)

	namespace := pod.GetNamespace()
	podName := pod.GetName()
	containerName := ctr.GetName()

	annotatedClass, ok := effectiveAnnotations(pod, ctr)["class"]
	if !ok || annotatedClass == "" {
		log.Debugf("StartContainer: container %q has no QoS class", ppName)
		return nil
	}

	qoscls, err := p.qosClass(annotatedClass)
	if qoscls == nil || err != nil {
		return loggedErrorf("cannot find QoS class for %s: %s", ppName, err)
	}
	if qoscls.MemtierdConfig == "" {
		log.Debugf("StartContainer: QoS class %q has no MemtierdConfig in the configuration", annotatedClass)
		return nil
	}

	fullCgroupsPath, err := p.getFullCgroupsPath(ctr)
	if err != nil {
		return loggedErrorf("cannot detect cgroup v2 path for container %q: %v", ppName, err)
	}
	mtdEnv, err := newMemtierdEnv(fullCgroupsPath, namespace, podName, containerName, qoscls.MemtierdConfig, opt.runDir)
	if err != nil || mtdEnv == nil {
		return loggedErrorf("failed to prepare memtierd run environment: %v", err)
	}
	err = mtdEnv.startMemtierd()
	if err != nil {
		return loggedErrorf("failed to start memtierd: %v", err)
	}
	p.ctrMemtierdEnv[ppName] = mtdEnv
	log.Infof("StartContainer: launched memtierd for %q with config %q", ppName, mtdEnv.configFile)
	return nil
}

// qosClass returns QoS class from plugin config based on class name.
func (p *plugin) qosClass(className string) (*qosClass, error) {
	if p.config == nil {
		return nil, fmt.Errorf("plugin is not configured")
	}
	for _, class := range p.config.Classes {
		if class.Name == className {
			return &class, nil
		}
	}
	return nil, nil
}

// StopContainer stops the memtierd that manages a container.
func (p *plugin) StopContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) ([]*api.ContainerUpdate, error) {
	ppName := pprintCtr(pod, ctr)

	mtdEnv, ok := p.ctrMemtierdEnv[ppName]
	if !ok || mtdEnv == nil {
		log.Tracef("StopContainer: no memtierd environment for %s", ppName)
		return nil, nil
	}
	delete(p.ctrMemtierdEnv, ppName)

	log.Debugf("StopContainer: stopping memtierd of %s, destroy %s", ppName, mtdEnv.ctrDir)

	if mtdEnv.cmd != nil && mtdEnv.cmd.Process != nil {
		pid := mtdEnv.cmd.Process.Pid
		log.Tracef("StopContainer: killing memtierd %d", pid)
		if err := mtdEnv.cmd.Process.Kill(); err != nil {
			log.Debugf("StopContainer: killing memtierd of %s (pid: %d) failed: %s", ppName, pid, err)
		}
		// Close files, read exit status (leave no zombie processes behind)
		go func() {
			if err := mtdEnv.cmd.Wait(); err != nil {
				log.Errorf("StopContainer: waiting for memtierd of %s (pid: %d) failed: %s", ppName, pid, err)
			}
		}()
	}

	log.Tracef("StopContainer: removing memtierd run directory %s", mtdEnv.ctrDir)
	if err := os.RemoveAll(mtdEnv.ctrDir); err != nil {
		log.Debugf("StopContainer: removing memtierd run dir of %s (%q) failed: %s",
			ppName, mtdEnv.ctrDir, err)
	}
	log.Infof("StopContainer: stopped memtierd of %s", ppName)
	return nil, nil
}

// onClose handles losing connection to the NRI server
func (p *plugin) onClose() {
	log.Infof("Connection to the runtime lost, exiting...")
	os.Exit(0)
}

// detectCgroupsDir sets plugin's cgroups mount point
func (p *plugin) detectCgroupsDir() error {
	file, err := os.Open("/proc/mounts")
	if err != nil {
		return fmt.Errorf("failed to open /proc/mounts: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)

		if fields[0] == "cgroup2" {
			p.cgroupsDir = fields[1]
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read /proc/mounts: %v", err)
	}
	return fmt.Errorf("cgroup2 missing in /proc/mounts")
}

// getFullCgroupsPath returns container's cgroups directory.
func (p *plugin) getFullCgroupsPath(ctr *api.Container) (string, error) {
	var fullCgroupsPath string
	cgroupsPath := ctr.Linux.CgroupsPath
	log.Tracef("getFullCgroupsPath: ctr.Id=%q ctr.cgroupsPath=%q", ctr.Id, cgroupsPath)
	err := filepath.WalkDir(p.cgroupsDir, func(path string, info os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if strings.Contains(path, ctr.Id) {
				log.Tracef("getFullCgroupsPath: container Id matches %s", path)
				fullCgroupsPath = path
				return io.EOF
			}
		}
		return nil
	})
	if err == io.EOF {
		err = nil
	} else {
		log.Tracef("getFullCgroupsPath: could not find a directory matching *%s* anywhere under cgroups root %q", ctr.Id, p.cgroupsDir)
	}
	return fullCgroupsPath, err
}

// newMemtierdEnv prepares new memtierd run environment with a
// configuration file template instantiated for managing a container.
func newMemtierdEnv(fullCgroupPath string, namespace string, podName string, containerName string, memtierdConfigIn string, runDir string) (*memtierdEnv, error) {
	// Create container directory if it doesn't exist
	ctrDir := fmt.Sprintf("%s/%s/%s/%s", runDir, namespace, podName, containerName)
	if err := os.MkdirAll(ctrDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create memtierd run directory %q: %w", ctrDir, err)
	}

	outputFilePath := fmt.Sprintf("%s/memtierd.output", ctrDir)
	statsFilePath := fmt.Sprintf("%s/memtierd.stats", ctrDir)
	pidFilePath := fmt.Sprintf("%s/memtierd.pid", ctrDir)

	// Instantiate memtierd configuration from configuration template
	replace := map[string]string{
		"$CGROUP2_ABS_PATH":         fullCgroupPath,
		"$MEMTIERD_SWAP_STATS_PATH": statsFilePath,
	}
	memtierdConfigOut := string(memtierdConfigIn)
	for key, value := range replace {
		memtierdConfigOut = strings.Replace(memtierdConfigOut, key, value, -1)
	}

	configFilePath := fmt.Sprintf("%s/memtierd.config.yaml", ctrDir)
	if err := os.WriteFile(configFilePath, []byte(memtierdConfigOut), 0644); err != nil {
		return nil, fmt.Errorf("cannot write memtierd configuration into file %q: %w", configFilePath, err)
	}

	me := memtierdEnv{}
	me.outputFile = outputFilePath
	me.configFile = configFilePath
	me.pidFile = pidFilePath
	me.ctrDir = ctrDir
	return &me, nil
}

// startMemtierd launches memtierd in prepared environment.
func (me *memtierdEnv) startMemtierd() error {
	outputFile, err := os.OpenFile(me.outputFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create memtierd output file: %w", err)
	}

	// Create the command and write its output to the output file
	cmd := exec.Command("memtierd", "-c", "", "-config", me.configFile)
	cmd.Stdout = outputFile
	cmd.Stderr = outputFile

	// Start the command in a new session and process group
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Start the command in the background
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command %s: %q", cmd, err)
	}
	if cmd.Process != nil {
		if err := os.WriteFile(me.pidFile,
			[]byte(fmt.Sprintf("%d\n", cmd.Process.Pid)),
			0400); err != nil {
			log.Warnf("failed to write PID file %q: %s", me.pidFile, err)
		}
	}
	me.cmd = cmd
	return nil
}

// main program to run the plugin.
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
	flag.StringVar(&opt.cgroupsDir, "cgroups-dir", "", "cgroups root directory")
	flag.StringVar(&opt.runDir, "run-dir", "", "Directory prefix for memtierd runtime environments")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.BoolVar(&veryVerbose, "vv", false, "very verbose output")
	flag.Parse()

	if verbose {
		log.SetLevel(logrus.DebugLevel)
	}
	if veryVerbose {
		log.SetLevel(logrus.TraceLevel)
	}

	if opt.runDir == "" {
		opt.runDir = filepath.Join(os.TempDir(), "nri-memtierd")
	}

	p := &plugin{
		ctrMemtierdEnv: map[string]*memtierdEnv{},
	}

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

	p.cgroupsDir = opt.cgroupsDir

	if p.cgroupsDir == "" {
		if err := p.detectCgroupsDir(); err != nil {
			log.Fatalf("cannot find cgroup2 mount point. %s", err)
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
