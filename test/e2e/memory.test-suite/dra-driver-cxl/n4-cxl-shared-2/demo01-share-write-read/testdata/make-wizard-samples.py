#!/usr/bin/env python3
"""Write the generated samples of the case "wizard": DIR/wizard-*/
and DIR/trace-wizard/.

Usage: make-wizard-samples.py DIR (bin/common.py testdata() runs it)

wizard-W0 is the cluster after demo setup wizard: the published devices
(the local memdevs of vm2 and vm1, lp0 of vm2, pooled0, shared0 and fs0
of the pool) and no objects of the wizard. wizard-W8 has eight objects of
the wizard, made with the claims that bin/wizard.py writes for the
options of each (and the companion claims of the broker for the leases),
in the states that the scripted set (bin/wizard-rules.sh) shows: Running
claims and Ready leases of local slices, of shared0 exclusively, of the
filesystem of fs0 read-write and of the block device of lp0; a lease that
waits for shared0, a claim of fs0 of the other node that waits (AttachFailed
Conflict), and a claim that no device satisfies. The objects of the probe
(vm2.json, vm1.json) and of the pool servers (pool.json: the devices of
fake-cxl-pool-server, lp0 of local-pmem, the filesystem state of the
persistent ones) are those of bin/wizard_views.py. trace-wizard has the
streams of dra-trace-start of a few of those steps.
"""
import importlib.util
import json
import os
import sys
import zlib

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "bin"))
import common    # noqa: E402
import wizard    # noqa: E402

spec = importlib.util.spec_from_file_location("make_samples", os.path.join(HERE, "make-samples.py"))
ms = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ms)

VM1, VM2, NS, GI, MI = ms.VM1, ms.VM2, ms.NS, ms.GI, ms.MI
VM2_LOCAL = "0xc100e2e1"
VM1_LOCAL = "0xc100e2e1"   # the VMs' local devices have the same serial
LP = "0xc100f5e1"
POOLED = "0xc1ee0001"
SHARED0 = "0xc1ae0001"
FS0 = "0xc1ae0002"
BLOCK = 128 * MI


def state_text():
    return "\n".join([
        "# generated state of demo setup wizard (make-wizard-samples.py)",
        "DEMO_CASE=wizard", "VM2_DIR=/nonexistent/n4-cxl-shared-2-fedora-43-containerd",
        "VM1_DIR=/nonexistent/n4-cxl-shared-1-fedora-43-containerd", "VM2_NODE=" + VM2, "VM1_NODE=" + VM1,
        "NS=" + NS, "SERIAL=" + FS0, "POOL_DEVICE=fs0", "POOL_URL=http://127.0.0.1:9909",
        "LP_URL=http://127.0.0.1:9912", "LP_DEVICE=lp0", "LP_SERIAL=" + LP,
        "VM2_LOCAL_SERIAL=" + VM2_LOCAL, "VM1_LOCAL_SERIAL=" + VM1_LOCAL,
        "FS_TYPES=erofs,ext4,xfs,btrfs", "FS_SETUP=ext4", "FS_SERIALS=%s,%s" % (FS0, LP),
        "DEMO_IMAGE=localhost/cxl-demo-tools:demo", "TRACE_DIR=trace", ""])


def sliced(name, serial, size):
    return {"name": name, "allowMultipleAllocations": True,
            "attributes": {"type": {"string": "memdev"}, "source": {"string": "local"}, "serial": {"string": serial},
                           "shared": {"bool": False}, "persistent": {"bool": False}, "size": {"int": size},
                           "resource.kubernetes.io/numaNode": {"int": 0}, "memoryBlockSize": {"int": BLOCK}},
            "capacity": {"memory": {"value": common.k8s_size(size),
                                    "requestPolicy": {"default": common.k8s_size(size),
                                                      "validRange": {"min": "128Mi", "step": "128Mi"}}}},
            "nodeAllocatableResources": {"memory": {"mapping": {"capacityKey": "memory", "capacityMultiplier": "1"}}}}


