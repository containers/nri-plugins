#!/usr/bin/env python3
"""The wizard of the demo case "wizard": a pod and a claim (or a lease) of
any kind of CXL memory that the driver offers, its YAML, created and
deleted in the demo namespace (docs/memory/cxl/demo.md, The wizard).

Usage:
  wizard.py                      interactive: questions, preview, create
  wizard.py new [OPTIONS]        the same without questions
  wizard.py list                 the objects that the wizard made
  wizard.py yaml NAME            the YAML of NAME
  wizard.py delete NAME...|--all delete objects that the wizard made
  wizard.py check                the desk checks of the rules and the YAML

Options of new, the questions in this order (each has a default; only the
choices that the driver accepts are taken, the others say why not):
  --kind claim|lease             a pod with a ResourceClaim of the device,
                                 or a pod with a lease claim and the
                                 cxl-request command that the wizard runs
  --source local|pool|any        any: local first, then pool
  --memory volatile|persistent
  --share[=false]                one share of a shared device (shares "1")
  --mode numa|dax|block|fs
  --size whole|SIZE              a slice of a local memdev, the least size
                                 of an unshared pool device
  --serial any|SERIAL            one device by its serial
  --read-only[=false]            dax, block, fs
  --fs-type detect|TYPE          fs
  --access default|direct|cached fs
  --path default|PATH            fs, claims
  --steering none|auto|...       numa, claims
  --node vm1|vm2|any             the node of the pod; any: a demo node
                                 (label cxl-demo/node: vm1 or vm2)
  --command idle|env|read|write TEXT|mkfs TYPE
                                 what the container runs (/demo/cxl-wizard.py)
  --name NAME                    default w<N>
  --dry-run                      print the YAML, create nothing
A choice that the devices cannot satisfy now is taken with a warning: the
pod or the lease waits. Every object is in run/wizard/<name>.yaml, with
labels app=cxl-demo-wizard and cxl-demo/wizard=<name>.

DEMO_WIZARD_SAMPLE=DIR takes the state and the cluster of a test data
directory (state.env, vm2.json) instead of the demo (only --dry-run);
DEMO_WIZARD_WAIT (default 30) is how many seconds new waits for the pod.
Exit status: 0 done, 1 failure, 2 a choice that the driver refuses.
"""
import json
import os
import re
import shlex
import sys
import time

import common as c
import wizard_views

LOCAL_DRIVER = "cxl.nri.io"
POOL_DRIVER = "cxl-pool.nri.io"
CONFIG_API = "cxl.nri.io/v1alpha1"
APP = "cxl-demo-wizard"
NAME_LABEL = "cxl-demo/wizard"
NODE_LABEL = "cxl-demo/node"
TOOL = "/demo/cxl-wizard.py"
IMAGE = "localhost/cxl-demo-tools:demo"
IDLE = "trap 'exit 0' TERM; sleep infinity & wait"
DONE = 'echo "cxl-wizard exit $?"'   # the last line of the command in the pod's log
DONE_RE = re.compile(r"^cxl-wizard exit (\d+)$")
WIZARD_DIR = os.path.join(c.STATE_DIR, "wizard")
FS_TYPES = ("erofs", "ext4", "xfs", "btrfs")       # demo setup wizard: fsTypes
MKFS_TYPES = ("erofs", "ext4", "xfs", "btrfs")     # the mkfs tools of the demo image
DAX_FS_TYPES = ("erofs", "ext4", "xfs")
READ_ONLY_FS_TYPES = ("erofs", "squashfs")
XFS_MIN = 300 << 20
STEERING = ("none", "auto", "first-dram", "first-cxl", "start-interleaved", "end-interleaved", "waypoints")
FORBIDDEN_DIRS = ("/dev", "/proc", "/sys", "/run/cxl", "/var/run/cxl")
QUESTIONS = ("kind", "source", "memory", "share", "mode", "size", "serial", "rw", "fs_type", "access",
             "path", "steering", "node", "command", "name")
PROMPTS = {
    "kind": "what does the pod hold?", "source": "which devices?", "memory": "what kind of memory?",
    "share": "one share of a shared device?", "mode": "how does the container get it?", "size": "how much?",
    "serial": "which device?", "rw": "may the container write?", "fs_type": "which filesystem type?",
    "access": "file pages in the device or cached?", "path": "where in the container?",
    "steering": "steer the container's memory?", "node": "on which node?", "command": "what does it run?",
    "name": "the name of the pod and its claim"}
LABELS = {
    ("kind", "claim"): "a ResourceClaim of the device",
    ("kind", "lease"): "a lease claim; cxl-request asks",
    ("source", "local"): "CXL memory of the node",
    ("source", "pool"): "a device of the memory pool",
    ("source", "any"): "local first, then pool",
    ("memory", "volatile"): "RAM: no data kept",
    ("memory", "persistent"): "pmem: data stays",
    ("share", "no"): "a device or a slice of its own",
    ("share", "yes"): "one share of a device",
    ("mode", "numa"): "RAM of a NUMA node",
    ("mode", "dax"): "a dax device to mmap",
    ("mode", "block"): "the pmem block device",
    ("mode", "fs"): "the filesystem on it",
    ("rw", "read-write"): "", ("rw", "read-only"): "",
    ("access", "default"): "direct if the type has DAX",
    ("access", "direct"): "pages are device memory",
    ("access", "cached"): "the node's page cache",
    ("node", "any"): "vm1 or vm2, as it fits",
    ("command", "idle"): "nothing", ("command", "env"): "print its CXL_* variables",
    ("command", "read"): "read what is there",
    ("command", "write"): "write TEXT",
    ("command", "mkfs"): "make a filesystem TYPE"}


# The questions that a flag answers, as cxl-request's: the flag, the
# answer of the flag alone (or =true) and that of =false.
FLAGS = {"share": ("--share", "yes", "no"), "rw": ("--read-only", "read-only", "read-write")}


def option_words(q, v):
    """Return the options of demo wizard new that answer question Q with V."""
    if q in FLAGS:
        flag, yes, _ = FLAGS[q]
        return [flag if v == yes else flag + "=false"]
    return ["--" + q.replace("_", "-"), v]


class Invalid(Exception):
    """A choice that the driver refuses: the question and the reason."""

    def __init__(self, question, value, reason):
        super().__init__("%s: %s" % (" ".join(option_words(question, value)), reason))
        self.question, self.value, self.reason = question, value, reason


###
### The devices and claims of the cluster
###

def short_serial(serial):
    """Return the serial 0x... as lower-case hex, None if it is not a number."""
    n = c.int_any(serial)
    return None if n is None else "0x%x" % n


def inventory(slices_doc, state):
    """Return the published memdevs of the ResourceSlices as dicts.

    Each has name, driver, pool, source (local or pool), serial, shared,
    persistent, size, node (vm1 or vm2 of a local memdev, None for pool
    devices), hosts (the nodes a pool reaches, short names), capacity
    ("memory" or "shares"), block (the memory block size of a sliced
    memdev) and label (how the wizard names it).
    """
    out = []
    lp = short_serial((state or {}).get("LP_SERIAL"))
    for s in c.items(slices_doc):
        spec = s.get("spec") or {}
        hosts = []
        for term in (spec.get("nodeSelector") or {}).get("nodeSelectorTerms") or []:
            for e in term.get("matchExpressions") or []:
                if e.get("key") == "kubernetes.io/hostname":
                    hosts += [c.short_node(v, state) for v in e.get("values") or []]
        for d in spec.get("devices") or []:
            if c.device_attr(d, "type") != "memdev":
                continue
            cap = d.get("capacity") or {}
            source = c.device_attr(d, "source") or ("pool" if spec.get("driver") == POOL_DRIVER else "local")
            node = c.short_node(spec.get("nodeName"), state) if spec.get("nodeName") else None
            serial = short_serial(c.device_attr(d, "serial")) or "?"
            dev = {"name": d.get("name"), "driver": spec.get("driver"), "pool": (spec.get("pool") or {}).get("name"),
                   "source": source, "serial": serial, "shared": c.device_attr(d, "shared") is True,
                   "persistent": c.device_attr(d, "persistent") is True, "size": c.device_attr(d, "size") or 0,
                   "node": node, "hosts": hosts if source == "pool" else [node],
                   "capacity": "shares" if c.SHARES in cap else ("memory" if "memory" in cap else ""),
                   "block": c.device_attr(d, "memoryBlockSize")}
            if source == "pool":
                dev["label"] = d.get("name")
            elif serial == lp:
                dev["label"] = "lp0"
            else:
                dev["label"] = "%s of %s" % (serial[2:], node)
            out.append(dev)
    return sorted(out, key=lambda d: (d["source"] != "local", d["persistent"], d["shared"], d["node"] or "", d["serial"]))


