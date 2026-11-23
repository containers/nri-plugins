#!/usr/bin/env python3
"""Allocate memory that prefers one NUMA node, touch it, and hold it.

Usage:
  numa-touch.py [--hold] MIB NODE|-

Set the memory policy of the process to MPOL_PREFERRED on NODE
(set_mempolicy(2) through ctypes: plain libc syscall(), so it works with
musl in python:3-alpine, no libnuma), or with "-" keep the policy that the
process inherited, allocate MIB MiB of private anonymous memory, write a
byte to every page, and print the policy of the memory and where the pages
are:

  numa-touch: pid P size MIB MiB preferred node NODE (or: inherited policy)
  numa-touch: policy POLICY (as in /proc/self/numa_maps: default, prefer:2, ...)
  numa-touch: pages N0=... N2=... (4 KiB pages per node, /proc/self/numa_maps)
  numa-touch: ready

With --hold, keep the memory until SIGTERM or SIGINT, and print the
"pages" line again on every SIGUSR1: the pages move when the cpuset of the
container shrinks, and the process lives on. Exit status 1 if NODE is not
allowed (set_mempolicy fails with EINVAL when NODE is not in
Mems_allowed), 2 on usage errors.
"""
import ctypes
import mmap
import os
import platform
import signal
import sys

MPOL_PREFERRED = 1
SYS_SET_MEMPOLICY = {"x86_64": 238, "aarch64": 237}
PAGE = mmap.PAGESIZE


def say(text):
    print("numa-touch: " + text, flush=True)


def set_preferred(node):
    nr = SYS_SET_MEMPOLICY.get(platform.machine())
    if nr is None:
        sys.exit("numa-touch: no set_mempolicy syscall number for %s" % platform.machine())
    maxnode = max(64, node + 1)
    words = (maxnode + 63) // 64
    mask = (ctypes.c_ulong * words)()
    mask[node // 64] = 1 << (node % 64)
    libc = ctypes.CDLL(None, use_errno=True)
    libc.syscall.restype = ctypes.c_long
    if libc.syscall(ctypes.c_long(nr), ctypes.c_int(MPOL_PREFERRED), mask,
                    ctypes.c_ulong(words * 64 + 1)) != 0:
        err = ctypes.get_errno()
        say("set_mempolicy(MPOL_PREFERRED, node %d) failed: %s" % (node, os.strerror(err)))
        sys.exit(1)


def vma_start(addr):
    """Start of the VMA that contains addr, from /proc/self/maps: the kernel
    may merge the mapping with an anonymous neighbour, then it starts
    below the address that mmap returned."""
    with open("/proc/self/maps") as f:
        for line in f:
            lo, hi = (int(x, 16) for x in line.split()[0].split("-"))
            if lo <= addr < hi:
                return lo
    return addr


def numa_map(start):
    """The fields of the line of the mapping at start in
    /proc/self/numa_maps, None if there is none."""
    start = vma_start(start)
    with open("/proc/self/numa_maps") as f:
        for line in f:
            fields = line.split()
            if int(fields[0], 16) == start:
                return fields
    return None


def policy(start):
    """Memory policy of the mapping at start: its own or the process's."""
    fields = numa_map(start)
    return fields[1] if fields else "mapping not found"


def pages(start):
    """Pages per node of the mapping at start, from /proc/self/numa_maps."""
    fields = numa_map(start)
    if fields is None:
        return "mapping not found"
    per_node = {k: int(v) for k, v in
                (w.split("=", 1) for w in fields[2:] if w[:1] == "N" and "=" in w)}
    return " ".join("%s=%d" % (k, per_node[k]) for k in sorted(per_node, key=lambda k: int(k[1:]))) or "none"


def main():
    args = sys.argv[1:]
    hold = args[:1] == ["--hold"]
    if hold:
        args = args[1:]
    if len(args) != 2:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    mib, node = int(args[0]), None if args[1] == "-" else int(args[1])
    if node is not None:
        set_preferred(node)
    size = mib << 20
    m = mmap.mmap(-1, size, flags=mmap.MAP_PRIVATE | mmap.MAP_ANONYMOUS,
                  prot=mmap.PROT_READ | mmap.PROT_WRITE)
    start = ctypes.addressof(ctypes.c_char.from_buffer(m))
    for off in range(0, size, PAGE):
        m[off] = 1
    if node is None:
        say("pid %d size %d MiB inherited policy" % (os.getpid(), mib))
    else:
        say("pid %d size %d MiB preferred node %d" % (os.getpid(), mib, node))
    say("policy " + policy(start))
    say("pages " + pages(start))
    say("ready")
    if not hold:
        return
    signal.signal(signal.SIGUSR1, lambda s, f: say("pages " + pages(start)))
    signal.signal(signal.SIGTERM, lambda s, f: sys.exit(0))
    while True:
        signal.pause()


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        pass
