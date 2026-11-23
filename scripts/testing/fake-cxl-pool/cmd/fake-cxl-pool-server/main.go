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

// fake-cxl-pool-server serves CXL memory devices (backing files) to qemu
// VMs on this host: it hotplugs and hot-removes cxl-type3 devices over the
// QMP/HMP monitors of running qemus. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/server"
)

// discoveryInfo returns the discovery names, control and slots of a
// configuration that are set, as ", discovery ..." clauses.
func discoveryInfo(d *server.DiscoveryConfig) string {
	s := ""
	if len(d.Names) > 0 {
		s += fmt.Sprintf(", discovery names %v", d.Names)
	}
	if d.Control != "" {
		s += ", discovery control " + d.Control
	}
	if len(d.Slots) > 0 {
		s += fmt.Sprintf(", discovery slots %v", d.Slots)
	}
	return s
}

func main() {
	configFile := flag.String("config", "", "YAML config file (default: built-in defaults)")
	stateFile := flag.String("state", "", "state file (default: <dir>.state.json of the config; \"-\": no persistence)")
	verbose := flag.Bool("v", false, "log every qemu command and http request")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-config FILE] [-state FILE] [-v]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	logger := log.New(os.Stderr, "fake-cxl-pool: ", log.LstdFlags|log.Lmicroseconds)

	var (
		cfg *server.Config
		err error
	)
	if *configFile != "" {
		cfg, err = server.LoadConfig(*configFile)
		if err != nil {
			logger.Fatalf("%v", err)
		}
	} else {
		cfg = server.DefaultConfig()
	}
	if *stateFile != "" {
		cfg.StateFile = *stateFile
	}

	srv, err := server.New(cfg, server.Options{Logger: logger, Verbose: *verbose})
	if err != nil {
		logger.Fatalf("%v", err)
	}
	l, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Fatalf("%v", err)
	}
	logger.Printf("fake-cxl-pool-server %s listening on http://%s (state file %s%s)", cfg.Name, l.Addr(), cfg.StateFile, discoveryInfo(&cfg.Discovery))
	srv.Start()
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	// Shutdown waits for every request, an event stream too.
	hs.RegisterOnShutdown(srv.CloseEvents)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(l) }()
	select {
	case <-ctx.Done():
		logger.Printf("shutting down")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("http: %v", err)
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	srv.Stop()
}
