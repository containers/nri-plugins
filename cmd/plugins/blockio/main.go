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
	"flag"
	"log/slog"
	"os"

	"github.com/containerd/nri/pkg/stub"
	"github.com/intel/goresctrl/pkg/blockio"
	"github.com/sirupsen/logrus"
)

var log = logrus.StandardLogger()

func main() {
	var (
		pluginName  string
		pluginIdx   string
		configFile  string
		verbose     bool
		veryVerbose bool
	)
	flag.StringVar(&pluginName, "name", "", "plugin name to register to NRI")
	flag.StringVar(&pluginIdx, "idx", "", "plugin index to register to NRI")
	flag.StringVar(&configFile, "config", "", "block I/O class configuration file in goresctrl YAML format")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.BoolVar(&veryVerbose, "vv", false, "very verbose output")
	flag.Parse()

	log.SetFormatter(&logrus.TextFormatter{PadLevelText: true})
	level := slog.LevelInfo
	if verbose {
		log.SetLevel(logrus.DebugLevel)
		level = slog.LevelDebug
	}
	if veryVerbose {
		log.SetLevel(logrus.TraceLevel)
		level = slog.LevelDebug
	}
	blockio.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	p := &plugin{configFile: configFile}
	if configFile != "" {
		if err := p.loadConfig(); err != nil {
			log.Fatal(err)
		}
	}

	opts := []stub.Option{stub.WithOnClose(func() {
		log.Info("Connection to the runtime lost, exiting")
		os.Exit(0)
	})}
	if pluginName != "" {
		opts = append(opts, stub.WithPluginName(pluginName))
	}
	if pluginIdx != "" {
		opts = append(opts, stub.WithPluginIdx(pluginIdx))
	}
	s, err := stub.New(p, opts...)
	if err != nil {
		log.Fatalf("failed to create plugin stub: %v", err)
	}
	if err := s.Run(context.Background()); err != nil {
		log.Errorf("plugin exited: %v", err)
		os.Exit(1)
	}
}
