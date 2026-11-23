# A node-local CXL memory device shared by the claims of pods: one
# exclusive writer, then read-only readers that wait for it
#
# test14 slices a local CXL memory device: every claim of DeviceClass
# cxl-local gets a piece of its own, zeroed for it. A local device can be
# shared instead, like a shared device of the pool (test13, test19): the
# driver configuration lists it, sharedMemdevs: ["<serial>"], and the node
# plugin then publishes it whole as a shared device, attributes type
# memdev, source local, shared true, serial, size and numaNode, and
# one capacity, "shares", 1M without a requestPolicy. Claims of class
# cxl-local-shared get the whole device as one dax device:
# - A claim without a shares request consumes all shares: the device is
#   its own, and the scheduler allocates the device to no other claim
#   until it is gone. Claims that ask meanwhile wait, their pods Pending.
# - A claim with capacity.requests.shares: "1" is one share: any number
#   of such claims hold the device at the same time, each with its own
#   MemoryConfig (device section readOnly: true gives a device node that
#   the container can only open for reading).
# All of them get the same /dev/daxX.0 of the one region of the device,
# CXL_<KEY>_SHARED=1 and _MODE=dax, and see the same bytes. The node
# plugin zeroes the device once, when it creates the region for the first
# claim; the region stays while the device is configured shared, also
# when no claim uses it, and what the last writer wrote stays there.
# The region goes only when the driver runs without the device in
# sharedMemdevs; then the device is a sliced local device again.
# A running container leases the device with cxl-request as it would
# claim it: the broker makes a companion claim of class cxl-local-shared
# for each lease, one share or all of them.
#
# What you will see: the shared device in the node's ResourceSlice, a
# claim of cxl-local that it does not match, the writer creating the
# region, finding zeros where the test left stale bytes, and writing, two
# readers waiting while it runs and both getting the device when it goes,
# a third reader joining, an exclusive claim waiting for the readers, two
# pods of one claim, refused configurations (mode numa, readOnly with
# mode numa, access, shares "0" that the scheduler allocates and the
# driver refuses), the bytes surviving a period without claims, leases of
# the device (an exclusive one that reads the kept bytes, one share by
# size, the local device before the pool, a refused pool request, a
# read-only share mapped when its pod goes), two restarts of the driver,
# and the region going when the device is no longer shared; at the end
# the trace of what every component did.
#
# The local device is cxl_memdev0 of the topology (1G, host bridge 0),
# attached through fake-cxl-pool-server as in test14; the pool has the
# shared device shared0 for the alternative of leases by size. All pods
# run on this VM (VM2), the control plane of the cluster of the test; VM1
# (n4-cxl-shared-1) and VM0 (n4-cxl-shared-0), its workers, have no local
# device attached. Run test00-up of n4-cxl-shared-1, -0 and -2 first: they
# create the VMs and make them one cluster. The DRA driver comes from its
# image, which has cxl-request too, build it first:
#   make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

echo "### preconditions: one cluster, VM2 control plane, VM1 and VM0 workers (test00-up)"
pool-cluster-require

VM1="$POOL_VM1_DIR"    # a worker of the cluster
VM2="$OUTPUT_DIR"      # the VM of this test, the control plane
VM2_NAME="$POOL_VM2_NAME"
SHARED_CLASS=cxl-local-shared
SHARED0_SERIAL=0xc1ae0001   # 0xc1ae....: shared pool devices
TAG="dra-local-shared $(date +%Y-%m-%dT%H:%M:%S.%N)"
TEXT0="$TAG: at 0, from the writer"
TEXT1M="$TAG: at 1M, from the writer"
STALE="stale bytes of $TAG"   # dra-local-seed: what the driver must zero
CLAIM_JSON=""          # claim-check: the claim
CLAIM_UID=""           # and its uid
POD_ENV=""             # env-check: the CXL_ variables of the pod
POD_DAX=""             # and its dax device
CDI_NODE=""            # cdi-check: the device nodes of the CDI device
SHARED_REGION=""       # B: the region of the shared device, kept until H
SHARED_DAX=""          # its dax device (daxX.Y)
SHARED_DEV=""          # the major:minor of that
SHARED_MEMDEV=""       # and the memdev (memN)
COMPANION_JSON=""      # lease-companion: the companion claim of a lease

claim-pod() {
    # Usage: claim-pod NAME SHARES PARAMETERS SCRIPT
    #
    # Create ResourceClaim NAME of class cxl-local-shared for the local
    # device with SHARES ("1" a share, "" exclusive) and the fields
    # PARAMETERS of the device section of its MemoryConfig, and pod NAME on
    # VM2 with the claim, running the shell SCRIPT (dra-serial-claim-pod).
    dra-serial-claim-pod "$VM2" "$1" "$DRA_LOCAL_SERIAL" "$2" "$3" "$SHARED_CLASS" "$4"
}

reader-script() {
    # Usage: reader-script
    #
    # Print the script of a reader: print its CXL_ variables, check that
    # the device can be neither opened for writing nor mapped writable,
    # and watch the string at offset 0 in the background, holding a
    # read-only mapping until /tmp/stop exists (dax-rw.py watch: "read:
    # TEXT", "holding", "final: TEXT").
    cat <<EOF
env | grep ^CXL_ | sort
python3 /tools/dax-rw.py check-ro "\$CXL_${DRA_LOCAL_HEX}_DAX" 0 || exit 1
python3 /tools/dax-rw.py watch "\$CXL_${DRA_LOCAL_HEX}_DAX" 0 /tmp/stop &
EOF
}

