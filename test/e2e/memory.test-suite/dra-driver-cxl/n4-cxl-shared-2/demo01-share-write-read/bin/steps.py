#!/usr/bin/env python3
"""The step runner of the presenter terminal (T1).

Usage: steps.py local|pool|pool-shared-fs|pool-fs|llm-share [N | N- | N-M | --auto] [--no-color]

Each step prints its caption and its command as typed, waits for a key
(Enter run, s skip, z zoom, q quit), runs the command in VM2 (in
/root/demo, with the kubeconfig of the demo namespace) or, for the steps
that look at the host (llm-share), on the host with bin/ in PATH, and
prints a one to four line result and how long it took. "N" runs step N,
"N-" the steps from N on, "N-M" the steps N to M, nothing all of them; any
other argument is refused with a message. "--auto" runs the steps
selected with short pauses (DEMO_AUTO_PAUSE seconds, default 2) and prints
the timings. The full output of every step goes to run/steps.log, the
timings of --auto to run/timings.txt; run/step.env names the step that
ran last and when it started (demo truth dumps the logs since then).
"""
import json
import os
import re
import subprocess
import sys
import time

import common as c
import llm

READERS = ["reader-a", "reader-b", "reader-c"]
LLM_READERS = llm.READERS
CASES = ("local", "pool", "pool-shared-fs", "pool-fs", "llm-share")
FLAGS = ("--auto", "--no-color", "--color")   # the arguments after the case that select no step


class Step:
    def __init__(self, caption, command, check=None, expect_fail=False, rows=3, host=False, timeout=300):
        self.caption = caption
        self.command = command
        self.check = check
        self.expect_fail = expect_fail
        self.rows = rows         # the terminal rows of the result
        self.host = host         # the command runs on the host, not in VM2
        self.timeout = timeout   # seconds of the command at most


def steps(case, serial, fstype="ext4", access="direct", repeat=3):
    """Return the steps of CASE (the stories of docs/memory/cxl/demo.md), the device SERIAL, pool-fs's FSTYPE and llm-share's first ACCESS and bench REPEAT in the commands."""
    S = serial
    if case == "pool-fs":
        return poolfs_steps(S, fstype)
    if case == "llm-share":
        return llm_steps(S, access, repeat)
    if case == "pool-shared-fs":
        return fs_steps(S)
    where = "vm1" if case == "pool" else "vm2"
    return [
        Step("a writer starts on %s; it has no CXL memory yet" % where,
             "kubectl apply -f %s/writer.yaml" % case, check_running(["writer"])),
        Step("the writer asks for the whole device, exclusively",
             "kubectl exec writer -- cxl-request request --serial %s --mode dax" % S, check_lease),
        Step("the writer writes a line into the device",
             'kubectl exec writer -- /demo/write "Hello from the writer, $(date +%T)"', check_last_line),
        Step("three readers start" + (": two on vm2, one on vm1" if case == "pool" else ""),
             "kubectl apply -f %s/readers.yaml" % case, check_running(READERS)),
        Step("readers ask for a read-only share: they must wait",
             "for r in reader-a reader-b reader-c; do kubectl exec $r -- cxl-request request --serial %s --share --read-only --wait 0s; done" % S,
             check_leases),
        Step("the writer is done and releases the device",
             "kubectl exec writer -- cxl-request release --all", check_readers_get_it),
        Step("each reader reads the line and keeps it mapped",
             "for r in reader-a reader-b reader-c; do kubectl exec $r -- /demo/read; kubectl exec $r -- /demo/hold; done",
             check_reads),
        Step("a reader tries to write: the kernel refuses",
             'kubectl exec reader-a -- /demo/write "overwrite"', check_last_line, expect_fail=True),
        Step("everything goes away", "kubectl delete -f %s/" % case, check_gone),
    ]


def fs_steps(S):
    """Return the steps F1-F9 of pool-shared-fs, the device serial S in the commands."""
    case = "pool-shared-fs"
    fs = "/run/cxl/fs/hello"
    return [
        Step("a writer starts on vm1; it has no CXL memory yet",
             "kubectl apply -f %s/writer.yaml" % case, check_running(["writer"])),
        Step("the writer asks for the device as a block device",
             "kubectl exec writer -- cxl-request request --serial %s --mode block" % S, check_lease),
        Step("the writer makes a file system with hello.txt",
             'kubectl exec writer -- /demo/mkfs "Hello from the writer, $(date +%T)"', check_last_line),
        Step("three readers start: two on vm2, one on vm1",
             "kubectl apply -f %s/readers.yaml" % case, check_running(READERS)),
        Step("readers ask for the file system: they must wait",
             "for r in reader-a reader-b reader-c; do kubectl exec $r -- cxl-request request --serial %s --share --read-only --mode fs --name hello --wait 0s; done" % S,
             check_leases),
        Step("the writer is done and releases the device",
             "kubectl exec writer -- cxl-request release --all", check_readers_get_it),
        Step("each reader reads hello.txt from its mount",
             "for r in reader-a reader-b reader-c; do kubectl exec $r -- cat %s/hello.txt; done" % fs, check_cat),
        Step("a reader tries to write: read-only file system",
             "kubectl exec reader-a -- sh -c 'echo overwrite > %s/hello.txt'" % fs, check_last_line, expect_fail=True),
        Step("everything goes away", "kubectl delete -f %s/" % case, check_gone),
    ]


