#!/usr/bin/env python3
"""The live views of the demo, one per terminal of 50 x 10.

Usage:
  views.py VIEW [local|pool|pool-shared-fs|pool-fs|wizard|llm-share] [--color|--no-color] [--once] [--interval S]
  views.py VIEW --sample DIR [--now EPOCH] [--until EPOCH]

VIEW is t2 (pods and leases), t3 (scheduler: devices and claims), t4
(nodes: CXL in the kernel), t5 (trace), t6 (memory; the pool server in the
pool demos). The case defaults to DEMO_CASE of the state of demo setup.
A view refreshes once a second: a title with a clock and at most 9 lines,
cut to the terminal (50 x 10). A failed refresh keeps the last rendering
and marks the title "stale". t5 paints its title and last rows once, then
only the new lines below them, so that the terminal scrolls; the end of
the demo and a new trace are a line each, the cursor never goes back. --once prints
one rendering without color (captures).
--sample renders the files of a test data directory instead: state.env,
vm2.json, vm1.json and vm0.json (probe.py output), pool.json, trace/.
"""
import json
import os
import re
import sys
import threading
import time

import common as c
import fsinfo
import wizard_views as wv

CASES = ("local", "pool", "pool-shared-fs", "pool-fs", "wizard", "llm-share")


def pool_case(state):
    """Return True if the case of STATE is a pool demo (pool, pool-shared-fs, pool-fs, llm-share)."""
    return (state.get("DEMO_CASE") or "").startswith("pool") or state.get("DEMO_CASE") == "llm-share"


def demo_nodes(state):
    """Return the node names of the demo of STATE: vm2 and vm1, and vm0 in llm-share."""
    return [state.get(k) for k in ("VM2_NODE", "VM1_NODE", "VM0_NODE") if state.get(k)]


###
### T2 PODS & LEASES
###

def pod_state(p):
    if c.deleting(p):
        return "Exiting"
    phase = (p.get("status") or {}).get("phase") or "?"
    return {"Succeeded": "Done"}.get(phase, phase)[:7]


def demo_pods(pods_doc):
    """Return the pods of the demo without the companion pods of leases, writer first."""
    pods = [p for p in c.items(pods_doc, "Pod") if not c.lease_id(p)]
    return sorted(pods, key=lambda p: (c.meta(p).get("name") != "writer", c.meta(p).get("name")))


def leases_of(claims, pod):
    uid = c.meta(pod).get("uid")
    name = c.meta(pod).get("name")
    out = [cl for cl in claims if c.lease_id(cl) and
           (c.requester(cl) == name or c.labels(cl).get(c.REQUESTER_UID_LABEL) == uid)]
    return sorted(out, key=lambda cl: c.meta(cl).get("creationTimestamp") or "")


def owned_claims(claims, pod):
    """Return the claims of POD from its templates that are no lease device (class cxl-lease): a reader's claim of the model."""
    uid = c.meta(pod).get("uid")
    out = []
    for cl in claims:
        if c.lease_id(cl) or not any(o.get("uid") == uid for o in c.meta(cl).get("ownerReferences") or []):
            continue
        if any(r.get("deviceClassName") == "cxl-lease" for r in c.subrequests(cl)):
            continue
        out.append(cl)
    return out


def claim_memory(cl):
    """Return the MEMORY column of a claim: that of a lease, and the access of mode fs if it is there."""
    words = c.lease_memory(cl)
    dev = c.device_section(cl)
    if words.startswith("fs ") and dev.get("access"):
        words += " " + dev["access"]
    return words


def render_t2(data, state, now=None, stale=False):
    if state.get("DEMO_CASE") == "wizard":
        return wv.render_t2(data, state, now, stale)
    k8s = (data.get("vm2") or {}).get("k8s") or {}
    lines = [c.title("PODS & LEASES", stale, now), "POD       NODE STATE   LEASE    MEMORY"]
    claims = c.items(k8s.get("claims"), "ResourceClaim")
    fs_case = state.get("DEMO_CASE") == "pool-fs"
    rows = []
    for p in demo_pods(k8s.get("pods")):
        name = c.meta(p).get("name")
        node = c.short_node((p.get("spec") or {}).get("nodeName"), state)
        head = "%-9s %-4s %-7s " % (name[:9], node[:4], pod_state(p))
        leases = leases_of(claims, p)
        for cl in owned_claims(claims, p):
            rows.append(head + "%-8s %s" % ("claim", claim_memory(cl)))
            head = " " * len(head)
        if not leases and head.strip():
            rows.append(head + "%-8s %s" % ("-", "-"))
        for i, cl in enumerate(leases):
            prefix = head if i == 0 else " " * len(head)
            memory = fs_lease_memory(cl, k8s.get("events"), state) if fs_case else c.lease_memory(cl)
            rows.append(prefix + "%-8s %s" % (c.lease_id(cl)[:8], memory))
    if not rows:
        rows = ["no demo pods yet"]
    return lines + c.fit(rows, c.CONTENT_ROWS - 1)


def fs_lease_memory(cl, events, state):
    """Return the MEMORY column of a lease in pool-fs: "(vm1 only)" for a writer of its node only, attaching, or waiting with the reason of a failed attach."""
    if c.deleting(cl):
        return "releasing"
    why = c.attach_failure(cl, events)
    if why:
        return "waiting (%s)" % (why if why == "Conflict" else "attach")
    if c.attaching(cl):
        return "attaching"
    words = c.lease_memory(cl)
    host = c.single_host(cl)
    if host and words.endswith(" share"):
        # A share of the device, its node's attachment exclusive (ReadWriteOnce).
        words = words[:-len("share")] + "(%s only)" % c.short_node(host, state)
    return words


###
### T3 SCHEDULER
###

def driver_status(drivers_doc, state, case):
    nodes = demo_nodes(state)
    words = []
    pods = c.items(drivers_doc, "Pod")
    for n in [n for n in nodes if n]:
        mine = [p for p in pods if (p.get("spec") or {}).get("nodeName") == n]
        if not mine:
            st = "down"
        else:
            p = mine[-1]
            cs = (p.get("status") or {}).get("containerStatuses") or []
            if c.deleting(p):
                st = "restarting"
            elif (p.get("status") or {}).get("phase") == "Running" and cs and all(x.get("ready") for x in cs):
                st = "ok"
            else:
                st = "restarting" if any(x.get("restartCount") for x in cs) else "starting"
        words.append("%s %s" % (c.short_node(n, state), st))
    return "drivers: " + "  ".join(words) if words else "drivers: ?"


def shared_devices(slices_doc, serial=None):
    """Return [(driver, pool, where, device)] of the shared devices, of SERIAL only if given."""
    out = []
    for drv, pool, where, d in c.slice_devices(slices_doc):
        if c.device_attr(d, "shared") is not True:
            continue
        if serial and (c.int_any(c.device_attr(d, "serial")) != c.int_any(serial)):
            continue
        out.append((drv, pool, where, d))
    return out


