"""The desk checks of the wizard (wizard.py check): the rules, the YAML,
the views of the case and the scripted set, against the samples of
testdata/wizard-*.

Usage: wizard.py check [--quiet] [--block]

- every rule row: a choice that the driver accepts gives the claim (class,
  capacity, selectors, MemoryConfig) or the cxl-request command that the
  driver documents; a choice that it refuses is refused with the reason;
- every YAML file: printable ASCII, the comment header of at most 79
  columns, labels app=cxl-demo-wizard and cxl-demo/wizard=NAME, read back
  equal to the objects;
- the questions of the interactive mode fit 50 columns;
- the views T2, T3, T4, T6 of the samples fit 50 x 10;
- every row of bin/wizard-rules.sh: accepted or refused as it expects, a
  wait that the samples' devices predict has its warning;
- the states that new and list print for the objects of wizard-W8;
- the commands of a claim's pod and of a lease end with "cxl-wizard
  exit N", which new waits for (run with the tool and a fake cxl-request);
- write and read of a dax mapping that refuses msync, as devdax does;
- mkfs of every type and read of the pod tool inside the demo image, on a
  device node and through fd 3 (when docker has the image);
- with --block: the same on a real block device (a loop device) in an
  unprivileged container, where mkfs.xfs on the node itself fails;
- every pod of the wizard (each rule row, the scripted set, --node any of
  a claim and a lease) and every Pod of manifests/ lands only on a node
  with the label cxl-demo/node (vm1, vm2): vm0 has no demo image;
- the state file read as setup writes it (printf %q; the file of a live
  setup, testdata/state-wizard-setup.env).
Runs every check; exit status 1 if one failed, after the report.
"""
import json
import os
import re
import shlex
import sys
import time

os.environ["COLUMNS"], os.environ["LINES"] = "50", "10"
os.environ["TZ"] = "UTC"
time.tzset()

import common as c        # noqa: E402
import wizard as w        # noqa: E402
import wizard_views as wv  # noqa: E402

TESTDATA = c.testdata()
RULES = os.path.join(c.BIN_DIR, "wizard-rules.sh")
failures = []


def expect(name, cond, detail=""):
    if not cond:
        failures.append("%s: %s" % (name, detail))


def context(sample, names=()):
    sdir = os.path.join(TESTDATA, sample)
    state = c.load_state(os.path.join(sdir, "state.env")) or {}
    with open(os.path.join(sdir, "vm2.json")) as f:
        k8s = json.load(f).get("k8s") or {}
    return w.Context(state, k8s, names), state, k8s


def serials(state):
    return {"VM2_LOCAL_SERIAL": state["VM2_LOCAL_SERIAL"], "VM1_LOCAL_SERIAL": state["VM1_LOCAL_SERIAL"],
            "LP_SERIAL": state["LP_SERIAL"], "FS0_SERIAL": "0xc1ae0002", "SHARED0_SERIAL": "0xc1ae0001",
            "POOLED0_SERIAL": "0xc1ee0001"}


def opts(text, env):
    """Return the answers of the options TEXT of demo wizard new, $NAMES of ENV expanded."""
    args = shlex.split(re.sub(r"\$(\w+)", lambda m: env.get(m.group(1), m.group(0)), text))
    given, flags = w.given_options(args)
    return given, flags