def persistent_local(name, serial, size):
    return {"name": name, "allowMultipleAllocations": True,
            "attributes": {"type": {"string": "memdev"}, "source": {"string": "local"}, "serial": {"string": serial},
                           "shared": {"bool": False}, "persistent": {"bool": True}, "size": {"int": size},
                           "resource.kubernetes.io/numaNode": {"int": 1}},
            "capacity": {"shares": {"value": "1M"}}}


def node_slice(node, devs):
    base = ms.local_slice(node, memdev=False)
    base["spec"]["devices"] = devs + base["spec"]["devices"]
    return base


def pool_device(name, serial, size, shared, persistent):
    d = {"name": name, "attributes": {"type": {"string": "memdev"}, "source": {"string": "pool"}, "serial": {"string": serial},
                                      "shared": {"bool": shared}, "persistent": {"bool": persistent}, "size": {"int": size},
                                      "coherent": {"bool": True}},
         "bindsToNode": True, "bindingConditions": ["cxl-pool.nri.io/Attached"],
         "bindingFailureConditions": ["cxl-pool.nri.io/AttachFailed"]}
    if shared:
        d["allowMultipleAllocations"] = True
        d["capacity"] = {"shares": {"value": "1M"}}
    else:
        d["capacity"] = {"memory": {"value": common.k8s_size(size)}}
    return d


def slices():
    pool = ms.pool_slice(False)
    pool["spec"]["devices"] = [pool_device("pooled0", POOLED, 512 * MI, False, False),
                               pool_device("shared0", SHARED0, 256 * MI, True, False),
                               pool_device("fs0", FS0, 512 * MI, True, True)]
    return [node_slice(VM2, [sliced("memdev-c100e2e1", VM2_LOCAL, GI), persistent_local("memdev-c100f5e1", LP, 512 * MI)]),
            node_slice(VM1, [sliced("memdev-c100e2e1", VM1_LOCAL, 256 * MI)]), pool]


def k8s(pods, claims):
    return {"pods": ms.lst(pods), "claims": ms.lst(claims), "slices": ms.lst(slices()), "drivers": ms.lst(ms.drivers())}


def pod(name, node, phase="Running", claim=None, lease=False, unschedulable=False):
    p = ms.pod(name, node, phase=phase, scheduled=phase == "Running")
    p["metadata"]["labels"] = wizard.labels(name)
    if lease:
        p["spec"]["resourceClaims"] = [{"name": "lease", "resourceClaimTemplateName": name + "-lease"}]
        p["status"]["resourceClaimStatuses"] = [{"name": "lease", "resourceClaimName": name + "-lease-x7k2p"}]
    else:
        p["spec"]["resourceClaims"] = [{"name": "mem", "resourceClaimName": claim or name}]
    if unschedulable:
        p["status"]["conditions"] = [{"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
                                      "message": "0/3 nodes are available: 3 cannot allocate all claims."}]
    return p


def allocate(cl, node, driver, pool, device, consumed, request="mem"):
    """Give the claim CL the allocation of DEVICE on NODE with the CONSUMED capacity."""
    cfg = [dict(e, source="FromClaim") for e in cl["spec"]["devices"].get("config") or []]
    r = {"request": request, "driver": driver, "pool": pool, "device": device}
    if consumed:
        r["consumedCapacity"] = consumed
    cl.setdefault("status", {})["allocation"] = {
        "devices": {"results": [r], "config": cfg},
        "nodeSelector": {"nodeSelectorTerms": [{"matchFields": [{"key": "metadata.name", "operator": "In", "values": [node]}]}]}}
    cl["status"]["reservedFor"] = [{"resource": "pods", "name": c_name(cl)}]
    if driver == "cxl-pool.nri.io":
        # The pool controller's status of an attached device.
        cl["status"]["devices"] = [{"driver": driver, "pool": pool, "device": device, "conditions": [
            {"type": "cxl-pool.nri.io/Attached", "status": "True", "reason": "Attached",
             "message": "%s attached to %s" % (device, node)}], "data": {"serial": "0x0", "host": node}}]
    return cl