def device_holders(entry, claims):
    """Return (exclusive, shared, waiting, attaching) holder words of a shared device ENTRY.

    A claim allocated a pool device holds it only once it is attached:
    before, it is attaching; after a failed attach, waiting with the reason.
    """
    drv, pool, _, d = entry
    serial = (c.device_attr(d, "serial") or "").lower()
    excl, shared, waiting, attaching = [], [], [], []
    for cl in claims:
        res = [r for r in c.results(cl) if (r.get("driver"), r.get("pool"), r.get("device")) == (drv, pool, d.get("name"))]
        why = c.attach_failure(cl) if res and not c.deleting(cl) else None
        if why:
            waiting.append("%s (%s)" % (c.holder_name(cl), why))
        elif res and c.attaching(cl) and not c.deleting(cl):
            attaching.append(c.holder_name(cl))
        elif res:
            ex, ro = c.holding(cl, res[0], d)
            word = "%s %s%s" % (c.holder_name(cl), "ro" if ro else "rw", " releasing" if c.deleting(cl) else "")
            (excl if ex else shared).append(word)
        elif not c.allocation(cl) and not c.deleting(cl) and serial and serial in c.claim_serials(cl):
            waiting.append(c.holder_name(cl))
    return sorted(excl), sorted(shared), sorted(waiting), sorted(attaching)


def fs_holder_rows(entry, claims, events, state):
    """Return the rows of the claims of the persistent device ENTRY in pool-fs.

    The writer of a node (ReadWriteOnce), the other holders exclusive or
    shared, those whose attach is pending, and those waiting, with the
    reason of a failed attach (Conflict).
    """
    drv, pool, _, d = entry
    serial = (c.device_attr(d, "serial") or "").lower()
    groups = {}
    for cl in claims:
        res = [r for r in c.results(cl) if (r.get("driver"), r.get("pool"), r.get("device")) == (drv, pool, d.get("name"))]
        if not res and (c.allocation(cl) or c.deleting(cl) or not serial or serial not in c.claim_serials(cl)):
            continue
        name = c.holder_name(cl)
        why = c.attach_failure(cl, events)
        if why:
            groups.setdefault("  waiting (%s): " % why, []).append(name)
        elif not res:
            groups.setdefault("  waiting: ", []).append(name)
        elif c.attaching(cl) and not c.deleting(cl):
            groups.setdefault("  attaching: ", []).append(name)
        else:
            ex, ro = c.holding(cl, res[0], d)
            host = c.single_host(cl)
            # A claim of the whole device is exclusive; a share that writes has its node alone.
            key = "  exclusive: " if ex else ("  writer on %s only: " % c.short_node(host, state)) if host else "  shared: "
            groups.setdefault(key, []).append("%s %s%s" % (name, "ro" if ro else "rw", " releasing" if c.deleting(cl) else ""))
    rows = []
    for key in sorted(groups, key=lambda k: (not k.startswith("  writer"), k != "  exclusive: ", k != "  shared: ",
                                             k != "  attaching: ", k)):
        names = key.startswith(("  waiting", "  attaching"))
        rows += c.wrap_list(key, sorted(groups[key]), indent=4, sep="" if names else ",")
    return rows


def render_t3(data, state, now=None, stale=False):
    if state.get("DEMO_CASE") == "wizard":
        return wv.render_t3(data, state, now, stale)
    k8s = (data.get("vm2") or {}).get("k8s") or {}
    case = state.get("DEMO_CASE", "local")
    lines = [c.title("SCHEDULER: DEVICES AND CLAIMS", stale, now)]
    status = driver_status(k8s.get("drivers"), state, case)
    pool_slices = [s for s in c.items(k8s.get("slices"))
                   if (s.get("spec") or {}).get("driver") == "cxl-pool.nri.io"
                   and not (s.get("spec") or {}).get("nodeName")]
    if pool_case(state):
        status += "  pool %s" % ("ok" if pool_slices else "down")
    rows = [status]
    claims = c.items(k8s.get("claims"), "ResourceClaim")
    devices = shared_devices(k8s.get("slices"), state.get("SERIAL")) or shared_devices(k8s.get("slices"))
    matched = set()
    for entry in devices:
        drv, pool, where, d = entry
        at = "@" + (c.short_node(where, state) if where else "pool")
        rows.append("%s %s %s shared%s" % (d.get("name"), at, c.k8s_size(c.device_attr(d, "size")),
                                           " persistent" if c.device_attr(d, "persistent") is True else ""))
        fs_rows = fs_holder_rows(entry, claims, k8s.get("events"), state) if case == "pool-fs" else None
        excl, shared, waiting, attaching = ([], [], [], []) if fs_rows is not None else device_holders(entry, claims)
        rows += fs_rows or []
        for w in excl:
            rows.append("  exclusive: " + w)
        if shared:
            rows += c.wrap_list("  shared: ", shared, indent=4)
        if attaching:
            rows += c.wrap_list("  attaching: ", attaching, indent=4, sep="")
        if waiting:
            rows += c.wrap_list("  waiting: ", waiting, indent=4, sep="")
        if not (excl or shared or waiting or attaching or fs_rows):
            rows.append("  no claims")
        serial = (c.device_attr(d, "serial") or "").lower()
        matched.update(id(cl) for cl in claims if serial in c.claim_serials(cl))
    if not devices:
        rows.append("no shared device published (yet)")
    other = [c.holder_name(cl) for cl in claims if c.lease_id(cl) and not c.allocation(cl)
             and not c.deleting(cl) and id(cl) not in matched]
    if other:
        rows += c.wrap_list("waiting (any device): ", other, indent=2, sep="")
    return lines + c.fit(rows)


###
### T4 NODES
###

def pod_names(pods_doc):
    """Return pod uid -> name; companions of leases are named after their requester."""
    out = {}
    for p in c.items(pods_doc, "Pod"):
        uid = c.meta(p).get("uid")
        if uid:
            out[uid] = c.requester(p) or c.meta(p).get("name")
    return out


def lease_claim_pods(k8s):
    """Return lease claim uid -> pod name: the claim's name in the pods' status.resourceClaimStatuses."""
    by_name = {}
    for p in c.items(k8s.get("pods"), "Pod"):
        for st in (p.get("status") or {}).get("resourceClaimStatuses") or []:
            if st.get("resourceClaimName"):
                by_name[st["resourceClaimName"]] = c.meta(p).get("name")
    out = {}
    for cl in c.items(k8s.get("claims"), "ResourceClaim"):
        uid, name = c.meta(cl).get("uid"), c.meta(cl).get("name")
        if uid and name in by_name:
            out[uid] = by_name[name]
    return out


