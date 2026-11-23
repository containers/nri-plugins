# Any filesystem, read-write, on a shared persistent CXL memory device of
# the pool: ReadWriteOnce across nodes, writers and readers on one node
#
# test16 mounts an erofs read-only on many nodes. Here the device fs0
# (persistent, shared, 512Mi) holds filesystems of other types, and claims
# mount them read-write with MemoryConfig device {mode: fs, readOnly:
# false}, the explicit opt-in; without readOnly a claim of mode fs is
# read-only. The driver detects the filesystem from its superblock
# (CXL_<KEY>_FS_TYPE), and a single-host filesystem (ext4, xfs, btrfs) is
# read-write on one node at a time:
# - ReadWriteOnce: a claim that writes makes the pool attachment of its
#   node exclusive (fake-cxl-pool-controller, NeedsSingleHost), and writes
#   singleHost: true into its status data, which the node needs before it
#   mounts read-write. Claims of other nodes, readers too, get AttachFailed
#   reason Conflict and wait: the scheduler allocates them again until the
#   writer is gone.
# - One node: one superblock per device. Writers and readers of the node
#   share it: read-write binds for the writers, read-only binds for the
#   readers ([bind, nosuid, nodev] and [bind, ro, nosuid, nodev] in their
#   CDI devices). The superblock is read-write while a writer of the node
#   is prepared, and /dev/pmemN is read-only (BLKROSET) otherwise: the last
#   writer's Unprepare remounts the superblock read-only (a clean ext4),
#   the first writer next to readers remounts it read-write, while the
#   readers keep their read-only binds.
# - Refusals before anything is mounted: a declared fsType that is not the
#   filesystem, a read-only mount of an ext4 that needs recovery, a
#   read-write claim of a read-only filesystem (erofs), access direct on a
#   filesystem without DAX (btrfs), access cached next to a read-write
#   mount with access direct, a writer with access cached next to readers
#   with access direct. A writer of a btrfs that a reader of the node has
#   mounted waits: only ext4 and xfs are remounted read-write in place.
# - Leases of the ext4 from a running container, read-only by default and
#   read-write with --read-only=false; a read-write lease on another
#   node is never Ready: its companion claim gets the Conflict, and
#   cxl-request --wait ends with the lease released and Failed.
#
# Phases: A the pool device and its class; B a writer (mode block,
# exclusive, on VM1) writes an ext4 image built in VM1; C rw-a, rw-b and
# ro-c on VM2 share the ext4; D ro-d and rw-e on VM1 wait (Conflict); E
# the writers of VM2 go: VM2 remounts it read-only, ro-d starts on VM1, rw-e still
# waits; F rw-f on VM2 waits until ro-d is gone, then VM2 remounts it read-write
# under ro-c; G access cached next to it is refused; H the refusals,
# squashfs read-only, btrfs read-write after its reader; I xfs: two
# accesses on VM2 and a reader on VM1 at once (nouuid), then a writer on
# VM1; J a writer with the other access than the readers of the node is
# refused, leases; K nothing left.
#
# Proofs: what each pod reads is what another one wrote (on its node or
# the other), the writes of readers fail with "Read-only file system", the
# environment, the mounts in the containers, the node mounts (type,
# superblock ro/rw, mount attributes, dax=always for direct, loop devices
# of cached, nouuid of xfs), /sys/block/pmemN/ro, the CDI mounts, the
# status data of the claims, the attachments of the pool server, the
# conditions of waiting claims, the logs of the driver and the controller,
# the events of refused pods, and nothing left at the end.
#
# The cluster: VM2 (n4-cxl-shared-2, the VM of the test) is the control
# plane, VM1 (n4-cxl-shared-1) and VM0 (n4-cxl-shared-0) workers; the pool
# is cabled to VM1 and VM2. Pods are pinned to their node by a nodeSelector
# (dra-pod-yaml). The writer runs on VM1, where the images are built.
#
# Host prerequisites: none (nothing is downloaded on the host). VM1 installs
# xfsprogs and squashfs-tools (and erofs-utils, btrfs-progs, e2fsprogs if
# missing) with dnf through its proxy, and keeps nothing: the images in
# /var/tmp/cxl-fs-e2e are removed at the end. The data and LSA files of
# fs0 (/tmp/fake-cxl-pool/fs0.raw and fs0.lsa, host RAM) are removed at
# the end, also after a failure, unless a node still mounts the device.
#
# The driver runs with the configuration fsTypes: [erofs, squashfs, ext4,
# xfs, btrfs]: by default it mounts erofs only.
#
# Run test00-up of n4-cxl-shared-1, -0 and -2 first: they create the VMs,
# boot them on the CXL kernel (ext4, xfs, btrfs, squashfs, erofs, FS_DAX)
# and make them one cluster. The DRA driver comes from its image, build it
# first:
#   make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

echo "### preconditions: one cluster, VM2 control plane, VM1 and VM0 workers (test00-up)"
pool-cluster-require

