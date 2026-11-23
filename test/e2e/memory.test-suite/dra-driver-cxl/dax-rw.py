#!/usr/bin/env python3
"""Write and read strings in CXL memory through a dax device.

Run in a VM as root (from /usr/local/bin) or in a pod (from /tools).

Usage:
  dax-rw.py write DEV OFFSET TEXT        write TEXT and a NUL byte at OFFSET
  dax-rw.py read DEV OFFSET              print the NUL-terminated string at OFFSET
  dax-rw.py watch DEV OFFSET STOPFILE    print "read: STRING" and "holding", keep
                                              the device mapped read-only, print
                                              "changed: STRING" when it changes, and
                                              "final: STRING" when STOPFILE exists
  dax-rw.py check-ro DEV OFFSET          exit 0 if the device cannot be written:
                                              open for writing (DEV) and a writable
                                              mapping are refused; print the errors
  dax-rw.py --fd N OP OFFSET [ARG]       the same through the open fd N

DEV is /dev/daxX.Y. OFFSET is in bytes, or with a K, M or G suffix.
read, watch and check-ro open DEV read-only and map it read-only: they
work on a device node that the container may only read (a read-only
claim), and on a read-only fd (a read-only lease).

With --fd N the device is not opened: N is a file descriptor that the
process inherited, for instance from "cxl-request request --serial SERIAL
--share --exec" (fd 3, CXL_<KEY>_FD=3), which received it from
nri-dra-driver-cxl over a unix socket. The container needs no device node
for it. The size of the mapping comes from the sysfs of the device the fd
refers to, from CXL_<KEY>_SIZE of the KEY whose CXL_<KEY>_FD is N, or, for
a regular file (tests), from its size.

The whole device is mapped MAP_SHARED: devdax refuses private mappings and
mappings that are not aligned to the device alignment (2M). The mapping
goes straight to the CXL memory, that is, to the backing file of the memory
device on the host, so every VM that has the same shared device attached
sees a write at once.
"""
import errno
import mmap
import os
import stat
import sys
import time

MAX_STRING = 4096
UNITS = {"K": 1 << 10, "M": 1 << 20, "G": 1 << 30}


def parse_offset(s):
    if s and s[-1].upper() in UNITS:
        return int(s[:-1], 0) * UNITS[s[-1].upper()]
    return int(s, 0)


def dax_size(name):
    with open("/sys/bus/dax/devices/%s/size" % os.path.basename(name)) as f:
        return int(f.read())


def env_size(fd):
    """Return CXL_<KEY>_SIZE of the KEY whose CXL_<KEY>_FD is fd, or None."""
    for name, value in os.environ.items():
        if name.startswith("CXL_") and name.endswith("_FD") and value == str(fd):
            size = os.environ.get(name[:-len("_FD")] + "_SIZE")
            if size:
                return int(size, 0)
    return None


def fd_size(fd):
    st = os.fstat(fd)
    if stat.S_ISREG(st.st_mode):
        return st.st_size
    try:
        return dax_size(os.readlink("/proc/self/fd/%d" % fd))
    except OSError:
        pass
    size = env_size(fd)
    if size is not None:
        return size
    sys.exit("cannot find the size of fd %d: no sysfs entry, no CXL_<KEY>_SIZE with CXL_<KEY>_FD=%d" % (fd, fd))


def string_at(m, offset):
    return bytes(m[offset:offset + MAX_STRING]).split(b"\0", 1)[0].decode(errors="replace")


def refused(what, f):
    """Return True and print the error if f() fails with EACCES or EPERM."""
    try:
        f()
    except PermissionError as e:
        print("%s refused: %s" % (what, errno.errorcode.get(e.errno, e.errno)), flush=True)
        return True
    print("%s: allowed" % what, flush=True)
    return False


def check_ro(dev, fd, own, size):
    ok = True
    if own:
        ok = refused("open for writing", lambda: os.close(os.open(dev, os.O_RDWR)))
    ok = refused("writable mapping", lambda: mmap.mmap(fd, size, mmap.MAP_SHARED, mmap.PROT_READ | mmap.PROT_WRITE).close()) and ok
    return ok


def watch(m, offset, stopfile):
    text = string_at(m, offset)
    print("read: " + text, flush=True)
    print("holding", flush=True)
    while not os.path.exists(stopfile):
        time.sleep(0.2)
        now = string_at(m, offset)
        if now != text:
            text = now
            print("changed: " + text, flush=True)
    print("final: " + text, flush=True)


def main():
    args = sys.argv[1:]
    fd = None
    if args[:1] == ["--fd"]:
        if len(args) < 2:
            sys.exit(__doc__)
        fd, args = int(args[1]), args[2:]
        args = args[:1] + ["fd:%d" % fd] + args[1:]   # the place of DEV
    nargs = {"write": 4, "read": 3, "watch": 4, "check-ro": 3}
    if len(args) < 3 or nargs.get(args[0]) != len(args):
        sys.exit(__doc__)
    op, dev, offset = args[0], args[1], parse_offset(args[2])
    if fd is None:
        size = dax_size(dev)
    else:
        size = fd_size(fd)
    if offset < 0 or offset + MAX_STRING > size:
        sys.exit("offset %d out of range, %s has %d bytes" % (offset, dev, size))
    own = fd is None
    if own:
        fd = os.open(dev, os.O_RDWR if op == "write" else os.O_RDONLY)
    try:
        if op == "check-ro":
            sys.exit(0 if check_ro(dev, fd, own, size) else 1)
        prot = mmap.PROT_READ | (mmap.PROT_WRITE if op == "write" else 0)
        m = mmap.mmap(fd, size, mmap.MAP_SHARED, prot)
        if op == "write":
            data = args[3].encode() + b"\0"
            if len(data) > MAX_STRING:
                sys.exit("text too long, max %d bytes" % (MAX_STRING - 1))
            m[offset:offset + len(data)] = data
        elif op == "watch":
            watch(m, offset, args[3])
        else:
            print(string_at(m, offset))
        m.close()
    finally:
        if own:
            os.close(fd)


if __name__ == "__main__":
    main()