def poolfs_steps(S, fstype):
    """Return the steps R1-R11 of pool-fs, the device serial S and the filesystem type FSTYPE in the commands."""
    case = "pool-fs"
    fs = "/run/cxl/fs/data"
    return [
        Step("a writer starts on vm1; it has no CXL memory yet",
             "kubectl apply -f %s/writer.yaml" % case, check_running(["writer"])),
        Step("the writer asks for the device as a block device",
             "kubectl exec writer -- cxl-request request --serial %s --mode block" % S, check_lease),
        Step("the writer makes an empty %s filesystem on it" % fstype,
             "kubectl exec writer -- /demo/mkfs --type %s" % fstype, check_last_line),
        Step("the writer mounts it read-write instead",
             "kubectl exec writer -- sh -c 'cxl-request release --all && cxl-request request --serial %s --share --mode fs "
             "--read-only=false --name data'" % S, check_fs_writer),
        Step("the writer writes hello.txt",
             'kubectl exec writer -- /demo/fswrite "Hello from the writer, $(date +%T)"', check_last_line),
        Step("three readers start: two on vm2, one on vm1",
             "kubectl apply -f %s/readers.yaml" % case, check_running(READERS)),
        Step("readers ask to read: vm1's reader at once, vm2's must wait",
             "for r in reader-a reader-b reader-c; do kubectl exec $r -- cxl-request request --serial %s --share --mode fs "
             "--name data --wait 0s; done" % S, check_join, rows=2),
        Step("the writer adds a line; vm1's reader sees it at once",
             'kubectl exec writer -- /demo/fswrite --append "A second line, $(date +%%T)"; '
             "kubectl exec reader-c -- cat %s/hello.txt" % fs, check_append),
        Step("the writer is done: vm1 turns the filesystem read-only, vm2's readers mount it",
             "kubectl exec writer -- cxl-request release --all", check_writer_leaves),
        Step("each reader reads hello.txt; a write fails",
             "for r in reader-a reader-b reader-c; do kubectl exec $r -- cat %s/hello.txt; done; "
             "kubectl exec reader-a -- sh -c 'echo x >> %s/hello.txt'" % (fs, fs), check_read_refused, expect_fail=True),
        Step("everything goes away", "kubectl delete -f %s/" % case, check_gone),
    ]


# llm-share: what the readers' access means, in the captions.
LLM_ACCESS_WORDS = {"direct": "direct access, the model mapped from the device",
                    "cached": "page-cached access, a copy in each VM"}
LLM_MEM_CAPTIONS = {"direct": "the host has the model once: every VM maps the same pages",
                    "cached": "every VM has its own copy of the model in its page cache"}


def llm_steps(S, access, repeat):
    """Return the steps M1-M15 of llm-share, the device serial S, the readers' first ACCESS (direct or cached) and the bench REPEAT in the commands."""
    case = "llm-share"
    other = "cached" if access == "direct" else "direct"
    bench = "kubectl exec client -- /demo/llm bench %%s --repeat %d" % repeat
    return [
        Step("a writer starts on vm1; it has no CXL memory yet",
             "kubectl apply -f %s/writer.yaml" % case, check_running(["writer"])),
        Step("the writer asks for llm0 as a block device",
             "kubectl exec writer -- cxl-request request --serial %s --mode block" % S, check_lease),
        Step("the writer fetches the model and writes an erofs of it into llm0",
             "kubectl exec writer -- /demo/mkfs --model", check_model, timeout=1800),
        Step("the writer is done and releases the device",
             "kubectl exec writer -- cxl-request release --all", check_released),
        Step("a reader starts on every VM: llama-server on a read-only share of llm0, %s" % access,
             "kubectl apply -f %s/model-%s.yaml -f %s/readers.yaml -f %s/client.yaml" % (case, access, case, case),
             check_readers(access), rows=2),
        Step("a client asks each reader, as an agent asks its model: the model calls a tool",
             "kubectl exec client -- /demo/llm ask", check_answers, rows=3),
        Step("each reader alone: time to first token, per token (%s)" % access,
             bench % "single", check_bench(access), rows=4, timeout=1800),
        Step("all readers at once (%s)" % access,
             bench % "simultaneous", check_bench(access), rows=4, timeout=1800),
        Step(LLM_MEM_CAPTIONS[access], "demo llm mem %s" % access, check_mem, rows=4, host=True),
        Step("the readers start again with %s" % LLM_ACCESS_WORDS[other],
             "kubectl delete -f %s/readers.yaml -f %s/model-%s.yaml && kubectl apply -f %s/model-%s.yaml -f %s/readers.yaml"
             % (case, case, access, case, other, case), check_readers(other), rows=2),
        Step("the client asks each reader again (%s)" % other,
             "kubectl exec client -- /demo/llm ask", check_answers, rows=3),
        Step("each reader alone (%s)" % other, bench % "single", check_bench(other), rows=4, timeout=1800),
        Step("all readers at once (%s)" % other, bench % "simultaneous", check_bench(other), rows=4, timeout=1800),
        Step(LLM_MEM_CAPTIONS[other], "demo llm mem %s --since %s" % (other, access), check_mem, rows=4, host=True),
        Step("everything goes away", "kubectl delete -f %s/ --ignore-not-found" % case, check_gone_llm),
    ]


