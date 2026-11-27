#!/usr/bin/env python3
"""Ground truth of one moment of the demo, to check every line of the views.

Usage: truth.py LABEL [--since EPOCH]

Writes captures/<case>-LABEL-truth.txt with the raw state of the moment,
taken in parallel, each section stamped with the host clock:
- the cluster (in vm2): kubectl get pods,resourceclaims -o yaml in the
  demo namespace, its resourceclaimtemplates, resourceslices -o yaml, the
  driver pods, the events of the demo namespace;
- each node (vm2, vm1, and vm0 in llm-share): cxl list -M -R -X -u -i, /sys/bus/dax/devices (driver,
  size, target node), the pmem block devices (size, read-only flag,
  filesystem of blkid -p), the mountinfo lines of the node mounts, lease
  binds and mounts of pmem and loop devices, the loop devices, the
  processes of the node that map a dax device or have a pmem block device
  open (with the pod uid of their cgroup), the first 64 bytes of every dax
  device;
- the pool server (POOL_URL) and in the wizard case the local-pmem
  server (LP_URL): their devices and attachments, and the backing file of
  the demo device (POOL_DEVICE, LP_DEVICE): its first bytes, the pfn info
  block of a persistent device and what blkid -p (and dumpe2fs -h of
  ext4) of the host say of the filesystem after it, where "Hello" is;
- the trace (the streams of dra-trace-start that T5 reads: drivers,
  controller, pool server, kube events, claims, udev): every line since
  the start of the last step of run/step.env, or since EPOCH.
"""
import json
import os
import shlex
import shutil
import subprocess
import tempfile
import sys
import threading
import time

import common as c

CLUSTER_SCRIPT = r"""
export KUBECONFIG=/root/.kube/config
ns=%(ns)s
echo "== kubectl get pods,resourceclaims -n $ns -o yaml"
kubectl get pods,resourceclaims -n "$ns" -o yaml 2>&1
echo "== kubectl get resourceslices -o yaml"
kubectl get resourceslices -o yaml 2>&1
echo "== kubectl get resourceclaimtemplates -n $ns -o yaml"
kubectl get resourceclaimtemplates -n "$ns" -o yaml 2>&1
echo "== the driver pods"
kubectl get pods -n kube-system -l app.kubernetes.io/name=nri-dra-driver-cxl -o wide 2>&1
echo "== the events of $ns, the last 40"
kubectl get events -n "$ns" --sort-by=.lastTimestamp \
    -o custom-columns=LAST:.lastTimestamp,KIND:.involvedObject.kind,NAME:.involvedObject.name,REASON:.reason,MSG:.message 2>&1 | tail -n 40
"""

NODE_SCRIPT = r"""
echo "== $(hostname)"
echo "== cxl list -M -R -X -u -i"
cxl list -M -R -X -u -i 2>&1
echo "== /sys/bus/dax/devices: name driver size target_node"
n=0
for d in /sys/bus/dax/devices/*; do
    [ -e "$d" ] || continue
    n=$((n + 1))
    drv=$(readlink "$d/driver" 2>/dev/null)
    echo "${d##*/} ${drv##*/} $(cat "$d/size" 2>/dev/null) $(cat "$d/target_node" 2>/dev/null)"
done
[ "$n" -gt 0 ] || echo none
echo "== pmem block devices: name bytes ro blkid -p"
n=0
for b in /sys/block/pmem*; do
    [ -e "$b" ] || continue
    n=$((n + 1))
    name=${b##*/}
    echo "$name $(( $(cat "$b/size") * 512 )) ro=$(cat "$b/ro") $(blkid -p -o export "/dev/$name" 2>&1 | grep -E '^(TYPE|UUID|LABEL)=' | tr '\n' ' ')"
done
[ "$n" -gt 0 ] || echo none
echo "== mountinfo: node mounts, lease binds, mounts of pmem and loop devices"
grep -E ' /var/lib/kubelet/plugins/cxl(-pool)?\.nri\.io/(mounts|lease-fs)/| - [^ ]+ /dev/(pmem|loop)' /proc/self/mountinfo || echo none
echo "== losetup -l"
losetup -l 2>&1
echo "== processes that map a dax device: pid comm pod perms device"
grep -l ' /dev/dax' /proc/[0-9]*/maps 2>/dev/null | while read -r f; do
    p=${f%/maps}
    pod=$(grep -o 'pod[0-9a-f_-]\{36\}' "$p/cgroup" 2>/dev/null | head -n 1)
    awk -v pid="${p#/proc/}" -v comm="$(cat "$p/comm" 2>/dev/null)" -v pod="${pod:--}" \
        '$6 ~ /^\/dev\/dax/ {print pid, comm, pod, $2, $6}' "$f" | sort -u
done
echo "== processes with a pmem block device open: pid fd device flags comm pod"
find /proc/[0-9]*/fd -maxdepth 1 -lname '/dev/pmem*' -printf '%h %f %l\n' 2>/dev/null | while read -r dir fd target; do
    p=${dir%/fd}
    pod=$(grep -o 'pod[0-9a-f_-]\{36\}' "$p/cgroup" 2>/dev/null | head -n 1)
    echo "${p#/proc/} $fd $target flags=$(awk '/^flags:/ {print $2}' "$p/fdinfo/$fd" 2>/dev/null) $(cat "$p/comm" 2>/dev/null) ${pod:--}"
done
echo "== the first 64 bytes of the dax devices"
n=0
for d in /dev/dax*; do
    [ -e "$d" ] || continue
    n=$((n + 1))
    python3 -c 'import mmap, os, sys
fd = os.open(sys.argv[1], os.O_RDONLY)
m = mmap.mmap(fd, 2 << 20, mmap.MAP_SHARED, mmap.PROT_READ)
print(sys.argv[1], repr(bytes(m[:64])))' "$d" 2>&1
done
[ "$n" -gt 0 ] || echo none
"""


