#!/usr/bin/env python3
# Copyright The NRI Plugins Authors. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Evaluate a Python expression on a JSON document, for the e2e helpers.

Usage: e2e_json.py assert|eval KIND EXPRESSION [FILE]

The document is FILE, or stdin without FILE. KIND gives the names that
EXPRESSION can use besides the document j, json, decimal and node_list:

  k8s   kubectl JSON and the state document of the CXL DRA driver
  pool  the JSON of a CXL memory pool server: j only
  cxl   the output of cxl-dump (cxl-py of cxl.source.sh lists the names)

assert exits 0 if the value is true, 1 if not (cxl prints why on
stderr); eval prints the value. Invalid JSON exits 2.
"""

import decimal
import json
import sys


def node_list(text):
    """The set of NUMA nodes of a node list such as "0-1,3"."""
    out = set()
    for part in text.strip().split(","):
        if part:
            lo, _, hi = part.partition("-")
            out.update(range(int(lo), int(hi or lo) + 1))
    return out


def k8s(j):
    """The names of kind k8s."""
    items = j.get("items", [j]) if isinstance(j, dict) else j
    items = items or []

    def names():
        return sorted(o["metadata"]["name"] for o in items)

    def by_name(name):
        return next((o for o in items if o["metadata"]["name"] == name), None)

    def slices(driver=None, node=None):
        return [s for s in items
                if (driver is None or s["spec"].get("driver") == driver)
                and (node is None or s["spec"].get("nodeName") == node)]

    def pool_slices(pool, driver="cxl-pool.nri.io"):
        return [s for s in slices(driver) if (s["spec"].get("pool") or {}).get("name") == pool]

    def pool_nodes(pool, driver="cxl-pool.nri.io"):
        sels = [s["spec"].get("nodeSelector") for s in pool_slices(pool, driver) if not s["spec"].get("allNodes")]
        if not sels or len(sels) != len(pool_slices(pool, driver)) or any(sel != sels[0] for sel in sels):
            return None
        terms = (sels[0] or {}).get("nodeSelectorTerms") or []
        if len(terms) != 1 or sorted(terms[0]) != ["matchExpressions"] or len(terms[0]["matchExpressions"]) != 1:
            return None
        e = terms[0]["matchExpressions"][0]
        if e.get("key") != "kubernetes.io/hostname" or e.get("operator") != "In":
            return None
        return sorted(e.get("values") or [])

    def devices(driver=None, node=None):
        return [d for s in slices(driver, node) for d in s["spec"].get("devices") or []]

    def device(name, driver=None, node=None):
        return next((d for d in devices(driver, node) if d["name"] == name), None)

    def attr(dev, name):
        attrs = (dev or {}).get("attributes") or {}
        a = attrs.get(name)
        if a is None:
            a = next((v for k, v in attrs.items() if k.endswith("/" + name)), None)
        if a is None:
            return None
        return next(iter(a.values()))

    def capacity(dev, name):
        c = ((dev or {}).get("capacity") or {}).get(name)
        return c.get("value") if c else None

    def request_policy(dev, name):
        c = ((dev or {}).get("capacity") or {}).get(name)
        return c.get("requestPolicy") if c else None

    MiB = 1 << 20
    SUFFIXES = [("Ki", 1 << 10), ("Mi", 1 << 20), ("Gi", 1 << 30), ("Ti", 1 << 40), ("Pi", 1 << 50), ("Ei", 1 << 60),
                ("k", 10 ** 3), ("M", 10 ** 6), ("G", 10 ** 9), ("T", 10 ** 12), ("P", 10 ** 15), ("E", 10 ** 18)]

    def quantity(q):
        q = str(q).strip()
        for suffix, mult in SUFFIXES:
            if q.endswith(suffix):
                return int(decimal.Decimal(q[:-len(suffix)]) * mult)
        return int(decimal.Decimal(q))

    def results(claim=None):
        claim = claim or j
        alloc = (claim.get("status") or {}).get("allocation") or {}
        return (alloc.get("devices") or {}).get("results") or []

    def consumed(claim=None, name="memory"):
        return sum(quantity((r.get("consumedCapacity") or {}).get(name, 0)) for r in results(claim))

    def allocated_node(claim=None):
        claim = claim or j
        alloc = (claim.get("status") or {}).get("allocation") or {}
        for term in (alloc.get("nodeSelector") or {}).get("nodeSelectorTerms") or []:
            for f in term.get("matchFields") or []:
                if f.get("key") == "metadata.name" and f.get("values"):
                    return f["values"][0]
        return None

    def status_devices(claim=None):
        claim = claim or j
        return (claim.get("status") or {}).get("devices") or []

    def condition(ctype, claim=None):
        for d in status_devices(claim):
            for c in d.get("conditions") or []:
                if c.get("type") == ctype:
                    return c.get("status")
        return None

    def device_data(dev, claim=None):
        return next((d.get("data") for d in status_devices(claim) if d.get("device") == dev), None)

    def subrequests(claim=None):
        claim = claim or j
        out = []
        for r in claim["spec"]["devices"].get("requests") or []:
            if r.get("exactly"):
                out.append(dict(r["exactly"], name=r["name"]))
            for s in r.get("firstAvailable") or []:
                out.append(dict(s, name=r["name"] + "/" + s["name"]))
        return out

    def configs(kind=None, claim=None):
        claim = claim or j
        return [dict(c["opaque"], requests=c.get("requests")) for c in claim["spec"]["devices"].get("config") or []
                if c.get("opaque") and (kind is None or c["opaque"]["parameters"].get("kind") == kind)]

    def config_section(c, name):
        return (c.get("parameters") or {}).get(name) or {}

    def events(reason=None, name=None):
        return [e for e in items
                if (reason is None or e.get("reason") == reason)
                and (name is None or e["involvedObject"].get("name") == name)]

    def dax_slices(driver=None):
        return [d for r in j.get("regions") or [] for d in r.get("dax") or []
                if d.get("size") and (driver is None or d.get("driver") == driver)]

    def grant(ref):
        return next((g for g in j.get("grants") or [] if g.get("ref") == ref), None)

    def region(ref):
        return next((r for r in j.get("regions") or [] if (r.get("name") or r.get("notedName")) == ref), None)

    def serial_number(s):
        return s if isinstance(s, int) else int(str(s), 16)

    def memdev(serial):
        return next((m for m in j.get("memdevs") or [] if serial_number(m["serial"]) == serial_number(serial)), None)

    def local_memdevs():
        return [m for m in j.get("memdevs") or [] if m.get("source") == "local"]

    def claim_uses(m, uid):
        gs = [grant(r) for r in m.get("grants") or [] if r.get("claim") == uid]
        return [g for g in gs if g and g.get("state") in ("REALISED", "REALISED_UNRESTORED", "RELEASING")]

    def local_claims(m):
        out = []
        for r in m.get("grants") or []:
            g = grant(r)
            if g and g.get("state") in ("REALISED", "REALISED_UNRESTORED", "RELEASING", "JOURNALED", "REALISING") \
                    and r.get("claim") not in out:
                out.append(r.get("claim"))
        regs = [reg for reg in (region(x) for x in m.get("regions") or []) if reg and reg.get("owner") == "ours"]
        if any(not d.get("whole") and d.get("state") != "SEED" and not d.get("users") for reg in regs for d in reg.get("daxDevices") or []):
            out.append("-")
        return out

    def lease(lid):
        return next((l for l in j.get("leases") or [] if l.get("id") == lid), None)
    return locals()


def pool(j):
    """The names of kind pool."""
    return {"j": j}


def cxl(d):
    """The names of kind cxl."""
    memdevs = d.get("memdevs") or []
    regions = d.get("regions") or []
    daxdevs = d.get("daxDevices") or []
    blocks = d.get("blocks") or []
    nodes = d.get("numaNodes") or []
    block_size = d.get("blockSize") or 0
    for m in memdevs:
        for k, v in (("ramSize", 0), ("pmemSize", 0), ("major", 0), ("minor", 0), ("endpoint", ""), ("ports", []), ("regions", [])):
            m.setdefault(k, v)
    for r in regions:
        for k, v in (("locked", False), ("targets", []), ("memdevs", []), ("serials", []), ("rootDecoder", ""), ("daxRegion", ""), ("ndRegion", "")):
            r.setdefault(k, v)
    for x in daxdevs:
        for k, v in (("driver", ""), ("major", 0), ("minor", 0)):
            x.setdefault(k, v)
    for b in blocks:
        b.setdefault("zones", "")
    endpoints = [m["endpoint"] for m in memdevs if m["endpoint"]]

    MiB = 1 << 20

    def by_name(objs, name):
        """Object called name, None if there is no such object."""
        return next((o for o in objs if o.get("name") == name), None)

    def by_serial(serial):
        """Memory device with the serial number, None if it is not there."""
        return next((m for m in memdevs if m.get("serial") == serial), None)

    def names(objs):
        """Sorted names of the objects."""
        return sorted(o["name"] for o in objs or [])

    def node(node_id):
        """NUMA node with the id, None if the node is not online."""
        return next((n for n in nodes if n.get("id") == node_id), None)

    def cpu_nodes():
        """Ids of the NUMA nodes of the CPUs, that is, all but the CXL ones."""
        return sorted(set(n["id"] for n in nodes) - set(r["numaNode"] for r in regions))

    def enabled(o):
        """True if a memdev has its driver or a region is enabled."""
        return o["state"] in ("BOUND", "READY", "ENABLED")

    def size(r):
        """Size of the address range of a region in bytes, 0 without a range."""
        rg = r["range"]
        return rg["end"] - rg["start"] + 1 if rg["end"] else 0

    def online_size(r):
        """Bytes of the memory blocks in the address range of a region that are online."""
        rg = r["range"]
        if not rg["end"] or not block_size:
            return 0
        return block_size * sum(1 for b in blocks if b["state"] == "ONLINE" and
                                rg["start"] <= b["index"] * block_size and (b["index"] + 1) * block_size - 1 <= rg["end"])

    def summary():
        """One line description of the dump."""
        return "%d memdevs (%s), %d regions (%s), %d endpoints, nodes %s" % (
            len(memdevs),
            ", ".join("%s%s" % (m["name"], "" if enabled(m) else " disabled") for m in memdevs),
            len(regions),
            ", ".join("%s node %d %d/%dM online" %
                      (r["name"], r["numaNode"], online_size(r) // MiB, size(r) // MiB)
                      for r in regions),
            len(endpoints),
            ", ".join("%d:%dM" % (n["id"], n["memTotal"] // MiB) for n in nodes),
        )
    return locals()


KINDS = {"k8s": k8s, "pool": pool, "cxl": cxl}


def main(argv):
    mode, kind, expression = argv[1], argv[2], argv[3]
    path = argv[4] if len(argv) > 4 else None
    text = open(path).read() if path else sys.stdin.read()
    try:
        doc = json.loads(text)
    except ValueError as e:
        print("%s: invalid JSON (%s): %s" % (kind, e, text[:2000]), file=sys.stderr)
        return 2
    names = {"json": json, "decimal": decimal, "node_list": node_list, "j": doc}
    names.update(KINDS[kind](doc))
    value = eval(expression, names)
    if mode == "eval":
        print(value)
        return 0
    if value:
        return 0
    if kind == "cxl":
        print("expected true: %s" % expression, file=sys.stderr)
        print("in %s: %s" % (path, names["summary"]()), file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
