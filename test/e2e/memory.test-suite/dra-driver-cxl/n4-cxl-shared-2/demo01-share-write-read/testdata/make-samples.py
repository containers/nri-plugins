#!/usr/bin/env python3
"""Write the generated samples of the views: DIR/<scenario>/.

Usage: make-samples.py DIR (bin/common.py testdata() runs it)

Each scenario is the state of one step of a demo script: state.env (as
demo setup writes it), vm2.json and vm1.json (what probe.py prints in the
VMs: Kubernetes objects, CXL in the kernel, the bytes), pool.json (the
pool server's device and the first bytes of its backing file, or in
pool-shared-fs the file system in it as common.erofs_files reads the real
image hello.erofs.hex). trace-local and trace-fs are generated trace
streams of the local and the pool-shared-fs scripts, with the log lines of
the current driver and controller (cmd/plugins/dra-driver-cxl,
scripts/testing/fake-cxl-pool/pkg/controller). The object shapes follow
the current driver: companion claims with one MemoryConfig
(cxl.nri.io/v1alpha1) of the lease's device section, capacity shares 1M
without a requestPolicy, class cxl-local-shared, a reader's companion
claim with shares "1"; the pool slice with a nodeSelector of its hosts.
"""
import json
import os
import shutil
import sys
import zlib

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "bin"))
import common   # noqa: E402

VM1 = "n4-cxl-shared-1-fedora-43-containerd"
VM2 = "n4-cxl-shared-2-fedora-43-containerd"
NS = "cxl-demo"
LOCAL = "0xc100e2e1"
SHARED = "0xc1ae0001"
GI, MI = 1 << 30, 1 << 20
UIDS = {"writer": "30c961ce-314b-4317-b08f-af695a53d3f2", "reader-a": "0644829a-2838-4cb6-9708-f7ff0d84a9dc",
        "reader-b": "a64408de-46c9-4958-9cea-7de90e55ddd5", "reader-c": "7d1c55e0-1a2b-4c3d-8e9f-0a1b2c3d4e5f"}
LEASES = {"writer": "3f2a91c0", "reader-a": "8c1d02e4", "reader-b": "51aa7b09", "reader-c": "e0c4d3f1"}


def uid(name):
    return UIDS.get(name, "%08x-0000-4000-8000-000000000000" % zlib.crc32(name.encode()))


def pod(name, node, phase="Running", lease=None, requester=None, deleting=False, scheduled=True, claim=None):
    p = {"apiVersion": "v1", "kind": "Pod",
         "metadata": {"name": name, "namespace": NS, "uid": uid(name), "labels": {}, "annotations": {}},
         "spec": {"nodeName": node if scheduled else None},
         "status": {"phase": phase}}
    if claim:
        p["status"]["resourceClaimStatuses"] = [{"name": "cxl", "resourceClaimName": claim}]
    if lease:
        p["metadata"]["labels"] = {"cxl.nri.io/lease": lease, "cxl.nri.io/requester-uid": uid(requester),
                                   "cxl.nri.io/node": node}
        p["metadata"]["annotations"] = {"cxl.nri.io/requester": "%s/%s/main" % (NS, requester)}
        if not scheduled:
            p["status"]["conditions"] = [{"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
                                          "message": "0/2 nodes are available: 1 cannot allocate all claims, 1 node(s) didn't satisfy plugin(s) [NodeAffinity]."}]
    if deleting:
        p["metadata"]["deletionTimestamp"] = "2026-10-20T12:01:30Z"
    return p


def companion(requester, node, serial, source, shared, ro, allocated, deleting=False, device=None, mode="dax", access=None):
    """The companion claim of a lease of REQUESTER, as the broker makes it (companionRequest of broker.go)."""
    lease = LEASES[requester]
    driver = "cxl.nri.io" if source == "local" else "cxl-pool.nri.io"
    cls = "cxl-local-shared" if source == "local" else "cxl-pool-shared"
    if mode in ("block", "fs"):
        cls = "cxl-pool-persistent"
    exactly = {"allocationMode": "ExactCount", "count": 1, "deviceClassName": cls,
               "selectors": [{"cel": {"expression": 'device.attributes["%s"].serial == "%s"' % (driver, serial)}}]}
    if shared:
        exactly["capacity"] = {"requests": {"shares": "1"}}
    dev = {"mode": mode}
    if mode == "fs":
        # readOnly explicitly, access only when the lease asks for it
        dev["readOnly"] = bool(ro)
        if access:
            dev["access"] = access
    elif ro:
        dev["readOnly"] = True
    params = {"apiVersion": "cxl.nri.io/v1alpha1", "kind": "MemoryConfig", "device": dev}
    cl = {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceClaim",
          "metadata": {"name": "cxl-lease-" + lease, "namespace": NS, "uid": "c1a1%04x-0000-4000-8000-000000000000" % (zlib.crc32(requester.encode()) & 0xffff),
                       "creationTimestamp": "2026-10-20T12:00:%02dZ" % (10 + len(requester)),
                       "labels": {"cxl.nri.io/lease": lease, "cxl.nri.io/requester-uid": uid(requester), "cxl.nri.io/node": node},
                       "annotations": {"cxl.nri.io/requester": "%s/%s/main" % (NS, requester)}},
          "spec": {"devices": {"requests": [{"name": "lease", "exactly": exactly}],
                               "config": [{"opaque": {"driver": driver, "parameters": params}}]}},
          "status": {}}
    if allocated:
        pool = node if source == "local" else "fake-cxl-pool"
        cl["status"]["allocation"] = {
            "devices": {"results": [{"request": "lease", "driver": driver, "pool": pool, "device": device,
                                     "consumedCapacity": {"shares": "1" if shared else "1M"}}],
                        "config": [{"source": "FromClaim", "opaque": {"driver": driver, "parameters": params}}]},
            "nodeSelector": {"nodeSelectorTerms": [{"matchFields": [{"key": "metadata.name", "operator": "In", "values": [node]}]}]}}
        cl["status"]["reservedFor"] = [{"resource": "pods", "name": "cxl-lease-" + lease}]
    if deleting:
        cl["metadata"]["deletionTimestamp"] = "2026-10-20T12:01:30Z"
    return cl


def with_attached(cl):
    """Return the pool companion claim CL with the Attached condition of its result, as the controller writes it."""
    r = cl["status"]["allocation"]["devices"]["results"][0]
    cl["status"]["devices"] = [{"driver": r["driver"], "pool": r["pool"], "device": r["device"],
                                "conditions": [{"type": "cxl-pool.nri.io/Attached", "status": "True", "reason": "Attached",
                                                "message": "%s attached" % r["device"]}],
                                "data": {"serial": SHARED, "shared": True}}]
    return cl