def device_holders(dev, claims):
    """Return the holders of DEV: [{who, node, exclusive, ro, mode, memory}] of the allocated claims."""
    out = []
    for cl in claims:
        for r in c.results(cl):
            if (r.get("driver"), r.get("pool"), r.get("device")) != (dev["driver"], dev["pool"], dev["name"]):
                continue
            sec = c.device_section(cl, r.get("driver"), r.get("request"))
            consumed = r.get("consumedCapacity") or {}
            if dev["capacity"] == "shares":
                q = c.quantity(c.consumed_shares(r))
                exclusive = q is None or q > 1
            else:
                exclusive = dev["capacity"] != "memory" or not dev["block"]
            out.append({"who": c.holder_name(cl), "node": c.allocated_node(cl), "exclusive": exclusive, "ro": c.device_read_only(sec),
                        "mode": c.device_mode(sec, dev["shared"]),
                        "memory": c.quantity(consumed.get("memory")) or 0})
    return out


def state_list(value):
    """Return the items of a list of the state of demo setup: comma-separated."""
    return [v for v in (value or "").split(",") if v]


class Context:
    """What the rules look at: the published devices, the claims, the state of demo setup."""

    def __init__(self, state, k8s, names=()):
        self.state = state or {}
        k8s = k8s or {}
        self.devices = inventory(k8s.get("slices"), self.state)
        self.claims = c.items(k8s.get("claims"), "ResourceClaim")
        self.pods = c.items(k8s.get("pods"), "Pod")
        self.names = set(names)
        self.fs_types = tuple(state_list(self.state.get("FS_TYPES")) or FS_TYPES)
        self.fs_setup = self.state.get("FS_SETUP") or "ext4"
        self.fs_serials = [short_serial(s) for s in state_list(self.state.get("FS_SERIALS"))]

    def by_serial(self, serial):
        """Return the first published device with SERIAL (local memdevs of several nodes may share one), None."""
        return next(iter(self.all_by_serial(serial)), None)

    def all_by_serial(self, serial):
        """Return every published device with SERIAL."""
        s = short_serial(serial)
        return [d for d in self.devices if d["serial"] == s]

    def serial_label(self, serial):
        """Return how the wizard names the devices of SERIAL: "lp0", "fs0", "c100e2e1 of vm1, vm2"."""
        devs = self.all_by_serial(serial)
        if len(devs) > 1 and all(d["source"] == "local" for d in devs):
            return "%s of %s" % (devs[0]["serial"][2:], ", ".join(sorted(d["node"] for d in devs)))
        return devs[0]["label"] if devs else serial

    def short_node(self, name):
        return c.short_node(name, self.state)


###
### The rules (docs/memory/cxl, pkg/cxl/config/device.go, the lease broker)
###

def is_shared(a):
    return a.get("share") == "yes"


def sources(a):
    return ["local", "pool"] if a.get("source") == "any" else [a.get("source")]


def read_only(a):
    return a.get("rw") == "read-only"


def size_bytes(size):
    """Return the bytes of a size answer, None for whole."""
    if size in (None, "whole"):
        return None
    q = c.quantity(size)
    return None if q is None else int(q)


def device_of(a, ctx):
    return ctx.by_serial(a["serial"]) if a.get("serial") not in (None, "any") else None


def asked(q, a):
    """Return None if question Q is asked after the answers A, else why not."""
    mode, kind = a.get("mode"), a.get("kind")
    if q == "rw" and mode == "numa":
        return "readOnly needs mode dax, block or fs: numa memory is the container's RAM"
    if q in ("fs_type", "access", "path") and mode != "fs":
        return "%s needs mode fs" % {"fs_type": "fsType", "access": "access", "path": "path"}[q]
    if q == "path" and kind == "lease":
        return "a lease's filesystem is at /run/cxl/fs/NAME (cxl-request --name), NAME the wizard's name"
    if q == "steering" and mode != "numa":
        return "steering moves a container's memory between NUMA nodes: mode numa only"
    if q == "steering" and kind == "lease":
        return "cxl-request has no steering: a claim has (container.steering)"
    return None


def fit_reason(d, a):
    """Return None if the device D can be the device of the answers A so far, else why not."""
    if d["source"] not in sources(a):
        return "a %s device, not %s" % (d["source"], a["source"])
    if a["memory"] == "persistent" and not d["persistent"]:
        return "volatile memory, not persistent"
    if a["memory"] == "volatile" and d["persistent"]:
        return "persistent memory: a persistent device needs mode block or fs"
    if a["memory"] == "volatile":
        if is_shared(a) and not d["shared"]:
            return "not shareable: %s" % ("every claim gets a slice of its own" if d["source"] == "local"
                                          else "an unshared pool device has no shares")
        if d["shared"] and a.get("mode") != "dax":
            return "a shared device is used as dax only"
        if d["shared"] and a.get("size") not in (None, "whole"):
            return "a shared device is used whole: no size"
    elif is_shared(a) and d["capacity"] != "shares":
        return "not shareable: it has no shares"
    return None


def options(q, a, ctx):
    """Return the choices of question Q after the answers A: [(value, reason)], reason None if it is valid.

    Questions with free answers (size, serial, path, command, name) list
    examples; check() decides about any answer.
    """
    mode = a.get("mode")
    if q == "kind":
        return [("claim", None), ("lease", None)]
    if q == "source":
        return [("local", None), ("pool", None), ("any", None)]
    if q == "memory":
        return [("volatile", None), ("persistent", None)]
    if q == "share":
        return [("no", None), ("yes", None)]
    if q == "mode":
        if a["memory"] == "persistent":
            no = "a persistent device needs mode block or fs"
            return [("numa", no), ("dax", no), ("block", None), ("fs", None)]
        no = "mode %s needs a persistent device"
        numa = "a shared device is used as dax only (never onlined as RAM)" if is_shared(a) else None
        return [("numa", numa), ("dax", None), ("block", no % "block"), ("fs", no % "fs")]
    if q == "size":
        out = [("whole", None)]
        why = size_reason(a)
        examples = ["128Mi", "256Mi", "512Mi"]
        return out + [(s, why) for s in examples]
    if q == "serial":
        out = [("any", serial_any_reason(a))]
        for d in ctx.devices:
            if d["serial"] not in [v for v, _ in out]:
                out.append((d["serial"], fit_reason(d, a) or serial_source_reason(a)))
        return out
    if q == "rw":
        return [("read-write", rw_reason(a, ctx)), ("read-only", None)]
    if q == "fs_type":
        return [("detect", None)] + [(t, fs_type_reason(t, a, ctx)) for t in ctx.fs_types]
    if q == "access":
        t = a.get("fs_type")
        direct = "%s has no DAX: use access cached" % t if t not in ("detect", None) and t not in DAX_FS_TYPES else None
        return [("default", None), ("direct", direct), ("cached", None)]
    if q == "path":
        return [("default", None), ("/data", None)]
    if q == "steering":
        return [(p, "policy waypoints needs container.steering.waypoints: write that claim by hand"
                 if p == "waypoints" else None) for p in STEERING]
    if q == "node":
        only = lease_nodes(a, ctx)
        why = "a lease takes a local device in a pod of its node only (%s)" % ", ".join(only or [])
        return [(n, None if only is None or n in only or (n == "any" and set(only) >= {"vm1", "vm2"}) else why)
                for n in ("vm1", "vm2", "any")]
    if q == "command":
        return [("idle", None), ("env", None), ("read", command_reason("read", a)),
                ("write", command_reason("write", a)), ("mkfs", command_reason("mkfs", a))]
    if q == "name":
        return [(next_name(ctx), None)]
    raise ValueError(q)


