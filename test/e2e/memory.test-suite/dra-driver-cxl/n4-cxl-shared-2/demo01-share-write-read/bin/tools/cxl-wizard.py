#!/usr/bin/env python3
"""The tool of the wizard's pods: what the container does with the memory
of its claim or lease. ConfigMap demo-tools mounts it at /demo.

Usage:
  cxl-wizard.py env          print the CXL_* variables
  cxl-wizard.py read         dax: the string at offset 0; block: the
                             filesystem on the device; fs: hello.txt
  cxl-wizard.py write TEXT   dax: TEXT and a NUL at offset 0; fs: TEXT as
                             hello.txt
  cxl-wizard.py mkfs TYPE    block: a filesystem of TYPE (erofs, ext4, xfs,
                             btrfs) with hello.txt on the device (xfs: no
                             files)
  cxl-wizard.py lease ARG... [-- CMD...]
                             cxl-request ARG... --wait 0s, then, if it
                             asked, cxl-request ARG... --wait 600s --exec
                             cxl-wizard.py CMD in the background, its
                             output appended to /tmp/wizard.log
                             (CXL_WIZARD_LOG), then "cxl-wizard exit N"
The memory is the first device of the CXL_<KEY>_MODE variables. A claim's
dax and block devices are device nodes (CXL_<KEY>_DAX, CXL_<KEY>_PATH);
those of a lease are fd 3 (CXL_<KEY>_FD), which cannot be opened again.
mkfs writes an image made in /tmp through either. Files are made in /tmp.
"""
import errno
import mmap
import os
import shutil
import subprocess
import sys
import time

MAP_LEN = 2 << 20      # devdax maps in multiples of its 2 MiB alignment
FS_FILE = "hello.txt"
WORK = "/tmp/wizard-fs"
IMAGE = "/tmp/wizard.img"
LOG = os.environ.get("CXL_WIZARD_LOG", "/tmp/wizard.log")
DONE = 'echo "cxl-wizard exit $?"'   # the last line of a command's output: its exit status
MAGICS = (("erofs", 1024, b"\xe2\xe1\xf5\xe0"), ("ext4", 1080, b"\x53\xef"), ("xfs", 0, b"XFSB"),
          ("btrfs", 65600, b"_BHRfS_M"))
# mkfs.xfs of the demo image (xfsprogs 7.0.1, Alpine) crashes with any -p
# (protofile or directory): its xfs is made without files.
EMPTY_TYPES = ("xfs",)
MKFS = {"ext4": ["mkfs.ext4", "-q", "-F", "-b", "4096", "-d", WORK],
        "xfs": ["mkfs.xfs", "-q", "-f"],
        "btrfs": ["mkfs.btrfs", "-q", "-f", "--rootdir", WORK]}


def device():
    """Return (key, mode) of the first memory device of the environment."""
    keys = sorted(k[4:-5] for k, v in os.environ.items() if k.startswith("CXL_") and k.endswith("_MODE") and v)
    if not keys:
        sys.exit("no CXL memory: no CXL_<KEY>_MODE variable")
    return keys[0], os.environ["CXL_%s_MODE" % keys[0]]


def var(key, name, default=None):
    return os.environ.get("CXL_%s_%s" % (key, name), default)


def device_fd(key, node_var, write):
    """Return the fd of the dax or block device of KEY: fd 3 of a lease, else the device node opened."""
    fd = var(key, "FD")
    if fd is not None:
        return int(fd)
    return os.open(var(key, node_var), os.O_RDWR if write else os.O_RDONLY)


def refused(e, what):
    print("refused: %s (%s)" % (what, errno.errorcode.get(e.errno, e.errno)))
    sys.exit(1)


def do_env():
    for k, v in sorted(os.environ.items()):
        if k.startswith("CXL_"):
            print("%s=%s" % (k, v))


def dax_map(key, write):
    size = min(MAP_LEN, int(var(key, "SIZE", MAP_LEN)))
    try:
        fd = device_fd(key, "DAX", write)
        return mmap.mmap(fd, size, mmap.MAP_SHARED, mmap.PROT_READ | (mmap.PROT_WRITE if write else 0))
    except PermissionError as e:
        refused(e, "read-only")
    except OSError as e:
        if write and e.errno in (errno.EACCES, errno.EPERM, errno.EBADF, errno.EROFS):
            refused(e, "read-only")
        raise


def fs_type(data):
    for t, off, magic in MAGICS:
        if data[off:off + len(magic)] == magic:
            return t
    return "no filesystem" if any(data) else "zeros"


def do_read(key, mode):
    if mode == "dax":
        m = dax_map(key, False)
        text = bytes(m[:4096]).split(b"\0", 1)[0].decode(errors="replace")
        print('%s: read at 0: "%s"' % (key, text))
    elif mode == "block":
        fd = device_fd(key, "PATH", False)
        data = os.pread(fd, 128 << 10, 0)
        print("%s: block device of %s bytes: %s" % (key, var(key, "SIZE", "?"), fs_type(data)))
    elif mode == "fs":
        p = os.path.join(var(key, "PATH"), FS_FILE)
        try:
            with open(p) as f:
                print('%s: read %s (%s): "%s"' % (key, p, var(key, "FS_TYPE", "?"), f.readline().rstrip("\n")))
        except OSError as e:
            print("%s: cannot read %s: %s; the directory has: %s" % (key, p, e.strerror,
                                                                   " ".join(sorted(os.listdir(var(key, "PATH")))) or "nothing"))
            sys.exit(1)
    else:
        sys.exit("read: mode %s has no bytes to read (env shows it)" % mode)


