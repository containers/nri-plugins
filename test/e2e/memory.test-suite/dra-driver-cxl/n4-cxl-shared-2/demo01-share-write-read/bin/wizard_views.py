"""The views of the demo case "wizard" (docs/memory/cxl/demo.md, The
wizard): T2 the
pods of the wizard with their claims or leases and what they got, T3 every
published memory device with its holders and the claims that wait, T4 both
nodes with a line per memdev and who uses its memory, T6 the devices of
both pool servers (fake-cxl-pool-server, local-pmem) with the filesystem
state of the persistent ones (bin/fsinfo.py, never the files). T5 is that
of every case (views.py).
"""
import common as c
import fsinfo


def wizard_pods(k8s):
    """Return the pods of the namespace without the companion pods of leases, by name."""
    pods = [p for p in c.items(k8s.get("pods"), "Pod") if not c.lease_id(p)]
    return sorted(pods, key=lambda p: (len(c.meta(p).get("name") or ""), c.meta(p).get("name")))


def pod_claims(pod, claims):
    """Return ("claim", [claim]) of a pod with a ResourceClaim, ("lease", [companions]) of a pod with a lease claim."""
    name, uid = c.meta(pod).get("name"), c.meta(pod).get("uid")
    refs = (pod.get("spec") or {}).get("resourceClaims") or []
    static = [r.get("resourceClaimName") for r in refs if r.get("resourceClaimName")]
    if static:
        return "claim", [cl for cl in claims if c.meta(cl).get("name") in static]
    comps = [cl for cl in claims if c.lease_id(cl) and
             (c.requester(cl) == name or c.labels(cl).get(c.REQUESTER_UID_LABEL) == uid)]
    return "lease", sorted(comps, key=lambda cl: c.meta(cl).get("creationTimestamp") or "")


def attach_failed(claim):
    """Return the reason of a true AttachFailed condition of CLAIM, "" if there is none."""
    for d in (claim.get("status") or {}).get("devices") or []:
        for cond in d.get("conditions") or []:
            if (cond.get("type") or "").endswith("AttachFailed") and cond.get("status") == "True":
                return cond.get("reason") or "?"
    return ""


def device_word(r, state):
    """Return the short name of the device of an allocation result: the pool device, lp0, or the serial hex."""
    dev = r.get("device") or "?"
    if dev.startswith("memdev-"):
        hexs = dev[len("memdev-"):]
        lp = (state.get("LP_SERIAL") or "").lower()
        return "lp0" if lp and lp[2:] == hexs else hexs
    return dev


def claim_memory(cl, state):
    """Return what a claim got in a few words: mode, access, excl/share or the slice size, the device; or why it waits."""
    if c.deleting(cl):
        return "releasing"
    why = attach_failed(cl)
    res = c.results(cl)
    if why:
        return "waiting (%s)" % why
    if not res:
        return "waiting"
    r = res[0]
    if c.attaching(cl):
        # A pool device is the claim's only once it is attached.
        return "attaching %s" % device_word(r, state)
    classes = [s.get("deviceClassName") or "" for s in c.subrequests(cl)]
    sliced = c.consumed_shares(r) is None and c.shares_request(cl) is None and \
        not any(x.endswith(("-shared", "-persistent")) for x in classes)
    if sliced:
        sec = c.device_section(cl, r.get("driver"), r.get("request"))
        mode = c.device_mode(sec)
        mem = c.quantity((r.get("consumedCapacity") or {}).get("memory"))
        words = [mode] + (["ro"] if mode == "dax" and c.device_read_only(sec) else []) + \
            ([c.k8s_size(mem)] if mem else [])
        return " ".join(words + [device_word(r, state)])
    return "%s %s" % (c.lease_memory(cl), device_word(r, state))


