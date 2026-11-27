"""The superblock of the filesystem on a pool device: type, size, label, state.

Standard library only. Reads the start of a block device (in the demo: the
bytes of the fsdax namespace after its pfn info block, common.read_fsdax),
never the files of a writable filesystem: erofs, ext4, xfs and btrfs by
their superblock magics, as probe.py and the driver detect them.
"""
import struct
import time

READ_LEN = 72 << 10   # the btrfs superblock is at 64 KiB, 4 KiB long
# The superblock magics: (type, offset, bytes); the same as probe.py FS_MAGICS.
MAGICS = (("erofs", 1024, b"\xe2\xe1\xf5\xe0"), ("ext4", 1080, b"\x53\xef"), ("xfs", 0, b"XFSB"),
          ("btrfs", 65600, b"_BHRfS_M"))
EXT4_SUPER = 1024
EXT4_VALID_FS, EXT4_ERROR_FS = 0x1, 0x2
EXT4_INCOMPAT_RECOVER, EXT4_INCOMPAT_64BIT = 0x4, 0x80
BTRFS_SUPER = 65536


def text(raw):
    """Return the NUL-terminated bytes RAW as printable ASCII."""
    return "".join(chr(b) if 32 <= b < 127 else "?" for b in raw.split(b"\0", 1)[0])


def uuid(raw):
    h = raw.hex()
    return "%s-%s-%s-%s-%s" % (h[:8], h[8:12], h[12:16], h[16:20], h[20:32])


def signatures(data):
    """Return the filesystem types whose superblock magic DATA has, in the order of MAGICS."""
    return [t for t, off, magic in MAGICS if data[off:off + len(magic)] == magic]


def fs_super(data):
    """Return what the superblock at the start DATA of a block device says.

    The fields: type (erofs, ext4, xfs, btrfs; "zeros" if DATA is all
    zeros, "none" without a magic, "several" with the magics of several
    types), signatures (the types found), size (bytes), blockSize, label,
    uuid; ext4 also clean (s_state valid), errors (s_state error),
    needsRecovery (the journal is in use: mounted read-write, or
    not unmounted cleanly), written (s_wtime, epoch), mounts (s_mnt_count)
    and the feature flags; btrfs also generation; error says what
    could not be read.
    """
    out = {"type": "none", "signatures": [], "error": ""}
    if not any(data):
        out["type"] = "zeros"
        return out
    sigs = signatures(data)
    out["signatures"] = sigs
    if not sigs:
        return out
    if len(sigs) > 1:
        out["type"] = "several"
        return out
    out["type"] = sigs[0]
    try:
        out.update({"erofs": _erofs, "ext4": _ext4, "xfs": _xfs, "btrfs": _btrfs}[sigs[0]](data))
    except struct.error:
        out["error"] = "superblock beyond the data"
    return out


def _erofs(data):
    bits = data[1024 + 12]
    blocks = struct.unpack_from("<I", data, 1024 + 36)[0]
    return {"blockSize": 1 << bits, "size": blocks << bits, "label": text(data[1024 + 64:1024 + 80]),
            "uuid": uuid(data[1024 + 48:1024 + 64])}


def _ext4(data):
    s = EXT4_SUPER
    blocks_lo, log_bs = struct.unpack_from("<I", data, s + 0x4)[0], struct.unpack_from("<I", data, s + 0x18)[0]
    wtime, mounts, _, _, state = struct.unpack_from("<IHhHH", data, s + 0x30)
    compat, incompat, ro_compat = struct.unpack_from("<III", data, s + 0x5C)
    blocks = blocks_lo
    if incompat & EXT4_INCOMPAT_64BIT:
        blocks |= struct.unpack_from("<I", data, s + 0x150)[0] << 32
    bs = 1024 << log_bs
    return {"blockSize": bs, "size": blocks * bs, "label": text(data[s + 0x78:s + 0x88]), "uuid": uuid(data[s + 0x68:s + 0x78]),
            "clean": bool(state & EXT4_VALID_FS), "errors": bool(state & EXT4_ERROR_FS),
            "needsRecovery": bool(incompat & EXT4_INCOMPAT_RECOVER), "written": wtime, "mounts": mounts,
            "featureCompat": "0x%x" % compat, "featureIncompat": "0x%x" % incompat, "featureRoCompat": "0x%x" % ro_compat}