def c_name(cl):
    return cl["metadata"]["name"]


def static_claim(a, ctx):
    cl = wizard.claim_doc(a, ctx)
    cl["metadata"].update(namespace=NS, uid="5a7e%04x-0000-4000-8000-000000000000" % (zlib.crc32(a["name"].encode()) % 0x10000),
                          creationTimestamp="2026-11-03T12:00:00Z")
    return cl


def companion(a, ctx, lease):
    """The companion claim of the lease of the answers A, as the broker makes it."""
    reqs = wizard.requests_of(a, ctx)
    s, driver, req = reqs[0]
    params = wizard.memory_config(a)
    params["device"].pop("path", None)
    cl = {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceClaim",
          "metadata": {"name": "cxl-lease-" + lease, "namespace": NS, "uid": "c1a1%s-0000-4000-8000-000000000000" % lease[:4],
                       "creationTimestamp": "2026-11-03T12:01:00Z",
                       "labels": {"cxl.nri.io/lease": lease, "cxl.nri.io/requester-uid": ms.uid(a["name"])},
                       "annotations": {"cxl.nri.io/requester": "%s/%s/main" % (NS, a["name"])}},
          "spec": {"devices": {"requests": [{"name": "lease", "exactly": dict(req, allocationMode="ExactCount", count=1)}],
                               "config": [{"opaque": {"driver": driver, "parameters": params}}]}},
          "status": {}}
    return cl


def lease_claim(name, node):
    cl = ms.lease_claim(name, node)
    cl["metadata"]["name"] = name + "-lease-x7k2p"
    cl["metadata"]["labels"] = wizard.labels(name)
    return cl


def companion_pod(name, node, lease, phase="Running"):
    return ms.pod("cxl-lease-" + lease, node, phase, lease, name, scheduled=phase == "Running")


def answers(ctx, **given):
    return wizard.resolve({k: v for k, v in given.items()}, ctx)