# The rule rows: options, then what the claim or lease must be. An entry
# "cls" is the DeviceClass of every request, "cap" its capacity requests,
# "dev" the device section of every MemoryConfig, "sel" whether a serial
# selector is there, "alt" the alternatives of firstAvailable, "args" the
# arguments of cxl-request.
ACCEPTED = [
    ("--source local --mode numa --size 256Mi --serial $VM2_LOCAL_SERIAL",
     {"cls": "cxl-local", "cap": {"memory": "256Mi"}, "dev": {"mode": "numa"}, "sel": True}),
    ("--source local --mode dax --size 128Mi", {"cls": "cxl-local", "cap": {"memory": "128Mi"}, "dev": {"mode": "dax"}, "sel": False}),
    ("--source local --mode dax --read-only", {"cls": "cxl-local", "cap": None, "dev": {"mode": "dax", "readOnly": True}}),
    ("--source local --mode numa --steering auto",
     {"cls": "cxl-local", "dev": {"mode": "numa"}, "container": {"steering": {"policy": "auto"}}}),
    ("--source local --share", {"cls": "cxl-local-shared", "cap": {"shares": "1"}, "dev": {"mode": "dax"}}),
    ("--source pool --mode numa --size 512Mi", {"cls": "cxl-pool", "cap": {"memory": "512Mi"}, "dev": {"mode": "numa"}}),
    ("--source pool --mode dax --serial $POOLED0_SERIAL --read-only",
     {"cls": "cxl-pool", "cap": None, "dev": {"mode": "dax", "readOnly": True}, "sel": True}),
    ("--source pool --mode dax --serial $SHARED0_SERIAL", {"cls": "cxl-pool-shared", "cap": None, "dev": {"mode": "dax"}}),
    ("--source pool --share --read-only", {"cls": "cxl-pool-shared", "cap": {"shares": "1"},
                                                    "dev": {"mode": "dax", "readOnly": True}}),
    ("--source any --mode dax --size 256Mi", {"alt": [("local", "cxl-local"), ("pool", "cxl-pool")],
                                              "cap": {"memory": "256Mi"}, "dev": {"mode": "dax"}}),
    ("--source any --share", {"alt": [("local", "cxl-local-shared"), ("pool", "cxl-pool-shared")],
                                       "cap": {"shares": "1"}}),
    ("--source pool --memory persistent --share --serial $FS0_SERIAL",
     {"cls": "cxl-pool-persistent", "cap": {"shares": "1"}, "dev": {"mode": "fs", "readOnly": True}}),
    ("--source pool --memory persistent --share --read-only=false --fs-type ext4 --serial $FS0_SERIAL",
     {"cls": "cxl-pool-persistent", "dev": {"mode": "fs", "readOnly": False, "fsType": "ext4"}}),
    ("--source pool --memory persistent --share --access cached --path /data",
     {"cls": "cxl-pool-persistent", "dev": {"mode": "fs", "readOnly": True, "access": "cached", "path": "/data"}}),
    ("--source pool --memory persistent --mode block --serial $FS0_SERIAL --command mkfs ext4",
     {"cls": "cxl-pool-persistent", "cap": None, "dev": {"mode": "block"}}),
    ("--source pool --memory persistent --share --mode block --read-only",
     {"cls": "cxl-pool-persistent", "cap": {"shares": "1"}, "dev": {"mode": "block", "readOnly": True}}),
    ("--source local --memory persistent --mode block --serial $LP_SERIAL",
     {"cls": "cxl-local-persistent", "cap": None, "dev": {"mode": "block"}}),
    ("--source local --memory persistent --share --read-only=false --serial $LP_SERIAL",
     {"cls": "cxl-local-persistent", "cap": {"shares": "1"}, "dev": {"mode": "fs", "readOnly": False}}),
    ("--source any --memory persistent --share",
     {"alt": [("local", "cxl-local-persistent"), ("pool", "cxl-pool-persistent")], "cap": {"shares": "1"}}),
    ("--kind lease --source local --mode dax --serial $VM1_LOCAL_SERIAL --node vm2",
     {"args": ["request", "--source", "local", "--mode", "dax", "--serial", "0xc100e2e1"]}),
    ("--kind lease --source local --mode numa --size 128Mi",
     {"args": ["request", "--source", "local", "--mode", "numa", "--size", "128Mi"]}),
    ("--kind lease --source pool --share --read-only --serial $SHARED0_SERIAL",
     {"args": ["request", "--source", "pool", "--mode", "dax", "--share", "--read-only", "--serial", "0xc1ae0001"]}),
    ("--kind lease --source pool --mode dax --serial $POOLED0_SERIAL",
     {"args": ["request", "--source", "pool", "--mode", "dax", "--serial", "0xc1ee0001"]}),
    ("--kind lease --source pool --memory persistent --share --read-only=false --serial $FS0_SERIAL --name rw1",
     {"args": ["request", "--source", "pool", "--mode", "fs", "--share", "--read-only=false", "--name", "rw1", "--serial", "0xc1ae0002"]}),
    ("--kind lease --source local --memory persistent --mode block --serial $LP_SERIAL --command mkfs ext4",
     {"args": ["request", "--source", "local", "--mode", "block", "--serial", "0xc100f5e1"]}),
    ("--kind lease --source any --memory persistent --share --name any1",
     {"args": ["request", "--mode", "fs", "--share", "--name", "any1"]}),
]
REFUSED = [
    ("--memory persistent --mode dax", "mode", "a persistent device needs mode block or fs"),
    ("--memory volatile --mode fs", "mode", "mode fs needs a persistent device"),
    ("--share --mode numa", "mode", "dax only"),
    ("--mode numa --read-only", "rw", "readOnly needs mode dax, block or fs"),
    ("--memory persistent --share --mode block --read-only=false", "rw", "writable in mode block only exclusively"),
    ("--source pool --memory persistent --share --mode block --read-only=false", "rw",
     "the device is writable in mode block only exclusively"),
    ("--memory persistent --read-only=false --fs-type erofs", "fs_type", "erofs is a read-only filesystem"),
    ("--memory persistent --fs-type btrfs --access direct", "access", "btrfs has no DAX"),
    ("--memory persistent --fs-type squashfs", "fs_type", "not allowed on this node"),
    ("--memory persistent --fs-type Ext4", "fs_type", "invalid filesystem type"),
    ("--memory persistent --path /run/cxl/x", "path", "invalid path"),
    ("--memory persistent --path data", "path", "invalid path"),
    ("--kind lease --memory persistent --path /data", "path", "a lease's filesystem is at /run/cxl/fs/NAME"),
    ("--mode dax --steering auto", "steering", "mode numa only"),
    ("--kind lease --mode numa --size 128Mi --steering auto", "steering", "cxl-request has no steering"),
    ("--steering waypoints", "steering", "waypoints"),
    ("--share --size 256Mi", "size", "a shared device is used whole"),
    ("--memory persistent --size 256Mi", "size", "a persistent device is used whole"),
    ("--size 0", "size", "invalid size"),
    ("--kind lease --source local --serial any", "serial", "cxl-request needs --size, --serial or --share"),
    ("--source any --serial $SHARED0_SERIAL", "serial", "a serial names a device of one source"),
    ("--source local --serial $SHARED0_SERIAL", "serial", "a pool device, not local"),
    ("--source pool --memory persistent --serial $SHARED0_SERIAL", "serial", "volatile memory, not persistent"),
    ("--source pool --share --serial $POOLED0_SERIAL", "serial", "not shareable"),
    ("--source pool --share --serial $FS0_SERIAL", "serial", "persistent memory"),
    ("--source pool --mode numa --serial $SHARED0_SERIAL", "serial", "a shared device is used as dax only"),
    ("--source pool --mode dax --size 256Mi --serial $SHARED0_SERIAL", "serial", "a shared device is used whole"),
    ("--serial nope", "serial", "invalid serial"),
    ("--kind lease --source local --memory persistent --share --serial $LP_SERIAL --node vm1", "node",
     "in a pod of its node only (vm2)"),
    ("--mode numa --command read", "command", "numa memory"),
    ("--mode dax --command mkfs ext4", "command", "mkfs writes a block device"),
    ("--memory persistent --mode block --command write x", "command", "mkfs"),
    ("--memory persistent --mode block --command mkfs ntfs", "command", "TYPE one of"),
    ("--mode dax --command write", "command", "write TEXT"),
    ("--name W1", "name", "a DNS label"),
    ("--name w1", "name", "w1 exists"),
    ("--kind pod", "kind", "not one of claim, lease"),
]