def stamp(t=None):
    """Return the host clock T as HH:MM:SS.mmm."""
    t = time.time() if t is None else t
    return time.strftime("%H:%M:%S", time.localtime(t)) + ".%03d" % int((t % 1) * 1000)


def vm_section(title, vmdir, script):
    """Return the lines of SCRIPT run as root in the VM of VMDIR, under TITLE."""
    t = time.time()
    rc, out = c.run_vm(vmdir, script, timeout=60)
    head = "#### %s, taken at %s%s" % (title, stamp(t), "" if rc == 0 else ", exit status %d" % rc)
    return [head] + out.rstrip("\n").splitlines()


def pool_section(url, name, title):
    """Return the lines of the devices and attachments of the pool server at URL and of the backing file of its device NAME."""
    t = time.time()
    out = ["#### %s %s, taken at %s" % (title, url, stamp(t))]
    dev = None
    for kind in ("devices", "attachments"):
        out.append("== GET /api/v1/%s" % kind)
        try:
            doc = c.http_json("%s/api/v1/%s" % (url, kind))
        except (OSError, ValueError) as e:
            out.append("error: %s" % e)
            continue
        out += json.dumps(doc, indent=1, sort_keys=True).splitlines()
        if kind == "devices":
            dev = next((d for d in doc or [] if d.get("name") == name), None)
    path = (dev or {}).get("path")
    out.append("== the backing file of %s: %s" % (name, path))
    if not path:
        return out
    try:
        with open(path, "rb") as f:
            head = f.read(8192)
            out.append("at 0: %s" % repr(head[:64]))
            out.append("at 4096 (the pfn info block of a persistent device): %s" % repr(head[4096:4096 + 64]))
            if dev.get("persistent"):
                out += superblock_lines(path, c.fsdax_data_offset(head))
            f.seek(0)
            found, base, tail = {}, 0, b""
            while len(found) < 4:
                chunk = f.read(16 << 20)
                if not chunk:
                    break
                data = tail + chunk
                i = data.find(b"Hello")
                while i >= 0 and len(found) < 4:
                    found.setdefault(base - len(tail) + i, data[i:i + 64])
                    i = data.find(b"Hello", i + 1)
                tail = data[-64:]
                base += len(chunk)
            out += ['"Hello" at %d: %s' % (o, repr(b.split(b"\0", 1)[0])) for o, b in sorted(found.items())] or ['no "Hello" in it']
    except OSError as e:
        out.append("error: %s" % e)
    return out


def superblock_lines(path, offset):
    """Return what the filesystem tools of the host say of the filesystem OFFSET bytes into the backing file PATH.

    blkid -p for every type; dumpe2fs -h for ext2/3/4 (state, mount and
    write times); the file is only read.
    """
    out = ["== the block device in the backing file starts at %d (the pfn info block's dataoff)" % offset]

    def run(title, cmd):
        out.append("== " + title)
        try:
            p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=20)
            text = p.stdout.decode(errors="replace").rstrip()
            out.extend((text or "(no output, exit status %d)" % p.returncode).splitlines())
        except (OSError, subprocess.TimeoutExpired) as e:
            out.append("error: %s" % e)
    blkid = shutil.which("blkid") or "/usr/sbin/blkid"
    first = len(out)
    run("blkid -p -O %d -o export" % offset, [blkid, "-p", "-O", str(offset), "-o", "export", path])
    if any(l.startswith("TYPE=ext") for l in out[first:]):
        # dumpe2fs takes no offset: a copy of the first 8 MiB of the filesystem
        dumpe2fs = shutil.which("dumpe2fs") or "/usr/sbin/dumpe2fs"
        with tempfile.NamedTemporaryFile() as head, open(path, "rb") as f:
            f.seek(offset)
            head.write(f.read(8 << 20))
            head.flush()
            run("dumpe2fs -h of the first 8 MiB from %d" % offset, [dumpe2fs, "-h", head.name])
    return out