claim-check() {
    # Usage: claim-check NAME SHARES
    #
    # Check that ResourceClaim NAME is allocated to the local device on VM2
    # and consumed SHARES shares of it. CLAIM_JSON and CLAIM_UID are the
    # claim.
    dra-json "$VM2" resourceclaim -n "$DRA_NS" "$1"
    CLAIM_JSON="$COMMAND_OUTPUT"
    dra-assert "$CLAIM_JSON" "[(r['driver'], r['device']) for r in results()] == [('$DRA_LOCAL_DRIVER', '$DRA_LOCAL_DEVICE')] and allocated_node() == '$VM2_NAME'"
    dra-assert "$CLAIM_JSON" "quantity((results()[0].get('consumedCapacity') or {}).get('shares', 0)) == $2"
    CLAIM_UID=$(dra-value "$CLAIM_JSON" "j['metadata']['uid']")
}

env-check() {
    # Usage: env-check POD READONLY
    #
    # Check the CXL_ variables of container main of pod POD: the shared
    # local device in mode dax, the whole device, READONLY (0 or 1), and
    # the marker of the result of its claim, nothing else; its dax device
    # is the dax device of the region of the shared device
    # (DRA_LOCAL_SHARED_DAX), a character device with the numbers of that
    # dax device (SHARED_DEV).
    # POD_ENV are the variables, POD_DAX the device.
    local pod="$1" ro="$2"
    # shellcheck disable=SC2016 # expanded by sh in the container
    dra-exec "$VM2" "$pod" sh -c 'env | grep ^CXL_ | sort' ||
        command-error "cannot read the environment of $pod"
    POD_ENV="$COMMAND_OUTPUT"
    dra-env-assert "$pod" "$POD_ENV" "$DRA_LOCAL_HEX" \
        SERIAL="$DRA_LOCAL_SERIAL" SHARED=1 MODE=dax SIZE="$DRA_LOCAL_SIZE" "DAX=/dev/dax*" READ_ONLY="$ro"
    [ "$(grep -cE '^CXL_CLAIM_[0-9a-f_]+_0=1$' <<< "$POD_ENV")" == 1 ] && [ "$(grep -c '^CXL_' <<< "$POD_ENV")" == 7 ] ||
        error "$pod does not have the six variables of the device and one marker of result 0 of a claim: $POD_ENV"
    POD_DAX=$(dra-env-value "$POD_ENV" "CXL_${DRA_LOCAL_HEX}_DAX")
    [ "$POD_DAX" == "/dev/$DRA_LOCAL_SHARED_DAX" ] ||
        error "$pod has $POD_DAX, expected /dev/$DRA_LOCAL_SHARED_DAX, the dax device of the shared device"
    dra-exec "$VM2" "$pod" python3 -c 'import os, stat, sys; s = os.stat(sys.argv[1]); print("%s %d:%d" % ("c" if stat.S_ISCHR(s.st_mode) else "-", os.major(s.st_rdev), os.minor(s.st_rdev)))' "$POD_DAX" ||
        command-error "no $POD_DAX in $pod"
    [ "$COMMAND_OUTPUT" == "c $SHARED_DEV" ] || error "$POD_DAX in $pod is '$COMMAND_OUTPUT', expected the character device $SHARED_DEV"
}

cdi-check() {
    # Usage: cdi-check UID READONLY
    #
    # Check the CDI device of the allocation result of claim UID,
    # claim-UID-0 (spec cxl-claims.json): the device node of the shared
    # device, with permissions r if READONLY is 1, the environment of the
    # device and the marker of the result (CXL_CLAIM_<uid>_0), and no other
    # CDI device of the claim. CDI_NODE is its device nodes.
    local uid="$1" ro="$2" cdi
    cdi=$(dra-cdi-device "$VM2" "claim-$uid-0")
    echo "CDI device claim-$uid-0: $cdi"
    [ -n "$cdi" ] || error "no CDI device claim-$uid-0"
    dra-assert "$cdi" "j['kind'] == '$DRA_LOCAL_DRIVER/device' and any(n.get('path') == '/dev/$DRA_LOCAL_SHARED_DAX' for n in j['deviceNodes'])"
    if [ "$ro" == 1 ]; then
        dra-assert "$cdi" "any(n.get('permissions') == 'r' for n in j['deviceNodes'])"
    else
        dra-assert "$cdi" "not any('permissions' in n for n in j['deviceNodes'])"
    fi
    dra-assert "$cdi" "'CXL_${DRA_LOCAL_HEX}_READ_ONLY=$ro' in j['env'] and 'CXL_CLAIM_${uid//-/_}_0=1' in j['env']"
    [ "$(dra-cdi-devices-with "$VM2" "$uid")" == "claim-$uid-0" ] ||
        error "the claim $uid has other CDI devices than claim-$uid-0: $(dra-cdi-devices-with "$VM2" "$uid")"
    CDI_NODE=$(dra-value "$cdi" "j['deviceNodes']")
}

