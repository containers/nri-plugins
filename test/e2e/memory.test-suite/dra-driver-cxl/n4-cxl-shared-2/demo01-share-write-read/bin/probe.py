#!/usr/bin/env python3
"""Probe of the demo views: one call per node per refresh.

Runs as root in a VM: "ssh ... node sudo python3 - ARGS < probe.py".
Prints one JSON object with the sections that ARGS ask for:

  --k8s KINDS    Kubernetes objects through kubectl (comma list):
                 pods, claims, events (namespace --ns), slices, drivers (the
                 driver pods in kube-system), allclaims (all namespaces)
  --ns NS        the namespace of the demo
  --node         CXL in the kernel: memdevs (serial, size, enabled), regions (mode,
                 size, memdevs, dax devices with size and driver, the fsdax
                 namespaces of pmem regions with their block device, its
                 read-only flag and the filesystem type on it), the
                 processes of pods that map a dax device or a file of a
                 filesystem on a pmem block device (/proc/PID/maps) or
                 have an fd of a pmem block device (/proc/PID/fd), with the
                 pod uid of their cgroup, and the mounts of the pmem block
                 devices (mountinfo)
  --bytes SERIAL the first --len bytes (default 256) of the dax device of
                 the region of the memdev SERIAL, mapped read-only
  --root DIR     prefix of /sys, /proc and /dev (tests)

Errors of a section go to "errors"; the probe itself exits 0.
"""
import json
import mmap
import os
import re
import subprocess
import sys
import threading

ROOT = ""
KUBECTL_TIMEOUT = 3.0
DRIVER_LABEL = "app.kubernetes.io/name=nri-dra-driver-cxl"
# The node mounts of mode fs of both drivers, the lease binds of fs leases.
NODE_MOUNT_DIRS = ("/var/lib/kubelet/plugins/cxl-pool.nri.io/mounts/", "/var/lib/kubelet/plugins/cxl.nri.io/mounts/")
LEASE_FS_DIR = "/var/lib/kubelet/plugins/cxl.nri.io/lease-fs/"
# The superblock magics of the filesystem types of mode fs: (offset, bytes).
FS_MAGICS = (("erofs", 1024, b"\xe2\xe1\xf5\xe0"), ("ext4", 1080, b"\x53\xef"), ("xfs", 0, b"XFSB"),
             ("btrfs", 65600, b"_BHRfS_M"))
POD_UID = re.compile(r"pod([0-9a-f]{8}[_-][0-9a-f]{4}[_-][0-9a-f]{4}[_-][0-9a-f]{4}[_-][0-9a-f]{12})")


def path(p):
    return ROOT + p


def read(p, default=None):
    try:
        with open(path(p)) as f:
            return f.read().strip()
    except OSError:
        return default


def int_any(s):
    try:
        return int(str(s).strip(), 0)
    except (TypeError, ValueError):
        return None


def listdir(p):
    try:
        return sorted(os.listdir(path(p)))
    except OSError:
        return []


def link_name(p):
    try:
        return os.path.basename(os.readlink(path(p)))
    except OSError:
        return None


def kubectl(args, out, key, errors):
    env = dict(os.environ)
    if os.path.exists("/root/.kube/config"):
        env["KUBECONFIG"] = "/root/.kube/config"
    env["PATH"] = env.get("PATH", "") + ":/usr/local/bin:/usr/bin:/bin"
    try:
        p = subprocess.run(["kubectl"] + args + ["-o", "json"], stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE, timeout=KUBECTL_TIMEOUT, env=env)
        if p.returncode != 0:
            errors.append("kubectl %s: %s" % (" ".join(args), p.stderr.decode(errors="replace").strip()[:200]))
            return
        out[key] = json.loads(p.stdout.decode(errors="replace"))
    except (OSError, ValueError, subprocess.TimeoutExpired) as e:
        errors.append("kubectl %s: %s" % (" ".join(args), e))


