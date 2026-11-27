#!/usr/bin/env python3
"""The demo tools in the containers: the memory of a lease, through its fd.

ConfigMap demo-tools mounts this at /demo with the wrappers write, read,
hold, env and mkfs:
  /demo/write TEXT   write TEXT and a NUL at offset 0 of the lease's memory,
                     keep the mapping DEMO_HOLD seconds (default 3)
  /demo/read         print the NUL-terminated string at offset 0
  /demo/hold         keep a read-only mapping in the background until
                     /demo/hold --stop (or the pod ends)
  /demo/env          the CXL_* variables of every lease of the pod
  /demo/mkfs TEXT    make an erofs with hello.txt (TEXT and a newline) with
                     mkfs.erofs and write it at offset 0 of the block device
                     of a block lease, keep the fd DEMO_HOLD seconds
  /demo/mkfs --type TYPE
                     make an empty filesystem of TYPE (ext4, xfs, btrfs) of
                     the size of the block device of a block lease with
                     mkfs.TYPE, write all of it through the fd, check its
                     superblock on the device, keep the fd DEMO_HOLD seconds
  /demo/mkfs --model fetch the model file LLM_MODEL_FILE from LLM_MODEL_URL
                     (http_proxy, https_proxy and no_proxy apply), check its
                     sha256 LLM_MODEL_SHA256, make an erofs of it that a
                     node can mount with DAX and write that at offset 0 of
                     the block device of a block lease, keep the fd
                     DEMO_HOLD seconds; the page cache of the VM (Cached of
                     /proc/meminfo) before the fetch and after the write
  /demo/fswrite [--append] TEXT
                     write TEXT and a newline into hello.txt of the
                     filesystem of the newest read-write fs lease (append
                     with --append), fsync it
The lease is the newest dax lease of the container (block lease for mkfs,
read-write fs lease for fswrite; cxl-request list); DEMO_LEASE=ID picks
one. Images are written through the fd in chunks, never read into memory
whole, and removed when they are written; the pages that the write left
in the page cache of the block device are dropped. The fd comes from
"cxl-request env --lease ID --exec": the container needs no device node. A
read-only lease's fd refuses a writable mapping: write prints "refused:
read-only (EACCES)"; mkfs prints "refused: read-only (EBADF)"; mkfs --model
prints "refused: the device is too small for the image of N bytes (ENOSPC)"
if the model does not fit. The tools keep their files in /tmp: /run/cxl of
the container is read-only.
"""
import errno
import hashlib
import json
import mmap
import os
import shutil
import signal
import subprocess
import sys
import time
import urllib.request

FD = 3
MAP_LEN = 2 << 20     # devdax maps in multiples of its 2 MiB alignment
STOP = "/tmp/demo-hold.stop"
HOLD_LOG = "/tmp/demo-hold.log"
FS_DIR = "/tmp/fs"
FS_FILE = "hello.txt"
FS_IMAGE = "/tmp/hello.erofs"
EMPTY_IMAGE = "/tmp/empty.img"
MODEL_DIR = "/tmp/model"          # the root of the erofs of mkfs --model: the model file only
MODEL_IMAGE = "/tmp/model.erofs"
CHUNK = 4 << 20
# The erofs of the model: its data in one plain extent at a 2 MiB-aligned
# offset, no inline tail (else the file gets no DAX).
MKFS_MODEL = ["mkfs.erofs", "-b4096", "--dsunit=512", "-Enoinline_data"]
# mkfs of the empty filesystems: the block size of ext4 is the page size (DAX).
MKFS = {"ext4": ["mkfs.ext4", "-q", "-F", "-b", "4096", "-L", "demo"], "xfs": ["mkfs.xfs", "-q", "-f", "-L", "demo"],
        "btrfs": ["mkfs.btrfs", "-q", "-f", "-L", "demo"]}
# The superblock of each type: (offset, length), compared on the device after the write.
SUPERBLOCK = {"ext4": (1024, 1024), "xfs": (0, 512), "btrfs": (65536, 4096)}
HINTS = {"dax": "no dax lease: cxl-request request ... first",
         "block": "no block lease: cxl-request request --mode block first"}


