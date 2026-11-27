# Helpers for filesystems on persistent CXL memory devices in the VMs of
# n4-cxl-shared-1 and n4-cxl-shared-2 (test20-dra-fs-read-write,
# test21-dra-local-persistent): filesystem images built in a VM, written
# into a device by a block writer claim, and pods whose claims mount them
# (MemoryConfig device {mode: fs}).
#
# The pods of dra-serial-claim-pod run $DRA_RUNNER (dra-pod-yaml) and the tests
# run commands in their container "main" with dra-exec. The helpers take
# VMDIR first, like those of pool.source.sh and dra.source.sh.

FS_VM_DIR=/var/tmp/cxl-fs-e2e   # the images in the VM that builds them
FS_FILE=hello.txt               # the file of every image
# The commands of the filesystem tools and their Fedora packages.
declare -A FS_TOOL_PACKAGE=([mkfs.ext4]=e2fsprogs [debugfs]=e2fsprogs [dumpe2fs]=e2fsprogs
                            [mkfs.xfs]=xfsprogs [mkfs.btrfs]=btrfs-progs
                            [mksquashfs]=squashfs-tools [mkfs.erofs]=erofs-utils [blkid]=util-linux)
FS_IMAGE=""          # fs-image-build: the image in the VM,
FS_IMAGE_SIZE=""     # its size in bytes
FS_IMAGE_SHA256=""   # and its sha256
FS_NS_SIZE=""        # fs-write: the size of the fsdax namespace (CXL_<KEY>_SIZE of the writer)
FS_LOG=""            # fs-writer-wait: the log of the writer
FS_ENV=""            # fs-ctr-env: the CXL_<KEY>_ lines of a container
FS_PMEM=""           # fs-node-mounts-assert: the pmem block device under the node mounts

