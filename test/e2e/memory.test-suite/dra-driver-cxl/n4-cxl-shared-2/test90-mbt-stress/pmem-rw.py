#!/usr/bin/env python3
"""Write and read a persistent CXL pool device in the pods of test90.

dra-tools-install puts this into the ConfigMap tools (/tools in the
pods), next to the erofs template hello.erofs.hex (8192 bytes of
mkfs.erofs 1.9 -b4096 -Enoinline_data with one 32-byte hello.txt in
block 1).

Usage:
  pmem-rw.py image DEV TOKEN     write the template with hello.txt = TOKEN
                                 (padded with spaces to 31 bytes, then a
                                 newline) and bytes 0-63 = TOKEN (NUL padded)
                                 at offset 0 of the block device DEV, fsync;
                                 print TOKEN
  pmem-rw.py junk DEV            write 8192 zero bytes at offset 0, fsync
  pmem-rw.py read DEV            print the NUL-terminated string in bytes 0-63
  pmem-rw.py --fd N image|junk|read [TOKEN]
                                 the same through the inherited fd N (a block
                                 lease: cxl-request env --lease L --exec ...)
  pmem-rw.py read-file DIR       print hello.txt of the erofs at DIR, stripped
  pmem-rw.py write-file DIR TOKEN
                                 try to write hello.txt under DIR (a read-only
                                 mount: fails)
  pmem-rw.py hold DIR STOPFILE   map DIR/hello.txt read-only and keep the file
                                 open until STOPFILE exists; print "holding"
  pmem-rw.py lease block|fs|any OP [ARG]
                                 OP on the newest Ready (or Releasing) lease of
                                 the mode of this container (cxl-request
                                 list): a block lease through its fd
                                 (cxl-request env --exec) with image, junk or
                                 read; an fs lease at its path with read-file,
                                 write-file or hold, image and junk being
                                 write-file and read read-file there; when the
                                 driver does not answer cxl-request list, an
                                 fs lease is the one mount under LEASE_FS_DIR

The erofs superblock (offset 1024) has a checksum over bytes 1024-4095 of
block 0 only ("to allow for x86 boot sectors"): bytes 0-63 and the file
data in block 1 are free to patch. erofs has no data checksum.
Exit status: 0 ok, 1 an error of the operation (EACCES, EPERM, EROFS, EBADF:
a read-only device, fd or mount), 2 usage.
"""
import errno
import json
import mmap
import os
import subprocess
import sys
import time

IMAGE_SIZE = 8192
FILE_OFF = 4096           # hello.txt is block 1 of the template
FILE_LEN = 32
RAW_LEN = 64
FILE = "hello.txt"
CXL_REQUEST = "cxl-request"
LEASE_FS_DIR = "/run/cxl/fs"  # the mounts of the fs leases of the container
HERE = os.path.dirname(os.path.abspath(__file__))
TEMPLATE = os.path.join(HERE, "hello.erofs.hex")


def fail(msg, code=1):
    print(msg, file=sys.stderr, flush=True)
    sys.exit(code)


def template():
    with open(TEMPLATE) as f:
        data = bytes(int(x, 16) for line in f if not line.startswith("#") for x in line.split())
    if len(data) != IMAGE_SIZE or data[1024:1028] != b"\xe2\xe1\xf5\xe0":
        fail("%s: not the %d-byte erofs template" % (TEMPLATE, IMAGE_SIZE), 2)
    return data


def image(token):
    t = token.encode()
    if len(t) > FILE_LEN - 1 or b"\0" in t or b"\n" in t:
        fail("token %r: at most %d bytes, no NUL or newline" % (token, FILE_LEN - 1), 2)
    b = bytearray(template())
    b[0:RAW_LEN] = t.ljust(RAW_LEN, b"\0")
    b[FILE_OFF:FILE_OFF + FILE_LEN] = t.ljust(FILE_LEN - 1, b" ") + b"\n"
    return bytes(b)


def errtext(e):
    return "%s: %s" % (errno.errorcode.get(e.errno, e.errno), e.strerror)


def write_fd(fd, data):
    n = os.pwrite(fd, data, 0)
    if n != len(data):
        fail("short write: %d of %d bytes" % (n, len(data)))
    os.fsync(fd)


def read_fd(fd):
    return os.pread(fd, RAW_LEN, 0).split(b"\0", 1)[0].decode(errors="replace")


def block_op(op, fd, args):
    try:
        if op == "image":
            write_fd(fd, image(args[0]))
            print(args[0])
        elif op == "junk":
            write_fd(fd, bytes(IMAGE_SIZE))
            print("")
        elif op == "read":
            print(read_fd(fd))
        else:
            fail("unknown operation %r" % op, 2)
    except OSError as e:
        fail("%s: %s" % (op, errtext(e)))


def block_dev(op, dev, args):
    flags = os.O_RDONLY if op == "read" else os.O_RDWR
    try:
        fd = os.open(dev, flags)
    except OSError as e:
        fail("open %s %s: %s" % (dev, "read-only" if flags == os.O_RDONLY else "read-write", errtext(e)))
    block_op(op, fd, args)