def size_reason(a):
    """Return None if a SIZE may be asked for with the answers A, else why not."""
    if a["memory"] == "persistent":
        return "a persistent device is used whole: its capacity is shares"
    if is_shared(a):
        return "a shared device is used whole: its capacity is shares"
    return None


def serial_any_reason(a):
    """Return None if --serial any is valid with the answers A, else why not."""
    if a["kind"] == "lease" and a.get("size") == "whole" and not is_shared(a):
        return "cxl-request needs --size, --serial or --share: name a device, or a size"
    return None


def serial_source_reason(a):
    if a["source"] == "any":
        return "a serial names a device of one source: --source local or pool"
    return None


def rw_reason(a, ctx):
    """Return None if the answers A may be read-write, else why not."""
    mode, d = a.get("mode"), device_of(a, ctx)
    if mode == "block":
        if is_shared(a):
            return "the device is writable in mode block only exclusively (no shares request)"
        if d and d["source"] == "local" and d["shared"]:
            return "other hosts may use the device (sharedMemdevs): it is not written in mode block"
    if mode == "fs" and d and d["source"] == "local" and d["shared"]:
        return "other hosts may use the device (sharedMemdevs): read-write needs a cluster filesystem (gfs2) on coherent memory"
    return None


def fs_type_reason(t, a, ctx):
    """Return None if the filesystem type T is valid with the answers A, else why not."""
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,31}", t):
        return 'invalid filesystem type "%s" (lower-case letters, digits, "_" and "-")' % t
    if t not in ctx.fs_types:
        return "filesystem type %s is not allowed on this node (fsTypes: %s)" % (t, ", ".join(ctx.fs_types))
    if t in READ_ONLY_FS_TYPES and not read_only(a):
        return "%s is a read-only filesystem: set readOnly: true" % t
    return None


def lease_nodes(a, ctx):
    """Return the nodes whose pods may lease the local device of the serial of A (sorted), None if any node may ask."""
    devs = ctx.all_by_serial(a["serial"]) if a.get("serial") not in (None, "any") else []
    if a.get("kind") == "lease" and devs and devs[0]["source"] == "local":
        return sorted(d["node"] for d in devs)
    return None


def command_reason(cmd, a):
    """Return None if the command CMD fits the answers A, else why not."""
    mode = a.get("mode")
    if cmd in ("read", "write") and mode == "numa":
        return "numa memory is RAM of a NUMA node: no bytes at an offset (env shows its node)"
    if cmd == "write" and mode == "block":
        return "a block device takes a filesystem image: mkfs"
    if cmd == "mkfs" and mode != "block":
        return "mkfs writes a block device: mode block"
    return None


def path_reason(p):
    """Return None if P may be device.path, else the driver's error."""
    clean = os.path.normpath(p) if p.startswith("/") else ""
    if not p.startswith("/") or clean != p or p == "/" or "//" in p or \
            any(p == d or p.startswith(d + "/") for d in FORBIDDEN_DIRS):
        return 'invalid path "%s" (an absolute path outside %s)' % (p, ", ".join(FORBIDDEN_DIRS[:-1]) + " and " + FORBIDDEN_DIRS[-1])
    return None


def next_name(ctx):
    n = 1
    for name in ctx.names:
        m = re.fullmatch(r"w(\d+)", name)
        if m:
            n = max(n, int(m.group(1)) + 1)
    return "w%d" % n


def check(q, v, a, ctx):
    """Return None if V is a valid answer to question Q after the answers A, else why not."""
    if q in ("kind", "source", "memory", "share", "mode", "rw", "access", "steering", "node"):
        opts = dict(options(q, a, ctx))
        if v not in opts:
            return "not one of %s" % ", ".join(opts)
        return opts[v]
    if q == "size":
        if v == "whole":
            return None
        why = size_reason(a)
        if why:
            return why
        b = size_bytes(v)
        if b is None or b <= 0 or c.quantity(v) != b:
            return 'invalid size "%s" (a positive quantity: 256Mi, 1Gi)' % v
        return None
    if q == "serial":
        if v == "any":
            return serial_any_reason(a)
        if short_serial(v) is None:
            return 'invalid serial "%s"' % v
        if serial_source_reason(a):
            return serial_source_reason(a)
        d = ctx.by_serial(v)
        if d:
            why = fit_reason(d, a)
            return "%s is %s: %s" % (d["serial"], ctx.serial_label(v), why) if why else None
        return None
    if q == "fs_type":
        return None if v == "detect" else fs_type_reason(v, a, ctx)
    if q == "path":
        return None if v == "default" else path_reason(v)
    if q == "command":
        cmd = v[0]
        if cmd not in ("idle", "env", "read", "write", "mkfs"):
            return "not one of idle, env, read, write TEXT, mkfs TYPE"
        why = command_reason(cmd, a)
        if why:
            return why
        if cmd in ("idle", "env", "read") and len(v) != 1:
            return "%s takes no argument" % cmd
        if cmd == "write" and (len(v) != 2 or not re.fullmatch(r"[ -~]{1,200}", v[1])):
            return "write TEXT: one argument, 1 to 200 printable ASCII characters"
        if cmd == "mkfs" and (len(v) != 2 or v[1] not in MKFS_TYPES):
            return "mkfs TYPE: TYPE one of %s (the tools of the demo image)" % ", ".join(MKFS_TYPES)
        return None
    if q == "name":
        if not re.fullmatch(NAME_RE, v):
            return "a DNS label of at most 40 characters (lower-case letters, digits, -)"
        if v in ctx.names:
            return "%s exists: demo wizard delete %s first" % (v, v)
        return None
    raise ValueError(q)


def default(q, a, ctx):
    """Return the default answer of question Q after the answers A, None if no choice is valid."""
    if q == "mode":
        prefer = ["fs", "block"] if a["memory"] == "persistent" else (["dax"] if is_shared(a) else ["numa", "dax"])
    elif q == "rw":
        prefer = ["read-only"] if a["mode"] == "fs" else ["read-write", "read-only"]
    elif q == "serial":
        prefer = ["any"] + [d["serial"] for d in ctx.devices]
    elif q == "node":
        devs = ctx.all_by_serial(a["serial"]) if a.get("serial") not in (None, "any") else []
        nodes = sorted(d["node"] for d in devs if d["source"] == "local")
        prefer = (nodes if len(nodes) == 1 else []) + ["any", "vm2", "vm1"]
    elif q == "command":
        return ("env",)
    elif q == "name":
        return next_name(ctx)
    else:
        prefer = [v for v, _ in options(q, a, ctx)]
    for v in prefer:
        if check(q, v, a, ctx) is None:
            return v
    return None


def normalize(q, v):
    """Return the answer V of question Q in the form of the rules."""
    if q == "command":
        if isinstance(v, str):
            v = v.split(" ", 1)
        return tuple(v)
    if q == "serial" and v != "any":
        return short_serial(v) or v
    return v


def resolve(given, ctx):
    """Return the answers of every question asked: GIVEN, else the defaults. Raise Invalid."""
    a = {}
    for q in QUESTIONS:
        v = given.get(q)
        why = asked(q, a)
        if why:
            if v is not None:
                raise Invalid(q, v if not isinstance(v, (list, tuple)) else " ".join(v), why)
            continue
        if v is None:
            v = default(q, a, ctx)
            if v is None:
                raise Invalid(q, "?", "no choice is valid: " + "; ".join(
                    "%s: %s" % (o, r) for o, r in options(q, a, ctx) if r)[:300])
        else:
            v = normalize(q, v)
            why = check(q, v, a, ctx)
            if why:
                raise Invalid(q, v if not isinstance(v, tuple) else " ".join(v), why)
        a[q] = v
    return a


###
### What the answers ask for: warnings, the classes, the YAML
###

def fitting(a, ctx):
    """Return the published devices that the answers A may get."""
    out = []
    for d in ctx.devices:
        if fit_reason(d, a):
            continue
        if a["serial"] != "any" and d["serial"] != a["serial"]:
            continue
        if a["serial"] == "any" and a["memory"] == "volatile" and not is_shared(a) and d["shared"]:
            continue   # a shared device is taken exclusively by its serial only
        out.append(d)
    return out


