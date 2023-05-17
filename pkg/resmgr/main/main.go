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

package resmgr

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/containers/nri-plugins/pkg/config"
	"github.com/containers/nri-plugins/pkg/instrumentation"
	"github.com/containers/nri-plugins/pkg/resmgr"

	logger "github.com/containers/nri-plugins/pkg/log"
	version "github.com/containers/nri-plugins/pkg/version"
)

var (
	log = logger.Default()
)

type Main struct {
	policy string
	mgr    resmgr.ResourceManager
}

func New(policy string) (*Main, error) {
	m := &Main{
		policy: policy,
	}

	m.setupLoggers()
	m.parseCmdline()
	m.startTracing()

	mgr, err := resmgr.NewResourceManager()
	if err != nil {
		return nil, fmt.Errorf("failed to create resource manager: %w", err)
	}

	m.mgr = mgr

	return m, nil
}

func (m *Main) Run() error {
	log.Infof("starting '%s' policy version %s/build %s...", m.policy,
		version.Version, version.Build)

	defer m.stopTracing()

	if err := m.mgr.Start(); err != nil {
		return fmt.Errorf("failed to start resource manager: %w", err)
	}

	for {
		time.Sleep(15 * time.Second)
	}
}

func (m *Main) setupLoggers() {
	rate := logger.Rate{Limit: logger.Every(1 * time.Minute)}
	logger.SetGrpcLogger("grpc", &rate)
	logger.SetStdLogger("stdlog")
	logger.SetupDebugToggleSignal(syscall.SIGUSR1)
}

func (m *Main) parseCmdline() {
	printCfg := flag.Bool("print-config", false, "Print default configuration and exit.")
	flag.Parse()
	logger.Flush()

	switch {
	case *printCfg:
		config.Print(nil)
		os.Exit(0)
	}

	if args := flag.Args(); len(args) > 0 {
		switch args[0] {
		case "config-help", "help":
			config.Describe(args[1:]...)
			os.Exit(0)
		case "version":
			fmt.Printf("version: %s\n", version.Version)
			fmt.Printf("build: %s\n", version.Build)
			os.Exit(0)
		default:
			log.Errorf("unknown command line arguments: %s", strings.Join(args, " "))
			flag.Usage()
			os.Exit(1)
		}
	}
}

func (m *Main) startTracing() error {
	err := instrumentation.Start()
	if err != nil {
		return fmt.Errorf("failed to set up instrumentation: %v", err)
	}

	return nil
}

func (m *Main) stopTracing() {
	instrumentation.Stop()
}
