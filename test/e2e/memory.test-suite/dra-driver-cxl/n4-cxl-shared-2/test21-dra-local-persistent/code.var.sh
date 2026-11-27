# A persistent local CXL memory device: its block device and a read-write
# filesystem on it for the pods of its node, without a pool
#
# A local memdev of persistent memory (persistent capacity, no RAM) is a
# device of the node like the local memdevs of test14, but its data
# outlives the claims: the cxl.nri.io driver publishes it whole as
# memdev-<serial>, attributes type memdev, source local, serial, shared
# false, persistent true, size and numaNode, capacity shares 1M
# without a requestPolicy, and DeviceClass cxl-local-persistent selects it
# (cxl-local does not). Its claims have the modes of persistent pool
# devices (test16, test20): block, the block device /dev/pmemK of an fsdax
# namespace of a pmem region that the driver creates on the memdev, and
# fs, a filesystem on it that the driver detects and mounts at
# /var/lib/kubelet/plugins/cxl.nri.io/mounts/<serial hex>-<access>. Not
# in sharedMemdevs it is single-host by construction: a claim of one
# share may mount it read-write.
#
# The e2e VMs have no persistent local memdev, and none is made by
# recreating a VM: a second fake-cxl-pool-server, local-pmem, without a
# pool controller, holds the persistent device lp0 (serial 0xc100f5e1)
# and hot-plugs it into VM2 on host bridge 1 over HMP; the default server
# (the pool controller's) is cabled to host bridge 0 only. No ResourceSlice
# of cxl-pool.nri.io names the serial, so the local driver takes the
# hot-added memdev as local and publishes it.
#
# Phases: A the device attached, published, matched by
# cxl-local-persistent and not by cxl-local; B a writable block claim
# with a shares request is refused, then a block writer (no shares) writes
# an ext4 image built in VM2, and the region goes with its claim; B2 the
# driver, with its default configuration (fsTypes: [erofs]), refuses to
# mount the ext4, and is installed again with fsTypes: [erofs, ext4]; C a
# read-write and a read-only fs claim (one share each) share the ext4 on
# VM2 (the image outlived the region of the writer); D an fs lease of one
# share (--share) in a running container reads, next to them, what the
# read-write claim wrote; E the claims go, the region is released, the
# memdev stays enabled, the second server detaches the device, the driver
# withdraws it, nothing is left.
#
# Everything happens on VM2 (n4-cxl-shared-2, the VM of the test), the
# control plane of the cluster of VM2, VM1 and VM0; the pods are pinned to
# it by a nodeSelector (dra-pod-yaml).
#
# Host prerequisites: none. The servers listen on 127.0.0.1:9909 (the
# default one) and 127.0.0.1:9912 (local-pmem); the data and LSA files of
# lp0 are /tmp/fake-cxl-pool-local-pmem/lp0.raw and lp0.lsa (host RAM),
# removed at the end, also after a failure, unless VM2 still mounts the
# device. VM2 builds the ext4 image in /var/tmp/cxl-fs-e2e (e2fsprogs) and
# removes it at the end.
#
# Run test00-up of n4-cxl-shared-1, -0 and -2 first: they create the VMs,
# boot them on the CXL kernel and make them one cluster. The DRA driver
# comes from its image, build it first:
#   make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

echo "### preconditions: one cluster, VM2 control plane, VM1 and VM0 workers (test00-up)"
pool-cluster-require

