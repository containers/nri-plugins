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

// Command cxl-request requests CXL memory for the running container, of
// the node's local memdevs or of a memory pool, from the CXL DRA driver
// through the lease socket $CXL_LEASE_SOCKET, which the driver mounts into
// containers of claims on DeviceClass cxl-lease. See
// docs/memory/cxl/leases.md.
//
//	cxl-request request [--source local|pool] [--mode numa|dax|fs|block] [--share]
//	                    [--read-only[=false]] [--access direct|cached] [--fs-type TYPE]
//	                    [--name NAME] [--size SIZE] [--serial SERIAL] [--wait DURATION]
//	                    [--request-id ID] [--exec CMD [ARG...]]
//	cxl-request release (--lease ID | --all) [--wait DURATION]
//	cxl-request list
//	cxl-request env --lease ID [--wait DURATION] [--exec CMD [ARG...]]
//	cxl-request help
//
// request, release and list print the driver's JSON response line, env
// prints the environment of the lease as sorted KEY=VALUE lines. With
// --exec CMD runs with the environment of the lease, and with the fd of a
// dax lease (a dax device) or a block lease (the block device of a
// persistent device) as fd 3; an fs lease has its filesystem at
// CXL_<KEY>_PATH. env --exec of a dax or block lease gets its fd again,
// in the container that requested the lease only. Exit codes: 0 ok, 1
// error, 2 usage, 3 the lease is Failed, not Ready (request, env) or not
// Released (release) within --wait. With --exec the exit code of CMD is
// passed through.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/api"
)

const (
	exitOK         = 0
	exitError      = 1
	exitUsage      = 2
	exitLeaseState = 3 // failed, not Ready or not Released in time
	execFd         = 3 // the fd of a dax or block lease in the command
)

const usage = `usage:
  cxl-request request [--source local|pool] [--mode numa|dax|fs|block] [--share]
                      [--read-only[=false]] [--access direct|cached] [--fs-type TYPE]
                      [--name NAME] [--size SIZE] [--serial SERIAL] [--wait DURATION]
                      [--request-id ID] [--exec CMD [ARG...]]
  cxl-request release (--lease ID | --all) [--wait DURATION]
  cxl-request list
  cxl-request env --lease ID [--wait DURATION] [--exec CMD [ARG...]]
  cxl-request help
request: memory for this container, the whole device unless --share:
  --source    local (a memdev of this node) or pool; default local, then pool
  --mode      numa (lent as a NUMA node), dax (a dax device fd), fs (the
              filesystem of a persistent device at /run/cxl/fs/NAME) or
              block (an fd of the block device of a persistent device);
              default numa, dax with --share or for a shared device
  --share     one share of a shared device (default: the whole device)
  --read-only a dax or block fd that allows reads only; fs is read-only
              unless --read-only=false
  --access    fs: direct (file pages are the memory of the device) or
              cached (the page cache of the node); default direct when the
              filesystem has DAX, cached otherwise
  --fs-type   fs: the expected filesystem type (default: detected)
  --name      fs: the mount point /run/cxl/fs/NAME; default the lease id
  --size      at least this much, a quantity such as 512Mi
  --serial    this device (0x hex or decimal)
  --wait      until Ready (Go duration; 0s returns at once); default: the
              driver's --lease-wait; release: until Released
  --request-id the same id returns the same lease
  --exec      run CMD with the lease's environment and a dax or block lease's fd 3
release, env: --lease ID of a lease of this pod; release --all: all of them.
env --exec of a dax or block lease: in the container that requested it.
The socket is $CXL_LEASE_SOCKET.
`

// options lists the options of each operation; --exec is the last one.
var options = map[string][]string{
	api.OpRequest: {"--source", "--mode", "--share", "--read-only", "--access", "--fs-type", "--name", "--size", "--serial", "--wait",
		"--request-id", "--exec"},
	api.OpRelease: {"--lease", "--all", "--wait"},
	api.OpList:    {},
	api.OpEnv:     {"--lease", "--wait", "--exec"},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// command is a parsed command line.
type command struct {
	req  api.Request
	exec []string // nil without --exec
}

// usageErr prints a usage error and the usage, and returns the exit code
// of usage errors.
func usageErr(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "cxl-request: %v\n", err)
	_, _ = fmt.Fprint(stderr, usage)
	return exitUsage
}

// parseArgs returns the command of the arguments after the program name:
// the operation, then its options. --exec ends the options: the rest is
// the command to run.
func parseArgs(args []string) (*command, error) {
	if len(args) == 0 {
		return nil, errors.New("no operation")
	}
	cmd := &command{req: api.Request{Op: args[0]}}
	if _, ok := options[cmd.req.Op]; !ok {
		return nil, fmt.Errorf("unknown operation %q", cmd.req.Op)
	}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		name, val, inline := strings.Cut(rest[i], "=")
		if err := cmd.checkOption(name, rest[i]); err != nil {
			return nil, err
		}
		if name == "--exec" {
			if inline || i+1 >= len(rest) {
				return nil, errors.New("--exec needs a command")
			}
			cmd.exec = rest[i+1:]
			break
		}
		if !inline && !cmd.isFlag(name) {
			if i+1 >= len(rest) {
				return nil, fmt.Errorf("%s needs a value", name)
			}
			i++
			val = rest[i]
		}
		if err := cmd.setOption(name, val, inline); err != nil {
			return nil, err
		}
	}
	return cmd, cmd.req.Validate()
}