def pmem_lines(region, node, names, claim_pods, node_mounts=False):
    """Return the lines of the fsdax namespaces of a pmem REGION: block device, its read-only flag, filesystem, openers, mounts.

    NODE_MOUNTS adds a line per node mount: type, superblock ro or rw, dax
    or the loop device of access cached.
    """
    out = []
    for n in region.get("namespaces") or []:
        block = n.get("block")
        if not block:
            out.append("    %s %s %s, no block device" % (n.get("name"), c.kernel_size(n.get("size")), n.get("mode")))
            continue
        fs = {None: "file system ?", "?": "file system ?", "none": "no file system"}.get(n.get("fs"), n.get("fs"))
        flag = "" if n.get("ro") is None else " (block %s)" % ("ro" if n["ro"] else "rw")
        out.append("    %s %s %s%s" % (block, c.kernel_size(n.get("size")), fs, flag))
        if node_mounts:
            for m in sorted((m for m in node.get("mounts") or [] if m.get("block") == block and m.get("kind") == "node"),
                            key=lambda m: m.get("target") or ""):
                how = "cached " + os.path.basename(m.get("source") or "?") if m.get("loop") else ("dax" if m.get("dax") else "no dax")
                out.append("    node mount %s %s (%s)" % (m.get("fstype") or "?", "ro" if m.get("ro") else "rw", how))
        who = {}
        for o in node.get("openers") or []:
            if o.get("block") == block:
                name = names.get(o.get("podUID")) or "pid %s" % o.get("pid")
                if who.get(name) != "rw":
                    who[name] = o.get("perms") or "ro"
        if who:
            out += c.wrap_list("    open: ", ["%s %s" % (k, v) for k, v in sorted(who.items())], indent=6)
        mounts = [m for m in node.get("mounts") or [] if m.get("block") == block]
        groups = {}
        for m in mounts:
            if m.get("kind") == "lease":
                how = "cached" if m.get("loop") else ("dax" if m.get("dax") else "no dax")
                if m.get("mountRO") is False:
                    how += " rw"
                groups.setdefault(how, []).append(claim_pods.get(m.get("claimUID")) or (m.get("claimUID") or "?")[:8])
        if groups:
            words = []
            for how in sorted(groups):
                words += sorted(set(groups[how])) + ["(%s)%s" % (how, "," if how != sorted(groups)[-1] else "")]
            out += c.wrap_list("    mounted: ", words, indent=6, sep="")
        elif any(m.get("kind") == "node" for m in mounts):
            out.append("    mounted: node mount, no lease")
        elif n.get("fs") not in (None, "?", "none"):
            out.append("    mounted: -")
    if not region.get("namespaces"):
        out.append("    no namespace yet")
    return out


def node_lines(label, node, names, claim_pods=None, node_mounts=False):
    if node is None:
        return ["%s unreachable" % label]
    devs = node.get("memdevs") or []
    if not devs:
        return ["%s no CXL memory" % label]
    out = []
    regions = node.get("regions") or []
    mappers = node.get("mappers") or []
    for i, d in enumerate(devs):
        pre = label if i == 0 else " " * len(label)
        serial = d.get("serial", "0x?")[2:]
        mine = [r for r in regions if d["name"] in (r.get("memdevs") or [])]
        if not mine:
            what = "disabled, no region" if d.get("enabled") is False else "no region"
            out.append("%s %s %s %s  %s" % (pre, d["name"], serial, c.kernel_size(d.get("size")), what))
            continue
        for j, r in enumerate(mine):
            head = "%s %s %s %s" % (pre, d["name"], serial, c.kernel_size(d.get("size"))) if j == 0 else " " * (len(label) + 1)
            if r.get("mode") == "pmem":
                out.append("%s  %s pmem %s" % (head, r["name"], c.kernel_size(r.get("size"))))
                out += pmem_lines(r, node, names, claim_pods or {}, node_mounts)
                continue
            out.append("%s  %s %s" % (head, r["name"], c.kernel_size(r.get("size"))))
            dax_names = []
            for x in r.get("dax") or []:
                drv = x.get("driver") or "no driver"
                if drv == "kmem" and x.get("targetNode") is not None:
                    drv = "kmem node %s" % x["targetNode"]
                out.append("    %s %s %s" % (x["name"], c.kernel_size(x.get("size")), drv))
                dax_names.append(x["name"])
            if not r.get("dax"):
                out.append("    no dax device (mode %s)" % r.get("mode"))
            who = {}
            for m in mappers:
                if m.get("dax") in dax_names:
                    n = names.get(m.get("podUID")) or "pid %s" % m.get("pid")
                    rw = "rw" if "w" in (m.get("perms") or "") else "ro"
                    if who.get(n) != "rw":
                        who[n] = rw
            words = ["%s %s" % (n, rw) for n, rw in sorted(who.items())]
            out += c.wrap_list("    mapped: ", words or ["-"], indent=6)
    return out


def llm_node_lines(label, node, names):
    """Return the lines of a node of llm-share: its memdev, region and filesystem, the node mount and who maps a file of it."""
    if node is None:
        return ["%s unreachable" % label]
    devs = node.get("memdevs") or []
    if not devs:
        return ["%s no CXL memory" % label]
    out = []
    for d in devs:
        mine = [r for r in node.get("regions") or [] if d["name"] in (r.get("memdevs") or [])]
        spaces = [n for r in mine for n in r.get("namespaces") or []]
        what = ("%s %s" % (mine[0]["name"], " ".join("%s %s" % (n.get("block") or n.get("name"), n.get("fs") or "")
                                                     for n in spaces) or "no namespace")) if mine else "no region"
        out.append("%s %s %s %s" % (label, d["name"], c.kernel_size(d.get("size")), what.rstrip()))
        for n in spaces:
            for m in sorted((m for m in node.get("mounts") or [] if m.get("block") == n.get("block") and m.get("kind") == "node"),
                            key=lambda m: m.get("target") or ""):
                how = "cached " + os.path.basename(m.get("source") or "?") if m.get("loop") else ("dax" if m.get("dax") else "no dax")
                who = sorted(set(names.get(x.get("podUID")) or "pid %s" % x.get("pid") for x in node.get("mappers") or []
                                 if x.get("path") and x.get("source") == os.path.basename(m.get("source") or "")))
                out += c.wrap_list("    %s %s %s: " % (m.get("fstype") or "?", "ro" if m.get("ro") else "rw", how),
                                   who or ["not mapped"], indent=6)
    return out


def render_t4(data, state, now=None, stale=False):
    if state.get("DEMO_CASE") == "wizard":
        return wv.render_t4(data, state, now, stale)
    vm2 = data.get("vm2") or {}
    vm1 = data.get("vm1") or {}
    k8s = vm2.get("k8s") or {}
    names = pod_names(k8s.get("pods"))
    claim_pods = lease_claim_pods(k8s)
    lines = [c.title("NODES: CXL IN THE KERNEL", stale, now)]
    if state.get("DEMO_CASE") == "llm-share":
        rows = []
        for label in ("vm0", "vm1", "vm2"):
            d = data.get(label)
            rows += llm_node_lines(label, d.get("node") if d else None, names)
        return lines + c.fit(rows)
    order = [("vm2", vm2), ("vm1", vm1)]
    if pool_case(state):
        order.reverse()
    rows = []
    for label, d in order:
        rows += node_lines(label, d.get("node") if d else None, names, claim_pods, state.get("DEMO_CASE") == "pool-fs")
    return lines + c.fit(rows)