VM2="$OUTPUT_DIR"      # the VM of this test, the control plane, the node of the device
VM2_NAME="$POOL_VM2_NAME"
LP_SERVER=local-pmem   # the second fake-cxl-pool-server, without a controller
LP_PORT=9912
LP_DEVICE=lp0
LP_SERIAL=0xc100f5e1   # used by no pool server and no other test
LP_HEX=C100F5E1        # the serial in environment variable names
LP_MOUNT_ID=c100f5e1   # the serial in the names of the node mounts
LP_NAME=memdev-c100f5e1   # its device in the cxl.nri.io ResourceSlice
LP_SIZE=$(( 512 << 20 ))
LP_OWNER=e2e:test21
LP_DATA="/tmp/fake-cxl-pool-$LP_SERVER/$LP_DEVICE.raw"
LP_LSA="/tmp/fake-cxl-pool-$LP_SERVER/$LP_DEVICE.lsa"
LP="device('$LP_NAME', '$DRA_LOCAL_DRIVER', '$VM2_NAME')"   # dra-py
LP_REGIONS="[r for r in regions if by_serial($LP_SERIAL) is not None and by_serial($LP_SERIAL)['name'] in sorted(r['memdevs'])]"   # cxl-py
CLASS=cxl-local-persistent
FS_PATH="$DRA_FS_CTR_DIR/$LP_HEX"     # the mount in a container without "path"
MNT_DIRECT="$DRA_LOCAL_MOUNT_DIR/$LP_MOUNT_ID-direct"
LEASE_TEMPLATE=cxl-lease
TAG="dra-local-persistent $(date +%Y-%m-%dT%H:%M:%S.%N)"
TEXT_EXT4="$TAG: the ext4 image"
TEXT_C="$TAG: c.txt, written by fs-rw"
TEXT_LOCAL_BLOCK_SHARE="MemoryConfig: device.mode block: the device is writable only exclusively (no shares request)"
TEXT_FSTYPE_NOT_ALLOWED="filesystem type ext4 is not allowed on this node (fsTypes: erofs)"
# local-pmem has no pool controller, also when dra-install runs again.
DRA_UNCONTROLLED_POOLS=("$LP_SERVER")

exit-cleanup() {
    # Usage: exit-cleanup
    #
    # The EXIT trap after dra-install: fs-exit-cleanup of lp0 (pool-cleanup
    # releases and detaches lp0 if it is still attached).
    fs-exit-cleanup "$LP_DEVICE" "--server http://192.168.76.2:$LP_PORT guest disable $LP_SERIAL" "$VM2" "$LP_DATA" "$LP_LSA"
}

region-wait() {
    # Usage: region-wait yes|no
    #
    # Wait until the memdev of lp0 in VM2 is enabled and has a region (yes)
    # or none (no), 60 s at most. Fail the test on timeout.
    local want="len($LP_REGIONS) == 1"
    [ "$1" == yes ] || want="$LP_REGIONS == []"
    pool-cxl-wait "$VM2" "lp0-region-$1" "by_serial($LP_SERIAL) is not None and enabled(by_serial($LP_SERIAL)) and $want" 60
    echo "VM2: the memdev of $LP_DEVICE is enabled, region: $1"
}

claim-local-assert() {
    # Usage: claim-local-assert CLAIM
    #
    # Fail the test unless ResourceClaim CLAIM is allocated the persistent
    # local device of VM2.
    dra-json "$VM2" resourceclaim -n "$DRA_NS" "$1"
    dra-assert "$COMMAND_OUTPUT" "[(r['driver'], r['device']) for r in results()] == [('$DRA_LOCAL_DRIVER', '$LP_NAME')] and allocated_node() == '$VM2_NAME'"
}

echo "### preconditions: patched qemu, hdm_for_passthrough=on, the filesystems in the kernel, pmem windows"
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"
fs-kernel-require "$VM2" "$(basename "$TOPOLOGY_DIR")"
# The root decoders (CXL windows) of both host bridges allow persistent
# memory: the region of lp0 needs one on host bridge 1.
vmdir-command "$VM2" 'cd /sys/bus/cxl/devices && for d in decoder0.*; do echo "$d cap_pmem $(cat $d/cap_pmem) targets $(cat $d/target_list)"; [ "$(cat $d/cap_pmem)" == 1 ] || exit 1; done' ||
    error "a root decoder of VM2 has no cap_pmem: a persistent memdev gets no region there"

echo "### preconditions: cxl, daxctl and cxl-dump, no CXL memory devices, e2fsprogs"
pool-vm-tools-install "$VM2"
pool-vm-reset "$VM2"
pool-cxl-dump "$VM2" no-devices
cxl-assert 'memdevs == [] and regions == [] and endpoints == []'
fs-tools-install "$VM2" mkfs.ext4 dumpe2fs blkid
vmdir-command "$VM2" "rm -rf $FS_VM_DIR && mkdir -p $FS_VM_DIR" || command-error "cannot create $FS_VM_DIR in VM2"