// values returns the options with a value of the command.
func (cmd *command) values() map[string]*string {
	return map[string]*string{
		"--source": &cmd.req.Source, "--mode": &cmd.req.Mode, "--access": &cmd.req.Access, "--fs-type": &cmd.req.FSType, "--name": &cmd.req.Name,
		"--size": &cmd.req.Size, "--serial": &cmd.req.Serial,
		"--wait": &cmd.req.Wait, "--request-id": &cmd.req.RequestID, "--lease": &cmd.req.Lease,
	}
}

// flags returns the setters of the options without a value of the
// command: true, or the value given inline (--read-only=false).
func (cmd *command) flags() map[string]func(bool) {
	return map[string]func(bool){
		"--share":     func(b bool) { cmd.req.Share = b },
		"--read-only": func(b bool) { cmd.req.ReadOnly = &b },
		"--all":       func(b bool) { cmd.req.All = b },
	}
}

// isFlag tells whether an option takes no value.
func (cmd *command) isFlag(name string) bool {
	return cmd.flags()[name] != nil
}

// checkOption returns an error if arg, an option name, is not an option of
// the operation of the command.
func (cmd *command) checkOption(name, arg string) error {
	switch {
	case !strings.HasPrefix(name, "--"):
		return fmt.Errorf("unexpected argument %q", arg)
	case cmd.values()[name] == nil && !cmd.isFlag(name) && name != "--exec":
		return fmt.Errorf("unknown option %s", name)
	case name == "--exec" && (cmd.req.Op == api.OpRelease || cmd.req.Op == api.OpList):
		return errors.New("--exec is for request and env")
	case !slices.Contains(options[cmd.req.Op], name):
		return fmt.Errorf("%s is not an option of %s", name, cmd.req.Op)
	}
	return nil
}

// setOption sets an option of the command: a flag to true, or to val if
// it was given inline (--opt=VAL), an option with a value to val.
func (cmd *command) setOption(name, val string, inline bool) error {
	set := cmd.flags()[name]
	switch {
	case set != nil && inline:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("%s: invalid value %q", name, val)
		}
		set(b)
	case set != nil:
		set(true)
	default:
		*cmd.values()[name] = val
	}
	return nil
}

// run runs the command line of the arguments after the program name and
// returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		_, _ = fmt.Fprint(stdout, usage)
		return exitOK
	}
	cmd, err := parseArgs(args)
	if err != nil {
		return usageErr(stderr, err)
	}
	sock := os.Getenv(api.SocketEnv)
	if sock == "" {
		_, _ = fmt.Fprintf(stderr, "cxl-request: %s is not set (the pod needs a claim on DeviceClass cxl-lease)\n", api.SocketEnv)
		return exitError
	}
	resp, line, fd, err := call(sock, &cmd.req)
	if err == nil && cmd.exec != nil && cmd.req.Op == api.OpEnv && exitCode(&cmd.req, resp) == exitOK && hasFd(resp.Mode) {
		// The fd of a dax or block lease comes with op fd, which the
		// driver answers in the container that requested the lease.
		closeFd(fd)
		resp, line, fd, err = call(sock, &api.Request{Op: api.OpFd, Lease: cmd.req.Lease, Wait: "0s"})
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cxl-request: %v\n", err)
		return exitError
	}
	if cmd.exec == nil {
		closeFd(fd)
		return output(cmd, resp, line, stdout, stderr)
	}
	return execute(cmd, resp, line, fd, stdout, stderr)
}

// call sends the request and returns its response, the response line and
// the fd that came with it (-1 for none).
func call(sock string, req *api.Request) (*api.Response, []byte, int, error) {
	line, fd, err := roundTrip(sock, req)
	if err != nil {
		return nil, nil, -1, err
	}
	var resp api.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		closeFd(fd)
		return nil, nil, -1, fmt.Errorf("invalid response %q: %v", line, err)
	}
	return &resp, line, fd, nil
}

// output prints the response of a command without --exec, and returns the
// exit code: env prints the environment of a Ready lease, the other
// operations the response line.
func output(cmd *command, resp *api.Response, line []byte, stdout, stderr io.Writer) int {
	code := exitCode(&cmd.req, resp)
	switch {
	case cmd.req.Op != api.OpEnv:
		_, _ = fmt.Fprintf(stdout, "%s\n", bytes.TrimSpace(line))
	case code == exitOK:
		for _, kv := range api.ExecEnv(resp.Env, resp.Lease, -1) {
			_, _ = fmt.Fprintln(stdout, kv)
		}
	default:
		_, _ = fmt.Fprintf(stderr, "cxl-request: %s\n", bytes.TrimSpace(line))
	}
	return code
}