###
### T6 MEMORY (local) and POOL (pool)
###

def text_at(hexdata, width=None):
    """Return the NUL-terminated text at the start of HEXDATA quoted, cut with "..." to WIDTH columns."""
    data = bytes.fromhex(hexdata or "")
    s = data.split(b"\0", 1)[0]
    if not s:
        return "(zeros)" if data and not any(data) else "(empty)"
    text = '"%s"' % "".join(chr(b) if 32 <= b < 127 else "." for b in s)
    if width and len(text) > width:
        text = text[:width - 4] + '"...'
    return text


def hex_line(hexdata, offset=0):
    data = bytes.fromhex(hexdata or "")[offset:offset + 16]
    groups = [data[i:i + 4].hex() for i in range(0, len(data), 4)]
    return "%04x: %s" % (offset, " ".join(groups))


def render_t6_local(data, state, now=None, stale=False):
    vm2 = data.get("vm2") or {}
    b = vm2.get("bytes") or {}
    serial = state.get("SERIAL", "")
    where = b.get("dax") or (serial[2:] if serial.startswith("0x") else serial)
    lines = [c.title("MEMORY: %s on vm2" % where, stale, now)]
    rows = []
    if b.get("hex"):
        rows.append("at 0: " + text_at(b["hex"], c.WIDTH - 6))
        rows.append(hex_line(b["hex"]))
    elif b.get("error") == "no dax device":
        rows.append("%s %s: no region, no dax device yet" % (b.get("memdev", "memdev"), serial))
        rows.append("the first claim creates them")
    else:
        rows.append("cannot read: %s" % (b.get("error") or "no data"))
    k8s = vm2.get("k8s") or {}
    devs = shared_devices(k8s.get("slices"), serial)
    if devs:
        excl, shared, waiting, _ = device_holders(devs[0], c.items(k8s.get("claims"), "ResourceClaim"))
        if excl:
            rows.append("used by: %s (exclusive)" % ", ".join(excl))
        elif shared:
            rows += c.wrap_list("shared by: ", shared, indent=4)
        else:
            rows.append("used by: nobody (the bytes stay)")
        if waiting:
            rows += c.wrap_list("waiting: ", waiting, indent=4, sep="")
    return lines + c.fit(rows)


def render_t6_pool(data, state, now=None, stale=False):
    pool = data.get("pool") or {}
    dev = pool.get("device") or {}
    lines = [c.title("POOL: fake-cxl-pool-server", stale, now)]
    rows = []
    if not dev:
        rows.append("no device %s: %s" % (state.get("POOL_DEVICE", "?"), pool.get("error", "?")))
        return lines + rows
    rows.append("%s %s %s  %s" % (dev.get("name"), c.k8s_size(dev.get("size")), c.device_kind(dev), dev.get("state")))
    atts = dev.get("attachments") or []
    words = []
    for a in sorted(atts, key=lambda a: a.get("host") or ""):
        w = c.short_node(a.get("host"), state)
        if a.get("state") != "attached":
            w += " " + (a.get("state") or "?")
        words.append(w)
    excl = any(a.get("singleHost") for a in atts)
    if words:
        rows += c.wrap_list("  attached: ", words + (["single-host"] if excl else []), indent=4,
                            sep="," if not excl else "")
    else:
        rows.append("  attached: nowhere")
    if pool.get("fileError"):
        rows.append("backing file: " + pool["fileError"])
    elif state.get("DEMO_CASE") in ("pool-shared-fs", "llm-share"):
        rows += fs_lines(pool.get("fs"))
    elif state.get("DEMO_CASE") == "pool-fs":
        rows += fsinfo.lines(pool.get("super"))
    elif pool.get("hex"):
        rows.append("at 0: " + text_at(pool["hex"], c.WIDTH - 6))
        rows.append(hex_line(pool["hex"]))
    if pool.get("fileResident") is not None:
        rows.append("host memory: %s of the file, once" % c.kernel_size(pool["fileResident"]))
    rows.append("file " + (dev.get("path") or "?"))
    return lines + c.fit(rows)


def fs_lines(fs):
    """Return the lines of the file system FS (common.erofs_files) of the pool device."""
    if not fs:
        return ["file system: no data"]
    if fs.get("type") == "zeros":
        return ["no file system (zeros)"]
    if fs.get("type") != "erofs":
        return ["no file system"]
    files = fs.get("files") or []
    if not files:
        return ["erofs (not readable: %s)" % fs["error"] if fs.get("error") else "erofs: no files"]
    rows = []
    for i, f in enumerate(files):
        if f.get("type") == "dir":
            what = f["name"] + "/"
        else:
            what = "%s %s bytes" % (f["name"], f["size"] if f.get("size") is not None else "?")
        rows.append(("erofs: " if i == 0 else "       ") + what)
    first = next((f for f in files if f.get("type") == "file"), None)
    if first:
        text = first.get("text")
        rows.append("  " + (text if text in (None, "(not readable)") else '"%s"' % text))
    return rows


def render_t6(data, state, now=None, stale=False):
    if state.get("DEMO_CASE") == "wizard":
        return wv.render_t6(data, state, now, stale)
    if pool_case(state):
        return render_t6_pool(data, state, now, stale)
    return render_t6_local(data, state, now, stale)


###
### T5 TRACE
###

KLOG = re.compile(r"^([IWEF])\d{4} [\d:.]+\s+\d+ [^\]]+\] (.*)$")
LEASE_MARK = "\x01%s\x02"
LEASE_RE = re.compile("\x01([0-9a-f]{8})\x02")


