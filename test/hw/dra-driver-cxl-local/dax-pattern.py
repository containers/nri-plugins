#!/usr/bin/env python3
"""Write, verify or check zeroes of a whole devdax device.

Usage:
  dax-pattern.py write  DEV SEED   write SEED and the page index into the first
                                   16 bytes of every 4 KiB page of DEV
  dax-pattern.py verify DEV SEED   check what "write" wrote, read-only
  dax-pattern.py zero   DEV        check that every byte of DEV is zero, read-only

DEV is /dev/daxX.Y; the size comes from sysfs. The device is mapped
MAP_SHARED (devdax refuses private mappings), read-only for verify and
zero. Prints one line:

  dax-pattern: write DEV pages P in S s
  dax-pattern: verify DEV pages P bad B in S s
  dax-pattern: zero DEV MiB M non-zero MiB Z in S s

Exit status 1 if verify finds bad pages or zero finds non-zero bytes.
"""
import mmap
import os
import struct
import sys
import time

PAGE = 4096
MIB = 1 << 20


def dax_size(dev):
    name = os.path.basename(dev)
    with open("/sys/bus/dax/devices/%s/size" % name) as f:
        return int(f.read())


def main():
    if len(sys.argv) < 3 or sys.argv[1] not in ("write", "verify", "zero"):
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    op, dev = sys.argv[1], sys.argv[2]
    seed = int(sys.argv[3], 0) if len(sys.argv) > 3 else 0
    size = dax_size(dev)
    write = op == "write"
    fd = os.open(dev, os.O_RDWR if write else os.O_RDONLY)
    prot = mmap.PROT_READ | (mmap.PROT_WRITE if write else 0)
    m = mmap.mmap(fd, size, mmap.MAP_SHARED, prot)
    t = time.time()
    if op == "write":
        for i, off in enumerate(range(0, size, PAGE)):
            m[off:off + 16] = struct.pack("<QQ", i, seed)
        print("dax-pattern: write %s pages %d in %.1f s" % (dev, size // PAGE, time.time() - t))
        return
    if op == "verify":
        bad = sum(1 for i, off in enumerate(range(0, size, PAGE))
                  if struct.unpack_from("<QQ", m, off) != (i, seed))
        print("dax-pattern: verify %s pages %d bad %d in %.1f s" % (dev, size // PAGE, bad, time.time() - t))
        sys.exit(1 if bad else 0)
    zero = bytes(MIB)
    nonzero = sum(1 for off in range(0, size, MIB) if m[off:off + MIB] != zero[:min(MIB, size - off)])
    print("dax-pattern: zero %s MiB %d non-zero MiB %d in %.1f s" % (dev, size // MIB, nonzero, time.time() - t))
    sys.exit(1 if nonzero else 0)


if __name__ == "__main__":
    main()