waiting() {
    # Usage: waiting POD...
    #
    # Return 0 if every pod POD is Pending and its claim, named like the
    # pod, has no allocation.
    local p json
    for p in "$@"; do
        json=$(vmdir-command-q "$VM2" "kubectl get pod,resourceclaim -n $DRA_NS $p -o json") || return 1
        dra-py assert "$json" "sorted(o['kind'] for o in items) == ['Pod', 'ResourceClaim'] and all(o['status'].get('phase') == 'Pending' for o in items if o['kind'] == 'Pod') and not any((o.get('status') or {}).get('allocation') for o in items if o['kind'] == 'ResourceClaim')" || return 1
    done
}

unschedulable-wait() {
    # Usage: unschedulable-wait POD
    #
    # Wait until pod POD is not scheduled because its claim cannot be
    # allocated, and that its claim (named like the pod) has no
    # allocation.
    dra-unschedulable-wait "$VM2" "$1"
    waiting "$1" || error "$1 is not Pending with an unallocated claim"
}

holding() {
    # Usage: holding POD...
    #
    # Return 0 if every reader POD holds its mapping: its log has
    # "holding" and no "final:" or "changed:".
    local p log
    for p in "$@"; do
        log=$(dra-pod-log "$VM2" "$p")
        grep -qx holding <<< "$log" && ! grep -qE '^(final|changed): ' <<< "$log" || return 1
    done
}

prepared-line() {
    # Usage: prepared-line NAME READ-ONLY USES
    #
    # Print the log line of the driver of the Prepare of claim NAME of the
    # shared device, READ-ONLY true or false, USES "exclusive" or "1 share".
    echo "$DRA_LOCAL_DRIVER: claim $DRA_NS/$1: prepared $DRA_LOCAL_DEVICE (memdev $DRA_LOCAL_SERIAL, share true, mode dax, read-only $2) as $SHARED_MEMDEV $SHARED_REGION dax $SHARED_DAX size $DRA_LOCAL_SIZE NUMA node -1 $3"
}

plugin-log-has() {
    # Usage: plugin-log-has TEXT
    #
    # Fail the test unless a line of the log of the driver on VM2 has the
    # fixed string TEXT.
    dra-driver-log "$VM2" | grep -F -- "$1" ||
        error "the log of $DRA_CONTAINER on VM2 has no line with '$1'"
}

restart-kept() {
    # Usage: restart-kept CLAIM-UID...
    #
    # Check by the log of the driver pod that runs now on VM2 that, when it
    # started, it kept the region of the shared device for the claims
    # CLAIM-UID (none: idle), and did neither zero nor release it.
    local log
    dra-driver-log-wait "$VM2" "local memdev $DRA_LOCAL_SERIAL ($SHARED_MEMDEV): keeping shared $SHARED_REGION and $SHARED_DAX, claims [$*]"
    log=$(dra-driver-log "$VM2")
    ! grep -E "zeroed new shared |releasing its region|released $SHARED_REGION" <<< "$log" ||
        error "the restarted driver zeroed or released the region of the shared device, see above"
}

same-region() {
    # Usage: same-region
    #
    # Return 0 if the local device has one region, SHARED_REGION, with one
    # dax device, SHARED_DAX, in sysfs.
    dra-memdev-sysfs "$VM2" "$DRA_LOCAL_SERIAL" &&
        dra-py assert "$DRA_MEMDEV" "[(r['name'], [d['name'] for d in r['dax'] if d['size']]) for r in j['regions']] == [('$SHARED_REGION', ['$SHARED_DAX'])]"
}

lease-objects() {
    # Usage: lease-objects LEASE
    #
    # Print the companion pod and claim of lease LEASE that exist, quietly.
    vmdir-command-q "$VM2" "kubectl get pods,resourceclaims -n $DRA_NS -l $DRA_LEASE_LABEL=$1 -o name"
}

lease-companion() {
    # Usage: lease-companion LEASE
    #
    # Wait until the companion claim of lease LEASE, cxl-lease-LEASE, is
    # allocated to the local device on VM2, and check that it has no CDI
    # device: the companion pod uses nothing of it. COMPANION_JSON is the
    # claim.
    dra-wait "$VM2" "companion-$1" "resourceclaim -n $DRA_NS cxl-lease-$1" \
        "[(r['driver'], r['device']) for r in results()] == [('$DRA_LOCAL_DRIVER', '$DRA_LOCAL_DEVICE')] and allocated_node() == '$VM2_NAME'" 60
    COMPANION_JSON="$COMMAND_OUTPUT"
    [ -z "$(dra-cdi-devices-with "$VM2" "$(dra-value "$COMPANION_JSON" "j['metadata']['uid']")")" ] ||
        error "lease $1: the companion claim has CDI devices"
}

lease-release() {
    # Usage: lease-release LEASE
    #
    # Release lease LEASE of pod leaser and wait until its companion pod
    # and claim are gone.
    local lease="$1"
    dra-request "$VM2" leaser release --lease "$lease"
    dra-assert "$DRA_REQ_JSON" "j.get('lease') == '$lease' and j.get('state') == 'Released'"
    # shellcheck disable=SC2016 # evaluated by retry-until
    retry-until --timeout 60 --interval 1 --message "nothing left of lease $lease" \
        '[ -z "$(lease-objects "$lease")" ]' ||
        error "lease $lease left $(lease-objects "$lease")"
}