def local_slice(node, memdev=True):
    devs = [{"name": "lease", "attributes": {"type": {"string": "lease"}}, "allowMultipleAllocations": True},
            {"name": "dram", "attributes": {"type": {"string": "dram"}, "source": {"string": "local"}, "size": {"int": 8362471424}},
             "capacity": {"memory": {"value": "8166476Ki"}}}]
    if memdev:
        devs.insert(0, {"name": "memdev-c100e2e1", "allowMultipleAllocations": True,
                        "attributes": {"type": {"string": "memdev"}, "source": {"string": "local"}, "serial": {"string": LOCAL},
                                       "shared": {"bool": True}, "persistent": {"bool": False}, "size": {"int": GI},
                                       "resource.kubernetes.io/numaNode": {"int": 0}},
                        "capacity": {"shares": {"value": "1M"}}})
    return {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceSlice",
            "metadata": {"name": node + "-cxl.nri.io-x"},
            "spec": {"driver": "cxl.nri.io", "nodeName": node, "pool": {"name": node, "generation": 1, "resourceSliceCount": 1},
                     "devices": devs}}


def pool_slice(with_shared0=True, persistent=False):
    devs = []
    if with_shared0:
        devs.append({"name": "shared0", "allowMultipleAllocations": True,
                     "attributes": {"type": {"string": "memdev"}, "source": {"string": "pool"}, "serial": {"string": SHARED},
                                    "shared": {"bool": True}, "persistent": {"bool": persistent}, "size": {"int": 256 * MI},
                                    "coherent": {"bool": False}},
                     "capacity": {"shares": {"value": "1M"}}, "bindsToNode": True,
                     "bindingConditions": ["cxl-pool.nri.io/Attached"], "bindingFailureConditions": ["cxl-pool.nri.io/AttachFailed"]})
    hosts = {"nodeSelectorTerms": [{"matchExpressions": [{"key": "kubernetes.io/hostname", "operator": "In", "values": [VM1, VM2]}]}]}
    return {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceSlice", "metadata": {"name": "fake-cxl-pool-x"},
            "spec": {"driver": "cxl-pool.nri.io", "nodeSelector": hosts, "pool": {"name": "fake-cxl-pool", "generation": 1, "resourceSliceCount": 1},
                     "devices": devs}}


def drivers(vm2="ok", vm1="ok"):
    out = []
    for node, st in ((VM2, vm2), (VM1, vm1)):
        if st == "down":
            continue
        p = {"kind": "Pod", "metadata": {"name": "nri-dra-driver-cxl-" + node[14], "namespace": "kube-system"},
             "spec": {"nodeName": node},
             "status": {"phase": "Running", "containerStatuses": [{"ready": st == "ok", "restartCount": 1 if st == "restarting" else 0}]}}
        out.append(p)
    return out


def lst(objs):
    return {"apiVersion": "v1", "kind": "List", "items": objs}


def memdev(name, serial, size, enabled=True):
    return {"name": name, "serial": serial, "size": size, "numaNode": 0, "enabled": enabled}


def region(name, mem, size, dax=None):
    return {"name": name, "mode": "ram", "size": size, "memdevs": [mem],
            "dax": [{"name": dax, "size": size, "driver": "device_dax", "targetNode": 2}] if dax else []}


def pmem_region(name, mem, size, fs, ro=False):
    """A pmem region with its fsdax namespace and block device pmem0, FS "erofs" or "none", read-only if RO."""
    return {"name": name, "mode": "pmem", "size": size, "memdevs": [mem], "dax": [],
            "namespaces": [{"name": "namespace0.0", "mode": "fsdax", "size": size - 2 * MI, "block": "pmem0",
                            "major": 259, "minor": 0, "fs": fs, "ro": ro}]}


def lease_claim(pod_name, node):
    """The lease claim of the demo pod POD_NAME (template cxl-lease), allocated to the lease device of NODE."""
    return {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceClaim",
            "metadata": {"name": pod_name + "-cxl-7hhht", "namespace": NS, "uid": "1ea5e000-0000-4000-8000-%012x" % zlib.crc32(pod_name.encode()),
                         "creationTimestamp": "2026-10-22T12:00:00Z"},
            "spec": {"devices": {"requests": [{"name": "lease", "exactly": {"deviceClassName": "cxl-lease"}}]}},
            "status": {"allocation": {"devices": {"results": [{"request": "lease", "driver": "cxl.nri.io", "pool": node, "device": "lease"}]}},
                       "reservedFor": [{"resource": "pods", "name": pod_name, "uid": uid(pod_name)}]}}


def fs_mount(kind, pod_name=None, loop=False):
    """An erofs mount of pmem0 as probe.py reports it: the node mount or the lease bind of POD_NAME."""
    m = {"source": "/dev/loop0" if loop else "/dev/pmem0", "block": "pmem0", "loop": loop, "dax": not loop, "kind": kind,
         "fstype": "erofs", "ro": True, "mountRO": True}
    if kind == "node":
        m["target"] = "/var/lib/kubelet/plugins/cxl-pool.nri.io/mounts/c1ae0001-%s" % ("cached" if loop else "direct")
    else:
        cuid = lease_claim(pod_name, VM1)["metadata"]["uid"]
        m.update(target="/var/lib/kubelet/plugins/cxl.nri.io/lease-fs/%s/hello" % cuid, claimUID=cuid, name="hello")
    return m


def mapper(pod_name, dax, rw, pid):
    return {"pid": pid, "comm": "python3", "podUID": uid(pod_name), "dax": dax, "perms": "rw-s" if rw else "r--s"}


def text_hex(text):
    data = text.encode() + b"\0" if text else b""
    return (data + b"\0" * (256 - len(data))).hex()


def write(sdir, name, obj):
    with open(os.path.join(sdir, name), "w") as f:
        json.dump(obj, f, indent=1, sort_keys=True)
        f.write("\n")


def state(case, serial):
    return "\n".join([
        "# generated state of demo setup %s (make-samples.py)" % case,
        "DEMO_CASE=" + case, "VM2_DIR=/nonexistent/n4-cxl-shared-2-fedora-43-containerd",
        "VM1_DIR=/nonexistent/n4-cxl-shared-1-fedora-43-containerd", "VM2_NODE=" + VM2, "VM1_NODE=" + VM1,
        "NS=" + NS, "SERIAL=" + serial, "POOL_DEVICE=shared0", "POOL_URL=http://127.0.0.1:9909",
        "TRACE_DIR=trace", ""])


TEXT = "Hello from the writer, 12:01:12"
READERS = ["reader-a", "reader-b", "reader-c"]