echo "### preconditions: the default server on host bridge 0 with its pool controller, the driver on every node"
pool-server-start --slots "*hb0"
dra-build
dra-install
trap exit-cleanup EXIT
dra-pull-images "$VM2" "$DRA_TOOLS_IMAGE" "$DRA_COMPANION_IMAGE"
dra-tools-install "$VM2"
dra-trace-start "$VM2"

echo "### A: the second server, without a controller, hot-plugs the persistent device $LP_DEVICE into VM2"
# No pool controller (DRA_UNCONTROLLED_POOLS): no ResourceSlice of
# $DRA_POOL_DRIVER ever names its device.
rm -f "$LP_DATA" "$LP_LSA" || error "cannot remove $LP_DATA and $LP_LSA"
pool-server-start --name "$LP_SERVER" --port "$LP_PORT" --hosts "$VM2" --control hmp --slots "*hb1" --local-devices no "
  - name: $LP_DEVICE
    size: 512Mi
    shared: false
    persistent: true
    serial: $LP_SERIAL"
[ "${DRA_CONTROLLER_UNITS[*]}" == "$DRA_CONTROLLER_UNIT" ] ||
    error "pool controllers ${DRA_CONTROLLER_UNITS[*]}, expected $DRA_CONTROLLER_UNIT only"
pool-device-files --pool "$LP_SERVER" "$LP_DEVICE"
pool-assert "$COMMAND_OUTPUT" "j['persistent'] == True and j['shared'] == False and j['serial'] == '$LP_SERIAL'"
[ "$POOL_DEVICE_DATA" == "$LP_DATA" ] || error "the data file of $LP_DEVICE is $POOL_DEVICE_DATA, expected $LP_DATA"
[ "$POOL_DEVICE_LSA" == "$LP_LSA" ] || error "the LSA file of $LP_DEVICE is $POOL_DEVICE_LSA, expected $LP_LSA"
pool-client-install "$VM2"
pool-host-client --pool "$LP_SERVER" -o json attach "$LP_DEVICE" --host "$VM2_NAME" --owner "$LP_OWNER" ||
    command-error "cannot attach $LP_DEVICE to VM2"
pool-cxl-wait "$VM2" lp0-attached "by_serial($LP_SERIAL) is not None and enabled(by_serial($LP_SERIAL)) and by_serial($LP_SERIAL)['pmemSize'] == $LP_SIZE and by_serial($LP_SERIAL)['ramSize'] == 0" 60
cxl-assert "$LP_REGIONS == []"
# The driver publishes a hot-added memdev at once, or holds it back for
# up to 5 minutes (poolSerials, docs/memory/cxl/local.md).
dra-wait "$VM2" lp0-published resourceslices "$LP is not None" 360
SLICES="$COMMAND_OUTPUT"
dra-kubectl "$VM2" "get resourceslices"
dra-assert "$SLICES" "attr($LP, 'type') == 'memdev' and attr($LP, 'source') == 'local' and attr($LP, 'serial') == '$LP_SERIAL' and attr($LP, 'shared') == False and attr($LP, 'persistent') == True and attr($LP, 'size') == $LP_SIZE"
# No memoryBlockSize, no memory capacity: nothing of it is onlined.
dra-assert "$SLICES" "set(k.split('/')[-1] for k in ${LP}['attributes']) - {'numaNode'} == {'persistent', 'serial', 'shared', 'size', 'source', 'type'}"
dra-assert "$SLICES" "sorted(${LP}.get('capacity') or {}) == ['shares'] and quantity(capacity($LP, 'shares')) == $DRA_SHARES and request_policy($LP, 'shares') is None"
dra-assert "$SLICES" "${LP}.get('allowMultipleAllocations') == True and not ${LP}.get('nodeAllocatableResources')"
dra-assert "$SLICES" "not any(attr(d, 'serial') == '$LP_SERIAL' for d in devices('$DRA_POOL_DRIVER'))"
dra-json "$VM2" deviceclass "$CLASS" cxl-local
dra-assert "$COMMAND_OUTPUT" "names() == sorted(['$CLASS', 'cxl-local'])"
dra-assert "$COMMAND_OUTPUT" "all(t in by_name('$CLASS')['spec']['selectors'][0]['cel']['expression'] for t in ['\"$DRA_LOCAL_DRIVER\"', '\"memdev\"', '.persistent == true'])"
dra-assert "$COMMAND_OUTPUT" "'.persistent == false' in by_name('cxl-local')['spec']['selectors'][0]['cel']['expression']"
dra-unmatched unmatched "$LP_SERIAL" cxl-local