VM1="$POOL_VM1_DIR"    # worker, the node of the writer and the images
VM2="$OUTPUT_DIR"      # control plane, the VM of this test
VM0_NAME="$POOL_VM0_NAME"
VM1_NAME="$POOL_VM1_NAME"
VM2_NAME="$POOL_VM2_NAME"
FS_DEVICE=fs0
FS_SERIAL=0xc1ae0001   # 0xc1ae....: shared pool devices
FS_HEX=C1AE0001        # the serial in environment variable names
FS_MOUNT_ID=c1ae0001   # the serial in the names of the node mounts
FS_DEV_SIZE=$(( 512 << 20 ))
FS_DATA="$POOL_DIR/$FS_DEVICE.raw"   # the files of the device on the host
FS_LSA="$POOL_DIR/$FS_DEVICE.lsa"
FS_PATH="$DRA_FS_CTR_DIR/$FS_HEX"        # the mount in a container without "path"
MNT_DIRECT="$DRA_POOL_MOUNT_DIR/$FS_MOUNT_ID-direct"
MNT_CACHED="$DRA_POOL_MOUNT_DIR/$FS_MOUNT_ID-cached"
CLASS=cxl-pool-persistent
RW="mode: fs, readOnly: false"   # the read-write opt-in
RO="mode: fs"                    # read-only: mode fs without readOnly
CDI_RW="['bind', 'nosuid', 'nodev']"
CDI_RO="['bind', 'ro', 'nosuid', 'nodev']"
TAG="dra-fs-read-write $(date +%Y-%m-%dT%H:%M:%S.%N)"
TEXT_EXT4="$TAG: the ext4 image"
TEXT_A="$TAG: a.txt, written by rw-a on VM2"
TEXT_B="$TAG: b.txt, written by rw-b on VM2"
TEXT_F="$TAG: f.txt, written by rw-f on VM2"
TEXT_SQ="$TAG: the squashfs image"
TEXT_BTRFS="$TAG: the btrfs image"
TEXT_W="$TAG: w.txt, written on btrfs"
TEXT_XFS="$TAG: the xfs image"
TEXT_X="$TAG: x.txt, written on xfs by rw-x on VM1"
TEXT_G="$TAG: g.txt, written by rw-g on VM2"
TEXT_L="$TAG: l.txt, written through a read-write lease on VM2"
LEASE_TEMPLATE=cxl-lease
TEXT_CACHED_NEXT_TO_RW="memdev $FS_SERIAL: it is mounted read-write with access direct on this node: access cached would mount it a second time"
TEXT_RW_CACHED_NEXT_TO_DIRECT="memdev $FS_SERIAL: it is mounted with access direct only on this node: a read-write claim with access cached would mount it a second time"
TEXT_BTRFS_RW_WAITS="memdev $FS_SERIAL: btrfs is mounted read-only on this node and cannot be remounted read-write: a read-write claim waits until the read-only claims of the node are gone"

fs0-free() {
    # Usage: fs0-free
    #
    # Wait until fs0 is free and nothing of it is left in VM2 and VM1
    # (fs-device-free).
    fs-device-free "$FS_DEVICE" "$VM2" "$VM1"
}

exit-cleanup() {
    # Usage: exit-cleanup
    #
    # The EXIT trap after dra-install: fs-exit-cleanup of fs0.
    fs-exit-cleanup "$FS_DEVICE" "guest disable $FS_SERIAL" "$VM2 $VM1" "$FS_DATA" "$FS_LSA"
}

attached-assert() {
    # Usage: attached-assert HOST:SINGLE_HOST...
    #
    # Fail the test unless the attachments of fs0 in the pool server are
    # exactly those of the HOSTs, each attached and single host (True) or
    # not (False).
    local want="" a
    for a in "$@"; do
        want+="('${a%%:*}', ${a#*:}, 'attached'), "
    done
    pool-attachments "$FS_DEVICE"
    pool-assert "$COMMAND_OUTPUT" "sorted((a['host'], a.get('singleHost', False), a['state']) for a in j or []) == sorted([$want])"
}

attached-wait() {
    # Usage: attached-wait HOST:SINGLE_HOST...
    #
    # Wait until attached-assert HOST:SINGLE_HOST... holds, 60 s at most.
    local want="" a
    for a in "$@"; do
        want+="('${a%%:*}', ${a#*:}, 'attached'), "
    done
    retry-until --timeout 60 --message "the attachments of $FS_DEVICE: $*" \
        'pool-host-client -o json attachments --device "$FS_DEVICE" >/dev/null && pool-py assert "$COMMAND_OUTPUT" "sorted((a[\"host\"], a.get(\"singleHost\", False), a[\"state\"]) for a in j or []) == sorted([$want])"' ||
        error "the attachments of $FS_DEVICE are not $*: $COMMAND_OUTPUT"
    attached-assert "$@"
}

conflict-watch() {
    # Usage: conflict-watch CLAIM HOST
    #
    # Wait until the pool controller fails the attachment of the claim
    # CLAIM with AttachFailed reason Conflict, 120 s at most, and fail the
    # test unless the message names HOST, the node whose attachment is in
    # the way.
    dra-claim-condition-watch "$VM2" "$1" "$DRA_POOL_DRIVER/AttachFailed" Conflict 120
    grep -qF "\"$2\"" <<< "$COMMAND_OUTPUT" ||
        error "the AttachFailed Conflict of $1 does not name \"$2\", the node whose attachment is in the way: $COMMAND_OUTPUT"
}

superblock-wait() {
    # Usage: superblock-wait VMDIR ro|rw ACCESS...
    #
    # Wait until the ext4 node mounts of fs0 on the VM of VMDIR have the
    # superblock ro or rw (fs-node-mounts-assert), 60 s at most.
    local vm="$1" sb="$2" accesses
    shift 2
    accesses="$*"
    # shellcheck disable=SC2086 # the accesses are words
    retry-until --timeout 60 --message "$(vmdir-label "$vm"): the ext4 of $FS_DEVICE $sb" \
        'fs-node-mounts-assert "$vm" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" ext4 "$sb" "" $accesses >/dev/null 2>&1' ||
        error "$(vmdir-label "$vm"): the superblock of the ext4 of $FS_DEVICE is not $sb"
    fs-node-mounts-assert "$vm" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" ext4 "$sb" "" "$@"
}

ext4-clean-assert() {
    # Usage: ext4-clean-assert VMDIR DEV
    #
    # Fail the test unless the ext4 on DEV in the VM of VMDIR is clean: no
    # needs_recovery feature, state clean (dumpe2fs -h).
    vmdir-command "$1" "dumpe2fs -h $2 2>/dev/null | grep -E '^(Filesystem features|Filesystem state):'" ||
        command-error "cannot read the ext4 superblock of $2 in $(vmdir-label "$1")"
    ! grep -qw needs_recovery <<< "$COMMAND_OUTPUT" || error "$(vmdir-label "$1"): the ext4 on $2 needs recovery after the downgrade"
    grep -qE '^Filesystem state: +clean$' <<< "$COMMAND_OUTPUT" || error "$(vmdir-label "$1"): the ext4 on $2 is not clean after the downgrade"
}

