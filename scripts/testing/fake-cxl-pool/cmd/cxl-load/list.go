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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxListItem bounds the numbers of a CPU or node list.
const maxListItem = 1 << 16

// parseList returns the numbers of a list like "1,3-5", sorted and without
// duplicates.
func parseList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty list")
	}
	seen := map[int]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		lo, hi, isRange := strings.Cut(item, "-")
		first, err := parseNumber(lo)
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", s, err)
		}
		last := first
		if isRange {
			if last, err = parseNumber(hi); err != nil {
				return nil, fmt.Errorf("list %q: %w", s, err)
			}
			if last < first {
				return nil, fmt.Errorf("list %q: range %s ends before it starts", s, item)
			}
		}
		for n := first; n <= last; n++ {
			seen[n] = true
		}
	}
	numbers := make([]int, 0, len(seen))
	for n := range seen {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	return numbers, nil
}

// parseNumber returns the number s, a CPU or node number.
func parseNumber(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 || n >= maxListItem {
		return 0, fmt.Errorf("%q is not a number from 0 to %d", s, maxListItem-1)
	}
	return n, nil
}

// formatList returns sorted numbers as a list with ranges, like "1,3-5".
func formatList(numbers []int) string {
	var b strings.Builder
	for i := 0; i < len(numbers); {
		j := i
		for j+1 < len(numbers) && numbers[j+1] == numbers[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		if j == i {
			fmt.Fprintf(&b, "%d", numbers[i])
		} else {
			fmt.Fprintf(&b, "%d-%d", numbers[i], numbers[j])
		}
		i = j + 1
	}
	return b.String()
}

// parseSize returns the number of bytes of a size like "4096", "256K",
// "64KiB", "512M" or "2G": the suffixes K, M and G (with an optional "iB"
// or "B") are powers of 1024.
func parseSize(s string) (int, error) {
	orig := s
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "B")
	s = strings.TrimSuffix(s, "i")
	unit := 1
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K', 'k':
			unit = 1 << 10
		case 'M', 'm':
			unit = 1 << 20
		case 'G', 'g':
			unit = 1 << 30
		}
		if unit > 1 {
			s = s[:n-1]
		}
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a size (a positive number with an optional K, M or G)", orig)
	}
	if n > (1<<62)/unit {
		return 0, fmt.Errorf("size %q is too large", orig)
	}
	return n * unit, nil
}

// formatSize returns bytes as a size with the largest binary unit that
// divides it, like "512 MiB".
func formatSize(bytes int) string {
	for _, u := range []struct {
		unit int
		name string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if bytes%u.unit == 0 {
			return fmt.Sprintf("%d %s", bytes/u.unit, u.name)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}

// formatDuration returns d in ASCII, like "20us" for 20 microseconds.
func formatDuration(d time.Duration) string {
	return strings.ReplaceAll(d.String(), "\u00b5", "u")
}
