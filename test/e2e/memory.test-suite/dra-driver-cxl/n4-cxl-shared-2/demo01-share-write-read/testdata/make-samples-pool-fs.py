#!/usr/bin/env python3
"""Write the generated samples of the pool-fs views: DIR/pool-fs-*/.

Usage: make-samples-pool-fs.py DIR (bin/common.py testdata() runs it)
       make-samples-pool-fs.py --super   (pool-fs-super.json again, with mkfs)

Each scenario is the state of one step R1-R11 of the pool-fs script (one
writer on vm1, reader-a and reader-b on vm2, reader-c on vm1, the
persistent shared pool device fs0 of 512Mi), in the shapes of
make-samples.py (whose helpers this uses): state.env, vm2.json and
vm1.json (probe.py output, with the Events of the demo namespace),
pool.json (the pool server's device and the superblock of the filesystem
in its backing file as fsinfo.fs_super reads it). The companion claims
have the claim status of the pool controller: Attached or AttachFailed
conditions and the device data (singleHost of a writer). trace-pool-fs
holds the streams of dra-trace-start of R1-R11 with the log lines of the
current driver, controller and pool server, a reader's Conflict repeated
by the scheduler's retries.

pool-fs-super.json holds the superblocks of real empty filesystems of the
size of fs0's block device (510 MiB), made by mkfs.ext4 -b 4096, mkfs.xfs
and mkfs.btrfs with the label demo (--super makes them again).
"""
import importlib.util
import json
import os
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True   # no __pycache__ of make-samples.py in testdata/
TESTDATA = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(TESTDATA, "..", "bin"))
import fsinfo   # noqa: E402

_spec = importlib.util.spec_from_file_location("make_samples", os.path.join(TESTDATA, "make-samples.py"))
ms = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ms)

VM1, VM2, NS, SERIAL, MI = ms.VM1, ms.VM2, ms.NS, ms.SHARED, ms.MI
DEV, DEV_SIZE = "fs0", 512 * MI
NS_SIZE = DEV_SIZE - 2 * MI        # the fsdax namespace after its pfn info block
SUPER_FILE = os.path.join(TESTDATA, "pool-fs-super.json")
# The superblock ranges of each type: [offset, length].
SUPER_RANGES = {"ext4": [(1024, 1024)], "xfs": [(0, 512)], "btrfs": [(65536, 4096)]}
WHERE = {"writer": VM1, "reader-a": VM2, "reader-b": VM2, "reader-c": VM1}
READERS = ["reader-a", "reader-b", "reader-c"]
# Lease ids and companion claim uids: the writer's block lease, its fs lease, the readers' fs leases.
LEASES = {"block": "3f2a91c0", "writer": "7b3e5a10", "reader-a": "8c1d02e4", "reader-b": "51aa7b09", "reader-c": "e0c4d3f1"}
CUIDS = {k: "c1a1%04x-0000-4000-8000-000000000000" % (0x10 + i) for i, k in enumerate(LEASES)}
NODE_MOUNT = "/var/lib/kubelet/plugins/cxl-pool.nri.io/mounts/c1ae0001-%s"
LEASE_FS = "/var/lib/kubelet/plugins/cxl.nri.io/lease-fs/%s/data"
ATTACHED, FAILED = "cxl-pool.nri.io/Attached", "cxl-pool.nri.io/AttachFailed"
CONFLICT = 'attach fs0 to %s: Conflict: device "fs0" has a single-host attachment to "%s"' % (VM2, VM1)


def make_super():
    """Write pool-fs-super.json: the superblock ranges of empty filesystems made by mkfs."""
    out = {"_doc": "superblocks of empty filesystems of %d bytes (label demo): {type: [[offset, hex]]}" % NS_SIZE}
    cmds = {"ext4": ["mkfs.ext4", "-q", "-F", "-b", "4096", "-L", "demo"], "xfs": ["mkfs.xfs", "-q", "-f", "-L", "demo"],
            "btrfs": ["mkfs.btrfs", "-q", "-f", "-L", "demo"]}
    env = dict(os.environ, PATH=os.environ.get("PATH", "") + ":/usr/sbin:/sbin")
    with tempfile.TemporaryDirectory() as tmp:
        for t, cmd in cmds.items():
            img = os.path.join(tmp, t + ".img")
            with open(img, "wb") as f:
                f.truncate(NS_SIZE)
            subprocess.run(cmd + [img], check=True, env=env)
            with open(img, "rb") as f:
                data = f.read(fsinfo.READ_LEN)
            out[t] = [[off, data[off:off + n].hex()] for off, n in SUPER_RANGES[t]]
    with open(SUPER_FILE, "w") as f:
        json.dump(out, f, indent=1, sort_keys=True)
        f.write("\n")