fs-kernel-require() {
    # Usage: fs-kernel-require VMDIR TOPOLOGY [OPTION...]
    #
    # Fail the test unless the running kernel of the VM of VMDIR, created
    # from TOPOLOGY, has every CONFIG_OPTION built in, by default those of
    # ext4, xfs, btrfs, squashfs, erofs and FS_DAX.
    local vmdir="$1" topology="$2" opt options
    shift 2
    options=("$@")
    [ ${#options[@]} -gt 0 ] || options=(EXT4_FS XFS_FS BTRFS_FS SQUASHFS EROFS_FS FS_DAX)
    for opt in "${options[@]}"; do
        vmdir-command-q "$vmdir" "grep -qx CONFIG_$opt=y /boot/config-\$(uname -r)" ||
            error "kernel of $(vmdir-name "$vmdir") lacks CONFIG_$opt=y: run memory.test-suite/dra-driver-cxl/$topology/test00-up: it installs the CXL kernel of test/e2e/playbook/files/qemu-cxl-kernel.config"
    done
    echo "$(vmdir-label "$vmdir"): kernel with ${options[*]}"
}

fs-tools-install() {
    # Usage: fs-tools-install VMDIR COMMAND...
    #
    # Install the packages of the filesystem tools COMMANDs
    # (FS_TOOL_PACKAGE) that the VM of VMDIR lacks, with dnf.
    local vmdir="$1" cmd pkgs=""
    shift
    for cmd in "$@"; do
        [ -n "${FS_TOOL_PACKAGE[$cmd]:-}" ] || error "fs-tools-install: no package of $cmd"
        vmdir-command-q "$vmdir" "command -v $cmd >/dev/null" || pkgs+=" ${FS_TOOL_PACKAGE[$cmd]}"
    done
    if [ -n "$pkgs" ]; then
        vmdir-command "$vmdir" "dnf install -y$pkgs" ||
            command-error "cannot install$pkgs in $(vmdir-label "$vmdir")"
    fi
    echo "$(vmdir-label "$vmdir"): $* installed"
}

fs-image-build() {
    # Usage: fs-image-build VMDIR TYPE NAME TEXT [SIZE]
    #
    # Build the filesystem image $FS_VM_DIR/NAME.img in the VM of VMDIR
    # with one file, $FS_FILE, whose only line is TEXT. TYPE: ext4 (block
    # size 4096, the page size: DAX), ext4-dirty (the same, marked
    # needs_recovery as if it was not unmounted cleanly), xfs, btrfs,
    # erofs, squashfs. SIZE (mkfs size of ext4, xfs and btrfs) defaults to
    # 64M, 320M (the minimum of mkfs.xfs is 300 MB) and 256M. Fail the test
    # unless blkid finds the type in the image. Set FS_IMAGE,
    # FS_IMAGE_SIZE and FS_IMAGE_SHA256.
    local vmdir="$1" type="$2" name="$3" text="$4" size="$5" label img dir mk want="$2" out
    label=$(vmdir-label "$vmdir")
    img="$FS_VM_DIR/$name.img"
    dir="$FS_VM_DIR/$name.d"
    case "$type" in
        ext4|ext4-dirty) mk="mkfs.ext4 -q -F -b 4096 -d $dir $img ${size:-64M}"; want=ext4;;
        xfs) mk="truncate -s ${size:-320M} $img && mkfs.xfs -q -f $img && m=\$(mktemp -d) && mount -o loop $img \$m && cp $dir/$FS_FILE \$m/ && umount \$m && rmdir \$m";;
        btrfs) mk="truncate -s ${size:-256M} $img && mkfs.btrfs -q -f --rootdir $dir $img";;
        erofs) mk="mkfs.erofs -b4096 $img $dir";;
        squashfs) mk="mksquashfs $dir $img -noappend -quiet";;
        *) error "fs-image-build: unknown type $type";;
    esac
    [ "$type" != ext4-dirty ] || mk+=" && debugfs -w -R 'feature needs_recovery' $img"
    vmdir-command "$vmdir" "rm -rf $dir $img && mkdir -p $dir && printf '%s\n' $(printf %q "$text") > $dir/$FS_FILE && $mk" ||
        command-error "cannot build the $type image $img in $label"
    vmdir-command "$vmdir" "stat -c %s $img && sha256sum $img && blkid -p -o value -s TYPE $img" ||
        command-error "cannot read the image $img in $label"
    out="$COMMAND_OUTPUT"
    [ "$(sed -n 3p <<< "$out")" == "$want" ] || error "fs-image-build: blkid finds $(sed -n 3p <<< "$out") in $img of $label, not $want"
    if [ "$type" == ext4-dirty ]; then
        vmdir-command "$vmdir" "dumpe2fs -h $img 2>/dev/null | grep -E '^Filesystem features:'" ||
            command-error "cannot read the superblock of $img in $label"
        grep -qw needs_recovery <<< "$COMMAND_OUTPUT" || error "fs-image-build: $img of $label is not marked needs_recovery"
    fi
    FS_IMAGE="$img"
    FS_IMAGE_SIZE=$(head -n 1 <<< "$out")
    FS_IMAGE_SHA256=$(sed -n '2s/ .*//p' <<< "$out")
    echo "fs-image-build: $label:$img, $type, $FS_IMAGE_SIZE bytes, sha256 $FS_IMAGE_SHA256"
}

fs-writer-script() {
    # Usage: fs-writer-script KEY IMAGE
    #
    # Print the shell script of a writer container (image $DRA_TOOLS_IMAGE,
    # IMAGE mounted from the node) with the block device of device KEY:
    # show the environment of the device, check that the image fits in
    # CXL_<KEY>_SIZE, zero the first MiB of CXL_<KEY>_PATH (no signature of
    # an earlier filesystem is left there), write the image with dd and
    # print the sha256 of as many bytes of the device as the image has
    # ("device sha256: SUM"). The last line it logs is "writer exit
    # STATUS".
    local key="$1" img="$2"
    cat <<SCRIPT
env | grep ^CXL_${key}_ | sort
dev="\$CXL_${key}_PATH"
size="\$CXL_${key}_SIZE"
bytes=\$(stat -c %s $img)
if [ -b "\$dev" ]; then echo "block device: \$dev"; else echo "not a block device: \$dev"; fi
echo "image $img: \$bytes bytes, device \$dev: \$size bytes"
if [ "\$bytes" -gt "\$size" ]; then
    echo "the image does not fit into the device"
    echo "writer exit 2"
else
    dd if=/dev/zero of="\$dev" bs=1M count=1 conv=fsync && dd if=$img of="\$dev" bs=1M conv=fsync
    rc=\$?
    echo "dd exit \$rc"
    echo "device sha256: \$(head -c "\$bytes" "\$dev" | sha256sum | cut -d' ' -f1)"
    echo "writer exit \$rc"
fi
SCRIPT
}