def rule_test():
    ctx, state, _ = context("wizard-W0", names={"w1"})
    env = serials(state)
    for text, want in ACCEPTED:
        given, _ = opts(text, env)
        try:
            a = w.resolve(given, ctx)
        except w.Invalid as e:
            failures.append("accepted %r: refused: %s" % (text, e))
            continue
        name = "rule %r" % text
        docs = w.documents(a, ctx)
        yaml_test(name, a, ctx, docs)
        if a["kind"] == "lease":
            got = w.lease_args(a)
            expect(name, got[:-2] == want["args"] and got[-2:] == ["--request-id", a["name"]], "args %s" % got)
            expect(name, docs[0]["kind"] == "ResourceClaimTemplate" and
                   docs[1]["spec"]["resourceClaims"] == [{"name": "lease", "resourceClaimTemplateName": a["name"] + "-lease"}],
                   "lease objects %s" % json.dumps(docs)[:200])
            continue
        claim = docs[0]
        req = claim["spec"]["devices"]["requests"][0]
        subs = req["firstAvailable"] if "firstAvailable" in req else [dict(req["exactly"], name=None)]
        if "alt" in want:
            expect(name, [(s["name"], s["deviceClassName"]) for s in subs] == want["alt"], "alternatives %s" % subs)
        else:
            expect(name, [s["deviceClassName"] for s in subs] == [want["cls"]], "classes %s" % [s["deviceClassName"] for s in subs])
        if "cap" in want:
            caps = [(s.get("capacity") or {}).get("requests") for s in subs]
            expect(name, all(cp == want["cap"] for cp in caps), "capacity %s" % caps)
        if "sel" in want:
            expect(name, all(bool(s.get("selectors")) == want["sel"] for s in subs), "selectors %s" % subs)
        cfgs = claim["spec"]["devices"]["config"]
        drivers = [("cxl.nri.io" if s["deviceClassName"].startswith("cxl-local") else "cxl-pool.nri.io") for s in subs]
        expect(name, [e["opaque"]["driver"] for e in cfgs] == drivers, "config drivers %s" % cfgs)
        for e in cfgs:
            p = e["opaque"]["parameters"]
            expect(name, p["apiVersion"] == "cxl.nri.io/v1alpha1" and p["kind"] == "MemoryConfig", "kind %s" % p)
            if "dev" in want:
                expect(name, p["device"] == want["dev"], "device %s" % p["device"])
            if "container" in want:
                expect(name, p.get("container") == want["container"], "container %s" % p.get("container"))
        if len(subs) > 1:
            expect(name, [e.get("requests") for e in cfgs] == [["mem/" + s["name"]] for s in subs], "config requests %s" % cfgs)
    for text, q, reason in REFUSED:
        given, _ = opts(text, env)
        try:
            a = w.resolve(given, ctx)
            failures.append("refused %r: accepted %s" % (text, a))
        except w.Invalid as e:
            expect("refused %r" % text, e.question == q and reason in e.reason, "%s: %s" % (e.question, e.reason))


DEMO_NODES = ("vm1", "vm2")   # the values of the label cxl-demo/node of demo setup