def k8s(kinds, ns, errors):
    """Return the objects of KINDS from one kubectl call: kubectl's start costs more than the objects."""
    known = {"pods", "claims", "allclaims", "slices", "drivers", "events"}
    for kind in kinds:
        if kind not in known:
            errors.append("unknown kind %s" % kind)
    kinds = [k for k in kinds if k in known]
    if not kinds:
        return {}
    resources = sorted({{"pods": "pods", "drivers": "pods", "claims": "resourceclaims", "allclaims": "resourceclaims",
                         "slices": "resourceslices", "events": "events"}[k] for k in kinds})
    scope = ["-n", ns] if set(kinds) <= {"pods", "claims", "events"} else ["-A"]
    got = {}
    kubectl(["get", ",".join(resources)] + scope, got, "all", errors)
    if "all" not in got:
        return {}
    its = got["all"].get("items") or []

    def pick(cond):
        return {"apiVersion": "v1", "kind": "List", "items": [o for o in its if cond(o)]}

    def ns_of(o):
        return (o.get("metadata") or {}).get("namespace")
    driver = DRIVER_LABEL.split("=", 1)
    out = {}
    for kind in kinds:
        if kind == "pods":
            out[kind] = pick(lambda o: o.get("kind") == "Pod" and ns_of(o) == ns)
        elif kind == "drivers":
            out[kind] = pick(lambda o: o.get("kind") == "Pod" and ns_of(o) == "kube-system" and
                             ((o.get("metadata") or {}).get("labels") or {}).get(driver[0]) == driver[1])
        elif kind == "claims":
            out[kind] = pick(lambda o: o.get("kind") == "ResourceClaim" and ns_of(o) == ns)
        elif kind == "allclaims":
            out[kind] = pick(lambda o: o.get("kind") == "ResourceClaim")
        elif kind == "events":
            out[kind] = pick(lambda o: o.get("kind") == "Event" and ns_of(o) == ns)
        else:
            out[kind] = pick(lambda o: o.get("kind") == "ResourceSlice")
    return out


def memdevs():
    """Return the memdevs and a map endpoint id -> memdev name."""
    devs, endpoints = [], {}
    base = "/sys/bus/cxl/devices"
    for name in listdir(base):
        if re.fullmatch(r"mem\d+", name):
            devs.append({
                "name": name,
                "serial": "0x%x" % (int_any(read("%s/%s/serial" % (base, name))) or 0),
                "size": (int_any(read("%s/%s/ram/size" % (base, name), "0")) or 0) +
                        (int_any(read("%s/%s/pmem/size" % (base, name), "0")) or 0),
                "numaNode": int_any(read("%s/%s/numa_node" % (base, name))),
                # A memdev without its driver is disabled: released, before a detach.
                "enabled": os.path.lexists(path("%s/%s/driver" % (base, name))),
            })
        m = re.fullmatch(r"endpoint(\d+)", name)
        if m:
            up = link_name("%s/%s/uport" % (base, name))
            if up:
                endpoints[m.group(1)] = up
    return devs, endpoints


def regions(endpoints):
    base = "/sys/bus/cxl/devices"
    out = []
    for name in listdir(base):
        if not re.fullmatch(r"region\d+", name):
            continue
        r = {"name": name, "mode": read("%s/%s/mode" % (base, name)),
             "size": int_any(read("%s/%s/size" % (base, name), "0")) or 0,
             "memdevs": [], "dax": []}
        i = 0
        while True:
            target = read("%s/%s/target%d" % (base, name, i))
            if target is None:
                break
            m = re.fullmatch(r"decoder(\d+)\.\d+", target)
            if m and m.group(1) in endpoints:
                r["memdevs"].append(endpoints[m.group(1)])
            i += 1
        rnum = name[len("region"):]
        dax_dir = "%s/%s/dax_region%s" % (base, name, rnum)
        for d in listdir(dax_dir):
            if re.fullmatch(r"dax\d+\.\d+", d):
                r["dax"].append({
                    "name": d,
                    "size": int_any(read("%s/%s/size" % (dax_dir, d), "0")) or 0,
                    "driver": link_name("/sys/bus/dax/devices/%s/driver" % d),
                    "targetNode": int_any(read("%s/%s/target_node" % (dax_dir, d))),
                })
        if r["mode"] == "pmem":
            r["namespaces"] = namespaces(name)
        out.append(r)
    return out


def blockfs(block):
    """Return the filesystem type of the superblock magic on /dev/BLOCK (FS_MAGICS), "none" if none, "?" if unreadable."""
    try:
        fd = os.open(path("/dev/" + block), os.O_RDONLY)
        try:
            for fstype, offset, magic in FS_MAGICS:
                if os.pread(fd, len(magic), offset) == magic:
                    return fstype
        finally:
            os.close(fd)
    except OSError:
        return "?"
    return "none"