write-image() {
    # Usage: write-image TYPE TEXT
    #
    # Build an image of TYPE with $FS_FILE TEXT in VM1 and write it into
    # fs0 with a writer on VM1, then wait until fs0 is free.
    fs-image-build "$VM1" "$1" "$1" "$2"
    fs-write "$VM1" "writer-$1" "$FS_SERIAL" 1 "$CLASS"
    fs0-free
}

lease-bind-assert() {
    # Usage: lease-bind-assert VMDIR CLAIM-UID NAME ro|rw
    #
    # Fail the test unless the node of the VM of VMDIR has exactly one mount
    # at $DRA_LEASE_FS_DIR/CLAIM-UID/NAME: a ro or rw bind of the ext4 of
    # the direct node mount (fs-lease-bind-assert).
    fs-lease-bind-assert "$1" "$DRA_LEASE_FS_DIR/$2/$3" "$MNT_DIRECT" ext4 "$4"
}

lease-pod-start() {
    # Usage: lease-pod-start NAME VMDIR
    #
    # Create pod NAME on the node of VMDIR with a lease claim from
    # $LEASE_TEMPLATE in container main, and wait until it runs. Set
    # LEASE_CLAIM_UID to the uid of its lease claim.
    dra-apply "$VM2" <<< "$(dra-pod-yaml "$1" "$(vmdir-name "$2")" "lease=template:$LEASE_TEMPLATE")"
    dra-pod-wait "$VM2" "$1" Running 120
    dra-json "$VM2" pod -n "$DRA_NS" "$1"
    dra-json "$VM2" resourceclaim -n "$DRA_NS" "$(dra-value "$COMMAND_OUTPUT" "j['status']['resourceClaimStatuses'][0]['resourceClaimName']")"
    LEASE_CLAIM_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
}

lease-release() {
    # Usage: lease-release POD LEASE
    #
    # Release lease LEASE of container main of pod POD and fail the test
    # unless it is Released within 60 s and its companion objects are gone.
    dra-exec "$VM2" "$1/main" "$DRA_REQUEST_CTR_BIN" release --lease "$2" --wait 60s ||
        command-error "$1: cxl-request release --lease $2 failed"
    dra-assert "$(dra-last-json "$COMMAND_OUTPUT")" "j.get('lease') == '$2' and j.get('state') == 'Released'"
    dra-wait "$VM2" "lease-$2-gone" "pods,resourceclaims -n $DRA_NS" "'cxl-lease-$2' not in names()" 90
}

echo "### preconditions: patched qemu, hdm_for_passthrough=on, the filesystems of the test in the kernels"
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"
pool-vm-require "$VM1" n4-cxl-shared-1
fs-kernel-require "$VM2" "$(basename "$TOPOLOGY_DIR")"
fs-kernel-require "$VM1" n4-cxl-shared-1

echo "### preconditions: cxl, daxctl and cxl-dump in both VMs, no CXL memory devices; the filesystem tools"
pool-vm-tools-install "$VM2"
pool-vm-tools-install "$VM1"
pool-vm-reset "$VM2"
pool-vm-reset "$VM1"
for vm in "$VM2" "$VM1"; do
    pool-cxl-dump "$vm" no-devices
    cxl-assert 'memdevs == [] and regions == [] and endpoints == []'
done
fs-tools-install "$VM1" mkfs.ext4 debugfs dumpe2fs mkfs.xfs mkfs.btrfs mksquashfs mkfs.erofs blkid
fs-tools-install "$VM2" dumpe2fs
vmdir-command "$VM1" "rm -rf $FS_VM_DIR && mkdir -p $FS_VM_DIR" || command-error "cannot create $FS_VM_DIR in VM1"

echo "### preconditions: fake-cxl-pool-server with the persistent device $FS_DEVICE, its files new"
rm -f "$FS_DATA" "$FS_LSA" || error "cannot remove $FS_DATA and $FS_LSA"
pool-server-start "
  - name: $FS_DEVICE
    size: 512Mi
    shared: true
    persistent: true
    serial: $FS_SERIAL"
trap 'pool-cleanup; rm -f "$FS_DATA" "$FS_LSA"' EXIT
pool-client-install "$VM2" "$VM1"
pool-device-files "$FS_DEVICE"
pool-assert "$COMMAND_OUTPUT" "j['persistent'] == True and j['shared'] == True"
[ "$POOL_DEVICE_DATA" == "$FS_DATA" ] || error "the data file of $FS_DEVICE is $POOL_DEVICE_DATA, expected $FS_DATA"
[ "$POOL_DEVICE_LSA" == "$FS_LSA" ] || error "the LSA file of $FS_DEVICE is $POOL_DEVICE_LSA, expected $FS_LSA"

echo "### preconditions: the pool controller on the control plane, the driver on every node"
# The driver mounts erofs only unless its configuration lists more types:
# the others are not hardened against crafted images (test21 shows the
# default refusing ext4).
DRA_DRIVER_CONFIG="fsTypes: [erofs, squashfs, ext4, xfs, btrfs]"
dra-build
dra-install
trap exit-cleanup EXIT
for vm in "$VM2" "$VM1"; do
    dra-pull-images "$vm" "$DRA_TOOLS_IMAGE" "$DRA_COMPANION_IMAGE"
done
dra-tools-install "$VM2"
dra-trace-start "$VM2" "$VM1"

