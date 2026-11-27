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

// fake-cxl-pool-client is the command line client of fake-cxl-pool. It
// talks to fake-cxl-pool-server, and in a VM it also prepares and releases
// hotplugged CXL memory (the "guest" commands).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/client"
)

// Exit codes.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitConflict = 3 // conflict or timeout: the device is still held
)

const usage = `Usage: fake-cxl-pool-client [--server URL] [-o table|json] COMMAND [ARGS]

Server URL default: $FAKE_CXL_POOL_SERVER, else ` + api.DefaultServerURL + `

Commands:
  status
  hosts [NAME]
  whoami                                  resolve this VM (uuid, hostname) to a host
  devices [NAME]
  create --size 256Mi [--shared] [--persistent] [--name N] [--serial 0x..]
  delete NAME [--force]
  attach DEVICE (--self | --host HOST) [--slot BUS] [--numa-node N] [--owner O] [--timeout 30s] [--single-host]
  detach DEVICE (--self | --host HOST) [--timeout 30s] [--force]
  attachments [--self | --host H] [--device D]
  events                                  stream server events (JSON lines)

Guest commands (run inside the VM, as root; DEVICE is a device name or a serial, 0x... or decimal):
  guest wait DEVICE [--timeout 30s]       wait until the device appears, print memN
  guest memdev DEVICE                     print memN of the device
  guest region create DEVICE --mode numa|dax [--decoder decoderX.Y]
  guest online DEVICE [--movable]         online the memory of the region (mode numa)
  guest info DEVICE                       print memdev, region, dax device, mode, node
  guest disable DEVICE                    offline, destroy regions, disable memdev

Exit codes: 0 ok, 1 error, 2 usage, 3 conflict/timeout (device still held).
`

type cli struct {
	server  string
	output  string
	stdout  io.Writer
	stderr  io.Writer
	c       *client.Client
	verbose bool
	// k is the kernel of the guest commands, kernel.New() if nil.
	k *kernel.Kernel
}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := &cli{stdout: stdout, stderr: stderr}
	fs := c.flagSet("fake-cxl-pool-client")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, usage)
			return exitOK
		}
		_, _ = fmt.Fprintf(stderr, "error: %v\n\n%s", err, usage)
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return exitUsage
	}
	err := c.dispatch(ctx, rest[0], rest[1:])
	return c.exitCode(err)
}

func (c *cli) exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		_, _ = fmt.Fprintf(c.stderr, "error: %s\n\n%s", ue.msg, usage)
		return exitUsage
	}
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprint(c.stdout, usage)
		return exitOK
	}
	_, _ = fmt.Fprintf(c.stderr, "error: %v\n", err)
	if api.IsCode(err, api.CodeConflict) || errors.Is(err, context.DeadlineExceeded) {
		return exitConflict
	}
	return exitError
}

func (c *cli) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.server, "server", c.server, "server URL")
	fs.StringVar(&c.output, "o", c.output, "output format: table or json")
	fs.BoolVar(&c.verbose, "v", c.verbose, "verbose")
	return fs
}

// parse parses flags and positional arguments in any order.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usagef("%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func (c *cli) client() *client.Client {
	if c.c == nil {
		c.c = client.New(c.server)
	}
	return c.c
}

func (c *cli) dispatch(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "status":
		return c.cmdStatus(ctx, args)
	case "hosts":
		return c.cmdHosts(ctx, args)
	case "whoami":
		return c.cmdWhoami(ctx, args)
	case "devices":
		return c.cmdDevices(ctx, args)
	case "create":
		return c.cmdCreate(ctx, args)
	case "delete":
		return c.cmdDelete(ctx, args)
	case "attach":
		return c.cmdAttach(ctx, args)
	case "detach":
		return c.cmdDetach(ctx, args)
	case "attachments":
		return c.cmdAttachments(ctx, args)
	case "events":
		return c.cmdEvents(ctx, args)
	case "guest":
		return c.cmdGuest(ctx, args)
	case "help", "-h", "--help":
		_, err := fmt.Fprint(c.stdout, usage)
		return err
	}
	return usagef("unknown command %q", cmd)
}

func (c *cli) json() bool { return c.output == "json" }

