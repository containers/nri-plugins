#!/usr/bin/env python3
"""Observations of test90-mbt-stress (contract.txt 6).

Usage:
  mbt-observe.py vm [--ns NS] [--stacks-after S]
                     [--cluster --nodes ID=NODE,... --units P=UNIT,...
                     [--pools P=POOL,...]
                     [--inspected UID:RESTARTS,...]]
      In a VM, as root (mbt.source.sh installs it as /usr/local/bin/
      mbt-observe.py): print one JSON line {"vm": OBS} where OBS is the
      per-VM observation of contract 6.3: boot id, CXL memdevs, regions
      and their dax devices, the CXL ports with committed decoders
      (amendment A10), NUMA nodes, the state files of the driver,
      the device names of the CDI specs, and the cpuset.mems,
      cpuset.mems.effective and memory.high of every running container of
      namespace NS (default cxl-e2e).
      With --cluster (on the control plane) also "cluster": the parts of
      the settle observation (contract 6.1) that kubectl and systemd give:
      "pods" (one per mbt.slot pod, with its newest Warning event, A10),
      "companions", "others", "drivers",
      "controllers" and "inspect", the environment (and the leases of
      lease pods) of the Running and Ready slot pods whose UID:RESTARTS is
      not in --inspected (entries UID:RESTARTS[:SLOT]). ID are the VM ids
      of the model (vm2, vm1, vm0),
      NODE their Kubernetes node names; P are pool ids (a, b, c), UNIT the
      systemd units of their controllers.
      With --stacks-after S, an observation still running after S seconds
      writes to stderr the line STACKS_MARKER, the state, system call (of
      a read or write with its file) and kernel stack of the threads of
      the driver process and of itself (/proc/PID/task/TID/), and a
      second later the Python stacks of its threads.
  mbt-observe.py assemble --step N --poll K --nodes ID=NODE,...
                          --vm ID=FILE... --attachments P=FILE...
                          [--inspected-in FILE] [--inspected-out FILE]
                          [--summary FILE] [--pdev NAME=DATA:LSA...]
      On the host: merge the outputs of "vm" (FILE: what the VM printed)
      and the attachment lists of the pool servers (FILE: the JSON of
      GET /api/v1/attachments) into the settle observation of contract
      6.1, printed as one JSON line. --inspected-in has the
      UID:RESTARTS:SLOT entries inspected before (one per line),
      --inspected-out gets those of the pods that still exist plus the
      ones inspected now, but for those that list a lease on its way to
      Ready (Creating, Pending, Lending): inspected again next time.
      --summary gets lines for the shell: "driver ID POD RESTARTS READY".
      --pdev: "pdevs" of the persistent device NAME from its backing
      files DATA and LSA on the host.

Python 3 standard library only. Every part is read in its own try: a part
that cannot be read is null, and the reason is in the "errors" list of the
object that has the part.

Where the data comes from:
  memdevs       /sys/bus/cxl/devices/memN: serial, ram/size + pmem/size,
                enabled = the memdev has a driver
  regions       /sys/bus/cxl/devices/regionN: size, mode (the kernel's: ram
                or pmem), the serials of the memdevs of its targets
                (targetI -> decoderE.D -> endpointE -> uport memN), node =
                target_node of its dax devices; dax = the dax devices of
                its dax region with a size (the zero-size seed device is
                left out): size, driver (kmem, device_dax or null), online
                = bytes of its memory blocks that are online (its ranges
                are the /proc/iomem resources with the name of the device)
  ports         /sys/bus/cxl/devices/portN and endpointN with
                decoders_committed > 0: kind (hostbridge: a port right
                under rootR; switch: a port below one; endpoint), hb (the
                host bridge it is under: hbK for the root decoder
                decoderR.K whose target_list names the dport that is the
                host bridge's uport; the topologies of test90 have
                cxl-fmw.K -> cxlhbK), the decoders 0..N-1 (N =
                decoders_committed: committed, or pinned below a
                committed one, A9) with region ("" for none),
                start and size
  nodes         /sys/devices/system/node/nodeN/meminfo MemTotal
  state         the records of the driver's document
                /var/lib/kubelet/plugins/cxl.nri.io/state.json (every
                part is null without it): "bootID" the boot of the
                records; "prepared" by driver the UIDs of the claims
                that kubelet may take as prepared (PREPARED,
                PREPARED_UNRESTORED, UNPREPARING); "localMemdevs" per
                local memdev its serial and per claim UID the number of
                its grants that use the memdev (0 for a grant that holds
                the region without a use yet); "unusedDax" the dax
                devices of the regions of local memdevs that no grant
                uses; "leaseClaims" the UIDs of the lease claims;
                "leases" by id: mode, state (Creating, Pending, Lending,
                Ready, Releasing, Failed) and pod name
  cdi           /var/run/cdi/*.json: file -> device names
  containers    crictl ps (running containers of NS) and crictl inspect:
                the cgroup v2 directory of each container is that of its
                init process (/proc/PID/cgroup). mems is cpuset.mems, or,
                when that file is empty (the cgroup sets no nodes and
                has its parent's), cpuset.mems.effective: never ""
                (amendment A11). A field
                that cannot be read is null, with an error. Left out:
                containers that are not running (crictl ps lists
                running ones; one that exits between the listing and
                the reading is dropped). assemble lists in "missing"
                every container that the pod status says runs and that
                is not here (A11). "devices" (A44): the device nodes in
                the container's /dev (through /proc/PID/root/dev, the
                top level and one directory below it but pts, shm and
                mqueue): path, type (c or b), major, minor, dev
                ("MAJOR:MINOR"); runc's default nodes (null, zero, full,
                random, urandom, tty, console, ptmx) are left out.
                devicesFrom is "proc", or "spec" when /proc could not
                be read: the device nodes of the runtime spec (crictl
                inspect info.runtimeSpec.linux.devices)
  cxlDevices    (A44) the CXL dax and pmem devices of the VM: name
                (dax0.1, pmem1), path (/dev/NAME), kind (dax, pmem),
                major, minor, dev ("MAJOR:MINOR"; null for a dax device
                without a device number), driver (of a dax device:
                kmem, device_dax or null), region (the CXL region),
                serial (the memdev of the region's first target; serials
                all of them): the dax devices of "regions", the block
                devices of the namespaces of "pmem"
  driverLog     the running nri-dra-driver-cxl container of the node
                (crictl): its id, the log lines of
                its log file (status.logPath) that match "panic|fatal
                error|SIGSEGV" (count, the first 5), invariants (the
                count of "engine invariant: " and "containers
                invariant: " lines, the violations of its own
                invariants that the driver logs at V(4), and the first
                5 in invariantErrors), lendsRestored (its
                log has "NRI fence: lends restored": NRI and lending are
                up, in every fencing mode, A15), fencing (the mode of
                its start line "defaults.container.fencing: MODE: ...",
                null before it, A36) and sockets (both dra.sock files
                exist); the
                file is read incrementally, the position is kept in
                /run/mbt-observe (A13)
  pods[].events one "kubectl get events -n NS --field-selector
                type=Warning" per observation: the Warning events of the
                pod (by uid), newest first, at most 5: reason, message
                (at most 4000 bytes: a longer one keeps its first 1900
                and last 2000 bytes around " [...] ", so that the end,
                where the driver's reason is, stays); "event" is the
                first of them, null if none (A10, A12). Newest is by the
                apiserver's time of the event's last occurrence (of a
                series: lastObservedTime)
  pods[].claims every claim of the pod in the order of its spec, each as
                "claim" (which is the first one): [] for none; two for
                dnuma (A39), c0 of mbt-lnuma and c1 of mbt-xnuma-c
  claim.attachFailed  (of "claim" and of every entry of "claims", A44
                (4)) true iff the status entry of a pool device of the
                claim's allocation has the condition
                cxl-pool.nri.io/AttachFailed True (the pool controller's,
                fake-cxl-pool pkg/controller/status.go condFailed: it
                removes Attached when it sets it and the other way
                round), false for an allocated pool claim without it,
                null for a claim without pool devices (a local claim, one
                not allocated)
  slices        one "kubectl get resourceslices": per VM the devices of
                its cxl.nri.io slices: name, serial, shared, type (A13);
  poolSlices    from the same call, per pool id (--pools ID=POOL, default
                ID=pool-ID) its cxl-pool.nri.io slices: name, device
                names, nodes (VM ids of the nodeSelector), [] for none
                (A18)
Persistent pool devices (A24), all from sysfs, /proc/self/mountinfo and
the driver's files; no file under a node mount or a lease bind is opened
and no /dev/pmem* (the driver's umount would get EBUSY, its O_EXCL
holder check and a concurrent erofs mount would fail):
  pmem          per CXL region of mode pmem: its serials, resource, size,
                the nvdimm region of the same physical address
                (/sys/bus/nd/devices/regionM/resource) and its namespaces
                with a size: name, mode (memory = fsdax), size, uuid,
                holder (pfnM.K), map (the pfn's mode: ram = map mem),
                blockdev (pmemK), major, minor
  fsMounts      the mounts under <pool plugin dir>/mounts in the mount
                namespace of the VM (the node mounts): target, source,
                fstype, superopts, vfsopts, root, count (a node mount is
                listed twice: the rshared self-bind of /var/lib/kubelet),
                and for a loop device source its backing file, dio, ro
  leaseBinds    the mounts under <cxl.nri.io plugin dir>/lease-fs: as
                fsMounts
  leaseDirs     the names in lease-fs/ (per lease claim uid, the names in
                lease-fs/<uid>, which are mount points: not followed),
                leases/ and mounts/
  loops         loop devices whose backing file is /dev/pmem*: name,
                backing, dio, ro
  ctrMounts     per running container of NS (as containers): its erofs
                mounts from /proc/PID/mountinfo: target, source, root,
                vfsopts
  poolClaims    of state.json ({} without it): per prepared claim of
                cxl-pool.nri.io by UID its claimName, the devices of its
                grants (serial, mode, readOnly, persistent, blockDevice,
                access, path, nodeMount, loopDevice; null for what the
                grant does not have) and lend (of a companion claim:
                lease, pod, container, fsDir, name, mounts; else null)
  cdiPool       the CDI devices of the pool driver's spec (cxl-pool-claims.json):
                per device name its deviceNodes (path, type,
                permissions) and mounts (hostPath, containerPath,
                options)
  kernel        /dev/kmsg records since the last observation that match
                BUG:|Oops|WARNING:|general protection|nd_pmem|pfn|erofs
                (lines, the first 20; count; bad: of them, those of
                BUG:|Oops|WARNING:|general protection); the position is
                kept in /run/mbt-observe/kernel.json
  killAt        the state of a driver-kill-at watcher (kill-at below) in
                /run/mbt-killat/state.json, null if none
  driverLog     also the count of lines with "not restoring" or "not
                binding" (notRemounted)
  inspect       of the persistent kinds and plse, one kubectl exec: env,
                "mount" (the mountinfo entry of CXL_<KEY>_PATH, null if
                none), "mounts" (the entries under /run/cxl/fs), and
                "pmemNodes" (the /dev/pmem* of the container); plse also
                its leases
  pdevs         (assemble, on the host: --pdev NAME=DATA:LSA) the content
                of the backing files of a persistent device: lsa zero|
                labels, lsaHash, headHash (sha256 of the first 4 MiB of the
                data file), pfn ("NVDIMM_PFN_INFO" at 4096), dataOff (le64
                at 4152), erofs (its magic at dataOff+1024), token (the 32
                bytes at dataOff+4096, stripped, if erofs, else null), raw
                (the string in bytes 0-63 at dataOff; null without pfn)

  mbt-observe.py kill-at --marker region|ns|mount|bind [--timeout S]
                         [--interval MS]
      In a VM, as root: arm a one-shot watcher (A23 driver-kill-at) and
      print its state once it is armed; mbt.source.sh runs it detached
      (setsid). It SIGKILLs the driver process of the node (crictl, as
      mbt-driver-kill) at the first poll (every MS, default 2) that sees
      the marker of a persistent Prepare that was not there when it was
      armed: region a CXL region of mode pmem; ns a namespace with a
      size in the nvdimm region of such a region; mount a mount under
      the pool plugin's mounts/; bind a mount under lease-fs/. It gives
      up after S seconds (default 60). It is bound to the driver
      process it was armed against (a pidfd): if that one ends first
      (any driver restart), it ends, not fired; it never kills another
      process. State in /run/mbt-killat/state.json: marker, armed,
      fired, done, reason (fired, expired, driver restarted, disarmed by
      VERB, or an error), ms (from arming to the kill or the end), seen
      (what it saw), pid, container, stateChanged (poolClaims of
      state.json changed between arming and the kill).
  mbt-observe.py kill-at-disarm [--verb VERB]
      Stop that watcher and print its state, done; not fired yet: reason
      "disarmed by VERB".
"""