def step_letter(case):
    """Return the letter of the step numbers of CASE: F for pool-shared-fs, R for pool-fs, M for llm-share, L for the others."""
    return {"pool-shared-fs": "F", "pool-fs": "R", "llm-share": "M"}.get(case, "L")


###
### Checks: what the step did, in one or two lines
###

def k8s(state, kinds="pods,claims"):
    return (c.run_probe(state["VM2_DIR"], ["--k8s", kinds, "--ns", state["NS"]], timeout=8) or {}).get("k8s") or {}


def wait_for(state, cond, timeout, kinds="pods,claims"):
    """Return (value, seconds) of the first true COND(k8s objects of KINDS) within TIMEOUT, (None, seconds) else."""
    t0 = time.time()
    while True:
        try:
            v = cond(k8s(state, kinds))
        except RuntimeError:
            v = None
        if v or time.time() - t0 > timeout:
            return v, time.time() - t0
        time.sleep(1)


def check_running(names):
    def check(state, out, rc):
        def cond(k):
            pods = {c.meta(p)["name"]: p for p in c.items(k.get("pods"), "Pod")}
            if all((pods.get(n, {}).get("status") or {}).get("phase") == "Running" for n in names):
                return ", ".join("%s %s" % (n, c.short_node(pods[n]["spec"].get("nodeName"), state)) for n in names)
            return None
        v, t = wait_for(state, cond, 180)
        if not v:
            return ["not Running after %.0f s" % t] + last_lines(out, 1)
        return ["Running: " + v, "(%.1f s until Running)" % t]
    return check


def responses(out):
    found = []
    for line in out.splitlines():
        line = line.strip()
        if line.startswith("{"):
            try:
                found.append(json.loads(line))
            except ValueError:
                pass
    return found


def lease_words(r):
    if r.get("error"):
        return "refused: " + r["error"]
    words = [r.get("lease", "?"), r.get("state", "?")]
    if r.get("mode"):
        words.append(r["mode"])
    words.append("ro" if r.get("readOnly") else "rw")
    if r.get("state") == "Ready":
        words.append("(share)" if r.get("share") else "(exclusive)")
    # "-> lease " goes before the words in T1.
    if r.get("mode") == "fs" and r.get("access") and len(" ".join(words + ["(%s)" % r["access"]])) + 9 <= c.WIDTH:
        words.append("(%s)" % r["access"])
    return " ".join(words)


def check_lease(state, out, rc):
    rs = responses(out)
    if not rs:
        return last_lines(out, 2)
    r = rs[-1]
    second = next(("%s %s" % (k, r[k]) for k in ("dax", "block", "path") if r.get(k)), None)
    return ["lease " + lease_words(r)] + ([second] if second else [])


def check_leases(state, out, rc):
    rs = responses(out)
    if not rs:
        return last_lines(out, 2)
    words = ["%s %s" % (n, lease_words(r).split(" ")[1] if not r.get("error") else "refused")
             for n, r in zip(READERS, rs)]
    return c.wrap_list("", words, indent=0)[:2]


def check_last_line(state, out, rc):
    return last_lines(out, 1)


def check_readers_get_it(state, out, rc):
    rs = responses(out)
    if rs and rs[-1].get("leases") is not None:
        first = ["released: " + (", ".join("%s %s" % (l.get("lease", "?"), l.get("state", "?"))
                                          for l in rs[-1]["leases"]) or "no leases")]
    elif rs:
        first = ["release: " + lease_words(rs[-1])]
    else:
        first = last_lines(out, 1)

    def cond(k):
        # A companion pod runs once its claim is allocated and prepared
        # (for the pool: attached too): the lease is Ready then.
        comps = [p for p in c.items(k.get("pods"), "Pod") if c.lease_id(p) and c.requester(p) in READERS]
        if len(comps) >= len(READERS) and all((p.get("status") or {}).get("phase") == "Running" for p in comps):
            return True
        return None
    v, t = wait_for(state, cond, 180)
    return first + ["readers' leases Ready after %.1f s" % t if v else "readers still waiting after %.0f s" % t]


