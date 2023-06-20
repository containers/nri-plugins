/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"gopkg.in/yaml.v2"

	"github.com/sirupsen/logrus"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
)

type plugin struct {
	stub stub.Stub
	mask stub.EventMask
}

type MemtierdConfig struct {
	Policy   Policy     `yaml:"policy"`
	Routines []Routines `yaml:"routines"`
}

type Routines struct {
	Name   string `yaml:"name"`
	Config string `yaml:"config"`
}

type Policy struct {
	Name   string `yaml:"name"`
	Config string `yaml:"config"`
}

type options struct {
	HostRoot string
}

var opt = options{}

var (
	log *logrus.Logger
)

func (p *plugin) Configure(ctx context.Context, config, runtime, version string) (stub.EventMask, error) {
	log.Infof("Connected to %s/%s...", runtime, version)

	if config == "" {
		return 0, nil
	}

	return 0, nil
}

func (p *plugin) StartContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	log.Infof("Starting container %s/%s/%s...", pod.GetNamespace(), pod.GetName(), ctr.GetName())

	hostRoot := opt.HostRoot

	podName := pod.GetName()
	containerName := ctr.GetName()
	annotations := pod.GetAnnotations()

	// If memtierd annotation is not present, don't execute further
	class, ok := annotations["class.memtierd.nri"]
	if !ok {
		return nil
	}

	// Check that class is of correct form
	pattern := "^[A-Za-z0-9_-]+$"
	regex, err := regexp.Compile(pattern)
	if err != nil {
		// Handle error if the pattern is invalid
		log.Fatalf("Invalid regex pattern:", err)
		return nil
	}

	if !regex.MatchString(class) {
		log.Fatalf("Invalid memtierd.class.nri!")
		return nil
	}

	// You can specify the template here based on the given class ex. class.memtierd.nri: low-prio -> template = low-prio.yaml
	// Plugin looks in the /template directory and looks for the low-prio.yaml then
	template := ""

	if class == "example-configuration" {
		template = "example-configuration.yaml"
	}

	fullCgroupPath := getFullCgroupPath(ctr)
	podDirectory, outputFilePath, configFilePath := editMemtierdConfig(fullCgroupPath, podName, containerName, template, hostRoot)
	startMemtierd(podName, containerName, podDirectory, outputFilePath, configFilePath)

	return nil
}

func (p *plugin) StopContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) ([]*api.ContainerUpdate, error) {
	log.Infof("Stopped container %s/%s/%s...", pod.GetNamespace(), pod.GetName(), ctr.GetName())

	podName := pod.GetName()
	dirPath := fmt.Sprintf("%s/memtierd/%s", os.TempDir(), podName)

	// Kill the memtierd process
	out, err := exec.Command("sudo", "pkill", "-f", dirPath).CombinedOutput()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 1 {
			// Error occurred that is not related to "no processes found"
			log.Fatalf("Error killing memtierd process: %v. Output: %s\n", err, out)
		} else {
			// "No processes found" error, do nothing
			log.Printf("No processes found for memtierd process\n")
		}
	}

	err = os.RemoveAll(dirPath)
	if err != nil {
		fmt.Println(err)
	}

	return []*api.ContainerUpdate{}, nil
}

func (p *plugin) onClose() {
	log.Infof("Connection to the runtime lost, exiting...")
	os.Exit(0)
}

func getFullCgroupPath(ctr *api.Container) string {
	cgroupPath := ctr.Linux.CgroupsPath

	split := strings.Split(cgroupPath, ":")

	partOne := split[0]
	partTwo := fmt.Sprintf("%s-%s.scope", split[1], split[2])

	partialPath := fmt.Sprintf("%s/%s", partOne, partTwo)

	fullPath := fmt.Sprintf("*/kubepods*/%s", partialPath)

	if !strings.HasSuffix(fullPath, ".scope") && !strings.HasSuffix(fullPath, ".slice") {
		log.Fatalf("Cgroupfs not supported.")
	}

	file, err := os.Open("/proc/mounts")
	if err != nil {
		log.Fatalf("failed to open /proc/mounts: %v", err)
	}
	defer file.Close()

	cgroupMountPoint := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)

		if fields[0] == "cgroup2" {
			cgroupMountPoint = fields[1]
			break
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("failed to read /proc/mounts: %v", err)
	}

	// Find the cgroup path corresponding to the container
	var fullCgroupPath string
	err = filepath.WalkDir(cgroupMountPoint, func(path string, info os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			matches, err := filepath.Glob(filepath.Join(path, fullPath))
			if err != nil {
				return err
			}

			if len(matches) > 0 {
				fullCgroupPath = matches[0]
				return filepath.SkipDir
			}
		}

		return nil
	})

	if err != nil {
		log.Fatalf("failed to traverse cgroup directories: %v", err)
	}

	if fullCgroupPath == "" {
		log.Fatalf("cgroup path not found")
	}

	log.Printf("Cgroup path: %s", fullCgroupPath)

	return fullCgroupPath
}