def super_data(fstype, recovery=False):
    """Return the first fsinfo.READ_LEN bytes of the block device with an empty FSTYPE (pool-fs-super.json), the ext4 journal in use if RECOVERY."""
    data = bytearray(fsinfo.READ_LEN)
    if fstype:
        with open(SUPER_FILE) as f:
            for off, hexdata in json.load(f)[fstype]:
                raw = bytes.fromhex(hexdata)
                data[off:off + len(raw)] = raw
    if recovery:
        data[1024 + 0x60] |= fsinfo.EXT4_INCOMPAT_RECOVER
    return bytes(data)


def fs0_slice():
    s = ms.pool_slice(True, persistent=True)
    d = s["spec"]["devices"][0]
    d["name"] = DEV
    d["attributes"]["size"] = {"int": DEV_SIZE}
    d["attributes"]["coherent"] = {"bool": True}
    return s


def device_status(node, single_host=False, failed=None, exclusive=False):
    """Return the status.devices entry of the pool controller for fs0 on NODE: Attached, or AttachFailed reason Conflict (FAILED).

    EXCLUSIVE: the claim has all shares; SINGLE_HOST: its node has fs0
    alone (a writer: a read-write fs share or the whole device).
    """
    st = {"driver": "cxl-pool.nri.io", "pool": "fake-cxl-pool", "device": DEV}
    if failed:
        st["conditions"] = [{"type": FAILED, "status": "True", "reason": "Conflict", "message": failed,
                             "lastTransitionTime": "2026-11-03T12:02:00Z"}]
        return st
    st["conditions"] = [{"type": ATTACHED, "status": "True", "reason": "Attached",
                         "message": "fs0 attached to %s (slot cxlsw_ds0_usrp0hb0)" % node, "lastTransitionTime": "2026-11-03T12:01:00Z"}]
    data = {"serial": SERIAL[2:], "shared": True, "persistent": True, "size": DEV_SIZE, "host": node,
            "attachment": "fs0@" + node, "qemuDeviceID": "fcp_fs0.hp1", "coherent": True}
    if single_host:
        data["singleHost"] = True
    if exclusive:
        data["exclusive"] = True
    st["data"] = data
    return st


def claim(key, requester, mode, shared, ro, allocated, attached=True, failed=None, deleting=False):
    """Return the companion claim of lease KEY (LEASES) of REQUESTER on its node."""
    node = WHERE[requester]
    cl = ms.companion(requester, node, SERIAL, "pool", shared, ro, allocated, deleting, device=DEV, mode=mode)
    lid = LEASES[key]
    cl["metadata"].update(name="cxl-lease-" + lid, uid=CUIDS[key])
    cl["metadata"]["labels"]["cxl.nri.io/lease"] = lid
    if allocated:
        cl["status"]["reservedFor"] = [{"resource": "pods", "name": "cxl-lease-" + lid}]
        if failed:
            cl["status"]["devices"] = [device_status(node, failed=failed)]
        elif attached:
            # As the live claims: a writer of a share, and the whole-device block claim, have singleHost.
            cl["status"]["devices"] = [device_status(node, single_host=not ro and (mode == "fs" or not shared), exclusive=not shared)]
    return cl


def companion_pod(key, requester, phase="Running"):
    return ms.pod("cxl-lease-" + LEASES[key], WHERE[requester], phase, LEASES[key], requester, scheduled=phase == "Running")


def event(key, reason, message, count=1, kind="Warning"):
    return {"apiVersion": "v1", "kind": "Event", "type": kind, "reason": reason, "message": message, "count": count,
            "metadata": {"name": "cxl-lease-%s.%x" % (LEASES[key], count), "namespace": NS},
            "involvedObject": {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceClaim", "namespace": NS,
                               "name": "cxl-lease-" + LEASES[key], "uid": CUIDS[key]},
            "source": {"component": "fake-cxl-pool-controller"},
            "firstTimestamp": "2026-11-03T12:02:00Z", "lastTimestamp": "2026-11-03T12:02:%02dZ" % (10 * count)}