echo "### A: one cluster, one pool with the persistent device $FS_DEVICE, coherent between hosts, its class"
dra-json "$VM2" nodes
dra-assert "$COMMAND_OUTPUT" "sorted(n['metadata']['name'] for n in j['items']) == sorted(['$VM0_NAME', '$VM1_NAME', '$VM2_NAME'])"
dra-kubectl "$VM2" "get resourceslices"
dra-json "$VM2" resourceslices
D0="device('$FS_DEVICE', '$DRA_POOL_DRIVER')"
dra-assert "$COMMAND_OUTPUT" "len(slices('$DRA_POOL_DRIVER')) == 1 and attr($D0, 'persistent') == True and attr($D0, 'coherent') == True and attr($D0, 'shared') == True and attr($D0, 'serial') == '$FS_SERIAL' and attr($D0, 'size') == $FS_DEV_SIZE"
dra-assert "$COMMAND_OUTPUT" "quantity(capacity($D0, 'shares')) == $DRA_SHARES and request_policy($D0, 'shares') is None"
dra-json "$VM2" deviceclass "$CLASS"
dra-assert "$COMMAND_OUTPUT" "names() == ['$CLASS']"

echo "### B: a writer on VM1 takes $FS_DEVICE exclusively and writes an ext4 image into it"
write-image ext4 "$TEXT_EXT4"
[[ "$FS_NS_SIZE" =~ ^[0-9]+$ ]] && [ "$FS_NS_SIZE" -le "$FS_DEV_SIZE" ] && [ "$FS_NS_SIZE" -ge $(( 320 << 20 )) ] ||
    error "the fsdax namespace of $FS_DEVICE has $FS_NS_SIZE bytes, expected at least the xfs image (320 MiB) and at most the device"
NS_SIZE="$FS_NS_SIZE"

echo "### C: ReadWriteOnce on VM2: two writers and a reader share the ext4"
# rw-a first: its claim makes the attachment of VM2 exclusive. rw-b joins
# the read-write superblock, ro-c gets a read-only bind of it.
dra-serial-claim-pod "$VM2" rw-a "$FS_SERIAL" 1 "$RW" "$CLASS"
dra-pod-wait "$VM2" rw-a Running 180
dra-serial-claim-pod "$VM2" rw-b "$FS_SERIAL" 1 "$RW" "$CLASS"
dra-pod-wait "$VM2" rw-b Running 120
dra-serial-claim-pod "$VM2" ro-c "$FS_SERIAL" 1 "$RO" "$CLASS"
dra-pod-wait "$VM2" ro-c Running 120
for p in rw-a rw-b ro-c; do
    ro=0 rw=rw opts="$CDI_RW"
    if [ "$p" == ro-c ]; then
        ro=1 rw=ro opts="$CDI_RO"
    fi
    fs-ctr-env-assert "$p" "$FS_HEX" \
        SERIAL="$FS_SERIAL" SHARED=1 MODE=fs SIZE="$NS_SIZE" PATH="$FS_PATH" ACCESS=direct READ_ONLY="$ro" FS_TYPE=ext4
    fs-ctr-mount-assert "$p" "$FS_PATH" "$rw" ext4
    fs-ctr-read-assert "$p" "$FS_PATH" "$FS_FILE" "$TEXT_EXT4"
    fs-cdi-assert "$VM2" "$p" "$DRA_POOL_DRIVER/device" "$MNT_DIRECT" "$FS_PATH" "$opts"
done
fs-ctr-write rw-a "$FS_PATH" a.txt "$TEXT_A"
fs-ctr-read-assert rw-b "$FS_PATH" a.txt "$TEXT_A"
fs-ctr-write rw-b "$FS_PATH" b.txt "$TEXT_B"
fs-ctr-read-assert ro-c "$FS_PATH" a.txt "$TEXT_A"
fs-ctr-read-assert ro-c "$FS_PATH" b.txt "$TEXT_B"
fs-ctr-write-refused ro-c "$FS_PATH"
# One superblock on VM2, read-write, DAX, on a writable /dev/pmemN.
fs-node-mounts-assert "$VM2" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" ext4 rw "" direct
PMEM2="$FS_PMEM"
fs-pmem-ro-assert "$VM2" "$PMEM2" 0
dra-driver-log-wait "$VM2" "mounted $PMEM2 at $MNT_DIRECT (ext4, rw, dax=always)"
# The writers' claims: singleHost (the node mounts read-write only with
# it), coherent memory, persistent; the reader's claim coherent too, not
# singleHost.
for p in rw-a rw-b; do
    fs-claim-data-assert "$p" "$FS_DEVICE" "d.get('singleHost') == True and d.get('coherent') == True and d.get('persistent') == True and not d.get('exclusive')"
done
fs-claim-data-assert ro-c "$FS_DEVICE" "d.get('coherent') == True and d.get('persistent') == True and not d.get('singleHost')"
attached-assert "$VM2_NAME:True"
# The attach request of the first writer made the attachment exclusive;
# rw-b joined it without a request.
fs-controller-log-wait "$DRA_CONTROLLER_UNIT" "attach $FS_DEVICE to $VM2_NAME for claim $DRA_NS/rw-a device $FS_DEVICE node $VM2_NAME: single host for a read-write filesystem claim"

echo "### D: claims of VM1 wait while VM2 writes: AttachFailed reason Conflict"
dra-serial-claim-pod "$VM1" ro-d "$FS_SERIAL" 1 "$RO" "$CLASS"
conflict-watch ro-d "$VM2_NAME"
dra-serial-claim-pod "$VM1" rw-e "$FS_SERIAL" 1 "$RW" "$CLASS"
conflict-watch rw-e "$VM2_NAME"
dra-pending-assert ro-d rw-e
attached-assert "$VM2_NAME:True"
pool-cxl-dump "$VM1" while-vm2-writes
cxl-assert 'memdevs == [] and regions == []'
fs-no-mounts-assert "$VM1" "$DRA_POOL_MOUNT_DIR"
# The writers of VM2 are not disturbed.
fs-ctr-read-assert rw-a "$FS_PATH" b.txt "$TEXT_B"