class Trace:
    """Reads the streams of dra-trace-start and translates them to one line per event."""

    def __init__(self, trace_dir, state):
        self.dir = trace_dir
        self.state = state
        self.ns = state.get("NS", "")
        self.offsets = {}
        self.events = []        # (t, tag, text)
        self.who = {}           # lease id -> requester pod
        self.uid_claim = {}     # claim uid prefix -> claim name
        self.claims = {}        # claim name -> (allocated, deleting)
        self.busy = set()       # lease ids whose busy unmount is shown
        self.retrying = set()   # requesters whose attach Conflict is shown, until attached
        self.mem_added = {}     # (node, memdev) -> time of its udev add
        self.new_lines = 0      # new lines between latest and previous poll()

    def lease_who(self, name):
        """Return a marker of the requester of a companion claim or pod cxl-lease-ID, else NAME.

        The marker is resolved when the line is shown: the driver names the
        requester of a lease, but its log may come after the scheduler's.
        """
        m = re.fullmatch(r"cxl-lease-([0-9a-f]{8})", name or "")
        if m:
            return LEASE_MARK % m.group(1)
        return name

    def text(self, text):
        """Return TEXT with the requesters of the lease markers."""
        return re.sub(LEASE_RE, lambda m: self.who.get(m.group(1), "lease " + m.group(1)), text)

    def node_label(self, tag):
        vm = tag.split(".")[0]
        return "vm2" if vm == "vm" else vm

    def short(self, text):
        return c.short_text(text, self.state)

    def emit(self, t, tag, text):
        self.events.append([t, tag, self.short(text)])

    def poll(self, until=None):
        """Read what the streams added, translate it; return True if anything changed."""
        if not self.dir or not os.path.isdir(self.dir):
            return False
        rows = []
        for f in sorted(os.listdir(self.dir)):
            if not f.endswith(".log"):
                continue
            p = os.path.join(self.dir, f)
            try:
                with open(p, "rb") as fh:
                    fh.seek(self.offsets.get(f, 0))
                    data = fh.read()
            except OSError:
                continue
            end = data.rfind(b"\n") + 1
            self.offsets[f] = self.offsets.get(f, 0) + end
            for raw in data[:end].decode(errors="replace").splitlines():
                parts = raw.split(" ", 2)
                if len(parts) < 3:
                    continue
                try:
                    t = float(parts[0])
                except ValueError:
                    continue
                if until is not None and t > until:
                    continue
                rows.append((t, parts[1], parts[2]))
        rows.sort(key=lambda r: r[0])
        before = len(self.events)
        for t, tag, text in rows:
            kind = tag.split(".")[-1]
            fn = getattr(self, "on_" + kind, None)
            if fn:
                fn(t, tag, text)
        self.new_lines = len(self.events) - before
        return len(self.events) != before

    def on_events(self, t, tag, text):
        cols = re.split(r"\s{2,}|\t", text.strip(), maxsplit=4)
        if len(cols) < 4 or cols[0] != self.ns:
            return
        kind, name, reason = cols[1], cols[2], cols[3]
        msg = cols[4] if len(cols) > 4 else ""
        who = self.lease_who(name)
        node = re.search(r" to (\S+)$", msg)
        if who in self.retrying and reason in ("Scheduled", "BindingConditionsPending", "FailedScheduling", "AttachFailed"):
            return   # the scheduler's next attempts while another host has the device alone
        conflict = re.match(r"attach (\S+) to (\S+): (Conflict): ", msg) if reason == "AttachFailed" else None
        if conflict:
            self.retrying.add(who)
            self.emit(t, "ctrl", "%s: %s not on %s: %s" % (who, conflict.group(1), conflict.group(2), conflict.group(3)))
        elif kind == "Pod" and reason == "FailedScheduling":
            why = "cannot allocate" if "cannot allocate all claims" in msg else msg
            if "deallocation of ResourceClaim completed" in msg:
                why = "claim deallocated"   # after a failed binding condition: allocated again
            self.emit(t, "sched", "%s waits: %s" % (who, why))
        elif kind == "Pod" and reason == "Scheduled":
            what = "lease scheduled" if who != name else "scheduled"
            self.emit(t, "sched", "%s: %s on %s" % (who, what, node.group(1) if node else "?"))
        elif kind == "Pod" and reason == "BindingConditionsPending":
            self.emit(t, "sched", "%s: waits for the attach" % who)
        elif kind == "ResourceClaim" and reason == "AttachFailed":
            self.emit(t, "ctrl", "%s: AttachFailed %s" % (who, msg))
        elif reason.startswith("Failed") or reason in ("Unschedulable",):
            self.emit(t, "kube", "%s: %s %s" % (who, reason, msg))

    def on_claims(self, t, tag, text):
        m = re.match(r"(\S+) claim (\S+)/(\S+) uid=(\S+) allocated=(\S+) node=(\S+) reservedFor=\S+ status=\S+( deleting)?", text)
        if not m or m.group(2) != self.ns:
            return
        event, name, uid, alloc, node, dele = m.group(1), m.group(3), m.group(4), m.group(5), m.group(6), bool(m.group(7))
        self.uid_claim[uid[:8]] = name
        if alloc.endswith("/lease") or (alloc == "-" and not name.startswith("cxl-lease-")):
            return   # the lease claims of the demo pods (device "lease"), before their allocation
        who = self.lease_who(name)
        was = self.claims.get(name)
        if event == "DELETED":
            self.claims.pop(name, None)
            self.retrying.discard(who)
            self.emit(t, "api", "%s: claim deleted" % who)
            return
        self.claims[name] = (alloc, dele)
        if was is None and event == "ADDED":
            self.emit(t, "api", "%s: claim created" % who)
        if alloc != "-" and (was is None or was[0] == "-") and who not in self.retrying:
            self.emit(t, "api", "%s: allocated %s" % (who, alloc.split("/")[-1]))
        if dele and (was is None or not was[1]):
            self.emit(t, "api", "%s: claim deleting" % who)

    def on_driver(self, t, tag, text):
        m = KLOG.match(text)
        if not m:
            return
        level, msg = m.group(1), m.group(2)
        node = self.node_label(tag)
        lm = re.match(r"lease broker: lease ([0-9a-f]{8}): (.*)$", msg)
        if lm:
            lid, rest = lm.group(1), lm.group(2)
            rq = re.match(r"\S+?/([^/\s]+)/\S+ requests .*mode (\w+), share (\w+), .*read-only (\w+)[,)]", rest)
            if rq:
                self.who[lid] = rq.group(1)
                self.emit(t, "drv", "%s %s asks %s %s %s" % (
                    node, rq.group(1), rq.group(2), "ro" if rq.group(4) == "true" else "rw",
                    "share" if rq.group(3) == "true" else "excl"))
                return
            who = LEASE_MARK % lid
            if rest.startswith("Ready"):
                self.retrying.discard(who)
                self.emit(t, "drv", "%s %s: lease Ready%s" % (node, who, self.ready_what(rest)))
            elif rest.startswith("releasing"):
                why = re.match(r"releasing \(([^,]*)", rest)
                why = (why.group(1) if why else "?").replace("requester ", "").replace("container", "ctr")
                if why.startswith("released by"):
                    self.emit(t, "drv", "%s %s: lease releasing" % (node, who))
                else:
                    # "vm2 reader-a releases: ctr gone" fits the 35 columns.
                    self.emit(t, "drv", "%s %s releases: %s" % (node, who, why))
            elif rest.startswith("released"):
                self.emit(t, "drv", "%s %s: lease released" % (node, who))
            elif level in "EF" or rest.startswith("failed") or "Failed" in rest:
                self.emit(t, "drv", "%s %s: ERROR %s" % (node, who, rest))
            return
        pp = re.search(r'claim \S+?/(\S+): prepared \S+ \(.*mode (\w+), read-only (\w+)\) as \S+ \S+ namespace \S+ block (\S+) access "([^"]*)"(?: fs "([^"]*)")?', msg)
        if pp:
            ro = "ro" if pp.group(3) == "true" else "rw"
            if pp.group(2) == "fs":
                # Read-only is the default of fs: only rw is said, the access fits the 35 columns.
                what = "%s%s %s" % (pp.group(6) or "fs", "" if ro == "ro" else " rw", pp.group(5))
                if len(self.text("%s %s: prepared %s" % (node, self.lease_who(pp.group(1)), what))) > c.WIDTH - 15:
                    what = what.rsplit(" ", 1)[0]   # "vm1 writer: prepared btrfs rw cached": the access goes
            else:
                what = "%s %s" % (os.path.basename(pp.group(4)), ro)
            self.emit(t, "drv", "%s %s: prepared %s" % (node, self.lease_who(pp.group(1)), what))
            return
        if self.on_node_mount(t, node, msg):
            return
        if self.on_lease_mount(t, node, msg):
            return
        pm = re.search(r"claim \S+?/(\S+): prepared (\S+) \(.*read-only (\w+)\) as \S+ \S+ dax (\S+) .*?node \S+( exclusive| \d+ shares?)?$", msg)
        if pm:
            ro = "ro" if pm.group(3) == "true" else "rw"
            if pm.group(5):
                # The shares of a shared local memdev.
                self.emit(t, "drv", "%s %s: %s %s %s" % (
                    node, self.lease_who(pm.group(1)), pm.group(4), ro, "excl" if "exclusive" in pm.group(5) else "share"))
            else:
                self.emit(t, "drv", "%s %s: prepared %s %s" % (node, self.lease_who(pm.group(1)), pm.group(4), ro))
            return
        um = re.search(r"unprepared claim \S+?/(\S+)", msg)
        if um:
            self.emit(t, "drv", "%s %s: unprepared" % (node, self.lease_who(um.group(1))))
            return
        # The pool driver disables a memdev that no claim uses; the detach follows.
        rm = re.match(r"engine: \w+ memdev \S+ \((mem\d+)\): disabled$", msg)
        if rm:
            self.emit(t, "drv", "%s disabled %s" % (node, rm.group(1)))
            return
        # The other lines of a shared local device (05-status-DRV section 5).
        sm = re.match(r"engine: local memdev \S+ \(\S+\): (.*)$", msg)
        if sm:
            rest = sm.group(1)
            cr = re.match(r"created (region\d+) \(devdax, dax (\S+?),", rest)
            rl = re.match(r"claim (\S+): released shared (\S+), claims left \[(.*)\]", rest)
            if cr:
                self.emit(t, "drv", "%s created %s %s" % (node, cr.group(1), cr.group(2)))
            elif rl:
                claim = self.uid_claim.get(rl.group(1)[:8], rl.group(1)[:8])
                left = len(rl.group(3).split())
                self.emit(t, "drv", "%s %s: released, %d left" % (node, self.lease_who(claim), left))
            elif rest.startswith("zeroed new shared"):
                self.emit(t, "drv", "%s zeroed new %s (once)" % (node, rest.split()[3]))
            elif rest.startswith("keeping") and "idle" in rest:
                self.emit(t, "drv", "%s idle: keeping the region" % node)
            elif re.match(r"destroyed region\d+", rest):
                self.emit(t, "drv", "%s %s" % (node, rest.split(" in ")[0]))
            elif rest.startswith("not in sharedMemdevs"):
                self.emit(t, "drv", "%s unshared: %s the region" % (node, "keeping" if "keeping" in rest else "destroying"))
            return
        if level in "EF":
            self.emit(t, "drv", "%s ERROR %s" % (node, msg))

    def ready_what(self, rest):
        """Return what a Ready line of the lease broker names: " block pmem0", " fs hello", " dax0.0"."""
        block = re.search(r'block "([^"]+)"', rest)
        path = re.search(r'path "([^"]+)"', rest)
        dax = re.search(r'dax "([^"]+)"', rest)
        if block:
            return " block " + os.path.basename(block.group(1))
        if path:
            return " fs " + os.path.basename(path.group(1))
        return " " + os.path.basename(dax.group(1)) if dax else ""

    def on_node_mount(self, t, node, msg):
        """Emit the node mount, remount, unmount and block device read-only lines of persistent devices in MSG; return True if MSG was one."""
        dm = re.match(r"engine: \w+ memdev (\S+): (.*)$", msg)
        if not dm:
            return False
        rest = dm.group(2)
        mm = re.match(r"mounted (\S+) at \S+ \(([^,)]+), (ro|rw)(?:, [^)]*)?\)$", rest)
        rm = re.match(r"remounted \S+ (read-write|read-only)$", rest)
        um = re.match(r"unmounted \S+$", rest)
        fm = re.match(r"(/dev/\S+) (read-only|read-write)$", rest)
        if mm:
            self.emit(t, "drv", "%s node mount %s %s %s" % (node, os.path.basename(mm.group(1)), mm.group(2), mm.group(3)))
        elif rm:
            self.emit(t, "drv", "%s node mount now %s" % (node, rm.group(1)))
        elif um:
            self.emit(t, "drv", "%s node mount unmounted" % node)
        elif fm:
            self.emit(t, "drv", "%s %s %s" % (node, os.path.basename(fm.group(1)), fm.group(2)))
        return bool(mm or rm or um or fm)

    def on_lease_mount(self, t, node, msg):
        """Emit the bind, unbind and busy unmount lines of fs leases in MSG; return True if MSG was one."""
        bm = re.search(r": lease ([0-9a-f]{8}): bound \S+ at (\S+?)( again)?$", msg)
        um = re.search(r": lease ([0-9a-f]{8}): unbound (\S+)$", msg)
        busy = re.search(r"claim \S+: lease ([0-9a-f]{8}): unmounting \S+: .*busy", msg)
        if bm:
            self.emit(t, "drv", "%s %s: mounted %s%s" % (node, LEASE_MARK % bm.group(1), os.path.basename(bm.group(2)),
                                                       " again" if bm.group(3) else ""))
        elif um:
            self.busy.discard(um.group(1))
            self.emit(t, "drv", "%s %s: unmounted %s" % (node, LEASE_MARK % um.group(1), os.path.basename(um.group(2))))
        elif busy:
            # kubelet retries the Unprepare: one line until the unmount succeeds.
            if busy.group(1) not in self.busy:
                self.busy.add(busy.group(1))
                self.emit(t, "drv", "%s %s: unmount busy, retry" % (node, LEASE_MARK % busy.group(1)))
        return bool(bm or um or busy)

    def on_kubelet(self, t, tag, text):
        self.on_lease_mount(t, self.node_label(tag), text)

    def on_udev(self, t, tag, text):
        m = re.match(r"KERNEL\[[\d.]+\]\s+(add|remove|bind|unbind)\s+(\S+) \((cxl|dax|block)\)", text)
        if not m:
            return
        name = os.path.basename(m.group(2))
        # pmemN of the cxl subsystem is the memdev's nvdimm, of block the block device.
        if not re.fullmatch(r"mem\d+|region\d+|dax\d+\.\d+", name) and not (m.group(3) == "block" and re.fullmatch(r"pmem\d+", name)):
            return
        node = self.node_label(tag)
        # Of the driver binds only that of a memdev enabled again without a new
        # attach (the driver's "disabled memN" says the unbind).
        if m.group(1) == "add" and re.fullmatch(r"mem\d+", name):
            self.mem_added[(node, name)] = t
        if m.group(1) == "unbind" or (m.group(1) == "bind" and not (
                m.group(3) == "cxl" and re.fullmatch(r"mem\d+", name) and t - self.mem_added.get((node, name), 0) > 5)):
            return
        head = "%s %s" % (node, m.group(1))
        last = self.events[-1] if self.events else None
        # One line for what one action did: "vm1 add mem0 region0 dax0.0".
        if last and last[1] == "kern" and last[2].startswith(head + " ") and t - last[0] < 2:
            if name not in last[2].split():
                last[2] += " " + name
            return
        self.emit(t, "kern", "%s %s" % (head, name))

    def on_server(self, t, tag, text):
        m = re.search(r"attachment (\S+)@(\S+): (attached|detached|detaching|failed|single host \w+)", text)
        if not m:
            return
        dev, host, what = m.group(1), m.group(2), m.group(3)
        if what == "attached":
            self.emit(t, "pool", "%s attached to %s" % (dev, host))
        elif what == "detached":
            self.emit(t, "pool", "%s detached from %s" % (dev, host))
        elif what == "detaching":
            self.emit(t, "pool", "%s detaching from %s" % (dev, host))
        elif what == "failed":
            self.emit(t, "pool", "%s on %s: attachment failed" % (dev, host))
        else:
            if what == "single host false":
                # No single-host attachment any more: the next attempts are shown.
                self.retrying.clear()
            self.emit(t, "pool", "%s on %s: %s" % (dev, host, "single-host" if what == "single host true" else "not single-host"))

    def on_controller(self, t, tag, text):
        text = re.sub(r"^fake-cxl-pool-controller: \S+ \S+ ", "", text)
        m = re.match(r"(attached|detached) (\S+) (to|from) (\S+)(.*?)(?: for \S+?/(\S+))?$", text)
        wm = re.match(r"attach (\S+) to (\S+) for claim \S+?/(\S+) .*: single host for a read-write (\S+) claim$", text)
        fm = re.match(r"ERROR: (\w+) for claim \S+?/(\S+) device (\S+) node (\S+): ", text)
        am = re.match(r"attach (\S+) to (\S+) for claim \S+?/(\S+) .*: already attached", text)
        if m:
            excl = " excl" if "single host true" in (m.group(5) or "") else ""
            who = self.lease_who(m.group(6)) + ": " if m.group(6) else ""
            if m.group(6):
                self.retrying.discard(self.lease_who(m.group(6)))
            self.emit(t, "ctrl", "%s%s %s %s %s%s" % (who, m.group(1)[:-2] if m.group(1) == "attached" else "detach",
                                                      m.group(2), m.group(3), m.group(4), excl))
        elif wm:
            self.emit(t, "ctrl", "%s: %s excl: rw %s" % (self.lease_who(wm.group(3)), wm.group(1), wm.group(4)))
        elif fm:
            who = self.lease_who(fm.group(2))
            if fm.group(1) == "Conflict":
                if who in self.retrying:
                    return   # shown once until it is attached
                self.retrying.add(who)
            self.emit(t, "ctrl", "%s: %s not on %s: %s" % (who, fm.group(3), fm.group(4), fm.group(1)))
        elif text.startswith("status of "):
            pass   # the claim status written after an attach or its failure: the lines above tell it
        elif am:
            self.emit(t, "ctrl", "%s: %s on %s already" % (self.lease_who(am.group(3)), am.group(1), am.group(2)))
        elif re.search(r"(?i)\berror\b|failed", text):
            self.emit(t, "ctrl", "ERROR " + text)