echo "### B: block claims: a writable one with a shares request is refused, the exclusive writer writes an ext4 image"
fs-refused "$VM2" blk-share "$LP_SERIAL" "mode: block" "$CLASS" "$TEXT_LOCAL_BLOCK_SHARE"
pool-cxl-dump "$VM2" blk-share-refused
cxl-assert "$LP_REGIONS == []"
fs-image-build "$VM2" ext4 lp-ext4 "$TEXT_EXT4"
fs-write "$VM2" writer "$LP_SERIAL" 0 "$CLASS"
NS_SIZE="$FS_NS_SIZE"
[[ "$NS_SIZE" =~ ^[0-9]+$ ]] && [ "$NS_SIZE" -ge "$FS_IMAGE_SIZE" ] && [ "$NS_SIZE" -le "$LP_SIZE" ] ||
    error "the fsdax namespace of $LP_DEVICE has $NS_SIZE bytes, expected the image ($FS_IMAGE_SIZE) at least and the device ($LP_SIZE) at most"
# The last claim of the device released its region; the memdev stays.
region-wait no
fs-no-mounts-assert "$VM2" "$DRA_LOCAL_MOUNT_DIR" "$DRA_POOL_MOUNT_DIR"

echo "### B2: the default configuration mounts erofs only; the driver again with ext4 allowed"
fs-refused "$VM2" ext4-default "$LP_SERIAL" "mode: fs" "$CLASS" "$TEXT_FSTYPE_NOT_ALLOWED"
region-wait no
DRA_DRIVER_CONFIG="fsTypes: [erofs, ext4]"
dra-install
trap exit-cleanup EXIT
dra-trace-driver-follow "$VM2"
dra-tools-install "$VM2"
[ "${DRA_CONTROLLER_UNITS[*]}" == "$DRA_CONTROLLER_UNIT" ] ||
    error "pool controllers ${DRA_CONTROLLER_UNITS[*]}, expected $DRA_CONTROLLER_UNIT only"
dra-wait "$VM2" lp0-republished resourceslices "$LP is not None and attr($LP, 'persistent') == True" 360

echo "### C: a read-write and a read-only claim share the ext4 on VM2"
dra-serial-claim-pod "$VM2" fs-rw "$LP_SERIAL" 1 "mode: fs, readOnly: false" "$CLASS"
dra-pod-wait "$VM2" fs-rw Running 180
dra-serial-claim-pod "$VM2" fs-ro "$LP_SERIAL" 1 "mode: fs" "$CLASS"
dra-pod-wait "$VM2" fs-ro Running 120
for p in fs-rw fs-ro; do
    ro=0 rw=rw opts="['bind', 'nosuid', 'nodev']"
    if [ "$p" == fs-ro ]; then
        ro=1 rw=ro opts="['bind', 'ro', 'nosuid', 'nodev']"
    fi
    claim-local-assert "$p"
    fs-ctr-env-assert "$p" "$LP_HEX" \
        SERIAL="$LP_SERIAL" SHARED=0 MODE=fs SIZE="$NS_SIZE" PATH="$FS_PATH" ACCESS=direct READ_ONLY="$ro" FS_TYPE=ext4
    fs-ctr-mount-assert "$p" "$FS_PATH" "$rw" ext4
    fs-ctr-read-assert "$p" "$FS_PATH" "$FS_FILE" "$TEXT_EXT4"
    fs-cdi-assert "$VM2" "$p" "$DRA_LOCAL_DRIVER/device" "$MNT_DIRECT" "$FS_PATH" "$opts"