hold-log() {
    # Usage: hold-log
    #
    # Print /tmp/hold.log of pod leaser, the output of its read-only lease
    # (section L), quietly.
    vmdir-command-q "$VM2" "kubectl exec -n $DRA_NS leaser -- cat /tmp/hold.log" 2>/dev/null
}

no-leases() {
    # Usage: no-leases
    #
    # Return 0 if the state document of the driver ($DRA_STATE) has
    # no leases, or there is no document.
    local json
    json=$(dra-state-json "$VM2" 2>/dev/null) || json='{}'
    dra-py assert "$json" "not j.get('leases')"
}

echo "### preconditions: patched qemu, hdm_for_passthrough=on, kernel with CONFIG_FS_DAX"
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"

echo "### preconditions: cxl, daxctl and cxl-dump in the VM, no CXL memory devices in VM2 and VM1"
pool-vm-tools-install "$VM2"
pool-vm-reset "$VM2"
pool-vm-reset "$VM1"
pool-cxl-dump "$VM2" no-devices
cxl-assert 'memdevs == [] and regions == [] and endpoints == []'

echo "### preconditions: fake-cxl-pool-server on the host with shared0, stale bytes in the local device"
pool-server-start "
  - name: shared0
    size: 256Mi
    shared: true
    serial: $SHARED0_SERIAL"
pool-client-install "$VM2"
# The local device attached before the driver runs, with non-zero bytes at
# the start, at 1M and at its end, written through a region of the guest
# tools that is gone again: the first claim must find zeros there.
dra-local-device "$VM2"
SEED_OFFSETS=(0 1M $(( DRA_LOCAL_SIZE - 4096 )))
dra-local-seed "$VM2" "$STALE" "${SEED_OFFSETS[@]}"

echo "### preconditions: the driver with the local device shared"
DRA_DRIVER_CONFIG="sharedMemdevs: [\"$DRA_LOCAL_SERIAL\"]"
# Never pool-cleanup while a region of the shared device exists.
DRA_EXIT_TRAP='dra-local-shared-cleanup "$VM2"'
dra-build
dra-install
dra-pull-images "$VM2" "$DRA_TOOLS_IMAGE" "$DRA_COMPANION_IMAGE"
dra-tools-install "$VM2"
dra-trace-start "$VM2"

echo "### A: the local device is published whole, as a shared device"
dra-local-device "$VM2"
LD="$DRA_LOCAL_LD"             # the device in the ResourceSlices (dra-py)
pool-cxl-dump "$VM2" local-attached
cxl-assert "by_serial($DRA_LOCAL_SERIAL) is not None and enabled(by_serial($DRA_LOCAL_SERIAL)) and by_serial($DRA_LOCAL_SERIAL)['ramSize'] == $DRA_LOCAL_SIZE"
# The first claim creates the region.
cxl-assert "regions == []"
dra-kubectl "$VM2" "get resourceslices"
dra-json "$VM2" resourceslices
SLICES="$COMMAND_OUTPUT"
dra-assert "$SLICES" "attr($LD, 'type') == 'memdev' and attr($LD, 'source') == 'local' and attr($LD, 'shared') == True"
dra-assert "$SLICES" "attr($LD, 'serial') == '$DRA_LOCAL_SERIAL' and attr($LD, 'size') == $DRA_LOCAL_SIZE"
# No memoryBlockSize: a shared device is never sliced.
dra-assert "$SLICES" "sorted(k.split('/')[-1] for k in ${LD}['attributes']) == ['numaNode', 'persistent', 'serial', 'shared', 'size', 'source', 'type'] and attr($LD, 'persistent') == False"
# One capacity, shares, without a requestPolicy: no memory to consume,
# and nothing to put online.
dra-assert "$SLICES" "sorted(${LD}.get('capacity') or {}) == ['shares'] and quantity(capacity($LD, 'shares')) == $DRA_SHARES and request_policy($LD, 'shares') is None"
dra-assert "$SLICES" "${LD}.get('allowMultipleAllocations') == True and not ${LD}.get('nodeAllocatableResources')"
dra-json "$VM2" deviceclasses
dra-assert "$COMMAND_OUTPUT" "sorted(n for n in names() if n.startswith('cxl-')) == sorted([$(printf "'%s', " "${DRA_CLASSES[@]}")])"
dra-assert "$COMMAND_OUTPUT" "all(t in by_name('$SHARED_CLASS')['spec']['selectors'][0]['cel']['expression'] for t in ['\"$DRA_LOCAL_DRIVER\"', '\"memdev\"', 'shared == true'])"
dra-assert "$COMMAND_OUTPUT" "'shared == false' in by_name('cxl-local')['spec']['selectors'][0]['cel']['expression']"
# A claim for a slice of local memory, without an amount, does not match
# the shared device: the class cxl-local has only devices that are not
# shared.
dra-claim-pod "$VM2" sliced cxl-local "" dax
dra-unschedulable-wait "$VM2" sliced
dra-delete-pod "$VM2" sliced
pool-cxl-dump "$VM2" sliced-refused
cxl-assert "regions == []"