def render_t5(trace, state, now=None, stale=False, new_lines_only=False):
    """Return the title and the last rows of the trace, or only the rows of its last poll (NEW_LINES_ONLY)."""
    if new_lines_only:
        lines = []
        row_data = []
        if trace.new_lines > 0:
            row_data = trace.events[-trace.new_lines:]
    else:
        lines = [c.title("TRACE", stale, now)]
        row_data = trace.events[-c.CONTENT_ROWS:]
    rows = ["%s %-5s %s" % (time.strftime("%H:%M:%S", time.localtime(t)), tag, trace.text(text))
            for t, tag, text in row_data] if trace else []
    return lines + rows


###
### Collecting, painting
###

def collect(view, state):
    """Return the data of VIEW from the VMs, the pool server or the host."""
    ns = state.get("NS", "default")
    vm2, vm1 = state.get("VM2_DIR"), state.get("VM1_DIR")
    events = ",events" if state.get("DEMO_CASE") == "pool-fs" else ""
    if view == "t2":
        return {"vm2": need(c.run_probe(vm2, ["--k8s", "pods,claims" + events, "--ns", ns]), "pods", "claims")}
    if view == "t3":
        return {"vm2": need(c.run_probe(vm2, ["--k8s", "claims,slices,drivers" + events, "--ns", ns]), "claims", "slices", "drivers")}
    if view == "t4":
        out, errs = {}, []

        def get(key, vmdir, args):
            try:
                out[key] = c.run_probe(vmdir, args)
            except RuntimeError as e:
                errs.append("%s: %s" % (key, e))
        ts = [threading.Thread(target=get, args=("vm2", vm2, ["--node", "--k8s", "pods,claims", "--ns", ns])),
              threading.Thread(target=get, args=("vm1", vm1, ["--node"]))]
        if state.get("VM0_DIR"):
            ts.append(threading.Thread(target=get, args=("vm0", state["VM0_DIR"], ["--node"])))
        for t in ts:
            t.start()
        for t in ts:
            t.join()
        out["partial"] = errs
        if len(errs) == len(ts):
            raise RuntimeError("; ".join(errs))
        return out
    if view == "t6" and state.get("DEMO_CASE") == "wizard":
        return wv.collect_t6(state)
    if view == "t6" and pool_case(state):
        dev = state.get("POOL_DEVICE", "shared0")
        out = {"device": c.http_json("%s/api/v1/devices/%s" % (state["POOL_URL"], dev))}
        try:
            with open(out["device"].get("path") or "", "rb") as f:
                if state.get("DEMO_CASE") == "llm-share":
                    # The pages of the backing file are the device in host memory.
                    out["fileResident"] = os.fstat(f.fileno()).st_blocks * 512
                if state.get("DEMO_CASE") in ("pool-shared-fs", "llm-share"):
                    # The erofs is on the block device: after the pfn info
                    # block. The model's data starts 2 MiB into it.
                    out["fsOffset"], data = c.read_fsdax(f, (4 if state.get("DEMO_CASE") == "llm-share" else 1) << 20)
                    out["fs"] = c.erofs_files(data)
                elif state.get("DEMO_CASE") == "pool-fs":
                    # The superblock of the filesystem on the block device.
                    out["fsOffset"], data = c.read_fsdax(f, fsinfo.READ_LEN)
                    out["super"] = fsinfo.fs_super(data)
                else:
                    out["hex"] = f.read(256).hex()
        except OSError as e:
            out["fileError"] = e.strerror or str(e)
        return {"pool": out}
    if view == "t6":
        return {"vm2": need(c.run_probe(vm2, ["--bytes", state.get("SERIAL", "0"), "--k8s", "claims,slices", "--ns", ns]),
                            "claims", "slices")}
    raise ValueError(view)