def _xfs(data):
    bs, dblocks = struct.unpack_from(">IQ", data, 4)
    return {"blockSize": bs, "size": bs * dblocks, "label": text(data[108:120]), "uuid": uuid(data[32:48])}


def _btrfs(data):
    s = BTRFS_SUPER
    generation = struct.unpack_from("<Q", data, s + 0x48)[0]
    total = struct.unpack_from("<Q", data, s + 0x70)[0]
    sector = struct.unpack_from("<I", data, s + 0x90)[0]
    return {"blockSize": sector, "size": total, "label": text(data[s + 0x12B:s + 0x22B]), "uuid": uuid(data[s + 0x20:s + 0x30]),
            "generation": generation}


def size_text(n):
    """Return bytes N as 8K, 510M, 1.5G (as the kernel views print sizes)."""
    if n is None:
        return "?"
    if n >= 1 << 30:
        return "%.1fG" % (n / (1 << 30))
    if n >= 1 << 20:
        return "%dM" % (n >> 20)
    return "%dK" % (n >> 10)


def lines(sb):
    """Return the line of the superblock SB (fs_super) in the pool view."""
    if not sb:
        return ["file system: no data"]
    t = sb.get("type")
    if t == "zeros":
        return ["no file system (zeros)"]
    if t == "none":
        return ["no file system"]
    if t == "several":
        return ["signatures of " + ", ".join(sb.get("signatures") or [])]
    if sb.get("error"):
        return ["%s: %s" % (t, sb["error"])]
    head = "%s%s %s" % (t, ' "%s"' % sb["label"] if sb.get("label") else "", size_text(sb.get("size")))
    if t == "ext4":
        state = "needs recovery" if sb.get("needsRecovery") else ("clean" if sb.get("clean") else "not clean")
        # s_wtime is not the time of the last change (a remount read-only keeps it): not shown.
        return [head + ", " + state + (", errors" if sb.get("errors") else "")]
    if t == "btrfs":
        return [head + ", generation %d" % sb.get("generation", 0)]
    return [head]


def details(sb):
    """Return the lines of every field of the superblock SB (fs_super), for a zoom."""
    t = sb.get("type")
    if t in ("zeros", "none", "several"):
        return lines(sb)
    where = {"erofs": 1024, "ext4": 1024, "xfs": 0, "btrfs": BTRFS_SUPER}.get(t)
    out = ["%s superblock at %d: block size %s, size %s bytes (%s), label %r, uuid %s" % (
        t, where, sb.get("blockSize"), sb.get("size"), size_text(sb.get("size")), sb.get("label", ""), sb.get("uuid", "?"))]
    if t == "ext4":
        # Read-only mounts write no superblock: neither counts them.
        out.append("  state %s%s, needs_recovery %s, mount count %d, last write time %s" % (
            "clean" if sb.get("clean") else "not clean", ", errors" if sb.get("errors") else "",
            "yes" if sb.get("needsRecovery") else "no", sb.get("mounts", 0),
            time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(sb["written"])) if sb.get("written") else "never"))
        out.append("  features compat %s, incompat %s, ro_compat %s" % (
            sb.get("featureCompat"), sb.get("featureIncompat"), sb.get("featureRoCompat")))
        out.append("  needs_recovery: the journal is in use (a node has it read-write, or it was not unmounted cleanly)")
    elif t == "btrfs":
        out.append("  generation %d (each commit of the filesystem increments it)" % sb.get("generation", 0))
    if sb.get("error"):
        out.append("not readable: " + sb["error"])
    return out