func editMemtierdConfig(fullCgroupPath string, podName string, containerName string, template string, hostRoot string) (string, string, string) {
	templatePath := fmt.Sprintf("/templates/%s", template)
	yamlFile, err := ioutil.ReadFile(templatePath)
	if err != nil {
		log.Fatalf("Error reading YAML file: %v\n", err)
	}

	// Create pod directory if it doesn't exist
	podDirectory := fmt.Sprintf("%s%s/memtierd/%s", hostRoot, os.TempDir(), podName)
	if err := os.MkdirAll(podDirectory, 0755); err != nil {
		log.Fatalf("Error creating directory: %v", err)
	}

	outputFilePath := fmt.Sprintf("%s/memtierd.%s.output", podDirectory, containerName)

	var memtierdConfig MemtierdConfig
	err = yaml.Unmarshal(yamlFile, &memtierdConfig)
	if err != nil {
		log.Fatalf("Error unmarshaling YAML: %v\n", err)
	}

	fullCgroupPathString := fullCgroupPath

	// Edit the Policy and Routine configs
	policyConfigFieldString := string(memtierdConfig.Policy.Config)
	policyConfigFieldString = strings.Replace(policyConfigFieldString, "$CGROUP2_ABS_PATH", fullCgroupPathString, 1)

	// Loop through the routines
	for i := 0; i < len(memtierdConfig.Routines); i++ {
		routineConfigFieldString := string(memtierdConfig.Routines[i].Config)
		routineConfigFieldString = strings.Replace(routineConfigFieldString, "$MEMTIERD_SWAP_STATS_PATH", outputFilePath, 1)
		memtierdConfig.Routines[i].Config = routineConfigFieldString
	}

	memtierdConfig.Policy.Config = policyConfigFieldString

	out, err := yaml.Marshal(&memtierdConfig)
	if err != nil {
		log.Fatalf("Error marshaling YAML: %v\n", err)
	}

	configFilePath := fmt.Sprintf(podDirectory+"/%s.yaml", containerName)
	err = ioutil.WriteFile(configFilePath, out, 0644)
	if err != nil {
		log.Fatalf("Error writing YAML file: %v\n", err)
	}
	log.Infof("YAML file successfully modified.")

	return podDirectory, outputFilePath, configFilePath
}

func startMemtierd(podName string, containerName string, podDirectory string, outputFilePath string, configFilePath string) {
	log.Infof("Starting Memtierd")

	outputFile, err := os.OpenFile(outputFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Printf("Failed to open output file: %v\n", err)
	}

	// Create the command and write its output to the output file
	cmd := exec.Command("memtierd", "-c", "", "-config", configFilePath)
	cmd.Stdout = outputFile
	cmd.Stderr = outputFile

	// Start the command in a new session and process group
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Start the command in the background
	if err := cmd.Start(); err != nil {
		fmt.Printf("Failed to start command: %v\n", err)
	}
}

func main() {
	var (
		pluginName string
		pluginIdx  string
		err        error
	)

	log = logrus.StandardLogger()
	log.SetFormatter(&logrus.TextFormatter{
		PadLevelText: true,
	})

	flag.StringVar(&pluginName, "name", "", "plugin name to register to NRI")
	flag.StringVar(&pluginIdx, "idx", "", "plugin index to register to NRI")
	flag.StringVar(&opt.HostRoot, "host-root", "", "Directory prefix under which the host's tmp, etc. are mounted.")
	flag.Parse()

	p := &plugin{}
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