def need(probe, *kinds):
    missing = [k for k in kinds if k not in (probe.get("k8s") or {})]
    if missing:
        raise RuntimeError((probe.get("errors") or ["no %s" % ",".join(missing)])[0])
    return probe


TITLES = {"t2": "PODS & LEASES", "t3": "SCHEDULER: DEVICES AND CLAIMS", "t4": "NODES: CXL IN THE KERNEL",
          "t5": "TRACE", "t6": "MEMORY"}
RENDER = {"t2": render_t2, "t3": render_t3, "t4": render_t4, "t5": render_t5, "t6": render_t6}


def paint(lines, color, clear=True):
    """Paint LINES: a whole screen from the top (CLEAR, the title first), else below the last line painted.

    The lines painted below the others scroll the terminal: the cursor
    stays at the end of the last line.
    """
    rows = []
    for i, line in enumerate(lines[:c.ROWS] if clear else lines):
        line = c.cut(line)
        if color:
            line = (c.BOLD + line + c.RESET) if clear and i == 0 else c.colorize(line)
        rows.append(line + "\033[K")
    if clear:
        sys.stdout.write("\033[H" + "\n".join(rows) + "\033[J")
    else:
        sys.stdout.write("".join("\n" + r for r in rows))
    sys.stdout.flush()


