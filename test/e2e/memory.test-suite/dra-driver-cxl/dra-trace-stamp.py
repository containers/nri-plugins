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

"""Stamp lines with the host time of their arrival (dra-trace-start).

Usage: dra-trace-stamp.py TAG lines|claims

lines:  print "<epoch seconds.micro> TAG <line>" for every input line.
claims: read the JSON stream of
        "kubectl get resourceclaims -w --output-watch-events -o json"
        and print one line per change of a claim: event, name,
        allocated devices, node, reserving pods, device conditions.
"""
import json
import sys
import time

tag, mode = sys.argv[1], sys.argv[2]


def emit(text):
    sys.stdout.write("%.6f %s %s\n" % (time.time(), tag, text))
    sys.stdout.flush()


def claim_line(obj):
    o = obj.get("object", obj)
    md = o.get("metadata") or {}
    st = o.get("status") or {}
    alloc = st.get("allocation") or {}
    res = (alloc.get("devices") or {}).get("results") or []
    devs = ",".join("%s/%s/%s" % (r.get("driver"), r.get("pool"), r.get("device")) for r in res) or "-"
    node = "-"
    for term in (alloc.get("nodeSelector") or {}).get("nodeSelectorTerms") or []:
        for f in term.get("matchFields") or []:
            node = ",".join(f.get("values") or []) or node
    reserved = ",".join(r.get("name", "?") for r in st.get("reservedFor") or []) or "-"
    conds = []
    for d in st.get("devices") or []:
        for c in d.get("conditions") or []:
            conds.append("%s:%s=%s" % (d.get("device"), c.get("type"), c.get("status")))
        data = d.get("data") or {}
        if "serial" in data:
            conds.append("%s:serial=%s" % (d.get("device"), data["serial"]))
    text = "claim %s/%s uid=%s allocated=%s node=%s reservedFor=%s status=%s" % (
        md.get("namespace"), md.get("name"), (md.get("uid") or "")[:8],
        devs, node, reserved, ",".join(conds) or "-")
    if md.get("deletionTimestamp"):
        text += " deleting"
    return "%s/%s" % (md.get("namespace"), md.get("name")), obj.get("type", "?"), text


def lines():
    for raw in iter(sys.stdin.buffer.readline, b""):
        emit(raw.decode(errors="replace").rstrip("\r\n"))


def claims():
    decoder = json.JSONDecoder()
    buf = ""
    last = {}
    for raw in iter(sys.stdin.buffer.readline, b""):
        line = raw.decode(errors="replace")
        buf += line
        # kubectl prints one event per line (--output-watch-events) or
        # indented objects: try to decode whenever a line ends an object.
        if not line.rstrip().endswith("}"):
            continue
        while True:
            s = buf.lstrip()
            try:
                obj, end = decoder.raw_decode(s)
            except ValueError:
                buf = s
                break
            buf = s[end:]
            key, event, text = claim_line(obj)
            if event == "MODIFIED" and last.get(key) == text:
                continue
            last[key] = text
            if event == "DELETED":
                last.pop(key, None)
            emit("%s %s" % (event, text))
        if len(buf) > (1 << 24):
            emit("unparsable watch output: %s" % buf[:200].replace("\n", " "))
            buf = ""


{"lines": lines, "claims": claims}[mode]()