// execute runs the command of --exec with the environment of a Ready
// lease, and its fd for a dax or block lease, and returns its exit code.
func execute(cmd *command, resp *api.Response, line []byte, fd int, stdout, stderr io.Writer) int {
	code := exitCode(&cmd.req, resp)
	if code == exitOK && resp.State != api.StateReady {
		code = exitLeaseState // CMD needs the memory
	}
	if code != exitOK {
		closeFd(fd)
		_, _ = fmt.Fprintf(stderr, "cxl-request: %s\n", bytes.TrimSpace(line))
		return code
	}
	envFd := -1
	switch {
	case !hasFd(resp.Mode):
		closeFd(fd)
		fd = -1
	case fd < 0:
		_, _ = fmt.Fprintf(stderr, "cxl-request: no fd with the response of %s lease %s\n", resp.Mode, resp.Lease)
		return exitError
	default:
		envFd = execFd
	}
	return runCommand(cmd.exec, fd, api.ExecEnv(resp.Env, resp.Lease, envFd), stdout, stderr)
}

// exitCode returns the exit code of a response.
func exitCode(req *api.Request, resp *api.Response) int {
	switch {
	case resp.State == api.StateFailed:
		return exitLeaseState
	case resp.Error != "":
		return exitError
	}
	switch req.Op {
	case api.OpRequest:
		if d, err := time.ParseDuration(req.Wait); resp.State == api.StateReady || (err == nil && d == 0) {
			return exitOK
		}
		return exitLeaseState
	case api.OpEnv:
		if resp.State != api.StateReady {
			return exitLeaseState
		}
	case api.OpRelease:
		return releaseExitCode(resp)
	}
	return exitOK
}

// hasFd tells whether a lease of the mode passes an fd: dax and block
// leases do.
func hasFd(mode string) bool {
	return mode == api.ModeDax || mode == api.ModeBlock
}

// releaseExitCode returns the exit code of a response to release: ok when
// the lease, or every lease, is Released.
func releaseExitCode(resp *api.Response) int {
	all := []api.Response{*resp}
	if resp.Leases != nil {
		all = *resp.Leases
	}
	for _, l := range all {
		if l.State != api.StateReleased {
			return exitLeaseState
		}
	}
	return exitOK
}

// roundTrip sends the request and reads one response line with the fd
// that came with it (-1 for none).
func roundTrip(sock string, req *api.Request) ([]byte, int, error) {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return nil, -1, err
	}
	defer func() { _ = c.Close() }()
	data, err := json.Marshal(req)
	if err != nil {
		return nil, -1, err
	}
	if _, err := c.Write(append(data, '\n')); err != nil {
		return nil, -1, err
	}
	return readResponse(c)
}

// readResponse reads a line; the fd arrives with its first bytes.
func readResponse(c *net.UnixConn) ([]byte, int, error) {
	fd := -1
	buf := make([]byte, 64*1024)
	oob := make([]byte, syscall.CmsgSpace(4*4))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil && n == 0 {
		return nil, -1, fmt.Errorf("reading response: %w", err)
	}
	if oobn > 0 {
		fd, err = parseFd(oob[:oobn])
		if err != nil {
			return nil, -1, err
		}
	}
	line := buf[:n]
	if !bytes.Contains(line, []byte{'\n'}) {
		rest, err := bufio.NewReader(c).ReadBytes('\n')
		line = append(line, rest...)
		if err != nil && !bytes.Contains(line, []byte{'\n'}) {
			closeFd(fd)
			return nil, -1, fmt.Errorf("reading response: %w", err)
		}
	}
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return line, fd, nil
}

// parseFd returns the first fd of SCM_RIGHTS messages and closes others.
func parseFd(oob []byte) (int, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, fmt.Errorf("control message: %w", err)
	}
	fd := -1
	for i := range msgs {
		fds, err := syscall.ParseUnixRights(&msgs[i])
		if err != nil {
			continue
		}
		for _, f := range fds {
			if fd < 0 {
				fd = f
			} else {
				_ = syscall.Close(f)
			}
		}
	}
	return fd, nil
}

func closeFd(fd int) {
	if fd >= 0 {
		_ = syscall.Close(fd)
	}
}

// runCommand runs the command with env added to the environment and fd as
// fd 3 (none if fd < 0), forwards signals, and returns its exit code. (A
// fork rather than an in-place exec: dup2 onto fd 3 could clobber a file
// the Go runtime holds there.)
func runCommand(argv []string, fd int, env []string, stdout, stderr io.Writer) int {
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, stdout, stderr
	c.Env = append(os.Environ(), env...)
	var f *os.File
	if fd >= 0 {
		f = os.NewFile(uintptr(fd), "lease")
		defer func() { _ = f.Close() }()
		c.ExtraFiles = []*os.File{f} // fd 3
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	if err := c.Start(); err != nil {
		_, _ = fmt.Fprintf(stderr, "cxl-request: %v\n", err)
		return exitError
	}
	if f != nil {
		_ = f.Close() // the child has it
	}
	go func() {
		for s := range sigs {
			_ = c.Process.Signal(s)
		}
	}()
	err := c.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	default:
		_, _ = fmt.Fprintf(stderr, "cxl-request: %v\n", err)
		return exitError
	}
}
