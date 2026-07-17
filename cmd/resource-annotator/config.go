// Copyright Intel Corporation. All Rights Reserved.
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
	"flag"
	"fmt"
	"os"
	"strconv"
)

// Config holds runtime configuration for the resource annotator.
type Config struct {
	Port     uint
	CertFile string
	KeyFile  string
}

const (
	// DefaultPort is the default port our HTTPS server listens on.
	DefaultPort = 8443
	// DefaultCertFile is the default path to our TLS certificate file.
	DefaultCertFile = "/etc/resource-annotator/certs.d/tls.crt"
	// DefaultKeyFile is the default path to our TLS private key file.
	DefaultKeyFile = "/etc/resource-annotator/certs.d/tls.key"

	// EnvPort is the environment variable used to override the default port.
	EnvPort = "RESOURCE_ANNOTATOR_PORT"
	// EnvCertFile is the environment variable used to override the default certificate file.
	EnvCertFile = "RESOURCE_ANNOTATOR_CERT_FILE"
	// EnvKeyFile is the environment variable used to override the default key file.
	EnvKeyFile = "RESOURCE_ANNOTATOR_KEY_FILE"
)

// GetConfig acquires configuration from the environment and arguments.
func GetConfig(args []string) (*Config, error) {
	var (
		cfg = &Config{
			Port:     DefaultPort,
			CertFile: DefaultCertFile,
			KeyFile:  DefaultKeyFile,
		}
		pendingErr error
	)

	if v, ok := os.LookupEnv(EnvPort); ok {
		cfg.Port = 0
		i, err := strconv.ParseUint(v, 10, 16)
		if err != nil {
			pendingErr = fmt.Errorf("invalid port in environment %s=%s: %w", EnvPort, v, err)
		}
		cfg.Port = uint(i)
	}
	if v, ok := os.LookupEnv(EnvCertFile); ok {
		cfg.CertFile = v
	}
	if v, ok := os.LookupEnv(EnvKeyFile); ok {
		cfg.KeyFile = v
	}

	flag.UintVar(&cfg.Port, "port", cfg.Port, "HTTPS port to listen on")
	flag.StringVar(&cfg.CertFile, "cert-file", cfg.CertFile, "TLS certificate file")
	flag.StringVar(&cfg.KeyFile, "key-file", cfg.KeyFile, "TLS certificate key file")
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)

	err := flag.CommandLine.Parse(args)
	if err == flag.ErrHelp {
		os.Exit(0)
	}

	if err == nil && pendingErr != nil && cfg.Port == 0 {
		return nil, pendingErr
	}

	if err != nil {
		return nil, err
	}

	return cfg, nil
}