def wait_reason(d, a, ctx, note):
    """Return why the answers A wait for the device D now, None if D has room for them; add notes to NOTE."""
    holders = device_holders(d, ctx.claims)
    want = size_bytes(a.get("size"))
    if a["node"] != "any" and d["source"] == "local" and d["node"] != a["node"]:
        return "%s is %s's, the pod is on %s" % (d["label"], d["node"], a["node"])
    if a["node"] != "any" and d["source"] == "pool" and d["hosts"] and a["node"] not in d["hosts"]:
        return "%s reaches %s only" % (d["label"], ", ".join(d["hosts"]))
    if d["capacity"] == "memory" and d["block"]:
        used = sum(h["memory"] for h in holders)
        need = d["size"] if want is None else -(-want // d["block"]) * d["block"]
        if want is not None and want % d["block"]:
            note.append("%s: a slice is whole memory blocks: %s gets %s" % (d["label"], a["size"], c.k8s_size(need)))
        if need > d["size"] - used:
            return "%s has %s free, %s asked" % (d["label"], c.k8s_size(d["size"] - used), c.k8s_size(need))
        return None
    if want is not None and d["size"] < want:
        return "%s has %s, less than %s" % (d["label"], c.k8s_size(d["size"]), a["size"])
    excl = [h["who"] for h in holders if h["exclusive"]]
    if excl:
        return "%s is held exclusively by %s" % (d["label"], ", ".join(excl))
    if holders and not is_shared(a):
        return "%s is shared by %s: an exclusive claim waits until they are gone" % (
            d["label"], ", ".join(h["who"] for h in holders))
    if a["mode"] == "fs" and d["source"] == "pool" and d["shared"]:
        mine = a["node"]
        others = sorted({ctx.short_node(h["node"]) for h in holders if h["node"]} - ({mine} if mine != "any" else set()))
        writers = sorted({ctx.short_node(h["node"]) for h in holders if h["node"] and h["mode"] == "fs" and not h["ro"]} -
                         ({mine} if mine != "any" else set()))
        if not read_only(a) and others:
            return "%s is on %s too: a read-write claim waits (AttachFailed Conflict) until it is gone there" % (
                d["label"], ", ".join(others))
        if writers:
            return "%s is read-write on %s: a claim of another node waits (AttachFailed Conflict)" % (
                d["label"], ", ".join(writers))
    return None


def warnings(a, ctx):
    """Return the warnings (why the pod or the lease waits) and the notes of the answers A."""
    warn, note = [], []
    waits = "the lease waits" if a["kind"] == "lease" else "the pod waits"
    devs = fitting(a, ctx)
    if a["serial"] != "any" and not ctx.by_serial(a["serial"]):
        warn.append("no published device has serial %s: %s" % (a["serial"], waits))
    elif not devs:
        warn.append("no published device fits %s: %s" % (describe_device(a), waits))
    reasons = [wait_reason(d, a, ctx, note) for d in devs]
    if devs and all(reasons):
        warn += ["%s: %s" % (r, waits) for r in reasons]
    if a["mode"] == "fs":
        t = a["fs_type"]
        made = [d["label"] for d in devs if d["serial"] in ctx.fs_serials]
        if t != "detect" and t != ctx.fs_setup and made:
            warn.append("demo setup made %s on %s: unless a writer made %s there since, its Prepare fails and %s"
                        % (ctx.fs_setup, " and ".join(made), t, waits))
        if not read_only(a) and any(d["source"] == "pool" and d["shared"] for d in devs):
            note.append("ReadWriteOnce: while this claim writes, the device is attached to its node only; "
                        "a pod of another node waits (AttachFailed Conflict)")
    cmd = a["command"]
    small = [d["label"] for d in devs if d["size"] < XFS_MIN + (2 << 20)]
    if cmd[0] == "mkfs" and cmd[1] == "xfs" and small:
        warn.append("mkfs.xfs needs at least 300 MiB: %s is smaller" % ", ".join(small))
    if cmd[0] in ("write", "mkfs") and read_only(a):
        note.append("the device is read-only for this pod: the kernel refuses the %s" % cmd[0])
    return warn, note


def request_class(a, source, d):
    """Return (DeviceClass, shares) of the request of SOURCE for the answers A and the device D of the serial."""
    if a["memory"] == "persistent":
        return ("cxl-local-persistent" if source == "local" else "cxl-pool-persistent"), is_shared(a)
    shared_device = is_shared(a) or bool(d and d["shared"])
    return ("cxl-%s%s" % (source, "-shared" if shared_device else "")), is_shared(a)


def driver_of(source):
    return LOCAL_DRIVER if source == "local" else POOL_DRIVER


def memory_config(a):
    """Return the MemoryConfig of the answers A."""
    dev = {"mode": a["mode"]}
    if a["mode"] in ("dax", "block") and read_only(a):
        dev["readOnly"] = True
    if a["mode"] == "fs":
        dev["readOnly"] = read_only(a)
        if a["fs_type"] != "detect":
            dev["fsType"] = a["fs_type"]
        if a["access"] != "default":
            dev["access"] = a["access"]
        if a.get("path", "default") != "default":
            dev["path"] = a["path"]
    params = {"apiVersion": CONFIG_API, "kind": "MemoryConfig", "device": dev}
    if a.get("steering", "none") != "none":
        params["container"] = {"steering": {"policy": a["steering"]}}
    return params


def requests_of(a, ctx):
    """Return [(source, driver, request)] of the claim of the answers A."""
    d = device_of(a, ctx)
    out = []
    for s in sources(a):
        cls, shares = request_class(a, s, d)
        req = {"deviceClassName": cls}
        if a["serial"] != "any":
            req["selectors"] = [{"cel": {"expression": 'device.attributes["%s"].serial == "%s"' % (driver_of(s), a["serial"])}}]
        if shares:
            req["capacity"] = {"requests": {"shares": "1"}}
        elif a["size"] != "whole" and cls in ("cxl-local", "cxl-pool"):
            req["capacity"] = {"requests": {"memory": a["size"]}}
        out.append((s, driver_of(s), req))
    return out


def claim_doc(a, ctx):
    """Return the ResourceClaim of the answers A."""
    reqs = requests_of(a, ctx)
    if len(reqs) == 1:
        request = {"name": "mem", "exactly": reqs[0][2]}
    else:
        request = {"name": "mem", "firstAvailable": [dict([("name", s)] + list(r.items())) for s, _, r in reqs]}
    configs = []
    for s, drv, _ in reqs:
        entry = {"opaque": {"driver": drv, "parameters": memory_config(a)}}
        if len(reqs) > 1:
            entry = {"requests": ["mem/" + s], "opaque": entry["opaque"]}
        configs.append(entry)
    return {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceClaim",
            "metadata": {"name": a["name"], "labels": labels(a["name"])},
            "spec": {"devices": {"requests": [request], "config": configs}}}


def labels(name):
    return {"app": APP, NAME_LABEL: name}


def lease_args(a):
    """Return the arguments of cxl-request request of the answers A (without --wait and --exec)."""
    args = ["request"]
    if a["source"] != "any":
        args += ["--source", a["source"]]
    args += ["--mode", a["mode"]]
    if is_shared(a):
        args.append("--share")
    if a["mode"] in ("dax", "block") and read_only(a):
        args.append("--read-only")
    if a["mode"] == "fs":
        if not read_only(a):
            args.append("--read-only=false")
        if a["fs_type"] != "detect":
            args += ["--fs-type", a["fs_type"]]
        if a["access"] != "default":
            args += ["--access", a["access"]]
        args += ["--name", a["name"]]
    if a["size"] != "whole":
        args += ["--size", a["size"]]
    if a["serial"] != "any":
        args += ["--serial", a["serial"]]
    return args + ["--request-id", a["name"]]


def pod_doc(a, state):
    """Return the Pod of the answers A."""
    name = a["name"]
    if a["kind"] == "claim":
        claims = [{"name": "mem", "resourceClaimName": name}]
        ref = "mem"
        if a["command"][0] == "idle":
            command = ["sh", "-c", IDLE]
        else:
            command = ["sh", "-c", 'python3 %s "$@"; %s; %s' % (TOOL, DONE, IDLE), "cxl-wizard"] + list(a["command"])
    else:
        claims = [{"name": "lease", "resourceClaimTemplateName": name + "-lease"}]
        ref = "lease"
        command = ["sh", "-c", "touch /tmp/wizard.log; tail -F /tmp/wizard.log & " + IDLE]
    spec = {}
    if a["node"] != "any":
        spec["nodeSelector"] = {NODE_LABEL: a["node"]}
    else:
        # Any demo node: setup labels vm1 and vm2 only (vm0 has no demo image).
        spec["affinity"] = {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {
            "nodeSelectorTerms": [{"matchExpressions": [{"key": NODE_LABEL, "operator": "Exists"}]}]}}}
    spec.update({
        "terminationGracePeriodSeconds": 2, "restartPolicy": "Never", "resourceClaims": claims,
        "volumes": [{"name": "demo", "configMap": {"name": "demo-tools", "defaultMode": Raw("0755")}}],
        "containers": [{"name": "main", "image": (state or {}).get("DEMO_IMAGE") or IMAGE, "imagePullPolicy": "Never",
                        "command": command, "volumeMounts": [{"name": "demo", "mountPath": "/demo"}],
                        "resources": {"claims": [{"name": ref}]}}]})
    return {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "labels": labels(name)}, "spec": spec}