def check_reads(state, out, rc):
    reads = [l for l in out.splitlines() if l.startswith("read at 0:")]
    holds = [l for l in out.splitlines() if l.startswith("holding")]
    if not reads:
        return last_lines(out, 2)
    same = len(set(reads)) == 1
    return ["%d readers %s" % (len(reads), reads[0] if same else "read different bytes!"),
            "%d hold a read-only mapping" % len(holds)]


def check_cat(state, out, rc):
    texts = last_lines(out, len(READERS))
    if len(texts) == len(READERS) and len(set(texts)) == 1:
        return ['%d readers read: "%s"' % (len(texts), texts[0])]
    return ["readers read different texts!"] + texts[-1:]


def lease_claims(k, names):
    """Return requester -> its newest lease companion claim of the k8s objects K, of the pods NAMES."""
    out = {}
    for cl in sorted(c.items(k.get("claims"), "ResourceClaim"), key=lambda cl: c.meta(cl).get("creationTimestamp") or ""):
        if c.lease_id(cl) and c.requester(cl) in names:
            out[c.requester(cl)] = cl
    return out


def check_fs_writer(state, out, rc):
    """Return the writer's read-write fs lease and the node that has the device for it alone (ReadWriteOnce)."""
    rs = responses(out)
    if not rs or rs[-1].get("leases") is not None:
        return last_lines(out, 2)
    r = rs[-1]
    if r.get("error"):
        return ["refused: " + r["error"]]
    first = "lease %s %s %s %s (%s)" % (r.get("lease", "?"), r.get("state", "?"), r.get("fsType") or "fs",
                                        "ro" if r.get("readOnly") else "rw", r.get("access") or "?")

    def cond(k):
        cl = lease_claims(k, ["writer"]).get("writer")
        return c.single_host(cl) if cl and c.lease_id(cl) == r.get("lease") else None
    host, t = wait_for(state, cond, 30)
    where = "attached to %s only (ReadWriteOnce)" % c.short_node(host, state) if host else "not attached to one node only after %.0f s" % t
    return [first, "path %s" % r.get("path", "?"), where]


def check_join(state, out, rc):
    """Return which readers' leases are Ready next to the writer and which wait, with the reason."""
    if len(responses(out)) < len(READERS):
        return last_lines(out, 2)

    def cond(k):
        pods = {c.meta(p)["name"]: p for p in c.items(k.get("pods"), "Pod")}
        node = (pods.get("writer", {}).get("spec") or {}).get("nodeName")
        comps = {c.requester(p): p for p in pods.values() if c.lease_id(p)}
        claims = lease_claims(k, READERS)
        near = [r for r in READERS if (pods.get(r, {}).get("spec") or {}).get("nodeName") == node]
        far = [r for r in READERS if r not in near]
        ready = all((comps.get(r, {}).get("status") or {}).get("phase") == "Running" for r in near)
        why = {r: c.attach_failure(claims[r], k.get("events")) if r in claims else None for r in far}
        if ready and all(why.values()):
            return near, why, node
        return None
    v, t = wait_for(state, cond, 120, "pods,claims,events")
    if not v:
        return ["not as expected after %.0f s: zoom pod reader-a" % t]
    near, why, node = v
    reasons = sorted(set(why.values()))
    return ["%s Ready on %s, next to the writer" % (" ".join(near), c.short_node(node, state)),
            "%s waiting: %s (%.1f s)" % (" ".join(sorted(why)), ", ".join(reasons), t)]


def check_append(state, out, rc):
    """Return whether the reader of the writer's node reads the line that the writer appended."""
    lines = last_lines(out, 1000)
    m = next((re.match(r'^appended to \S+: "(.*)"$', l) for l in lines if l.startswith("appended to ")), None)
    if not m:
        return lines[-2:]
    if lines[-1] == m.group(1):
        return ['reader-c reads it at once: "%s"' % m.group(1)]
    return ["reader-c does not read the new line:", lines[-1]]


def check_writer_leaves(state, out, rc):
    """Return the release, when the readers' leases are Ready, and the read-only filesystem of the writer's node."""
    result = check_readers_get_it(state, out, rc)
    try:
        node = (c.run_probe(state["VM1_DIR"], ["--node"], timeout=8) or {}).get("node") or {}
    except RuntimeError as e:
        return result + ["vm1: %s" % e]
    words = []
    for r in node.get("regions") or []:
        for n in r.get("namespaces") or []:
            for m in node.get("mounts") or []:
                if m.get("block") == n.get("block") and m.get("kind") == "node":
                    words.append("%s %s" % (m.get("fstype"), "read-only" if m.get("ro") else "read-write"))
            if n.get("ro") is not None:
                words.append("%s %s" % (n.get("block"), "read-only" if n["ro"] else "read-write"))
    return result + ["vm1: " + (", ".join(words) or "no filesystem mounted")]