def scenario_w8(ctx0):
    """Return (vm2, vm1, pool) of eight objects of the wizard."""
    a1 = answers(ctx0, kind="claim", source="local", memory="volatile", share="no", mode="numa", size="256Mi",
                 serial=VM2_LOCAL, node="vm2", command=("env",), name="w1")
    a2 = answers(ctx0, kind="claim", source="pool", memory="volatile", share="no", mode="dax", size="whole",
                 serial=SHARED0, node="vm1", command=("write", "Hello from w2"), name="w2")
    a3 = answers(ctx0, kind="lease", source="pool", memory="volatile", share="yes", mode="dax", serial=SHARED0,
                 rw="read-only", node="vm2", command=("read",), name="w3")
    a4 = answers(ctx0, kind="claim", source="pool", memory="persistent", share="yes", mode="fs", serial=FS0,
                 rw="read-write", fs_type="ext4", node="vm1", command=("write", "Hello from w4"), name="w4")
    a5 = answers(ctx0, kind="claim", source="pool", memory="persistent", share="yes", mode="fs", serial=FS0,
                 node="vm2", command=("read",), name="w5")
    a6 = answers(ctx0, kind="claim", source="local", memory="persistent", share="no", mode="block", serial=LP,
                 rw="read-write", node="vm2", command=("read",), name="w6")
    a7 = answers(ctx0, kind="claim", source="pool", memory="volatile", share="no", mode="numa", size="1Gi",
                 node="any", command=("env",), name="w7")
    a8 = answers(ctx0, kind="lease", source="local", memory="volatile", share="no", mode="dax", size="128Mi",
                 rw="read-only", node="vm1", command=("read",), name="w8")
    c1 = allocate(static_claim(a1, ctx0), VM2, "cxl.nri.io", VM2, "memdev-c100e2e1", {"memory": "256Mi"})
    c2 = allocate(static_claim(a2, ctx0), VM1, "cxl-pool.nri.io", "fake-cxl-pool", "shared0", {"shares": "1M"})
    c3 = companion(a3, ctx0, "3a3a0003")
    c4 = allocate(static_claim(a4, ctx0), VM1, "cxl-pool.nri.io", "fake-cxl-pool", "fs0", {"shares": "1"})
    c5 = allocate(static_claim(a5, ctx0), VM2, "cxl-pool.nri.io", "fake-cxl-pool", "fs0", {"shares": "1"})
    c5["status"]["devices"] = [{"driver": "cxl-pool.nri.io", "pool": "fake-cxl-pool", "device": "fs0", "conditions": [
        {"type": "cxl-pool.nri.io/AttachFailed", "status": "True", "reason": "Conflict",
         "message": 'attach fs0 to %s: Conflict: device "fs0" has a single-host attachment to "%s"' % (VM2, VM1)}]}]
    c6 = allocate(static_claim(a6, ctx0), VM2, "cxl.nri.io", VM2, "memdev-c100f5e1", {"shares": "1M"})
    c7 = static_claim(a7, ctx0)
    c8 = allocate(companion(a8, ctx0, "8b8b0008"), VM1, "cxl.nri.io", VM1, "memdev-c100e2e1", {"memory": "128Mi"}, request="lease")
    c8["status"]["reservedFor"] = [{"resource": "pods", "name": "cxl-lease-8b8b0008"}]
    pods = [pod("w1", VM2), pod("w2", VM1), pod("w3", VM2, lease=True), pod("w4", VM1),
            pod("w5", VM2, phase="Pending"), pod("w6", VM2), pod("w7", None, phase="Pending", unschedulable=True),
            pod("w8", VM1, lease=True),
            companion_pod("w3", VM2, "3a3a0003", "Pending"), companion_pod("w8", VM1, "8b8b0008")]
    claims = [c1, c2, c3, c4, c5, c6, c7, c8, lease_claim("w3", VM2), lease_claim("w8", VM1)]
    vm2_node = {"hostname": VM2, "memdevs": [ms.memdev("mem0", VM2_LOCAL, GI), ms.memdev("mem1", LP, 512 * MI)],
                "regions": [{"name": "region0", "mode": "ram", "size": GI, "memdevs": ["mem0"],
                             "dax": [{"name": "dax0.0", "size": 256 * MI, "driver": "kmem", "targetNode": 2}]},
                            dict(ms.pmem_region("region1", "mem1", 512 * MI, "ext4", ro=False), name="region1")],
                "mappers": [], "openers": [{"pid": 4242, "comm": "python3", "podUID": ms.uid("w6"), "block": "pmem0", "perms": "rw"}],
                "mounts": []}
    fs0_mount = {"source": "/dev/pmem0", "block": "pmem0", "loop": False, "dax": True, "kind": "node", "fstype": "ext4",
                 "ro": False, "mountRO": False, "target": "/var/lib/kubelet/plugins/cxl-pool.nri.io/mounts/c1ae0002-direct"}
    vm1_node = {"hostname": VM1, "memdevs": [ms.memdev("mem0", VM1_LOCAL, 256 * MI), ms.memdev("mem1", SHARED0, 256 * MI),
                                             ms.memdev("mem2", FS0, 512 * MI)],
                "regions": [ms.region("region0", "mem0", 256 * MI, "dax0.0"), ms.region("region1", "mem1", 256 * MI, "dax1.0"),
                            ms.pmem_region("region2", "mem2", 512 * MI, "ext4", ro=False)],
                "mappers": [ms.mapper("w8", "dax0.0", False, 5008), ms.mapper("w2", "dax1.0", True, 5002)],
                "openers": [], "mounts": [fs0_mount]}
    vm1_node["regions"][0]["dax"][0]["size"] = 128 * MI
    vm2 = {"k8s": k8s(pods, claims), "node": vm2_node, "errors": []}
    vm1 = {"node": vm1_node, "errors": []}
    owner = "k8s:resourceclaim/%s"
    pool = pool_json([("pooled0", POOLED, 512 * MI, False, False, [], "free", None),
                      ("shared0", SHARED0, 256 * MI, True, False, [(VM1, True)], "allocated", owner % c2["metadata"]["uid"]),
                      ("fs0", FS0, 512 * MI, True, True, [(VM1, True)], "allocated", None)],
                     [(VM2, False)], {"fs0": "ext4 needs recovery", "lp0": "ext4 needs recovery"})
    return vm2, vm1, pool