def namespaces(region):
    """Return the namespaces of the pmem region REGION with their block devices.

    The nd region of a CXL pmem region is the one with the same physical
    address (resource); the block device of an fsdax namespace is on its
    holder (pfnX.Y), of a raw one on the namespace itself.
    """
    out = []
    res = read("/sys/bus/cxl/devices/%s/resource" % region)
    base = "/sys/bus/nd/devices"
    for ndr in listdir(base):
        if not re.fullmatch(r"region\d+", ndr) or res is None or read("%s/%s/resource" % (base, ndr)) != res:
            continue
        rdir = "%s/%s" % (base, ndr)
        for ns in listdir(rdir):
            if not re.fullmatch(r"namespace\d+\.\d+", ns):
                continue
            d = "%s/%s" % (rdir, ns)
            size = int_any(read(d + "/size", "0")) or 0
            if not size:
                continue
            holder = read(d + "/holder") or ""
            blocks = listdir(d + "/block") or (listdir("%s/%s/block" % (rdir, holder)) if holder else [])
            mode = read(d + "/mode")
            n = {"name": ns, "mode": "fsdax" if mode == "memory" else mode, "size": size,
                 "block": blocks[0] if blocks else None, "major": None, "minor": None, "fs": None, "ro": None}
            if blocks:
                dev = (read("/sys/block/%s/dev" % blocks[0]) or "").split(":")
                if len(dev) == 2:
                    n["major"], n["minor"] = int_any(dev[0]), int_any(dev[1])
                sectors = int_any(read("/sys/block/%s/size" % blocks[0]))
                if sectors:
                    n["size"] = sectors * 512
                n["fs"] = blockfs(blocks[0])
                ro = read("/sys/block/%s/ro" % blocks[0])
                n["ro"] = None if ro is None else ro == "1"
            out.append(n)
    return out


def pod_pids():
    """Return [(pid, pod uid)] of the processes of pods, this one excluded."""
    out = []
    me = str(os.getpid())
    for pid in listdir("/proc"):
        if not pid.isdigit() or pid == me:
            continue
        m = POD_UID.search(read("/proc/%s/cgroup" % pid, "") or "")
        if m:
            out.append((pid, m.group(1).replace("_", "-")))
    return out


def mappers(pids, fs_devs=None):
    """Return the processes of PIDS that map a dax device, or a file of a filesystem on a device of FS_DEVS.

    FS_DEVS maps the device numbers of mountinfo ("259:0") to (the pmem
    block device under the filesystem, the device of the mount: the same
    or a loop device over it); such a mapping is {"block", "source",
    "path"} instead of {"dax"}.
    """
    out = []
    for pid, pod_uid in pids:
        try:
            with open(path("/proc/%s/maps" % pid)) as f:
                maps = f.read()
        except OSError:
            continue
        seen, files = {}, {}
        for line in maps.splitlines():
            parts = line.split()
            if len(parts) >= 6 and re.fullmatch(r"/dev/dax\d+\.\d+", parts[5]):
                dev = parts[5][len("/dev/"):]
                perms = parts[1]
                if "w" in perms or dev not in seen:
                    seen[dev] = perms
            elif len(parts) >= 6 and fs_devs and parts[3] != "00:00":
                major, minor = (int(x, 16) for x in parts[3].split(":"))
                dev = fs_devs.get("%d:%d" % (major, minor))
                if dev:
                    files[parts[5]] = dev
        comm = read("/proc/%s/comm" % pid, "?") if seen or files else None
        for dev, perms in seen.items():
            out.append({"pid": int(pid), "comm": comm, "podUID": pod_uid, "dax": dev, "perms": perms})
        for fpath, (block, source) in sorted(files.items()):
            out.append({"pid": int(pid), "comm": comm, "podUID": pod_uid, "block": block, "source": source, "path": fpath})
    return out


def openers(pids, blocks):
    """Return the processes of PIDS that have an fd of one of the block devices BLOCKS."""
    out = []
    want = {"/dev/" + b: b for b in blocks}
    for pid, pod_uid in pids:
        seen = {}
        for fd in listdir("/proc/%s/fd" % pid):
            try:
                target = os.readlink(path("/proc/%s/fd/%s" % (pid, fd)))
            except OSError:
                continue
            if target not in want:
                continue
            flags = re.search(r"^flags:\s+([0-7]+)", read("/proc/%s/fdinfo/%s" % (pid, fd), "") or "", re.M)
            perms = "rw" if flags and int(flags.group(1), 8) & 3 else "ro"
            if seen.get(want[target]) != "rw":
                seen[want[target]] = perms
        for block, perms in seen.items():
            out.append({"pid": int(pid), "comm": read("/proc/%s/comm" % pid, "?"), "podUID": pod_uid,
                        "block": block, "perms": perms})
    return out


def unescape(field):
    """Return a mountinfo FIELD with its octal escapes (\\040) decoded."""
    return re.sub(r"\\([0-7]{3})", lambda m: chr(int(m.group(1), 8)), field)