def do_write(key, mode, text):
    if mode == "dax":
        m = dax_map(key, True)
        data = text.encode() + b"\0"
        # No msync: a devdax mapping is the memory itself, and msync of it
        # fails with EINVAL (as /demo/write, cxl-demo.py).
        m[:len(data)] = data
        print('%s: wrote at 0: "%s"' % (key, text))
    elif mode == "fs":
        p = os.path.join(var(key, "PATH"), FS_FILE)
        try:
            with open(p, "w") as f:
                f.write(text + "\n")
                f.flush()
                os.fsync(f.fileno())
        except OSError as e:
            print("%s: cannot write %s: %s" % (key, p, e.strerror))
            sys.exit(1)
        print('%s: wrote %s: "%s"' % (key, p, text))
    else:
        sys.exit("write: mode %s takes no text (block: mkfs)" % mode)


def make_files(text):
    """Make WORK with hello.txt."""
    shutil.rmtree(WORK, ignore_errors=True)
    os.makedirs(WORK)
    with open(os.path.join(WORK, FS_FILE), "w") as f:
        f.write(text + "\n")


def run(cmd):
    try:
        p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    except FileNotFoundError:
        sys.exit("%s is not in the image (image/Dockerfile)" % cmd[0])
    if p.returncode != 0:
        sys.stdout.write(p.stdout.decode(errors="replace"))
        sys.exit("%s failed with exit status %d" % (cmd[0], p.returncode))


def write_through(fd, image):
    """Write the file IMAGE at offset 0 of FD, every block, and sync it; return its size."""
    done = 0
    with open(image, "rb") as f:
        while True:
            chunk = f.read(1 << 20)
            if not chunk:
                break
            n = 0
            while n < len(chunk):
                n += os.pwrite(fd, chunk[n:], done + n)
            done += len(chunk)
    os.fsync(fd)
    return done


def do_mkfs(key, mode, fstype, text):
    """Make a filesystem of FSTYPE with hello.txt (TEXT) on the block device of KEY.

    mkfs makes it in an image file of the device's size in /tmp, which is
    written whole through the device (the node of a claim, fd 3 of a
    lease): mkfs on a block device node sets its block size (BLKBSZSET),
    which an unprivileged container may not, and a lease has no node. An
    erofs image is smaller; the first MiB of the device is zeroed first.
    """
    if mode != "block":
        sys.exit("mkfs: needs mode block, not %s" % mode)
    make_files(text)
    size = int(var(key, "SIZE", "0"))
    try:
        fd = device_fd(key, "PATH", True)
        os.pwrite(fd, b"\0" * (1 << 20), 0)
    except OSError as e:
        refused(e, "read-only")
    if os.path.exists(IMAGE):
        os.unlink(IMAGE)
    if fstype == "erofs":
        run(["mkfs.erofs", "-b4096", "-Enoinline_data", IMAGE, WORK])
    else:
        with open(IMAGE, "wb") as f:
            f.truncate(size)
        run(MKFS[fstype] + [IMAGE])
    if os.path.getsize(IMAGE) > size:
        sys.exit("mkfs: %s needs %d bytes, the device has %d" % (fstype, os.path.getsize(IMAGE), size))
    t0 = time.time()
    n = write_through(fd, IMAGE)
    os.unlink(IMAGE)
    took = time.time() - t0
    if fstype in EMPTY_TYPES:
        print("%s: made %s (%d bytes in %.1f s), no files (an fs claim read-write writes %s)" % (key, fstype, n, took, FS_FILE))
    else:
        print('%s: made %s (%d bytes in %.1f s) with %s: "%s"' % (key, fstype, n, took, FS_FILE, text))


def do_lease(args):
    """Ask for the lease of ARGS without waiting, then start the command after -- in the background."""
    cmd = []
    if "--" in args:
        i = args.index("--")
        args, cmd = args[:i], args[i + 1:]
    p = subprocess.run(["cxl-request"] + args + ["--wait", "0s"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    sys.stdout.write(p.stdout.decode(errors="replace"))
    if p.returncode != 0 or not cmd:
        return p.returncode
    log = open(LOG, "a")
    log.write("%s: cxl-request %s --exec %s\n" % (time.strftime("%H:%M:%S"), " ".join(args), " ".join(cmd)))
    log.flush()
    bg = subprocess.Popen(["sh", "-c", 'cxl-request "$@"; ' + DONE, "cxl-request"] + args +
                          ["--wait", "600s", "--exec", sys.executable, os.path.abspath(__file__)] + cmd,
                          stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
    print("started in the background, pid %d: %s (output in %s)" % (bg.pid, " ".join(cmd), LOG))
    return 0


def main():
    args = sys.argv[1:]
    op = args[0] if args else ""
    if op == "lease":
        return do_lease(args[1:])
    if op == "env" and len(args) == 1:
        return do_env()
    key, mode = device()
    if op == "read" and len(args) == 1:
        do_read(key, mode)
    elif op == "write" and len(args) == 2:
        do_write(key, mode, args[1])
    elif op == "mkfs" and len(args) == 2 and args[1] in ("erofs",) + tuple(MKFS):
        name = os.environ.get("HOSTNAME", "the wizard")
        do_mkfs(key, mode, args[1], "Hello from %s, %s" % (name, time.strftime("%H:%M:%S")))
    else:
        sys.exit(__doc__)
    return 0


if __name__ == "__main__":
    sys.exit(main())
