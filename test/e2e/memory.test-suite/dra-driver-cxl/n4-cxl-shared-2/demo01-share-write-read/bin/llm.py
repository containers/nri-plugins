#!/usr/bin/env python3
"""The host side of the llm-share demo: where the model is in memory, and the bench results.

Usage:
  llm.py mem LABEL [--since LABEL]
  llm.py results [FILE...]

mem prints where the model is now and saves it to run/llm-mem-LABEL.json:
on the host, the pool directory (its mount options: the page size and
the memory policy of the pages of the devices), the backing file of llm0
(its bytes in host memory, which cxl-numa-place where shows per NUMA node
when it is built) and the mapping of it by the qemu of each VM (smaps Rss
and Pss: the same pages for every VM; ShmemPmdMapped: in 2 MiB pages;
numa_maps: their nodes) with the private memory of that qemu (RssAnon:
the guest's RAM); in each VM, through /demo/llm-mem of its
reader (kubectl exec), the page cache and free memory of the VM, the page
cache charged to the reader and its mapping of the gguf. --since LABEL
adds the changes since the snapshot LABEL. The last line is "mem: JSON".
results prints the table of the bench results of run/llm-results/ (or of
the FILEs), the steps of llm-share save each bench there with what it ran
on: model, access, the latency setup (off, local or far, with the level of
the background load), the pool directory and its page size, setup.
"""
import glob
import json
import os
import re
import subprocess
import sys
import time

import common as c

READERS = ["reader-0", "reader-1", "reader-2"]
VMS = (("vm0", "VM0_DIR"), ("vm1", "VM1_DIR"), ("vm2", "VM2_DIR"))
RESULTS_DIR = os.path.join(c.STATE_DIR, "llm-results")
# The nri-plugins tree: bin/demo exports it, else the one around the demo.
NRI_SRC = os.environ.get("NRI_PLUGINS_SRC") or os.path.normpath(os.path.join(c.DEMO_DIR, *[".."] * 6))
NUMA_PLACE = os.path.join(NRI_SRC, "scripts", "testing", "fake-cxl-pool", "bin", "cxl-numa-place")
LATENCY = os.path.join(c.BIN_DIR, "latency.sh")
MEM_FIELDS = ("Rss", "Pss", "Shared_Clean", "Shared_Dirty", "Private_Clean", "Private_Dirty", "ShmemPmdMapped")


def qemu_pid(vmdir):
    """Return the pid of the qemu of the VM of the e2e output directory VMDIR, None if it does not run.

    The disk image of the VM is under VMDIR/.vagrant/machines/: the
    command line names it (lib/vm.bash vm-qemu-pid).
    """
    want = (os.path.realpath(vmdir) + "/.vagrant/machines/").encode()
    for pid in os.listdir("/proc"):
        if not pid.isdigit():
            continue
        try:
            with open("/proc/%s/cmdline" % pid, "rb") as f:
                cmd = f.read()
        except OSError:
            continue
        if b"qemu-system" in cmd.split(b"\0", 1)[0] and want in cmd:
            return int(pid)
    return None


def file_mapping(pid, path):
    """Return the smaps fields (kB) and NUMA nodes (pages) of the mappings of PATH by process PID, {} if it has none."""
    out = {}
    try:
        with open("/proc/%d/smaps" % pid) as f:
            mine = False
            for line in f:
                if re.match(r"^[0-9a-f]+-[0-9a-f]+ ", line):
                    mine = line.split()[-1] == path
                elif mine:
                    k, _, v = line.partition(":")
                    if k in MEM_FIELDS:
                        out[k] = out.get(k, 0) + int(v.split()[0])
        with open("/proc/%d/numa_maps" % pid) as f:
            for line in f:
                if "file=" + path in line.split():
                    for m in re.finditer(r"\bN(\d+)=(\d+)", line):
                        nodes = out.setdefault("nodes", {})
                        nodes[m.group(1)] = nodes.get(m.group(1), 0) + int(m.group(2))
    except OSError as e:
        out["error"] = e.strerror
    return out


def rss_anon(pid):
    """Return RssAnon of process PID in kB, None if it cannot be read."""
    try:
        with open("/proc/%d/status" % pid) as f:
            for line in f:
                if line.startswith("RssAnon:"):
                    return int(line.split()[1])
    except OSError:
        pass
    return None