def check_read_refused(state, out, rc):
    """Return whether every reader read the same lines and the write was refused."""
    lines = last_lines(out, 1000)
    if "Read-only file system" not in lines[-1]:
        return ["the write was not refused:"] + lines[-1:]
    texts = lines[:-1]
    n = len(texts) // len(READERS)
    parts = [texts[i * n:(i + 1) * n] for i in range(len(READERS))]
    same = n > 0 and len(texts) == n * len(READERS) and all(p == parts[0] for p in parts)
    return ["%d readers read the same %d lines" % (len(READERS), n) if same else "readers read different texts!", lines[-1]]


def check_gone(state, out, rc):
    def cond(k):
        return not c.items(k.get("pods"), "Pod") and not [cl for cl in c.items(k.get("claims")) if c.lease_id(cl)]
    v, t = wait_for(state, cond, 180)
    return ["pods and leases gone after %.1f s" % t if v else "still there after %.0f s" % t]


def check_model(state, out, rc):
    """Return what the writer fetched, how long the fetch, the erofs and the write took, and the change of the VM's page cache."""
    lines = last_lines(out, 1000)
    got = {}
    for l in lines:
        for key, pat in (("before", r"^page cache of the VM before the fetch: (-?\d+) MiB$"),
                         ("fetch", r"^fetched \S+: (\d+) MiB in ([\d.]+) s \((\d+) MB/s\), sha256 ok$"),
                         ("mkfs", r"^made an erofs of it: (\d+) MiB in ([\d.]+) s$"),
                         ("write", r"^streamed \S+ through the fd of the lease: (\d+) bytes in ([\d.]+) s$"),
                         ("after", r"^removed \S+: page cache of the VM now (-?\d+) MiB$")):
            m = re.match(pat, l)
            if m:
                got[key] = m.groups()
    if len(got) < 5:
        return lines[-2:]
    before, after = int(got["before"][0]), int(got["after"][0])
    return ["fetched %s MiB in %s s (%s MB/s), sha256 ok" % got["fetch"],
            "erofs of %s MiB in %s s, written in %s s" % (got["mkfs"][0], got["mkfs"][1], got["write"][1]),
            "VM page cache %+d MiB (%d -> %d MiB)" % (after - before, before, after)]


def pool_attachments(state):
    """Return the hosts that the pool device of the demo is attached to, as vm0, vm1, vm2."""
    dev = c.http_json("%s/api/v1/devices/%s" % (state["POOL_URL"], state.get("POOL_DEVICE", "llm0")))
    return sorted(c.short_node(a.get("host"), state) for a in dev.get("attachments") or [])


def wait_detached(state, timeout):
    """Return (True, seconds) once the pool device of the demo is attached nowhere, (False, seconds) after TIMEOUT."""
    t0 = time.time()
    while True:
        try:
            if not pool_attachments(state):
                return True, time.time() - t0
        except (OSError, ValueError):
            pass
        if time.time() - t0 > timeout:
            return False, time.time() - t0
        time.sleep(1)


def check_released(state, out, rc):
    """Return the release and when the pool device is detached from the writer's node."""
    rs = responses(out)
    first = ["released: " + (", ".join("%s %s" % (l.get("lease", "?"), l.get("state", "?")) for l in rs[-1]["leases"])
                             or "no leases")] if rs and rs[-1].get("leases") is not None else last_lines(out, 1)
    gone, t = wait_detached(state, 90)
    return first + ["llm0 detached after %.1f s" % t if gone else "llm0 still attached after %.0f s" % t]


def reader_pods(k):
    """Return name -> pod of the readers of llm-share in the k8s objects K."""
    return {c.meta(p)["name"]: p for p in c.items(k.get("pods"), "Pod") if c.meta(p).get("name") in LLM_READERS}


def pod_ready(p):
    """Return True if the containers of pod P run and are ready."""
    cs = (p.get("status") or {}).get("containerStatuses") or []
    return not c.deleting(p) and (p.get("status") or {}).get("phase") == "Running" and bool(cs) and all(x.get("ready") for x in cs)