def on_demo_nodes(pod):
    """Return True if POD can only land on a node with the label cxl-demo/node (vm1, vm2) of demo setup."""
    spec = pod.get("spec") or {}
    if (spec.get("nodeSelector") or {}).get(w.NODE_LABEL) in DEMO_NODES:
        return True
    req = (((spec.get("affinity") or {}).get("nodeAffinity") or {}).get("requiredDuringSchedulingIgnoredDuringExecution") or {})
    terms = req.get("nodeSelectorTerms") or []

    def demo_term(t):
        for e in t.get("matchExpressions") or []:
            if e.get("key") == w.NODE_LABEL and (e.get("operator") == "Exists" or
                                                 (e.get("operator") == "In" and set(e.get("values") or []) <= set(DEMO_NODES))):
                return True
        return False
    return bool(terms) and all(demo_term(t) for t in terms)


def manifests_test():
    """Check that every Pod of manifests/ (the cases' pods, the setup's formatter) is pinned to a demo node."""
    mdir = os.path.join(c.DEMO_DIR, "manifests")
    count = 0
    for case in sorted(os.listdir(mdir)):
        # llm-share has a reader on every VM: setup labels vm0 too.
        nodes = DEMO_NODES + ("vm0",) if case == "llm-share" else DEMO_NODES
        for f in sorted(os.listdir(os.path.join(mdir, case))):
            if not f.endswith(".yaml"):
                continue
            with open(os.path.join(mdir, case, f)) as fh:
                docs = re.split(r"^---$", fh.read(), flags=re.M)
            for doc in docs:
                if not re.search(r"^kind: Pod$", doc, re.M):
                    continue
                count += 1
                expect("manifests/%s/%s" % (case, f),
                       re.search(r"^  nodeSelector:\n    %s: (%s)\b" % (re.escape(w.NODE_LABEL), "|".join(nodes)), doc, re.M),
                       "a Pod without nodeSelector %s: %s:\n%s" % (w.NODE_LABEL, "|".join(nodes), doc[:300]))
    expect("manifests", count >= 10, "%d Pods" % count)


def yaml_test(name, a, ctx, docs):
    """Check the YAML of the answers A: ASCII, header width, labels, read back equal to DOCS."""
    warn, note = w.warnings(a, ctx)
    text = w.yaml_text(a, ctx, warn, note)
    expect(name, all(32 <= ord(ch) < 127 or ch == "\n" for ch in text), "not printable ASCII")
    head = [l for l in text.splitlines() if l.startswith("#")]
    expect(name, head and all(len(l) <= 79 for l in head), "header lines over 79: %s" % [l for l in head if len(l) > 79])
    for d in docs:
        expect(name, d["metadata"]["labels"] == {"app": w.APP, w.NAME_LABEL: a["name"]}, "labels %s" % d["metadata"])
    expect(name, on_demo_nodes(docs[-1]), "the pod may land on a node without %s: %s" % (
        w.NODE_LABEL, json.dumps(docs[-1]["spec"].get("affinity") or docs[-1]["spec"].get("nodeSelector"))))
    try:
        back = w.from_yaml(text)
    except ValueError as e:
        failures.append("%s: YAML not read back: %s" % (name, e))
        return
    expect(name, back == plain(docs), "YAML read back differs:\n%s" % text)


def plain(obj):
    """Return OBJ with the Raw scalars as the values that they write (0755: 493)."""
    if isinstance(obj, dict):
        return {k: plain(v) for k, v in obj.items()}
    if isinstance(obj, list):
        return [plain(v) for v in obj]
    if isinstance(obj, w.Raw):
        return w.parse_scalar(str(obj))
    return obj


def yaml_scalar_test():
    for v, want in (("1", "'1'"), ("256Mi", "'256Mi'"), ("cxl-local", "cxl-local"), ("-c", "'-c'"), ("yes", "'yes'"),
                    ("it's", "'it''s'"), (True, "true"), (2, "2"), (w.Raw("0755"), "0755"), ("/demo", "/demo"),
                    ('device.attributes["cxl.nri.io"].serial == "0x1"', '\'device.attributes["cxl.nri.io"].serial == "0x1"\'')):
        expect("yaml_scalar %r" % v, w.yaml_scalar(v) == want, w.yaml_scalar(v))
    obj = {"a": [{"b": [1, "x y"], "c": {}}, "d"], "e": {"f": {"g": False}}, "h": []}
    expect("yaml round trip", w.from_yaml("\n".join(w.to_yaml(obj))) == [obj], "\n".join(w.to_yaml(obj)))


def question_test():
    """Check that every question screen of a few walks fits 50 columns."""
    ctx, state, _ = context("wizard-W8", names={"w%d" % i for i in range(1, 9)})
    env = serials(state)
    for text in ("", "--memory persistent --mode fs", "--memory persistent --mode block --serial $FS0_SERIAL",
                 "--kind lease --share", "--source any --mode numa"):
        given, _ = opts(text, env)
        a = {}
        for q in w.QUESTIONS:
            if w.asked(q, a):
                continue
            lines, _ = w.option_lines(q, a, ctx, w.default(q, a, ctx))
            head = "%d/%d %s: %s" % (len(a) + 1, len(w.QUESTIONS), q, w.PROMPTS[q])
            wide = [l for l in [head] + lines if len(l) > 50]
            expect("question %s of %r" % (q, text), not wide, "over 50 columns: %s" % wide)
            v = w.normalize(q, given[q]) if q in given else w.default(q, a, ctx)
            if v is None:
                failures.append("question %s of %r: no default" % (q, text))
                break
            a[q] = v