echo "### B: the writer claims the device exclusively and writes"
# No capacity request: the claim consumes all shares. No MemoryConfig:
# mode dax, the only mode of a shared device, read-write.
claim-pod writer "" "" "env | grep ^CXL_ | sort"
dra-pod-wait "$VM2" writer Running 120
claim-check writer "$DRA_SHARES"
WRITER_UID="$CLAIM_UID"
dra-assert "$CLAIM_JSON" "configs() == [] and [s.get('capacity') for s in subrequests()] == [None]"
# The claim created the region: one dax device of all of the device,
# nothing online.
dra-local-shared-wait "$VM2" writer "$WRITER_UID"
SHARED_REGION="$DRA_LOCAL_SHARED_REGION"
SHARED_DAX="$DRA_LOCAL_SHARED_DAX"
SHARED_MEMDEV="$DRA_LOCAL_SHARED_MEMDEV"
SHARED_DEV=$(dra-value "$DRA_MEMDEV" "dax_slices()[0]['dev']")
env-check writer 0
cdi-check "$WRITER_UID" 0
plugin-log-has "$(prepared-line writer false exclusive)"
# The new region was zeroed once: where dra-local-seed left stale bytes,
# the writer finds zeros.
plugin-log-has "zeroed new shared $SHARED_DAX size $DRA_LOCAL_SIZE in "
for off in "${SEED_OFFSETS[@]}"; do
    dra-exec "$VM2" writer python3 /tools/dax-rw.py read "$POD_DAX" "$off" || command-error "the writer cannot read at $off"
    [ -z "$COMMAND_OUTPUT" ] || error "the writer found '$COMMAND_OUTPUT' at $off of the new region, expected zeros"
done
dra-local-unchanged "$VM2"
dra-exec "$VM2" writer python3 /tools/dax-rw.py write "$POD_DAX" 0 "$TEXT0" ||
    command-error "the writer cannot write at 0"
dra-exec "$VM2" writer python3 /tools/dax-rw.py write "$POD_DAX" 1M "$TEXT1M" ||
    command-error "the writer cannot write at 1M"
dra-exec "$VM2" writer python3 /tools/dax-rw.py read "$POD_DAX" 0 || command-error "the writer cannot read"
[ "$COMMAND_OUTPUT" == "$TEXT0" ] || error "the writer read '$COMMAND_OUTPUT' at 0, expected '$TEXT0'"

echo "### C: two readers ask for a share while the writer has the device: they wait"
for r in reader-a reader-b; do
    claim-pod "$r" 1 "readOnly: true" "$(reader-script)"
done
for r in reader-a reader-b; do
    unschedulable-wait "$r"
done
dra-stays 10 "reader-a and reader-b stay Pending, their claims unallocated" 'waiting reader-a reader-b'

echo "### D: the writer goes, both readers get the device and read what it wrote"
dra-claim-pod-delete writer
for r in reader-a reader-b; do
    dra-pod-wait "$VM2" "$r" Running 60
done
declare -A READER_UID=()
for r in reader-a reader-b; do
    claim-check "$r" 1
    READER_UID[$r]="$CLAIM_UID"
    dra-assert "$CLAIM_JSON" "[c['parameters'] for c in configs()] == [{'apiVersion': '$DRA_CONFIG_API', 'kind': '$DRA_CONFIG_KIND', 'device': {'readOnly': True}}]"
    dra-wait-log "$VM2" "$r" "read: $TEXT0" 60
    dra-wait-log "$VM2" "$r" holding 30
    for line in "open for writing refused: EPERM" "writable mapping refused: EACCES"; do
        dra-pod-log "$VM2" "$r" | grep -qxF "$line" || error "$r did not log '$line'"
    done
    env-check "$r" 1
    cdi-check "$CLAIM_UID" 1
    plugin-log-has "$(prepared-line "$r" true "1 share")"
done
# The same device node in both, the region of the writer.
dra-local-shared-wait "$VM2" readers "${READER_UID[reader-a]}" "${READER_UID[reader-b]}"
same-region || error "the readers have another region or dax device than the writer: $DRA_MEMDEV"
# A third reader joins while the two hold the device.
claim-pod reader-c 1 "readOnly: true" "$(reader-script)"
dra-pod-wait "$VM2" reader-c Running 60
claim-check reader-c 1
READER_UID[reader-c]="$CLAIM_UID"
dra-wait-log "$VM2" reader-c "read: $TEXT0" 60
dra-wait-log "$VM2" reader-c holding 30
env-check reader-c 1
dra-local-shared-wait "$VM2" three-readers "${READER_UID[@]}"
same-region || error "reader-c has another region or dax device: $DRA_MEMDEV"
# An exclusive claim cannot take the device from the readers: it waits.
claim-pod late-writer "" "" "echo started"
unschedulable-wait late-writer
dra-stays 10 "late-writer stays Pending while the readers hold the device" 'waiting late-writer && holding reader-a reader-b reader-c'
dra-claim-pod-delete late-writer