def pmem(fstype, ro, openers=(), mounts=()):
    region = ms.pmem_region("region0", "mem0", DEV_SIZE, fstype or "none", ro)
    return {"memdevs": [ms.memdev("mem0", SERIAL, DEV_SIZE)], "regions": [region], "mappers": [],
            "openers": list(openers), "mounts": list(mounts)}


def mount(kind, fstype, sb_ro, mount_ro, who=None, loop=False):
    """Return a mount of pmem0 as probe.py reports it: the node mount, or the lease bind of the fs lease of WHO."""
    m = {"source": "/dev/loop0" if loop else "/dev/pmem0", "block": "pmem0", "loop": loop, "dax": not loop, "kind": kind,
         "fstype": fstype, "ro": sb_ro, "mountRO": mount_ro}
    if kind == "node":
        m["target"] = NODE_MOUNT % ("cached" if loop else "direct")
    else:
        cuid = ms.lease_claim(who, WHERE[who])["metadata"]["uid"]
        m.update(target=LEASE_FS % cuid, claimUID=cuid, name="data")
    return m


def scenarios(fstype):
    """Return {name: (vm2, vm1, pool)} of the pool-fs demo of FSTYPE, steps R1-R11."""
    out = {}
    slices = [ms.local_slice(VM2, memdev=False), ms.local_slice(VM1, memdev=False), fs0_slice()]
    empty = {"memdevs": [], "regions": [], "mappers": []}
    loop = fstype == "btrfs"   # no DAX: access cached, a loop device over pmem0

    def vm(name, pods, claims, node, events=()):
        d = {"node": dict(node, hostname=name), "errors": []}
        if pods is not None:
            d["k8s"] = {"pods": ms.lst(pods), "claims": ms.lst(claims), "slices": ms.lst(slices), "drivers": ms.lst(ms.drivers()),
                        "events": ms.lst(list(events))}
        return d

    def pool(atts, data, state="allocated", owner=None):
        dev = {"name": DEV, "serial": SERIAL, "size": DEV_SIZE, "shared": True, "persistent": True, "state": state,
               "scope": "pool", "path": "/tmp/fake-cxl-pool/fs0.raw", "lsaPath": "/tmp/fake-cxl-pool/fs0.lsa", "static": True,
               "attachments": [{"device": DEV, "host": h, "state": "attached", "singleHost": ex} for h, ex in atts],
               "allocation": {"owner": owner} if owner else None}
        return {"device": dev, "fsOffset": 2 * MI, "super": fsinfo.fs_super(data)}

    lclaims = [ms.lease_claim(n, WHERE[n]) for n in ["writer"] + READERS]
    writer = ms.pod("writer", VM1, claim=lclaims[0]["metadata"]["name"])
    readers = [ms.pod(r, WHERE[r], claim=ms.lease_claim(r, WHERE[r])["metadata"]["name"]) for r in READERS]
    block = claim("block", "writer", "block", False, False, True)
    block_pod = companion_pod("block", "writer")
    fsw = claim("writer", "writer", "fs", True, False, True)
    fsw_pod = companion_pod("writer", "writer")
    opener = {"pid": 4242, "comm": "python3", "podUID": ms.uid("writer"), "block": "pmem0", "perms": "rw"}
    owner = "k8s:resourceclaim/" + CUIDS["block"]
    zeros, made = super_data(None), super_data(fstype)
    in_use = super_data(fstype, recovery=fstype == "ext4")
    node_rw = mount("node", fstype, False, False, loop=loop)
    node_ro = mount("node", fstype, True, False, loop=loop)
    w_bind = mount("lease", fstype, False, False, "writer", loop)

    def bind(r, sb_ro):
        return mount("lease", fstype, sb_ro, True, r, loop)

    t = "pool-fs-" + ("" if fstype == "ext4" else fstype + "-")
    if fstype == "ext4":
        out[t + "R1"] = (vm(VM2, [writer], lclaims[:1], empty), vm(VM1, None, None, empty), pool([], zeros, "free"))
        out[t + "R2"] = (vm(VM2, [writer, block_pod], [block] + lclaims[:1], empty),
                         vm(VM1, None, None, pmem(None, False, [opener])), pool([(VM1, True)], zeros, owner=owner))
        out[t + "R3"] = (vm(VM2, [writer, block_pod], [block] + lclaims[:1], empty),
                         vm(VM1, None, None, pmem(fstype, False, [opener])), pool([(VM1, True)], made, owner=owner))
        out[t + "R5"] = (vm(VM2, [writer, fsw_pod], [fsw] + lclaims[:1], empty),
                         vm(VM1, None, None, pmem(fstype, False, mounts=[node_rw, w_bind])), pool([(VM1, True)], in_use))
    # R7: reader-c next to the writer; reader-a allocated again by the scheduler and failed, reader-b waiting.
    r_c = claim("reader-c", "reader-c", "fs", True, True, True)
    r_a = claim("reader-a", "reader-a", "fs", True, True, True, failed=CONFLICT)
    r_b = claim("reader-b", "reader-b", "fs", True, True, False)
    events = [event("reader-a", "AttachFailed", CONFLICT, 3), event("reader-b", "AttachFailed", CONFLICT, 2),
              event("reader-c", "Attached", "fs0 attached to %s (slot cxlsw_ds0_usrp0hb0)" % VM1, 1, "Normal")]
    pods7 = [writer, fsw_pod] + readers + [companion_pod("reader-c", "reader-c"), companion_pod("reader-a", "reader-a", "Pending"),
                                           companion_pod("reader-b", "reader-b", "Pending")]
    out[t + "R7"] = (vm(VM2, pods7, [fsw, r_a, r_b, r_c] + lclaims, empty, events),
                     vm(VM1, None, None, pmem(fstype, False, mounts=[node_rw, w_bind, bind("reader-c", False)])), pool([(VM1, True)], in_use))
    # R9: the writer is gone; vm1 read-only, vm2 attached, every reader Ready.
    r_all = [claim(r, r, "fs", True, True, True) for r in READERS]
    pods9 = [writer] + readers + [companion_pod(r, r) for r in READERS]
    mounts2 = [node_ro, bind("reader-a", True), bind("reader-b", True)]
    out[t + "R9"] = (vm(VM2, pods9, r_all + lclaims, pmem(fstype, True, mounts=mounts2), events),
                     vm(VM1, None, None, pmem(fstype, True, mounts=[node_ro, bind("reader-c", True)])),
                     pool([(VM1, False), (VM2, False)], made))
    if fstype == "ext4":
        out[t + "R11"] = (vm(VM2, [], [], empty), vm(VM1, None, None, empty), pool([], made, "free"))
    return out