echo "### E: the writers of VM2 go: VM2 remounts the ext4 read-only, ro-d starts on VM1"
dra-claim-pod-delete rw-a rw-b
superblock-wait "$VM2" ro direct
fs-pmem-ro-assert "$VM2" "$PMEM2" 1
dra-driver-log-wait "$VM2" "remounted $MNT_DIRECT read-only"
ext4-clean-assert "$VM2" "$PMEM2"
fs-ctr-read-assert ro-c "$FS_PATH" a.txt "$TEXT_A"
fs-ctr-write-refused ro-c "$FS_PATH"
# The attachment of VM2 is not exclusive any more: the scheduler's next
# attempt of ro-d attaches fs0 to VM1 next to it.
dra-pod-wait "$VM2" ro-d Running 240
fs-ctr-env-assert ro-d "$FS_HEX" \
    SERIAL="$FS_SERIAL" SHARED=1 MODE=fs SIZE="$NS_SIZE" PATH="$FS_PATH" ACCESS=direct READ_ONLY=1 FS_TYPE=ext4
fs-ctr-mount-assert ro-d "$FS_PATH" ro ext4
# What VM2 wrote, read on VM1.
fs-ctr-read-assert ro-d "$FS_PATH" a.txt "$TEXT_A"
fs-ctr-read-assert ro-d "$FS_PATH" b.txt "$TEXT_B"
fs-ctr-write-refused ro-d "$FS_PATH"
fs-node-mounts-assert "$VM1" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" ext4 ro "" direct
fs-pmem-ro-assert "$VM1" "$FS_PMEM" 1
attached-wait "$VM1_NAME:False" "$VM2_NAME:False"
# rw-e would write on VM1 while VM2 has fs0.
conflict-watch rw-e "$VM2_NAME"
dra-pending-assert rw-e
fs-claim-data-assert ro-d "$FS_DEVICE" "not d.get('singleHost')"
dra-claim-pod-delete rw-e

echo "### F: a writer of VM2 waits until VM1 lets go, then VM2 remounts the ext4 read-write under its reader"
dra-serial-claim-pod "$VM2" rw-f "$FS_SERIAL" 1 "$RW" "$CLASS"
conflict-watch rw-f "$VM1_NAME"
dra-pending-assert rw-f
# Only the claim that wants the exclusive attachment fails: ro-c, of the
# same attachment, stays attached.
dra-json "$VM2" resourceclaim -n "$DRA_NS" ro-c
dra-assert "$COMMAND_OUTPUT" "condition('$DRA_POOL_DRIVER/Attached') == 'True' and condition('$DRA_POOL_DRIVER/AttachFailed') != 'True'"
fs-ctr-read-assert ro-c "$FS_PATH" b.txt "$TEXT_B"
dra-claim-pod-delete ro-d
pool-cxl-wait "$VM1" ro-d-released "memdevs == []" 60
fs-no-mounts-assert "$VM1" "$DRA_POOL_MOUNT_DIR"
dra-pod-wait "$VM2" rw-f Running 240
superblock-wait "$VM2" rw direct
fs-pmem-ro-assert "$VM2" "$PMEM2" 0
dra-driver-log-wait "$VM2" "remounted $MNT_DIRECT read-write"
attached-assert "$VM2_NAME:True"
fs-ctr-env-assert rw-f "$FS_HEX" \
    SERIAL="$FS_SERIAL" SHARED=1 MODE=fs SIZE="$NS_SIZE" PATH="$FS_PATH" ACCESS=direct READ_ONLY=0 FS_TYPE=ext4
fs-ctr-write rw-f "$FS_PATH" f.txt "$TEXT_F"
# ro-c kept its read-only bind through the remount read-write and sees the new file.
fs-ctr-mount-assert ro-c "$FS_PATH" ro ext4
fs-ctr-read-assert ro-c "$FS_PATH" f.txt "$TEXT_F"
fs-ctr-write-refused ro-c "$FS_PATH"

echo "### G: one node, two accesses: access cached next to the read-write direct mount is refused"
fs-refused "$VM2" cached-g "$FS_SERIAL" "mode: fs, access: cached" "$CLASS" "$TEXT_CACHED_NEXT_TO_RW"
fs-node-mounts-assert "$VM2" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" ext4 rw "" direct

echo "### H: refusals before anything is mounted, squashfs read-only, btrfs read-write"
# The node mount exists: its type is the filesystem.
fs-refused "$VM2" xfs-on-ext4 "$FS_SERIAL" "mode: fs, fsType: xfs" "$CLASS" "memdev $FS_SERIAL: " "has a filesystem of type ext4, not xfs"
fs-node-mounts-assert "$VM2" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" ext4 rw "" direct
fs-ctr-read-assert rw-f "$FS_PATH" f.txt "$TEXT_F"
dra-claim-pod-delete rw-f ro-c
fs0-free
# An ext4 that was not unmounted cleanly: no read-only mount.
write-image ext4-dirty "$TAG: the dirty ext4 image"
fs-refused "$VM2" dirty-ro "$FS_SERIAL" "$RO" "$CLASS" "memdev $FS_SERIAL: ext4 on /dev/pmem" \
    "needs recovery (it was not unmounted cleanly): a read-only mount cannot recover it, a read-write claim on one node does"
fs0-free
# squashfs: read-only, no DAX: access cached by default, a read-only loop
# device.
write-image squashfs "$TEXT_SQ"
dra-serial-claim-pod "$VM2" sq-r "$FS_SERIAL" 1 "$RO" "$CLASS"
dra-pod-wait "$VM2" sq-r Running 180
fs-ctr-env-assert sq-r "$FS_HEX" \
    SERIAL="$FS_SERIAL" SHARED=1 MODE=fs SIZE="$NS_SIZE" PATH="$FS_PATH" ACCESS=cached READ_ONLY=1 FS_TYPE=squashfs