import argparse
import errno
import faulthandler
import glob
import hashlib
import json
import os
import re
import select
import signal
import stat
import struct
import subprocess
import sys
import threading
import time

CXL = "/sys/bus/cxl/devices"
DAX = "/sys/bus/dax/devices"
IOMEM = "/proc/iomem"
ND = "/sys/bus/nd/devices"
NODES = "/sys/devices/system/node"
MEMORY = "/sys/devices/system/memory"
LOCAL_DIR = "/var/lib/kubelet/plugins/cxl.nri.io"
POOL_DIR = "/var/lib/kubelet/plugins/cxl-pool.nri.io"
POOL_MOUNTS = POOL_DIR + "/mounts"
LEASE_FS = LOCAL_DIR + "/lease-fs"
LEASES_DIR = LOCAL_DIR + "/leases"
LEASE_FS_CTR = "/run/cxl/fs"
KILLAT_DIR = "/run/mbt-killat"
CDI_DIR = "/var/run/cdi"
SLOT_LABEL = "mbt.slot"
LEASE_LABEL = "cxl.nri.io/lease"
REQUESTER_LABEL = "cxl.nri.io/requester-uid"
DRIVER_LABEL = "app.kubernetes.io/name=nri-dra-driver-cxl"
DRIVER_CONTAINER = "nri-dra-driver-cxl"
POOL_DRIVER = "cxl-pool.nri.io"
MAIN = "main"
CXL_REQUEST = "/usr/local/bin/cxl-request"
EXEC_TIMEOUT = 30


def natkey(path):
    """Sort key of a path: its digits as numbers (mem2 before mem10)."""
    return [int(t) if t.isdigit() else t for t in re.split(r"(\d+)", path)]


def read(path):
    with open(path) as f:
        return f.read().strip()


def serial_text(value):
    """0x<lower-case hex> of a serial: a number, or hex text with or
    without 0x (sysfs, serialHex)."""
    if isinstance(value, int):
        return "0x%x" % value
    text = str(value).strip().lower()
    try:
        return "0x%x" % int(text, 16)
    except ValueError:
        return text


def run(args, timeout=30):
    env = dict(os.environ)
    if "CONTAINER_RUNTIME_ENDPOINT" not in env:
        for sock in ("/run/containerd/containerd.sock", "/run/crio/crio.sock"):
            if os.path.exists(sock):
                env["CONTAINER_RUNTIME_ENDPOINT"] = "unix://" + sock
                break
    return subprocess.run(args, capture_output=True, text=True, timeout=timeout, env=env)


def part(errors, name, fn, *args):
    """Return fn(*args), or None with an error in errors."""
    try:
        return fn(*args)
    except Exception as e:  # every part on its own
        errors.append("%s: %s: %s" % (name, type(e).__name__, e))
        return None


###
### The VM (contract 6.3)
###

def block_size():
    return int(read(MEMORY + "/block_size_bytes"), 16)


def endpoint_memdevs():
    """Endpoint id -> memdev name (endpointE/uport is the memdev)."""
    out = {}
    for p in glob.glob(CXL + "/endpoint[0-9]*"):
        try:
            out[os.path.basename(p)[len("endpoint"):]] = os.path.basename(os.readlink(p + "/uport"))
        except OSError:
            continue
    return out


def memdevs(errors):
    out = []
    for p in sorted(glob.glob(CXL + "/mem[0-9]*"), key=natkey):
        name = os.path.basename(p)
        try:
            serial = serial_text(read(p + "/serial"))
        except OSError as e:
            errors.append("memdev %s: serial: %s" % (name, e))
            serial = None
        size = 0
        for kind in ("ram", "pmem"):
            try:
                size += int(read("%s/%s/size" % (p, kind)), 0)
            except (OSError, ValueError):
                pass
        out.append({"name": name, "serial": serial, "size": size})
    return out


def dax_ranges():
    """Return dax device name (daxX.Y) -> its (start, end) ranges in
    /proc/iomem.

    The dax drivers request one iomem resource per range of a device. The
    resource has the name of the device. The observer reads no mappingN
    file of a dax device. A read of such a file can deadlock with a dax
    size write of the driver (drivers/dax/bus.c)."""
    out = {}
    for line in read(IOMEM).splitlines():
        span, sep, name = line.strip().partition(" : ")
        if not sep or not re.fullmatch(r"dax\d+\.\d+", name):
            continue
        start, _, end = span.partition("-")
        start, end = int(start, 16), int(end, 16)
        if end == 0:
            raise ValueError("%s has no addresses: not read as root" % IOMEM)
        out.setdefault(name, []).append((start, end))
    return out