done
fs-ctr-write fs-rw "$FS_PATH" c.txt "$TEXT_C"
fs-ctr-read-assert fs-ro "$FS_PATH" c.txt "$TEXT_C"
fs-ctr-write-refused fs-ro "$FS_PATH"
# One read-write superblock under the mounts of cxl.nri.io, none of the
# pool driver.
fs-node-mounts-assert "$VM2" "$DRA_LOCAL_MOUNT_DIR" "$LP_MOUNT_ID" ext4 rw "" direct
fs-no-mounts-assert "$VM2" "$DRA_POOL_MOUNT_DIR"
PMEM="$FS_PMEM"
fs-pmem-ro-assert "$VM2" "$PMEM" 0
dra-driver-log-wait "$VM2" "mounted $PMEM at $MNT_DIRECT (ext4, rw, dax=always)"
region-wait yes

echo "### D: an fs lease of one share of the device in a running container reads what fs-rw wrote"
dra-apply "$VM2" <<< "$(dra-lease-template-yaml "$LEASE_TEMPLATE")"
dra-apply "$VM2" <<< "$(dra-pod-yaml leaser "$VM2_NAME" "lease=template:$LEASE_TEMPLATE")"
dra-pod-wait "$VM2" leaser Running 120
dra-json "$VM2" pod -n "$DRA_NS" leaser
dra-json "$VM2" resourceclaim -n "$DRA_NS" "$(dra-value "$COMMAND_OUTPUT" "j['status']['resourceClaimStatuses'][0]['resourceClaimName']")"
LEASE_CLAIM_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
# shellcheck disable=SC2016 # expanded by sh in the container
LEASE_SCRIPT='env | grep "^CXL_'"$LP_HEX"'_" | sort
p="$CXL_'"$LP_HEX"'_PATH"
echo "read: $(cat "$p/c.txt")"
if out=$(touch "$p/lease-probe" 2>&1); then echo "write: done"; else echo "write refused: $out"; fi'
dra-exec "$VM2" leaser/main "$DRA_REQUEST_CTR_BIN" request --source local --serial "$LP_SERIAL" --share --mode fs --name data --wait 120s \
    --exec sh -c "$LEASE_SCRIPT" || command-error "leaser: the fs lease of $LP_DEVICE failed"
LOG="$COMMAND_OUTPUT"
dra-env-assert leaser "$LOG" "$LP_HEX" \
    SERIAL="$LP_SERIAL" SHARED=0 MODE=fs SIZE="$NS_SIZE" PATH="$DRA_LEASE_FS_CTR_DIR/data" ACCESS=direct READ_ONLY=1 FS_TYPE=ext4
grep -qxF "read: $TEXT_C" <<< "$LOG" || error "leaser: the lease did not read what fs-rw wrote: $(grep '^read:' <<< "$LOG")"
grep -qE "^write refused: .*Read-only file system$" <<< "$LOG" ||
    error "leaser: a write through the read-only lease did not fail with \"Read-only file system\": $(grep '^write' <<< "$LOG")"