def template_doc(a):
    """Return the ResourceClaimTemplate of the lease claim of the answers A."""
    name = a["name"] + "-lease"
    return {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceClaimTemplate",
            "metadata": {"name": name, "labels": labels(a["name"])},
            "spec": {"metadata": {"labels": labels(a["name"])},
                     "spec": {"devices": {"requests": [{"name": "lease", "exactly": {"deviceClassName": "cxl-lease"}}]}}}}


def describe_device(a):
    """Return the words of what the answers A ask for, the device part."""
    what = {"local": "local CXL memory device", "pool": "pool device",
            "any": "local CXL memory device, else of a pool device"}[a["source"]]
    kind = "(%s)" % a["memory"]
    if a["size"] != "whole" and a["memory"] == "volatile" and not is_shared(a):
        return {"local": "a %s slice of a local CXL memory device %s" % (a["size"], kind),
                "pool": "a pool device %s of %s or more" % (kind, a["size"]),
                "any": "a %s slice of a local CXL memory device, else a pool device of %s or more %s"
                       % (a["size"], a["size"], kind)}[a["source"]]
    if is_shared(a):
        return "one share of a %s %s" % (what, kind)
    return "a whole %s %s" % (what, kind)


def describe(a, ctx):
    """Return the lines of the comment header: what the objects of the answers A ask for, in words."""
    d = device_of(a, ctx)
    who = "the device %s (%s)" % (ctx.serial_label(d["serial"]), d["serial"]) if d else (
        "the device %s" % a["serial"] if a["serial"] != "any" else "any device that fits")
    sentences = []
    holder = "a ResourceClaim" if a["kind"] == "claim" else "a lease (cxl-request in the running container)"
    node = "on %s" % a["node"] if a["node"] != "any" else "on any demo node (vm1 or vm2)"
    sentences.append("A pod %s with %s of %s: %s." % (node, holder, describe_device(a), who))
    if a["source"] == "any":
        sentences.append("The request lists a local and a pool alternative (firstAvailable): "
                         "the scheduler takes the first that a device satisfies.")
    excl = ("Exclusive: no other claim has the device meanwhile." if not is_shared(a) else
            "Shared: capacity shares 1, next to other sharers.")
    if a["memory"] == "volatile" and not is_shared(a) and not (d and d["shared"]):
        excl = ("Its own slice: other claims get other slices." if a["source"] == "local" else
                "Exclusive: the pool device is attached to this pod's node only.")
    sentences.append(excl)
    mode = {"numa": "As RAM of a NUMA node (mode numa): CXL_<KEY>_NUMA_NODE names it.",
            "dax": "As a dax device to mmap (mode dax), %s." % ("read-only" if read_only(a) else "read-write"),
            "block": "As the block device of its fsdax namespace (mode block), %s." % ("read-only" if read_only(a) else "writable"),
            "fs": "As the filesystem on it (mode fs), %s." % ("read-only" if read_only(a) else
                                                               "read-write (readOnly: false is the opt-in to write)")}
    sentences.append(mode[a["mode"]])
    if a["mode"] == "fs":
        t = "detected" if a["fs_type"] == "detect" else "declared %s" % a["fs_type"]
        acc = "default (direct for erofs, ext4 and xfs, else cached)" if a["access"] == "default" else a["access"]
        where = ("at /run/cxl/fs/%s" % a["name"]) if a["kind"] == "lease" else \
            ("at %s" % a["path"] if a["path"] != "default" else "at /run/cxl-fs/<KEY>")
        sentences.append("Filesystem type %s, access %s, %s in the container." % (t, acc, where))
    if a.get("steering", "none") != "none":
        sentences.append("The container is steered with policy %s." % a["steering"])
    cmd = " ".join(shlex.quote(x) for x in a["command"])
    sentences.append("The container runs %s, then idles." % ("nothing" if cmd == "idle" else "%s %s" % (TOOL, cmd)))
    if a["kind"] == "lease":
        sentences.append("The wizard runs in it: cxl-request %s --wait 0s%s." % (
            " ".join(shlex.quote(x) for x in lease_args(a)),
            "" if cmd == "idle" else "; then the same with --wait 600s --exec python3 %s %s in the background "
                                     "(its output is the pod's log)" % (TOOL, cmd)))
    return sentences


def comment_lines(a, ctx, warn, note, width=76):
    """Return the comment header of the YAML file of the answers A."""
    head = "%s: made by demo wizard, %s" % (a["name"], time.strftime("%Y-%m-%d %H:%M:%S"))
    out = ["# " + head]
    for s in describe(a, ctx) + ["Warning: " + w for w in warn] + ["Note: " + n for n in note]:
        out += ["# " + l for l in wrap_words(s, width - 2, "  ")]
    tail = "Labels app=%s and %s=%s; demo wizard delete %s removes it." % (APP, NAME_LABEL, a["name"], a["name"])
    return out + ["# " + l for l in wrap_words(tail, width - 2, "  ")]


def wrap_words(text, width, cont=""):
    """Return TEXT wrapped at WIDTH columns, the continuation lines starting with CONT."""
    lines, cur = [], ""
    for w in text.split(" "):
        if cur and len(cur) + 1 + len(w) > width:
            lines.append(cur)
            cur = cont + w
        else:
            cur = (cur + " " + w) if cur else w
    lines.append(cur)
    return lines


def documents(a, ctx):
    """Return the objects of the answers A, in the order they are created."""
    if a["kind"] == "claim":
        return [claim_doc(a, ctx), pod_doc(a, ctx.state)]
    return [template_doc(a), pod_doc(a, ctx.state)]


###
### YAML: a writer for the objects and a reader of what it writes
###

class Raw(str):
    """A scalar written as it is (defaultMode: 0755)."""


PLAIN = re.compile(r"[A-Za-z_/][A-Za-z0-9_./-]*")
RESERVED = {"true", "false", "null", "yes", "no", "on", "off", "y", "n", "~"}


def yaml_scalar(v):
    """Return the YAML text of the scalar V."""
    if isinstance(v, Raw):
        return str(v)
    if v is True:
        return "true"
    if v is False:
        return "false"
    if v is None:
        return "null"
    if isinstance(v, int):
        return str(v)
    s = str(v)
    if PLAIN.fullmatch(s) and s.lower() not in RESERVED:
        return s
    return "'" + s.replace("'", "''") + "'"