def read_file(d):
    try:
        with open(os.path.join(d, FILE), "rb") as f:
            print(f.read().decode(errors="replace").strip())
    except OSError as e:
        fail("read %s/%s: %s" % (d, FILE, errtext(e)))


def write_file(d, token):
    try:
        with open(os.path.join(d, FILE), "r+b") as f:
            f.write(token.encode())
    except OSError as e:
        fail("write %s/%s: %s" % (d, FILE, errtext(e)))
    print(token)


def hold(d, stop):
    path = os.path.join(d, FILE)
    try:
        f = open(path, "rb")
        m = mmap.mmap(f.fileno(), 0, mmap.MAP_SHARED, mmap.PROT_READ)
    except OSError as e:
        fail("hold %s: %s" % (path, errtext(e)))
    if os.path.exists(stop):
        os.unlink(stop)
    print("holding %s, pid %d: %s" % (path, os.getpid(), bytes(m[:FILE_LEN]).decode(errors="replace").strip()), flush=True)
    while not os.path.exists(stop):
        time.sleep(0.2)
    m.close()
    f.close()
    print("released", flush=True)


class LeaseListError(Exception):
    """cxl-request list failed: the driver does not answer."""


def leases():
    p = subprocess.run([CXL_REQUEST, "list"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if p.returncode != 0:
        raise LeaseListError("cxl-request list: exit %d: %s" % (p.returncode, (p.stderr or p.stdout).decode(errors="replace").strip()))
    text = p.stdout.decode().strip()
    try:
        j = json.loads(text) if text else []
        if isinstance(j, dict):
            j = j.get("leases", [j] if "lease" in j else [])
        return j or []
    except ValueError:
        return [json.loads(l) for l in text.splitlines() if l.strip().startswith("{")]


def fs_lease_mount(err):
    """The path of the one fs lease mounted under LEASE_FS_DIR, for an fs
    lease used while the driver does not answer (err: why cxl-request list
    failed). The mount needs no driver; finding it by name does."""
    try:
        paths = [os.path.join(LEASE_FS_DIR, n) for n in sorted(os.listdir(LEASE_FS_DIR))]
    except OSError as e:
        fail("%s; and %s: %s" % (err, LEASE_FS_DIR, errtext(e)))
    paths = [p for p in paths if os.path.ismount(p)]
    if len(paths) != 1:
        fail("%s; and %d fs lease mounts under %s" % (err, len(paths), LEASE_FS_DIR))
    return paths[0]


def lease(mode, op, args):
    """OP on the newest Ready (or Releasing) lease of mode (block, fs, or
    any of them) of this container: a block lease through its fd, an fs
    lease at its path (image and junk: write-file, read: read-file)."""
    modes = ("block", "fs") if mode == "any" else (mode,)
    try:
        found = [l for l in leases() if l.get("mode") in modes and l.get("state") in ("Ready", "Releasing")]
    except LeaseListError as e:
        if "fs" not in modes:
            fail(str(e))
        found = [{"mode": "fs", "lease": "?", "path": fs_lease_mount(str(e))}]
    if not found:
        fail("no Ready %s lease in this container" % " or ".join(modes))
    l = found[-1]
    if l.get("mode") == "block":
        if op not in ("image", "junk", "read"):
            fail("lease %s is a block lease: no %s" % (l.get("lease"), op))
        cmd = [CXL_REQUEST, "env", "--lease", l["lease"], "--wait", "30s", "--exec",
               sys.executable, os.path.abspath(__file__), "--fd", "3", op] + args[:1]
        os.execvp(cmd[0], cmd)
    path = l.get("path")
    if not path:
        fail("lease %s has no path: %s" % (l.get("lease"), json.dumps(l)))
    fop = {"image": "write-file", "junk": "write-file", "read": "read-file"}.get(op, op)
    if fop == "write-file":
        args = (args or [""])[:1]
    main([fop, path] + args)


def main(argv):
    if len(argv) >= 2 and argv[0] == "--fd":
        if len(argv) < 3:
            fail(__doc__, 2)
        block_op(argv[2], int(argv[1]), argv[3:])
        return
    if not argv:
        fail(__doc__, 2)
    op, args = argv[0], argv[1:]
    want = {"image": 2, "junk": 1, "read": 1, "read-file": 1, "write-file": 2, "hold": 2}
    if op == "lease":
        if len(args) < 2 or args[0] not in ("block", "fs", "any"):
            fail(__doc__, 2)
        lease(args[0], args[1], args[2:])
        return
    if op not in want or len(args) != want[op]:
        fail(__doc__, 2)
    if op in ("image", "junk", "read"):
        block_dev(op, args[0], args[1:])
    elif op == "read-file":
        read_file(args[0])
    elif op == "write-file":
        write_file(args[0], args[1])
    else:
        hold(args[0], args[1])


if __name__ == "__main__":
    main(sys.argv[1:])