def fs_mounts(blocks):
    """Return the mounts of the block devices BLOCKS, directly or through a loop device.

    Each has the filesystem type, whether its superblock is read-only (ro)
    and whether the mount point is (mountRO), and its kind: node (a node
    mount of mode fs), lease (a lease bind) or other.
    """
    out = []
    want = {"/dev/" + b: b for b in blocks}
    try:
        with open(path("/proc/self/mountinfo")) as f:
            lines = f.read().splitlines()
    except OSError:
        return out
    for line in lines:
        parts = line.split(" - ", 1)
        if len(parts) != 2:
            continue
        left, right = parts[0].split(), parts[1].split()
        if len(left) < 6 or len(right) < 2:
            continue
        target, source = unescape(left[4]), unescape(right[1])
        loop = bool(re.fullmatch(r"/dev/loop\d+", source))
        backing = read("/sys/block/%s/loop/backing_file" % source[len("/dev/"):]) if loop else source
        if backing not in want:
            continue
        opts = (right[2] if len(right) > 2 else "").split(",")
        m = {"target": target, "source": source, "block": want[backing], "loop": loop, "fstype": right[0],
             "ro": "ro" in opts, "mountRO": "ro" in left[5].split(","),
             "dax": "dax=always" in opts or "dax" in opts, "kind": "other"}
        if target.startswith(NODE_MOUNT_DIRS):
            m["kind"] = "node"
        else:
            lm = re.fullmatch(re.escape(LEASE_FS_DIR) + r"([^/]+)/([^/]+)", target)
            if lm:
                m.update(kind="lease", claimUID=lm.group(1), name=lm.group(2))
        out.append(m)
    return out


def node(errors):
    try:
        devs, endpoints = memdevs()
        regs = regions(endpoints)
        blocks = [n["block"] for r in regs for n in r.get("namespaces") or [] if n.get("block")]
        pids = pod_pids()
        out = {"hostname": read("/proc/sys/kernel/hostname", ""), "memdevs": devs, "regions": regs}
        fs_devs = {}
        if blocks:
            out["openers"] = openers(pids, blocks)
            out["mounts"] = fs_mounts(blocks)
            for m in out["mounts"]:
                source = m["source"][len("/dev/"):]
                num = read("/sys/class/block/%s/dev" % source)
                if num:
                    fs_devs[num] = (m["block"], source)
        out["mappers"] = mappers(pids, fs_devs)
        return out
    except Exception as e:   # the views show it, the probe must not die
        errors.append("node: %r" % e)
        return None


def read_bytes(serial, length, errors):
    serial = int_any(serial)
    devs, endpoints = memdevs()
    names = [d["name"] for d in devs if int_any(d["serial"]) == serial]
    if not names:
        return {"error": "no memdev with serial 0x%x" % (serial or 0)}
    for r in regions(endpoints):
        if names[0] in r["memdevs"] and r["dax"]:
            dax = r["dax"][0]
            try:
                fd = os.open(path("/dev/" + dax["name"]), os.O_RDONLY)
                try:
                    size = os.fstat(fd).st_size or (2 << 20)
                    m = mmap.mmap(fd, min(2 << 20, size), mmap.MAP_SHARED, mmap.PROT_READ)
                    data = bytes(m[:length])
                    m.close()
                finally:
                    os.close(fd)
            except OSError as e:
                return {"memdev": names[0], "region": r["name"], "dax": dax["name"], "error": str(e)}
            return {"memdev": names[0], "region": r["name"], "dax": dax["name"], "hex": data.hex()}
    return {"memdev": names[0], "error": "no dax device"}


def main():
    global ROOT
    args = sys.argv[1:]
    opts = {"--k8s": None, "--ns": "default", "--bytes": None, "--len": "256", "--root": ""}
    flags = set()
    i = 0
    while i < len(args):
        a = args[i]
        if a in opts and i + 1 < len(args):
            opts[a] = args[i + 1]
            i += 2
        elif a == "--node":
            flags.add(a)
            i += 1
        else:
            sys.exit("probe.py: unknown argument %s" % a)
    ROOT = opts["--root"]
    errors, out = [], {}
    threads = []
    if opts["--k8s"]:
        def run_k8s():
            out["k8s"] = k8s([k for k in opts["--k8s"].split(",") if k], opts["--ns"], errors)
        t = threading.Thread(target=run_k8s)
        t.start()
        threads.append(t)
    if "--node" in flags:
        out["node"] = node(errors)
    if opts["--bytes"]:
        try:
            out["bytes"] = read_bytes(opts["--bytes"], int(opts["--len"]), errors)
        except Exception as e:
            errors.append("bytes: %r" % e)
    for t in threads:
        t.join()
    out["errors"] = errors
    json.dump(out, sys.stdout)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
