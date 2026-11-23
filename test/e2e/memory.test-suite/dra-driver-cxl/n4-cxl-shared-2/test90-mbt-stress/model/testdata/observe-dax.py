#!/usr/bin/env python3
"""The reads of the dax devices and the stacks of mbt-observe.py.

Usage: observe-dax.py MBT_OBSERVE MBT_SOURCE

On a sysfs tree and an iomem file in a temporary directory: dax_devices
of MBT_OBSERVE counts the online memory blocks of a dax device of one
range, of several ranges and of no driver as /proc/iomem gives the ranges,
and opens no mappingN file. dax_ranges refuses an iomem file without
addresses. Then arm_stacks writes STACKS_MARKER, the kernel stacks of the threads of
the process and the Python stacks of its threads to stderr, nothing once
cancelled; the stacks of a child blocked in a read of a pipe name the
call and the pipe; and the executor MBT_SOURCE (mbt.source.sh) looks for
the same marker. Prints "ok" and exits 0, or prints the failures and exits 1.

Python 3 standard library only.
"""

import importlib.util
import io
import os
import subprocess
import sys
import tempfile
import time

sys.dont_write_bytecode = True

BS = 128 << 20
START = 0x250000000

# the dax devices: their ranges (start block, blocks) in mapping order, and
# their driver
DEVICES = {
    "dax0.0": ([(0, 2)], "kmem"),
    "dax0.1": ([(2, 1), (5, 2)], "kmem"),
    "dax0.2": ([(3, 2)], None),
    "dax0.10": ([(7, 1)], "kmem"),
}
ONLINE = {0, 2, 6, 7}


def load(path):
    """The module of the observer in path."""
    spec = importlib.util.spec_from_file_location("mbt_observe", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text + "\n")


def sysfs(root):
    """Write the region, dax devices, memory blocks and iomem file of
    DEVICES and ONLINE under root; return the region path and the online
    bytes of each device."""
    region = os.path.join(root, "cxl", "region0")
    end = START + 8 * BS - 1
    iomem = ["%x-%x : CXL Window 0" % (START, end), "  %x-%x : region0" % (START, end)]
    want = {}
    for name, (ranges, driver) in DEVICES.items():
        d = os.path.join(region, "dax_region0", name)
        size = 0
        want[name] = 0
        for i, (first, n) in enumerate(ranges):
            start, end = START + first * BS, START + (first + n) * BS - 1
            write("%s/mapping%d/start" % (d, i), "%#x" % start)
            write("%s/mapping%d/end" % (d, i), "%#x" % end)
            size += n * BS
            if driver:
                iomem.append("    %x-%x : %s" % (start, end, name))
                iomem.append("      %x-%x : System RAM (%s)" % (start, end, driver))
                want[name] += BS * len(ONLINE & set(range(first, first + n)))
        write(d + "/size", str(size))
        write(d + "/resource", "%#x" % (START + ranges[0][0] * BS))
        write(d + "/target_node", "2")
        if driver:
            os.symlink("../../../../bus/dax/drivers/" + driver, d + "/driver")
    for b in range(START // BS, START // BS + 8):
        state = "online" if b - START // BS in ONLINE else "offline"
        write(os.path.join(root, "memory", "memory%d" % b, "state"), state)
    write(os.path.join(root, "iomem"), "\n".join(iomem))
    return region, want


def check_online_bytes(obs, failures):
    with tempfile.TemporaryDirectory() as root:
        region, want = sysfs(root)
        obs.MEMORY, obs.IOMEM = os.path.join(root, "memory"), os.path.join(root, "iomem")
        opened = []

        def tracked_open(path, *args, **kwargs):
            opened.append(path)
            return open(path, *args, **kwargs)
        obs.open = tracked_open
        try:
            errors = []
            got = {d["name"]: d["online"] for d in obs.dax_devices(region, BS, obs.dax_ranges(), errors)}
        finally:
            del obs.open
        if errors:
            failures.append("dax_devices: errors %s" % errors)
        if got != want:
            failures.append("online %s, expected %s" % (got, want))
        mappings = [p for p in opened if "/mapping" in p]
        if mappings:
            failures.append("opened %s" % mappings)
        write(obs.IOMEM, "00000000-00000000 : dax0.0")
        try:
            ranges = obs.dax_ranges()
            failures.append("an iomem file without addresses gives %s" % ranges)
        except ValueError:
            pass


def stderr_of(fn, seconds):
    """What fn() and the next seconds write to stderr."""
    with tempfile.TemporaryFile() as out:
        saved, stderr = os.dup(2), sys.stderr
        os.dup2(out.fileno(), 2)
        sys.stderr = io.TextIOWrapper(os.fdopen(os.dup(2), "wb"), write_through=True)
        try:
            fn()
            time.sleep(seconds)
        finally:
            sys.stderr.close()
            sys.stderr = stderr
            os.dup2(saved, 2)
            os.close(saved)
        out.seek(0)
        return out.read().decode(errors="replace")


def check_stacks(obs, failures):
    text = stderr_of(lambda: obs.arm_stacks(0.1), 1.6)
    lines = text.splitlines()
    if not lines or lines[0] != obs.STACKS_MARKER:
        failures.append("stacks: the first line is not the marker: %r" % text[:200])
    for part in ("== observer pid %d" % os.getpid(), "-- task %d " % os.getpid(), "most recent call first"):
        if part not in text:
            failures.append("stacks: no %r in %r" % (part, text[:2000]))
    text = stderr_of(lambda: obs.arm_stacks(0.1)(), 1.6)
    if text:
        failures.append("stacks after the cancel: %r" % text[:200])
    # a thread blocked in a read: the call and its file
    child = subprocess.Popen([sys.executable, "-I", "-c", "import sys; sys.stdin.read()"], stdin=subprocess.PIPE)
    try:
        time.sleep(0.5)
        lines = obs.task_stacks(child.pid)
    finally:
        child.stdin.close()
        child.wait()
    want = "syscall read pipe:["
    if not any(want in line for line in lines if line.startswith("-- task %d " % child.pid)):
        failures.append("stacks of a reader of a pipe: no %r in %r" % (want, lines[:3]))


def main():
    obs = load(sys.argv[1])
    failures = []
    check_online_bytes(obs, failures)
    check_stacks(obs, failures)
    with open(sys.argv[2]) as f:
        if 'MBT_OBS_STACKS_MARKER="%s"' % obs.STACKS_MARKER not in f.read():
            failures.append("%s: no MBT_OBS_STACKS_MARKER of %r" % (sys.argv[2], obs.STACKS_MARKER))
    if failures:
        print("\n".join(failures))
        sys.exit(1)
    print("ok")


if __name__ == "__main__":
    main()
