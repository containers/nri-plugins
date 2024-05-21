/*
Copyright 2023 Intel Corporation

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
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	tomlv2 "github.com/pelletier/go-toml/v2"
	"github.com/sirupsen/logrus"
)

const (
	containerdConfigFile = "/etc/containerd/config.toml"
	crioConfigFile       = "/etc/crio/crio.conf.d/10-enable-nri.conf"
	nriPluginKey         = "io.containerd.nri.v1.nri"
	replaceMode          = "replace"
	resultDone           = "done"
	containerdUnit       = "containerd.service"
	crioUnit             = "crio.service"
)

var (
	log = logrus.StandardLogger()
)

type nriConfig struct {
	registrationTimeout string
	requestTimeout      string
}

func main() {
	var cfg nriConfig

	flag.StringVar(&cfg.requestTimeout,
		"nri-plugin-request-timeout", "", "NRI plugin request timeout to patch, as time.Duration")
	flag.StringVar(&cfg.registrationTimeout,
		"nri-plugin-registration-timeout", "", "NRI plugin registration timeout to patch, as time.Duration")

	flag.Parse()

	err := cfg.check()
	if err != nil {
		log.Fatalf("invalid NRI configuration requested: %v", err)
	}

	unit, conn, err := detectRuntime()
	if err != nil {
		log.Fatalf("failed to autodetect container runtime: %v", err)
	}
	defer conn.Close()

	switch unit {
	case containerdUnit:
		err = configureNriForContainerd(&cfg)
	case crioUnit:
		err = configureNriForCrio(&cfg)
	default:
		log.Fatalf("unknown container runtime %q", unit)
	}

	if err != nil {
		log.Fatalf("error enabling NRI: %v", err)
	}

	//
	// TODO(klihub): Kludge warning...
	//   If the runtime is CRI-O, it looks like we need to cut it some
	//   slack, after we've been started up by it but before we restart
	//   it. Otherwise it always reports our exit status as -1 (255).
	//   We are an init-container so a non-zero exit status would prevent
	//   other containers in our pod from ever starting...
	//

	if unit == crioUnit {
		time.Sleep(3 * time.Second)
	}

	if err = restartSystemdUnit(conn, unit); err != nil {
		log.Fatalf("failed to restart %q unit: %v", unit, err)
	}

	log.Println("enabled NRI for", unit)
}

func configureNriForContainerd(cfg *nriConfig) error {
	log.Infof("configuring NRI for containerd...")
	tomlMap, err := readConfig(containerdConfigFile)
	if err != nil {
		return fmt.Errorf("error reading TOML file: %w", err)
	}

	updatedTomlMap := updateContainerdConfig(tomlMap, cfg)

	err = writeToContainerdConfig(containerdConfigFile, updatedTomlMap)
	if err != nil {
		return fmt.Errorf("failed to write updated config into a file %q: %w", containerdConfigFile, err)
	}
	return nil
}

func configureNriForCrio(cfg *nriConfig) error {
	log.Infof("configuring NRI for CRI-O...")
	f, err := os.Create(crioConfigFile)
	if err != nil {
		return fmt.Errorf("error creating a drop-in file for CRI-O: %w", err)
	}
	defer f.Close()

	_, err = f.WriteString("[crio.nri]\nenable_nri = true\n")
	if err != nil {
		return fmt.Errorf("error writing a drop-in file for CRI-O: %w", err)
	}

	err = cfg.writeCrioConfig(f)
	if err != nil {
		return fmt.Errorf("error writing NRI configuration for CRI-O: %w", err)
	}

	return nil
}

func writeToContainerdConfig(file string, config map[string]interface{}) error {
	var buf bytes.Buffer
	enc := tomlv2.NewEncoder(&buf)
	enc.SetIndentTables(true)
	if err := enc.Encode(config); err != nil {
		return fmt.Errorf("error encoding file: %w", err)
	}

	f, err := os.Create(file)
	if err != nil {
		return fmt.Errorf("error truncating file: %w", err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	_, err = w.WriteString(buf.String())
	if err != nil {
		return fmt.Errorf("error writing to file: %w", err)
	}
	return w.Flush()
}

func readConfig(file string) (map[string]interface{}, error) {
	tomlData, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	var tomlMap map[string]interface{}
	if err := tomlv2.Unmarshal(tomlData, &tomlMap); err != nil {
		return nil, fmt.Errorf("error unmarshaling TOML: %w", err)
	}
	return tomlMap, nil
}

func updateContainerdConfig(config map[string]interface{}, cfg *nriConfig) map[string]interface{} {
	plugins, exists := config["plugins"].(map[string]interface{})
	if !exists {
		log.Println("top level plugins section not found, adding it to enable NRI...")
		plugins = make(map[string]interface{})
		config["plugins"] = plugins
	}

	nri, exists := plugins[nriPluginKey].(map[string]interface{})
	if !exists {
		log.Println("NRI plugin section not found, adding it to enable NRI...")
		nri = make(map[string]interface{})
		plugins[nriPluginKey] = nri
	}

	nri["disable"] = false

	cfg.updateContainerdConfig(config)

	return config
}

func (cfg *nriConfig) check() error {
	switch {
	case cfg.registrationTimeout == "" && cfg.requestTimeout == "":
		return nil
	case cfg.registrationTimeout != "" && cfg.requestTimeout == "":
		return fmt.Errorf("NRI plugin registration timeout set without request timeout")
	case cfg.registrationTimeout == "" && cfg.requestTimeout != "":
		return fmt.Errorf("NRI plugin request timeout set without registration timeout")
	}

	register, err := time.ParseDuration(cfg.registrationTimeout)
	if err != nil {
		return fmt.Errorf("invalid plugin registration timeout: %w", err)
	}
	request, err := time.ParseDuration(cfg.requestTimeout)
	if err != nil {
		return fmt.Errorf("invalid plugin request timeout: %w", err)
	}

	if register <= request {
		return fmt.Errorf("NRI plugin registration timeout (%s) must be > request timeout (%s)",
			register, request)
	}

	return nil
}

func (cfg *nriConfig) writeCrioConfig(f *os.File) error {
	const (
		registrationTimeout = "nri_plugin_registration_timeout"
		requestTimeout      = "nri_plugin_request_timeout"
	)
	if cfg.registrationTimeout != "" {
		key, value := registrationTimeout, "\""+cfg.registrationTimeout+"\""
		if _, err := f.WriteString(key + " = " + value + "\n"); err != nil {
			return err
		}
	}
	if cfg.requestTimeout != "" {
		key, value := requestTimeout, "\""+cfg.requestTimeout+"\""
		if _, err := f.WriteString(key + " = " + value + "\n"); err != nil {
			return err
		}
	}
	return nil
}

func (cfg *nriConfig) updateContainerdConfig(tomlCfg map[string]interface{}) {
	const (
		registrationTimeout = "plugin_registration_timeout"
		requestTimeout      = "plugin_request_timeout"
	)

	if cfg.registrationTimeout != "" {
		tomlCfg[registrationTimeout] = cfg.registrationTimeout
	}
	if cfg.requestTimeout != "" {
		tomlCfg[requestTimeout] = cfg.requestTimeout
	}
}

func detectRuntime() (string, *dbus.Conn, error) {
	log.Infof("setting up D-Bus connection...")
	conn, err := dbus.NewSystemConnectionContext(context.Background())
	if err != nil {
		return "", nil, fmt.Errorf("failed to create DBus connection: %w", err)
	}

	// Filter out active container runtime (CRI-O or containerd) systemd units on the node.
	// It is expected that only one container runtime systemd unit should be active at a time
	// (either containerd or CRI-O).If more than one container runtime systemd unit is found
	// to be in an active state, the process fails.
	log.Infof("looking for active runtime units on D-Bus...")
	units, err := conn.ListUnitsFilteredContext(context.Background(), []string{"active"})
	if err != nil {
		return "", nil, fmt.Errorf("failed to detect container runtime in use: %w", err)
	}

	expectedUnits := make([]dbus.UnitStatus, 0)
	for _, unit := range units {
		if unit.Name == containerdUnit || unit.Name == crioUnit {
			expectedUnits = append(expectedUnits, unit)
		}
	}

	if len(expectedUnits) == 0 {
		return "", nil, fmt.Errorf("failed to detect container runtime in use: got 0 systemd units")
	}

	if len(expectedUnits) > 1 {
		return "", nil, fmt.Errorf("detected more than one container runtime on the host, expected one")
	}

	log.Infof("found %s...", expectedUnits[0].Name)

	return expectedUnits[0].Name, conn, nil
}

func restartSystemdUnit(conn *dbus.Conn, unit string) error {
	resC := make(chan string)
	defer close(resC)

	log.Infof("restarting D-Bus unit %s...", unit)
	_, err := conn.RestartUnitContext(context.Background(), unit, replaceMode, resC)
	if err != nil {
		return fmt.Errorf("failed to restart systemd unit %q: %w", unit, err)
	}

	result := <-resC

	if result != resultDone {
		return fmt.Errorf("failed to restart systemd unit %q, with result %q", unit, result)
	}
	return nil
}
