#!/usr/bin/env python3
"""Zooms for questions: details the views leave out (docs/memory/cxl/demo.md, Look inside).

Usage: zoom.py WHAT [ARGS]
  slice [DEVICE]    the ResourceSlice entry of DEVICE (default: every memdev):
                    its nodes, attributes, capacity
  claim POD         the companion claims of POD's leases (of a pod of the
                    wizard also its ResourceClaim): request, allocation,
                    config
  pod POD           why the companions of POD's leases (a pod of the
                    wizard: the pod) wait (PodScheduled)
  env POD           the CXL_* variables of POD's leases (of a pod of the
                    wizard also those of its container)
  cdi NODE          the CDI specs of the node's claims and leases (a device
                    per allocation result with its marker
                    CXL_CLAIM_<uid>_<n>, a device per lease claim)
  driver NODE [N]   the last N (30) lines of the driver's log
  kernel NODE       the kernel lines of cxl, dax, memory since demo setup
                    (the journal, the host's clock)
  cxl NODE          cxl list -M -R -X -u -i (memdevs, disabled ones too,
                    regions, dax)
  pool              the pool server's devices and attachments
  fs                the erofs in the pool device's backing file: the
                    superblock and the files of the root directory; in
                    pool-fs the superblock of the ext4, xfs or btrfs (type,
                    size, label, ext4's state), no files
  mounts NODE       the mounts of the node's pmem block devices (mount point
                    and superblock options: ro or rw), their read-only
                    flags, loop devices and lease binds
  bytes [OFFSET]    a hex dump of 256 bytes of the demo device from OFFSET
                    (default 0); in pool-shared-fs and pool-fs of its block
                    device (1024 is the erofs and the ext4 superblock)
  last              the full output of the last step
NODE is vm1 or vm2, or vm0 in llm-share. The output is plain text; "demo
zoom" pages it.
"""
import json
import os
import re
import shlex
import sys

import common as c
import steps
import fsinfo


def vmdir(state, node):
    key = {"vm0": "VM0_DIR", "vm1": "VM1_DIR", "vm2": "VM2_DIR"}.get(node)
    if not key or not state.get(key):
        sys.exit("NODE is vm1 or vm2%s, not %r" % (", or vm0" if state.get("VM0_DIR") else "", node))
    return state[key]


def k8s(state, kinds):
    p = c.run_probe(state["VM2_DIR"], ["--k8s", kinds, "--ns", state["NS"]], timeout=10)
    for e in p.get("errors") or []:
        print("error: " + e)
    return p.get("k8s") or {}


def companions(state, pod, kinds="claims"):
    k = k8s(state, kinds + ",pods")
    objs = [o for o in c.items(k.get("claims")) + c.items(k.get("pods")) if c.lease_id(o) and c.requester(o) == pod]
    # A pod of the wizard and its own claim (not the claim of its lease template).
    objs += [o for o in c.items(k.get("claims")) + c.items(k.get("pods")) if c.labels(o).get("cxl-demo/wizard") == pod
             and not any(s.get("deviceClassName") == "cxl-lease" for s in c.subrequests(o))]
    if not objs:
        print("%s has no leases" % pod)
    return objs


def show(obj, indent="  "):
    print("\n".join(indent + l for l in json.dumps(obj, indent=1, sort_keys=True).splitlines()))


def slice_scope(spec, state):
    """Return the nodes of a ResourceSlice spec in words: node vm2, nodes vm1 vm2, all nodes."""
    if spec.get("nodeName"):
        return "node " + c.short_node(spec["nodeName"], state)
    if spec.get("allNodes"):
        return "all nodes"
    nodes = []
    for term in (spec.get("nodeSelector") or {}).get("nodeSelectorTerms") or []:
        for e in term.get("matchExpressions") or []:
            if e.get("operator") == "In":
                nodes += [c.short_node(v, state) for v in e.get("values") or []]
    return "nodes " + " ".join(sorted(set(nodes))) if nodes else "nodes of a selector"


def zoom_slice(state, args):
    k = k8s(state, "slices")
    for s in c.items(k.get("slices")):
        spec = s.get("spec") or {}
        for d in spec.get("devices") or []:
            if args and d.get("name") != args[0]:
                continue
            if not args and c.device_attr(d, "type") != "memdev":
                continue
            show_device(state, spec, d)


