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

// cxl-numa-place shows on which NUMA nodes of the host the pages of a
// file are: the backing files of fake-cxl-pool devices, which the qemu of
// every VM that attaches a device maps.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `Usage: cxl-numa-place COMMAND [-o table|json] FILE...

Commands:
  where FILE...                 print the resident pages of each file per NUMA node

Only the pages of a file that are in memory count: holes and pages on disk
stay as they are. Sizes are in MiB. Exit codes: 0 ok, 1 error, 2 usage.
`

// cli is the command line of cxl-numa-place.
type cli struct {
	stdout, stderr io.Writer
	output         string
}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

func main() {
	c := &cli{stdout: os.Stdout, stderr: os.Stderr}
	os.Exit(c.run(os.Args[1:]))
}

// run runs the command line args and returns the exit code.
func (c *cli) run(args []string) int {
	err := c.dispatch(args)
	var ue *usageError
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		_, _ = fmt.Fprint(c.stdout, usage)
		return exitOK
	case errors.As(err, &ue):
		_, _ = fmt.Fprintf(c.stderr, "cxl-numa-place: %s\n\n%s", ue.msg, usage)
		return exitUsage
	default:
		_, _ = fmt.Fprintf(c.stderr, "cxl-numa-place: %v\n", err)
		return exitError
	}
}

// dispatch runs the command of args.
func (c *cli) dispatch(args []string) error {
	if len(args) == 0 {
		return usagef("no command")
	}
	switch args[0] {
	case "where":
		return c.cmdWhere(args[1:])
	case "-h", "--help", "help":
		return flag.ErrHelp
	default:
		return usagef("unknown command %q", args[0])
	}
}

// flagSet returns the flags of a command, with -o.
func (c *cli) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.output, "o", "table", "output format: table or json")
	return fs
}

// parse parses the flags of fs in args, before and after the files, and
// returns the files.
func (c *cli) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var files []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usagef("%s: %v", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		files = append(files, args[0])
		args = args[1:]
	}
	if c.output != "table" && c.output != "json" {
		return nil, usagef("-o %q: the output is table or json", c.output)
	}
	if len(files) == 0 {
		return nil, usagef("%s: no FILE", fs.Name())
	}
	return files, nil
}

func (c *cli) cmdWhere(args []string) error {
	files, err := c.parse(c.flagSet("where"), args)
	if err != nil {
		return err
	}
	var ps []*Placement
	for _, f := range files {
		p, err := where(f)
		if err != nil {
			return err
		}
		ps = append(ps, p)
	}
	return c.write(ps)
}

// write writes the placements in the output format.
func (c *cli) write(ps []*Placement) error {
	if c.output == "json" {
		return writeJSON(c.stdout, ps)
	}
	return writeTable(c.stdout, ps)
}