fs-ctr-mount-assert sq-r "$FS_PATH" ro squashfs
fs-ctr-read-assert sq-r "$FS_PATH" "$FS_FILE" "$TEXT_SQ"
fs-ctr-write-refused sq-r "$FS_PATH"
fs-node-mounts-assert "$VM2" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" squashfs ro "" cached
dra-claim-pod-delete sq-r
fs0-free
# erofs is read-only: no read-write claim.
write-image erofs "$TAG: the erofs image"
fs-refused "$VM2" erofs-rw "$FS_SERIAL" "$RW" "$CLASS" "memdev $FS_SERIAL: erofs is a read-only filesystem: set readOnly: true"
fs0-free
# btrfs has no DAX: no access direct. It cannot be remounted read-write
# in place: a writer waits while a reader of the node has it mounted, and
# mounts it read-write, access cached by default, on a writable loop
# device once the reader is gone.
write-image btrfs "$TEXT_BTRFS"
fs-refused "$VM2" btrfs-direct "$FS_SERIAL" "mode: fs, access: direct" "$CLASS" "memdev $FS_SERIAL: btrfs has no DAX: use access cached"
fs0-free
dra-serial-claim-pod "$VM2" btrfs-ro "$FS_SERIAL" 1 "$RO" "$CLASS"
dra-pod-wait "$VM2" btrfs-ro Running 180
fs-ctr-env-assert btrfs-ro "$FS_HEX" \
    SERIAL="$FS_SERIAL" SHARED=1 MODE=fs SIZE="$NS_SIZE" PATH="$FS_PATH" ACCESS=cached READ_ONLY=1 FS_TYPE=btrfs
fs-ctr-read-assert btrfs-ro "$FS_PATH" "$FS_FILE" "$TEXT_BTRFS"
fs-ctr-write-refused btrfs-ro "$FS_PATH"
fs-node-mounts-assert "$VM2" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" btrfs ro "" cached
dra-serial-claim-pod "$VM2" btrfs-rw "$FS_SERIAL" 1 "$RW" "$CLASS"
dra-pod-event-wait btrfs-rw "$TEXT_BTRFS_RW_WAITS"
fs-node-mounts-assert "$VM2" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" btrfs ro "" cached
fs-ctr-read-assert btrfs-ro "$FS_PATH" "$FS_FILE" "$TEXT_BTRFS"
dra-claim-pod-delete btrfs-ro
dra-pod-wait "$VM2" btrfs-rw Running 240
fs-ctr-env-assert btrfs-rw "$FS_HEX" \
    SERIAL="$FS_SERIAL" SHARED=1 MODE=fs SIZE="$NS_SIZE" PATH="$FS_PATH" ACCESS=cached READ_ONLY=0 FS_TYPE=btrfs
fs-ctr-mount-assert btrfs-rw "$FS_PATH" rw btrfs
fs-ctr-read-assert btrfs-rw "$FS_PATH" "$FS_FILE" "$TEXT_BTRFS"
fs-ctr-write btrfs-rw "$FS_PATH" w.txt "$TEXT_W"
fs-ctr-read-assert btrfs-rw "$FS_PATH" w.txt "$TEXT_W"
fs-node-mounts-assert "$VM2" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" btrfs rw "" cached
fs-claim-data-assert btrfs-rw "$FS_DEVICE" "d.get('singleHost') == True"
attached-assert "$VM2_NAME:True"
dra-claim-pod-delete btrfs-rw
fs0-free
# A declared type that the driver does not know, on the btrfs.
fs-refused "$VM2" f2fs-on-btrfs "$FS_SERIAL" "mode: fs, fsType: f2fs" "$CLASS" "memdev $FS_SERIAL: " "has a filesystem of type btrfs, not f2fs"
fs0-free

echo "### I: xfs: direct and cached readers on VM2 and a reader on VM1 at once, then a writer on VM1"
write-image xfs "$TEXT_XFS"
dra-serial-claim-pod "$VM2" ro-x1 "$FS_SERIAL" 1 "mode: fs, access: direct" "$CLASS"
dra-serial-claim-pod "$VM2" ro-x2 "$FS_SERIAL" 1 "mode: fs, access: cached" "$CLASS"
dra-serial-claim-pod "$VM1" ro-x3 "$FS_SERIAL" 1 "$RO" "$CLASS"
declare -A X_ACCESS=([ro-x1]=direct [ro-x2]=cached [ro-x3]=direct)
for p in ro-x1 ro-x2 ro-x3; do
    dra-pod-wait "$VM2" "$p" Running 180
done
for p in ro-x1 ro-x2 ro-x3; do
    fs-ctr-env-assert "$p" "$FS_HEX" \
        SERIAL="$FS_SERIAL" SHARED=1 MODE=fs SIZE="$NS_SIZE" PATH="$FS_PATH" ACCESS="${X_ACCESS[$p]}" READ_ONLY=1 FS_TYPE=xfs
    fs-ctr-mount-assert "$p" "$FS_PATH" ro xfs
    fs-ctr-read-assert "$p" "$FS_PATH" "$FS_FILE" "$TEXT_XFS"
    fs-ctr-write-refused "$p" "$FS_PATH"
done
# Two superblocks of one xfs on VM2, both read-only: nouuid.
fs-node-mounts-assert "$VM2" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" xfs ro nouuid direct cached
fs-pmem-ro-assert "$VM2" "$FS_PMEM" 1
dra-driver-log-wait "$VM2" "at $MNT_DIRECT (xfs, ro, "
dra-driver-log-wait "$VM2" "at $MNT_CACHED (xfs, ro, "
fs-node-mounts-assert "$VM1" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" xfs ro nouuid direct
attached-assert "$VM1_NAME:False" "$VM2_NAME:False"
dra-claim-pod-delete ro-x1 ro-x2 ro-x3
fs0-free
dra-serial-claim-pod "$VM1" rw-x "$FS_SERIAL" 1 "$RW" "$CLASS"
dra-pod-wait "$VM2" rw-x Running 180
fs-ctr-env-assert rw-x "$FS_HEX" \
    SERIAL="$FS_SERIAL" SHARED=1 MODE=fs SIZE="$NS_SIZE" PATH="$FS_PATH" ACCESS=direct READ_ONLY=0 FS_TYPE=xfs