fs-writer-wait() {
    # Usage: fs-writer-wait VMDIR POD [TIMEOUT]
    #
    # Wait until the writer in pod POD logs "writer exit STATUS", TIMEOUT
    # seconds at most (default 180). Fail the test on timeout or unless
    # STATUS is 0. FS_LOG has the log of the pod then.
    local vmdir="$1" pod="$2" tmo="${3:-180}"
    retry-until --timeout "$tmo" --interval 1 --message "$pod: the writer exits" \
        'FS_LOG=$(dra-pod-log "$vmdir" "$pod") && grep -qE "^writer exit [0-9]+$" <<< "$FS_LOG"' || {
        vmdir-command "$vmdir" "kubectl logs -n $DRA_NS $pod; kubectl describe pod -n $DRA_NS $pod | tail -n 20"
        error "the writer in pod $pod did not exit in ${tmo}s"
    }
    echo "$FS_LOG"
    grep -qx "writer exit 0" <<< "$FS_LOG" || error "the writer in pod $pod failed: $(grep '^writer exit ' <<< "$FS_LOG")"
}

fs-write() {
    # Usage: fs-write VMDIR NAME SERIAL SHARED CLASS
    #
    # Write the image of the last fs-image-build ($FS_IMAGE, built in the
    # VM of VMDIR) into the persistent device SERIAL: ResourceClaim NAME of
    # DeviceClass CLASS for the whole device (no shares request) in mode
    # block, and pod NAME on the node of VMDIR with the image mounted from
    # the node (fs-writer-script). Fail the test unless the writer got the
    # environment of a writable block device (SHARED: 1 or 0, the
    # CXL_<KEY>_SHARED of the device) and its device has the image
    # afterwards. Set FS_NS_SIZE. Delete the pod and the claim; the caller
    # waits until the device is free.
    local vmdir="$1" name="$2" serial="$3" shared="$4" class="$5" key
    key=${serial#0x}
    key=${key^^}
    [ -n "$FS_IMAGE" ] || error "fs-write: no image, run fs-image-build first"
    dra-apply "$OUTPUT_DIR" <<< "$(dra-shared-claim-yaml "$name" "$serial" "" "mode: block" "$class")
---
$(dra-script-pod-yaml "$name" "$vmdir" "$name" "$(fs-writer-script "$key" "$FS_IMAGE")" "File:$FS_IMAGE")"
    fs-writer-wait "$OUTPUT_DIR" "$name"
    dra-env-assert "$name" "$FS_LOG" "$key" \
        SERIAL="$serial" SHARED="$shared" MODE=block "SIZE=[1-9]*" "PATH=/dev/pmem*" READ_ONLY=0
    FS_NS_SIZE=$(dra-env-value "$FS_LOG" "CXL_${key}_SIZE")
    grep -qE "^block device: /dev/pmem[0-9]+$" <<< "$FS_LOG" || error "$name: the writer has no block device"
    grep -qx "dd exit 0" <<< "$FS_LOG" || error "$name: dd of the writer failed: $(grep '^dd exit' <<< "$FS_LOG")"
    grep -qx "device sha256: $FS_IMAGE_SHA256" <<< "$FS_LOG" ||
        error "$name: the device does not have the image $FS_IMAGE (sha256 $FS_IMAGE_SHA256): $(grep '^device sha256:' <<< "$FS_LOG")"
    echo "$name: $FS_IMAGE written into $serial (namespace $FS_NS_SIZE bytes)"
    dra-delete-pod "$OUTPUT_DIR" "$name"
    dra-delete "$OUTPUT_DIR" resourceclaim "$name" --wait=true --timeout=60s
}

# The pods are in the cluster of the VM of the test: the fs-ctr-*
# helpers run kubectl exec there, whatever the node of the pod.

fs-ctr-env() {
    # Usage: fs-ctr-env POD KEY
    #
    # Set FS_ENV to the CXL_KEY_ variables of container main of pod POD,
    # sorted KEY=VALUE lines. Fail the test if they cannot be read.
    dra-exec "$OUTPUT_DIR" "$1/main" env || command-error "cannot read the environment of $1"
    FS_ENV=$(grep "^CXL_$2_" <<< "$COMMAND_OUTPUT" | LC_ALL=C sort)
}

fs-ctr-env-assert() {
    # Usage: fs-ctr-env-assert POD KEY FIELD=VALUE...
    #
    # Fail the test unless the CXL_KEY_ variables of container main of pod
    # POD are exactly the FIELD=VALUEs (dra-env-assert).
    local pod="$1" key="$2"
    shift 2
    fs-ctr-env "$pod" "$key"
    dra-env-assert "$pod" "$FS_ENV" "$key" "$@"
}

fs-ctr-mount-assert() {
    # Usage: fs-ctr-mount-assert POD PATH ro|rw TYPE
    #
    # Fail the test unless container main of pod POD has a mount at PATH of
    # filesystem TYPE whose mount options start with ro or rw, and no pmem
    # block device.
    local pod="$1" path="$2" rw="$3" type="$4" line
    # shellcheck disable=SC2016 # expanded by sh in the container
    dra-exec "$OUTPUT_DIR" "$pod/main" sh -c 'grep " $1 " /proc/self/mountinfo; ls /dev/pmem* 2>/dev/null; true' sh "$path" ||
        command-error "cannot read the mounts of $pod"
    line=$(grep " $path " <<< "$COMMAND_OUTPUT" | tail -n 1)
    python3 -c '
import sys
line, path, rw, fstype = sys.argv[1:5]
pre, sep, post = line.partition(" - ")
f = pre.split()
if not sep or len(f) < 6 or f[4] != path:
    sys.exit("no mount at %s: %r" % (path, line))
if f[5].split(",")[0] != rw or post.split()[0] != fstype:
    sys.exit("%s is %s with %s, not %s %s" % (path, post.split()[0], f[5], rw, fstype))' "$line" "$path" "$rw" "$type" ||
        error "$pod: $path is not a $rw $type mount in its container"
    ! grep -q '^/dev/pmem' <<< "$COMMAND_OUTPUT" || error "$pod has a pmem device: $(grep '^/dev/pmem' <<< "$COMMAND_OUTPUT")"
    echo "$pod: $path is a $rw $type mount, no pmem device"
}

fs-ctr-write() {
    # Usage: fs-ctr-write POD DIR FILE TEXT
    #
    # Write the line TEXT into DIR/FILE in container main of pod POD and
    # sync. Fail the test if that fails.
    # shellcheck disable=SC2016 # expanded by sh in the container
    dra-exec "$OUTPUT_DIR" "$1/main" sh -c 'printf "%s\n" "$3" > "$1/$2" && sync' sh "$2" "$3" "$4" ||
        command-error "$1 cannot write $2/$3"
    echo "$1: wrote $2/$3"
}

fs-ctr-read-assert() {
    # Usage: fs-ctr-read-assert POD DIR FILE TEXT
    #
    # Fail the test unless DIR/FILE in container main of pod POD is the line
    # TEXT.
    dra-exec "$OUTPUT_DIR" "$1/main" cat "$2/$3" || command-error "$1 cannot read $2/$3"
    [ "$COMMAND_OUTPUT" == "$4" ] || error "$1: $2/$3 is '$COMMAND_OUTPUT', expected '$4'"
    echo "$1: $2/$3 has the expected text"
}

fs-ctr-write-refused() {
    # Usage: fs-ctr-write-refused POD DIR
    #
    # Fail the test unless creating a file under DIR in container main of
    # pod POD fails with "Read-only file system".
    dra-exec "$OUTPUT_DIR" "$1/main" touch "$2/refused" &&
        error "$1 could create a file under the read-only $2"
    grep -qF "Read-only file system" <<< "$COMMAND_OUTPUT
$DRA_EXEC_STDERR" || error "$1: creating a file under $2 failed for another reason than \"Read-only file system\": $DRA_EXEC_STDERR"
    echo "$1: a write under $2 is refused: Read-only file system"
}

fs-node-mounts-assert() {
    # Usage: fs-node-mounts-assert VMDIR DIR ID TYPE ro|rw EXTRA ACCESS...
    #
    # Fail the test unless the node mounts under DIR in the VM of VMDIR are
    # exactly DIR/ID-ACCESS for every ACCESS, all of filesystem TYPE on one
    # pmem block device, with:
    # - the superblock ro or rw (the first superblock option),
    # - the mount attributes nosuid and nodev, and ro for the read-only
    #   types erofs and squashfs, rw for the others,
    # - every option of EXTRA (comma-separated, "" none) among the
    #   superblock options,
    # - direct: on /dev/pmemN, with dax=always for erofs, ext4 and xfs,
    # - cached: on a direct-I/O loop device over /dev/pmemN, without
    #   dax=always, the loop device read-only unless a writable type's
    #   superblock is rw.
    # Set FS_PMEM to /dev/pmemN.
    local vmdir="$1" dir="$2" id="$3" type="$4" sb="$5" extra="$6" label
    shift 6
    label=$(vmdir-label "$vmdir")
    dra-fs-mounts "$vmdir" "$dir" || error "cannot read the mounts of $label"
    FS_PMEM=$(python3 -c '
import json, re, sys
mounts, base, mid, fstype, sb, extra, accesses = json.loads(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5], sys.argv[6], sys.argv[7:]
want = sorted("%s/%s-%s" % (base, mid, a) for a in accesses)
got = sorted(m["target"] for m in mounts)
if got != want:
    sys.exit("mounts %s, expected %s" % (got, want))
readonly_type = fstype in ("erofs", "squashfs")
pmem = set()
for m in mounts:
    a = m["target"].rsplit("-", 1)[1]
    opts, vfs = m["options"].split(","), m["vfs"].split(",")
    if m["fstype"] != fstype:
        sys.exit("%s is %s, not %s" % (m["target"], m["fstype"], fstype))
    if opts[0] != sb:
        sys.exit("%s: superblock %s (%s), not %s" % (m["target"], opts[0], m["options"], sb))
    if not {"nosuid", "nodev"} <= set(vfs) or vfs[0] != ("ro" if readonly_type else "rw"):
        sys.exit("%s: mount attributes %s, not %s,nosuid,nodev" % (m["target"], m["vfs"], "ro" if readonly_type else "rw"))
    missing = [o for o in extra.split(",") if o and o not in opts]
    if missing:
        sys.exit("%s: no %s in %s" % (m["target"], ",".join(missing), m["options"]))
    if a == "direct":
        if not re.match(r"^/dev/pmem[0-9]+$", m["source"]):
            sys.exit("direct: %s on %s, not on a pmem block device" % (m["target"], m["source"]))
        if fstype in ("erofs", "ext4", "xfs") and "dax=always" not in opts:
            sys.exit("direct: %s has no dax=always: %s" % (m["target"], m["options"]))
        pmem.add(m["source"])
    else:
        ro = "0" if sb == "rw" and not readonly_type else "1"
        if not re.match(r"^/dev/loop[0-9]+$", m["source"]) or not re.match(r"^/dev/pmem[0-9]+$", m["backing"]) or m["dio"] != "1" or m["ro"] != ro or "dax=always" in opts:
            sys.exit("cached: %s on %s (backing %r, dio %r, ro %r) with %s, expected a direct-I/O loop device over a pmem block device, read-only %s, no dax=always" % (m["target"], m["source"], m["backing"], m["dio"], m["ro"], m["options"], ro))
        pmem.add(m["backing"])
if len(pmem) != 1:
    sys.exit("the mounts are not on one pmem device: %s" % sorted(pmem))
for m in mounts:
    print("mount: %s on %s%s (%s; %s)" % (m["target"], m["source"], " over " + m["backing"] if m["backing"] else "", m["options"], m["vfs"]), file=sys.stderr)
print(pmem.pop())' "$COMMAND_OUTPUT" "$dir" "$id" "$type" "$sb" "$extra" "$@") ||
        error "$label: the node mounts under $dir are not $type ($sb, ${extra:-no extra options}) for $* on one pmem device"
    echo "$label: node mounts of $id: $type, superblock $sb, access $*, on $FS_PMEM"
}

fs-no-mounts-assert() {
    # Usage: fs-no-mounts-assert VMDIR DIR...
    #
    # Fail the test if the VM of VMDIR has a mount under a DIR or a loop
    # device over a pmem block device.
    local vmdir="$1" dir label loops
    shift
    label=$(vmdir-label "$vmdir")
    for dir in "$@"; do
        dra-fs-mounts "$vmdir" "$dir" || error "cannot read the mounts of $label"
        [ "$COMMAND_OUTPUT" == "[]" ] || error "$label has mounts under $dir: $COMMAND_OUTPUT"
    done
    loops=$(dra-pmem-loops "$vmdir") || error "cannot list the loop devices of $label"
    [ -z "$loops" ] || error "$label has loop devices over pmem: $loops"
    echo "$label: no mounts under $*, no loop devices over pmem"
}

fs-pmem-ro-assert() {
    # Usage: fs-pmem-ro-assert VMDIR DEV 0|1
    #
    # Fail the test unless the block device DEV (/dev/pmemN) of the VM of
    # VMDIR is read-only (1) or writable (0): /sys/block/pmemN/ro.
    local ro
    ro=$(vmdir-command-q "$1" "cat /sys/block/${2#/dev/}/ro") || error "cannot read /sys/block/${2#/dev/}/ro in $(vmdir-label "$1")"
    [ "$ro" == "$3" ] || error "$(vmdir-label "$1"): /sys/block/${2#/dev/}/ro is $ro, expected $3"
    echo "$(vmdir-label "$1"): /sys/block/${2#/dev/}/ro is $ro"
}

fs-cdi-assert() {
    # Usage: fs-cdi-assert VMDIR CLAIM KIND HOSTPATH CTRPATH OPTIONS
    #
    # Fail the test unless the VM of VMDIR has the CDI device
    # claim-<uid>-0 of ResourceClaim CLAIM, of KIND, without device nodes
    # and with exactly one mount: a bind of HOSTPATH at CTRPATH with the
    # options OPTIONS (a Python list).
    local vmdir="$1" claim="$2" kind="$3" host="$4" ctr="$5" opts="$6" uid cdi
    dra-json "$OUTPUT_DIR" resourceclaim -n "$DRA_NS" "$claim"
    uid=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
    cdi=$(dra-cdi-device "$vmdir" "claim-$uid-0")
    echo "CDI device claim-$uid-0 of $claim: $cdi"
    [ -n "$cdi" ] || error "no CDI device claim-$uid-0 for $claim"
    dra-assert "$cdi" "j['kind'] == '$kind' and j['deviceNodes'] == []"
    dra-assert "$cdi" "[(m.get('hostPath'), m.get('containerPath'), m.get('type'), m.get('options')) for m in j['mounts']] == [('$host', '$ctr', 'bind', $opts)]"
}

fs-claim-data-assert() {
    # Usage: fs-claim-data-assert CLAIM DEVICE EXPRESSION
    #
    # Fail the test unless the Python EXPRESSION is true on d, the data
    # that the pool controller wrote for DEVICE into the status of
    # ResourceClaim CLAIM ({} if none).
    local json
    dra-json "$OUTPUT_DIR" resourceclaim -n "$DRA_NS" "$1"
    json="$COMMAND_OUTPUT"
    dra-claim-conditions "$OUTPUT_DIR" "$1"
    dra-py assert "$json" "(lambda d: $3)(device_data('$2') or {})" ||
        error "the status data of $2 in claim $1 is not $3: $(dra-value "$json" "device_data('$2')")"
    echo "claim $1: data of $2: $3"
}

fs-node-state() {
    # Usage: fs-node-state VMDIR
    #
    # Print the node mounts of both drivers and the loop devices over pmem
    # block devices of the VM of VMDIR, quietly. Return non-zero if they
    # cannot be read.
    local dir
    for dir in "$DRA_POOL_MOUNT_DIR" "$DRA_LOCAL_MOUNT_DIR"; do
        dra-fs-mounts "$1" "$dir" || return 1
    done
    dra-pmem-loops "$1" || return 1
}

fs-refused() {
    # Usage: fs-refused VMDIR NAME SERIAL PARAMETERS CLASS TEXT...
    #
    # Create a claim of one share of the device SERIAL with the device
    # section PARAMETERS and its pod NAME on the node of VMDIR
    # (dra-serial-claim-pod), wait until an Event of the pod has every TEXT, and
    # fail the test unless the pod waits in ContainerCreating and the node
    # mounts and loop devices of the node are what they were before (the
    # refused claim mounted nothing). Then delete the pod and the claim.
    local vmdir="$1" name="$2" serial="$3" params="$4" class="$5" before after
    shift 5
    before=$(fs-node-state "$vmdir") || error "cannot read the mounts of $(vmdir-label "$vmdir")"
    dra-serial-claim-pod "$vmdir" "$name" "$serial" 1 "$params" "$class"
    dra-pod-event-wait "$name" "$@"
    dra-wait "$OUTPUT_DIR" "$name-creating" "pod -n $DRA_NS $name" \
        "[(cs.get('state') or {}).get('waiting', {}).get('reason') for cs in j['status'].get('containerStatuses') or []] == ['ContainerCreating']" 60
    after=$(fs-node-state "$vmdir") || error "cannot read the mounts of $(vmdir-label "$vmdir")"
    [ "$after" == "$before" ] || error "$name: the node mounts or loop devices of $(vmdir-label "$vmdir") changed for the refused claim:
before: $before
after:  $after"
    echo "$name: refused, nothing mounted"
    dra-claim-pod-delete "$name"
}

fs-controller-log-wait() {
    # Usage: fs-controller-log-wait UNIT TEXT... [-- TIMEOUT]
    #
    # Wait until the journal of the pool controller UNIT in the VM of the
    # test has a line with every TEXT (fixed strings), TIMEOUT seconds at
    # most (default 30), and print the line. Fail the test on timeout.
    local unit="$1" texts=() tmo=30 filter t
    shift
    while [ $# -gt 0 ]; do
        if [ "$1" == -- ]; then
            tmo="$2"
            break
        fi
        texts+=("$1")
        shift
    done
    filter="journalctl --no-pager -o cat -u $unit"
    for t in "${texts[@]}"; do
        filter+=" | grep -F -- $(printf %q "$t")"
    done
    retry-until --timeout "$tmo" --interval 1 --message "the log of $unit has a line with: ${texts[*]}" \
        '[ -n "$(vmdir-command-q "$OUTPUT_DIR" "$filter")" ]' ||
        error "the log of $unit has no line with: ${texts[*]}"
    vmdir-command-q "$OUTPUT_DIR" "$filter" | tail -n 3
}

fs-lease-bind-assert() {
    # Usage: fs-lease-bind-assert VMDIR TARGET NODE-MOUNT TYPE ro|rw
    #
    # Fail the test unless the VM of VMDIR has exactly one mount at TARGET
    # under $DRA_LEASE_FS_DIR, the bind of a lease: a TYPE filesystem with
    # the source and the superblock options of the node mount NODE-MOUNT,
    # and the mount options ro or rw, nosuid and nodev.
    local vm="$1" target="$2" node_mount="$3" type="$4" rw="$5" nodes label
    label=$(vmdir-label "$vm")
    dra-fs-mounts "$vm" "$(dirname "$node_mount")" || error "cannot read the mounts of $label"
    nodes="$COMMAND_OUTPUT"
    dra-lease-fs-mounts "$vm" || error "cannot read the lease mounts of $label"
    python3 -c '
import json, sys
nodes, binds, target, node_mount, fstype, rw = json.loads(sys.argv[1]), json.loads(sys.argv[2]), sys.argv[3], sys.argv[4], sys.argv[5], sys.argv[6]
n = [m for m in nodes if m["target"] == node_mount]
b = [m for m in binds if m["target"] == target]
if len(n) != 1 or len(b) != 1:
    sys.exit("node mount %s: %s, binds at %s: %s" % (node_mount, n, target, b))
n, b = n[0], b[0]
vfs = b["vfs"].split(",")
if b["fstype"] != fstype or b["source"] != n["source"] or b["options"] != n["options"] or vfs[0] != rw or not {"nosuid", "nodev"} <= set(vfs):
    sys.exit("%s is %s on %s (%s; %s), not a %s,nosuid,nodev %s bind of %s on %s (%s)" % (target, b["fstype"], b["source"], b["options"], b["vfs"], rw, fstype, node_mount, n["source"], n["options"]))
print("lease bind %s: %s on %s (%s), %s, the node mount %s" % (target, fstype, b["source"], b["options"], b["vfs"], node_mount))' \
        "$nodes" "$COMMAND_OUTPUT" "$target" "$node_mount" "$type" "$rw" ||
        error "$label: no $rw $type bind of $node_mount at $target"
}

fs-device-free() {
    # Usage: fs-device-free DEVICE VMDIR...
    #
    # Wait until the VMs of VMDIRs have no CXL memory device and DEVICE is
    # free in the server (pool-device-free-wait), and check that no VM has
    # a CXL region, a node mount under $DRA_POOL_MOUNT_DIR or a loop device
    # over pmem. Fail the test on timeout or if something is left.
    local device="$1" vm regions
    shift
    pool-device-free-wait "$device" "$@"
    for vm in "$@"; do
        regions=$(vmdir-command-q "$vm" "ls /sys/bus/cxl/devices/") ||
            error "cannot list the CXL devices of $(vmdir-label "$vm")"
        ! grep -q '^region[0-9]' <<< "$regions" ||
            error "$(vmdir-label "$vm") still has a CXL region: $(grep '^region[0-9]' <<< "$regions" | tr '\n' ' ')"
        fs-no-mounts-assert "$vm" "$DRA_POOL_MOUNT_DIR"
    done
    echo "$device is free: no CXL memory devices, regions, node mounts or loop devices in $*"
}

fs-exit-cleanup() {
    # Usage: fs-exit-cleanup DEVICE CLIENT-ARGS "VMDIR..." FILE...
    #
    # The EXIT trap of a test of the persistent device DEVICE after
    # dra-install: dra-cleanup, then pool-cleanup and the removal of the
    # files FILE of the device, unless something holds the device on the
    # VMs of VMDIRs or that cannot be read (fs-held). Then stop the servers
    # only, leave the device, the VMs and the files as they are for
    # debugging, and print what holds the device and how to clean up by
    # hand: "fake-cxl-pool-client CLIENT-ARGS" in a VM releases it.
    local device="$1" client="$2" vms held
    read -r -a vms <<< "$3"
    shift 3
    dra-cleanup
    held=$(fs-held "${vms[@]}")
    if [ -z "$held" ]; then
        pool-cleanup
        rm -f "$@"
        return 0
    fi
    # Releasing a region under a live filesystem can crash the guest.
    echo "exit-cleanup: $device is still held, not releasing or detaching it:
$held
  To clean up by hand in each VM above: stop what uses the mount (fuser -vm MOUNT),
  umount MOUNT (never umount -l; the lease mounts first), check that no /sys/block/loop*/loop/backing_file
  names the pmem device, then: fake-cxl-pool-client $client.
  Or run the test again: it restarts a VM that still has CXL memory devices
  (pool-vm-reset). Remove $* after that."
    pool-server-stop
}

fs-held() {
    # Usage: fs-held VMDIR...
    #
    # Print what still holds persistent devices on the VMs of VMDIRs, one
    # "  <vm>: ..." line each: mounts under $DRA_POOL_MOUNT_DIR,
    # $DRA_LOCAL_MOUNT_DIR and $DRA_LEASE_FS_DIR, loop devices over pmem
    # block devices, or that they cannot be read. Print nothing if nothing
    # holds them.
    local vm label dir loops
    for vm in "$@"; do
        label=$(vmdir-label "$vm")
        for dir in "$DRA_POOL_MOUNT_DIR" "$DRA_LOCAL_MOUNT_DIR"; do
            if ! dra-fs-mounts "$vm" "$dir" >/dev/null; then
                echo "  $label: cannot read its mounts under $dir"
            elif [ "$COMMAND_OUTPUT" != "[]" ]; then
                echo "  $label: mounts $COMMAND_OUTPUT"
            fi
        done
        if ! dra-lease-fs-mounts "$vm" >/dev/null; then
            echo "  $label: cannot read its lease mounts"
        elif [ "$COMMAND_OUTPUT" != "[]" ]; then
            echo "  $label: lease mounts $COMMAND_OUTPUT"
        fi
        if ! loops=$(dra-pmem-loops "$vm"); then
            echo "  $label: cannot list its loop devices"
        elif [ -n "$loops" ]; then
            echo "  $label: loop devices over pmem: $(tr '\n' ' ' <<< "$loops")"
        fi
    done
}
