#!/usr/bin/env python3
"""Allocate and touch anonymous memory with the default memory policy.

Usage:
  alloc-touch.py [--hold] MIB

Allocate MIB MiB of private anonymous memory without setting a memory
policy (whatever the container's cpuset.mems and the policy that the
driver set on the container's threads say), write a byte to every page,
read every page back to verify it, and print the 4 KiB pages per NUMA node
from /proc/self/numa_maps:

  alloc-touch: pid P size MIB MiB mems_allowed LIST
  alloc-touch: pages N0=... N3=...
  alloc-touch: verify ok (or: verify FAILED: N of M pages differ)
  alloc-touch: ready

Every page gets its own index and the pid in its first 16 bytes, so that
memory that does not hold what was written (an unmapped or broken range of
physical memory) is noticed. Exit status 1 if verification fails.

With --hold, keep the memory until SIGTERM and print the "pages" line again
on every SIGUSR1.
"""
import ctypes
import mmap
import os
import signal
import struct
import sys

PAGE = mmap.PAGESIZE


def say(text):
    print("alloc-touch: " + text, flush=True)


def vma_start(addr):
    with open("/proc/self/maps") as f:
        for line in f:
            lo, hi = (int(x, 16) for x in line.split()[0].split("-"))
            if lo <= addr < hi:
                return lo
    return addr


def pages(start):
    start = vma_start(start)
    with open("/proc/self/numa_maps") as f:
        for line in f:
            fields = line.split()
            if int(fields[0], 16) != start:
                continue
            per_node = {k: int(v) for k, v in
                        (w.split("=", 1) for w in fields[2:] if w[:1] == "N" and "=" in w)}
            return " ".join("%s=%d" % (k, per_node[k]) for k in sorted(per_node, key=lambda k: int(k[1:]))) or "none"
    return "mapping not found"


def mems_allowed():
    with open("/proc/self/status") as f:
        for line in f:
            if line.startswith("Mems_allowed_list:"):
                return line.split()[1]
    return "?"


def main():
    args = sys.argv[1:]
    hold = args[:1] == ["--hold"]
    if hold:
        args = args[1:]
    if len(args) != 1:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    size = int(args[0]) << 20
    m = mmap.mmap(-1, size, flags=mmap.MAP_PRIVATE | mmap.MAP_ANONYMOUS,
                  prot=mmap.PROT_READ | mmap.PROT_WRITE)
    start = ctypes.addressof(ctypes.c_char.from_buffer(m))
    pid = os.getpid()
    for i, off in enumerate(range(0, size, PAGE)):
        m[off:off + 16] = struct.pack("<QQ", i, pid)
    say("pid %d size %d MiB mems_allowed %s" % (pid, size >> 20, mems_allowed()))
    say("pages " + pages(start))
    bad = sum(1 for i, off in enumerate(range(0, size, PAGE))
              if struct.unpack_from("<QQ", m, off) != (i, pid))
    if bad:
        say("verify FAILED: %d of %d pages differ" % (bad, size // PAGE))
    else:
        say("verify ok")
    say("ready")
    if bad:
        sys.exit(1)
    if not hold:
        return
    signal.signal(signal.SIGUSR1, lambda s, f: say("pages " + pages(start)))
    signal.signal(signal.SIGTERM, lambda s, f: sys.exit(0))
    while True:
        signal.pause()


if __name__ == "__main__":
    main()