def local_scenarios():
    """Return {name: (vm2, vm1)} of the local demo, steps L1-L9."""
    out = {}
    slices = [local_slice(VM2), local_slice(VM1, memdev=False), pool_slice(False)]
    no_region = {"hostname": VM2, "memdevs": [memdev("mem0", LOCAL, GI)], "regions": [], "mappers": []}
    with_region = dict(no_region, regions=[region("region0", "mem0", GI, "dax0.0")])
    vm1 = {"node": {"hostname": VM1, "memdevs": [], "regions": [], "mappers": []}}

    def vm2(pods, claims, node, text, drv=("ok", "ok")):
        b = {"memdev": "mem0", "error": "no dax device"} if not node["regions"] else \
            {"memdev": "mem0", "region": "region0", "dax": "dax0.0", "hex": text_hex(text)}
        return {"k8s": {"pods": lst(pods), "claims": lst(claims), "slices": lst(slices), "drivers": lst(drivers(*drv))},
                "node": node, "bytes": b, "errors": []}

    writer = pod("writer", VM2)
    w_lease = companion("writer", VM2, LOCAL, "local", False, False, True, device="memdev-c100e2e1")
    w_comp = pod("cxl-lease-" + LEASES["writer"], VM2, lease=LEASES["writer"], requester="writer")
    readers = [pod(r, VM2) for r in READERS]
    r_wait = [companion(r, VM2, LOCAL, "local", True, True, False) for r in READERS]
    r_wait_pods = [pod("cxl-lease-" + LEASES[r], VM2, "Pending", LEASES[r], r, scheduled=False) for r in READERS]
    r_have = [companion(r, VM2, LOCAL, "local", True, True, True, device="memdev-c100e2e1") for r in READERS]
    r_have_pods = [pod("cxl-lease-" + LEASES[r], VM2, lease=LEASES[r], requester=r) for r in READERS]
    out["local-L1"] = (vm2([writer], [], no_region, ""), vm1)
    out["local-L2"] = (vm2([writer, w_comp], [w_lease], with_region, ""), vm1)
    out["local-L3"] = (vm2([writer, w_comp], [w_lease], dict(with_region, mappers=[mapper("writer", "dax0.0", True, 4242)]), TEXT), vm1)
    out["local-L5"] = (vm2([writer, w_comp] + readers + r_wait_pods, [w_lease] + r_wait, with_region, TEXT), vm1)
    out["local-L6"] = (vm2([writer] + readers + r_have_pods, r_have, with_region, TEXT), vm1)
    held = dict(with_region, mappers=[mapper(r, "dax0.0", False, 5000 + i) for i, r in enumerate(READERS)])
    out["local-L8"] = (vm2([writer] + readers + r_have_pods, r_have, held, TEXT), vm1)
    out["local-L9"] = (vm2([], [], with_region, TEXT), vm1)
    many = ["reader-%s" % chr(ord("a") + i) for i in range(12)]
    for r in many:
        LEASES.setdefault(r, "%08x" % (0xa0000000 + ord(r[-1])))
    m_pods = [pod(r, VM2) for r in many]
    m_claims = [companion(r, VM2, LOCAL, "local", True, True, True, device="memdev-c100e2e1") for r in many]
    m_maps = dict(with_region, mappers=[mapper(r, "dax0.0", False, 6000 + i) for i, r in enumerate(many)])
    out["local-overflow"] = (vm2([writer] + m_pods, m_claims, m_maps, "x" * 80, drv=("restarting", "down")), vm1)
    return out


def pool_scenarios():
    """Return {name: (vm2, vm1, pool)} of the pool demo."""
    out = {}
    slices = [local_slice(VM2, memdev=False), local_slice(VM1, memdev=False), pool_slice(True)]
    empty = {"memdevs": [], "regions": [], "mappers": []}
    attached = {"memdevs": [memdev("mem0", SHARED, 256 * MI)], "regions": [region("region0", "mem0", 256 * MI, "dax0.0")], "mappers": []}

    def vm(name, pods, claims, node):
        d = {"node": dict(node, hostname=name), "errors": []}
        if pods is not None:
            d["k8s"] = {"pods": lst(pods), "claims": lst(claims), "slices": lst(slices), "drivers": lst(drivers())}
        return d

    def pool(atts, text, state="attached", owner=None):
        dev = {"name": "shared0", "serial": SHARED, "size": 256 * MI, "shared": True, "state": state, "scope": "pool",
               "path": "/tmp/fake-cxl-pool/shared0.mem", "static": True,
               "attachments": [{"device": "shared0", "host": h, "state": "attached", "singleHost": ex} for h, ex in atts],
               "allocation": {"owner": owner} if owner else None}
        return {"device": dev, "hex": text_hex(text)}

    writer = pod("writer", VM1)
    w_lease = with_attached(companion("writer", VM1, SHARED, "pool", False, False, True, device="shared0"))
    w_comp = pod("cxl-lease-" + LEASES["writer"], VM1, lease=LEASES["writer"], requester="writer")
    where = {"reader-a": VM2, "reader-b": VM2, "reader-c": VM1}
    readers = [pod(r, where[r]) for r in READERS]
    r_wait = [companion(r, where[r], SHARED, "pool", True, True, False) for r in READERS]
    r_wait_pods = [pod("cxl-lease-" + LEASES[r], where[r], "Pending", LEASES[r], r, scheduled=False) for r in READERS]
    r_have = [with_attached(companion(r, where[r], SHARED, "pool", True, True, True, device="shared0")) for r in READERS]
    # reader-a allocated, its attach to vm2 not done yet
    r_attaching = [companion("reader-a", VM2, SHARED, "pool", True, True, True, device="shared0")] + r_have[1:]
    r_have_pods = [pod("cxl-lease-" + LEASES[r], where[r], lease=LEASES[r], requester=r) for r in READERS]
    out["pool-L1"] = (vm(VM2, [writer], [], empty), vm(VM1, None, None, empty), pool([], "", "free"))
    out["pool-L3"] = (vm(VM2, [writer, w_comp], [w_lease], empty),
                      vm(VM1, None, None, dict(attached, mappers=[mapper("writer", "dax0.0", True, 4242)])),
                      pool([(VM1, True)], TEXT))
    out["pool-L5"] = (vm(VM2, [writer, w_comp] + readers + r_wait_pods, [w_lease] + r_wait, empty),
                      vm(VM1, None, None, attached), pool([(VM1, True)], TEXT))
    out["pool-L6-attaching"] = (vm(VM2, [writer] + readers + r_have_pods, r_attaching, empty),
                                vm(VM1, None, None, attached), pool([(VM1, False)], TEXT))
    out["pool-L6"] = (vm(VM2, [writer] + readers + r_have_pods, r_have, attached),
                      vm(VM1, None, None, attached), pool([(VM1, False), (VM2, False)], TEXT))
    held2 = dict(attached, mappers=[mapper("reader-a", "dax0.0", False, 5001), mapper("reader-b", "dax0.0", False, 5002)])
    held1 = dict(attached, mappers=[mapper("reader-c", "dax0.0", False, 5003)])
    out["pool-L8"] = (vm(VM2, [writer] + readers + r_have_pods, r_have, held2),
                      vm(VM1, None, None, held1), pool([(VM1, False), (VM2, False)], TEXT))
    out["pool-L9"] = (vm(VM2, [], [], empty), vm(VM1, None, None, empty), pool([], TEXT, "free"))
    # L9 at once: the drivers released (disabled) the memdevs, vm2's detach runs
    released = {"memdevs": [memdev("mem0", SHARED, 256 * MI, enabled=False)], "regions": [], "mappers": []}
    detaching = pool([(VM1, None), (VM2, None)], TEXT, "detaching")
    detaching["device"]["attachments"][1]["state"] = "detaching"
    out["pool-L9-release"] = (vm(VM2, [], [], released), vm(VM1, None, None, released), detaching)
    return out