def to_yaml(obj, indent=0):
    """Return the lines of OBJ in block style, at INDENT."""
    pad = " " * indent
    out = []
    if isinstance(obj, dict):
        for k, v in obj.items():
            if isinstance(v, dict) and v:
                out.append("%s%s:" % (pad, k))
                out += to_yaml(v, indent + 2)
            elif isinstance(v, list) and v:
                out.append("%s%s:" % (pad, k))
                out += to_yaml(v, indent)
            elif isinstance(v, (dict, list)):
                out.append("%s%s: %s" % (pad, k, "{}" if isinstance(v, dict) else "[]"))
            else:
                out.append("%s%s: %s" % (pad, k, yaml_scalar(v)))
    elif isinstance(obj, list):
        for v in obj:
            if isinstance(v, (dict, list)) and v:
                sub = to_yaml(v, indent + 2)
                out.append(pad + "- " + sub[0][indent + 2:])
                out += sub[1:]
            else:
                out.append(pad + "- " + yaml_scalar(v))
    return out


def yaml_text(a, ctx, warn=(), note=()):
    """Return the text of the YAML file of the answers A: the comment header and the objects."""
    lines = comment_lines(a, ctx, list(warn), list(note))
    for i, doc in enumerate(documents(a, ctx)):
        if i:
            lines.append("---")
        lines += to_yaml(doc)
    return "\n".join(lines) + "\n"


def parse_scalar(s):
    if s.startswith("'") and s.endswith("'") and len(s) >= 2:
        return s[1:-1].replace("''", "'")
    if s in ("true", "false"):
        return s == "true"
    if s == "null":
        return None
    if s == "{}":
        return {}
    if s == "[]":
        return []
    if re.fullmatch(r"0[0-7]+", s):
        return int(s, 8)
    if re.fullmatch(r"-?\d+", s):
        return int(s)
    return s


def from_yaml(text):
    """Return the documents of TEXT, YAML as to_yaml writes it (block style, no comments in values)."""
    docs = []
    for part in re.split(r"^---$", text, flags=re.M):
        lines = [l for l in part.splitlines() if l.strip() and not l.lstrip().startswith("#")]
        if lines:
            obj, rest = parse_block(lines, 0, 0)
            if rest != len(lines):
                raise ValueError("cannot parse line %d: %r" % (rest + 1, lines[rest]))
            docs.append(obj)
    return docs


def indent_of(line):
    return len(line) - len(line.lstrip(" "))


def parse_block(lines, i, indent):
    """Return (object, next line) of the block of LINES at I with INDENT."""
    if lines[i][indent:].startswith("- "):
        out = []
        while i < len(lines) and indent_of(lines[i]) == indent and lines[i][indent:].startswith("- "):
            body = lines[i][indent + 2:]
            if re.match(r"[^' ][^:]*: |[^' ][^:]*:$", body) and not body.startswith("'"):
                sub = [" " * (indent + 2) + body] + lines[i + 1:]
                obj, j = parse_block(sub, 0, indent + 2)
                out.append(obj)
                i += j
            else:
                out.append(parse_scalar(body))
                i += 1
        return out, i
    out = {}
    while i < len(lines) and indent_of(lines[i]) == indent:
        m = re.fullmatch(r"([^:' ][^:]*):(?: (.*))?", lines[i][indent:])
        if not m:
            raise ValueError("cannot parse %r" % lines[i])
        key, val = m.group(1), m.group(2)
        if val is None:
            nxt = lines[i + 1] if i + 1 < len(lines) else ""
            sub_indent = indent_of(nxt)
            if nxt and (sub_indent > indent or (sub_indent == indent and nxt[indent:].startswith("- "))):
                out[key], i = parse_block(lines, i + 1, sub_indent)
            else:
                out[key], i = None, i + 1
        else:
            out[key], i = parse_scalar(val), i + 1
    return out, i


###
### The cluster: kubectl in vm2
###

def load():
    """Return (state, k8s objects) of the demo, or of DEMO_WIZARD_SAMPLE."""
    sample = os.environ.get("DEMO_WIZARD_SAMPLE")
    if sample:
        state = c.load_state(os.path.join(sample, "state.env")) or {}
        with open(os.path.join(sample, "vm2.json")) as f:
            return state, (json.load(f).get("k8s") or {})
    state = c.load_state()
    if state is None:
        sys.exit("demo not set up: demo setup wizard")
    if state.get("DEMO_CASE") != "wizard":
        sys.exit("the demo is set up for %s, not wizard: demo teardown; demo setup wizard" % state.get("DEMO_CASE"))
    return state, cluster(state)


def cluster(state):
    p = c.run_probe(state["VM2_DIR"], ["--k8s", "pods,claims,slices", "--ns", state["NS"]], timeout=10)
    if "slices" not in (p.get("k8s") or {}):
        raise RuntimeError("; ".join(p.get("errors") or ["no objects"]))
    return p["k8s"]


def made_names(k8s):
    """Return the names of the wizard's objects: the files of run/wizard and the labeled pods and claims."""
    names = set()
    if os.path.isdir(WIZARD_DIR) and not os.environ.get("DEMO_WIZARD_SAMPLE"):
        names.update(f[:-5] for f in os.listdir(WIZARD_DIR) if f.endswith(".yaml"))
    for o in c.items((k8s or {}).get("pods")) + c.items((k8s or {}).get("claims")):
        if c.labels(o).get(NAME_LABEL):
            names.add(c.labels(o)[NAME_LABEL])
    return names


def kubectl(state, script, timeout=120):
    """Return (exit status, output) of the shell SCRIPT with kubectl of the demo namespace in vm2."""
    return c.run_vm(state["VM2_DIR"], "cd /root/demo && export KUBECONFIG=/root/demo/kubeconfig\n" + script, timeout=timeout)


def apply(state, text):
    marker = "WIZARD_YAML_%d" % os.getpid()
    return kubectl(state, "kubectl apply -f - <<'%s'\n%s%s\n" % (marker, text, marker))


def status(name, k8s, state):
    """Return the status words of the wizard object NAME: (word, detail)."""
    pods = [p for p in c.items(k8s.get("pods")) if c.labels(p).get(NAME_LABEL) == name and not c.lease_id(p)]
    if not pods:
        return "gone", ""
    pod = pods[0]
    claims = c.items(k8s.get("claims"), "ResourceClaim")
    phase = (pod.get("status") or {}).get("phase") or "?"
    lease = any(r.get("resourceClaimTemplateName") for r in (pod.get("spec") or {}).get("resourceClaims") or [])
    if lease:
        comps = [cl for cl in claims if c.lease_id(cl) and c.requester(cl) == name]
        cpods = [p for p in c.items(k8s.get("pods")) if c.lease_id(p) and c.requester(p) == name]
        if phase != "Running":
            return phase, scheduled_why(pod, state)
        if not comps:
            return "no lease", ""
        cl = comps[-1]
        why = attach_failed(cl)
        if why:
            return "waiting", why
        if c.allocation(cl) and cpods and (cpods[-1].get("status") or {}).get("phase") == "Running":
            return "Ready", ""
        return "waiting", scheduled_why(cpods[-1], state) if cpods else ""
    mine = [cl for cl in claims if c.meta(cl).get("name") == name]
    why = attach_failed(mine[0]) if mine else ""
    if phase == "Running":
        return "Running", ""
    if why:
        return "waiting", why
    return phase, scheduled_why(pod, state)


def attach_failed(claim):
    """Return the reason of a true AttachFailed condition of CLAIM, "" if there is none."""
    for d in (claim.get("status") or {}).get("devices") or []:
        for cond in d.get("conditions") or []:
            if cond.get("type", "").endswith("AttachFailed") and cond.get("status") == "True":
                return "AttachFailed %s" % (cond.get("reason") or "")
    return ""


def scheduled_why(pod, state):
    for cond in (pod.get("status") or {}).get("conditions") or []:
        if cond.get("type") == "PodScheduled" and cond.get("status") == "False":
            msg = cond.get("message") or cond.get("reason") or ""
            if "cannot allocate all claims" in msg:
                return "cannot allocate"
            return c.short_text(msg, state)[:120]
    return ""


