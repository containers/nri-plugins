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
	"fmt"
	"log"
	"os"

	"github.com/coreos/go-systemd/v22/dbus"
	tomlv2 "github.com/pelletier/go-toml/v2"
)

const (
	tomlFilePath = "/etc/containerd/config.toml"
	nriPluginKey = "io.containerd.nri.v1.nri"
	disableKey   = "disable"
	replaceMode  = "replace"
	resultDone   = "done"
	unit         = "containerd.service"
)

func main() {
	tomlMap, err := readConfig(tomlFilePath)
	if err != nil {
		log.Fatalf("Error reading TOML file: %v", err)
	}

	updatedTomlMap := updateNRIPlugin(tomlMap)

	err = writeConfig(tomlFilePath, updatedTomlMap)
	if err != nil {
		log.Fatalf("failed to write updated config into a file %q:, %v", tomlFilePath, err)
	}

	err = restartSystemdUnit(unit)
	if err != nil {
		log.Fatalf("failed to restart containerd: %v", err)
	}
}
func writeConfig(file string, config map[string]interface{}) error {
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

func updateNRIPlugin(config map[string]interface{}) map[string]interface{} {
	plugins, exists := config["plugins"].(map[string]interface{})
	if !exists {
		log.Println("Top level plugins section not found, adding it to enable NRI...")
		plugins = make(map[string]interface{})
		config["plugins"] = plugins
	}

	nri, exists := plugins[nriPluginKey].(map[string]interface{})
	if !exists {
		log.Println("NRI plugin section not found, adding it to enable NRI...")
		nri = make(map[string]interface{})
		plugins[nriPluginKey] = nri
	}

	nri[disableKey] = false
	log.Println("Enabled NRI...")
	return config
}

func restartSystemdUnit(unit string) error {
	conn, err := dbus.NewSystemConnectionContext(context.Background())
	if err != nil {
		return fmt.Errorf("failed to create DBus connection for unit %q: %w", unit, err)
	}
	defer conn.Close()

	resC := make(chan string)
	defer close(resC)

	_, err = conn.RestartUnitContext(context.Background(), unit, replaceMode, resC)
	if err != nil {
		return fmt.Errorf("failed to restart systemd unit %q: %w", unit, err)
	}

	result := <-resC

	if result != resultDone {
		return fmt.Errorf("failed to restart systemd unit %q, with result %q", unit, result)
	}
	return nil
}