def show_device(state, spec, d):
    """Print one device of a ResourceSlice: where, attributes, capacity."""
    drv, pool = spec.get("driver"), (spec.get("pool") or {}).get("name")
    print("%s  driver %s, pool %s, %s" % (d.get("name"), drv, pool, slice_scope(spec, state)))
    attrs = sorted(d.get("attributes") or {})
    width = max([14] + [len(name) for name in attrs])
    for name in attrs:
        print("  %-*s %s" % (width, name, c.device_attr(d, name)))
    for name, cap in sorted((d.get("capacity") or {}).items()):
        pol = cap.get("requestPolicy")
        print("  capacity %s: %s%s" % (name, cap.get("value"), " policy %s" % json.dumps(pol) if pol else " (no request policy)"))
    if d.get("allowMultipleAllocations"):
        print("  allowMultipleAllocations: true")


def zoom_claim(state, args):
    for cl in companions(state, args[0]):
        if cl.get("kind") != "ResourceClaim":
            continue
        print("claim %s%s%s" % (c.meta(cl).get("name"), " (lease %s)" % c.lease_id(cl) if c.lease_id(cl) else "",
                                " deleting" if c.deleting(cl) else ""))
        for r in c.subrequests(cl):
            print("  request %s: class %s" % (r.get("name"), r.get("deviceClassName")))
            for s in r.get("selectors") or []:
                print("    selector " + (s.get("cel") or {}).get("expression", "?"))
            print("    capacity %s" % json.dumps((r.get("capacity") or {}).get("requests") or "none (the whole device)"))
        if not c.allocation(cl):
            print("  allocation: none (waiting)")
        for res in c.results(cl):
            print("  allocated %s/%s on %s" % (res.get("pool"), res.get("device"), c.short_node(c.allocated_node(cl), state)))
            print("    consumed %s" % json.dumps(res.get("consumedCapacity") or {}))
        for drv, reqs, params in c.claim_configs(cl):
            print("  config %s%s: %s" % (drv, " " + ",".join(reqs) if reqs else "", json.dumps(params, sort_keys=True)))


def zoom_pod(state, args):
    for p in companions(state, args[0], "claims"):
        if p.get("kind") != "Pod":
            continue
        st = p.get("status") or {}
        print("%s %s: %s on %s" % ("companion pod" if c.lease_id(p) else "pod", c.meta(p).get("name"), st.get("phase"), c.short_node((p.get("spec") or {}).get("nodeName"), state)))
        for cond in st.get("conditions") or []:
            if cond.get("type") == "PodScheduled":
                print("  PodScheduled %s %s" % (cond.get("status"), cond.get("reason") or ""))
                if cond.get("message"):
                    print("  " + c.short_text(cond["message"], state))


def pool_device(state):
    """Return the pool server's JSON of the demo's pool device."""
    return c.http_json("%s/api/v1/devices/%s" % (state["POOL_URL"], state.get("POOL_DEVICE", "shared0")))


def zoom_fs(state):
    if not (state.get("DEMO_CASE") or "").startswith(("pool", "wizard")):
        sys.exit("zoom fs: the file system is in the pool device of pool-shared-fs, not in the %s demo" % state.get("DEMO_CASE"))
    dev = pool_device(state)
    if state.get("DEMO_CASE") in ("pool-fs", "wizard"):
        with open(dev["path"], "rb") as f:
            off, data = c.read_fsdax(f, fsinfo.READ_LEN)
        print("%s, the backing file %s on the host; its block device (pmem) starts at %d:" % (dev["name"], dev["path"], off))
        print("\n".join(fsinfo.details(fsinfo.fs_super(data))))
        return
    with open(dev["path"], "rb") as f:
        off, data = c.read_fsdax(f, 1 << 20)
    fs = c.erofs_files(data)
    print("%s, the backing file %s on the host; its block device (pmem) starts at %d:" % (dev["name"], dev["path"], off))
    if fs["type"] != "erofs":
        print("no file system%s" % (" (the first MiB is zeros)" if fs["type"] == "zeros" else " (no erofs magic at 1024)"))
        return
    sb = fs["super"]
    print("erofs superblock at 1024: magic %s, block size %d, inodes %d, blocks %d, root nid %d" % (
        sb["magic"], sb["blocksize"], sb["inos"], sb["blocks"], sb["rootNid"]))
    print("  meta_blkaddr %d, xattr_blkaddr %d, feature_compat %s, feature_incompat %s" % (
        sb["metaBlkaddr"], sb["xattrBlkaddr"], sb["featureCompat"], sb["featureIncompat"]))
    print("root directory:")
    for f in fs["files"]:
        size = "" if f.get("size") is None else " %d bytes" % f["size"]
        text = ' "%s"' % f["text"] if f.get("text") and f["text"] != "(not readable)" else (" " + f["text"] if f.get("text") else "")
        print("  %s %s%s%s" % (f["type"], f["name"], size, text))
    if fs["error"]:
        print("not readable: " + fs["error"])