def mount_options(text):
    """Return the mount options TEXT, like "rw,huge=within_size,mpol=interleave:1-2,5-6", as a dict (a flag has the value "")."""
    pieces = []
    for o in text.split(","):
        # The node list of an mpol option has commas, and the next piece
        # then starts with a digit (the kernel reads it the same way).
        if pieces and pieces[-1].startswith("mpol=") and o[:1].isdigit():
            pieces[-1] += "," + o
        else:
            pieces.append(o)
    return dict(o.split("=", 1) if "=" in o else (o, "") for o in pieces)


def pool_mount(state):
    """Return the mount of the pool directory of STATE: dir, source, fstype, options, and its huge and mpol options."""
    d = state.get("DEMO_POOL_DIR") or os.path.dirname(state.get("POOL_DEVICE_DATA") or "")
    out = {"dir": d}
    try:
        p = subprocess.run(["findmnt", "-n", "-o", "SOURCE,FSTYPE,OPTIONS", "-T", d], stdout=subprocess.PIPE,
                           stderr=subprocess.STDOUT, timeout=10)
        words = p.stdout.decode(errors="replace").split()
    except (OSError, subprocess.TimeoutExpired) as e:
        return dict(out, error=str(e))
    if len(words) < 3:
        return dict(out, error=" ".join(words) or "no mount")
    out.update(source=words[0], fstype=words[1], options=words[2])
    opts = mount_options(words[2])
    out["huge"] = opts.get("huge", "never")
    out["mpol"] = opts.get("mpol", "default")
    return out


def pmd_share(vms):
    """Return the share (0-1) of the mappings of llm0 by the qemus of VMS that is in 2 MiB pages, None without mappings."""
    rss = sum((v.get("mapping") or {}).get("Rss") or 0 for v in vms.values())
    pmd = sum((v.get("mapping") or {}).get("ShmemPmdMapped") or 0 for v in vms.values())
    return pmd / rss if rss else None


def pages_words(share):
    """Return the share of 2 MiB pages SHARE in words: "2M pages 99%", "?" for None."""
    return "?" if share is None else "2M pages %d%%" % round(100 * share)