fs-ctr-mount-assert rw-x "$FS_PATH" rw xfs
fs-ctr-write rw-x "$FS_PATH" x.txt "$TEXT_X"
fs-ctr-read-assert rw-x "$FS_PATH" x.txt "$TEXT_X"
fs-node-mounts-assert "$VM1" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" xfs rw "" direct
fs-pmem-ro-assert "$VM1" "$FS_PMEM" 0
attached-assert "$VM1_NAME:True"
dra-claim-pod-delete rw-x
fs0-free

echo "### J: leases of the ext4 from running containers"
write-image ext4 "$TEXT_EXT4"
# First one node, two accesses without a writer: a writer with the other
# access than the readers' is refused.
dra-serial-claim-pod "$VM2" ro-j "$FS_SERIAL" 1 "$RO" "$CLASS"
dra-pod-wait "$VM2" ro-j Running 180
fs-refused "$VM2" rw-cached "$FS_SERIAL" "mode: fs, readOnly: false, access: cached" "$CLASS" "$TEXT_RW_CACHED_NEXT_TO_DIRECT"
fs-node-mounts-assert "$VM2" "$DRA_POOL_MOUNT_DIR" "$FS_MOUNT_ID" ext4 ro "" direct
dra-claim-pod-delete ro-j
dra-apply "$VM2" <<< "$(dra-lease-template-yaml "$LEASE_TEMPLATE")"
dra-serial-claim-pod "$VM2" rw-g "$FS_SERIAL" 1 "$RW" "$CLASS"
dra-pod-wait "$VM2" rw-g Running 180
fs-ctr-write rw-g "$FS_PATH" g.txt "$TEXT_G"
lease-pod-start lease-a "$VM2"
LEASE_A_UID="$LEASE_CLAIM_UID"
# A read-only lease (the default of fs) next to the writer of the node: it
# reads what rw-g wrote, its writes fail.
# shellcheck disable=SC2016 # expanded by sh in the container
LEASE_SCRIPT='env | grep "^CXL_'"$FS_HEX"'_" | sort
p="$CXL_'"$FS_HEX"'_PATH"
echo "read: $(cat "$p/$1")"
if out=$(touch "$p/lease-probe" 2>&1); then echo "write: done"; else echo "write refused: $out"; fi'
dra-exec "$VM2" lease-a/main "$DRA_REQUEST_CTR_BIN" request --serial "$FS_SERIAL" --share --mode fs --name ro --wait 120s \
    --exec sh -c "$LEASE_SCRIPT" sh g.txt || command-error "lease-a: the read-only lease of $FS_DEVICE failed"
LOG_RO="$COMMAND_OUTPUT"
dra-env-assert "lease-a ro" "$LOG_RO" "$FS_HEX" \
    SERIAL="$FS_SERIAL" SHARED=1 MODE=fs SIZE="$NS_SIZE" PATH="$DRA_LEASE_FS_CTR_DIR/ro" ACCESS=direct READ_ONLY=1 FS_TYPE=ext4
grep -qxF "read: $TEXT_G" <<< "$LOG_RO" || error "lease-a: the read-only lease did not read what rw-g wrote: $(grep '^read:' <<< "$LOG_RO")"
grep -qE "^write refused: .*Read-only file system$" <<< "$LOG_RO" ||
    error "lease-a: a write through the read-only lease did not fail with \"Read-only file system\": $(grep '^write' <<< "$LOG_RO")"
dra-leases "$VM2" lease-a/main
dra-assert "$DRA_LEASES" "[(l['state'], l.get('source'), l.get('mode'), l.get('fsType'), l.get('readOnly'), l.get('serial'), l.get('path')) for l in j] == [('Ready', 'pool', 'fs', 'ext4', True, '$FS_SERIAL', '$DRA_LEASE_FS_CTR_DIR/ro')]"
LEASE_RO=$(dra-value "$DRA_LEASES" "j[0]['lease']")
lease-bind-assert "$VM2" "$LEASE_A_UID" ro ro
# A read-write lease on the same node: its companion claim writes, its
# file reaches rw-g.
dra-exec "$VM2" lease-a/main "$DRA_REQUEST_CTR_BIN" request --serial "$FS_SERIAL" --share --mode fs --read-only=false --name rw --wait 120s \
    --exec sh -c 'env | grep "^CXL_'"$FS_HEX"'_" | sort; printf "%s\n" "$1" > "$CXL_'"$FS_HEX"'_PATH/l.txt" && sync && echo "write: done"' sh "$TEXT_L" ||
    command-error "lease-a: the read-write lease of $FS_DEVICE failed"
LOG_RW="$COMMAND_OUTPUT"
dra-env-assert "lease-a rw" "$LOG_RW" "$FS_HEX" \
    SERIAL="$FS_SERIAL" SHARED=1 MODE=fs SIZE="$NS_SIZE" PATH="$DRA_LEASE_FS_CTR_DIR/rw" ACCESS=direct READ_ONLY=0 FS_TYPE=ext4