def render_t2(data, state, now=None, stale=False):
    k8s = (data.get("vm2") or {}).get("k8s") or {}
    lines = [c.title("PODS & CLAIMS", stale, now), "POD    NOD STATE   CLAIM    MEMORY"]
    claims = c.items(k8s.get("claims"), "ResourceClaim")
    rows = []
    for p in wizard_pods(k8s):
        name = c.meta(p).get("name") or "?"
        node = c.short_node((p.get("spec") or {}).get("nodeName"), state)
        state_word = "Exiting" if c.deleting(p) else ((p.get("status") or {}).get("phase") or "?")[:7]
        head = "%-6s %-3s %-7s " % (name[:6], node[:3], state_word)
        kind, mine = pod_claims(p, claims)
        if kind == "claim":
            rows.append(head + "%-8s %s" % ("claim", claim_memory(mine[0], state) if mine else "no claim"))
            continue
        if not mine:
            rows.append(head + "%-8s %s" % ("-", "-"))
        for i, cl in enumerate(mine):
            rows.append((head if i == 0 else " " * len(head)) + "%-8s %s" % (c.lease_id(cl)[:8], claim_memory(cl, state)))
    if not rows:
        rows = ["no pods yet: demo wizard"]
    return lines + c.fit(rows, c.CONTENT_ROWS - 1)


def driver_status(drivers_doc, state):
    words = []
    pods = c.items(drivers_doc, "Pod")
    for n in [state.get("VM2_NODE"), state.get("VM1_NODE")]:
        if not n:
            continue
        mine = [p for p in pods if (p.get("spec") or {}).get("nodeName") == n]
        st = "down"
        if mine:
            p = mine[-1]
            cs = (p.get("status") or {}).get("containerStatuses") or []
            if c.deleting(p):
                st = "restarting"
            elif (p.get("status") or {}).get("phase") == "Running" and cs and all(x.get("ready") for x in cs):
                st = "ok"
            else:
                st = "restarting" if any(x.get("restartCount") for x in cs) else "starting"
        words.append("%s %s" % (c.short_node(n, state), st))
    return "drivers: " + "  ".join(words)


def memdevs(slices_doc, state):
    """Return [(driver, pool, where, device, label)] of the published memdevs: local first, then pool."""
    lp = (state.get("LP_SERIAL") or "").lower()
    out = []
    for drv, pool, where, d in c.slice_devices(slices_doc):
        if c.device_attr(d, "type") != "memdev":
            continue
        serial = (c.device_attr(d, "serial") or "").lower()
        if where:
            label = "lp0" if lp and serial == lp else serial[2:]
            label += " @" + c.short_node(where, state)
        else:
            label = d.get("name") + " @pool"
        out.append((drv, pool, where, d, label))
    return sorted(out, key=lambda e: (e[2] is None, c.device_attr(e[3], "persistent") is True, e[4]))


def holder_words(entry, claims):
    """Return the holder words of a device ENTRY: "w1 256Mi" of a slice, "w3 ro share" of a device with shares."""
    drv, pool, _, d, _ = entry
    words = []
    for cl in claims:
        res = [r for r in c.results(cl) if (r.get("driver"), r.get("pool"), r.get("device")) == (drv, pool, d.get("name"))]
        if not res or attach_failed(cl):
            continue
        r = res[0]
        who = c.holder_name(cl)
        if c.attaching(cl):
            words.append("%s attaching" % who)
        elif c.device_shares(d) is not None:
            ex, ro = c.holding(cl, r, d)
            words.append("%s %s %s" % (who, "ro" if ro else "rw", "excl" if ex else "share"))
        elif d.get("allowMultipleAllocations"):
            mem = c.quantity((r.get("consumedCapacity") or {}).get("memory"))
            words.append("%s %s" % (who, c.k8s_size(mem) if mem else "whole"))
        else:
            words.append(who)
        if c.deleting(cl):
            words[-1] += " releasing"
    return sorted(words)


