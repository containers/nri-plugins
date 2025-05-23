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

// mpolset is an executable that sets the memory policy for a process
// and then executes the specified command.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/containers/nri-plugins/pkg/mempolicy"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	"github.com/sirupsen/logrus"
)

type logrusFormatter struct{}

func (f *logrusFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	return fmt.Appendf(nil, "mpolset: %s %s\n", entry.Level, entry.Message), nil
}

var (
	log *logrus.Logger
)

func modeToString(mode uint) string {
	// Convert mode to string representation
	flagsStr := ""
	for name, value := range mempolicy.Flags {
		if mode&value != 0 {
			flagsStr += "|"
			flagsStr += name
			mode &= ^value
		}
	}
	modeStr := mempolicy.ModeNames[mode]
	if modeStr == "" {
		modeStr = fmt.Sprintf("unknown mode %d)", mode)
	}
	return modeStr + flagsStr
}

func main() {
	var err error

	log = logrus.StandardLogger()
	log.SetFormatter(&logrusFormatter{})

	modeFlag := flag.String("mode", "", "Memory policy mode. Valid values are mode numbers and names, e.g. 3 or MPOL_INTERLEAVE. List available modes with -mode help")
	flagsFlag := flag.String("flags", "", "Comma-separated list of memory policy flags,e.g. MPOL_F_STATIC_NODES. List available flags with -flags help")
	nodesFlag := flag.String("nodes", "", "Comma-separated list of nodes, e.g. 0,1-3")
	ignoreErrorsFlag := flag.Bool("ignore-errors", false, "Ignore errors when setting memory policy")
	verboseFlag := flag.Bool("v", false, "Enable verbose logging")
	veryVerboseFlag := flag.Bool("vv", false, "Enable very verbose logging")
	flag.Parse()

	log.SetLevel(logrus.InfoLevel)
	if *verboseFlag {
		log.SetLevel(logrus.DebugLevel)
	}
	if *veryVerboseFlag {
		log.SetLevel(logrus.TraceLevel)
	}

	execCmd := flag.Args()

	mode := uint(0)
	switch {
	case *modeFlag == "help":
		fmt.Printf("Valid memory policy modes:\n")
		for mode := range len(mempolicy.ModeNames) {
			fmt.Printf("  %s (%d)\n", mempolicy.ModeNames[uint(mode)], mode)
		}
		os.Exit(0)
	case *modeFlag != "" && (*modeFlag)[0] >= '0' && (*modeFlag)[0] <= '9':
		imode, err := strconv.Atoi(*modeFlag)
		if err != nil {
			log.Fatalf("invalid -mode: %v", err)
		}
		mode = uint(imode)
	case *modeFlag != "":
		ok := false
		mode, ok = mempolicy.Modes[*modeFlag]
		if !ok {
			log.Fatalf("invalid -mode: %v", *modeFlag)
		}
	case len(execCmd) > 0:
		log.Fatalf("missing -mode")
	}

	nodes := []int{}
	if *nodesFlag != "" {
		nodeMask, err := cpuset.Parse(*nodesFlag)
		if err != nil {
			log.Fatalf("invalid -nodes: %v", err)
		}
		nodes = nodeMask.List()
	}

	if *flagsFlag != "" {
		if strings.Contains(*flagsFlag, "help") {
			fmt.Printf("Valid memory policy flags:\n")
			for flag := range mempolicy.Flags {
				fmt.Printf("  %s\n", flag)
			}
			os.Exit(0)
		}
		flags := strings.Split(*flagsFlag, ",")
		for _, flag := range flags {
			flagBit, ok := mempolicy.Flags[flag]
			if !ok {
				log.Fatalf("invalid -flags: %v", flag)
			}
			mode |= flagBit
		}
	}

	if len(execCmd) == 0 {
		mode, nodes, err := mempolicy.GetMempolicy()
		if err != nil {
			log.Fatalf("GetMempolicy failed: %v", err)
		}
		modeStr := modeToString(mode)
		fmt.Printf("Current memory policy: %s (%d), nodes: %s\n", modeStr, mode, cpuset.New(nodes...).String())
		os.Exit(0)
	}

	log.Debugf("setting memory policy: %s (%d), nodes: %v\n", modeToString(mode), mode, cpuset.New(nodes...).String())
	if err := mempolicy.SetMempolicy(mode, nodes); err != nil {
		log.Errorf("SetMempolicy failed: %v", err)
		if ignoreErrorsFlag == nil || !*ignoreErrorsFlag {
			os.Exit(1)
		}
	}

	log.Debugf("executing: %v\n", execCmd)
	executable, err := exec.LookPath(execCmd[0])
	if err != nil {
		log.Fatalf("Looking for executable %q failed: %v", execCmd[0], err)
	}
	log.Tracef("- executable: %q\n", execCmd[0])
	log.Tracef("- environment: %v\n", os.Environ())
	err = syscall.Exec(executable, execCmd, os.Environ())
	if err != nil {
		log.Fatalf("Executing %q failed: %v", executable, err)
	}
}