def host_facts(state):
    """Return the host side: the backing file of the pool device and each VM's qemu with its mapping of it."""
    path = state.get("POOL_DEVICE_DATA") or ""
    out = {"file": path, "vms": {}, "pool": pool_mount(state)}
    try:
        st = os.stat(path)
        out["fileSize"], out["fileResidentKb"] = st.st_size, st.st_blocks // 2
    except OSError as e:
        out["fileError"] = e.strerror
    if os.access(NUMA_PLACE, os.X_OK) and path:
        try:
            p = subprocess.run([NUMA_PLACE, "where", path], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
            out["numaPlace"] = p.stdout.decode(errors="replace").rstrip("\n")
        except (OSError, subprocess.TimeoutExpired) as e:
            out["numaPlace"] = "cxl-numa-place: %s" % e
    for vm, key in VMS:
        pid = qemu_pid(state.get(key) or "")
        out["vms"][vm] = {"qemuPid": pid, "mapping": file_mapping(pid, path) if pid else {},
                          "qemuRssAnonKb": rss_anon(pid) if pid else None}
    out["pmdShare"] = pmd_share(out["vms"])
    return out


def guest_facts(state):
    """Return reader -> its /demo/llm-mem facts (kB and counters), or {"error": ...} if it gave none."""
    script = "for r in %s; do kubectl exec $r -- /demo/llm-mem $r 2>&1 || echo \"$r error kubectl exec failed\"; done" % " ".join(READERS)
    rc, text = c.run_vm(state["VM2_DIR"], "cd /root/demo && export KUBECONFIG=/root/demo/kubeconfig\n" + script, timeout=60)
    out = {r: {"error": "no answer"} for r in READERS}
    for line in text.splitlines():
        words = line.split()
        if not words or words[0] not in out:
            continue
        kv = dict(w.split("=", 1) for w in words[1:] if "=" in w)
        if not kv:
            out[words[0]] = {"error": " ".join(words[1:])}
            continue
        out[words[0]] = {k: (int(v) if v.isdigit() else v) for k, v in kv.items()}
    return out


def mib(kb):
    """Return kB KB as whole MiB with M, "?" for None."""
    return "?" if kb is None else "%dM" % round(kb / 1024)


def delta(now, then, key):
    """Return now[key] - then[key], None unless both are numbers."""
    a, b = (now or {}).get(key), (then or {}).get(key)
    return a - b if isinstance(a, int) and isinstance(b, int) else None


def signed_mib(kb):
    return "?" if kb is None else "%+dM" % round(kb / 1024)


def mem_lines(snap, since=None):
    """Return the lines of the memory snapshot SNAP, with the changes since the snapshot SINCE."""
    h, g = snap["host"], snap["guest"]
    pool = h.get("pool") or {}
    lines = ["pool directory %s: %s" % (pool.get("dir") or "?", pool.get("error") or "%s %s %s" % (
                 pool.get("source"), pool.get("fstype"), pool.get("options"))),
             "host: %s, %s of it in host memory" % (h.get("file") or "?", mib(h.get("fileResidentKb")) if "fileResidentKb" in h else h.get("fileError", "?"))]
    for vm, v in sorted(h["vms"].items()):
        m = v["mapping"]
        nodes = " ".join("N%s %s" % (n, mib(p * 4)) for n, p in sorted((m.get("nodes") or {}).items()))
        line = "  %s qemu %s maps it: Rss %s (%s in 2 MiB pages) Pss %s%s; qemu RssAnon %s" % (
            vm, v["qemuPid"] or "?", mib(m.get("Rss")), mib(m.get("ShmemPmdMapped")), mib(m.get("Pss")),
            " (%s)" % nodes if nodes else "", mib(v["qemuRssAnonKb"]))
        if since:
            line += " (%s)" % signed_mib(delta(v, since["host"]["vms"].get(vm), "qemuRssAnonKb"))
        lines.append(line)
    if h.get("numaPlace"):
        lines += ["  cxl-numa-place where: " + l for l in h["numaPlace"].splitlines()]
    for r in READERS:
        f = g.get(r) or {}
        if f.get("error"):
            lines.append("%s: %s" % (r, f["error"]))
            continue
        line = "%s: maps %s of the gguf, page cache charged %s; VM Cached %s, MemAvailable %s" % (
            r, mib(f.get("ggufRssKb")), mib(f.get("cgFileKb")), mib(f.get("cachedKb")), mib(f.get("memAvailableKb")))
        if since:
            t = since["guest"].get(r)
            line += " (Cached %s, MemAvailable %s, pgsteal %+d)" % (
                signed_mib(delta(f, t, "cachedKb")), signed_mib(delta(f, t, "memAvailableKb")), delta(f, t, "pgsteal") or 0)
        lines.append(line)
    return lines


def mem_path(label):
    return os.path.join(c.STATE_DIR, "llm-mem-%s.json" % label)


def cmd_mem(state, label, since_label):
    """Print and save the memory snapshot LABEL, with the changes since SINCE_LABEL if given."""
    since = None
    if since_label:
        try:
            with open(mem_path(since_label)) as f:
                since = json.load(f)
        except (OSError, ValueError) as e:
            sys.exit("no snapshot %s (%s): llm.py mem %s first" % (since_label, e, since_label))
    snap = {"label": label, "time": time.time(), "model": state.get("DEMO_LLM_MODEL"),
            "host": host_facts(state), "guest": guest_facts(state)}
    if since:
        snap["since"] = since_label
    os.makedirs(c.STATE_DIR, exist_ok=True)
    with open(mem_path(label), "w") as f:
        json.dump(snap, f, indent=1, sort_keys=True)
    print("\n".join(mem_lines(snap, since)))
    print("mem: " + json.dumps({"snapshot": snap, "since": since}, sort_keys=True))


def latency_setup(name):
    """Return the latency setup NAME: off, local or far; an empty NAME is off, another one "?"."""
    if not name:
        return "off"
    return name if name in ("off", "local", "far") else "?"


def latency_label(rec):
    """Return the latency setup of the saved result REC: off, local or far, with "+LEVEL" for a background load."""
    load = rec.get("latencyLoad") or "off"
    return latency_setup(rec.get("latency") or "?") + ("" if load == "off" else "+" + load)


def latency_status(state):
    """Return the lines of "latency.sh status" for the VMs and the pool directory of STATE, or why there are none."""
    env = dict(os.environ, DEMO_VM_DIRS=" ".join(state.get(k) or "" for _, k in VMS).strip(),
               DEMO_POOL_DIR=state.get("DEMO_POOL_DIR") or "", DEMO_STATE_DIR=c.STATE_DIR)
    try:
        p = subprocess.run([LATENCY, "status"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=env, timeout=60)
    except (OSError, subprocess.TimeoutExpired) as e:
        return "latency.sh status: %s" % e
    return p.stdout.decode(errors="replace").rstrip("\n")


def save_bench(state, bench, access):
    """Return the file in RESULTS_DIR where the BENCH result of readers of ACCESS is saved, with its context.

    The context: the model, the access, the latency setup, its background load and its status,
    the mount of the pool directory and how much of llm0 the qemus map in
    2 MiB pages, the setup.
    """
    vms = {vm: {"mapping": file_mapping(pid, state.get("POOL_DEVICE_DATA") or "") if pid else {}}
           for vm, pid in ((vm, qemu_pid(state.get(key) or "")) for vm, key in VMS)}
    rec = {"bench": bench, "access": access, "model": state.get("DEMO_LLM_MODEL"), "modelFile": state.get("LLM_MODEL_FILE"),
           "latency": latency_setup(state.get("DEMO_LLM_LATENCY")), "latencyLoad": state.get("DEMO_LLM_LATENCY_LOAD") or "off",
           "latencyStatus": latency_status(state),
           "pool": dict(pool_mount(state), mappings={vm: v["mapping"] for vm, v in vms.items()}, pmdShare=pmd_share(vms)),
           "setup": state.get("SETUP_OUTPUT_DIR"), "saved": time.time()}
    os.makedirs(RESULTS_DIR, exist_ok=True)
    name = "%s-%s-latency-%s%s-%s-%s.json" % (time.strftime("%Y%m%d-%H%M%S"), rec["model"], rec["latency"],
                                              "" if rec["latencyLoad"] == "off" else "-load" + rec["latencyLoad"], access, bench.get("mode"))
    path = os.path.join(RESULTS_DIR, name)
    with open(path, "w") as f:
        json.dump(rec, f, indent=1, sort_keys=True)
    return path


def result_rows(rec):
    """Return the table rows of a saved bench result REC, one per reader."""
    b = rec["bench"]
    rows = []
    for r in b.get("readers") or []:
        p = b["perReader"].get(r) or {}

        def ms(key):
            st = p.get(key)
            if not st:
                return "-"
            return "%.0f (%.0f-%.0f)" % (st["median"], st["min"], st["max"])
        tpot = p.get("tpotMs") or {}
        pool = rec.get("pool") or {}
        share = pool.get("pmdShare")
        rows.append([rec.get("model") or "?", latency_label(rec), pool.get("huge") or "?",
                     "-" if share is None else "%d%%" % round(100 * share), rec.get("access") or "?", b.get("mode") or "?",
                     r, ms("ttftMs"), ms("promptMs"), ms("tpotMs"),
                     "%.1f" % (1000 / tpot["median"]) if tpot.get("median") else "-", str(p.get("errors", 0))])
    return rows


def cmd_results(files):
    """Print the table of the saved bench results FILES (default: all of RESULTS_DIR), oldest first."""
    files = files or sorted(glob.glob(os.path.join(RESULTS_DIR, "*.json")))
    if not files:
        sys.exit("no bench results in %s: run the bench steps of llm-share" % RESULTS_DIR)
    head = ["model", "latency", "huge", "2M", "access", "mode", "reader", "TTFT ms", "prompt ms", "TPOT ms", "tok/s", "failed"]
    rows = []
    for path in files:
        with open(path) as f:
            rows += result_rows(json.load(f))
    width = [max(len(r[i]) for r in [head] + rows) for i in range(len(head))]
    for r in [head] + rows:
        print("  ".join(v.ljust(width[i]) for i, v in enumerate(r)).rstrip())
    print("TTFT: request sent -> first streamed delta (client); prompt: the server's prompt time; TPOT: the server's time")
    print("per token of a tool-call answer, else the client's; median (min-max) of the samples of each reader;")
    print("latency: the setup (off: VMs unpinned; local: VMs and model on socket 0; far: model on socket 1, +LEVEL: load)")
    print("huge: the huge= option of the pool tmpfs; 2M: how much of llm0 the qemus map in 2 MiB pages")


def main():
    args = sys.argv[1:]
    if args[:1] == ["results"]:
        return cmd_results(args[1:])
    if args[:1] == ["mem"] and len(args) in (2, 4) and (len(args) == 2 or args[2] == "--since"):
        for label in args[1:2] + args[3:]:
            if not re.fullmatch(r"[A-Za-z0-9_.-]+", label):
                sys.exit("LABEL %r: letters, digits, dot, dash and underscore only" % label)
        state = c.load_state()
        if state is None or state.get("DEMO_CASE") != "llm-share":
            sys.exit("the demo is not set up for llm-share: demo setup llm-share")
        return cmd_mem(state, args[1], args[3] if len(args) == 4 else None)
    sys.exit(__doc__)


if __name__ == "__main__":
    main()