func (c *cli) printJSON(v any) error {
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table returns a tabwriter on stdout. Its writes fail only at its Flush.
func (c *cli) table() *tabwriter.Writer {
	return tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (c *cli) cmdStatus(ctx context.Context, args []string) error {
	fs := c.flagSet("status")
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		return firstErr(err, usagef("status takes no arguments"))
	}
	st, err := c.client().Status(ctx)
	if err != nil {
		return err
	}
	if c.json() {
		return c.printJSON(st)
	}
	w := c.table()
	_, _ = fmt.Fprintf(w, "name:\t%s\n", dash(st.Name))
	_, _ = fmt.Fprintf(w, "server:\t%s\n", c.client().BaseURL)
	_, _ = fmt.Fprintf(w, "version:\t%s\n", st.Version)
	_, _ = fmt.Fprintf(w, "uptime:\t%s\n", st.Uptime)
	_, _ = fmt.Fprintf(w, "hosts:\t%d\n", st.Hosts)
	_, _ = fmt.Fprintf(w, "devices:\t%d\n", st.Devices)
	_, _ = fmt.Fprintf(w, "attachments:\t%d\n", st.Attachments)
	_, _ = fmt.Fprintf(w, "state file:\t%s\n", dash(st.StateFile))
	for h, v := range st.Qemu.Versions {
		_, _ = fmt.Fprintf(w, "qemu %s:\t%s\n", h, v)
	}
	return w.Flush()
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func freeSlots(h api.Host) int {
	n := 0
	for _, s := range h.Slots {
		if s.Device == "" && s.Attachment == "" && s.LocalDevice == "" {
			n++
		}
	}
	return n
}

func (c *cli) cmdHosts(ctx context.Context, args []string) error {
	fs := c.flagSet("hosts")
	pos, err := parse(fs, args)
	if err != nil || len(pos) > 1 {
		return firstErr(err, usagef("hosts [NAME]"))
	}
	if len(pos) == 1 {
		h, err := c.client().Host(ctx, pos[0])
		if err != nil {
			return err
		}
		return c.printHost(*h)
	}
	hs, err := c.client().Hosts(ctx)
	if err != nil {
		return err
	}
	if c.json() {
		return c.printJSON(hs)
	}
	w := c.table()
	_, _ = fmt.Fprintf(w, "NAME\tSTATE\tCONTROL\tPID\tQEMU\tUUID\tFREE-SLOTS\tATTACHMENTS\tLOCAL-DEVICES\n")
	for _, h := range hs {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%d/%d\t%d\t%d\n", h.Name, h.State, dash(h.Control), h.PID,
			dash(h.QemuVersion), dash(h.UUID), freeSlots(h), len(h.Slots), len(h.Attachments), len(h.LocalDevices))
	}
	return w.Flush()
}

func (c *cli) printHost(h api.Host) error {
	if c.json() {
		return c.printJSON(h)
	}
	w := c.table()
	_, _ = fmt.Fprintf(w, "name:\t%s\n", h.Name)
	_, _ = fmt.Fprintf(w, "uuid:\t%s\n", dash(h.UUID))
	_, _ = fmt.Fprintf(w, "state:\t%s\n", h.State)
	_, _ = fmt.Fprintf(w, "pid:\t%d\n", h.PID)
	ctl := h.Control
	switch h.Control {
	case "qmp":
		ctl += " " + h.QMP
	case "hmp":
		ctl += " " + h.HMP
	}
	_, _ = fmt.Fprintf(w, "control:\t%s\n", dash(ctl))
	_, _ = fmt.Fprintf(w, "qemu:\t%s\n", dash(h.QemuVersion))
	if h.Error != "" {
		_, _ = fmt.Fprintf(w, "error:\t%s\n", h.Error)
	}
	_, _ = fmt.Fprintf(w, "local devices:\t%s\n", dash(strings.Join(h.LocalDevices, " ")))
	// A line without a tab ends a table of w.
	_, _ = fmt.Fprintf(w, "\nhost bridges:\nID\tNUMA\tFMW-SIZE\tATTACHED\n")
	for _, hb := range h.HostBridges {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", hb.ID, hb.NUMANode, api.FormatSize(hb.FMWSize), api.FormatSize(hb.AttachedBytes))
	}
	_, _ = fmt.Fprintf(w, "\nslots:\nBUS\tKIND\tHOST-BRIDGE\tNUMA\tDEVICE\tATTACHMENT\tRESERVED-FOR\n")
	for _, s := range h.Slots {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", s.Bus, s.Kind, s.HostBridge, s.NUMANode, dash(s.Device), dash(s.Attachment), dash(s.LocalDevice))
	}
	if len(h.Attachments) > 0 {
		_, _ = fmt.Fprintf(w, "\nattachments:\n")
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(h.Attachments) > 0 {
		return c.printAttachments(h.Attachments)
	}
	return nil
}

func (c *cli) cmdWhoami(ctx context.Context, args []string) error {
	fs := c.flagSet("whoami")
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		return firstErr(err, usagef("whoami takes no arguments"))
	}
	hostname, uuid := client.SelfIdentity()
	h, err := c.client().Resolve(ctx, hostname, uuid)
	if err != nil {
		return fmt.Errorf("resolve hostname %q uuid %q: %w", hostname, uuid, err)
	}
	return c.printHost(*h)
}

func (c *cli) cmdDevices(ctx context.Context, args []string) error {
	fs := c.flagSet("devices")
	pos, err := parse(fs, args)
	if err != nil || len(pos) > 1 {
		return firstErr(err, usagef("devices [NAME]"))
	}
	if len(pos) == 1 {
		d, err := c.client().Device(ctx, pos[0])
		if err != nil {
			return err
		}
		return c.printDevice(*d)
	}
	ds, err := c.client().Devices(ctx, "")
	if err != nil {
		return err
	}
	if c.json() {
		return c.printJSON(ds)
	}
	return c.printDevices(ds)
}

func attachedTo(d api.Device) string {
	var hs []string
	for _, a := range d.Attachments {
		s := a.Host
		if a.State != api.AttachmentAttached {
			s += "(" + a.State + ")"
		}
		hs = append(hs, s)
	}
	return strings.Join(hs, ",")
}

func (c *cli) printDevices(ds []api.Device) error {
	w := c.table()
	_, _ = fmt.Fprintln(w, "NAME\tSERIAL\tSIZE\tSHARED\tPERSISTENT\tSCOPE\tSTATE\tATTACHED-TO")
	for _, d := range ds {
		scope := d.Scope
		if d.Scope == api.ScopeLocal {
			scope += ":" + d.LocalHost
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%v\t%s\t%s\t%s\n", d.Name, d.Serial, api.FormatSize(d.Size), d.Shared, d.Persistent, scope, d.State, dash(attachedTo(d)))
	}
	return w.Flush()
}

func (c *cli) printDevice(d api.Device) error {
	if c.json() {
		return c.printJSON(d)
	}
	w := c.table()
	_, _ = fmt.Fprintf(w, "name:\t%s\n", d.Name)
	_, _ = fmt.Fprintf(w, "serial:\t%s\n", d.Serial)
	_, _ = fmt.Fprintf(w, "size:\t%s\n", api.FormatSize(d.Size))
	_, _ = fmt.Fprintf(w, "shared:\t%v\n", d.Shared)
	memory := "volatile"
	if d.Persistent {
		memory = "persistent"
	}
	_, _ = fmt.Fprintf(w, "memory:\t%s\n", memory)
	_, _ = fmt.Fprintf(w, "backend:\t%s %s\n", d.Backend, d.Path)
	if d.LSAPath != "" {
		_, _ = fmt.Fprintf(w, "lsa:\t%s\n", d.LSAPath)
	}
	scope := d.Scope
	if d.Scope == api.ScopeLocal {
		scope += " (" + d.LocalHost + ")"
	}
	_, _ = fmt.Fprintf(w, "scope:\t%s\n", scope)
	_, _ = fmt.Fprintf(w, "state:\t%s\n", d.State)
	if len(d.Attachments) > 0 {
		_, _ = fmt.Fprintf(w, "\nattachments:\n")
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(d.Attachments) > 0 {
		return c.printAttachments(d.Attachments)
	}
	return nil
}

func (c *cli) printAttachments(as []api.Attachment) error {
	w := c.table()
	_, _ = fmt.Fprintln(w, "ID\tSLOT\tNUMA\tQEMU-DEVICE\tQEMU-OBJECT\tSERIAL\tSTATE\tERROR")
	for _, a := range as {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Slot.Bus, a.Slot.NUMANode, a.QemuDeviceID, a.QemuObjectID, a.Serial, a.State, dash(a.Error))
	}
	return w.Flush()
}

func (c *cli) printAttachment(a *api.Attachment) error {
	if c.json() {
		return c.printJSON(a)
	}
	return c.printAttachments([]api.Attachment{*a})
}

func (c *cli) cmdCreate(ctx context.Context, args []string) error {
	fs := c.flagSet("create")
	size := fs.String("size", "", "device size, e.g. 256Mi")
	shared := fs.Bool("shared", false, "shared device")
	persistent := fs.Bool("persistent", false, "persistent memory device with a label storage area file")
	name := fs.String("name", "", "device name (default: generated)")
	serial := fs.String("serial", "", "serial number (default: allocated)")
	pos, err := parse(fs, args)
	if err != nil || len(pos) > 0 || *size == "" {
		return firstErr(err, usagef("create --size SIZE [--shared] [--persistent] [--name N] [--serial S]"))
	}
	sz, err := api.ParseSize(*size)
	if err != nil {
		return usagef("%v", err)
	}
	req := api.DeviceCreate{Name: *name, Size: api.Size(sz), Shared: *shared, Persistent: *persistent, Serial: *serial}
	d, err := c.client().CreateDevice(ctx, req)
	if err != nil {
		return err
	}
	if c.json() {
		return c.printJSON(d)
	}
	return c.printDevices([]api.Device{*d})
}

func (c *cli) cmdDelete(ctx context.Context, args []string) error {
	fs := c.flagSet("delete")
	force := fs.Bool("force", false, "detach first")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return firstErr(err, usagef("delete NAME [--force]"))
	}
	if err := c.client().DeleteDevice(ctx, pos[0], *force); err != nil {
		return err
	}
	if c.json() {
		return nil
	}
	_, err = fmt.Fprintf(c.stdout, "deleted %s\n", pos[0])
	return err
}

// hostArg returns the host of --host or --self.
func (c *cli) hostArg(ctx context.Context, host string, self bool) (string, error) {
	if self && host != "" {
		return "", usagef("give either --self or --host")
	}
	if host != "" {
		return host, nil
	}
	if !self {
		return "", usagef("--self or --host HOST is required")
	}
	h, err := c.client().Self(ctx)
	if err != nil {
		hostname, uuid := client.SelfIdentity()
		return "", fmt.Errorf("resolve this host (hostname %q uuid %q): %w", hostname, uuid, err)
	}
	return h.Name, nil
}

func (c *cli) cmdAttach(ctx context.Context, args []string) error {
	fs := c.flagSet("attach")
	self := fs.Bool("self", false, "attach to this VM")
	host := fs.String("host", "", "target host (name or uuid)")
	slot := fs.String("slot", "", "qemu bus of the slot")
	numa := fs.Int("numa-node", -1, "prefer a slot on this NUMA node")
	owner := fs.String("owner", "", "attachment owner")
	timeout := fs.String("timeout", "", "qemu timeout (default 30s)")
	singleHost := fs.Bool("single-host", false, "shared device: no other host may attach it meanwhile")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return firstErr(err, usagef("attach DEVICE (--self | --host HOST) [--slot BUS] [--numa-node N] [--owner O] [--single-host]"))
	}
	h, err := c.hostArg(ctx, *host, *self)
	if err != nil {
		return err
	}
	req := api.AttachRequest{Host: h, Slot: *slot, Owner: *owner, Timeout: *timeout, SingleHost: *singleHost}
	if *numa >= 0 {
		req.NUMANode = numa
	}
	a, _, err := c.client().Attach(ctx, pos[0], req)
	if err != nil {
		return err
	}
	return c.printAttachment(a)
}