def online_bytes(ranges, bs):
    """Bytes of the online memory blocks inside ranges, a list of
    (start, end)."""
    online = 0
    for start, end in ranges:
        for b in range(start // bs, end // bs + 1):
            try:
                if read("%s/memory%d/state" % (MEMORY, b)) == "online":
                    online += bs
            except OSError:
                continue
    return online


def dax_devices(region_path, bs, iomem, errors):
    out = []
    for dr in sorted(glob.glob(region_path + "/dax_region[0-9]*"), key=natkey):
        for d in sorted(glob.glob(dr + "/dax[0-9]*.[0-9]*"), key=natkey):
            name = os.path.basename(d)
            try:
                size = int(read(d + "/size"), 0)
            except (OSError, ValueError) as e:
                errors.append("dax %s: size: %s" % (name, e))
                continue
            if size == 0:
                continue  # the seed device of the dax region
            entry = {"name": name, "size": size, "driver": None, "node": None, "online": None}
            try:
                entry["driver"] = os.path.basename(os.readlink(d + "/driver"))
            except OSError:
                pass
            try:
                entry["node"] = int(read(d + "/target_node"))
            except (OSError, ValueError):
                pass
            if bs and iomem is not None:
                entry["online"] = online_bytes(iomem.get(name, []), bs)
            out.append(entry)
    return out


def regions(errors):
    eps = endpoint_memdevs()
    serials = {}
    for p in glob.glob(CXL + "/mem[0-9]*"):
        try:
            serials[os.path.basename(p)] = serial_text(read(p + "/serial"))
        except OSError:
            continue
    try:
        bs = block_size()
    except (OSError, ValueError) as e:
        errors.append("block size: %s" % e)
        bs = 0
    try:
        iomem = dax_ranges()
    except (OSError, ValueError) as e:
        errors.append("iomem: %s" % e)
        iomem = None
    out = []
    for p in sorted(glob.glob(CXL + "/region[0-9]*"), key=natkey):
        name = os.path.basename(p)
        r = {"name": name, "serials": [], "mode": None, "size": None, "node": None, "dax": []}
        try:
            r["size"] = int(read(p + "/size"), 0)
        except (OSError, ValueError) as e:
            errors.append("region %s: size: %s" % (name, e))
        try:
            r["mode"] = read(p + "/mode")
        except OSError:
            pass
        targets = sorted(glob.glob(p + "/target[0-9]*"), key=natkey)
        for t in targets:
            try:
                dec = read(t)
            except OSError:
                continue
            m = re.fullmatch(r"decoder(\d+)\.\d+", dec)
            mem = eps.get(m.group(1)) if m else None
            if mem:
                r["serials"].append(serials.get(mem, mem))
        r["dax"] = dax_devices(p, bs, iomem, errors)
        nodes = sorted(set(d.pop("node") for d in r["dax"]) - {None})
        if nodes:
            r["node"] = nodes[0]
            if len(nodes) > 1:
                errors.append("region %s: dax devices on nodes %s" % (name, nodes))
        out.append(r)
    return out


def port_number(name):
    m = re.search(r"(\d+)$", name)
    return m.group(1) if m else ""


def host_bridges():
    """The real path of a host bridge's uport -> hbK: the host bridge that
    root decoder decoderR.K targets (target_list: ids of dports of rootR,
    dportID links to the host bridge)."""
    out = {}
    for root in glob.glob(CXL + "/root[0-9]*"):
        rid = port_number(root)
        for rd in glob.glob("%s/decoder%s.[0-9]*" % (root, rid)):
            k = rd.rsplit(".", 1)[1]
            try:
                targets = [t.strip() for t in read(rd + "/target_list").split(",") if t.strip()]
            except OSError:
                continue
            for t in targets:
                dport = "%s/dport%s" % (root, t)
                if os.path.exists(dport):
                    out[os.path.realpath(dport)] = "hb" + k
    return out


def ports(errors):
    """CXL ports and endpoints with committed decoders (amendment A10)."""
    hbs = host_bridges()
    out = []
    paths = glob.glob(CXL + "/port[0-9]*") + glob.glob(CXL + "/endpoint[0-9]*")
    for p in sorted(paths, key=natkey):
        name = os.path.basename(p)
        try:
            committed = int(read(p + "/decoders_committed"))
        except (OSError, ValueError) as e:
            errors.append("port %s: decoders_committed: %s" % (name, e))
            continue
        if committed <= 0:
            continue
        real = os.path.realpath(p).split("/")
        roots = [i for i, c in enumerate(real) if re.fullmatch(r"root\d+", c)]
        chain = real[roots[-1] + 1:] if roots else [name]
        if name.startswith("endpoint"):
            kind = "endpoint"
        elif len(chain) == 1:
            kind = "hostbridge"
        else:
            kind = "switch"
        hb = None
        if roots and len(chain) >= 1:
            hbport = "/".join(real[:roots[-1] + 2])
            try:
                hb = hbs.get(os.path.realpath(hbport + "/uport"))
            except OSError:
                pass
        entry = {"port": name, "kind": kind, "hb": hb, "committed": committed, "decoders": []}
        num = port_number(name)
        for i in range(committed):
            d = "%s/decoder%s.%d" % (p, num, i)
            dec = {"name": os.path.basename(d), "region": None, "start": None, "size": None}
            try:
                dec["region"] = read(d + "/region")
            except OSError:
                dec["region"] = ""
            try:
                dec["start"] = int(read(d + "/start"), 0)
                dec["size"] = int(read(d + "/size"), 0)
            except (OSError, ValueError) as e:
                errors.append("port %s: %s: %s" % (name, dec["name"], e))
            entry["decoders"].append(dec)
        out.append(entry)
    return out


def numa_nodes():
    out = {}
    for p in sorted(glob.glob(NODES + "/node[0-9]*"), key=natkey):
        n = os.path.basename(p)[len("node"):]
        with open(p + "/meminfo") as f:
            for line in f:
                w = line.split()
                if len(w) >= 4 and w[2] == "MemTotal:":
                    out[n] = {"memTotal": int(w[3]) * 1024}
                    break
    return out


def json_file(path):
    """The JSON of path, None if it does not exist. A read that does not
    decode is retried."""
    last = None
    for _ in range(4):
        try:
            with open(path) as f:
                text = f.read()
        except FileNotFoundError:
            return None
        try:
            return json.loads(text)
        except ValueError as e:
            last = e
            time.sleep(0.05)
    raise ValueError("%s: %s" % (path, last))


def cdi_names(path):
    """The device names of the CDI spec file path (JSON)."""
    with open(path) as f:
        return [d.get("name") for d in json.load(f).get("devices") or []]


def cdi(errors):
    out = {}
    for path in sorted(glob.glob(CDI_DIR + "/*.json")):
        if not os.path.isfile(path):
            continue
        try:
            out[path] = cdi_names(path)
        except Exception as e:
            errors.append("cdi %s: %s" % (path, e))
            out[path] = None
    return out


def json_objects(text):
    """The JSON values of text: one value, a list, or concatenated values."""
    dec, out, text = json.JSONDecoder(), [], text.strip()
    while text:
        obj, end = dec.raw_decode(text)
        out.extend(obj if isinstance(obj, list) else [obj])
        text = text[end:].strip()
    return out


def crictl_inspect(ids):
    if not ids:
        return []
    r = run(["crictl", "inspect", "-o", "json"] + ids)
    if r.returncode == 0:
        return json_objects(r.stdout)
    # A container went away after the listing: the others one by one.
    out = []
    for i in ids:
        r = run(["crictl", "inspect", "-o", "json", i])
        if r.returncode == 0:
            out.extend(json_objects(r.stdout))
    return out


# The device nodes that runc gives every container (left out of "devices").
DEFAULT_DEVS = {"null", "zero", "full", "random", "urandom", "tty", "console", "ptmx"}
SKIP_DEV_DIRS = {"pts", "shm", "mqueue"}


def dev_entry(path, kind, major, minor):
    return {"path": path, "type": kind, "major": major, "minor": minor,
            "dev": "%d:%d" % (major, minor) if major is not None and minor is not None else None}


def container_devices(pid):
    """The device nodes in /dev of the container of process pid (A44),
    runc's defaults left out."""
    out = []
    base = "/proc/%d/root/dev" % pid

    def walk(path, rel, depth):
        for name in sorted(os.listdir(path), key=natkey):
            p = path + "/" + name
            try:
                st = os.lstat(p)
            except FileNotFoundError:
                continue
            if stat.S_ISDIR(st.st_mode):
                if depth == 0 and name not in SKIP_DEV_DIRS:
                    walk(p, rel + "/" + name, 1)
                continue
            if stat.S_ISCHR(st.st_mode):
                kind = "c"
            elif stat.S_ISBLK(st.st_mode):
                kind = "b"
            else:
                continue
            if depth == 0 and name in DEFAULT_DEVS:
                continue
            out.append(dev_entry("/dev" + rel + "/" + name, kind, os.major(st.st_rdev), os.minor(st.st_rdev)))
    walk(base, "", 0)
    return out


def spec_devices(info):
    """The device nodes of the runtime spec of a container (crictl inspect)."""
    devs = (((info.get("info") or {}).get("runtimeSpec") or {}).get("linux") or {}).get("devices") or []
    return [dev_entry(d.get("path"), d.get("type"), d.get("major"), d.get("minor")) for d in devs]


def cxl_devices(regs, pm):
    """The CXL dax and pmem devices of the VM (A44), from the parts
    "regions" and "pmem" (null if one of them is)."""
    if regs is None or pm is None:
        raise RuntimeError("regions or pmem not read")
    out = []
    for r in regs:
        for d in r.get("dax") or []:
            major = minor = None
            try:
                major, minor = (int(x) for x in read("%s/%s/dev" % (DAX, d["name"])).split(":"))
            except (OSError, ValueError):
                pass
            e = dev_entry("/dev/" + d["name"], "c", major, minor)
            e.update(name=d["name"], kind="dax", driver=d.get("driver"), region=r.get("name"),
                     serial=(r.get("serials") or [None])[0], serials=r.get("serials") or [])
            out.append(e)
    for r in pm:
        for ns in r.get("namespaces") or []:
            if not ns.get("blockdev"):
                continue
            e = dev_entry("/dev/" + ns["blockdev"], "b", ns.get("major"), ns.get("minor"))
            e.update(name=ns["blockdev"], kind="pmem", driver=None, region=r.get("region"),
                     serial=(r.get("serials") or [None])[0], serials=r.get("serials") or [])
            out.append(e)
    return sorted(out, key=lambda e: natkey(e["name"]))


def containers(ns, errors, ctr_mounts=None):
    """The running containers of namespace ns (contract 6.3, A11). With
    ctr_mounts (a list), append to it the erofs mounts of each one (A24
    ctrMounts)."""
    r = run(["crictl", "ps", "-o", "json", "--namespace", "^%s$" % re.escape(ns)])
    if r.returncode != 0:
        raise RuntimeError("crictl ps: %s" % r.stderr.strip()[-300:])
    listed = json.loads(r.stdout or "{}").get("containers") or []
    out = []
    for c in crictl_inspect([c["id"] for c in listed]):
        st = c.get("status") or {}
        if st.get("state") != "CONTAINER_RUNNING":
            continue
        lb = st.get("labels") or {}
        md = st.get("metadata") or {}
        entry = {
            "ns": lb.get("io.kubernetes.pod.namespace"),
            "pod": lb.get("io.kubernetes.pod.name"),
            "name": lb.get("io.kubernetes.container.name") or md.get("name"),
            "id": st.get("id"),
            "mems": None, "memsEffective": None, "memoryHigh": None,
            "devices": None, "devicesFrom": None,
        }
        pid = (c.get("info") or {}).get("pid") or 0
        try:
            entry["devices"], entry["devicesFrom"] = container_devices(pid), "proc"
        except OSError:
            # Not readable (it exited meanwhile, or no pid): the spec's.
            entry["devices"], entry["devicesFrom"] = spec_devices(c), "spec"
        try:
            with open("/proc/%d/cgroup" % pid) as f:
                cg = "/sys/fs/cgroup" + [l[3:].strip() for l in f if l.startswith("0::")][0]
            mems = read(cg + "/cpuset.mems")
            eff = read(cg + "/cpuset.mems.effective")
            entry["memoryHigh"] = read(cg + "/memory.high") or None
            if not eff:
                # Never empty for a live cgroup: unreadable.
                raise OSError("empty cpuset.mems.effective in %s" % cg)
            entry["memsEffective"] = eff
            entry["mems"] = mems or eff
            if entry["memoryHigh"] is None:
                errors.append("container %s/%s/%s: empty memory.high" % (entry["ns"], entry["pod"], entry["name"]))
        except (OSError, IndexError) as e:
            if pid and os.path.exists("/proc/%d" % pid):
                errors.append("container %s/%s/%s: cgroup: %s" % (entry["ns"], entry["pod"], entry["name"], e))
            else:
                continue  # it exited meanwhile
        out.append(entry)
        if ctr_mounts is not None:
            cm = {"pod": entry["pod"], "name": entry["name"], "mounts": None}
            try:
                cm["mounts"] = [{"target": m["target"], "source": m["source"], "root": m["root"],
                                 "vfsopts": m["vfsopts"], "superopts": m["superopts"]}
                                for m in mountinfo("/proc/%d/mountinfo" % pid) if m["fstype"] == "erofs"]
            except (OSError, ValueError) as e:
                if os.path.exists("/proc/%d" % pid):
                    errors.append("ctrMounts %s/%s/%s: %s" % (entry["ns"], entry["pod"], entry["name"], e))
            ctr_mounts.append(cm)
    if ctr_mounts is not None:
        ctr_mounts.sort(key=lambda e: (e["pod"] or "", e["name"] or ""))
    return sorted(out, key=lambda e: (e["pod"] or "", e["name"] or ""))


DRIVER_STATE_DIR = "/run/mbt-observe"
PANIC = re.compile(r"panic|fatal error|SIGSEGV")
LENDS_RESTORED = "NRI fence: lends restored"
# The driver's start line of its fencing mode (D/driver.go logDefaults, A36).
FENCING_LOG = re.compile(r"defaults\.container\.fencing: ([a-z-]+):")
# The driver's own invariant checks, logged at V(4): of the engine between
# requests (D/internal/engine observe.go checkBetween), of the fenced
# containers after a repair pass (D/internal/containers fence.go pass).
INVARIANT = re.compile(r"(engine|containers) invariant: ")
# Counted lines of the driver log (A24): a persistent device not mounted or
# bound again.
LOG_COUNTS = [
    ("notRemounted", re.compile(r"not restoring|not binding")),
]


def driver_log(errors):
    """The running driver container of the node and what its log says
    (amendment A13, A15, A24)."""
    out = {"container": None, "panics": 0, "errors": [],
           "invariants": 0, "invariantErrors": [], "lendsRestored": False, "fencing": None, "sockets": all(os.path.exists("/var/lib/kubelet/plugins/%s/dra.sock" % d)
                                                   for d in ("cxl.nri.io", "cxl-pool.nri.io"))}
    out.update((k, 0) for k, _ in LOG_COUNTS)
    r = run(["crictl", "ps", "-q", "--state", "running", "--namespace", "^kube-system$",
             "--name", "^%s$" % DRIVER_CONTAINER])
    if r.returncode != 0:
        raise RuntimeError("crictl ps: %s" % r.stderr.strip()[-300:])
    ids = r.stdout.split()
    if not ids:
        return out
    if len(ids) > 1:
        errors.append("driverLog: %d running driver containers: %s" % (len(ids), " ".join(i[:13] for i in ids)))
    info = crictl_inspect(ids[:1])
    if not info:
        return out  # it exited meanwhile
    st = info[0].get("status") or {}
    cid = st.get("id") or ids[0]
    out["container"] = cid
    path = st.get("logPath")
    if not path:
        raise RuntimeError("no logPath of container %s" % cid)
    os.makedirs(DRIVER_STATE_DIR, exist_ok=True)
    statefile = os.path.join(DRIVER_STATE_DIR, "driverlog-%s.json" % cid)
    saved = {"offset": 0, "panics": 0, "errors": [], "invariants": 0, "invariantErrors": [],
             "lendsRestored": False, "fencing": None}
    saved.update((k, 0) for k, _ in LOG_COUNTS)
    try:
        with open(statefile) as f:
            saved.update(json.load(f))
    except (OSError, ValueError):
        pass
    with open(path, "rb") as f:
        size = os.fstat(f.fileno()).st_size
        if size < saved["offset"]:
            saved["offset"] = 0  # rotated: the new file from its start
        f.seek(saved["offset"])
        data = f.read()
    # Only whole lines: a line that is being written is read next time.
    end = data.rfind(b"\n") + 1
    for raw in data[:end].splitlines():
        line = raw.decode(errors="replace")
        # CRI log format: TIME STREAM P|F CONTENT
        content = line.split(" ", 3)[3] if line.count(" ") >= 3 else line
        if not saved["lendsRestored"] and LENDS_RESTORED in content:
            saved["lendsRestored"] = True
        if saved["fencing"] is None:
            m = FENCING_LOG.search(content)
            if m:
                saved["fencing"] = m.group(1)
        if PANIC.search(content):
            saved["panics"] += 1
            if len(saved["errors"]) < 5:
                saved["errors"].append(content[:300])
        if INVARIANT.search(content):
            saved["invariants"] += 1
            if len(saved["invariantErrors"]) < 5:
                saved["invariantErrors"].append(content[:300])
        for k, rx in LOG_COUNTS:
            if rx.search(content):
                saved[k] += 1
    saved["offset"] += end
    try:
        with open(statefile + ".tmp", "w") as f:
            json.dump(saved, f)
        os.replace(statefile + ".tmp", statefile)
    except OSError as e:
        errors.append("driverLog: %s: %s" % (statefile, e))
    out.update(panics=saved["panics"], errors=saved["errors"], invariants=saved["invariants"],
               invariantErrors=saved["invariantErrors"], fencing=saved["fencing"],
               lendsRestored=saved["lendsRestored"])
    out.update((k, saved[k]) for k, _ in LOG_COUNTS)
    return out


###
### The driver's state: the records of the driver's document state.json
###

STATE_FILE = LOCAL_DIR + "/state.json"
LOCAL_DRIVER = "cxl.nri.io"
# the states of a preparation that kubelet may take as prepared
PREPARED = ("PREPARED", "PREPARED_UNRESTORED", "UNPREPARING")
# the states of a grant that uses its memdev, and of one that holds its
# region without a use yet
GRANT_USES = ("REALISED", "REALISED_UNRESTORED", "RELEASING")
GRANT_UNFINISHED = ("JOURNALED", "REALISING")
# the protocol's names of the lease states
LEASE_STATES = {"CREATING": "Creating", "PENDING": "Pending", "LENDING": "Lending", "READY": "Ready",
                "RELEASING": "Releasing", "RELEASING_BUSY": "Releasing", "FAILED": "Failed"}


def grant_key(ref):
    """The claim UID and result index of a grant reference, None for none."""
    return None if ref is None else (ref.get("claim"), ref.get("result"))


class Records:
    """The records of a state.json document and the driver's lookups over
    them: the first record of a reference wins."""

    def __init__(self, doc):
        self.doc = doc
        self.grants, self.regions = {}, {}
        for g in doc.get("grants") or []:
            self.grants.setdefault(grant_key(g.get("ref")), g)
        for r in doc.get("regions") or []:
            self.regions.setdefault(r.get("name") or r.get("notedName") or "", r)

    def grant(self, ref):
        return self.grants.get(grant_key(ref))

    def region(self, ref):
        return self.regions.get(ref or "")

    def prepared(self, driver):
        """The preparations of driver that kubelet may take as prepared."""
        return [p for p in self.doc.get("preparations") or []
                if p.get("driver") == driver and p.get("state") in PREPARED]

    def mount_of(self, g):
        """The node mount of grant g in its region, None if it has none."""
        got = g.get("got") or {}
        reg = self.region(got.get("region"))
        if reg is None or not got.get("mount"):
            return None
        return next((m for m in reg.get("mounts") or [] if m.get("access") == got["mount"]), None)

    def local_memdevs(self):
        """The memdev records of the local memdevs."""
        return [m for m in self.doc.get("memdevs") or [] if m.get("source") == "local"]


def prepared_uids(recs, driver):
    """The UIDs of the claims of driver that kubelet may take as prepared,
    sorted."""
    return sorted({p.get("uid") for p in recs.prepared(driver)})


def local_memdevs(recs):
    """The local memdevs: per memdev its serial and per claim UID the
    number of its grants that use the memdev (0: its grants hold the
    region without a use yet)."""
    out = []
    for m in recs.local_memdevs():
        claims = {}
        for ref in m.get("grants") or []:
            g = recs.grant(ref)
            if g is None:
                continue
            uid = ref.get("claim")
            if g.get("state") in GRANT_USES:
                claims[uid] = claims.get(uid, 0) + 1
            elif g.get("state") in GRANT_UNFINISHED:
                claims.setdefault(uid, 0)
        out.append({"serial": serial_text(m["serial"]) if m.get("serial") is not None else None, "claims": claims})
    return out


def unused_dax(recs):
    """The dax devices of the regions of ours on local memdevs that no
    grant uses, but the whole device and the seed, sorted."""
    out = set()
    for m in recs.local_memdevs():
        for ref in m.get("regions") or []:
            reg = recs.region(ref)
            if reg is None or reg.get("owner") != "ours":
                continue
            for d in reg.get("daxDevices") or []:
                if not (d.get("whole") or d.get("state") == "SEED" or d.get("users")):
                    out.add(d.get("name"))
    return sorted(out)


def lease_claims(recs):
    """The UIDs of the lease claims, sorted."""
    return sorted({c.get("uid") for c in recs.doc.get("leaseClaims") or []})


def leases(recs):
    """The leases sorted by id: id, mode, state (the protocol's name) and
    pod name."""
    out = []
    for l in recs.doc.get("leases") or []:
        out.append({"id": l.get("id"), "mode": (l.get("asked") or {}).get("mode") or "",
                    "state": LEASE_STATES.get(l.get("state"), ""), "pod": (l.get("pod") or {}).get("name") or ""})
    return sorted(out, key=lambda l: l["id"] or "")


def driver_state(doc, errors):
    """The part "state": the boot of the records, the prepared claims by
    driver, the local memdevs, the unused dax devices, the lease claims
    and the leases of the document; a part is null without a document."""
    keys = ("bootID", "prepared", "localMemdevs", "unusedDax", "leaseClaims", "leases")
    out = dict.fromkeys(keys)
    if doc is None:
        return out
    recs = Records(doc)
    parts = [
        ("bootID", lambda: doc.get("bootID") or ""),
        ("prepared", lambda: {d: prepared_uids(recs, d) for d in (LOCAL_DRIVER, POOL_DRIVER)}),
        ("localMemdevs", lambda: local_memdevs(recs)),
        ("unusedDax", lambda: unused_dax(recs)),
        ("leaseClaims", lambda: lease_claims(recs)),
        ("leases", lambda: leases(recs)),
    ]
    for key, fn in parts:
        try:
            out[key] = fn()
        except Exception as e:
            errors.append("state %s: %s" % (key, e))
    return out


def state_document(errors):
    """The document state.json and whether it was read: (None, True) if
    it does not exist."""
    try:
        return json_file(STATE_FILE), True
    except Exception as e:
        errors.append("state: %s" % e)
        return None, False


def pool_device(recs, r):
    """A device of a pool claim: the result r with what its grant asked and
    got (serial 0x0 and null facts when the grant is gone)."""
    e = {"serial": "0x0", "mode": "", "readOnly": None, "persistent": None, "blockDevice": None,
         "access": None, "path": None, "nodeMount": None, "loopDevice": None}
    g = recs.grant(r.get("grant"))
    if g is None:
        return e
    asked, got = g.get("asked") or {}, g.get("got") or {}
    mode = asked.get("mode") or ""
    e.update(serial=serial_text(g["serial"]) if g.get("serial") is not None else None, mode=mode,
             readOnly=True if asked.get("readOnly") else None)
    if mode in ("block", "fs"):
        e.update(persistent=True, blockDevice=(got.get("blockDevice") or {}).get("name") or None)
    if mode == "fs":
        m = recs.mount_of(g) or {}
        e.update(access=got.get("mount") or None, path=asked.get("path") or None,
                 nodeMount=m.get("path") or None, loopDevice=m.get("loop") or None)
    return e


def lend_of(recs, p):
    """The lend of a companion claim, None for another claim: the lease, and
    of the first grant with a lend its requester pod, container, fsDir and
    name; the targets of the lease binds of its grants (null for none)."""
    if not p.get("lease"):
        return None
    v = {"lease": p["lease"], "pod": {"namespace": "", "name": "", "uid": ""}, "container": "",
         "fsDir": None, "name": None, "mounts": None}
    identity, mounts = False, []
    for r in p.get("results") or []:
        g = recs.grant(r.get("grant"))
        if g is None:
            continue
        got = g.get("got") or {}
        lend = got.get("lend") or (g.get("intent") or {}).get("lend")
        if lend and not identity:
            pod = lend.get("pod") or {}
            v.update(pod={k: pod.get(k) or "" for k in ("namespace", "name", "uid")},
                     container=lend.get("container") or "", fsDir=lend.get("fsDir") or None,
                     name=lend.get("name") or None)
            identity = True
        b = got.get("bind")
        if b and b.get("state") in ("BOUND", "STALE") and b.get("target") not in mounts:
            mounts.append(b.get("target"))
    v.update(mounts=mounts or None)
    return v


def pool_claims(doc):
    """The part "poolClaims": per prepared claim of the pool driver its
    claimName, devices and lend; {} without a document."""
    if doc is None:
        return {}
    recs = Records(doc)
    out = {}
    for p in recs.prepared(POOL_DRIVER):
        devs = [pool_device(recs, r) for r in p.get("results") or [] if r.get("kind") == "grant"]
        out[p.get("uid")] = {"claimName": p.get("claimName") or "", "devices": devs, "lend": lend_of(recs, p)}
    return out


###
### Persistent pool devices (A24)
###

def unescape_mount(text):
    """A field of mountinfo with its octal escapes (\\040 ...) decoded."""
    return re.sub(r"\\([0-7]{3})", lambda m: chr(int(m.group(1), 8)), text)


def mountinfo(path="/proc/self/mountinfo"):
    """The entries of a mountinfo file: id, parent, majmin, root, target,
    vfsopts, optional (fields), fstype, source, superopts."""
    out = []
    with open(path) as f:
        text = f.read()
    for line in text.splitlines():
        pre, sep, post = line.partition(" - ")
        a, b = pre.split(" "), post.split(" ")
        if not sep or len(a) < 6:
            raise ValueError("%s: cannot parse %r" % (path, line))
        out.append({"id": int(a[0]), "parent": int(a[1]), "majmin": a[2], "root": unescape_mount(a[3]),
                    "target": unescape_mount(a[4]), "vfsopts": a[5], "optional": a[6:],
                    "fstype": b[0], "source": unescape_mount(b[1]) if len(b) > 1 else "",
                    "superopts": b[2] if len(b) > 2 else ""})
    return out


def loop_info(name):
    """A loop device (loopN): its backing file, dio, ro. backing_file is
    the path as the process that set the loop up saw it: after that
    process's mount namespace is gone (a driver container restarted), a
    /dev/pmemN shows as /pmemN, relative to the devtmpfs root. backing is
    normalized to /dev/pmemN then."""
    base = "/sys/block/%s" % name
    out = {"name": name, "backing": None, "dio": None, "ro": None}
    for key, path in (("backing", "loop/backing_file"), ("dio", "loop/dio"), ("ro", "ro")):
        try:
            v = read(os.path.join(base, path))
            out[key] = v if key == "backing" else v == "1"
        except OSError:
            pass
    raw = out["backing"]
    if raw and re.fullmatch(r"/pmem\d+(\.\d+)?s?", raw) and os.path.exists("/dev" + raw):
        out["backing"] = "/dev" + raw
    return out


def mounts_under(entries, base):
    """The mounts of entries (mountinfo) under the directory base, one per
    target with the number of entries (count)."""
    base = base.rstrip("/") + "/"
    out = {}
    for m in entries:
        if not m["target"].startswith(base):
            continue
        e = out.get(m["target"])
        if e is not None:
            e["count"] += 1
            continue
        e = {"target": m["target"], "source": m["source"], "fstype": m["fstype"], "root": m["root"],
             "superopts": m["superopts"], "vfsopts": m["vfsopts"], "count": 1}
        if re.fullmatch(r"/dev/loop\d+", m["source"]):
            li = loop_info(os.path.basename(m["source"]))
            e.update(backing=li["backing"], dio=li["dio"], ro=li["ro"])
        out[m["target"]] = e
    return [out[t] for t in sorted(out)]


def loops_over_pmem():
    out = []
    for p in sorted(glob.glob("/sys/block/loop[0-9]*"), key=natkey):
        li = loop_info(os.path.basename(p))
        if (li["backing"] or "").startswith("/dev/pmem"):
            out.append(li)
    return out


def listdir(path):
    """The names in directory path, sorted; None if it does not exist."""
    try:
        return sorted(os.listdir(path))
    except FileNotFoundError:
        return None


def lease_dirs():
    """The names in lease-fs/ (per lease claim uid, the names in it: the
    mount points of its binds, not followed), leases/ and mounts/."""
    fs = None
    names = listdir(LEASE_FS)
    if names is not None:
        fs = {}
        for uid in names:
            try:
                fs[uid] = sorted(os.listdir(os.path.join(LEASE_FS, uid)))
            except NotADirectoryError:
                fs[uid] = None
            except FileNotFoundError:
                continue
    return {"leaseFs": fs, "leases": listdir(LEASES_DIR), "mounts": listdir(POOL_MOUNTS)}


def int_or_none(text):
    try:
        return int(text, 0)
    except (TypeError, ValueError):
        return None


def nd_namespace(path):
    """A namespace of an nvdimm region with its pfn device and block device."""
    name = os.path.basename(path)
    ns = {"name": name, "mode": None, "size": None, "uuid": None, "holder": None, "map": None,
          "blockdev": None, "major": None, "minor": None}
    for key in ("mode", "uuid", "holder"):
        try:
            ns[key] = read(path + "/" + key) or None
        except OSError:
            pass
    try:
        ns["size"] = int(read(path + "/size"), 0)
    except (OSError, ValueError):
        pass
    blocks = glob.glob(path + "/block/pmem*")
    if ns["holder"]:
        hp = ND + "/" + ns["holder"]
        try:
            ns["map"] = read(hp + "/mode")
        except OSError:
            pass
        blocks = glob.glob(hp + "/block/pmem*") or blocks
    if blocks:
        bd = os.path.basename(sorted(blocks, key=natkey)[0])
        ns["blockdev"] = bd
        try:
            major, minor = read("/sys/block/%s/dev" % bd).split(":")
            ns["major"], ns["minor"] = int(major), int(minor)
        except (OSError, ValueError):
            pass
    return ns


def nd_regions():
    """The physical start address -> path of every nvdimm region."""
    out = {}
    for r in glob.glob(ND + "/region[0-9]*"):
        try:
            out[int(read(r + "/resource"), 0)] = r
        except (OSError, ValueError):
            continue
    return out


def pmem(errors):
    """Per CXL region of mode pmem: its nvdimm region and namespaces."""
    eps = endpoint_memdevs()
    serials = {}
    for p in glob.glob(CXL + "/mem[0-9]*"):
        try:
            serials[os.path.basename(p)] = serial_text(read(p + "/serial"))
        except OSError:
            continue
    nds = nd_regions()
    out = []
    for p in sorted(glob.glob(CXL + "/region[0-9]*"), key=natkey):
        name = os.path.basename(p)
        try:
            if read(p + "/mode") != "pmem":
                continue
        except OSError:
            continue
        r = {"region": name, "serials": [], "resource": None, "size": None, "ndRegion": None, "namespaces": []}
        try:
            r["resource"] = int_or_none(read(p + "/resource"))
            r["size"] = int_or_none(read(p + "/size"))
        except OSError as e:
            errors.append("pmem %s: %s" % (name, e))
        for t in sorted(glob.glob(p + "/target[0-9]*"), key=natkey):
            try:
                m = re.fullmatch(r"decoder(\d+)\.\d+", read(t))
            except OSError:
                continue
            mem = eps.get(m.group(1)) if m else None
            if mem:
                r["serials"].append(serials.get(mem, mem))
        nd = nds.get(r["resource"]) if r["resource"] is not None else None
        if nd:
            r["ndRegion"] = os.path.basename(nd)
            for ns in sorted(glob.glob(nd + "/namespace[0-9]*.[0-9]*"), key=natkey):
                entry = nd_namespace(ns)
                if entry["size"]:
                    r["namespaces"].append(entry)
        out.append(r)
    return out


def cdi_pool(errors):
    """The CDI devices of the pool driver's spec file: name -> deviceNodes,
    mounts."""
    out = {}
    path = CDI_DIR + "/cxl-pool-claims.json"
    try:
        with open(path) as f:
            spec = json.load(f)
    except FileNotFoundError:
        return out
    except Exception as e:
        errors.append("cdiPool %s: %s: %s" % (path, type(e).__name__, e))
        return out
    for dev in (spec or {}).get("devices") or []:
        ed = dev.get("containerEdits") or {}
        out[dev.get("name")] = {
            "deviceNodes": [{k: n.get(k) for k in ("path", "type", "permissions")}
                            for n in ed.get("deviceNodes") or []],
            "mounts": [{k: m.get(k) for k in ("hostPath", "containerPath", "options")}
                       for m in ed.get("mounts") or []],
        }
    return out


KERNEL_STATE = DRIVER_STATE_DIR + "/kernel.json"
KERNEL_MATCH = re.compile(r"BUG:|Oops|WARNING:|general protection|nd_pmem|pfn|erofs")
KERNEL_BAD = re.compile(r"BUG:|Oops|WARNING:|general protection")
KERNEL_LINES = 20


def kernel(errors):
    """The kernel log records since the last observation that match
    KERNEL_MATCH (A24)."""
    st = None
    try:
        with open(KERNEL_STATE) as f:
            st = json.load(f)
    except (OSError, ValueError):
        pass
    first = st is None
    st = st or {"seq": -1}
    out = {"lines": [], "count": 0, "bad": 0, "seq": st["seq"]}
    fd = os.open("/dev/kmsg", os.O_RDONLY | os.O_NONBLOCK)
    try:
        while True:
            try:
                rec = os.read(fd, 8192)
            except BlockingIOError:
                break
            except OSError as e:
                if e.errno == errno.EPIPE:
                    continue  # records overwritten while reading
                raise
            head, _, msg = rec.decode(errors="replace").partition(";")
            f = head.split(",")
            try:
                seq, ts = int(f[1]), int(f[2])
            except (IndexError, ValueError):
                continue
            if seq <= st["seq"]:
                continue
            st["seq"] = out["seq"] = seq
            msg = msg.split("\n", 1)[0]
            if first or not KERNEL_MATCH.search(msg):
                continue  # the first observation is the baseline: the log from then on
            out["count"] += 1
            line = "[%d.%06d] %s" % (ts // 1000000, ts % 1000000, msg[:300])
            if len(out["lines"]) < KERNEL_LINES:
                out["lines"].append(line)
            if KERNEL_BAD.search(msg):
                out["bad"] += 1
    finally:
        os.close(fd)
    try:
        os.makedirs(DRIVER_STATE_DIR, exist_ok=True)
        with open(KERNEL_STATE + ".tmp", "w") as f:
            json.dump(st, f)
        os.replace(KERNEL_STATE + ".tmp", KERNEL_STATE)
    except OSError as e:
        errors.append("kernel: %s: %s" % (KERNEL_STATE, e))
    return out


def kill_at_state():
    try:
        with open(KILLAT_DIR + "/state.json") as f:
            return json.load(f)
    except FileNotFoundError:
        return None


def vm_observation(ns):
    errors = []
    obs = {}
    obs["bootID"] = part(errors, "bootID", read, "/proc/sys/kernel/random/boot_id")
    obs["memdevs"] = part(errors, "memdevs", memdevs, errors)
    obs["regions"] = part(errors, "regions", regions, errors)
    obs["ports"] = part(errors, "ports", ports, errors)
    obs["numaNodes"] = part(errors, "numaNodes", numa_nodes)
    doc, doc_read = state_document(errors)
    obs["state"] = part(errors, "state", driver_state, doc, errors)
    obs["cdi"] = part(errors, "cdi", cdi, errors)
    ctr_mounts = []
    obs["containers"] = part(errors, "containers", containers, ns, errors, ctr_mounts)
    obs["ctrMounts"] = ctr_mounts if obs["containers"] is not None else None
    obs["driverLog"] = part(errors, "driverLog", driver_log, errors)
    # Persistent pool devices (A24).
    obs["pmem"] = part(errors, "pmem", pmem, errors)
    obs["cxlDevices"] = part(errors, "cxlDevices", cxl_devices, obs["regions"], obs["pmem"])
    mi = part(errors, "mountinfo", mountinfo)
    obs["fsMounts"] = None if mi is None else mounts_under(mi, POOL_MOUNTS)
    obs["leaseBinds"] = None if mi is None else mounts_under(mi, LEASE_FS)
    obs["leaseDirs"] = part(errors, "leaseDirs", lease_dirs)
    obs["loops"] = part(errors, "loops", loops_over_pmem)
    obs["poolClaims"] = part(errors, "poolClaims", pool_claims, doc) if doc_read else None
    obs["cdiPool"] = part(errors, "cdiPool", cdi_pool, errors)
    obs["kernel"] = part(errors, "kernel", kernel, errors)
    obs["killAt"] = part(errors, "killAt", kill_at_state)
    obs["errors"] = errors
    return obs


###
### The cluster (contract 6.1, the parts of kubectl and systemd)
###

def id_map(text):
    """"k=v,k=v" -> {k: v}."""
    out = {}
    for kv in (text or "").split(","):
        k, sep, v = kv.partition("=")
        if sep:
            out[k.strip()] = v.strip()
    return out


def slot_of_label(label):
    """mbt.slot label KIND_VM_I -> slot KIND@VM#I."""
    parts = (label or "").split("_")
    if len(parts) != 3:
        return label
    return "%s@%s#%s" % tuple(parts)


def condition(obj, ctype):
    for c in (obj.get("status") or {}).get("conditions") or []:
        if c.get("type") == ctype:
            return c
    return None


def container_status(pod, name):
    for cs in (pod.get("status") or {}).get("containerStatuses") or []:
        if cs.get("name") == name:
            return cs
    return None


def claim_parts(pod, claims):
    """The claim parts of every claim of the pod, in the order of its
    spec (dnuma, A39: c0 of mbt-lnuma, c1 of mbt-xnuma-c)."""
    spec_claims = (pod.get("spec") or {}).get("resourceClaims") or []
    return [claim_part(pod, claims, i) for i in range(len(spec_claims))]


def claim_part(pod, claims, index=0):
    """Claim index of the spec of the pod (contract 6.1 "claim"), None if
    it has none."""
    spec_claims = (pod.get("spec") or {}).get("resourceClaims") or []
    if index >= len(spec_claims):
        return None
    cname = spec_claims[index].get("name")
    name = spec_claims[index].get("resourceClaimName")
    for s in (pod.get("status") or {}).get("resourceClaimStatuses") or []:
        if s.get("name") == cname and s.get("resourceClaimName"):
            name = s["resourceClaimName"]
    claim = claims.get(name) if name else None
    if claim is None:
        return {"name": name, "uid": None, "allocated": False, "devices": [], "attached": None, "attachFailed": None}
    st = claim.get("status") or {}
    alloc = st.get("allocation")
    results = ((alloc or {}).get("devices") or {}).get("results") or []
    devices = [{"driver": r.get("driver"), "pool": r.get("pool"), "device": r.get("device")} for r in results]
    # Attached: every pool device of the allocation has the condition
    # Attached True; attachFailed: one has AttachFailed True (A44 (4))
    attached = attach_failed = None
    pool_results = [r for r in results if r.get("driver") == POOL_DRIVER]
    if pool_results:
        attached, attach_failed = True, False
        for r in pool_results:
            ok = False
            for d in st.get("devices") or []:
                if (d.get("driver"), d.get("pool"), d.get("device")) != (r.get("driver"), r.get("pool"), r.get("device")):
                    continue
                for c in d.get("conditions") or []:
                    if c.get("status") != "True":
                        continue
                    if c.get("type") == POOL_DRIVER + "/Attached":
                        ok = True
                    elif c.get("type") == POOL_DRIVER + "/AttachFailed":
                        attach_failed = True
            attached = attached and ok
    return {"name": name, "uid": (claim.get("metadata") or {}).get("uid"),
            "allocated": alloc is not None, "devices": devices, "attached": attached,
            "attachFailed": attach_failed}


def event_time(e):
    """The time of event e as a sortable string (RFC 3339, UTC)."""
    t = (e.get("series") or {}).get("lastObservedTime") or e.get("lastTimestamp") \
        or e.get("eventTime") or (e.get("metadata") or {}).get("creationTimestamp") or ""
    # 2026-10-24T08:10:51Z and 2026-10-24T08:10:51.123456Z sort alike.
    t = t.replace("Z", "")
    if not t:
        return ""
    head, _, frac = t.partition(".")
    return head + "." + (frac + "000000")[:6]


MAX_EVENTS = 5
MAX_MESSAGE = 4000


def short_message(text):
    """text in at most MAX_MESSAGE bytes (UTF-8): a longer one keeps its
    start and its end, where the driver puts the reason of a failure."""
    b = (text or "").encode()
    if len(b) <= MAX_MESSAGE:
        return text or ""
    return (b[:1900].decode(errors="ignore") + " [...] " + b[-2000:].decode(errors="ignore"))


def warning_events(items):
    """Pod uid -> its Warning events, newest first, at most MAX_EVENTS:
    reason, message."""
    by_uid = {}
    for e in items:
        io = e.get("involvedObject") or {}
        if io.get("kind") != "Pod" or not io.get("uid"):
            continue
        by_uid.setdefault(io["uid"], []).append((event_time(e), {
            "reason": e.get("reason"), "message": short_message(e.get("message"))}))
    return {uid: [ev for _, ev in sorted(evs, key=lambda x: x[0], reverse=True)[:MAX_EVENTS]]
            for uid, evs in by_uid.items()}


def container_states(pod):
    """The containers of the pod status: name, id (without the runtime
    prefix), state (running, waiting, terminated), restarts."""
    out = []
    for cs in (pod.get("status") or {}).get("containerStatuses") or []:
        state = next(iter((cs.get("state") or {}).keys()), None)
        out.append({"name": cs.get("name"), "id": (cs.get("containerID") or "").split("://")[-1] or None,
                    "state": state, "restarts": cs.get("restartCount") or 0})
    return out


def slices_part(items, vm_of):
    """VM -> the devices of its cxl.nri.io ResourceSlices: name, serial,
    shared, type."""
    def attr(dev, name):
        attrs = dev.get("attributes") or {}
        a = attrs.get(name)
        if a is None:
            a = next((v for k, v in attrs.items() if k.endswith("/" + name)), None)
        return next(iter(a.values()), None) if isinstance(a, dict) and a else None
    out = {}
    for sl in items:
        spec = sl.get("spec") or {}
        if spec.get("driver") != "cxl.nri.io" or not spec.get("nodeName"):
            continue
        vm = vm_of.get(spec["nodeName"], spec["nodeName"])
        for d in spec.get("devices") or []:
            out.setdefault(vm, []).append({"name": d.get("name"), "serial": attr(d, "serial"),
                                           "shared": bool(attr(d, "shared")), "type": attr(d, "type")})
    for vm in out:
        out[vm].sort(key=lambda d: d["name"] or "")
    return out


def pool_slices_part(items, vm_of, pools):
    """Pool id -> the cxl-pool.nri.io ResourceSlices of its pool: name,
    devices (names), nodes (VM ids of the nodeSelector; every VM for
    allNodes), [] for a pool without slices (amendment A18). pools maps
    pool ids to DRA pool names; a slice of another pool is keyed by its
    pool name."""
    id_of = {name: pid for pid, name in pools.items()}
    out = {pid: [] for pid in pools}
    for sl in items:
        spec = sl.get("spec") or {}
        if spec.get("driver") != POOL_DRIVER:
            continue
        name = (spec.get("pool") or {}).get("name")
        nodes = set()
        if spec.get("allNodes"):
            nodes.update(vm_of.values())
        if spec.get("nodeName"):
            nodes.add(vm_of.get(spec["nodeName"], spec["nodeName"]))
        for term in (spec.get("nodeSelector") or {}).get("nodeSelectorTerms") or []:
            for e in term.get("matchExpressions") or []:
                if e.get("key") == "kubernetes.io/hostname" and e.get("operator") == "In":
                    nodes.update(vm_of.get(v, v) for v in e.get("values") or [])
        out.setdefault(id_of.get(name, name), []).append({
            "name": (sl.get("metadata") or {}).get("name"),
            "devices": sorted(d.get("name") for d in spec.get("devices") or []),
            "nodes": sorted(nodes)})
    for pid in out:
        out[pid].sort(key=lambda x: x["name"] or "")
    return out


def pod_part(pod, claims, vm_of, events=None):
    md = pod.get("metadata") or {}
    st = pod.get("status") or {}
    cs = container_status(pod, MAIN) or {}
    ready = condition(pod, "Ready")
    sched = condition(pod, "PodScheduled")
    m = re.search(r"-g(\d+)$", md.get("name") or "")
    node = (pod.get("spec") or {}).get("nodeName")
    waiting = ((cs.get("state") or {}).get("waiting") or {}).get("reason")
    return {
        "slot": slot_of_label((md.get("labels") or {}).get(SLOT_LABEL)),
        "name": md.get("name"),
        "uid": md.get("uid"),
        "gen": int(m.group(1)) if m else None,
        "node": vm_of.get(node, node) if node else None,
        "phase": st.get("phase"),
        "ready": bool(ready and ready.get("status") == "True"),
        "deleting": bool(md.get("deletionTimestamp")),
        "restarts": cs.get("restartCount") or 0,
        "waiting": waiting,
        "unschedulable": bool(sched and sched.get("status") == "False" and sched.get("reason") == "Unschedulable"),
        "claim": claim_part(pod, claims),
        "claims": claim_parts(pod, claims),
        "events": (events or {}).get(md.get("uid")) or [],
        "event": next(iter((events or {}).get(md.get("uid")) or []), None),
        "containers": container_states(pod),
    }


def driver_part(pods, vm_of):
    by_vm = {}
    for p in pods:
        md = p.get("metadata") or {}
        node = (p.get("spec") or {}).get("nodeName")
        vm = vm_of.get(node, node)
        cs = container_status(p, DRIVER_CONTAINER) or {}
        ready = condition(p, "Ready")
        entry = {"pod": md.get("name"), "ready": bool(ready and ready.get("status") == "True"),
                 "restarts": cs.get("restartCount") or 0}
        rank = (bool(md.get("deletionTimestamp")), not entry["ready"])
        # The pod of the node, not a terminating one of an earlier restart.
        if vm not in by_vm or by_vm[vm][0] > rank:
            by_vm[vm] = (rank, entry)
    return {vm: entry for vm, (_, entry) in by_vm.items()}


# The states of a lease on its way to Ready (pkg/cxl/api).
LEASE_UNREADY = ("Creating", "Pending", "Lending")


def leases_unready(leases):
    """Whether an inspection lists a lease on its way to Ready."""
    return any(isinstance(l, dict) and l.get("state") in LEASE_UNREADY for l in leases or [])


def cxl_leases(text):
    text = text.strip()
    try:
        j = json.loads(text) if text else []
        if isinstance(j, dict):
            j = j.get("leases", [j] if "lease" in j else [])
        return j or []
    except ValueError:
        return [json.loads(l) for l in text.splitlines() if l.strip().startswith("{")]


# The kinds of persistent pool devices (A22) and the lease kind of
# persistent leases: inspected with one exec (inspect_persistent).
PERSISTENT_BASES = ("pwr", "pfsd", "pfsc", "pbro", "pbad", "xpb", "xpf")
# The script of that exec: the environment, the container's mountinfo
# (read by cat in the container's mount namespace, no file under a mount
# opened), the names in /dev, and the leases of a lease kind.
INSPECT_SCRIPT = """env
echo @@mountinfo
cat /proc/self/mountinfo
echo @@dev
ls -1 /dev
if [ "$1" = lease ]; then echo @@leases; exec %s list; fi
""" % CXL_REQUEST


def persistent_kind(kind):
    return kind == "plse" or kind.split("-")[0] in PERSISTENT_BASES


def inspect_sections(text):
    """The sections of the output of INSPECT_SCRIPT: name -> lines."""
    out, cur = {"env": []}, "env"
    for line in text.splitlines():
        if line.startswith("@@") and line[2:] in ("mountinfo", "dev", "leases"):
            cur = line[2:]
            out[cur] = []
            continue
        out[cur].append(line)
    return out


def inspect_persistent(ns, pod, kind, out, errors, lock):
    """inspect of a pod of a persistent kind or plse (A24): env, the mount
    of CXL_<KEY>_PATH, the mounts under /run/cxl/fs, the /dev/pmem* nodes,
    and the leases of plse."""
    name = pod["name"]
    lease = kind == "plse"
    r = run(["kubectl", "exec", "-n", ns, name, "-c", MAIN, "--", "sh", "-c", INSPECT_SCRIPT, "sh",
             "lease" if lease else "claim"], timeout=EXEC_TIMEOUT)
    if r.returncode != 0:
        with lock:
            errors.append("inspect %s: exit %d: %s" % (name, r.returncode, (r.stdout + r.stderr).strip()[-200:]))
        return
    sec = inspect_sections(r.stdout)
    env = {}
    for line in sec.get("env") or []:
        k, sep, v = line.partition("=")
        if sep and k.startswith("CXL_"):
            env[k] = v
    mounts, mount = [], None
    try:
        # mountinfo() reads a file: parse the lines of the section the same way.
        entries = []
        for line in sec.get("mountinfo") or []:
            pre, sep, post = line.partition(" - ")
            a, b = pre.split(" "), post.split(" ")
            if not sep or len(a) < 6:
                continue
            entries.append({"root": unescape_mount(a[3]), "target": unescape_mount(a[4]), "vfsopts": a[5],
                            "fstype": b[0], "source": unescape_mount(b[1]) if len(b) > 1 else "",
                            "superopts": b[2] if len(b) > 2 else ""})
        paths = [v for k, v in env.items() if k.endswith("_PATH") and not v.startswith("/dev/")]
        for e in entries:
            if e["target"] in paths:
                mount = e
            if e["target"].startswith(LEASE_FS_CTR + "/"):
                mounts.append(e)
    except Exception as e:
        with lock:
            errors.append("inspect %s: mountinfo: %s" % (name, e))
    leases = None
    if lease:
        try:
            leases = cxl_leases("\n".join(sec.get("leases") or []))
        except ValueError as e:
            with lock:
                errors.append("inspect %s: cxl-request list: %s" % (name, e))
            return
    pmem_nodes = sorted("/dev/" + n for n in sec.get("dev") or [] if n.startswith("pmem"))
    with lock:
        out[pod["slot"]] = {"uid": pod["uid"], "restarts": pod["restarts"], "env": env, "leases": leases,
                            "mount": mount, "mounts": sorted(mounts, key=lambda m: m["target"]),
                            "pmemNodes": pmem_nodes}


def inspect_pod(ns, pod, kind, out, errors, lock):
    if persistent_kind(kind):
        inspect_persistent(ns, pod, kind, out, errors, lock)
        return
    name = pod["name"]
    r = run(["kubectl", "exec", "-n", ns, name, "-c", MAIN, "--", "env"], timeout=EXEC_TIMEOUT)
    if r.returncode != 0:
        with lock:
            errors.append("inspect %s: env: exit %d: %s" % (name, r.returncode, r.stderr.strip()[-200:]))
        return
    env = {}
    for line in r.stdout.splitlines():
        k, sep, v = line.partition("=")
        if sep and k.startswith("CXL_"):
            env[k] = v
    leases = None
    if kind == "lease":
        r = run(["kubectl", "exec", "-n", ns, name, "-c", MAIN, "--", CXL_REQUEST, "list"], timeout=EXEC_TIMEOUT)
        if r.returncode != 0:
            with lock:
                errors.append("inspect %s: cxl-request list: exit %d: %s" % (name, r.returncode, (r.stdout + r.stderr).strip()[-200:]))
            return
        try:
            leases = cxl_leases(r.stdout)
        except ValueError as e:
            with lock:
                errors.append("inspect %s: cxl-request list: %s" % (name, e))
            return
    with lock:
        out[pod["slot"]] = {"uid": pod["uid"], "restarts": pod["restarts"], "env": env, "leases": leases}


def cluster_observation(ns, nodes, units, inspected, pools=None):
    errors = []
    vm_of = {node: vm for vm, node in nodes.items()}
    results = {}

    def get(key, args):
        try:
            r = run(args, timeout=60)
            if r.returncode != 0:
                raise RuntimeError("%s: exit %d: %s" % (" ".join(args[:4]), r.returncode, r.stderr.strip()[-300:]))
            results[key] = r.stdout
        except Exception as e:
            errors.append("%s: %s" % (key, e))

    unit_names = [units[p] for p in sorted(units)]
    jobs = [
        ("ns", ["kubectl", "get", "pods,resourceclaims", "-n", ns, "-o", "json"]),
        ("drivers", ["kubectl", "get", "pods", "-n", "kube-system", "-l", DRIVER_LABEL, "-o", "json"]),
        ("events", ["kubectl", "get", "events", "-n", ns, "--field-selector", "type=Warning", "-o", "json"]),
        ("slices", ["kubectl", "get", "resourceslices", "-o", "json"]),
    ]
    threads = [threading.Thread(target=get, args=job) for job in jobs]
    for t in threads:
        t.start()
    # systemctl is-active exits non-zero when a unit is not active: not an error.
    try:
        r = run(["systemctl", "is-active"] + unit_names, timeout=30)
        states = r.stdout.split()
        if len(states) != len(unit_names):
            raise RuntimeError("systemctl is-active printed %r" % r.stdout)
        controllers = {p: states[i] for i, p in enumerate(sorted(units))}
    except Exception as e:
        errors.append("controllers: %s" % e)
        controllers = None
    for t in threads:
        t.join()

    out = {"pods": None, "companions": None, "others": None, "drivers": None,
           "controllers": controllers, "inspect": None, "slices": None, "poolSlices": None}
    if "slices" in results:
        try:
            items = json.loads(results["slices"]).get("items") or []
            out["slices"] = slices_part(items, vm_of)
            out["poolSlices"] = pool_slices_part(items, vm_of, pools or {p: "pool-" + p for p in units})
        except Exception as e:
            errors.append("slices: %s: %s" % (type(e).__name__, e))
    events = None
    if "events" in results:
        try:
            events = warning_events(json.loads(results["events"]).get("items") or [])
        except Exception as e:
            errors.append("events: %s: %s" % (type(e).__name__, e))
    if "ns" in results:
        try:
            items = json.loads(results["ns"]).get("items") or []
            pods = [i for i in items if i.get("kind") == "Pod"]
            claims = {(i.get("metadata") or {}).get("name"): i for i in items if i.get("kind") == "ResourceClaim"}
            slot_pods, companions, others = [], [], []
            for p in pods:
                labels = (p.get("metadata") or {}).get("labels") or {}
                if SLOT_LABEL in labels:
                    slot_pods.append(pod_part(p, claims, vm_of, events))
                elif LEASE_LABEL in labels:
                    companions.append(p)
                else:
                    others.append((p.get("metadata") or {}).get("name"))
            by_uid = {sp["uid"]: sp["slot"] for sp in slot_pods}
            comp = []
            for p in companions:
                md = p.get("metadata") or {}
                labels = md.get("labels") or {}
                node = (p.get("spec") or {}).get("nodeName")
                comp.append({"name": md.get("name"), "node": vm_of.get(node, node) if node else None,
                             "containers": container_states(p),
                             "phase": (p.get("status") or {}).get("phase"),
                             "deleting": bool(md.get("deletionTimestamp")),
                             "lease": labels.get(LEASE_LABEL),
                             "requester": by_uid.get(labels.get(REQUESTER_LABEL))})
            out["pods"] = sorted(slot_pods, key=lambda sp: (sp["slot"] or "", sp["name"] or ""))
            out["companions"] = sorted(comp, key=lambda c: c["name"] or "")
            out["others"] = sorted(n for n in others if n)
        except Exception as e:
            errors.append("pods: %s: %s" % (type(e).__name__, e))
    if "drivers" in results:
        try:
            out["drivers"] = driver_part(json.loads(results["drivers"]).get("items") or [], vm_of)
        except Exception as e:
            errors.append("drivers: %s: %s" % (type(e).__name__, e))
    if out["pods"] is not None:
        inspect, lock, threads = {}, threading.Lock(), []
        for sp in out["pods"]:
            if sp["phase"] != "Running" or not sp["ready"] or sp["deleting"]:
                continue
            if "%s:%s" % (sp["uid"], sp["restarts"]) in inspected:
                continue
            kind = (sp["slot"] or "").split("@")[0]
            t = threading.Thread(target=inspect_pod, args=(ns, sp, kind, inspect, errors, lock))
            t.start()
            threads.append(t)
        for t in threads:
            t.join()
        out["inspect"] = inspect
    out["errors"] = errors
    return out


STACKS_MARKER = "mbt-observe: the observation is still running, stacks follow"


SYSCALL_NAMES = {"0": "read", "1": "write", "17": "pread64", "18": "pwrite64"}


def task_syscall(pid, task):
    """The system call of thread task of process pid
    (/proc/PID/task/TID/syscall): of a read or write its name and the file
    of its fd, else its number, or "running"."""
    try:
        words = read(task + "/syscall").split()
    except OSError as e:
        return "(%s)" % e.strerror
    name = SYSCALL_NAMES.get(words[0] if words else "")
    if name is None or len(words) < 2:
        return " ".join(words[:1])
    try:
        return "%s %s" % (name, os.readlink("/proc/%d/fd/%d" % (pid, int(words[1], 16))))
    except (OSError, ValueError):
        return "%s fd %s" % (name, words[1])


def task_stacks(pid):
    """Lines of the kernel stacks of the threads of process pid: per
    thread its name, state, wait channel, system call (task_syscall) and
    /proc/PID/task/TID/stack, which kernels without CONFIG_STACKTRACE do
    not have."""
    out = []
    for task in sorted(glob.glob("/proc/%d/task/[0-9]*" % pid), key=natkey):
        fields = {}
        for name in ("comm", "wchan", "stat", "stack"):
            try:
                fields[name] = read(task + "/" + name)
            except OSError as e:
                fields[name] = "(%s)" % e.strerror
        state = fields["stat"].rsplit(")", 1)[-1].split()[:1] or ["?"]
        out.append("-- task %s %s state %s wchan %s syscall %s" % (os.path.basename(task), fields["comm"], state[0],
                                                                 fields["wchan"], task_syscall(pid, task)))
        out.extend("   " + line for line in fields["stack"].splitlines())
    return out


def driver_pids():
    """The pids of the processes of the driver binary (argv[0] is
    .../DRIVER_CONTAINER), from /proc only."""
    out = []
    for d in glob.glob("/proc/[0-9]*"):
        try:
            with open(d + "/cmdline", "rb") as f:
                argv0 = f.read().split(b"\0", 1)[0].decode(errors="replace")
        except OSError:
            continue
        if os.path.basename(argv0) == DRIVER_CONTAINER:
            out.append(int(os.path.basename(d)))
    return sorted(out)


def write_stacks():
    """Write STACKS_MARKER and the kernel stacks of the threads of the
    driver processes and of this process to stderr."""
    lines = [STACKS_MARKER]
    for pid in driver_pids():
        lines.append("== driver pid %d" % pid)
        lines.extend(task_stacks(pid))
    lines.append("== observer pid %d" % os.getpid())
    lines.extend(task_stacks(os.getpid()))
    sys.stderr.write("\n".join(lines) + "\n")
    sys.stderr.flush()


def arm_stacks(seconds):
    """After seconds, write write_stacks and a second later the Python
    stacks of every thread to stderr; the returned function cancels
    both."""
    t = threading.Timer(seconds, write_stacks)
    t.daemon = True
    t.start()
    faulthandler.dump_traceback_later(seconds + 1, exit=False)

    def cancel():
        t.cancel()
        faulthandler.cancel_dump_traceback_later()
    return cancel


def cmd_vm(args):
    cancel = arm_stacks(args.stacks_after) if args.stacks_after > 0 else None
    out = {}
    threads = []
    if args.cluster:
        def cluster():
            out["cluster"] = cluster_observation(args.ns, id_map(args.nodes), id_map(args.units),
                                                 set(":".join(i.split(":")[:2]) for i in (args.inspected or "").split(",") if i),
                                                 id_map(args.pools) or None)
        threads.append(threading.Thread(target=cluster))
        threads[-1].start()
    out["vm"] = vm_observation(args.ns)
    for t in threads:
        t.join()
    if cancel:
        cancel()
    print(json.dumps(out, sort_keys=True, separators=(",", ":")))


###
### driver-kill-at (A23): a one-shot watcher in the VM
###

def write_json(path, obj):
    with open(path + ".tmp", "w") as f:
        json.dump(obj, f, sort_keys=True)
    os.replace(path + ".tmp", path)


def proc_start(pid):
    """The start time of process pid (field 22 of /proc/PID/stat), None if
    it is gone."""
    try:
        with open("/proc/%d/stat" % pid) as f:
            return int(f.read().rsplit(")", 1)[1].split()[19])
    except (OSError, IndexError, ValueError):
        return None


def driver_process():
    """(container id, pid) of the running driver container of the node."""
    r = run(["crictl", "ps", "-q", "--state", "running", "--namespace", "^kube-system$",
             "--name", "^%s$" % DRIVER_CONTAINER])
    ids = r.stdout.split() if r.returncode == 0 else []
    if not ids:
        return None, None
    info = crictl_inspect(ids[:1])
    pid = ((info[0].get("info") or {}).get("pid") or 0) if info else 0
    return ids[0], pid or None


class Marker:
    """The marker of a persistent Prepare that kill-at waits for. seen()
    returns what it saw that was not there when it was armed, else None:
    what disappears is forgotten, so that a region or mount made again
    with the name of an old one counts as new."""

    def __init__(self, kind):
        self.kind = kind
        self.ignore = set()   # new regions of another mode than pmem
        self.base = self.present()

    def present(self):
        if self.kind in ("region", "ns"):
            return set(n for n in os.listdir(CXL) if n.startswith("region"))
        base = (POOL_MOUNTS if self.kind == "mount" else LEASE_FS).rstrip("/") + "/"
        with open("/proc/self/mountinfo") as f:
            text = f.read()
        out = set()
        for line in text.splitlines():
            a = line.split(" ", 5)
            if len(a) >= 5 and a[4].startswith(base):
                out.add((a[0], unescape_mount(a[4])))
        return out

    def seen(self):
        now = self.present()
        self.base &= now
        self.ignore &= now
        new = now - self.base - self.ignore
        if self.kind in ("mount", "bind"):
            return sorted(t for _, t in new)[0] if new else None
        for name in sorted(new):
            p = CXL + "/" + name
            try:
                mode = read(p + "/mode")
            except OSError:
                continue
            if mode != "pmem":
                if mode:
                    self.ignore.add(name)  # a ram region: never this marker
                continue
            if self.kind == "region":
                return name
            try:
                res = int(read(p + "/resource"), 0)
            except (OSError, ValueError):
                continue
            nd = nd_regions().get(res)
            if not nd:
                continue
            for ns in glob.glob(nd + "/namespace[0-9]*.[0-9]*"):
                try:
                    if int(read(ns + "/size"), 0) > 0:
                        return "%s %s %s" % (name, os.path.basename(nd), os.path.basename(ns))
                except (OSError, ValueError):
                    continue
        return None


def pool_claims_now():
    """The part "poolClaims" of state.json now, None if it cannot be
    read."""
    try:
        return pool_claims(json_file(STATE_FILE))
    except Exception:
        return None


class DriverProcess:
    """The driver process that a watcher was armed against, and only that
    one: a pidfd where the kernel has them (signals go to that process or
    fail), else its pid and start time."""

    def __init__(self, pid):
        self.pid, self.start, self.fd = pid, proc_start(pid), None
        try:
            self.fd = os.pidfd_open(pid)
        except (AttributeError, OSError):
            pass

    def alive(self):
        if self.fd is not None:
            r, _, _ = select.select([self.fd], [], [], 0)
            return not r  # a pidfd is readable once its process exited
        return self.start is not None and proc_start(self.pid) == self.start

    def kill(self):
        if self.fd is not None:
            signal.pidfd_send_signal(self.fd, signal.SIGKILL)
        elif self.alive():
            os.kill(self.pid, signal.SIGKILL)
        else:
            raise ProcessLookupError("process %d is gone" % self.pid)


def cmd_kill_at(args):
    """A23 driver-kill-at: the watcher. It is bound to the driver process
    of the container running when it was armed: if that process ends
    first (driver-kill, driver-restart, containerd-restart, a crash), the
    watcher ends, done and not fired, reason "driver restarted"; it never
    kills another process. reason: fired, expired, driver restarted,
    disarmed by VERB (kill-at-disarm), or an error."""
    os.makedirs(KILLAT_DIR, exist_ok=True)
    statefile = KILLAT_DIR + "/state.json"
    t0 = time.monotonic()
    st = {"marker": args.marker, "armed": False, "fired": False, "done": False, "ms": None, "seen": None,
          "pid": None, "container": None, "watcher": os.getpid(), "armedAt": time.time(), "error": None,
          "reason": None}
    cid, pid = driver_process()
    if not pid:
        st.update(done=True, error="no running driver container", reason="no running driver container")
        write_json(statefile, st)
        print(json.dumps(st, sort_keys=True))
        return
    drv = DriverProcess(pid)
    marker = Marker(args.marker)
    claims = pool_claims_now()
    st.update(armed=True, pid=pid, container=cid)
    write_json(statefile, st)
    print(json.dumps(st, sort_keys=True), flush=True)
    interval = args.interval / 1000.0
    while time.monotonic() - t0 < args.timeout:
        if not drv.alive():
            st.update(done=True, reason="driver restarted", ms=int((time.monotonic() - t0) * 1000))
            write_json(statefile, st)
            return
        try:
            hit = marker.seen()
        except OSError:
            hit = None
        if hit:
            killed_at = time.monotonic()
            err = None
            try:
                drv.kill()
            except OSError as e:
                err = "kill %s: %s" % (pid, e)
            st.update(fired=err is None, done=True, seen=hit, error=err,
                      reason="fired" if err is None else "driver restarted",
                      ms=int((killed_at - t0) * 1000), stateChanged=pool_claims_now() != claims)
            write_json(statefile, st)
            return
        time.sleep(interval)
    st.update(done=True, expired=True, reason="expired", ms=int((time.monotonic() - t0) * 1000))
    write_json(statefile, st)


def cmd_kill_at_disarm(args):
    """Stop the watcher of kill-at, if it runs, and print its state: done;
    if it had not fired or ended yet, not fired, reason "disarmed by
    VERB"."""
    statefile = KILLAT_DIR + "/state.json"
    try:
        with open(statefile) as f:
            st = json.load(f)
    except (OSError, ValueError):
        print("null")
        return
    w = st.get("watcher")
    if w and not st.get("done"):
        try:
            os.kill(w, signal.SIGTERM)
        except OSError:
            pass
        for _ in range(100):
            if proc_start(w) is None:
                break
            time.sleep(0.01)
        with open(statefile) as f:
            st = json.load(f)  # it may have fired or ended meanwhile
        if not st.get("done"):
            st.update(done=True, reason="disarmed by %s" % args.verb,
                      ms=int((time.time() - st.get("armedAt", time.time())) * 1000))
            write_json(statefile, st)
    print(json.dumps(st, sort_keys=True))


###
### The host: the settle observation (contract 6.1)
###

PFN_SIG = b"NVDIMM_PFN_INFO\0"
EROFS_MAGIC = b"\xe2\xe1\xf5\xe0"
PFN_OFF = 4096       # the pfn info block in the namespace
HEAD = 4 << 20       # headHash: the first 4 MiB of the data file


def pdev(data, lsa):
    """The content of a persistent device in its backing files (A24
    pdevs): no guest involved."""
    out = {"lsa": None, "lsaHash": None, "headHash": None, "pfn": False, "dataOff": None, "erofs": False,
           "token": None, "raw": None}
    with open(lsa, "rb") as f:
        lb = f.read()
    out["lsa"] = "zero" if lb.count(0) == len(lb) else "labels"
    out["lsaHash"] = hashlib.sha256(lb).hexdigest()
    with open(data, "rb") as f:
        head = f.read(HEAD)
        out["headHash"] = hashlib.sha256(head).hexdigest()
        if head[PFN_OFF:PFN_OFF + len(PFN_SIG)] != PFN_SIG:
            return out
        out["pfn"] = True
        off = struct.unpack_from("<Q", head, PFN_OFF + 56)[0]
        out["dataOff"] = off
        f.seek(off)
        img = f.read(8192)
    out["raw"] = img[:64].split(b"\0", 1)[0].decode(errors="replace")
    if img[1024:1028] == EROFS_MAGIC:
        out["erofs"] = True
        out["token"] = img[4096:4128].decode(errors="replace").strip("\0 \n")
    return out

def load_first_json(path):
    with open(path) as f:
        text = f.read()
    for line in text.splitlines():
        line = line.strip()
        if line.startswith("{"):
            return json.loads(line)
    raise ValueError("no JSON line in %s: %r" % (path, text[-300:]))


def attachments(path, vm_of):
    with open(path) as f:
        text = f.read().strip()
    if not text:
        raise ValueError("no answer")
    j = json.loads(text) or []
    out = []
    for a in j:
        host = a.get("host")
        out.append({"device": a.get("device"), "host": vm_of.get(host, host),
                    "state": a.get("state"), "singleHost": bool(a.get("singleHost")),
                    "adopted": bool(a.get("adopted")), "owner": a.get("owner") or ""})
    return sorted(out, key=lambda a: (a["device"] or "", a["host"] or ""))


def mark_missing(obs, errors):
    """A11: per VM "missing", the containers that the status of a pod
    (slot pod or companion) on the VM says run and that the VM's
    "containers" lacks, or has without its cpuset."""
    pods = [("pod", p) for p in obs.get("pods") or []] + [("companion", c) for c in obs.get("companions") or []]
    for vm, v in (obs.get("vms") or {}).items():
        if v is None:
            continue
        missing = []
        if v.get("containers") is None:
            v["missing"] = None
            continue
        seen = {c.get("id"): c for c in v["containers"]}
        for kind, p in pods:
            if p.get("node") != vm:
                continue
            for c in p.get("containers") or []:
                if c.get("state") != "running" or not c.get("id"):
                    continue
                got = seen.get(c["id"])
                if got is None:
                    why = "the pod status says it runs, crictl does not list it"
                elif not got.get("memsEffective"):
                    why = "its cpuset cannot be read"
                else:
                    continue
                missing.append({"pod": p.get("name"), "kind": kind, "container": c.get("name"), "id": c["id"],
                                "reason": why})
        v["missing"] = missing


def cmd_assemble(args):
    errors = []
    vm_of = {node: vm for vm, node in id_map(args.nodes).items()}
    obs = {"step": args.step, "poll": args.poll, "pods": None, "companions": None, "others": None,
           "attachments": {}, "drivers": None, "controllers": None, "vms": {}, "inspect": None, "slices": None,
           "poolSlices": None}
    for spec in args.vm or []:
        vm, _, path = spec.partition("=")
        try:
            j = load_first_json(path)
        except Exception as e:
            errors.append("%s: %s" % (vm, e))
            obs["vms"][vm] = None
            continue
        obs["vms"][vm] = j.get("vm")
        if "cluster" in j:
            c = j["cluster"] or {}
            for key in ("pods", "companions", "others", "drivers", "controllers", "inspect", "slices", "poolSlices"):
                obs[key] = c.get(key)
            errors.extend("cluster: %s" % e for e in c.get("errors") or [])
    for spec in args.attachments or []:
        pool, _, path = spec.partition("=")
        try:
            obs["attachments"][pool] = attachments(path, vm_of)
        except Exception as e:
            errors.append("attachments %s: %s" % (pool, e))
            obs["attachments"][pool] = None
    mark_missing(obs, errors)
    if args.pdev:
        obs["pdevs"] = {}
        for spec in args.pdev:
            name, _, files = spec.partition("=")
            data, _, lsa = files.partition(":")
            try:
                obs["pdevs"][name] = pdev(data, lsa)
            except Exception as e:
                errors.append("pdevs %s: %s: %s" % (name, type(e).__name__, e))
                obs["pdevs"][name] = None
    obs["errors"] = errors
    if args.inspected_out:
        before = []
        if args.inspected_in and os.path.exists(args.inspected_in):
            with open(args.inspected_in) as f:
                before = [l.strip() for l in f if l.strip()]
        if obs["pods"] is None:
            keep = before
        else:
            uids = set(p["uid"] for p in obs["pods"])
            keep = [e for e in before if e.split(":")[0] in uids]
            # A pod with a lease on its way to Ready is inspected again:
            # an fs lease is in its container only once Ready, and a
            # lease whose companion's Prepare was cut stays Lending
            # until kubelet's retry (A29 d, A30, A33).
            keep += ["%s:%s:%s" % (i["uid"], i["restarts"], slot) for slot, i in (obs["inspect"] or {}).items()
                     if not leases_unready(i.get("leases"))]
        with open(args.inspected_out, "w") as f:
            f.write("".join(e + "\n" for e in sorted(set(keep))))
    if args.summary:
        with open(args.summary, "w") as f:
            for vm, d in sorted((obs["drivers"] or {}).items()):
                f.write("driver %s %s %s %s\n" % (vm, d.get("pod"), d.get("restarts"), "ready" if d.get("ready") else "unready"))
    print(json.dumps(obs, sort_keys=True, separators=(",", ":")))


def main():
    ap = argparse.ArgumentParser(description="observations of test90-mbt-stress")
    sub = ap.add_subparsers(dest="cmd", required=True)
    v = sub.add_parser("vm")
    v.add_argument("--ns", default="cxl-e2e")
    v.add_argument("--stacks-after", type=float, default=0)
    v.add_argument("--cluster", action="store_true")
    v.add_argument("--nodes", default="")
    v.add_argument("--units", default="")
    v.add_argument("--pools", default="")
    v.add_argument("--inspected", default="")
    a = sub.add_parser("assemble")
    a.add_argument("--step", type=int, required=True)
    a.add_argument("--poll", type=int, required=True)
    a.add_argument("--nodes", default="")
    a.add_argument("--vm", action="append")
    a.add_argument("--attachments", action="append")
    a.add_argument("--inspected-in")
    a.add_argument("--inspected-out")
    a.add_argument("--summary")
    a.add_argument("--pdev", action="append")
    k = sub.add_parser("kill-at")
    k.add_argument("--marker", required=True, choices=("region", "ns", "mount", "bind"))
    k.add_argument("--timeout", type=float, default=60)
    k.add_argument("--interval", type=float, default=2)
    kd = sub.add_parser("kill-at-disarm")
    kd.add_argument("--verb", default="the executor")
    args = ap.parse_args()
    if args.cmd == "vm":
        cmd_vm(args)
    elif args.cmd == "kill-at":
        cmd_kill_at(args)
    elif args.cmd == "kill-at-disarm":
        cmd_kill_at_disarm(args)
    else:
        cmd_assemble(args)


if __name__ == "__main__":
    main()