def view_test(out):
    """Render T2, T3, T4, T6 of the wizard samples; check 50 x 10."""
    for sample in sorted(d for d in os.listdir(TESTDATA) if d.startswith("wizard-")):
        sdir = os.path.join(TESTDATA, sample)
        state = c.load_state(os.path.join(sdir, "state.env")) or {}
        data = {}
        for k in ("vm2", "vm1", "pool"):
            with open(os.path.join(sdir, k + ".json")) as f:
                data[k] = json.load(f)
        for v, fn in (("t2", wv.render_t2), ("t3", wv.render_t3), ("t4", wv.render_t4), ("t6", wv.render_t6)):
            lines = [c.cut(l, 50) for l in fn(data, state, 1792500067.0)]
            raw = fn(data, state, 1792500067.0)
            expect("%s %s" % (sample, v), len(lines) <= 10 and all(len(l) <= 50 for l in raw),
                   "%d lines, widest %d" % (len(lines), max(len(l) for l in raw)))
            out.append("+" + "-" * 50 + "+ %s %s" % (sample, v))
            out += ["|" + l.ljust(50) + "|" for l in lines]


def rules_rows():
    """Return the rows of bin/wizard-rules.sh: [(line number, expect, options)]."""
    rows = []
    with open(RULES) as f:
        text = f.read()
    m = re.search(r"<<'ROWS'\n(.*?)\nROWS\n", text, re.S)
    if not m:
        failures.append("%s: no rows" % RULES)
        return rows
    start = text[:m.start(1)].count("\n") + 1
    for i, line in enumerate(m.group(1).splitlines()):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        word, _, rest = line.partition(" ")
        rows.append((start + i, word, rest.strip()))
    return rows


def script_test():
    """Check every new row of the scripted set against the devices of wizard-W0."""
    ctx0, state, k8s = context("wizard-W0")
    slices = k8s.get("slices")
    env = serials(state)
    names = set()
    seen = {"refused": 0, "new": 0}
    for n, word, rest in rules_rows():
        where = "wizard-rules.sh:%d" % n
        if word in ("delete", "await", "list"):
            continue
        if word not in ("Running", "Ready", "wait", "wait!") and not word.startswith("refused:"):
            failures.append("%s: unknown expectation %s" % (where, word))
            continue
        given, flags = opts(rest, env)
        ctx = w.Context(ctx0.state, {"slices": slices}, names)
        try:
            a = w.resolve(given, ctx)
        except w.Invalid as e:
            expect(where, word.startswith("refused:") and word[len("refused:"):].replace("_", " ") in e.reason,
                   "refused: %s" % e)
            seen["refused"] += 1
            continue
        if word.startswith("refused:"):
            failures.append("%s: accepted, expected %s" % (where, word))
            continue
        seen["new"] += 1
        expect(where, "name" in given, "a row names its object (--name)")
        expect(where, on_demo_nodes(w.pod_doc(a, ctx.state)), "the pod may land on a node without %s" % w.NODE_LABEL)
        names.add(a["name"])
        warn, _ = w.warnings(a, ctx)
        if word == "wait!":
            expect(where, warn, "expects a wait that the devices predict, no warning")
        elif word in ("Running", "Ready"):
            fixed = [x for x in warn if "no published device" in x or "reaches" in x or "'s, the pod is on" in x]
            expect(where, not fixed, "expects %s, warnings %s" % (word, fixed))
            expect(where, (word == "Ready") == (a["kind"] == "lease"), "%s of a %s" % (word, a["kind"]))
    expect("wizard-rules.sh", seen["new"] >= 25 and seen["refused"] >= 10, "rows %s" % seen)


def status_test():
    """Check the states of the objects of wizard-W8 as new and list say them."""
    _, state, k8s = context("wizard-W8")
    want = {"w1": ("Running", ""), "w2": ("Running", ""), "w3": ("waiting", "cannot allocate"), "w4": ("Running", ""),
            "w5": ("waiting", "AttachFailed Conflict"), "w6": ("Running", ""), "w7": ("Pending", "cannot allocate"),
            "w8": ("Ready", ""), "w9": ("gone", "")}
    for name, st in want.items():
        got = w.status(name, k8s, state)
        expect("status %s" % name, got == st, "%s, not %s" % (got, st))
    rows = {r[0]: r for r in w.object_rows(k8s, state)}
    expect("list w1", rows.get("w1") == ["w1", "claim", "Running", "vm2", "memdev-c100e2e1", "numa", "256Mi"], str(rows.get("w1")))
    expect("list w8", rows.get("w8") == ["w8", "lease", "Ready", "vm1", "memdev-c100e2e1", "dax", "ro 128Mi"], str(rows.get("w8")))
    expect("list w5", rows.get("w5", [""] * 7)[2] == "waiting (AttachFailed Conflict)", str(rows.get("w5")))