def check_readers(access):
    """Return a check of the readers with ACCESS: Ready (llama-server serves) on their nodes, llm0 attached to every VM."""
    def check(state, out, rc):
        def cond(k):
            pods = reader_pods(k)
            if len(pods) == len(LLM_READERS) and all(pod_ready(p) for p in pods.values()):
                if all(device_access(cl) == access for cl in reader_claims(k)):
                    return ", ".join("%s %s" % (n, c.short_node(pods[n]["spec"].get("nodeName"), state)) for n in LLM_READERS)
            return None
        v, t = wait_for(state, cond, 300)
        if not v:
            return ["readers not Ready after %.0f s" % t] + last_lines(out, 1)
        try:
            where = ", ".join(pool_attachments(state)) or "nowhere"
        except (OSError, ValueError) as e:
            where = "? (%s)" % e
        return ["Ready: %s" % v, "(%.1f s); llm0 attached to %s" % (t, where)]
    return check


def reader_claims(k):
    """Return the ResourceClaims of the readers of llm-share in the k8s objects K (from template llm-model)."""
    return [cl for cl in c.items(k.get("claims"), "ResourceClaim")
            if any((o.get("kind"), o.get("name")) == ("Pod", n) for o in c.meta(cl).get("ownerReferences") or [] for n in LLM_READERS)]


def device_access(claim):
    """Return the access of the fs claim CLAIM: direct (the default of erofs) or cached."""
    return c.device_section(claim).get("access") or "direct"


def check_answers(state, out, rc):
    """Return the answer of each reader in one row: its TTFT and the call of the tool (cut), or why there is none."""
    rows = []
    for l in last_lines(out, 1000):
        m = re.match(r"^(reader-\d+): (.*) \(TTFT ([\d.]+) s, \d+ tokens\)$", l)
        if m:
            row = "%s %s s: %s" % (m.group(1), m.group(3), m.group(2))
            rows.append(row if len(row) <= c.WIDTH - 3 else row[:c.WIDTH - 6] + "...")
        elif l.split(":", 1)[0] in LLM_READERS:
            rows.append(l)
    return rows or last_lines(out, 2)


def bench_result(out):
    """Return the JSON of the "bench: " line of OUT, None if there is none."""
    for line in out.splitlines():
        if line.startswith("bench: "):
            try:
                return json.loads(line[len("bench: "):])
            except ValueError:
                return None
    return None


def check_bench(access):
    """Return a check of a bench of readers with ACCESS: its line per reader, the result saved in run/llm-results/."""
    def check(state, out, rc):
        res = bench_result(out)
        if res is None:
            return last_lines(out, 2)
        llm.save_bench(state, res, access)
        rows = [l for l in out.splitlines() if l.split(" ", 1)[0] in LLM_READERS]
        return rows + ["saved: demo llm results prints them all"]
    return check


def mem_json(out):
    """Return the JSON of the "mem: " line of OUT, None if there is none."""
    for line in out.splitlines():
        if line.startswith("mem: "):
            try:
                return json.loads(line[len("mem: "):])
            except ValueError:
                return None
    return None


def check_mem(state, out, rc):
    """Return where the model is: once in the host, mapped by every VM; in each VM charged to its reader or not, and the change."""
    j = mem_json(out)
    if j is None:
        return last_lines(out, 2)
    snap, since = j["snapshot"], j.get("since")
    h, g = snap["host"], snap["guest"]
    maps = [v["mapping"] for v in h["vms"].values() if v.get("mapping")]
    rss = sorted(set(llm.mib(m.get("Rss")) for m in maps))
    pss = sorted(set(llm.mib(m.get("Pss")) for m in maps))
    out = ["host: %s of llm0, once; %s" % (llm.mib(h.get("fileResidentKb")), llm.pages_words(h.get("pmdShare"))),
           "%d qemus map it: Rss %s, Pss %s each" % (len(maps), "/".join(rss), "/".join(pss))]
    charged = [llm.mib((g.get(r) or {}).get("cgFileKb")) for r in LLM_READERS]
    out.append("page cache charged to readers: %s" % " ".join(charged))
    if since:
        cached = [llm.signed_mib(llm.delta(g.get(r), since["guest"].get(r), "cachedKb")) for r in LLM_READERS]
        anon = [llm.signed_mib(llm.delta(v, since["host"]["vms"].get(vm), "qemuRssAnonKb")) for vm, v in sorted(h["vms"].items())]
        out.append("VM Cached %s; qemu anon %s" % (" ".join(cached), " ".join(anon)))
    else:
        out.append("readers map the gguf: %s" % " ".join(llm.mib((g.get(r) or {}).get("ggufRssKb")) for r in LLM_READERS))
    return out


def check_gone_llm(state, out, rc):
    """Return when the pods and claims of llm-share are gone and llm0 is detached."""
    def cond(k):
        return not c.items(k.get("pods"), "Pod") and not c.items(k.get("claims"), "ResourceClaim")
    v, t = wait_for(state, cond, 180)
    if not v:
        return ["still there after %.0f s" % t]
    gone, t2 = wait_detached(state, 120)
    return ["pods and claims gone after %.1f s" % t, "llm0 detached after %.1f s more" % t2 if gone else "llm0 still attached"]