def render_t3(data, state, now=None, stale=False):
    k8s = (data.get("vm2") or {}).get("k8s") or {}
    lines = [c.title("SCHEDULER: DEVICES AND CLAIMS", stale, now)]
    pool_slices = [s for s in c.items(k8s.get("slices"))
                   if (s.get("spec") or {}).get("driver") == "cxl-pool.nri.io" and not (s.get("spec") or {}).get("nodeName")]
    rows = [driver_status(k8s.get("drivers"), state) + "  pool %s" % ("ok" if pool_slices else "down")]
    claims = c.items(k8s.get("claims"), "ResourceClaim")
    devs = memdevs(k8s.get("slices"), state)
    for e in devs:
        d = e[3]
        kind = "pers" if c.device_attr(d, "persistent") is True else ("shared" if c.device_attr(d, "shared") is True else "")
        head = "%s %s%s: " % (e[4], c.k8s_size(c.device_attr(d, "size")), " " + kind if kind else "")
        rows += c.wrap_list(head, holder_words(e, claims) or ["-"], indent=4)
    if not devs:
        rows.append("no memory device published (yet)")
    waiting = []
    for cl in claims:
        if c.deleting(cl) or not (c.lease_id(cl) or c.labels(cl).get("cxl-demo/wizard")):
            continue
        if c.subrequests(cl) and all((s.get("deviceClassName") or "") == "cxl-lease" for s in c.subrequests(cl)):
            continue
        why = attach_failed(cl)
        if c.allocation(cl) and not why:
            continue
        serials = c.claim_serials(cl)
        named = sorted({e[4].split(" @")[0] for e in devs if (c.device_attr(e[3], "serial") or "").lower() in serials})
        word = c.holder_name(cl)
        if why or named:
            word += " (%s)" % ", ".join(([why] if why else []) + named)
        waiting.append(word)
    if waiting:
        rows += c.wrap_list("waiting: ", sorted(waiting), indent=2)
    return lines + c.fit(rows)


def region_words(r):
    """Return a region in a few words: its name and size, or of pmem its block devices with filesystem and ro/rw."""
    if r.get("mode") == "pmem":
        out = [r["name"]]
        for n in r.get("namespaces") or []:
            if not n.get("block"):
                out.append("%s no block" % n.get("name"))
                continue
            fs = {None: "?", "?": "?", "none": "no fs"}.get(n.get("fs"), n.get("fs"))
            out.append("%s %s%s" % (n["block"], fs, "" if n.get("ro") is None else " " + ("ro" if n["ro"] else "rw")))
        return " ".join(out + ([] if r.get("namespaces") else ["no namespace"]))
    return "%s %s" % (r["name"], c.kernel_size(r.get("size")))


def slice_words(r):
    """Return the dax devices of a ram region in words: "kmem node2 512M" (their sum per node), "dax0.3 256M", or []."""
    out, kmem = [], {}
    for x in r.get("dax") or []:
        drv = x.get("driver") or "no driver"
        if drv == "kmem":
            kmem[x.get("targetNode")] = kmem.get(x.get("targetNode"), 0) + (x.get("size") or 0)
        elif drv == "device_dax":
            out.append("%s %s" % (x["name"], c.kernel_size(x.get("size"))))
        else:
            out.append("%s %s %s" % (x["name"], c.kernel_size(x.get("size")), drv))
    return ["kmem node%s %s" % (n, c.kernel_size(b)) for n, b in sorted(kmem.items(), key=lambda kv: str(kv[0]))] + out


def node_rows(label, node, names, claim_pods):
    """Return the rows of a node: one per memdev with its regions, then who uses them."""
    if node is None:
        return ["%s unreachable" % label]
    devs = node.get("memdevs") or []
    if not devs:
        return ["%s no CXL memory" % label]
    rows = []
    regions = node.get("regions") or []
    for i, d in enumerate(devs):
        mine = [r for r in regions if d["name"] in (r.get("memdevs") or [])]
        head = "%s %s %s %s" % (label if i == 0 else " " * len(label), d["name"], (d.get("serial") or "0x?")[2:],
                                c.kernel_size(d.get("size")))
        row = head + " " + ("; ".join(region_words(r) for r in mine) if mine else "no region")
        words = [w for r in mine if r.get("mode") != "pmem" for w in slice_words(r)]
        if words and len(row) + 1 + len(", ".join(words)) <= c.WIDTH:
            rows.append(row + " " + ", ".join(words))
        else:
            rows.append(row)
            if words:
                rows += c.wrap_list("    dax: ", [x + "," for x in words[:-1]] + [words[-1]], indent=6, sep="")
    used = {}
    for m in node.get("mappers") or []:
        who = names.get(m.get("podUID")) or "pid %s" % m.get("pid")
        used.setdefault(who, set()).add("rw" if "w" in (m.get("perms") or "") else "ro")
    for o in node.get("openers") or []:
        who = names.get(o.get("podUID")) or "pid %s" % o.get("pid")
        used.setdefault(who, set()).add(o.get("perms") or "ro")
    words = ["%s %s" % (who, "rw" if "rw" in how else "ro") for who, how in sorted(used.items())]
    for m in node.get("mounts") or []:
        if m.get("kind") == "lease":
            words.append("%s fs %s" % (claim_pods.get(m.get("claimUID")) or (m.get("claimUID") or "?")[:8],
                                       "ro" if m.get("mountRO") is not False else "rw"))
        elif m.get("kind") == "node":
            words.append("mount %s %s" % (m.get("fstype"), "ro" if m.get("ro") else "rw"))
    if words:
        rows += c.wrap_list("    used: ", words, indent=6)
    return rows