def fs_scenarios():
    """Return {name: (vm2, vm1, pool)} of the pool-shared-fs demo, steps F2, F3, F5, F6, F9."""
    out = {}
    slices = [local_slice(VM2, memdev=False), local_slice(VM1, memdev=False), pool_slice(True, persistent=True)]
    empty = {"memdevs": [], "regions": [], "mappers": []}
    hello = common.erofs_files(common.read_hex(os.path.join(os.path.dirname(os.path.abspath(__file__)), "hello.erofs.hex")))
    zeros = {"type": "zeros", "files": [], "error": "", "super": {}}

    def pmem(fs, openers=(), mounts=(), ro=False):
        return {"memdevs": [memdev("mem0", SHARED, 256 * MI)], "regions": [pmem_region("region0", "mem0", 256 * MI, fs, ro)],
                "mappers": [], "openers": list(openers), "mounts": list(mounts)}

    def vm(name, pods, claims, node):
        d = {"node": dict(node, hostname=name), "errors": []}
        if pods is not None:
            d["k8s"] = {"pods": lst(pods), "claims": lst(claims), "slices": lst(slices), "drivers": lst(drivers())}
        return d

    def pool(atts, fs, state="attached", owner=None):
        dev = {"name": "shared0", "serial": SHARED, "size": 256 * MI, "shared": True, "persistent": True, "state": state,
               "scope": "pool", "path": "/tmp/fake-cxl-pool/shared0.raw", "lsaPath": "/tmp/fake-cxl-pool/shared0.lsa",
               "static": True,
               "attachments": [{"device": "shared0", "host": h, "state": "attached", "singleHost": ex} for h, ex in atts],
               "allocation": {"owner": owner} if owner else None}
        return {"device": dev, "fs": fs}

    where = {"writer": VM1, "reader-a": VM2, "reader-b": VM2, "reader-c": VM1}
    lclaims = [lease_claim(n, where[n]) for n in ["writer"] + READERS]
    writer = pod("writer", VM1, claim=lease_claim("writer", VM1)["metadata"]["name"])
    w_lease = with_attached(companion("writer", VM1, SHARED, "pool", False, False, True, device="shared0", mode="block"))
    w_comp = pod("cxl-lease-" + LEASES["writer"], VM1, lease=LEASES["writer"], requester="writer")
    readers = [pod(r, where[r], claim=lease_claim(r, where[r])["metadata"]["name"]) for r in READERS]
    r_wait = [companion(r, where[r], SHARED, "pool", True, True, False, mode="fs") for r in READERS]
    r_wait_pods = [pod("cxl-lease-" + LEASES[r], where[r], "Pending", LEASES[r], r, scheduled=False) for r in READERS]
    r_have = [with_attached(companion(r, where[r], SHARED, "pool", True, True, True, device="shared0", mode="fs")) for r in READERS]
    r_have_pods = [pod("cxl-lease-" + LEASES[r], where[r], lease=LEASES[r], requester=r) for r in READERS]
    owner = "k8s:resourceclaim/%s" % w_lease["metadata"]["uid"]
    opener = {"pid": 4242, "comm": "python3", "podUID": uid("writer"), "block": "pmem0", "perms": "rw"}
    out["pool-shared-fs-F2"] = (vm(VM2, [writer, w_comp], [w_lease] + lclaims[:1], empty),
                                vm(VM1, None, None, pmem("none", [opener])), pool([(VM1, True)], zeros, "allocated", owner))
    out["pool-shared-fs-F3"] = (vm(VM2, [writer, w_comp], [w_lease] + lclaims[:1], empty),
                                vm(VM1, None, None, pmem("erofs", [opener])), pool([(VM1, True)], hello, "allocated", owner))
    out["pool-shared-fs-F5"] = (vm(VM2, [writer, w_comp] + readers + r_wait_pods, [w_lease] + r_wait + lclaims, empty),
                                vm(VM1, None, None, pmem("erofs")), pool([(VM1, True)], hello, "allocated", owner))
    vm2_mounts = [fs_mount("node"), fs_mount("lease", "reader-a"), fs_mount("lease", "reader-b")]
    vm1_mounts = [fs_mount("node"), fs_mount("lease", "reader-c")]
    out["pool-shared-fs-F6"] = (vm(VM2, [writer] + readers + r_have_pods, r_have + lclaims, pmem("erofs", mounts=vm2_mounts, ro=True)),
                                vm(VM1, None, None, pmem("erofs", mounts=vm1_mounts, ro=True)),
                                pool([(VM1, False), (VM2, False)], hello, "allocated"))
    out["pool-shared-fs-F9"] = (vm(VM2, [], [], empty), vm(VM1, None, None, empty), pool([], hello, "free"))
    return out