def loop_t5(case, color, interval):
    """Paint the trace as a sequence: a first screen (the title and the last events), then only lines below it.

    Every new event is a line; when the demo goes away or a new trace
    starts, one line says so and the new trace's events follow. The
    cursor never goes back, so the terminal's scrollback keeps it all.
    """
    sys.stdout.write("\033[2J\033[?25l")
    trace, showing, first = None, None, True
    try:
        while True:
            start = time.time()
            state = c.load_state()
            if state is not None and case:
                state["DEMO_CASE"] = case
            key = None if state is None else state.get("TRACE_DIR", "")
            lines = []
            if first or key != showing:
                clock = time.strftime("%H:%M:%S")
                if state is None:
                    trace = None
                    lines = ([c.title(TITLES["t5"]), "demo not set up: demo setup " + "|".join(CASES)] if first else
                             ["--- %s no demo (teardown, or not set up)" % clock])
                else:
                    trace = Trace(state.get("TRACE_DIR"), state)
                    trace.poll()
                    lines = (render_t5(trace, state) if first else
                             ["--- %s new trace: setup %s" % (clock, state.get("DEMO_CASE", "?"))] +
                             render_t5(trace, state, new_lines_only=True))
                showing = key
            elif trace is not None:
                trace.poll()
                lines = render_t5(trace, state, new_lines_only=True)
            if first or lines:
                paint(lines, color, clear=first)
            first = False
            time.sleep(max(0.1, interval - (time.time() - start)))
    except KeyboardInterrupt:
        pass
    finally:
        sys.stdout.write("\033[?25h\n")


def loop(view, case, color, interval, once):
    if view == "t5" and not once:
        return loop_t5(case, color, interval)
    last = None
    if not once:
        sys.stdout.write("\033[2J\033[?25l")
    try:
        while True:
            start = time.time()
            state = c.load_state()
            if state is None:
                lines = [c.title(TITLES[view]), "demo not set up: demo setup " + "|".join(CASES)]
                last = None
            else:
                if case:
                    state["DEMO_CASE"] = case
                stale = False
                if view == "t5":
                    # --once: one screen, the title and the last events
                    trace = Trace(state.get("TRACE_DIR"), state)
                    trace.poll()
                    lines = render_t5(trace, state)
                else:
                    err = ""
                    try:
                        data = collect(view, state)
                        last = data
                        stale = bool(data.get("partial"))
                    except Exception as e:   # keep the last good rendering
                        stale = True
                        data = last
                        err = str(e)
                    if data is None:
                        lines = [c.title(TITLES[view], True), "no data yet: " + err]
                    else:
                        lines = RENDER[view](data, state, None, stale)
            if once:
                print("\n".join(c.cut(l) for l in lines))
                return
            paint(lines, color)
            time.sleep(max(0.1, interval - (time.time() - start)))
    except KeyboardInterrupt:
        pass
    finally:
        if not once:
            sys.stdout.write("\033[?25h\n")


def sample(view, sdir, now, until):
    """Render VIEW from the test data in SDIR, at the clock NOW."""
    state = c.load_state(os.path.join(sdir, "state.env")) or {}
    if view == "t5":
        tr = Trace(os.path.join(sdir, "trace"), state)
        tr.poll(until)
        return render_t5(tr, state, now)
    data = {}
    for key in ("vm2", "vm1", "vm0", "pool"):
        p = os.path.join(sdir, key + ".json")
        if os.path.exists(p):
            with open(p) as f:
                data[key] = json.load(f)
    stale = os.path.exists(os.path.join(sdir, "stale"))
    return RENDER[view](data, state, now, stale)


def main():
    args = sys.argv[1:]
    if not args or args[0] in ("-h", "--help") or args[0] not in RENDER:
        sys.exit(__doc__)
    view = args[0]
    opt = {}
    rest = []
    i = 1
    while i < len(args):
        if args[i] in ("--sample", "--now", "--until", "--interval") and i + 1 < len(args):
            opt[args[i]] = args[i + 1]
            i += 2
        else:
            rest.append(args[i])
            i += 1
    if "--sample" in opt:
        now = float(opt.get("--now", time.time()))
        until = float(opt["--until"]) if "--until" in opt else None
        print("\n".join(c.cut(l) for l in sample(view, opt["--sample"], now, until)))
        return
    case = next((a for a in rest if a in CASES), None)
    loop(view, case, c.use_color(rest) and "--once" not in rest, float(opt.get("--interval", 1)), "--once" in rest)


if __name__ == "__main__":
    main()