def pool_json(devs, lp_atts, fs):
    out = []
    for name, serial, size, shared, persistent, atts, state, owner in devs:
        d = {"name": name, "serial": serial, "size": size, "shared": shared, "persistent": persistent, "state": state,
             "scope": "pool", "static": True, "path": "/tmp/fake-cxl-pool/%s.%s" % (name, "raw" if persistent else "mem"),
             "attachments": [{"device": name, "host": h, "state": "attached", "singleHost": ex} for h, ex in atts],
             "allocation": {"owner": owner} if owner else None}
        if persistent:
            d["lsaPath"] = "/tmp/fake-cxl-pool/%s.lsa" % name
        out.append(d)
    lp = {"name": "lp0", "serial": LP, "size": 512 * MI, "shared": False, "persistent": True, "state": "allocated",
          "scope": "pool", "static": True, "path": "/tmp/fake-cxl-pool-local-pmem/lp0.raw",
          "lsaPath": "/tmp/fake-cxl-pool-local-pmem/lp0.lsa",
          "attachments": [{"device": "lp0", "host": h, "state": "attached", "singleHost": ex} for h, ex in lp_atts],
          "allocation": {"owner": "demo:wizard"}}
    return {"devices": out, "lp": lp, "fs": fs}


def scenario_w0():
    vm2 = {"k8s": k8s([], []), "node": {"hostname": VM2, "memdevs": [ms.memdev("mem0", VM2_LOCAL, GI), ms.memdev("mem1", LP, 512 * MI)],
                                        "regions": [], "mappers": [], "openers": [], "mounts": []}, "errors": []}
    vm1 = {"node": {"hostname": VM1, "memdevs": [ms.memdev("mem0", VM1_LOCAL, 256 * MI)], "regions": [], "mappers": [],
                    "openers": [], "mounts": []}, "errors": []}
    pool = pool_json([("pooled0", POOLED, 512 * MI, False, False, [], "free", None),
                      ("shared0", SHARED0, 256 * MI, True, False, [], "free", None),
                      ("fs0", FS0, 512 * MI, True, True, [], "free", None)],
                     [(VM2, False)], {"fs0": "ext4 clean", "lp0": "ext4 clean"})
    return vm2, vm1, pool