def leases():
    p = subprocess.run(["cxl-request", "list"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if p.returncode != 0:
        sys.exit("cxl-request list: " + (p.stderr or p.stdout).decode(errors="replace").strip())
    try:
        return json.loads(p.stdout.decode()).get("leases") or []
    except ValueError:
        sys.exit("cxl-request list: unexpected output")


def lease(modes=("dax",)):
    """Return the id of the lease to use: DEMO_LEASE or the newest lease of one of MODES."""
    if os.environ.get("DEMO_LEASE"):
        return os.environ["DEMO_LEASE"]
    found = [l for l in leases() if l.get("mode") in modes and l.get("state") not in ("Failed", "Released", "Releasing")]
    if not found:
        sys.exit(HINTS.get(modes[0], "no %s lease" % modes[0]))
    return found[-1]["lease"]


def with_fd(op, *args, background=False, modes=("dax",)):
    """Run this tool's internal OP with the fd of a lease of MODES as fd 3."""
    cmd = ["cxl-request", "env", "--lease", lease(modes), "--wait", "120s", "--exec",
           sys.executable, os.path.abspath(__file__), op] + list(args)
    if not background:
        os.execvp(cmd[0], cmd)
    log = open(HOLD_LOG, "w")
    p = subprocess.Popen(cmd, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT,
                         start_new_session=True)
    for _ in range(50):
        time.sleep(0.1)
        with open(HOLD_LOG) as f:
            text = f.read()
        if "holding" in text or p.poll() is not None:
            break
    print(text.strip() or "holder started, pid %d" % p.pid)


def mapping(write):
    size = MAP_LEN
    for k, v in os.environ.items():
        if k.startswith("CXL_") and k.endswith("_SIZE") and v.isdigit():
            size = min(size, int(v))
    prot = mmap.PROT_READ | (mmap.PROT_WRITE if write else 0)
    return mmap.mmap(FD, size, mmap.MAP_SHARED, prot)


def text_at(m):
    s = bytes(m[:4096]).split(b"\0", 1)[0]
    return s.decode(errors="replace")


def do_write(text):
    try:
        m = mapping(True)
    except PermissionError as e:
        print("refused: read-only (%s)" % errno.errorcode.get(e.errno, e.errno))
        sys.exit(1)
    data = text.encode() + b"\0"
    m[:len(data)] = data
    print('wrote at 0: "%s"' % text)
    sys.stdout.flush()
    time.sleep(float(os.environ.get("DEMO_HOLD", "3")))
    m.close()


def do_read():
    m = mapping(False)
    print('read at 0: "%s"' % text_at(m))
    m.close()


def do_hold():
    m = mapping(False)
    if os.path.exists(STOP):
        os.unlink(STOP)
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    print('holding a read-only mapping, pid %d: "%s"' % (os.getpid(), text_at(m)), flush=True)
    while not os.path.exists(STOP):
        time.sleep(0.2)
    m.close()
    print("released the mapping", flush=True)


def do_mkfs(text):
    """Make the erofs image with hello.txt, then write it through the fd of a block lease."""
    os.makedirs(FS_DIR, exist_ok=True)
    with open(os.path.join(FS_DIR, FS_FILE), "w") as f:
        f.write(text + "\n")
    if os.path.exists(FS_IMAGE):
        os.unlink(FS_IMAGE)
    p = subprocess.run(["mkfs.erofs", "-b4096", "-Enoinline_data", FS_IMAGE, FS_DIR],
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if p.returncode != 0:
        sys.stdout.write(p.stdout.decode(errors="replace"))
        sys.exit("mkfs.erofs failed with exit status %d" % p.returncode)
    with_fd("_flash", FS_IMAGE, '%s: "%s"' % (FS_FILE, text), modes=("block",))


def fetch(url, path, want_sha256):
    """Return (bytes, seconds) of the download of URL to PATH; exit unless its sha256 is WANT_SHA256."""
    h = hashlib.sha256()
    size = 0
    t0 = time.time()
    try:
        with urllib.request.urlopen(url, timeout=60) as r, open(path, "wb") as f:
            while True:
                chunk = r.read(CHUNK)
                if not chunk:
                    break
                h.update(chunk)
                f.write(chunk)
                size += len(chunk)
    except OSError as e:
        sys.exit("cannot fetch %s: %s" % (url, e))
    t = time.time() - t0
    if h.hexdigest() != want_sha256:
        os.unlink(path)
        sys.exit("%s from %s has sha256 %s, expected %s" % (os.path.basename(path), url, h.hexdigest(), want_sha256))
    return size, t


def vm_cached_mib():
    """Return Cached of /proc/meminfo (the page cache of the VM: not of the container) in MiB."""
    with open("/proc/meminfo") as f:
        for line in f:
            if line.startswith("Cached:"):
                return int(line.split()[1]) >> 10
    return -1


def do_mkfs_model():
    """Fetch the model of LLM_MODEL_URL, check its sha256, make an erofs of it and write that through the fd of a block lease."""
    try:
        url, name, sha = (os.environ[k] for k in ("LLM_MODEL_URL", "LLM_MODEL_FILE", "LLM_MODEL_SHA256"))
    except KeyError as e:
        sys.exit("no %s: the pod has no ConfigMap llm-model (demo setup llm-share writes it)" % e)
    print("page cache of the VM before the fetch: %d MiB" % vm_cached_mib())
    shutil.rmtree(MODEL_DIR, ignore_errors=True)
    os.makedirs(MODEL_DIR)
    path = os.path.join(MODEL_DIR, name)
    size, t = fetch(url, path, sha)
    print("fetched %s: %d MiB in %.1f s (%.0f MB/s), sha256 ok" % (name, size >> 20, t, size / max(t, 1e-3) / 1e6))
    sys.stdout.flush()
    if os.path.exists(MODEL_IMAGE):
        os.unlink(MODEL_IMAGE)
    t0 = time.time()
    p = subprocess.run(MKFS_MODEL + [MODEL_IMAGE, MODEL_DIR], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    os.unlink(path)   # the image holds it
    if p.returncode != 0:
        sys.stdout.write(p.stdout.decode(errors="replace"))
        sys.exit("mkfs.erofs failed with exit status %d" % p.returncode)
    print("made an erofs of it: %d MiB in %.1f s" % (os.path.getsize(MODEL_IMAGE) >> 20, time.time() - t0))
    sys.stdout.flush()
    with_fd("_flash", MODEL_IMAGE, name, modes=("block",))


def stream_to_fd(image):
    """Return (bytes, seconds) of writing the file IMAGE at offset 0 of fd 3 in chunks of CHUNK, fsyncing it and dropping it from the page cache of the device."""
    t0 = time.time()
    off = 0
    with open(image, "rb") as f:
        while True:
            data = f.read(CHUNK)
            if not data:
                break
            done = 0
            while done < len(data):
                done += os.pwrite(FD, data[done:], off + done)
            off += len(data)
    os.fsync(FD)
    try:
        os.posix_fadvise(FD, 0, off, os.POSIX_FADV_DONTNEED)
    except OSError:
        pass   # the pages stay cached, nothing else changes
    return off, time.time() - t0


def do_flash(image, what):
    """Write IMAGE (an erofs with WHAT) at offset 0 of fd 3, fsync it, remove it, and keep the fd DEMO_HOLD seconds."""
    try:
        size, t = stream_to_fd(image)
    except OSError as e:
        if e.errno == errno.EBADF:
            print("refused: read-only (EBADF)")
            sys.exit(1)
        if e.errno == errno.ENOSPC:
            print("refused: the device is too small for the image of %d bytes (ENOSPC)" % os.path.getsize(image))
            os.unlink(image)
            sys.exit(1)
        raise
    os.unlink(image)
    print("streamed %s through the fd of the lease: %d bytes in %.1f s" % (image, size, t))
    print("removed %s: page cache of the VM now %d MiB" % (image, vm_cached_mib()))
    print("wrote erofs (%d bytes) with %s" % (size, what))
    sys.stdout.flush()
    time.sleep(float(os.environ.get("DEMO_HOLD", "3")))
    os.close(FD)


def do_mkfs_type(fstype):
    """Make an empty filesystem of FSTYPE of the size of the block device of fd 3 and write all of it through the fd."""
    try:
        size = os.lseek(FD, 0, os.SEEK_END)
    except OSError as e:
        sys.exit("cannot get the size of the block device: %s" % e.strerror)
    if os.path.exists(EMPTY_IMAGE):
        os.unlink(EMPTY_IMAGE)
    with open(EMPTY_IMAGE, "wb") as f:
        f.truncate(size)
    p = subprocess.run(MKFS[fstype] + [EMPTY_IMAGE], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if p.returncode != 0:
        sys.stdout.write(p.stdout.decode(errors="replace"))
        os.unlink(EMPTY_IMAGE)
        sys.exit("%s failed with exit status %d" % (MKFS[fstype][0], p.returncode))
    try:
        _, t = stream_to_fd(EMPTY_IMAGE)
    except OSError as e:
        os.unlink(EMPTY_IMAGE)
        if e.errno == errno.EBADF:
            print("refused: read-only (EBADF)")
            sys.exit(1)
        raise
    so, sl = SUPERBLOCK[fstype]
    with open(EMPTY_IMAGE, "rb") as f:
        f.seek(so)
        want = f.read(sl)
    os.unlink(EMPTY_IMAGE)
    if os.pread(FD, sl, so) != want:
        sys.exit("the superblock of %s on the device is not the one written" % fstype)
    print("wrote an empty %s of %d MiB in %.1f s" % (fstype, size >> 20, t))
    sys.stdout.flush()
    time.sleep(float(os.environ.get("DEMO_HOLD", "3")))
    os.close(FD)


def do_fswrite(text, append):
    """Write TEXT and a newline into hello.txt of the newest read-write fs lease, appended if APPEND, and fsync it."""
    found = [l for l in leases() if l.get("mode") == "fs" and l.get("readOnly") is False and l.get("state") == "Ready"
             and (not os.environ.get("DEMO_LEASE") or l.get("lease") == os.environ["DEMO_LEASE"])]
    if not found or not found[-1].get("path"):
        sys.exit("no read-write fs lease: cxl-request request --mode fs --read-only=false ... first")
    path = os.path.join(found[-1]["path"], FS_FILE)
    try:
        with open(path, "a" if append else "w") as f:
            f.write(text + "\n")
            f.flush()
            os.fsync(f.fileno())
    except OSError as e:
        print("refused: %s: %s" % (path, e.strerror))
        sys.exit(1)
    print('%s %s: "%s"' % ("appended to" if append else "wrote", path, text))


def do_env():
    for l in leases():
        print("lease %s %s" % (l.get("lease"), l.get("state")))
        for k, v in sorted((l.get("env") or {}).items()):
            print("  %s=%s" % (k, v))


def main():
    args = sys.argv[1:]
    op = args[0] if args else ""
    if op == "write" and len(args) == 2:
        with_fd("_write", args[1])
    elif op == "read" and len(args) == 1:
        with_fd("_read")
    elif op == "hold" and args[1:] == ["--stop"]:
        open(STOP, "w").close()
        print("asked the holder to release its mapping")
    elif op == "hold" and len(args) == 1:
        with_fd("_hold", background=True)
    elif op == "env":
        do_env()
    elif op == "mkfs" and len(args) == 3 and args[1] == "--type" and args[2] in MKFS:
        with_fd("_mkfs", args[2], modes=("block",))
    elif op == "mkfs" and args[1:] == ["--model"]:
        do_mkfs_model()
    elif op == "mkfs" and len(args) == 2 and not args[1].startswith("--"):
        do_mkfs(args[1])
    elif op == "_mkfs" and len(args) == 2:
        do_mkfs_type(args[1])
    elif op == "fswrite" and len(args) == 2:
        do_fswrite(args[1], False)
    elif op == "fswrite" and len(args) == 3 and args[1] == "--append":
        do_fswrite(args[2], True)
    elif op == "_flash" and len(args) == 3:
        do_flash(args[1], args[2])
    elif op == "_write":
        do_write(args[1])
    elif op == "_read":
        do_read()
    elif op == "_hold":
        do_hold()
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