def last_lines(out, n):
    """Return the last N lines of OUT that say something (not kubectl's exit code note)."""
    lines = [l for l in out.splitlines() if l.strip() and not l.startswith("command terminated with exit code")]
    return lines[-n:] if lines else ["(no output)"]


###
### Running
###

def result_lines(result, rows=3):
    """Return the ROWS terminal lines of the RESULT lines: "-> " first, a long one continued after its last fitting ": " or space."""
    out = []
    for line in result:
        prefix = "-> "
        while line and len(out) < rows:
            room = c.WIDTH - len(prefix)
            if len(line) <= room:
                out.append(prefix + line)
                break
            cut = line.rfind(": ", 0, room)
            cut = cut + 1 if cut > 0 else line.rfind(" ", 0, room + 1)
            if cut <= 0:
                cut = room
            out.append(prefix + line[:cut].rstrip())
            line, prefix = line[cut:].lstrip(), "   "
    return [c.cut(l) for l in out[:rows]]


def wrap_command(cmd):
    """Return the command as typed, "$ " first, wrapped at 50 columns."""
    out, cur = [], "$"
    for w in cmd.split():
        if len(cur) + 1 + len(w) > c.WIDTH and cur.strip() != "$":
            out.append(cur)
            cur = "   "
        cur += " " + w
    out.append(cur)
    return out


def host_tz():
    """Return " TZ=<zone>" of the host for the VM's shell (its clock prints UTC otherwise)."""
    tz = os.environ.get("TZ")
    if not tz:
        link = os.path.realpath("/etc/localtime")
        tz = link.split("zoneinfo/", 1)[1] if "zoneinfo/" in link else ""
    return " TZ=%s" % tz if re.fullmatch(r"[A-Za-z0-9_+/-]+", tz or "") else ""


def run_host(command, timeout):
    """Return (exit status, output) of the bash COMMAND on the host, in the demo directory, with bin/ first in PATH."""
    env = dict(os.environ, PATH=c.BIN_DIR + os.pathsep + os.environ.get("PATH", ""))
    try:
        p = subprocess.run(["bash", "-c", command], cwd=c.DEMO_DIR, env=env, stdout=subprocess.PIPE,
                           stderr=subprocess.STDOUT, timeout=timeout)
    except subprocess.TimeoutExpired as e:
        return 124, (e.output or b"").decode(errors="replace") + "\n(timeout after %ds)" % timeout
    return p.returncode, p.stdout.decode(errors="replace")


def getkey():
    import termios
    import tty
    fd = sys.stdin.fileno()
    old = termios.tcgetattr(fd)
    try:
        tty.setcbreak(fd)
        return sys.stdin.read(1)
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old)


def log(text):
    os.makedirs(c.STATE_DIR, exist_ok=True)
    with open(os.path.join(c.STATE_DIR, "steps.log"), "a") as f:
        f.write(text)


def mark_step(label, start):
    """Write run/step.env: the step LABEL and its START (epoch seconds)."""
    os.makedirs(c.STATE_DIR, exist_ok=True)
    tmp = os.path.join(c.STATE_DIR, "step.env.tmp")
    with open(tmp, "w") as f:
        f.write("STEP=%s\nSTART=%.6f\n" % (label, start))
    os.replace(tmp, os.path.join(c.STATE_DIR, "step.env"))


def run_step(state, case, n, step, auto, color, total):
    sys.stdout.write("\033[H\033[2J")
    hdr = c.title("PRESENTER %s %s%d/%d" % (case, step_letter(case), n, total))
    lines = [hdr] + c.wrap_list("", step.caption.split(" "), indent=2, sep="") + wrap_command(step.command)
    for i, l in enumerate(lines):
        print((c.BOLD + l + c.RESET) if color and i == 0 else l)
    while True:
        if auto:
            time.sleep(float(os.environ.get("DEMO_AUTO_PAUSE", "2")))
            key = "\n"
        else:
            sys.stdout.write("Enter run, s skip, z zoom, q quit ")
            sys.stdout.flush()
            key = getkey()
            sys.stdout.write("\r\033[K")
        if key in ("q", "Q"):
            return "quit", 0
        if key in ("s", "S"):
            print("skipped")
            return "skip", 0
        if key in ("z", "Z"):
            sys.stdout.write("zoom: ")
            sys.stdout.flush()
            args = sys.stdin.readline().split()
            if args:
                subprocess.call([os.path.join(c.BIN_DIR, "demo"), "zoom"] + args)
            print("\n".join(lines[-3:]))
            continue
        if key in ("\n", "\r"):
            break
    t0 = time.time()
    mark_step("%s%d" % (step_letter(case), n), t0)
    if step.host:
        rc, out = run_host(step.command, step.timeout)
    else:
        script = "cd /root/demo && export KUBECONFIG=/root/demo/kubeconfig%s\n%s\n" % (host_tz(), step.command)
        rc, out = c.run_vm(state["VM2_DIR"], script, timeout=step.timeout)
    t_cmd = time.time() - t0
    log("=== %s %s %s%d rc=%d %.1fs\n$ %s\n%s\n" % (time.strftime("%H:%M:%S"), case, step_letter(case), n, rc, t_cmd, step.command, out))
    try:
        result = step.check(state, out, rc) if step.check else last_lines(out, 2)
    except Exception as e:   # a broken check must not stop the talk
        result = ["(check failed: %s)" % e] + last_lines(out, 1)
    t_all = time.time() - t0
    ok = (rc != 0) if step.expect_fail else (rc == 0)
    if not ok and not step.expect_fail:
        result = ["exit %d: " % rc + (last_lines(out, 1)[0])] + result[:1]
    for l in result_lines(result, step.rows):
        print(c.colorize(l) if color else l)
    sys.stdout.write("   %.1f s (command %.1f s)" % (t_all, t_cmd))
    sys.stdout.flush()
    log("result: %s (%.1f s)\n" % (" | ".join(result), t_all))
    return "ok" if ok else "fail", t_all