def wizard_trace(sdir, t0=1792700000.0):
    """Write the streams of dra-trace-start of w1 (a local slice), w4 (fs0 read-write) and w5 (Conflict) to SDIR/trace."""
    tdir = os.path.join(sdir, "trace")
    os.makedirs(tdir, exist_ok=True)
    streams = {}

    def at(dt, tag, text):
        streams.setdefault(tag, []).append("%.6f %s %s" % (t0 + dt, tag, text))

    def klog(dt, tag, src, msg):
        at(dt, tag, "I1103 12:%02d:%02d.000000  4242 %s] %s" % (int(dt) // 60, int(dt) % 60, src, msg))

    def ev(dt, kind, name, reason, msg):
        at(dt, "vm.events", "%s   %s   %s   %s   %s" % (NS, kind, name, reason, msg))

    def cl(dt, event, name, cuid, alloc="-", node="-"):
        at(dt, "vm.claims", "%s claim %s/%s uid=%s allocated=%s node=%s reservedFor=- status=-" % (event, NS, name, cuid, alloc, node))

    def ctrl(dt, text):
        at(dt, "vm.controller", "fake-cxl-pool-controller: 2026/11/03 12:00:%02d.000000 %s" % (int(dt) % 60, text))

    cl(1, "ADDED", "w1", "5a7e0001")
    cl(1.2, "MODIFIED", "w1", "5a7e0001", "cxl.nri.io/%s/memdev-c100e2e1" % VM2, VM2)
    ev(1.3, "Pod", "w1", "Scheduled", "Successfully assigned %s/w1 to %s" % (NS, VM2))
    klog(2, "vm.driver", "local.go:779", "engine: local memdev %s (mem0): created region0 (devdax, dax dax0.0, 268435456 bytes) in 1.0s" % VM2_LOCAL)
    klog(2.5, "vm.driver", "draplugin.go:300", "cxl.nri.io: claim %s/w1: prepared memdev-c100e2e1 (serial %s, mode numa, read-only false) as mem0 region0 dax dax0.0 node 2" % (NS, VM2_LOCAL))
    cl(10, "ADDED", "w4", "5a7e0004")
    cl(10.2, "MODIFIED", "w4", "5a7e0004", "cxl-pool.nri.io/fake-cxl-pool/fs0", VM1)
    ev(10.3, "Pod", "w4", "BindingConditionsPending", "waiting for binding conditions")
    ctrl(11, "attach fs0 to %s for claim %s/w4 device fs0 node %s: single host for a read-write ext4 claim" % (VM1, NS, VM1))
    ctrl(12, "attached fs0 to %s (node %s, slot cxlsw_ds0_usrp0hb1, qemu device fcp_fs0.hp1, owner k8s:resourceclaim/5a7e0004-0000-4000-8000-000000000000, single host true) for %s/w4" % (VM1, VM1, NS))
    klog(15, "vm1.driver", "persistent.go:573", "engine: pool memdev %s: mounted /dev/pmem0 at /var/lib/kubelet/plugins/cxl-pool.nri.io/mounts/c1ae0002-direct (ext4, rw, dax=always)" % FS0)
    cl(20, "ADDED", "w5", "5a7e0005")
    cl(20.2, "MODIFIED", "w5", "5a7e0005", "cxl-pool.nri.io/fake-cxl-pool/fs0", VM2)
    ctrl(21, 'ERROR: Conflict for claim %s/w5 device fs0 node %s: attach fs0 to %s: Conflict: device "fs0" has a single-host attachment to "%s"' % (NS, VM2, VM2, VM1))
    ev(21.2, "ResourceClaim", "w5", "AttachFailed", 'attach fs0 to %s: Conflict: device "fs0" has a single-host attachment to "%s"' % (VM2, VM1))
    ev(30, "Pod", "w7", "FailedScheduling", "0/3 nodes are available: 3 cannot allocate all claims.")
    for tag, lines in streams.items():
        with open(os.path.join(tdir, tag + ".log"), "w") as f:
            f.write("\n".join(sorted(lines, key=lambda l: float(l.split(" ", 1)[0]))) + "\n")


def write_scenario(base, name, vm2, vm1, pool):
    sdir = os.path.join(base, name)
    os.makedirs(sdir, exist_ok=True)
    with open(os.path.join(sdir, "state.env"), "w") as f:
        f.write(state_text())
    ms.write(sdir, "vm2.json", vm2)
    ms.write(sdir, "vm1.json", vm1)
    ms.write(sdir, "pool.json", pool)


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: %s DIR" % os.path.basename(sys.argv[0]))
    base = sys.argv[1]
    vm2, vm1, pool = scenario_w0()
    write_scenario(base, "wizard-W0", vm2, vm1, pool)
    state = common.load_state(os.path.join(base, "wizard-W0", "state.env"))
    ctx0 = wizard.Context(state, vm2["k8s"])
    write_scenario(base, "wizard-W8", *scenario_w8(ctx0))
    tdir = os.path.join(base, "trace-wizard")
    os.makedirs(tdir, exist_ok=True)
    with open(os.path.join(tdir, "state.env"), "w") as f:
        f.write(state_text())
    wizard_trace(tdir)
    print("wizard samples written to %s" % base)


if __name__ == "__main__":
    main()
