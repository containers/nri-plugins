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
	"flag"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/intel/nri-resmgr/pkg/config"
	"github.com/intel/nri-resmgr/pkg/instrumentation"
	"github.com/intel/nri-resmgr/pkg/resmgr"

	logger "github.com/intel/nri-resmgr/pkg/log"
	version "github.com/intel/nri-resmgr/pkg/version"

	_ "github.com/intel/nri-resmgr/cmd/balloons/policy"
)

var log = logger.Default()

func main() {
	rate := logger.Rate{Limit: logger.Every(1 * time.Minute)}
	logger.SetGrpcLogger("grpc", &rate)
	logger.SetStdLogger("stdlog")

	printConfig := flag.Bool("print-config", false, "Print configuration and exit.")
	flag.Parse()

	switch {
	case *printConfig:
		config.Print(nil)
		os.Exit(0)

	default:
		if args := flag.Args(); len(args) > 0 {
			switch args[0] {
			case "config-help", "help":
				config.Describe(args[1:]...)
				os.Exit(0)
			default:
				log.Error("unknown command line arguments: %s", strings.Join(flag.Args(), ","))
				flag.Usage()
				os.Exit(1)
			}
		}
	}

	logger.Flush()
	logger.SetupDebugToggleSignal(syscall.SIGUSR1)
	log.Info("nri-resmgr (version %s, build %s) starting...", version.Version, version.Build)

	if err := instrumentation.Start(); err != nil {
		log.Fatal("failed to set up instrumentation: %v", err)
	}
	defer instrumentation.Stop()

	m, err := resmgr.NewResourceManager()
	if err != nil {
		log.Fatal("failed to create resource manager instance: %v", err)
	}

	if err := m.Start(); err != nil {
		log.Fatal("failed to start resource manager: %v", err)
	}

	for {
		time.Sleep(15 * time.Second)
	}
}
