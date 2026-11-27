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

// cxl-dump prints the CXL topology of the node (kernel.ObserveTopology) as
// JSON.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

func main() {
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: cxl-dump")
		os.Exit(2)
	}
	snap, err := kernel.New().ObserveTopology(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "cxl-dump:", err)
		os.Exit(1)
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cxl-dump:", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}
