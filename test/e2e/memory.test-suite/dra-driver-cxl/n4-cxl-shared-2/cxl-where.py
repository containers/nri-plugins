#!/usr/bin/env python3
"""Print where the page frames of the file mappings of a process are.

Usage: cxl-where.py PID [PATH...]

For every file mapping of process PID, or only those of the files PATH (the
path as /proc/PID/maps shows it), print one JSON object per line:

  {"path": "/dev/dax2.0", "start": "0x7f...", "size": 398458880,
   "pages": 97280, "present": 97280,
   "top": {"CXL Window 1": 97280},
   "where": {"CXL Window 1 > region2 > dax2.0": 97280}}

pages is the number of 4 KiB pages of the mapping and present the number of
them that have a page frame now. top counts the present pages by the
top-level resource of /proc/iomem that holds their frame ("System RAM" for
DRAM, the page cache included; "CXL Window N" for CXL memory), where by the
whole chain of resources down to the innermost one. Every page of the
mapping is counted.

Run as root in the initial namespaces of the node: for others the kernel
shows page frame numbers and /proc/iomem as zeros.
"""
import bisect
import json
import os
import sys

PAGE = 4096
PRESENT = 1 << 63
PFN_MASK = (1 << 55) - 1


def iomem_segments():
    """Return sorted, disjoint (start, end, chain) segments of /proc/iomem.

    chain is the list of resource names from the top level down to the
    innermost resource that holds the segment.
    """
    root = {"start": 0, "end": (1 << 64) - 1, "name": None, "children": []}
    stack = [(-1, root)]
    with open("/proc/iomem") as f:
        for line in f:
            if not line.strip():
                continue
            depth = (len(line) - len(line.lstrip(" "))) // 2
            span, _, name = line.strip().partition(" : ")
            start, _, end = span.partition("-")
            node = {"start": int(start, 16), "end": int(end, 16), "name": name, "children": []}
            while stack[-1][0] >= depth:
                stack.pop()
            stack[-1][1]["children"].append(node)
            stack.append((depth, node))
    if all(n["start"] == 0 and n["end"] == 0 for n in root["children"]):
        sys.exit("cxl-where.py: /proc/iomem shows no addresses: run as root")
    segments = []

    def walk(node, chain):
        at = node["start"]
        for child in sorted(node["children"], key=lambda n: n["start"]):
            if child["start"] > at and chain:
                segments.append((at, child["start"] - 1, chain))
            walk(child, chain + [child["name"]])
            at = max(at, child["end"] + 1)
        if at <= node["end"] and chain:
            segments.append((at, node["end"], chain))

    walk(root, [])
    segments.sort()
    return segments


def mappings(pid):
    """Return (start, end, path) of the file mappings of process pid."""
    out = []
    with open("/proc/%d/maps" % pid) as f:
        for line in f:
            fields = line.split(None, 5)
            if len(fields) < 6 or fields[4] == "0":
                continue
            start, _, end = fields[0].partition("-")
            out.append((int(start, 16), int(end, 16), fields[5].rstrip("\n")))
    return out


def where(pid, start, end, segments):
    """Return the page count, present count, and counts by top-level and by
    chain of the pages of [start, end) of process pid."""
    starts = [s[0] for s in segments]
    pages = (end - start) // PAGE
    present = 0
    top, chains = {}, {}
    fd = os.open("/proc/%d/pagemap" % pid, os.O_RDONLY)
    try:
        raw = os.pread(fd, pages * 8, (start // PAGE) * 8)
    finally:
        os.close(fd)
    pfns = 0
    for i in range(0, len(raw) // 8):
        entry = int.from_bytes(raw[i * 8:i * 8 + 8], "little")
        if not entry & PRESENT:
            continue
        present += 1
        phys = (entry & PFN_MASK) * PAGE
        pfns += phys != 0
        k = bisect.bisect_right(starts, phys) - 1
        if k >= 0 and segments[k][0] <= phys <= segments[k][1]:
            chain = segments[k][2]
        else:
            chain = ["(no resource)"]
        top[chain[0]] = top.get(chain[0], 0) + 1
        name = " > ".join(chain)
        chains[name] = chains.get(name, 0) + 1
    if present and not pfns:
        sys.exit("cxl-where.py: /proc/%d/pagemap shows no page frame numbers: run as root" % pid)
    return pages, present, top, chains


def main():
    if sys.argv[1:2] in (["-h"], ["--help"]):
        print(__doc__)
        return
    if len(sys.argv) < 2 or not sys.argv[1].isdigit():
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    pid = int(sys.argv[1])
    paths = set(sys.argv[2:])
    segments = iomem_segments()
    found = False
    for start, end, path in mappings(pid):
        if paths and path not in paths:
            continue
        found = True
        pages, present, top, chains = where(pid, start, end, segments)
        print(json.dumps({"path": path, "start": hex(start), "size": end - start, "pages": pages,
                          "present": present, "top": top, "where": chains}, sort_keys=True))
    if paths and not found:
        sys.exit("cxl-where.py: process %d has no mapping of %s" % (pid, " ".join(sorted(paths))))


if __name__ == "__main__":
    main()