def render_t4(data, state, now=None, stale=False):
    import views
    vm2, vm1 = data.get("vm2") or {}, data.get("vm1") or {}
    k8s = vm2.get("k8s") or {}
    names, claim_pods = views.pod_names(k8s.get("pods")), views.lease_claim_pods(k8s)
    rows = []
    for label, d in (("vm2", vm2), ("vm1", vm1)):
        rows += node_rows(label, d.get("node") if d else None, names, claim_pods)
    return [c.title("NODES: CXL IN THE KERNEL", stale, now)] + c.fit(rows)


def backing_fs(path):
    """Return the filesystem on the block device in the backing file PATH of a persistent device, in a few words.

    One of "no file", "zeros", "no filesystem", "signatures of A, B",
    "erofs", "xfs", "btrfs", "ext4 clean", "ext4 needs recovery" (mounted
    read-write now, or not unmounted cleanly), "ext4 not clean".
    """
    try:
        with open(path, "rb") as f:
            _, data = c.read_fsdax(f, fsinfo.READ_LEN)
    except OSError:
        return "no file"
    sb = fsinfo.fs_super(data)
    t = sb["type"]
    if t == "none":
        return "no filesystem"
    if t == "several":
        return "signatures of " + ", ".join(sb["signatures"])
    if t == "ext4":
        return "ext4 " + ("needs recovery" if sb.get("needsRecovery") else ("clean" if sb.get("clean") else "not clean"))
    return t


def collect_t6(state):
    """Return the data of T6: the devices of the pool server and lp0 of local-pmem, the filesystems of the persistent ones."""
    out = {"devices": [], "fs": {}}
    try:
        out["devices"] = [d for d in c.http_json("%s/api/v1/devices" % state["POOL_URL"]) or [] if d.get("scope") != "local"]
    except (OSError, ValueError) as e:
        out["error"] = str(e)
    if state.get("LP_URL"):
        try:
            out["lp"] = c.http_json("%s/api/v1/devices/%s" % (state["LP_URL"], state.get("LP_DEVICE", "lp0")))
        except (OSError, ValueError) as e:
            out["lpError"] = str(e)
    for d in out["devices"] + ([out["lp"]] if out.get("lp") else []):
        if d.get("persistent") and d.get("path"):
            out["fs"][d["name"]] = backing_fs(d["path"])
    if "error" in out and "lp" not in out:
        raise RuntimeError(out["error"])
    return {"pool": out}


def attached_words(dev, state):
    atts = dev.get("attachments") or []
    words = []
    for a in sorted(atts, key=lambda a: a.get("host") or ""):
        w = c.short_node(a.get("host"), state)
        if a.get("state") != "attached":
            w += " " + (a.get("state") or "?")
        if a.get("singleHost"):
            w += " exclusive"
        words.append(w)
    return words


def render_t6(data, state, now=None, stale=False):
    pool = data.get("pool") or {}
    lines = [c.title("POOL SERVERS: fake-cxl-pool, local-pmem", stale, now)]
    rows = []
    if pool.get("error"):
        rows.append("fake-cxl-pool-server: " + pool["error"])
    devs = list(pool.get("devices") or [])
    if pool.get("lp"):
        devs.append(dict(pool["lp"], server="local-pmem"))
    elif pool.get("lpError"):
        rows.append("local-pmem: " + pool["lpError"])
    for d in devs:
        kind = c.device_kind(d, "local" if d.get("server") else "pool")
        rows.append("%s %s %s  %s" % (d.get("name"), c.k8s_size(d.get("size")), kind, d.get("state") or "?"))
        words = attached_words(d, state) or ["nowhere"]
        fs = (pool.get("fs") or {}).get(d.get("name"))
        rows += c.wrap_list("  attached: ", [w + "," for w in words[:-1]] + [words[-1] + (";" if fs else "")] +
                            ([fs] if fs else []), indent=4, sep="")
    if not devs and not rows:
        rows.append("no pool devices")
    return lines + c.fit(rows)

