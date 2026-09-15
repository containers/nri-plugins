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

// This is a helper for the tests of the coverage package, and handy for
// poking at the endpoints by hand. runtime/coverage, which the package is
// built around, only works in binaries built with go build -cover, never in
// a test binary. So the tests build and run this program to check that we
// serve coverage data which the go tools can digest.
//
// Print the address of the server, then serve until terminated. Terminate
// with SIGTERM to get the coverage data written to $GOCOVERDIR, too.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	xhttp "github.com/containers/nri-plugins/pkg/http"
	"github.com/containers/nri-plugins/pkg/instrumentation/coverage"
)

func main() {
	srv := xhttp.NewServer()
	coverage.Setup(srv.GetMux())

	if err := srv.Start("127.0.0.1:0"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start server: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(srv.GetAddress())

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs

	srv.Stop()
}