dra-leases "$VM2" leaser/main
dra-assert "$DRA_LEASES" "[(l['state'], l.get('source'), l.get('mode'), l.get('fsType'), l.get('readOnly'), l.get('serial'), l.get('path')) for l in j] == [('Ready', 'local', 'fs', 'ext4', True, '$LP_SERIAL', '$DRA_LEASE_FS_CTR_DIR/data')]"
LEASE=$(dra-value "$DRA_LEASES" "j[0]['lease']")
dra-json "$VM2" resourceclaim -n "$DRA_NS" "cxl-lease-$LEASE"
dra-assert "$COMMAND_OUTPUT" "[(s['deviceClassName'], s.get('capacity')) for s in subrequests()] == [('$CLASS', {'requests': {'shares': '1'}})] and [(r['driver'], r['device']) for r in results()] == [('$DRA_LOCAL_DRIVER', '$LP_NAME')]"
dra-assert "$COMMAND_OUTPUT" "[(config_section(c, 'device').get('mode'), config_section(c, 'device').get('readOnly')) for c in configs()] == [('fs', True)]"
# The bind of the lease: the ext4 of the node mount of cxl.nri.io,
# read-only, nosuid, nodev.
fs-lease-bind-assert "$VM2" "$DRA_LEASE_FS_DIR/$LEASE_CLAIM_UID/data" "$MNT_DIRECT" ext4 ro
# The one superblock of the node, still read-write for fs-rw.
fs-node-mounts-assert "$VM2" "$DRA_LOCAL_MOUNT_DIR" "$LP_MOUNT_ID" ext4 rw "" direct
fs-pmem-ro-assert "$VM2" "$PMEM" 0
fs-ctr-read-assert fs-ro "$FS_PATH" c.txt "$TEXT_C"
dra-exec "$VM2" leaser/main "$DRA_REQUEST_CTR_BIN" release --lease "$LEASE" --wait 60s ||
    command-error "leaser: cxl-request release --lease $LEASE failed"
dra-assert "$(dra-last-json "$COMMAND_OUTPUT")" "j.get('lease') == '$LEASE' and j.get('state') == 'Released'"
dra-wait "$VM2" lease-gone "pods,resourceclaims -n $DRA_NS" "'cxl-lease-$LEASE' not in names()" 90

echo "### E: nothing holds the device: its region goes, the memdev stays; the second server detaches it"
dra-delete-pod "$VM2" leaser
dra-claim-pod-delete fs-rw fs-ro
dra-delete "$VM2" resourceclaimtemplate "$LEASE_TEMPLATE" --wait=true --timeout=60s
region-wait no
dra-assert-clean "$VM2"
retry-until --timeout 60 --interval 1 --message "VM2 has nothing under $DRA_LEASE_FS_DIR" \
    '[ "$(vmdir-command-q "$VM2" "{ test ! -d $DRA_LEASE_FS_DIR || find $DRA_LEASE_FS_DIR -mindepth 1; } && echo end")" == end ]' ||
    error "VM2 still has under $DRA_LEASE_FS_DIR: $(vmdir-command-q "$VM2" "find $DRA_LEASE_FS_DIR -mindepth 1")"
# The memdev is the node's: the driver never disables it. The server's
# client releases it in the VM, then the server detaches it.
pool-client --pool "$LP_SERVER" "$VM2" guest disable "$LP_SERIAL" ||
    command-error "cannot release $LP_DEVICE in VM2"
pool-host-client --pool "$LP_SERVER" detach "$LP_DEVICE" --host "$VM2_NAME" ||
    command-error "cannot detach $LP_DEVICE from VM2"
pool-cxl-wait "$VM2" lp0-detached "by_serial($LP_SERIAL) is None and regions == []" 60
dra-wait "$VM2" lp0-withdrawn resourceslices "$LP is None" 180
qtree=$(pool-vm-monitor "$VM2" "info qtree -b") || error "cannot read the qemu device tree of VM2"
grep -q '^ *bus: ' <<< "$qtree" || error "no qemu device tree of VM2: $qtree"
! grep -q 'dev: cxl-type3' <<< "$qtree" || error "qemu of VM2 still has a CXL memory device"
vmdir-command "$VM2" "rm -rf $FS_VM_DIR" || command-error "cannot remove $FS_VM_DIR in VM2"
echo "no pods, claims, leases, mounts, CDI devices, memory devices or regions of $LP_DEVICE left"

echo "### what every component of VM2 did, in order"
dra-trace-stop
dra-trace-summary

echo "### cleanup"
trap - EXIT
exit-cleanup
pool-port-busy && error "port $POOL_PORT is still busy after stopping the servers"
pool-port-busy "$LP_PORT" && error "port $LP_PORT is still busy after stopping the servers"
[ ! -e "$LP_DATA" ] && [ ! -e "$LP_LSA" ] || error "$LP_DATA or $LP_LSA is left, see exit-cleanup above"
echo "trace: $TEST_OUTPUT_DIR/trace.txt, summary: $TEST_OUTPUT_DIR/trace-summary.txt"