def trace(sdir, t0=1792700000.0):
    """Write the streams of dra-trace-start of the pool-fs script R1-R11 (ext4) to SDIR/trace."""
    tdir = os.path.join(sdir, "trace")
    os.makedirs(tdir, exist_ok=True)
    streams = {}
    vmtag = {VM1: "vm1", VM2: "vm"}
    luid = {r: ms.lease_claim(r, WHERE[r])["metadata"]["uid"] for r in WHERE}
    direct = NODE_MOUNT % "direct"

    def at(dt, tag, text):
        streams.setdefault(tag, []).append("%.6f %s %s" % (t0 + dt, tag, text))

    def klog(dt, node, src, msg, level="I"):
        at(dt, vmtag[node] + ".driver", "%s1103 12:%02d:%02d.000000  4242 %s] %s" % (level, int(dt) // 60, int(dt) % 60, src, msg))

    def ev(dt, kind, name, reason, msg):
        at(dt, "vm.events", "%s   %s   %s   %s   %s" % (NS, kind, name, reason, msg))

    def cl(dt, event_type, key, alloc="-", node="-", deleting=False):
        at(dt, "vm.claims", "%s claim %s/cxl-lease-%s uid=%s allocated=%s node=%s reservedFor=- status=-%s" % (
            event_type, NS, LEASES[key], CUIDS[key], alloc, node, " deleting" if deleting else ""))

    def udev(dt, node, action, devpath, subsys):
        tag = vmtag[node] + (".block.udev" if subsys == "block" else ".udev")
        at(dt, tag, "KERNEL[%.6f] %-8s %s (%s)" % (7000 + dt, action, devpath, subsys))

    def ctrl(dt, text):
        at(dt, "vm.controller", "fake-cxl-pool-controller: 2026/11/03 12:%02d:%02d.000000 %s" % (int(dt) // 60, int(dt) % 60, text))

    def server(dt, text):
        at(dt, "server", "fake-cxl-pool: 2026/11/03 12:%02d:%02d.000000 %s" % (int(dt) // 60, int(dt) % 60, text))

    def want(key, requester, node=None):
        return "claim %s/cxl-lease-%s device fs0 node %s" % (NS, LEASES[key], node or WHERE[requester])

    memdev_path = "/devices/pci0000:0c/0000:0c:00.0/0000:0d:00.0/0000:0e:00.0/0000:0f:00.0/mem0"
    region_path = "/devices/platform/ACPI0017:00/root0/decoder0.0/region0"
    pmem_path = "/devices/platform/ACPI0017:00/root0/nvdimm-bridge0/ndbus0/region0/pfn0.1/block/pmem0"
    alloc = "cxl-pool.nri.io/fake-cxl-pool/fs0"

    def attach(dt, node, key, excl):
        ctrl(dt, "attached fs0 to %s (node %s, slot cxlsw_ds0_usrp0hb0, qemu device fcp_fs0.hp1, owner k8s:resourceclaim/%s, exclusive %s) for %s/cxl-lease-%s"
             % (node, node, CUIDS[key], "true" if excl else "false", NS, LEASES[key]))
        server(dt - 0.1, "attachment fs0@%s: attached" % node)
        udev(dt + 0.2, node, "add", memdev_path, "cxl")
        udev(dt + 2.6, node, "add", region_path, "cxl")
        udev(dt + 2.9, node, "add", pmem_path, "block")

    def detach(dt, node, key):
        ctrl(dt, "detached fs0 from %s (node %s, owner k8s:resourceclaim/%s)" % (node, node, CUIDS[key]))
        server(dt - 0.1, "attachment fs0@%s: detached (qemu device fcp_fs0.hp1)" % node)
        udev(dt - 0.4, node, "remove", pmem_path, "block")
        udev(dt - 0.3, node, "remove", region_path, "cxl")
        udev(dt + 0.1, node, "remove", memdev_path, "cxl")

    def request(dt, key, who, mode, shared, ro, name):
        klog(dt, WHERE[who], "broker.go:729", 'lease broker: lease %s: %s/%s/main requests size "" serial "%s" (source "", mode %s, share %s, shared device true, read-only %s, access "", name "%s")'
             % (LEASES[key], NS, who, SERIAL, mode, "true" if shared else "false", "true" if ro else "false", name))

    def ready(dt, key, who, mode, ro=True):
        if mode == "block":
            what = 'read-only false, NUMA node -1, dax "", block "/dev/pmem0", access "", fs "", path ""'
        else:
            what = 'read-only %s, NUMA node -1, dax "", block "", access "direct", fs "ext4", path "/run/cxl/fs/data"' % ("true" if ro else "false")
        klog(dt, WHERE[who], "broker.go:1646", "lease broker: lease %s: Ready (source pool, serial %s, mode %s, %s, %d bytes)"
             % (LEASES[key], SERIAL, mode, what, NS_SIZE))

    def prepared(dt, key, who, mode, ro=True):
        if mode == "block":
            ro_s, access, fstype, mnt = "false", "", "", ""
        else:
            ro_s, access, fstype, mnt = "true" if ro else "false", "direct", "ext4", direct
        klog(dt, WHERE[who], "pool.go:689", 'cxl-pool.nri.io: claim %s/cxl-lease-%s: prepared fs0 (memdev %s, share true, mode %s, read-only %s) as mem0 region0 namespace namespace0.0 block /dev/pmem0 access "%s" fs "%s" mount "%s" loop ""'
             % (NS, LEASES[key], SERIAL, mode, ro_s, access, fstype, mnt))

    def unprepared(dt, key, who):
        klog(dt, WHERE[who], "pool.go:1521", "cxl-pool.nri.io: unprepared claim %s/cxl-lease-%s (%s)" % (NS, LEASES[key], CUIDS[key]))

    def device_log(dt, node, src, text):
        klog(dt, node, src, "engine: pool memdev %s: %s" % (SERIAL, text))

    def bound(dt, key, who):
        klog(dt, WHERE[who], "persistent.go:1130", "engine: lease %s: bound %s at %s" % (LEASES[key], direct, LEASE_FS % luid[who]))

    def unbound(dt, key, who):
        klog(dt, WHERE[who], "persistent.go:1246", "engine: lease %s: unbound %s" % (LEASES[key], LEASE_FS % luid[who]))

    def released(dt, key, who, why="released by main, was Ready"):
        klog(dt, WHERE[who], "broker.go:1189", "lease broker: lease %s: releasing (%s)" % (LEASES[key], why))
        klog(dt + 2.5, WHERE[who], "broker.go:1258", "lease broker: lease %s: released" % LEASES[key])

    def conflict(dt, key, who):
        """One attempt of the scheduler for the reader WHO of vm2 while vm1 writes: allocated, attach refused, deallocated."""
        name = "cxl-lease-" + LEASES[key]
        cl(dt, "MODIFIED", key, alloc, VM2)
        ev(dt + 0.1, "Pod", name, "BindingConditionsPending", "waiting for binding conditions")
        ctrl(dt + 0.3, "ERROR: Conflict for %s: %s" % (want(key, who), CONFLICT))
        ctrl(dt + 0.4, "status of %s: %s=True reason Conflict: %s" % (want(key, who), FAILED, CONFLICT))
        ev(dt + 0.4, "ResourceClaim", name, "AttachFailed", CONFLICT)
        cl(dt + 0.6, "MODIFIED", key)
        ev(dt + 0.7, "Pod", name, "FailedScheduling", "running PreBind plugin \"DynamicResources\": claim %s/%s: binding failed: %s" % (NS, name, FAILED))

    ev(1, "Pod", "writer", "Scheduled", "Successfully assigned %s/writer to %s" % (NS, VM1))
    # R2: the block lease, exclusive; R3 writes the ext4 through its fd.
    t = 20
    request(t, "block", "writer", "block", False, False, "")
    cl(t + 0.1, "ADDED", "block")
    cl(t + 0.2, "MODIFIED", "block", alloc, VM1)
    ev(t + 0.4, "Pod", "cxl-lease-" + LEASES["block"], "BindingConditionsPending", "waiting for binding conditions")
    attach(t + 1, VM1, "block", True)
    prepared(t + 4, "block", "writer", "block")
    ready(t + 4.2, "block", "writer", "block")
    # R4: the block lease goes, the fs lease of the writer comes: read-write, the attachment exclusive.
    t = 60
    released(t, "block", "writer")
    cl(t + 0.2, "MODIFIED", "block", alloc, VM1, deleting=True)
    unprepared(t + 1, "block", "writer")
    detach(t + 2, VM1, "block")
    cl(t + 2.4, "DELETED", "block", alloc, VM1, deleting=True)
    t = 64
    request(t, "writer", "writer", "fs", True, False, "data")
    cl(t + 0.1, "ADDED", "writer")
    cl(t + 0.2, "MODIFIED", "writer", alloc, VM1)
    # The fsType of the log is the claim's, none here (the driver detects ext4).
    ctrl(t + 0.5, "attach fs0 to %s for %s: single host for a read-write filesystem claim" % (VM1, want("writer", "writer")))
    attach(t + 1, VM1, "writer", True)
    device_log(t + 4.0, VM1, "persistent.go:679", "/dev/pmem0 read-write")
    device_log(t + 4.1, VM1, "persistent.go:573", "mounted /dev/pmem0 at %s (ext4, rw, dax=always)" % direct)
    prepared(t + 4.2, "writer", "writer", "fs", ro=False)
    bound(t + 4.3, "writer", "writer")
    ready(t + 4.4, "writer", "writer", "fs", ro=False)
    # R7: reader-c joins the writer's superblock, vm2's readers get Conflict, again and again.
    t = 120
    for i, r in enumerate(READERS):
        ev(t - 20 + i, "Pod", r, "Scheduled", "Successfully assigned %s/%s to %s" % (NS, r, WHERE[r]))
        request(t + i * 0.5, r, r, "fs", True, True, "data")
        cl(t + i * 0.5 + 0.1, "ADDED", r)
    cl(t + 1.2, "MODIFIED", "reader-c", alloc, VM1)
    ev(t + 1.3, "Pod", "cxl-lease-" + LEASES["reader-c"], "Scheduled", "Successfully assigned %s/cxl-lease-%s to %s" % (NS, LEASES["reader-c"], VM1))
    prepared(t + 2.0, "reader-c", "reader-c", "fs")
    bound(t + 2.1, "reader-c", "reader-c")
    ready(t + 2.2, "reader-c", "reader-c", "fs")
    for k in range(3):
        conflict(t + 1.5 + 10 * k, "reader-a", "reader-a")
        conflict(t + 2.0 + 10 * k, "reader-b", "reader-b")
    # R9: the writer leaves: vm1 remounts read-only, the attachment is not exclusive any more, vm2 attaches.
    t = 180
    released(t, "writer", "writer")
    cl(t + 0.2, "MODIFIED", "writer", alloc, VM1, deleting=True)
    unbound(t + 0.8, "writer", "writer")
    device_log(t + 0.9, VM1, "persistent.go:634", "remounted %s read-only" % direct)
    device_log(t + 1.0, VM1, "persistent.go:679", "/dev/pmem0 read-only")
    unprepared(t + 1.1, "writer", "writer")
    cl(t + 1.5, "DELETED", "writer", alloc, VM1, deleting=True)
    server(t + 1.8, "attachment fs0@%s: single host false" % VM1)
    ev(t + 1.9, "Pod", "cxl-lease-" + LEASES["reader-a"], "FailedScheduling",
       "0/3 nodes are available: 1 resourceclaim not available on the node, 2 node(s) didn't satisfy plugin(s) [NodeAffinity]. "
       "deallocation of ResourceClaim completed, preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.")
    for i, r in enumerate(["reader-a", "reader-b"]):
        cl(t + 6 + i * 0.2, "MODIFIED", r, alloc, VM2)
        ev(t + 6.1 + i * 0.2, "Pod", "cxl-lease-" + LEASES[r], "BindingConditionsPending", "waiting for binding conditions")
    attach(t + 7, VM2, "reader-a", False)
    for i, r in enumerate(["reader-a", "reader-b"]):
        dt = t + 10.5 + i
        ev(dt, "Pod", "cxl-lease-" + LEASES[r], "Scheduled", "Successfully assigned %s/cxl-lease-%s to %s" % (NS, LEASES[r], VM2))
        if i == 0:
            device_log(dt + 0.05, VM2, "persistent.go:679", "/dev/pmem0 read-only")
            device_log(dt + 0.1, VM2, "persistent.go:573", "mounted /dev/pmem0 at %s (ext4, ro, dax=always)" % direct)
        prepared(dt + 0.2, r, r, "fs")
        bound(dt + 0.3, r, r)
        ready(dt + 0.4, r, r, "fs")
    # R11: everything goes away.
    t = 240
    for i, r in enumerate(READERS):
        released(t + i * 0.3, r, r, "requester pod deleted, was Ready")
        unbound(t + 1 + i * 0.5, r, r)
        if r in ("reader-b", "reader-c"):
            device_log(t + 1.1 + i * 0.5, WHERE[r], "persistent.go:820", "unmounted %s" % direct)
        unprepared(t + 1.2 + i * 0.5, r, r)
        cl(t + 1.3 + i * 0.5, "DELETED", r, alloc, WHERE[r], deleting=True)
    detach(t + 4, VM2, "reader-b")
    detach(t + 6, VM1, "reader-c")
    for tag, lines in streams.items():
        lines.sort()
        with open(os.path.join(tdir, tag + ".log"), "w") as f:
            f.write("\n".join(lines) + "\n")
    with open(os.path.join(sdir, "state.env"), "w") as f:
        f.write(state("ext4"))


def state(fstype):
    """Return the state.env of demo setup pool-fs FSTYPE."""
    return ms.state("pool-fs", SERIAL).replace("POOL_DEVICE=shared0", "POOL_DEVICE=fs0") + "DEMO_FSTYPE=%s\n" % fstype


def main():
    if sys.argv[1:] == ["--super"]:
        make_super()
        print("superblocks written to %s" % SUPER_FILE)
        return
    if len(sys.argv) != 2:
        sys.exit("usage: %s DIR" % os.path.basename(sys.argv[0]))
    base = sys.argv[1]
    for fstype in ("ext4", "xfs", "btrfs"):
        for name, (v2, v1, pl) in scenarios(fstype).items():
            sdir = os.path.join(base, name)
            os.makedirs(sdir, exist_ok=True)
            with open(os.path.join(sdir, "state.env"), "w") as f:
                f.write(state(fstype))
            ms.write(sdir, "vm2.json", v2)
            ms.write(sdir, "vm1.json", v1)
            ms.write(sdir, "pool.json", pl)
    trace(os.path.join(base, "trace-pool-fs"))
    print("pool-fs samples written to %s" % base)


if __name__ == "__main__":
    main()