grep -qx "write: done" <<< "$LOG_RW" || error "lease-a: the write through the read-write lease failed"
fs-ctr-read-assert rw-g "$FS_PATH" l.txt "$TEXT_L"
dra-leases "$VM2" lease-a/main
LEASE_RW=$(dra-value "$DRA_LEASES" "[l['lease'] for l in j if l.get('path') == '$DRA_LEASE_FS_CTR_DIR/rw'][0]")
dra-assert "$DRA_LEASES" "[(l['state'], l.get('fsType'), l.get('readOnly')) for l in j if l['lease'] == '$LEASE_RW'] == [('Ready', 'ext4', False)]"
lease-bind-assert "$VM2" "$LEASE_A_UID" rw rw
dra-json "$VM2" resourceclaim -n "$DRA_NS" "cxl-lease-$LEASE_RW"
dra-assert "$COMMAND_OUTPUT" "[(s['deviceClassName'], s.get('capacity')) for s in subrequests()] == [('$CLASS', {'requests': {'shares': '1'}})]"
dra-assert "$COMMAND_OUTPUT" "[config_section(c, 'device').get('readOnly') for c in configs()] == [False] and [config_section(c, 'device').get('mode') for c in configs()] == ['fs']"
fs-claim-data-assert "cxl-lease-$LEASE_RW" "$FS_DEVICE" "d.get('singleHost') == True"
attached-assert "$VM2_NAME:True"
lease-release lease-a "$LEASE_RW"
lease-release lease-a "$LEASE_RO"
dra-lease-fs-mounts "$VM2" || error "cannot read the lease mounts of VM2"
[ "$COMMAND_OUTPUT" == "[]" ] || error "VM2 still has lease mounts after the release: $COMMAND_OUTPUT"
fs-ctr-read-assert rw-g "$FS_PATH" l.txt "$TEXT_L"
# A read-write lease on VM1 while rw-g writes on VM2: never Ready. With
# --wait it times out, is released and Failed; with --wait 0s it stays,
# its companion claim waiting with AttachFailed reason Conflict.
lease-pod-start lease-b "$VM1"
dra-exec "$VM2" lease-b/main "$DRA_REQUEST_CTR_BIN" request --serial "$FS_SERIAL" --share --mode fs --read-only=false --wait 30s &&
    error "lease-b: a read-write lease on VM1 got Ready while rw-g writes on VM2"
[ "$COMMAND_STATUS" == 3 ] || error "lease-b: cxl-request request exited $COMMAND_STATUS, expected 3 (not Ready in time)"
LEASE_B_JSON=$(dra-last-json "$COMMAND_OUTPUT
$DRA_EXEC_STDERR") || error "lease-b: no response of cxl-request request"
dra-assert "$LEASE_B_JSON" "j.get('state') == 'Failed' and 'timeout: not Ready after' in (j.get('error') or '')"
LEASE_B=$(dra-value "$LEASE_B_JSON" "j['lease']")
dra-wait "$VM2" "lease-$LEASE_B-gone" "pods,resourceclaims -n $DRA_NS" "'cxl-lease-$LEASE_B' not in names()" 90
dra-exec "$VM2" lease-b/main "$DRA_REQUEST_CTR_BIN" request --serial "$FS_SERIAL" --share --mode fs --read-only=false --wait 0s ||
    command-error "lease-b: cxl-request request --wait 0s failed"
LEASE_B_JSON=$(dra-last-json "$COMMAND_OUTPUT") || error "lease-b: no response of cxl-request request"
dra-assert "$LEASE_B_JSON" "j.get('state') in ('Creating', 'Pending', 'Lending') and j.get('lease')"
LEASE_B=$(dra-value "$LEASE_B_JSON" "j['lease']")
dra-wait "$VM2" lease-b-companion "resourceclaims -n $DRA_NS" "'cxl-lease-$LEASE_B' in names()" 60
conflict-watch "cxl-lease-$LEASE_B" "$VM2_NAME"
dra-exec "$VM2" lease-b/main "$DRA_REQUEST_CTR_BIN" env --lease "$LEASE_B" --wait 10s &&
    error "lease-b: lease $LEASE_B got Ready while rw-g writes on VM2"
[ "$COMMAND_STATUS" == 3 ] || error "lease-b: cxl-request env --lease $LEASE_B exited $COMMAND_STATUS, expected 3 (not Ready)"
attached-assert "$VM2_NAME:True"
fs-no-mounts-assert "$VM1" "$DRA_POOL_MOUNT_DIR" "$DRA_LEASE_FS_DIR"
lease-release lease-b "$LEASE_B"
fs-ctr-read-assert rw-g "$FS_PATH" g.txt "$TEXT_G"
dra-delete-pod "$VM2" lease-a
dra-delete-pod "$VM2" lease-b
dra-claim-pod-delete rw-g
dra-wait "$VM2" leases-gone "pods,resourceclaims -n $DRA_NS" "names() == []" 90
dra-delete "$VM2" resourceclaimtemplate "$LEASE_TEMPLATE" --wait=true --timeout=60s

echo "### K: nothing is left"
fs0-free
dra-assert-clean "$VM2" "$VM1"
for vm in "$VM2" "$VM1"; do
    label=$(vmdir-label "$vm")
    retry-until --timeout 60 --interval 1 --message "$label has nothing under $DRA_LEASE_FS_DIR" \
        '[ "$(vmdir-command-q "$vm" "{ test ! -d $DRA_LEASE_FS_DIR || find $DRA_LEASE_FS_DIR -mindepth 1; } && echo end")" == end ]' ||
        error "$label still has under $DRA_LEASE_FS_DIR: $(vmdir-command-q "$vm" "find $DRA_LEASE_FS_DIR -mindepth 1")"
    qtree=$(pool-vm-monitor "$vm" "info qtree -b") || error "cannot read the qemu device tree of $label"
    grep -q '^ *bus: ' <<< "$qtree" || error "no qemu device tree of $label: $qtree"
    ! grep -q 'dev: cxl-type3' <<< "$qtree" || error "qemu of $label still has a CXL memory device"
done
vmdir-command "$VM1" "rm -rf $FS_VM_DIR" || command-error "cannot remove $FS_VM_DIR in VM1"
echo "no pods, claims, leases, mounts, lease directories, loop devices, CDI devices, memory devices or regions of $FS_DEVICE left"

echo "### what every component of VM2 and VM1 did, in order"
dra-trace-stop
dra-trace-summary

echo "### cleanup"
trap - EXIT
exit-cleanup
pool-port-busy && error "port $POOL_PORT is still busy after stopping the server"
[ ! -e "$FS_DATA" ] && [ ! -e "$FS_LSA" ] || error "$FS_DATA or $FS_LSA is left, see exit-cleanup above"
echo "trace: $TEST_OUTPUT_DIR/trace.txt, summary: $TEST_OUTPUT_DIR/trace-summary.txt"
