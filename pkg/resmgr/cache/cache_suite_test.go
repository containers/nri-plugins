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

package cache_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/topology"
)

var (
	testdataDir     string
	enableDebugging bool = false

	cpulist  string = "0-7"
	numanode string = "0"
)

func TestMain(m *testing.M) {
	pwd, err := os.Getwd()
	if err != nil {
		panic(fmt.Errorf("unable to get current directory: %w", err))
	}

	if path, err := filepath.EvalSymlinks(pwd); err == nil {
		pwd = path
	}

	l := log.Get("topology-hints")
	topology.SetLogger(l)

	c := log.Get("cache")

	if enableDebugging {
		l.EnableDebug(true)
		c.EnableDebug(true)
	}

	testdataDir = pwd + "/no-such-dir"
	topology.SetSysRoot(testdataDir)

	code := m.Run()

	topology.SetSysRoot("")
	os.Exit(code)
}

var (
	podID = &IdGen{
		base: "pod",
	}
	ctrID = &IdGen{
		base: "ctr",
	}
)

type IdGen struct {
	sync.Mutex
	base   string
	nextID int
}

func (g *IdGen) Generate() string {
	g.Lock()
	defer g.Unlock()
	g.nextID++
	id := fmt.Sprintf("%s-#%d", g.base, g.nextID)
	return id
}