MOUNTS_SCRIPT = r"""
echo '== mounts of pmem and loop devices, node mounts, lease binds: target source type mount-point superblock'
findmnt -rn -o TARGET,SOURCE,FSTYPE,VFS-OPTIONS,FS-OPTIONS |
    awk '$2 ~ /^\/dev\/(pmem|loop)/ || $1 ~ /^\/var\/lib\/kubelet\/plugins\/cxl(-pool)?\.nri\.io\/(mounts|lease-fs)\// {n++; print} END {if (!n) print "none"}'
echo '== read-only flags of the pmem block devices'
for f in /sys/block/pmem*/ro; do [ -e "$f" ] && echo "${f%/ro}: $(cat "$f")"; done; ls /sys/block/pmem* >/dev/null 2>&1 || echo none
echo '== loop devices'; l=$(losetup -l); echo "${l:-none}"
echo '== lease binds'; l=$(find /var/lib/kubelet/plugins/cxl.nri.io/lease-fs -mindepth 2 -maxdepth 2 2>&1); echo "${l:-none}"
"""


# A spec per line: zoom_cdi indents the JSON.
CDI_SCRIPT = r"""
n=0
for f in /var/run/cdi/cxl*.json; do
    [ -f "$f" ] || continue
    n=$((n + 1))
    echo "== $f"
    tr -d '\n' < "$f"
    echo
done
[ "$n" -gt 0 ] || echo 'no CDI specs of the CXL drivers: no claim or lease is prepared on this node'
# Companion claims of leases get no CDI device: their memory reaches the
# container through its lease device (lease-<uid> of cxl-claims.json).
[ -s /var/run/cdi/cxl-pool-claims.json ] ||
    echo 'no pool claims with CDI devices (cxl-pool-claims.json): leases have none, their memory comes through the lease device'
"""

# The kernel lines of CXL, dax and memory since a time (%(since)s, epoch
# seconds), in the clock of the host (TZ %(tz)s); dmesg since boot without
# a journal.
KERNEL_SCRIPT = r"""
pat='cxl|dax|memory|region'
skip='(save|restore) config|pci_notify|IRQ mapping|bus mastering'
since='%(since)s'
if [ -n "$since" ] && journalctl -k -q -n 1 --no-pager >/dev/null 2>&1; then
    echo "== kernel lines since demo setup ($(TZ=%(tz)s date -d "@$since" '+%%Y-%%m-%%d %%H:%%M:%%S'))"
    TZ=%(tz)s journalctl -k -q --no-pager -o short-iso --since "@$since" | grep -iE "$pat" | grep -vE "$skip" | tail -n 40
else
    echo "== kernel lines since boot (no demo setup time or no journal): the older ones are of earlier tests"
    dmesg -T | grep -iE "$pat" | grep -vE "$skip" | tail -n 40
fi
"""


def zoom_kernel(state, node):
    tz = (steps.host_tz().split("=", 1) + [""])[1] or "UTC"
    since = state.get("DEMO_SETUP_START", "")
    zoom_shell(state, node, KERNEL_SCRIPT % {"since": since if since.isdigit() else "", "tz": tz})


def zoom_mounts(state, node):
    zoom_shell(state, node, MOUNTS_SCRIPT)


def pod_arg(name):
    """Return NAME quoted for a shell if it is a pod name (a DNS label);
    exit otherwise."""
    if not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", name):
        sys.exit("POD is a pod name (lower-case letters, digits, -), not %r" % name)
    return shlex.quote(name)


def zoom_shell(state, node, script, timeout=30):
    rc, out = c.run_vm(vmdir(state, node), script, timeout)
    print(c.short_text(out.rstrip(), state))


def zoom_cdi(state, node):
    rc, out = c.run_vm(vmdir(state, node), CDI_SCRIPT, 30)
    lines = []
    for line in out.rstrip().splitlines():
        if line.startswith("{"):
            try:
                line = json.dumps(json.loads(line), indent=2)
            except ValueError:
                pass
        lines.append(line)
    print(c.short_text("\n".join(lines), state))