echo "### E: two pods of one claim are one share"
claim-pod two-1 1 "readOnly: true" "echo \"read: \$(python3 /tools/dax-rw.py read \"\$CXL_${DRA_LOCAL_HEX}_DAX\" 0)\""
dra-apply "$VM2" <<< "$(dra-script-pod-yaml two-2 "$VM2" two-1 "echo \"read: \$(python3 /tools/dax-rw.py read \"\$CXL_${DRA_LOCAL_HEX}_DAX\" 0)\"")"
for p in two-1 two-2; do
    dra-pod-wait "$VM2" "$p" Running 60
    dra-wait-log "$VM2" "$p" "read: $TEXT0" 60
done
claim-check two-1 1
TWO_UID="$CLAIM_UID"
dra-assert "$CLAIM_JSON" "sorted(r['name'] for r in j['status']['reservedFor']) == ['two-1', 'two-2']"
dra-local-shared-wait "$VM2" two-pods "${READER_UID[@]}" "$TWO_UID"
same-region || error "two-1 and two-2 have another region or dax device: $DRA_MEMDEV"

echo "### F: configurations and requests that the driver refuses"
# The scheduler allocates the device to these claims, next to the
# readers; the driver refuses to prepare them, the pods stay
# ContainerCreating. A shared device is dax only, readOnly needs mode dax
# (checked first), and access is of mode fs.
claim-pod bad-numa 1 "mode: numa" "echo started"
claim-pod bad-numa-ro 1 "mode: numa, readOnly: true" "echo started"
claim-pod bad-access 1 "access: cached" "echo started"
# shares "0" passes the API and fits next to any holder: the scheduler
# allocates it, consuming nothing, and the driver refuses it.
claim-pod zero-shares 0 "readOnly: true" "echo started"
dra-pod-event-wait bad-numa "MemoryConfig: device: a shared device is used as dax only"
dra-pod-event-wait bad-numa-ro "MemoryConfig: device.readOnly needs mode dax"
dra-pod-event-wait bad-access "MemoryConfig: device.access needs mode fs"
dra-pod-event-wait zero-shares "memdev $DRA_LOCAL_SERIAL: consumed shares \"0\" is less than 1: request shares: 1 (a share) or no shares (exclusive)"
claim-check zero-shares 0
for p in bad-numa bad-numa-ro bad-access zero-shares; do
    dra-wait "$VM2" "$p-creating" "pod -n $DRA_NS $p" \
        "[(cs.get('state') or {}).get('waiting', {}).get('reason') for cs in j['status'].get('containerStatuses') or []] == ['ContainerCreating']" 60
done
holding reader-a reader-b reader-c || error "a reader lost the device to a refused claim"
dra-claim-pod-delete bad-numa bad-numa-ro bad-access zero-shares
dra-local-shared-wait "$VM2" refused-gone "${READER_UID[@]}" "$TWO_UID"
same-region || error "the refused claims changed the region: $DRA_MEMDEV"

echo "### G: all claims go, the region and the bytes stay"
for r in reader-a reader-b reader-c; do
    dra-exec "$VM2" "$r" touch /tmp/stop || command-error "cannot stop $r"
    dra-wait-log "$VM2" "$r" "final: $TEXT0" 30
done
# two-2 first: the claim two-1 goes when no pod has it any more.
dra-delete-pod "$VM2" two-2
dra-claim-pod-delete reader-a reader-b reader-c two-1
dra-wait "$VM2" no-claims "resourceclaims -n $DRA_NS" "items == []" 60
# Kept while the device is configured shared: the region, its dax device,
# the memdev enabled, the device published as before.
dra-local-shared-wait "$VM2" idle
same-region || error "the idle shared device has another region or dax device: $DRA_MEMDEV"
dra-local-unchanged "$VM2"
[ -z "$(dra-cdi-devices-with "$VM2" "CXL_${DRA_LOCAL_HEX}_")" ] ||
    error "CDI devices of claims of the local device are left: $(dra-cdi-devices-with "$VM2" "CXL_${DRA_LOCAL_HEX}_")"
# A new exclusive claim finds what the writer wrote: the kept region was
# not zeroed again, neither between the claims nor while idle.
claim-pod after-idle "" "" "echo \"at 0: \$(python3 /tools/dax-rw.py read \"\$CXL_${DRA_LOCAL_HEX}_DAX\" 0)\"
echo \"at 1M: \$(python3 /tools/dax-rw.py read \"\$CXL_${DRA_LOCAL_HEX}_DAX\" 1M)\""
dra-pod-wait "$VM2" after-idle Running 60
claim-check after-idle "$DRA_SHARES"
dra-wait-log "$VM2" after-idle "at 0: $TEXT0" 60
dra-wait-log "$VM2" after-idle "at 1M: $TEXT1M" 10
same-region || error "after-idle has another region or dax device: $DRA_MEMDEV"
zeroed=$(dra-driver-log "$VM2" | grep -c "zeroed new shared ")
[ "$zeroed" == 1 ] || error "the driver zeroed the shared device $zeroed times, expected once, when the region was created"
dra-claim-pod-delete after-idle