def select_steps(args, total):
    """Return (first, last), the numbers of the first and the last step to run of TOTAL steps, from the ARGS after the case.

    "N" is step N only, "N-" the steps from N on, "N-M" the steps N to M and
    nothing all of them. The flags may be anywhere. Raise ValueError, with a
    message that says what is wrong, for any other argument, for more than one
    selection and for a step that does not exist.
    """
    selections = []
    for a in args:
        if a in FLAGS:
            continue
        m = re.fullmatch(r"(\d+)(-(\d*))?", a)
        if not m:
            raise ValueError('cannot understand "%s": give N (step N), N- (from step N on), N-M (steps N to M), '
                             '--auto or --no-color' % a)
        selections.append(m)
    if len(selections) > 1:
        raise ValueError("give one selection of steps, not %s" % " and ".join('"%s"' % m.group(0) for m in selections))
    if not selections:
        return 1, total
    m = selections[0]
    first = int(m.group(1))
    last = first if m.group(2) is None else int(m.group(3)) if m.group(3) else total
    for n in (first, last):
        if not 1 <= n <= total:
            raise ValueError('"%s": step %d does not exist, the steps are 1-%d' % (m.group(0), n, total))
    if last < first:
        raise ValueError('"%s" selects no step: the last one is before the first' % m.group(0))
    return first, last


def main():
    args = sys.argv[1:]
    if not args or args[0] not in CASES:
        sys.exit(__doc__)
    case = args[0]
    state = c.load_state()
    if state is None:
        sys.exit("demo not set up: demo setup %s" % case)
    if state.get("DEMO_CASE") != case:
        sys.exit("the demo is set up for %s, not %s: demo teardown; demo setup %s" % (state.get("DEMO_CASE"), case, case))
    color = c.use_color(args)
    auto = "--auto" in args
    repeat = os.environ.get("DEMO_LLM_REPEAT") or state.get("DEMO_LLM_REPEAT") or "3"
    if not re.fullmatch(r"[1-9][0-9]*", repeat):
        sys.exit("steps.py %s: DEMO_LLM_REPEAT is %r: a number of repetitions" % (case, repeat))
    repeat = int(repeat)
    all_steps = steps(case, state["SERIAL"], state.get("DEMO_FSTYPE") or "ext4", state.get("DEMO_LLM_ACCESS") or "direct", repeat)
    try:
        first, last = select_steps(args[1:], len(all_steps))
    except ValueError as e:
        sys.exit("steps.py %s: %s" % (case, e))
    timings = []
    for n in range(first, last + 1):
        what, t = run_step(state, case, n, all_steps[n - 1], auto, color, len(all_steps))
        timings.append((n, what, t))
        if what == "quit":
            break
        if auto:
            print()
            time.sleep(float(os.environ.get("DEMO_AUTO_PAUSE", "2")) + 1)
        elif n < last:
            sys.stdout.write("; Enter: next ")
            sys.stdout.flush()
            if getkey() in ("q", "Q"):
                break
        else:
            print()
    if auto:
        summary = "%s %s: %s" % (time.strftime("%Y-%m-%d %H:%M:%S"), case,
                                 " ".join("%s%d=%.1fs%s" % (step_letter(case), n, t, "" if w == "ok" else "(%s)" % w)
                                          for n, w, t in timings))
        print(summary)
        with open(os.path.join(c.STATE_DIR, "timings.txt"), "a") as f:
            f.write(summary + "\n")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("\nstopped")
        sys.exit(130)