def state_test():
    """Check that the state file reads as demo setup writes it: printf %q of the setup, the file of a live setup."""
    import subprocess
    import tempfile
    values = {"FS_TYPES": "erofs,ext4,xfs,btrfs", "FS_SERIALS": "0xc1ae0002,0xc100f5e1", "DEMO_FSTYPE": "",
              "POOL_DEVICE_FILES": "/tmp/fake-cxl-pool/fs0.raw /tmp/fake-cxl-pool/fs0.lsa",
              "ODD": "it's \"x\" $y;z", "LP_URL": "http://127.0.0.1:9912"}
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "state.env")
        script = "for kv in \"$@\"; do printf '%s=%q\\n' \"${kv%%=*}\" \"${kv#*=}\"; done > \"$0\""
        subprocess.run(["bash", "-c", script, path] + ["%s=%s" % kv for kv in values.items()], check=True)
        got = c.load_state(path)
    for k, v in values.items():
        expect("state %s" % k, got.get(k) == v, "%r, not %r" % (got.get(k), v))
    live = c.load_state(os.path.join(TESTDATA, "state-wizard-setup.env"))
    expect("live state FS_TYPES", live.get("FS_TYPES") == "erofs,ext4,xfs,btrfs", repr(live.get("FS_TYPES")))
    expect("live state DEMO_FSTYPE", live.get("DEMO_FSTYPE") == "", repr(live.get("DEMO_FSTYPE")))
    expect("live state POOL_DEVICE_FILES", len(live.get("POOL_DEVICE_FILES", "").split(" ")) == 4, repr(live.get("POOL_DEVICE_FILES")))
    _, _, k8s = context("wizard-W0")
    ctx = w.Context(live, k8s)
    expect("live state fsTypes", ctx.fs_types == ("erofs", "ext4", "xfs", "btrfs"), str(ctx.fs_types))
    expect("live state fs serials", ctx.fs_serials == ["0xc1ae0002", "0xc100f5e1"], str(ctx.fs_serials))
    for text in ("--memory persistent --source pool --share --read-only=false --fs-type ext4",
                 "--memory persistent --source local --share --fs-type xfs", "--memory persistent --fs-type erofs"):
        given, _ = opts(text, serials(live))
        try:
            w.resolve(given, ctx)
        except w.Invalid as e:
            failures.append("live state %r: refused: %s" % (text, e))