echo "### L: leases of the shared device for a running container"
dra-apply "$VM2" <<< "$(dra-lease-template-yaml)"
dra-lease-pod-start "$VM2" leaser "$VM2_NAME"
LEASER_CLAIM_UID="$DRA_LEASE_CLAIM_UID"
# Without --share a lease takes the device exclusively, as the writer's
# claim did, and reads what the writer left in the kept region.
dra-request "$VM2" leaser request --serial "$DRA_LOCAL_SERIAL" --wait 60s
[ "$DRA_REQ_STATUS" == 0 ] || error "cxl-request request --serial $DRA_LOCAL_SERIAL exited $DRA_REQ_STATUS"
dra-assert "$DRA_REQ_JSON" "(j.get('state'), j.get('source'), j.get('mode'), bool(j.get('share')), bool(j.get('readOnly'))) == ('Ready', 'local', 'dax', False, False)"
LEASE=$(dra-value "$DRA_REQ_JSON" "j['lease']")
dra-exec "$VM2" leaser "$DRA_REQUEST_CTR_BIN" env --lease "$LEASE" --exec python3 /tools/dax-rw.py --fd 3 read 0 ||
    command-error "cannot read through the fd of the exclusive lease $LEASE"
[ "$(tail -n 1 <<< "$COMMAND_OUTPUT")" == "$TEXT0" ] ||
    error "the exclusive lease read '$(tail -n 1 <<< "$COMMAND_OUTPUT")', expected '$TEXT0'"
# shared is what the lease asked for; the device is shared all the same.
dra-leases "$VM2" leaser
dra-assert "$DRA_LEASES" "[(l['lease'], bool(l.get('share')), (l.get('env') or {}).get('CXL_${DRA_LOCAL_HEX}_SHARED')) for l in j] == [('$LEASE', False, '1')]"
lease-companion "$LEASE"
dra-assert "$COMPANION_JSON" "[(s['deviceClassName'], s.get('capacity')) for s in subrequests()] == [('$SHARED_CLASS', None)] and quantity((results()[0].get('consumedCapacity') or {}).get('shares', 0)) == $DRA_SHARES"
same-region || error "the exclusive lease has another region or dax device: $DRA_MEMDEV"
lease-release "$LEASE"
# One share by size: the companion request has one share and a size
# selector.
dra-request "$VM2" leaser request --source local --share --size 64Mi --wait 60s
[ "$DRA_REQ_STATUS" == 0 ] || error "cxl-request request --source local --share --size 64Mi exited $DRA_REQ_STATUS"
dra-assert "$DRA_REQ_JSON" "(j.get('state'), j.get('source'), j.get('serial'), j.get('mode'), bool(j.get('share'))) == ('Ready', 'local', '$DRA_LOCAL_SERIAL', 'dax', True)"
LEASE=$(dra-value "$DRA_REQ_JSON" "j['lease']")
lease-companion "$LEASE"
dra-assert "$COMPANION_JSON" "[(s['name'], s['deviceClassName'], s.get('capacity')) for s in subrequests()] == [('lease', '$SHARED_CLASS', {'requests': {'shares': '1'}})]"
dra-assert "$COMPANION_JSON" "any('size' in (sel.get('cel') or {}).get('expression', '') for sel in subrequests()[0].get('selectors') or [])"
lease-release "$LEASE"
# --share without --source: the local device first, then the pool
# (shared0 would do), one share of either in a firstAvailable request.
dra-request "$VM2" leaser request --share --size 64Mi --wait 60s
[ "$DRA_REQ_STATUS" == 0 ] || error "cxl-request request --share --size 64Mi exited $DRA_REQ_STATUS"
dra-assert "$DRA_REQ_JSON" "(j.get('state'), j.get('source'), j.get('serial')) == ('Ready', 'local', '$DRA_LOCAL_SERIAL')"
LEASE=$(dra-value "$DRA_REQ_JSON" "j['lease']")
lease-companion "$LEASE"
dra-assert "$COMPANION_JSON" "[(s['name'], s['deviceClassName'], s.get('capacity')) for s in subrequests()] == [('lease/local', '$SHARED_CLASS', {'requests': {'shares': '1'}}), ('lease/pool', 'cxl-pool-shared', {'requests': {'shares': '1'}})]"
dra-assert "$COMPANION_JSON" "[r['request'] for r in results()] == ['lease/local']"
lease-release "$LEASE"
[ -z "$(dra-pool-attachments "$VM2_NAME")" ] ||
    error "the pool attached devices for local leases: $(dra-pool-attachments "$VM2_NAME")"
# The local device is no device of the pool.
dra-request "$VM2" leaser request --serial "$DRA_LOCAL_SERIAL" --source pool
[ "$DRA_REQ_STATUS" == 1 ] || error "cxl-request request --serial $DRA_LOCAL_SERIAL --source pool exited $DRA_REQ_STATUS, expected 1"
grep -qF "memdev $DRA_LOCAL_SERIAL: not a pool device" <<< "$DRA_REQ_OUT" ||
    error "cxl-request request --serial $DRA_LOCAL_SERIAL --source pool did not print 'not a pool device': $DRA_REQ_OUT"
# A read-only share, mapped when its pod goes: the broker ends the lease,
# its companion and the lease directories of the pod go, the driver
# has no leases left, and the region stays.
# shellcheck disable=SC2016 # expanded by sh in the container
dra-exec "$VM2" leaser sh -c 'setsid sh -c "$1 request --serial $2 --share --read-only --wait 60s --exec sh -c \"python3 /tools/dax-rw.py --fd 3 check-ro 0; exec python3 /tools/dax-rw.py --fd 3 watch 0 /tmp/stop\"" > /tmp/hold.log 2>&1 < /dev/null &' sh "$DRA_REQUEST_CTR_BIN" "$DRA_LOCAL_SERIAL" ||
    command-error "cannot start a read-only lease in leaser"
