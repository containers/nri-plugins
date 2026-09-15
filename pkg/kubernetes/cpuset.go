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

package kubernetes

import (
	"strconv"
	"strings"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

type segment struct {
	beg, end, step int
}

func (s segment) String() string {
	if s.beg < 0 {
		return ""
	}
	if s.end < 0 {
		return strconv.FormatInt(int64(s.beg), 10)
	}
	if s.step == 1 {
		return strconv.FormatInt(int64(s.beg), 10) + "-" + strconv.FormatInt(int64(s.end), 10)
	}
	return strconv.FormatInt(int64(s.beg), 10) + "-" + strconv.FormatInt(int64(s.end), 10) + ":" + strconv.FormatInt(int64(s.step), 10)
}

// ShortCPUSet prints the cpuset as a string, trying to further shorten compared to .String().
func ShortCPUSet(cset libcpu.CPUSet) string {
	segments := []segment{{beg: -1}}
	for _, part := range strings.Split(cset.String(), ",") {
		if part == "" {
			continue
		}
		curr := len(segments) - 1
		if strings.Contains(part, "-") {
			parts := strings.SplitN(part, "-", 2)
			beg, _ := strconv.Atoi(parts[0])
			end, _ := strconv.Atoi(parts[1])
			seg := segment{beg: beg, end: end, step: 1}
			if segments[curr].beg < 0 {
				segments[curr] = seg
			} else {
				segments = append(segments, seg)
			}
			segments = append(segments, segment{beg: -1})
			continue
		}
		cpu, _ := strconv.Atoi(part)
		if segments[curr].beg < 0 {
			segments[curr] = segment{beg: cpu, end: -1}
			continue
		}
		if segments[curr].end < 0 {
			segments[curr].end = cpu
			segments[curr].step = segments[curr].end - segments[curr].beg
			continue
		}
		if cpu-segments[curr].end == segments[curr].step {
			segments[curr].end = cpu
			continue
		}
		segments = append(segments, segment{beg: cpu, end: -1})
	}

	str := strings.Builder{}
	sep := ""
	for _, seg := range segments {
		part := seg.String()
		if part == "" {
			continue
		}
		str.WriteString(sep)
		str.WriteString(part)
		sep = ","
	}
	return str.String()
}