def wait_status(name, state, seconds, pod_only=False):
    """Return the status of NAME when it is settled or after SECONDS: Running, Ready, or why it waits.

    With POD_ONLY, the status of its pod: Running once the pod runs.
    """
    t0 = time.time()
    word, why = "?", ""
    while time.time() - t0 < seconds:
        try:
            k8s = cluster(state)
            if pod_only:
                pods = [p for p in c.items(k8s.get("pods")) if c.labels(p).get(NAME_LABEL) == name and not c.lease_id(p)]
                word = ((pods[0].get("status") or {}).get("phase") or "?") if pods else "gone"
                why = scheduled_why(pods[0], state) if pods else ""
            else:
                word, why = status(name, k8s, state)
        except RuntimeError as e:
            word, why = "?", str(e)
        if word in ("Running", "Ready", "Failed", "Succeeded"):
            break
        if time.time() - t0 > 5 and (why == "cannot allocate" or why.startswith("AttachFailed")):
            break
        time.sleep(1)
    return word, why


def lease_start(a, state):
    """Run cxl-request in the lease pod of the answers A; return the response (a dict) and its output."""
    cmd = ["kubectl", "exec", a["name"], "--", "python3", TOOL, "lease"] + lease_args(a)
    if a["command"][0] != "idle":
        cmd += ["--"] + list(a["command"])
    rc, out = kubectl(state, " ".join(shlex.quote(x) for x in cmd), timeout=60)
    resp = {}
    for line in out.splitlines():
        if line.startswith("{"):
            try:
                resp = json.loads(line)
            except ValueError:
                pass
    if rc != 0 and not resp:
        resp = {"error": (out.strip().splitlines() or ["exit %d" % rc])[-1]}
    return resp, out


###
### Commands
###

def print_lines(lines):
    for l in lines:
        for w in wrap_words(l, c.WIDTH, "  "):
            print(c.cut(w))


def mark_step(name, start):
    """Write run/step.env: the wizard's object NAME and when its creation STARTed (demo truth dumps the logs since then)."""
    tmp = os.path.join(c.STATE_DIR, "step.env.tmp")
    with open(tmp, "w") as f:
        f.write("STEP=wizard-%s\nSTART=%.6f\n" % (name, start))
    os.replace(tmp, os.path.join(c.STATE_DIR, "step.env"))


def create(a, ctx, warn, note, dry_run):
    """Write run/wizard/NAME.yaml of the answers A and create its objects; return the exit status."""
    text = yaml_text(a, ctx, warn, note)
    if dry_run or os.environ.get("DEMO_WIZARD_SAMPLE"):
        sys.stdout.write(text)
        return 0
    os.makedirs(WIZARD_DIR, exist_ok=True)
    path = os.path.join(WIZARD_DIR, a["name"] + ".yaml")
    with open(path, "w") as f:
        f.write(text)
    print_lines(["warning: " + w for w in warn])
    print("created %s:" % os.path.relpath(path, c.DEMO_DIR))
    mark_step(a["name"], time.time())
    rc, out = apply(ctx.state, text)
    print_lines(out.strip().splitlines())
    if rc != 0:
        print("%s: refused by the cluster" % a["name"])
        return 1
    wait = float(os.environ.get("DEMO_WIZARD_WAIT", "30"))
    if a["kind"] == "lease":
        word, why = wait_status(a["name"], ctx.state, max(wait, 60), pod_only=True)
        if word != "Running":
            print("%s: pod %s%s" % (a["name"], word, ": " + why if why else ""))
            return 1
        resp, out = lease_start(a, ctx.state)
        if resp.get("error"):
            print("%s: refused: %s" % (a["name"], resp["error"]))
            return 1
        print("%s lease %s: %s" % (a["name"], resp.get("lease", "?"), resp.get("state", "?")))
        print_lines(l for l in out.strip().splitlines() if l.startswith("started "))
    word, why = wait_status(a["name"], ctx.state, wait)
    done = ""
    if word in ("Running", "Ready") and a["command"][0] != "idle":
        # A command that writes is done only when the pod's log says so:
        # deleting the object earlier stops it half way.
        rc, lines = wait_command(a["name"], ctx.state, max(wait, 120))
        print_lines(lines[-3:])
        cmd = " ".join(a["command"][:1] + a["command"][1:2] if a["command"][0] == "mkfs" else a["command"][:1])
        done = "; %s exit %d" % (cmd, rc) if rc is not None else "; %s still running" % cmd
    print("%s: %s%s%s" % (a["name"], word, ": " + why if why else "", done))
    return 0


def command_result(log):
    """Return (exit status, output lines) of the command in the pod log LOG; status None if it has not ended."""
    out = []
    for line in log.splitlines():
        m = DONE_RE.match(line.strip())
        if m:
            return int(m.group(1)), out
        out.append(line)
    return None, out


def wait_command(name, state, seconds):
    """Return (exit status, output lines) of the command of the pod NAME once it ended, status None after SECONDS."""
    t0 = time.time()
    rc, lines = None, []
    while time.time() - t0 < seconds:
        code, log = kubectl(state, "kubectl logs %s 2>/dev/null" % shlex.quote(name), timeout=30)
        rc, lines = command_result(log if code == 0 else "")
        if rc is not None:
            break
        time.sleep(1)
    return rc, [l for l in lines if l.strip() and not l.startswith(("CXL_", "lease "))]


BOOLS = {v: True for v in ("1", "t", "T", "TRUE", "true", "True")}
BOOLS.update({v: False for v in ("0", "f", "F", "FALSE", "false", "False")})


def given_options(args):
    """Return (answers, flags) of the new options ARGS; exit 2 on a usage error."""
    given, flags = {}, set()
    keys = {"--" + q.replace("_", "-"): q for q in QUESTIONS if q not in FLAGS and q != "command"}
    flag_of = {f[0]: q for q, f in FLAGS.items()}
    i = 0
    while i < len(args):
        a = args[i]
        name, eq, value = a.partition("=")
        if name in flag_of:
            q = flag_of[name]
            on = BOOLS.get(value) if eq else True
            if on is None:
                sys.exit(usage_error("%s: invalid value %r" % (name, value)))
            answer = FLAGS[q][1] if on else FLAGS[q][2]
            if given.get(q, answer) != answer:
                sys.exit(usage_error("%s given twice, with different values" % name))
            given[q] = answer
            i += 1
        elif a == "--dry-run":
            flags.add(a)
            i += 1
        elif a == "--command":
            if i + 1 >= len(args):
                sys.exit(usage_error("--command needs a value"))
            cmd = [args[i + 1]]
            i += 2
            if cmd[0] in ("write", "mkfs") and i < len(args):
                cmd.append(args[i])
                i += 1
            given["command"] = tuple(cmd)
        elif a in keys:
            if i + 1 >= len(args):
                sys.exit(usage_error("%s needs a value" % a))
            given[keys[a]] = args[i + 1]
            i += 2
        else:
            sys.exit(usage_error("unknown option %s" % a))
    return given, flags


# The names of the wizard's objects: DNS labels of at most 40 characters.
NAME_RE = r"[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?"


def usage_error(msg):
    print("wizard: %s" % msg, file=sys.stderr)
    return 2


def cmd_new(args):
    if "-h" in args or "--help" in args:
        print(__doc__)
        return 0
    given, flags = given_options(args)
    state, k8s = load()
    ctx = Context(state, k8s, made_names(k8s))
    try:
        a = resolve(given, ctx)
    except Invalid as e:
        print("wizard: refused: %s" % e, file=sys.stderr)
        return 2
    warn, note = warnings(a, ctx)
    return create(a, ctx, warn, note, "--dry-run" in flags)


