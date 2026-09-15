// Copyright 2020 Intel Corporation. All Rights Reserved.
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

package utils

import (
	"os"
	"strconv"
	"strings"
)

// GetMemoryCapacity parses memory capacity from /proc/meminfo (mimicking
// cAdvisor). It returns -1 if the amount cannot be determined.
//
// Note that this reads the real /proc/meminfo, not one below any host root.
func GetMemoryCapacity() int64 {
	var (
		data []byte
		err  error
		capa int64
	)

	if data, err = os.ReadFile("/proc/meminfo"); err != nil {
		return -1
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		keyval := strings.Split(line, ":")
		if len(keyval) != 2 || keyval[0] != "MemTotal" {
			continue
		}

		valunit := strings.Split(strings.TrimSpace(keyval[1]), " ")
		if len(valunit) != 2 || valunit[1] != "kB" {
			return -1
		}

		capa, err = strconv.ParseInt(valunit[0], 10, 64)
		if err != nil {
			return -1
		}

		capa *= 1024
		break
	}

	return capa
}
