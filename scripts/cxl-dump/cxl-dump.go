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
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/containers/nri-plugins/pkg/cxl"
	"sigs.k8s.io/yaml"
)

func main() {
	format := flag.String("o", "yaml", "output format: yaml or json")
	sysfsRoot := flag.String("sysfs-root", os.Getenv("SYSFS_ROOT"), "prefix for sysfs and proc paths, SYSFS_ROOT by default")
	flag.Parse()

	if *format != "yaml" && *format != "json" {
		fmt.Fprintf(os.Stderr, "invalid output format %q, expected yaml or json\n", *format)
		os.Exit(1)
	}

	devices, err := cxl.DevicesFromSysfs(*sysfsRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error reading CXL devices from sysfs:", err)
		os.Exit(1)
	}
	if err := dump(devices, *format); err != nil {
		fmt.Fprintln(os.Stderr, "Error dumping CXL devices:", err)
		os.Exit(1)
	}
}

func dump(devices *cxl.Devices, format string) error {
	var data []byte
	var err error
	switch format {
	case "yaml":
		data, err = yaml.Marshal(devices)
	case "json":
		data, err = json.MarshalIndent(devices, "", "  ")
	default:
		err = fmt.Errorf("invalid output format %q, expected yaml or json", format)
	}
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