def trace_section(state, since):
    """Return the lines of the trace streams since SINCE, in the order of their host stamps."""
    tdir = state.get("TRACE_DIR")
    out = ["#### the trace %s since %s" % (tdir, stamp(since))]
    rows = []
    try:
        names = sorted(f for f in os.listdir(tdir or "") if f.endswith(".log"))
    except OSError as e:
        return out + ["error: %s" % e]
    for name in names:
        try:
            with open(os.path.join(tdir, name), errors="replace") as f:
                for line in f:
                    try:
                        t = float(line.split(" ", 1)[0])
                    except ValueError:
                        continue
                    if t >= since:
                        rows.append((t, line.rstrip("\n")))
        except OSError as e:
            out.append("error: %s: %s" % (name, e))
    rows.sort(key=lambda r: r[0])
    return out + ["%s %s" % (stamp(t), line.split(" ", 1)[1]) for t, line in rows] + ["(%d lines)" % len(rows)]


def main():
    args = sys.argv[1:]
    if not args or args[0].startswith("-"):
        sys.exit(__doc__)
    label = args[0]
    state = c.load_state()
    if state is None:
        sys.exit("demo not set up")
    case = state.get("DEMO_CASE", "?")
    step = c.load_state(os.path.join(c.STATE_DIR, "step.env")) or {}
    since = time.time() - 60
    since_what = "60 s before"
    if "--since" in args:
        since, since_what = float(args[args.index("--since") + 1]), "--since"
    elif step.get("START"):
        since, since_what = float(step["START"]), "the start of step %s" % step.get("STEP", "?")
    t0 = time.time()
    sections = {}
    jobs = {
        "cluster": lambda: vm_section("the cluster (kubectl in vm2)", state["VM2_DIR"],
                                      CLUSTER_SCRIPT % {"ns": shlex.quote(state.get("NS", "default"))}),
        "vm2": lambda: vm_section("node vm2 (%s)" % state.get("VM2_NODE"), state["VM2_DIR"], NODE_SCRIPT),
        "vm1": lambda: vm_section("node vm1 (%s)" % state.get("VM1_NODE"), state["VM1_DIR"], NODE_SCRIPT),
        "trace": lambda: trace_section(state, since),
    }
    if state.get("VM0_DIR"):
        jobs["vm0"] = lambda: vm_section("node vm0 (%s)" % state.get("VM0_NODE"), state["VM0_DIR"], NODE_SCRIPT)
    if state.get("POOL_URL"):
        jobs["pool"] = lambda: pool_section(state["POOL_URL"], state.get("POOL_DEVICE", "shared0"), "the pool server")
    if state.get("LP_URL"):
        jobs["lp"] = lambda: pool_section(state["LP_URL"], state.get("LP_DEVICE", "lp0"), "the local-pmem pool server")

    def run(key, fn):
        try:
            sections[key] = fn()
        except Exception as e:   # one failed section must not lose the others
            sections[key] = ["#### %s: failed: %r" % (key, e)]
    threads = [threading.Thread(target=run, args=kv) for kv in jobs.items()]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    head = ["# demo truth %s of case %s at %s (host clock); logs since %s (%s)" % (
        label, case, time.strftime("%Y-%m-%d ", time.localtime(t0)) + stamp(t0), stamp(since), since_what)]
    lines = head
    for key in ("cluster", "vm2", "vm1", "vm0", "pool", "lp", "trace"):
        if key in sections:
            lines += [""] + sections[key]
    os.makedirs(os.path.join(c.DEMO_DIR, "captures"), exist_ok=True)
    path = os.path.join(c.DEMO_DIR, "captures", "%s-%s-truth.txt" % (case, label))
    with open(path, "w") as f:
        f.write("\n".join(lines) + "\n")
    print("demo truth: %s (%d lines, %.1f s)" % (path, len(lines), time.time() - t0))


if __name__ == "__main__":
    main()