def command_test():
    """Check that a claim's pod and a lease's background command end with "cxl-wizard exit N", and new reads it.

    The pod's command and the lease start run here with the tool of
    bin/tools, a filesystem directory as the device and a fake
    cxl-request.
    """
    import subprocess
    import tempfile
    tool = os.path.join(c.BIN_DIR, "tools", "cxl-wizard.py")
    expect("command_result", w.command_result("a\nb\ncxl-wizard exit 3\nlater") == (3, ["a", "b"]) and
           w.command_result("a\n")[0] is None, str(w.command_result("a\ncxl-wizard exit 3")))
    ctx, _, _ = context("wizard-W0")
    with tempfile.TemporaryDirectory() as d:
        fsdir = os.path.join(d, "fs")
        os.makedirs(fsdir)
        with open(os.path.join(fsdir, "hello.txt"), "w") as f:
            f.write("Hello from the desk\n")
        env = dict(os.environ, CXL_K_MODE="fs", CXL_K_PATH=fsdir, CXL_K_FS_TYPE="ext4", CXL_WIZARD_LOG=os.path.join(d, "log"))
        given, _ = opts("--memory persistent --source local --share --command read", {})
        a = w.resolve(given, ctx)
        cmd = w.pod_doc(a, {})["spec"]["containers"][0]["command"]
        script = cmd[2].replace(w.TOOL, tool).replace(w.IDLE, ":")
        p = subprocess.run(["sh", "-c", script] + cmd[3:], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        rc, lines = w.command_result(p.stdout.decode())
        expect("claim pod command", rc == 0 and any("Hello from the desk" in l for l in lines), p.stdout.decode())
        # A fake cxl-request: the response line, or with --exec the command after it.
        fake = os.path.join(d, "bin")
        os.makedirs(fake)
        with open(os.path.join(fake, "cxl-request"), "w") as f:
            f.write('#!/bin/sh\nfor a in "$@"; do shift; [ "$a" = --exec ] && exec "$@"; done\n'
                    'echo \'{"lease": "1a2b3c4d", "state": "Pending"}\'\n')
        os.chmod(os.path.join(fake, "cxl-request"), 0o755)
        env["PATH"] = fake + os.pathsep + env.get("PATH", "")
        p = subprocess.run([sys.executable, tool, "lease", "request", "--mode", "fs", "--", "read"], env=env,
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        expect("lease start", p.returncode == 0 and '"lease": "1a2b3c4d"' in p.stdout.decode(), p.stdout.decode())
        rc, lines, t0 = None, [], time.time()
        while rc is None and time.time() - t0 < 10:
            with open(env["CXL_WIZARD_LOG"]) as f:
                rc, lines = w.command_result(f.read())
            time.sleep(0.1)
        expect("lease background command", rc == 0 and any("Hello from the desk" in l for l in lines), "%s %s" % (rc, lines))


def load_tool():
    """Return the pod tool bin/tools/cxl-wizard.py as a module."""
    import importlib.util
    spec = importlib.util.spec_from_file_location("cxl_wizard", os.path.join(c.BIN_DIR, "tools", "cxl-wizard.py"))
    tool = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(tool)
    return tool


def dax_test():
    """Check write and read of a dax device whose mapping refuses msync (EINVAL), as devdax does."""
    import contextlib
    import errno
    import io
    import mmap as real
    import tempfile

    class DaxMapping:
        def __init__(self, m):
            self.m = m

        def __getitem__(self, k):
            return self.m[k]

        def __setitem__(self, k, v):
            self.m[k] = v

        def flush(self, *args):
            raise OSError(errno.EINVAL, "Invalid argument")

        def close(self):
            self.m.close()

    class Devdax:
        MAP_SHARED, PROT_READ, PROT_WRITE = real.MAP_SHARED, real.PROT_READ, real.PROT_WRITE

        @staticmethod
        def mmap(*args, **kwargs):
            return DaxMapping(real.mmap(*args, **kwargs))

    tool = load_tool()
    tool.mmap = Devdax
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "dax0.0")
        with open(path, "wb") as f:
            f.truncate(4 << 20)
        saved = dict(os.environ)
        os.environ.update(CXL_K_MODE="dax", CXL_K_DAX=path, CXL_K_SIZE=str(4 << 20))
        out = io.StringIO()
        try:
            with contextlib.redirect_stdout(out):
                tool.do_write("K", "dax", "Hello from the desk")
                tool.do_read("K", "dax")
        except (OSError, SystemExit) as e:
            failures.append("dax write and read: %r" % e)
        finally:
            os.environ.clear()
            os.environ.update(saved)
    expect("dax write and read", out.getvalue().splitlines() == ['K: wrote at 0: "Hello from the desk"',
                                                                  'K: read at 0: "Hello from the desk"'], out.getvalue())


def image_test(notes):
    """Run mkfs of every type and read of the pod tool inside the demo image, on a device node and through fd 3.

    Needs docker and the image of the current image/Dockerfile on the
    host (demo setup builds it); else it adds a note and checks nothing.
    """
    import hashlib
    import shutil
    import subprocess
    import tempfile
    with open(os.path.join(c.DEMO_DIR, "image", "Dockerfile"), "rb") as f:
        tag = "localhost/cxl-demo-tools:" + hashlib.sha256(f.read()).hexdigest()[:12]
    if not shutil.which("docker") or subprocess.run(["docker", "image", "inspect", tag], stdout=subprocess.DEVNULL,
                                                    stderr=subprocess.DEVNULL).returncode != 0:
        notes.append("image test skipped: no docker image %s (demo setup builds it)" % tag)
        return
    size = 534773760   # the fsdax namespace of lp0 and fs0
    script = """set -e
for t in xfs ext4 btrfs erofs; do
    truncate -s %(size)d /w/node.$t
    CXL_K_MODE=block CXL_K_PATH=/w/node.$t CXL_K_SIZE=%(size)d python3 /w/cxl-wizard.py mkfs $t
    CXL_K_MODE=block CXL_K_PATH=/w/node.$t CXL_K_SIZE=%(size)d python3 /w/cxl-wizard.py read
    truncate -s %(size)d /w/fd.$t
    CXL_K_MODE=block CXL_K_FD=3 CXL_K_SIZE=%(size)d python3 /w/cxl-wizard.py mkfs $t 3<>/w/fd.$t
    CXL_K_MODE=block CXL_K_FD=3 CXL_K_SIZE=%(size)d python3 /w/cxl-wizard.py read 3</w/fd.$t
done
""" % {"size": size}
    with tempfile.TemporaryDirectory() as d:
        shutil.copy(os.path.join(c.BIN_DIR, "tools", "cxl-wizard.py"), d)
        p = subprocess.run(["docker", "run", "--rm", "-v", "%s:/w:Z" % d, tag, "sh", "-c", script],
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        text = p.stdout.decode(errors="replace")
        subprocess.run(["docker", "run", "--rm", "-v", "%s:/w:Z" % d, tag, "sh", "-c", "rm -f /w/node.* /w/fd.*"],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for t in ("xfs", "ext4", "btrfs", "erofs"):
        expect("image mkfs %s" % t, text.count("K: made %s " % t) == 2 and text.count("bytes: %s" % t) == 2,
               "in %s:\n%s" % (tag, text))
    expect("image run", p.returncode == 0, text[-500:])


def demo_image():
    """Return the tag of the demo image of the current image/Dockerfile if docker has it, else None."""
    import hashlib
    import shutil
    import subprocess
    with open(os.path.join(c.DEMO_DIR, "image", "Dockerfile"), "rb") as f:
        tag = "localhost/cxl-demo-tools:" + hashlib.sha256(f.read()).hexdigest()[:12]
    if not shutil.which("docker") or subprocess.run(["docker", "image", "inspect", tag], stdout=subprocess.DEVNULL,
                                                    stderr=subprocess.DEVNULL).returncode != 0:
        return None
    return tag


def block_test(notes):
    """Run the pod tool's mkfs of every type on a real block device in an unprivileged container of the demo image.

    A loop device over a file of the namespace's size, attached and
    detached by a privileged container (the only privileged step), is the
    container's /dev/pmem9 (docker run --device, no privileges): as the
    device node of a claim. mkfs.xfs on the node itself must fail
    (BLKBSZSET needs CAP_SYS_ADMIN), the tool's mkfs must succeed and read
    must find the type.
    """
    import shutil
    import subprocess
    import tempfile
    tag = demo_image()
    if tag is None:
        notes.append("block test skipped: no docker image of image/Dockerfile (demo setup builds it)")
        return
    size = 534773760
    with tempfile.TemporaryDirectory() as d:
        shutil.copy(os.path.join(c.BIN_DIR, "tools", "cxl-wizard.py"), d)
        with open(os.path.join(d, "blk"), "wb") as f:
            f.truncate(size)
        vol = "%s:/w:Z" % d
        p = subprocess.run(["docker", "run", "--rm", "--privileged", "-v", vol, tag, "sh", "-c",
                            'l=$(losetup -f) && losetup "$l" /w/blk && echo "$l"'], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        loop = (p.stdout.decode().strip().splitlines() or [""])[-1]
        if p.returncode != 0 or not re.fullmatch(r"/dev/loop\d+", loop):
            failures.append("block test: no loop device: %s" % p.stdout.decode()[-300:])
            return
        try:
            script = "mkfs.xfs -q -f /dev/pmem9; echo \"direct exit $?\"\n" + "".join(
                "CXL_K_MODE=block CXL_K_PATH=/dev/pmem9 CXL_K_SIZE=%d python3 /w/cxl-wizard.py mkfs %s\n"
                "CXL_K_MODE=block CXL_K_PATH=/dev/pmem9 CXL_K_SIZE=%d python3 /w/cxl-wizard.py read\n" % (size, t, size)
                for t in ("xfs", "ext4", "btrfs", "erofs"))
            p = subprocess.run(["docker", "run", "--rm", "--device", "%s:/dev/pmem9" % loop, "-v", vol, tag, "sh", "-c", script],
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            text = p.stdout.decode(errors="replace")
        finally:
            subprocess.run(["docker", "run", "--rm", "--privileged", "-v", vol, tag, "losetup", "-d", loop],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(["docker", "run", "--rm", "-v", vol, tag, "rm", "-f", "/w/blk"],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    expect("block mkfs.xfs on the node", "Permission denied" in text and "direct exit 1" in text,
           "mkfs.xfs on a block device node without privileges did not fail as expected:\n" + text)
    for t in ("xfs", "ext4", "btrfs", "erofs"):
        expect("block mkfs %s" % t, ("K: made %s " % t) in text and ("bytes: %s" % t) in text, "in %s:\n%s" % (tag, text))


def node_test():
    """Check the pods of --node any, vm1 and vm2 of a claim and a lease, and that a pod without a demo node fails the check."""
    ctx, _, _ = context("wizard-W0")
    for text in ("--node any", "--node vm1", "--node vm2", "--kind lease --size 128Mi --node any",
                 "--kind lease --size 128Mi --node vm2"):
        given, _ = opts(text, {})
        a = w.resolve(given, ctx)
        pod = w.pod_doc(a, ctx.state)
        expect("node %r" % text, on_demo_nodes(pod), json.dumps(pod["spec"].get("affinity") or pod["spec"].get("nodeSelector")))
        if a["node"] == "any":
            expect("node %r header" % text, "any demo node (vm1 or vm2)" in " ".join(w.describe(a, ctx)), str(w.describe(a, ctx)))
    expect("node check itself", not on_demo_nodes({"spec": {}}) and
           not on_demo_nodes({"spec": {"nodeSelector": {"kubernetes.io/hostname": "n4-cxl-shared-0"}}}), "accepts a free pod")


def main(args):
    out = []
    notes = []
    node_test()
    manifests_test()
    state_test()
    command_test()
    dax_test()
    image_test(notes)
    if "--block" in args:
        block_test(notes)
    else:
        notes.append("block test not run: wizard.py check --block (attaches a loop device through privileged docker)")
    status_test()
    yaml_scalar_test()
    rule_test()
    question_test()
    view_test(out)
    script_test()
    if "--quiet" not in args:
        print("\n".join(out))
    if failures:
        print("wizard check: FAIL")
        for f in failures:
            print("  " + f)
        return 1
    for n in notes:
        print("note: " + n)
    print("wizard check: ok, %d accepted and %d refused rule rows, %d rows of wizard-rules.sh"
          % (len(ACCEPTED), len(REFUSED), len(rules_rows())))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
