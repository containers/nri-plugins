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

package steering

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const numaStat = `anon N0=4096 N1=2912256 N2=0
file N0=1048576 N1=0 N2=8192
kernel_stack N0=16384 N1=0 N2=0
shmem N0=0 N1=100 N2=200
anon_thp N0=0 N1=2097152 N2=0
`

func TestParseNumaStat(t *testing.T) {
	usage, err := parseNumaStat(strings.NewReader(numaStat), []string{"anon", "shmem"})
	require.NoError(t, err)
	require.Equal(t, NodeMem{0: 4096, 1: 2912356, 2: 200}, usage)

	usage, err = parseNumaStat(strings.NewReader(numaStat), []string{"missing"})
	require.NoError(t, err)
	require.Empty(t, usage)

	_, err = parseNumaStat(strings.NewReader("anon N0=4096 N1\n"), []string{"anon"})
	require.Error(t, err)

	_, err = parseNumaStat(strings.NewReader("anon N0=4096 X1=2\n"), []string{"anon"})
	require.Error(t, err)
}

func TestParseMemoryEvents(t *testing.T) {
	events, err := parseMemoryEvents(strings.NewReader("low 0\nhigh 42\nmax 1\noom 0\noom_kill 0\n"))
	require.NoError(t, err)
	require.Equal(t, int64(42), events["high"])
	require.Equal(t, int64(1), events["max"])

	_, err = parseMemoryEvents(strings.NewReader("high forty-two\n"))
	require.Error(t, err)
}

func TestCgroupFileReaders(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.current"), []byte("123456\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.numa_stat"), []byte(numaStat), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("1\n\n22\n"), 0644))

	current, err := memoryCurrent(dir)
	require.NoError(t, err)
	require.Equal(t, int64(123456), current)

	usage, err := numaUsage(dir, "anon")
	require.NoError(t, err)
	require.Equal(t, NodeMem{0: 4096, 1: 2912256, 2: 0}, usage)

	pids, err := procs(dir)
	require.NoError(t, err)
	require.Equal(t, []int{1, 22}, pids)

	_, err = memoryCurrent(filepath.Join(dir, "missing"))
	require.Error(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("1\nabc\n"), 0644))
	_, err = procs(dir)
	require.Error(t, err)
}

// TestNotifierWait checks the events of a notifier over regular files:
// the kernel's "high" counter, the lower bound by memory.current, a
// timeout, Interrupt and a removed cgroup.
func TestNotifierWait(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(data+"\n"), 0644))
	}
	events := func(high int) {
		t.Helper()
		write("memory.events", fmt.Sprintf("low 0\nhigh %d\nmax 0\noom 0\noom_kill 0", high))
	}
	write("memory.high", "max")
	write("memory.current", "1000")
	events(0)
	n, err := newNotifier(dir)
	require.NoError(t, err)
	defer func() { require.NoError(t, n.Close()) }()

	require.NoError(t, n.SetBounds(bounds{Lower: 500, Upper: 2000}))
	data, err := os.ReadFile(filepath.Join(dir, "memory.high"))
	require.NoError(t, err)
	require.Equal(t, "2000\n", string(data))
	ev, err := n.Wait(0)
	require.NoError(t, err)
	require.Equal(t, event{crossing: noCrossing, memoryCurrent: 1000}, ev)

	// the kernel throttled the cgroup at the bound
	events(1)
	ev, err = n.Wait(0)
	require.NoError(t, err)
	require.Equal(t, upperCrossed, ev.crossing)
	// until the next SetBounds
	ev, err = n.Wait(0)
	require.NoError(t, err)
	require.Equal(t, upperCrossed, ev.crossing)
	require.NoError(t, n.SetBounds(bounds{Lower: 500, Upper: 2000}))
	ev, err = n.Wait(0)
	require.NoError(t, err)
	require.Equal(t, noCrossing, ev.crossing)

	write("memory.current", "400")
	ev, err = n.Wait(0)
	require.NoError(t, err)
	require.Equal(t, event{crossing: lowerCrossed, memoryCurrent: 400}, ev)

	require.NoError(t, n.Interrupt())
	_, err = n.Wait(-1)
	require.ErrorIs(t, err, errInterrupted)

	// no bounds: memory.high max
	require.NoError(t, n.SetBounds(bounds{}))
	data, err = os.ReadFile(filepath.Join(dir, "memory.high"))
	require.NoError(t, err)
	require.Equal(t, "max\n", string(data))

	require.NoError(t, os.Remove(filepath.Join(dir, "memory.current")))
	_, err = n.Wait(0)
	require.True(t, gone(err), "%v", err)
}