def main():
    args = sys.argv[1:]
    if not args or args[0] in ("-h", "--help", "help"):
        sys.exit(__doc__)
    if args[0] == "last":
        try:
            with open(os.path.join(c.STATE_DIR, "steps.log")) as f:
                text = f.read()
        except OSError:
            sys.exit("no steps run yet")
        print(text[text.rfind("=== "):].rstrip())
        return
    state = c.load_state()
    if state is None:
        sys.exit("demo not set up: demo setup local|pool|pool-shared-fs|pool-fs|wizard")
    what, rest = args[0], args[1:]
    ns = state["NS"]
    if what == "slice":
        zoom_slice(state, rest)
    elif what == "claim" and rest:
        zoom_claim(state, rest)
    elif what == "pod" and rest:
        zoom_pod(state, rest)
    elif what == "env" and rest:
        if state.get("DEMO_CASE") == "wizard":
            zoom_shell(state, "vm2", "kubectl exec -n %s %s -- sh -c 'python3 /demo/cxl-wizard.py env; "
                       "[ -z \"$CXL_LEASE_SOCKET\" ] || /demo/env'" % (ns, pod_arg(rest[0])))
        else:
            zoom_shell(state, "vm2", "kubectl exec -n %s %s -- /demo/env" % (ns, pod_arg(rest[0])))
    elif what == "cdi" and rest:
        zoom_cdi(state, rest[0])
    elif what == "driver" and rest:
        vmdir(state, rest[0])   # refuses a NODE that the demo does not have
        n = int(rest[1]) if len(rest) > 1 else 30
        node = state.get({"vm0": "VM0_NODE", "vm1": "VM1_NODE"}.get(rest[0], "VM2_NODE")) or state["VM2_NODE"]
        zoom_shell(state, "vm2", "pod=$(kubectl get pods -n kube-system -l app.kubernetes.io/name=nri-dra-driver-cxl "
                   "--field-selector spec.nodeName=%s -o name | head -n 1); kubectl logs -n kube-system --tail=%d -c nri-dra-driver-cxl $pod "
                   "| grep -vE 'CA file|Watch close|Waited before sending request' | tail -n %d "
                   "| sed -E 's/^[IWEF][0-9]{4} [0-9:.]+ +[0-9]+ [^]]+\\] //'" % (node, 4 * n, n))
    elif what == "kernel" and rest:
        zoom_kernel(state, rest[0])
    elif what == "cxl" and rest:
        zoom_shell(state, rest[0], "cxl list -M -R -X -u -i 2>&1")
    elif what == "pool":
        for kind in ("devices", "attachments"):
            print("== %s" % kind)
            try:
                for o in c.http_json("%s/api/v1/%s" % (state["POOL_URL"], kind)) or []:
                    if kind == "devices":
                        print("%s %s %s %s %s scope %s%s" % (o.get("name"), o.get("serial"), c.k8s_size(o.get("size")),
                                                             c.device_kind(o), o.get("state"),
                                                             o.get("scope"), "  file " + o["path"] if o.get("path") else ""))
                    else:
                        print("%s %s%s owner %s" % (c.short_text(o.get("id", "?"), state), o.get("state"),
                                                   " single host" if o.get("singleHost") else "", o.get("owner") or "-"))
            except OSError as e:
                print("pool server: %s" % e)
    elif what == "fs":
        zoom_fs(state)
    elif what == "mounts" and rest:
        zoom_mounts(state, rest[0])
    elif what == "bytes":
        start = c.int_any(rest[0]) if rest else 0
        if start is None or not 0 <= start <= (2 << 20) - 256:
            sys.exit("bytes [OFFSET]: OFFSET is 0 to %d" % ((2 << 20) - 256))
        if state.get("DEMO_CASE") in ("pool-shared-fs", "pool-fs", "wizard"):
            dev = pool_device(state)
            with open(dev["path"], "rb") as f:
                off, data = c.read_fsdax(f, start + 256)
            data = data[start:]
            print("%s, the block device in the backing file %s (from %d), from %d:" % (dev["name"], dev["path"], off, start))
        elif state.get("DEMO_CASE") == "pool":
            dev = pool_device(state)
            with open(dev["path"], "rb") as f:
                f.seek(start)
                data = f.read(256)
            print("%s, the backing file %s on the host, from %d:" % (dev["name"], dev["path"], start))
        else:
            b = c.run_probe(state["VM2_DIR"], ["--bytes", state["SERIAL"], "--len", str(start + 256)], timeout=10).get("bytes") or {}
            if not b.get("hex"):
                sys.exit("cannot read: %s" % b.get("error"))
            data = bytes.fromhex(b["hex"])[start:]
            print("/dev/%s (%s, %s) on vm2, from %d:" % (b.get("dax"), b.get("region"), b.get("memdev"), start))
        for off in range(0, len(data), 16):
            chunk = data[off:off + 16]
            print("%04x  %-48s %s" % (start + off, " ".join("%02x" % x for x in chunk),
                                     "".join(chr(x) if 32 <= x < 127 else "." for x in chunk)))
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