def object_rows(k8s, state):
    """Return the rows of demo wizard list: [name, kind, pod, node, devices, mode, access]."""
    claims = c.items(k8s.get("claims"), "ResourceClaim")
    names = sorted(made_names(k8s), key=lambda n: (len(n), n))
    rows = []
    for name in names:
        pods = [p for p in c.items(k8s.get("pods")) if c.labels(p).get(NAME_LABEL) == name and not c.lease_id(p)]
        pod = pods[0] if pods else {}
        refs = (pod.get("spec") or {}).get("resourceClaims") or []
        kind = "lease" if any(r.get("resourceClaimTemplateName") for r in refs) else ("claim" if pod else "-")
        mine = [cl for cl in claims if (c.lease_id(cl) and c.requester(cl) == name) or
                (not c.lease_id(cl) and c.meta(cl).get("name") == name)]
        devs = sorted({r.get("device") for cl in mine for r in c.results(cl)})
        word, why = status(name, k8s, state)
        mode = access = "-"
        if mine:
            cl = mine[-1]
            res = c.results(cl)
            r = res[0] if res else {}
            sec = c.device_section(cl, r.get("driver"), r.get("request"))
            mode = sec.get("mode") or c.device_mode(sec)
            words = wizard_views.claim_memory(cl, state).split(" ")
            if words[0] == mode:
                words = words[1:-1]
            access = " ".join(words) or "-"
        node = c.short_node((pod.get("spec") or {}).get("nodeName"), state) if pod else "-"
        rows.append([name, kind, word + (" (%s)" % why if why and word not in ("Running", "Ready") else ""),
                     node, ",".join(d for d in devs if d) or "-", mode, access])
    return rows


def cmd_list(args):
    state, k8s = load()
    rows = object_rows(k8s, state)
    if not rows:
        print("no objects of the wizard: demo wizard new ...")
        return 0
    head = ["NAME", "KIND", "STATE", "NODE", "DEVICE", "MODE", "ACCESS"]
    widths = [max(len(r[i]) for r in rows + [head]) for i in range(len(head))]
    for r in [head] + rows:
        print("  ".join(v.ljust(w) for v, w in zip(r, widths)).rstrip())
    return 0


def cmd_yaml(args):
    if len(args) != 1:
        return usage_error("yaml NAME")
    path = os.path.join(WIZARD_DIR, args[0] + ".yaml")
    try:
        with open(path) as f:
            sys.stdout.write(f.read())
    except OSError:
        print("wizard: no %s: demo wizard list" % os.path.relpath(path, c.DEMO_DIR), file=sys.stderr)
        return 1
    return 0


def cmd_delete(args):
    if not args:
        return usage_error("delete NAME...|--all")
    bad = [n for n in args if n != "--all" and not re.fullmatch(NAME_RE, n)]
    if bad:
        return usage_error("delete: not names of the wizard: %s" % " ".join(map(repr, bad)))
    state, k8s = load()
    names = sorted(made_names(k8s)) if args == ["--all"] else args
    if not names:
        print("no objects of the wizard")
        return 0
    sel = "%s in (%s)" % (NAME_LABEL, ",".join(names))
    script = """kubectl delete pods -l '{sel}' --ignore-not-found --wait=true --timeout=90s
kubectl delete resourceclaims,resourceclaimtemplates -l '{sel}' --ignore-not-found --wait=true --timeout=90s
for i in $(seq 90); do
    left=$(kubectl get pods,resourceclaims -l cxl.nri.io/lease -o json | python3 -c '
import json, sys
names = set(sys.argv[1:])
for o in json.load(sys.stdin)["items"]:
    req = (o["metadata"].get("annotations") or {{}}).get("cxl.nri.io/requester", "")
    if req.split("/")[1:2] and req.split("/")[1] in names:
        print(o["kind"] + "/" + o["metadata"]["name"])' {names})
    [ -z "$left" ] && {{ echo "no leases left"; exit 0; }}
    sleep 1
done
echo "leases still there: $left"
exit 1
""".format(sel=sel, names=" ".join(shlex.quote(n) for n in names))
    rc, out = kubectl(state, script, timeout=240)
    print_lines(out.strip().splitlines())
    for n in names:
        p = os.path.join(WIZARD_DIR, n + ".yaml")
        if os.path.exists(p):
            os.unlink(p)
    print("deleted: %s" % " ".join(names) if rc == 0 else "wizard: delete did not finish")
    return 0 if rc == 0 else 1


###
### Interactive
###

def option_lines(q, a, ctx, dflt):
    """Return the lines of the choices of question Q and the values that may be typed by number.

    The valid choices are numbered, the default marked "*"; the others
    follow, grouped by the reason why they are not valid.
    """
    lines, values, why_not = [], [], {}
    for v, why in options(q, a, ctx):
        label = LABELS.get((q, v), "")
        d = ctx.by_serial(v) if q == "serial" and v != "any" else None
        if d:
            label = "%s %s" % (ctx.serial_label(v), "/".join(c.k8s_size(x["size"]) for x in ctx.all_by_serial(v)))
        if why:
            why_not.setdefault(why, []).append(ctx.serial_label(v) if d else v)
            continue
        values.append(v)
        mark = "*" if v == (dflt[0] if isinstance(dflt, tuple) else dflt) else " "
        lines.append(("%s%d %-10s %s" % (mark, len(values), v, label)).rstrip())
    for why, names in why_not.items():
        words = [n + "," for n in names[:-1]] + [names[-1] + ":"] + why.split(" ")
        lines += c.wrap_list("  - ", words, indent=6, sep="")
    return lines, values


def ask(q, a, ctx, n):
    """Ask question Q and return the answer; raise EOFError on q."""
    dflt = default(q, a, ctx)
    print()
    print(c.cut("%d/%d %s: %s" % (n, len(QUESTIONS), q.replace("_", "-"), PROMPTS[q])))
    lines, values = option_lines(q, a, ctx, dflt)
    for l in lines:
        print(c.cut(l))
    free = {"size": "or a size: 384Mi", "serial": "or a serial: 0x...", "path": "or a path: /data",
            "command": "write TEXT or mkfs TYPE", "name": "or a name"}.get(q)
    while True:
        shown = " ".join(dflt) if isinstance(dflt, tuple) else dflt
        ans = input("%s[%s]: " % ((free + " ") if free else "", shown)).strip()
        if ans in ("q", "quit"):
            raise EOFError
        if not ans:
            v = dflt
        elif ans.isdigit() and 1 <= int(ans) <= len(values):
            v = values[int(ans) - 1]
        else:
            v = ans
        if q == "command" and isinstance(v, str):
            v = tuple(v.split(" ", 1))
            if v[0] in ("write", "mkfs") and len(v) == 1:
                v += (input("%s: " % ("TEXT" if v[0] == "write" else "TYPE")).strip(),)
        v = normalize(q, v)
        why = check(q, v, a, ctx)
        if why is None:
            return v
        print(c.cut("not taken: ") + why)


def cmd_interactive():
    state, k8s = load()
    ctx = Context(state, k8s, made_names(k8s))
    a = {}
    try:
        n = 0
        for q in QUESTIONS:
            n += 1
            if asked(q, a):
                continue
            a[q] = ask(q, a, ctx, n)
    except (EOFError, KeyboardInterrupt):
        print("\nnothing made")
        return 0
    warn, note = warnings(a, ctx)
    print()
    print_lines(describe(a, ctx))
    print_lines(["warning: " + w for w in warn] + ["note: " + x for x in note])
    print("the same: demo wizard new " + " ".join(shlex.quote(x) for x in new_args(a)))
    while True:
        try:
            k = input("Enter create, y the YAML, q quit: ").strip()
        except (EOFError, KeyboardInterrupt):
            print()
            return 0
        if k == "y":
            sys.stdout.write(yaml_text(a, ctx, warn, note))
        elif k == "q":
            print("nothing made")
            return 0
        elif k == "":
            return create(a, ctx, warn, note, False)


def new_args(a):
    """Return the options of demo wizard new that make the answers A."""
    out = []
    for q in QUESTIONS:
        if q not in a:
            continue
        if q == "command":
            out += ["--command"] + list(a[q])
        else:
            out += option_words(q, a[q])
    return out


def main():
    args = sys.argv[1:]
    if not args:
        return cmd_interactive()
    cmd, rest = args[0], args[1:]
    if cmd == "new":
        return cmd_new(rest)
    if cmd == "list":
        return cmd_list(rest)
    if cmd == "yaml":
        return cmd_yaml(rest)
    if cmd == "delete":
        return cmd_delete(rest)
    if cmd == "check":
        os.execv(sys.executable, [sys.executable, os.path.join(c.BIN_DIR, "wizard_check.py")] + rest)
    if cmd in ("-h", "--help", "help"):
        print(__doc__)
        return 0
    return usage_error("unknown command %s (new, list, yaml, delete)" % cmd)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)