func (c *cli) cmdDetach(ctx context.Context, args []string) error {
	fs := c.flagSet("detach")
	self := fs.Bool("self", false, "detach from this VM")
	host := fs.String("host", "", "host (name or uuid)")
	timeout := fs.Duration("timeout", 0, "how long to wait for the guest to release the device (default: server's detachTimeout)")
	force := fs.Bool("force", false, "forget a failed attachment")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return firstErr(err, usagef("detach DEVICE (--self | --host HOST) [--timeout 30s] [--force]"))
	}
	h, err := c.hostArg(ctx, *host, *self)
	if err != nil {
		return err
	}
	a, _, err := c.client().Detach(ctx, pos[0], h, client.DetachOptions{Timeout: *timeout, Force: *force})
	if err != nil {
		if a.ID != "" && !c.json() {
			err = errors.Join(err, c.printAttachments([]api.Attachment{*a}))
		}
		return err
	}
	return c.printAttachment(a)
}

func (c *cli) cmdAttachments(ctx context.Context, args []string) error {
	fs := c.flagSet("attachments")
	self := fs.Bool("self", false, "attachments of this VM")
	host := fs.String("host", "", "host (name or uuid)")
	device := fs.String("device", "", "device")
	pos, err := parse(fs, args)
	if err != nil || len(pos) > 0 {
		return firstErr(err, usagef("attachments [--self | --host H] [--device D]"))
	}
	h := *host
	if *self {
		if h, err = c.hostArg(ctx, *host, true); err != nil {
			return err
		}
	}
	as, err := c.client().Attachments(ctx, h, *device)
	if err != nil {
		return err
	}
	if c.json() {
		return c.printJSON(as)
	}
	return c.printAttachments(as)
}

func (c *cli) cmdEvents(ctx context.Context, args []string) error {
	fs := c.flagSet("events")
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		return firstErr(err, usagef("events takes no arguments"))
	}
	err := c.client().Events(ctx, func(ev client.Event) error {
		b, _ := json.Marshal(ev)
		_, err := fmt.Fprintln(c.stdout, string(b))
		return err
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