def fs_trace(sdir, t0=1792600000.0):
    """Write the streams of dra-trace-start of the pool-shared-fs script F1-F9 to SDIR/trace.

    Hand-made with the format strings of the current driver (broker.go,
    pool.go, persistent.go: leases, prepares, node mounts, read-only block
    devices, binds), controller (reconciler.go) and pool server, the udev
    lines of a real run, and the block udev stream <vm>.block.udev of demo
    setup.
    """
    tdir = os.path.join(sdir, "trace")
    os.makedirs(tdir, exist_ok=True)
    streams = {}
    where = {"writer": VM1, "reader-a": VM2, "reader-b": VM2, "reader-c": VM1}
    vmtag = {VM1: "vm1", VM2: "vm"}
    luid = {r: "1ea5e000-0000-4000-8000-%012x" % zlib.crc32(r.encode()) for r in where}
    node_mount = "/var/lib/kubelet/plugins/cxl-pool.nri.io/mounts/c1ae0001-direct"
    nsize = 256 * MI - 2 * MI

    def at(dt, tag, text):
        streams.setdefault(tag, []).append("%.6f %s %s" % (t0 + dt, tag, text))

    def klog(dt, node, src, msg, level="I"):
        at(dt, vmtag[node] + ".driver", "%s1022 12:%02d:%02d.000000  4242 %s] %s" % (level, int(dt) // 60, int(dt) % 60, src, msg))

    def ev(dt, kind, name, reason, msg):
        at(dt, "vm.events", "%s   %s   %s   %s   %s" % (NS, kind, name, reason, msg))

    def cl(dt, event, name, cuid, alloc="-", node="-", deleting=False):
        at(dt, "vm.claims", "%s claim %s/%s uid=%s allocated=%s node=%s reservedFor=- status=-%s" % (
            event, NS, name, cuid, alloc, node, " deleting" if deleting else ""))

    def udev(dt, node, action, devpath, subsys):
        tag = vmtag[node] + (".block.udev" if subsys == "block" else ".udev")
        at(dt, tag, "KERNEL[%.6f] %-8s %s (%s)" % (6000 + dt, action, devpath, subsys))

    def ctrl(dt, text):
        at(dt, "vm.controller", "fake-cxl-pool-controller: 2026/10/22 12:00:%02d.000000 %s" % (int(dt) % 60, text))

    def server(dt, text):
        at(dt, "server", "fake-cxl-pool: 2026/10/22 12:00:%02d.000000 %s" % (int(dt) % 60, text))

    memdev_path = "/devices/pci0000:0c/0000:0c:00.0/0000:0d:00.0/0000:0e:00.0/0000:0f:00.0/mem0"
    region_path = "/devices/platform/ACPI0017:00/root0/decoder0.0/region0"
    pmem_path = "/devices/platform/ACPI0017:00/root0/nvdimm-bridge0/ndbus0/region0/pfn0.1/block/pmem0"

    def attach(dt, node, lease, excl):
        cuid = "c1a1%04x-0000-4000-8000-000000000000" % len(lease)
        ctrl(dt, "attached shared0 to %s (node %s, slot cxlsw_ds0_usrp0hb0, qemu device fcp_shared0.hp1, owner k8s:resourceclaim/%s, exclusive %s) for %s/cxl-lease-%s"
             % (node, node, cuid, "true" if excl else "false", NS, lease))
        server(dt - 0.1, "attachment shared0@%s: attached" % node)
        udev(dt + 0.2, node, "add", memdev_path, "cxl")
        udev(dt + 0.3, node, "add", memdev_path + "/pmem0", "cxl")   # the memdev's nvdimm, not a block device
        udev(dt + 2.6, node, "add", region_path, "cxl")
        udev(dt + 2.9, node, "add", pmem_path, "block")

    def release_region(dt, node):
        udev(dt, node, "remove", pmem_path, "block")
        udev(dt + 0.1, node, "remove", region_path, "cxl")

    def detach(dt, node):
        # the driver disables the memdev, the server detaches it
        klog(dt - 0.6, node, "pool.go:1479", "engine: pool memdev %s (mem0): disabled" % SHARED)
        server(dt - 0.5, "attachment shared0@%s: detaching qemu device fcp_shared0.hp1" % node)
        ctrl(dt, "detached shared0 from %s (node %s, owner k8s:resourceclaim/c1a10006-0000-4000-8000-000000000000)" % (node, node))
        server(dt - 0.1, "attachment shared0@%s: detached (qemu device fcp_shared0.hp1)" % node)
        udev(dt + 0.1, node, "remove", memdev_path, "cxl")

    def request(dt, who, mode, shared, ro, name):
        klog(dt, where[who], "broker.go:729", 'lease broker: lease %s: %s/%s/main requests size "" serial "%s" (source "", mode %s, share %s, shared device true, read-only %s, access "", name "%s")'
             % (LEASES[who], NS, who, SHARED, mode, "true" if shared else "false", "true" if ro else "false", name))

    def ready(dt, who, mode):
        if mode == "block":
            what = 'read-only false, NUMA node -1, dax "", block "/dev/pmem0", access "", fs "", path ""'
        else:
            what = 'read-only true, NUMA node -1, dax "", block "", access "direct", fs "erofs", path "/run/cxl/fs/hello"'
        klog(dt, where[who], "broker.go:1646", "lease broker: lease %s: Ready (source pool, serial %s, mode %s, %s, %d bytes)"
             % (LEASES[who], SHARED, mode, what, nsize))

    def prepared(dt, who, mode):
        ro, access, fstype, mount = ("false", "", "", "") if mode == "block" else ("true", "direct", "erofs", node_mount)
        klog(dt, where[who], "pool.go:689", 'cxl-pool.nri.io: claim %s/cxl-lease-%s: prepared shared0 (memdev %s, share true, mode %s, read-only %s) as mem0 region0 namespace namespace1.0 block /dev/pmem0 access "%s" fs "%s" mount "%s" loop ""'
             % (NS, LEASES[who], SHARED, mode, ro, access, fstype, mount))

    def device_log(dt, node, src, text):
        klog(dt, node, src, "engine: pool memdev %s: %s" % (SHARED, text))

    def target(who):
        return "/var/lib/kubelet/plugins/cxl.nri.io/lease-fs/%s/hello" % luid[who]

    ev(1, "Pod", "writer", "Scheduled", "Successfully assigned %s/writer to %s" % (NS, VM1))
    cl(1, "ADDED", "writer-cxl-7hhht", "1ea5e001", "cxl.nri.io/%s/lease" % VM1, VM1)
    # F2: the writer's block lease
    t, w = 20, "cxl-lease-" + LEASES["writer"]
    request(t, "writer", "block", False, False, "")
    cl(t + 0.1, "ADDED", w, "c1a10006")
    cl(t + 0.2, "MODIFIED", w, "c1a10006", "cxl-pool.nri.io/fake-cxl-pool/shared0", VM1)
    ev(t + 0.3, "Pod", w, "Scheduled", "Successfully assigned %s/%s to %s" % (NS, w, VM1))
    ev(t + 0.4, "Pod", w, "BindingConditionsPending", "waiting for binding conditions")
    attach(t + 1, VM1, LEASES["writer"], True)
    prepared(t + 3, "writer", "block")
    ready(t + 3.2, "writer", "block")
    # F5: the readers ask and wait
    t = 60
    for i, r in enumerate(READERS):
        ev(t - 10 + i, "Pod", r, "Scheduled", "Successfully assigned %s/%s to %s" % (NS, r, where[r]))
        request(t + i, r, "fs", True, True, "hello")
        cl(t + i + 0.1, "ADDED", "cxl-lease-" + LEASES[r], "c1a1%04x" % (10 + i))
        ev(t + i + 0.3, "Pod", "cxl-lease-" + LEASES[r], "FailedScheduling",
           "0/3 nodes are available: 1 cannot allocate all claims, 2 node(s) didn't match Pod's node affinity/selector.")
    # F6: the writer releases, the readers get the file system
    t = 100
    klog(t, VM1, "broker.go:1189", "lease broker: lease %s: releasing (released by main, was Ready)" % LEASES["writer"])
    cl(t + 0.2, "MODIFIED", w, "c1a10006", "cxl-pool.nri.io/fake-cxl-pool/shared0", VM1, deleting=True)
    release_region(t + 0.8, VM1)
    udev(t + 0.95, VM1, "unbind", memdev_path, "cxl")
    klog(t + 0.96, VM1, "pool.go:1479", "engine: pool memdev %s (mem0): disabled" % SHARED)
    klog(t + 1, VM1, "pool.go:1521", "cxl-pool.nri.io: unprepared claim %s/%s (c1a10006-0000-4000-8000-000000000000)" % (NS, w))
    cl(t + 2.5, "DELETED", w, "c1a10006", "cxl-pool.nri.io/fake-cxl-pool/shared0", VM1, deleting=True)
    klog(t + 2.6, VM1, "broker.go:1258", "lease broker: lease %s: released" % LEASES["writer"])
    for i, r in enumerate(READERS):
        cl(t + 3 + i * 0.1, "MODIFIED", "cxl-lease-" + LEASES[r], "c1a1%04x" % (10 + i), "cxl-pool.nri.io/fake-cxl-pool/shared0", where[r])
    # shared0 stays on vm1, not exclusive any more; vm2 gets it too
    server(t + 3.5, "attachment shared0@%s: single host false" % VM1)
    ctrl(t + 3.9, "attach shared0 to %s for claim %s/cxl-lease-%s device shared0 node %s: already attached (attached, owner \"%s\", single host false)"
         % (VM1, NS, LEASES["reader-c"], VM1, "k8s:resourceclaim/c1a10006-0000-4000-8000-000000000000"))
    attach(t + 4, VM2, LEASES["reader-a"], False)
    udev(t + 6.5, VM1, "bind", memdev_path, "cxl")
    udev(t + 6.6, VM1, "add", region_path, "cxl")
    udev(t + 6.9, VM1, "add", pmem_path, "block")
    mounted = set()
    for i, r in enumerate(READERS):
        dt = t + 8 + i
        ev(dt, "Pod", "cxl-lease-" + LEASES[r], "Scheduled", "Successfully assigned %s/cxl-lease-%s to %s" % (NS, LEASES[r], where[r]))
        if where[r] not in mounted:
            # the first reader of a node: no writer, the block device read-only, the node mount
            mounted.add(where[r])
            device_log(dt + 0.05, where[r], "persistent.go:679", "/dev/pmem0 read-only")
            device_log(dt + 0.1, where[r], "persistent.go:573", "mounted /dev/pmem0 at %s (erofs, ro, dax=always)" % node_mount)
        prepared(dt + 0.2, r, "fs")
        klog(dt + 0.3, where[r], "persistent.go:1130", "engine: lease %s: bound %s at %s" % (LEASES[r], node_mount, target(r)))
        ready(dt + 0.4, r, "fs")
    # F9: everything goes away; reader-a still has a file open for a moment
    t = 160
    for i, r in enumerate(READERS):
        klog(t + i * 0.3, where[r], "broker.go:1189", "lease broker: lease %s: releasing (requester container gone, was Ready)" % LEASES[r])
    for k in range(2):
        klog(t + 1 + k, VM2, "draplugin.go:612", '"UnprepareResourceClaims failed" err="cxl-pool.nri.io: claim %s/cxl-lease-%s: lease %s: unmounting %s: device or resource busy"'
             % (NS, LEASES["reader-a"], LEASES["reader-a"], target("reader-a")), "E")
    for i, r in enumerate(READERS):
        dt = t + 4 + i * 0.5
        klog(dt, where[r], "persistent.go:1246", "engine: lease %s: unbound %s" % (LEASES[r], target(r)))
        if r in ("reader-b", "reader-c"):
            # the last reader of its node
            device_log(dt + 0.05, where[r], "persistent.go:820", "unmounted %s" % node_mount)
        klog(dt + 0.1, where[r], "pool.go:1521", "cxl-pool.nri.io: unprepared claim %s/cxl-lease-%s (c1a1%04x-0000-4000-8000-000000000000)" % (NS, LEASES[r], 10 + i))
        cl(dt + 0.2, "DELETED", "cxl-lease-" + LEASES[r], "c1a1%04x" % (10 + i), "cxl-pool.nri.io/fake-cxl-pool/shared0", where[r], deleting=True)
        klog(dt + 0.3, where[r], "broker.go:1258", "lease broker: lease %s: released" % LEASES[r])
    release_region(t + 6.4, VM2)
    release_region(t + 6.6, VM1)
    detach(t + 7, VM2)
    detach(t + 9, VM1)
    for tag, lines in streams.items():
        lines.sort()
        with open(os.path.join(tdir, tag + ".log"), "w") as f:
            f.write("\n".join(lines) + "\n")
    with open(os.path.join(sdir, "state.env"), "w") as f:
        f.write(state("pool-shared-fs", SHARED))


def local_trace(sdir, t0=1792500000.0):
    """Write the streams of dra-trace-start of the local script L1-L9 to SDIR/trace.

    Hand-made after the streams of a real run of test13, with the format
    strings of the current driver (broker.go, local.go) for the shared
    local device.
    """
    tdir = os.path.join(sdir, "trace")
    os.makedirs(tdir, exist_ok=True)
    streams = {}

    def at(dt, tag, text):
        streams.setdefault(tag, []).append("%.6f %s %s" % (t0 + dt, tag, text))

    def klog(dt, src, msg, level="I"):
        at(dt, "vm.driver", "%s1020 12:00:%02d.000000  4242 %s] %s" % (level, int(dt) % 60, src, msg))

    def ev(dt, kind, name, reason, msg):
        at(dt, "vm.events", "%s   %s   %s   %s   %s" % (NS, kind, name, reason, msg))

    def cl(dt, event, name, uid, alloc="-", node="-", deleting=False):
        at(dt, "vm.claims", "%s claim %s/%s uid=%s allocated=%s node=%s reservedFor=- status=-%s" % (
            event, NS, name, uid, alloc, node, " deleting" if deleting else ""))

    def udev(dt, action, path, subsys):
        at(dt, "vm.udev", "KERNEL[%.6f] %-8s %s (%s)" % (5000 + dt, action, path, subsys))

    region = "/devices/platform/ACPI0017:00/root0/decoder0.0/region0"
    ev(1, "Pod", "writer", "Scheduled", "Successfully assigned %s/writer to %s" % (NS, VM2))
    cl(1, "ADDED", "writer-cxl-7hhht", "3ab7b04e", "cxl.nri.io/%s/lease" % VM2, VM2)
    t = 20
    klog(t, "broker.go:729", 'lease broker: lease 3f2a91c0: %s/writer/main requests size "" serial "%s" (source "", mode dax, share false, shared device true, read-only false, access "", name "")' % (NS, LOCAL))
    cl(t, "ADDED", "cxl-lease-3f2a91c0", "c1a10006")
    cl(t + 0.1, "MODIFIED", "cxl-lease-3f2a91c0", "c1a10006", "cxl.nri.io/%s/memdev-c100e2e1" % VM2, VM2)
    ev(t + 0.2, "Pod", "cxl-lease-3f2a91c0", "Scheduled", "Successfully assigned %s/cxl-lease-3f2a91c0 to %s" % (NS, VM2))
    udev(t + 1, "add", region, "cxl")
    udev(t + 1.1, "add", region + "/dax_region0", "cxl")
    udev(t + 1.2, "add", region + "/dax_region0/dax0.0", "dax")
    klog(t + 1.25, "local.go:779", "engine: local memdev %s (mem0): created region0 (devdax, dax dax0.0, NUMA node 2) in 100ms" % LOCAL)
    klog(t + 1.3, "plugin.go:362", "cxl.nri.io: claim %s/cxl-lease-3f2a91c0: prepared memdev-c100e2e1 (memdev %s, share true, mode dax, read-only false) as mem0 region0 dax dax0.0 size %d NUMA node -1 exclusive" % (NS, LOCAL, GI))
    klog(t + 1.27, "local.go:643", "engine: local memdev %s (mem0): zeroed new shared dax0.0 size %d in 100ms" % (LOCAL, GI))
    klog(t + 1.5, "broker.go:1646", 'lease broker: lease 3f2a91c0: Ready (source local, serial %s, mode dax, read-only false, NUMA node -1, dax "/dev/dax0.0", block "", access "", fs "", path "", %d bytes)' % (LOCAL, GI))
    t = 60
    for i, r in enumerate(READERS):
        ev(t - 5 + i, "Pod", r, "Scheduled", "Successfully assigned %s/%s to %s" % (NS, r, VM2))
        klog(t + i, "broker.go:729", 'lease broker: lease %s: %s/%s/main requests size "" serial "%s" (source "", mode dax, share true, shared device true, read-only true, access "", name "")' % (LEASES[r], NS, r, LOCAL))
        cl(t + i + 0.1, "ADDED", "cxl-lease-" + LEASES[r], "c1a1%04x" % (10 + i))
        ev(t + i + 0.3, "Pod", "cxl-lease-" + LEASES[r], "FailedScheduling",
           "0/2 nodes are available: 1 cannot allocate all claims, 1 node(s) didn't match Pod's node affinity/selector. preemption: 0/2 nodes are available: 2 Preemption is not helpful for scheduling.")
    t = 100
    klog(t, "broker.go:1189", "lease broker: lease 3f2a91c0: releasing (released by main, was Ready)")
    cl(t + 0.2, "MODIFIED", "cxl-lease-3f2a91c0", "c1a10006", "cxl.nri.io/%s/memdev-c100e2e1" % VM2, VM2, deleting=True)
    cl(t + 1.5, "DELETED", "cxl-lease-3f2a91c0", "c1a10006", "cxl.nri.io/%s/memdev-c100e2e1" % VM2, VM2, deleting=True)
    klog(t + 1.4, "local.go:1341", "engine: local memdev %s (mem0): claim c1a10006-0000-4000-8000-000000000000: released shared dax0.0, claims left []" % LOCAL)
    klog(t + 1.45, "local.go:1346", "engine: local memdev %s (mem0): keeping region0 and dax0.0 of the idle shared device" % LOCAL)
    klog(t + 1.6, "broker.go:1258", "lease broker: lease 3f2a91c0: released")
    for i, r in enumerate(READERS):
        cl(t + 2 + i * 0.1, "MODIFIED", "cxl-lease-" + LEASES[r], "c1a1%04x" % (10 + i), "cxl.nri.io/%s/memdev-c100e2e1" % VM2, VM2)
        ev(t + 2.2 + i * 0.1, "Pod", "cxl-lease-" + LEASES[r], "Scheduled", "Successfully assigned %s/cxl-lease-%s to %s" % (NS, LEASES[r], VM2))
        klog(t + 3 + i * 0.2, "plugin.go:362", "cxl.nri.io: claim %s/cxl-lease-%s: prepared memdev-c100e2e1 (memdev %s, share true, mode dax, read-only true) as mem0 region0 dax dax0.0 size %d NUMA node -1 1 share" % (NS, LEASES[r], LOCAL, GI))
        klog(t + 3.1 + i * 0.2, "broker.go:1646", 'lease broker: lease %s: Ready (source local, serial %s, mode dax, read-only true, NUMA node -1, dax "/dev/dax0.0", block "", access "", fs "", path "", %d bytes)' % (LEASES[r], LOCAL, GI))
    t = 160
    for i, r in enumerate(READERS):
        klog(t + i * 0.3, "broker.go:1189", "lease broker: lease %s: releasing (requester container gone, was Ready)" % LEASES[r])
        cl(t + 1 + i * 0.3, "DELETED", "cxl-lease-" + LEASES[r], "c1a1%04x" % (10 + i), "cxl.nri.io/%s/memdev-c100e2e1" % VM2, VM2, deleting=True)
        klog(t + 1.2 + i * 0.3, "broker.go:1258", "lease broker: lease %s: released" % LEASES[r])
    for tag, lines in streams.items():
        lines.sort()
        with open(os.path.join(tdir, tag + ".log"), "w") as f:
            f.write("\n".join(lines) + "\n")
    with open(os.path.join(sdir, "state.env"), "w") as f:
        f.write(state("local", LOCAL))


# llm-share: the model Qwen3-0.6B on the persistent pool device llm0, its
# erofs mounted on all three nodes; the writer (a lease claim on vm1) put
# it there, reader-0..2 (one per node) serve it, the client asks them. M9:
# the readers map the model directly (dax); M14: again through the page
# cache (a loop device).
VM0 = "n4-cxl-shared-0-fedora-43-containerd"
LLM_FILE = "Qwen3-0.6B-Q4_K_M.gguf"
LLM_SIZE = 512 * MI
LLM_NODES = [VM0, VM1, VM2]   # of reader-0, reader-1, reader-2


def llm_state():
    """The state of demo setup llm-share."""
    return "\n".join([
        "# generated state of demo setup llm-share (make-samples.py)",
        "DEMO_CASE=llm-share", "DEMO_FSTYPE=",
        "VM2_DIR=/nonexistent/" + VM2, "VM1_DIR=/nonexistent/" + VM1, "VM0_DIR=/nonexistent/" + VM0,
        "VM2_NODE=" + VM2, "VM1_NODE=" + VM1, "VM0_NODE=" + VM0,
        "NS=" + NS, "SERIAL=" + SHARED, "POOL_DEVICE=llm0", "POOL_URL=http://127.0.0.1:9909",
        "DEMO_LLM_MODEL=qwen3-0.6b", "DEMO_LLM_ACCESS=direct", "DEMO_LLM_LATENCY=off",
        "LLM_MODEL_FILE=" + LLM_FILE, "LLM_DEVICE_SIZE=512Mi", ""])


def llm_pod(name, node, step, ref=None, claim=None):
    """A Running pod of llm-share with the claim CLAIM of its claim reference REF; its uid is of
    the step (the readers of M14 are new pods)."""
    p = {"apiVersion": "v1", "kind": "Pod",
         "metadata": {"name": name, "namespace": NS, "uid": uid("%s-%s" % (name, step)), "labels": {}, "annotations": {}},
         "spec": {"nodeName": node}, "status": {"phase": "Running"}}
    if claim:
        p["status"]["resourceClaimStatuses"] = [{"name": ref, "resourceClaimName": claim}]
    return p


def llm_reader_claim(reader, step, access):
    """The claim of READER of the model's file system on llm0: one share, read-only, ACCESS direct or cached."""
    name = "%s-model-%s" % (reader, step.lower())
    return {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceClaim",
            "metadata": {"name": name, "namespace": NS, "uid": uid(name), "annotations": {},
                         "ownerReferences": [{"kind": "Pod", "name": reader, "uid": uid("%s-%s" % (reader, step))}]},
            "spec": {"devices": {"requests": [{"name": "model", "exactly": {
                "deviceClassName": "cxl-pool-persistent", "allocationMode": "ExactCount", "count": 1, "capacity": {},
                "selectors": [{"cel": {"expression": 'device.attributes["cxl-pool.nri.io"].serial == "%s"' % SHARED}}]}}]}},
            "status": {"allocation": {"devices": {
                "config": [{"opaque": {"driver": "cxl-pool.nri.io", "parameters": {
                    "apiVersion": "cxl.nri.io/v1alpha1", "kind": "MemoryConfig",
                    "device": {"access": access, "mode": "fs", "readOnly": True}}}}],
                "results": [{"request": "model", "driver": "cxl-pool.nri.io", "pool": "fake-cxl-pool", "device": "llm0",
                             "consumedCapacity": {"shares": "1"}}]}},
                "devices": [{"driver": "cxl-pool.nri.io", "pool": "fake-cxl-pool", "device": "llm0",
                             "conditions": [{"type": "cxl-pool.nri.io/Attached", "status": "True"}]}],
                "reservedFor": [{"resource": "pods", "name": reader}]}}


def llm_writer_claim(step):
    """The lease claim of the writer on vm1."""
    name = "writer-cxl-llm"
    return {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceClaim",
            "metadata": {"name": name, "namespace": NS, "uid": uid(name),
                         "ownerReferences": [{"kind": "Pod", "name": "writer", "uid": uid("writer-%s" % step)}]},
            "spec": {"devices": {"requests": [{"name": "lease", "exactly": {
                "deviceClassName": "cxl-lease", "allocationMode": "ExactCount", "count": 1}}]}},
            "status": {"allocation": {"devices": {"results": [{"request": "lease", "driver": "cxl.nri.io", "pool": VM1,
                                                               "device": "lease"}]}}}}


def llm_node_slice(node):
    """The cxl.nri.io slice of NODE: its DRAM and the lease device."""
    return {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceSlice",
            "metadata": {"name": "cxl.nri.io-" + node},
            "spec": {"driver": "cxl.nri.io", "nodeName": node, "pool": {"name": node, "generation": 1, "resourceSliceCount": 1},
                     "devices": [{"name": "dram", "attributes": {"type": {"string": "dram"}, "source": {"string": "local"},
                                                                 "memoryNUMANodes": {"string": "0-1"}, "size": {"int": 8 * GI}}},
                                 {"name": "lease", "attributes": {"type": {"string": "lease"}}}]}}


def llm_pool_slice():
    """The cxl-pool.nri.io slice of llm0."""
    return {"apiVersion": "resource.k8s.io/v1", "kind": "ResourceSlice",
            "metadata": {"name": "cxl-pool.nri.io-fake-cxl-pool"},
            "spec": {"driver": "cxl-pool.nri.io", "pool": {"name": "fake-cxl-pool", "generation": 1, "resourceSliceCount": 1},
                     "devices": [{"name": "llm0", "attributes": {"serial": {"string": SHARED}, "shared": {"bool": True},
                                                                 "persistent": {"bool": True}, "size": {"int": LLM_SIZE}},
                                  "capacity": {"shares": {"value": "1M"}}}]}}


def llm_driver(i):
    """The driver pod of the node of reader-I, ready."""
    node = LLM_NODES[i]
    return {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "nri-dra-driver-cxl-vm%d" % i, "namespace": "kube-system"},
            "spec": {"nodeName": node}, "status": {"phase": "Running", "containerStatuses": [{"ready": True}]}}


def llm_node(reader, step, cached):
    """What probe.py prints on the node of READER: llm0 as mem0 region0 pmem0, its erofs mounted
    (directly, or through loop0 when CACHED) and the model file mapped by the reader."""
    return {"node": {
        "memdevs": [{"name": "mem0", "serial": SHARED, "size": LLM_SIZE}],
        "regions": [{"name": "region0", "mode": "pmem", "memdevs": ["mem0"], "namespaces": [{"block": "pmem0", "fs": "erofs"}]}],
        "mounts": [fs_mount("node", loop=cached)],
        "mappers": [{"podUID": uid("%s-%s" % (reader, step)), "path": "/models/" + LLM_FILE,
                     "source": "loop0" if cached else "pmem0"}]}}


def llm_pool():
    """The pool server's llm0 and the erofs of the model in it."""
    return {"device": {"name": "llm0", "path": "/dev/shm/fake-cxl-pool-llm/llm0.raw", "size": LLM_SIZE, "state": "attached",
                       "shared": True, "persistent": True,
                       "attachments": [{"host": n, "state": "attached"} for n in LLM_NODES]},
            "fileResident": 399077376,
            "fs": {"type": "erofs", "files": [{"name": LLM_FILE, "type": "file", "size": 396705472,
                                               "text": "GGUF....6....... ...............general.architec"}]}}


def llm_scenarios():
    """llm-share-M9 and llm-share-M14: vm2, vm1, vm0 and pool."""
    out = {}
    for step, access in (("M9", "direct"), ("M14", "cached")):
        readers = ["reader-%d" % i for i in range(3)]
        claims = [llm_reader_claim(r, step, access) for r in readers] + [llm_writer_claim(step)]
        pods = [llm_pod("client", VM2, step)] + \
            [llm_pod(r, LLM_NODES[i], step, "model", claims[i]["metadata"]["name"]) for i, r in enumerate(readers)] + \
            [llm_pod("writer", VM1, step, "cxl", claims[3]["metadata"]["name"])]
        k8s = {"pods": lst(pods), "claims": lst(claims), "drivers": lst([llm_driver(i) for i in range(3)]),
               "slices": lst([llm_pool_slice()] + [llm_node_slice(n) for n in LLM_NODES])}
        nodes = [llm_node(r, step, access == "cached") for r in readers]
        vm2 = dict(nodes[2], k8s=k8s)
        out["llm-share-" + step] = (vm2, nodes[1], nodes[0], llm_pool())
    return out


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: %s DIR" % os.path.basename(sys.argv[0]))
    base = sys.argv[1]
    for name, (v2, v1) in local_scenarios().items():
        sdir = os.path.join(base, name)
        os.makedirs(sdir, exist_ok=True)
        with open(os.path.join(sdir, "state.env"), "w") as f:
            f.write(state("local", LOCAL))
        write(sdir, "vm2.json", v2)
        write(sdir, "vm1.json", v1)
    for name, (v2, v1, pl) in pool_scenarios().items():
        sdir = os.path.join(base, name)
        os.makedirs(sdir, exist_ok=True)
        with open(os.path.join(sdir, "state.env"), "w") as f:
            f.write(state("pool", SHARED))
        write(sdir, "vm2.json", v2)
        write(sdir, "vm1.json", v1)
        write(sdir, "pool.json", pl)
    for name, (v2, v1, pl) in fs_scenarios().items():
        sdir = os.path.join(base, name)
        os.makedirs(sdir, exist_ok=True)
        with open(os.path.join(sdir, "state.env"), "w") as f:
            f.write(state("pool-shared-fs", SHARED))
        write(sdir, "vm2.json", v2)
        write(sdir, "vm1.json", v1)
        write(sdir, "pool.json", pl)
    for name, (v2, v1, v0, pl) in llm_scenarios().items():
        sdir = os.path.join(base, name)
        os.makedirs(sdir, exist_ok=True)
        with open(os.path.join(sdir, "state.env"), "w") as f:
            f.write(llm_state())
        write(sdir, "vm2.json", v2)
        write(sdir, "vm1.json", v1)
        write(sdir, "vm0.json", v0)
        write(sdir, "pool.json", pl)
    local_trace(os.path.join(base, "trace-local"))
    fs_trace(os.path.join(base, "trace-fs"))
    stale = os.path.join(base, "local-L5-stale")
    if os.path.isdir(stale):
        shutil.rmtree(stale)
    shutil.copytree(os.path.join(base, "local-L5"), stale)
    open(os.path.join(stale, "stale"), "w").close()
    print("samples written to %s" % base)


if __name__ == "__main__":
    main()