# shellcheck disable=SC2016 # evaluated by retry-until
retry-until --timeout 60 --interval 1 --message "leaser holds a read-only share" \
    'grep -qx holding <<< "$(hold-log)"' ||
    error "leaser does not hold a read-only share: $(hold-log)"
log=$(hold-log)
echo "$log"
grep -qxF "read: $TEXT0" <<< "$log" || error "the read-only lease did not read '$TEXT0'"
grep -qxF "writable mapping refused: EACCES" <<< "$log" || error "the read-only lease could map the memory for writing"
dra-leases "$VM2" leaser
dra-assert "$DRA_LEASES" "[(l['state'], bool(l.get('share')), bool(l.get('readOnly'))) for l in j] == [('Ready', True, True)]"
dra-delete-pod "$VM2" leaser
dra-delete "$VM2" resourceclaimtemplate cxl-lease --wait=true --timeout=60s
dra-wait "$VM2" no-lease-objects "pods,resourceclaims -n $DRA_NS" "items == []" 60
retry-until --timeout 60 --interval 1 --message "no leases in $DRA_STATE" no-leases ||
    error "the driver still has leases in $DRA_STATE"
vmdir-command "$VM2" "test ! -e $DRA_LEASE_DIR/$LEASER_CLAIM_UID && test ! -e $DRA_LEASE_FS_DIR/$LEASER_CLAIM_UID" ||
    error "the lease directories of the claim of leaser are left in $DRA_LEASE_DIR or $DRA_LEASE_FS_DIR"
dra-local-shared-wait "$VM2" leases-gone
same-region || error "the shared device has another region or dax device after the leases: $DRA_MEMDEV"

echo "### H: restarts of the driver keep the region; without the configuration it goes"
claim-pod holder 1 "readOnly: true" "$(reader-script)"
dra-pod-wait "$VM2" holder Running 60
dra-wait-log "$VM2" holder holding 60
claim-check holder 1
HOLDER_UID="$CLAIM_UID"
cdi-check "$HOLDER_UID" 1
CDI_BEFORE="$CDI_NODE"
dra-driver-restart "$VM2"
# The new driver kept the region for the holder's claim (its log
# says so), the reader keeps its mapping, and the CDI device of the claim
# has the same device node.
restart-kept "$HOLDER_UID"
holding holder || error "holder lost its mapping with the restart: $(dra-pod-log "$VM2" holder)"
same-region || error "the region changed with the restart: $DRA_MEMDEV"
dra-local-shared-wait "$VM2" restarted "$HOLDER_UID"
cdi-check "$HOLDER_UID" 1
[ "$CDI_NODE" == "$CDI_BEFORE" ] || error "the device node of the claim of holder changed with the restart: '$CDI_BEFORE' -> '$CDI_NODE'"
dra-exec "$VM2" holder python3 /tools/dax-rw.py read "$POD_DAX" 0 || command-error "holder cannot read after the restart"
[ "$COMMAND_OUTPUT" == "$TEXT0" ] || error "holder read '$COMMAND_OUTPUT' after the restart, expected '$TEXT0'"
dra-exec "$VM2" holder touch /tmp/stop || command-error "cannot stop holder"
dra-wait-log "$VM2" holder "final: $TEXT0" 30
dra-claim-pod-delete holder
dra-local-shared-wait "$VM2" holder-gone
# No claims, still configured shared: a restart keeps the region.
dra-driver-restart "$VM2"
restart-kept
same-region || error "the region changed with the restart: $DRA_MEMDEV"
dra-local-shared-wait "$VM2" restarted-idle
# Without the device in the configuration the driver destroys the
# region of the idle device and publishes the device for slices again
# (as test14).
DRA_DRIVER_CONFIG=""
dra-install
dra-trace-driver-follow "$VM2"
# The driver itself released the region: its log says so.
dra-driver-log-wait "$VM2" "local memdev $DRA_LOCAL_SERIAL ($SHARED_MEMDEV): not in sharedMemdevs any more, destroying its region" 60
dra-local-slices-none "$VM2" unshared
dra-wait "$VM2" sliced-again resourceslices \
    "attr($LD, 'shared') == False and sorted(k.split('/')[-1] for k in ${LD}['attributes']) == ['memoryBlockSize', 'numaNode', 'persistent', 'serial', 'shared', 'size', 'source', 'type'] and quantity(capacity($LD, 'memory')) == $DRA_LOCAL_SIZE" 60

# The templates of the claims that went with their pods (dra-claim-pod)
# and of the lease claims.
dra-delete "$VM2" resourceclaimtemplates --all --wait=true --timeout=60s
dra-assert-clean "$VM2" "$VM1"

echo "### I: what every component did, in order"
dra-trace-stop
dra-trace-summary

echo "### cleanup"
dra-cleanup
dra-local-detach "$VM2"
pool-cleanup
trap - EXIT
pool-port-busy && error "port $POOL_PORT is still busy after stopping the server"
echo "trace: $TEST_OUTPUT_DIR/trace.txt, summary: $TEST_OUTPUT_DIR/trace-summary.txt"
