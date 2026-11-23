# Lending node-local CXL memory to a running container
#
# test12 lends pool memory to a running container: "cxl-request request
# --source pool --size 512Mi" in a container whose pod claims the lease
# device "lease" creates a lease, a companion pod with a claim of a pool
# device, and the driver lends the NUMA node of the device to the
# container. test14 gives pods NUMA memory and dax slices of a local CXL
# memory device through their claims. Here a running container leases
# local memory and gives it back, while it keeps running, next to pods
# that have static claims of the same device:
# - "cxl-request request --source local --size 128Mi": the companion
#   claim asks for 128Mi of DeviceClass cxl-local, with a MemoryConfig of
#   device.mode numa. Preparing it, the driver carves a slice of the
#   local device, onlines its memory blocks on the NUMA node of the device,
#   and lends the node to the container (cpuset.mems, through NRI). Nothing
#   is attached and there is no binding condition to wait for: a local lease
#   is Ready sooner than a pool lease. Containers that neither claim the
#   node nor lease it never have it, whatever their cpuset.mems.
# - "cxl-request request --source local --size 64Mi --mode dax --exec
#   CMD": the MemoryConfig of the claim says mode dax, the lease gets a
#   devdax slice of the device of its own, passed to CMD as fd 3
#   (CXL_<KEY>_FD=3); "cxl-request env --lease ID" names the device.
# - Without --source: local memory if a local device has room, else
#   memory of the pool (the companion claim has firstAvailable: local,
#   pool).
# Every slice is whole memory blocks: a request is rounded up to them.
# NUMA and dax slices, of leases and of static claims, share the device.
# The NUMA node grows by the blocks of every NUMA slice and shrinks by
# them when it goes. Pods with static claims and leases share the node; a
# container keeps it lent while any of its leases holds it. Steering is
# opt-in: a container whose static NUMA claim does not opt in is not
# steered, only fenced, and may lease more of the node.
#
# What you will see: a pod with a static NUMA claim, then the requester's
# NUMA lease on the same node, the node growing by exactly its blocks,
# with pages on it; two dax leases with slices of their own next to the
# NUMA slices; a second static pod and a second NUMA lease, and the leases
# given back one by one: the node stays lent while the second lease holds
# it, then the pages move back to DRAM, while the static pods keep the
# node; the device back in its initial state; a container with a static
# NUMA claim, not steered, taking a NUMA lease; a lease falling back to the
# pool when the local device is full; and the pod deleted with leases
# active, leaving nothing behind. At the end the trace summary of what
# every component did, in order.
#
# The local device is cxl_memdev0 of the topology (1G, 8 memory blocks of
# 128M in the VM), attached through fake-cxl-pool-server as in test14.
# Sizes are derived from the block size that the VM reports. The pods run
# on this VM (VM2), the control plane of the cluster of the test, the
# companions follow their requester; VM1 (n4-cxl-shared-1) and VM0
# (n4-cxl-shared-0), its workers, have no local device attached. Run
# test00-up of n4-cxl-shared-1, -0 and -2 first: they create the VMs and
# make them one cluster. The DRA driver comes from
# its image, which has cxl-request too, build it first:
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
POOLED0_SERIAL=0xc1ee0001   # 0xc1ee....: unshared pool devices
POOLED0_SIZE=$(( 512 << 20 ))
MiB=$(( 1 << 20 ))
LOCAL_CLASS=cxl-local
TAG_BASE="dra-local-live-attach $(date +%Y-%m-%dT%H:%M:%S.%N)"
declare -A NUMA_SLICES=()   # NUMA claims of the local device: LABEL -> "daxX.Y BYTES" of its slice

numa-lease() {
    # Usage: numa-lease SIZE [SOURCE]
    #
    # Container main of the requester asks for SIZE (a quantity) of NUMA
    # memory from SOURCE, or without a SOURCE local memory first, then the
    # pool ("cxl-request request --size SIZE [--source SOURCE]"). Fail
    # unless the lease is Ready, exclusive and read-write NUMA memory (the
    # defaults), with the environment of its NUMA node in the response.
    # LEASE is its id, LEASE_NODE its NUMA node.
    local size="$1" source="$2"
    dra-request "$VM2" requester/main request --size "$size" ${source:+--source "$source"} --wait 120s
    [ "$DRA_REQ_STATUS" == 0 ] || error "cxl-request request --size $size${source:+ --source $source} exited $DRA_REQ_STATUS"
    dra-assert "$DRA_REQ_JSON" "j.get('state') == 'Ready' and j.get('mode') == 'numa' and not j.get('share') and not j.get('readOnly') and int(j['numaNode']) >= $CPU_NODES"
    dra-assert "$DRA_REQ_JSON" "j['env']['CXL_%s_NUMA_NODE' % j['serial'][2:].upper()] == str(j['numaNode']) and j['env']['CXL_%s_MODE' % j['serial'][2:].upper()] == 'numa'"
    LEASE=$(dra-value "$DRA_REQ_JSON" "j['lease']")
    LEASE_NODE=$(dra-value "$DRA_REQ_JSON" "j['numaNode']")
    dra-lease-new "$LEASE"
    echo "lease $LEASE: $size from ${source:-local, then pool}, NUMA node $LEASE_NODE, Ready"
}

local-numa-lease() {
    # Usage: local-numa-lease BYTES
    #
    # Container main of the requester asks for BYTES of local NUMA memory.
    # Check that the lease is Ready on the local device and the NUMA node
    # of the device, with a slice of BYTES rounded up to blocks and its
    # environment, and its companion objects (see companion-check). LEASE:
    # see numa-lease.
    numa-lease "$(dra-mi "$1")" local
    dra-assert "$DRA_REQ_JSON" "j.get('source') == 'local' and j.get('serial') == '$DRA_LOCAL_SERIAL' and int(j['numaNode']) == $NUMA_NODE and int(j['size']) == $(dra-local-round "$1")"
    dra-assert "$DRA_REQ_JSON" "j['env'] == {'CXL_${DRA_LOCAL_HEX}_SERIAL': '$DRA_LOCAL_SERIAL', 'CXL_${DRA_LOCAL_HEX}_SHARED': '0', 'CXL_${DRA_LOCAL_HEX}_MODE': 'numa', 'CXL_${DRA_LOCAL_HEX}_SIZE': '$(dra-local-round "$1")', 'CXL_${DRA_LOCAL_HEX}_NUMA_NODE': '$NUMA_NODE'}"
    companion-check "$LEASE" "$LOCAL_CLASS" "$1" "$(dra-local-round "$1")" numa
}

dax-lease() {
    # Usage: dax-lease SIZE FLAGS SCRIPT [ARG...]
    #
    # Container main of the requester asks for a dax lease of SIZE
    # ("cxl-request request --size SIZE --mode dax FLAGS --exec") and runs
    # the shell SCRIPT with ARGs as $1... and the dax fd as fd 3. Check that
    # the environment of SCRIPT has the lease and the variables of one
    # device with FD=3 instead of DAX, and that "cxl-request env --lease"
    # names the dax device. DAX_LEASE, DAX_KEY, DAX_PATH, DAX_SIZE and
    # DAX_RO are the lease, the key of its device in the environment, its
    # dax device, size and read-only flag, DAX_OUT is what SCRIPT printed.
    local size="$1" flags="$2" script="$3" exec_env serial
    shift 3
    # shellcheck disable=SC2086 # FLAGS are separate words
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" request --size "$size" --mode dax $flags --wait 60s --exec \
        sh -c 'env | grep "^CXL_" | sort | sed "s/^/exec-env: /"; '"$script" sh "$@" ||
        command-error "cxl-request request --size $size --mode dax $flags --exec failed"
    exec_env=$(sed -n 's/^exec-env: //p' <<< "$COMMAND_OUTPUT")
    DAX_OUT=$(grep -v '^exec-env: ' <<< "$COMMAND_OUTPUT")
    DAX_LEASE=$(dra-env-value "$exec_env" CXL_LEASE)
    DAX_KEY=$(sed -n 's/^CXL_\(.*\)_FD=3$/\1/p' <<< "$exec_env")
    [[ "$DAX_LEASE" =~ ^[0-9a-f]{8}$ && "$DAX_KEY" =~ ^[0-9A-F]+$ ]] ||
        error "cxl-request --exec: no CXL_LEASE and one CXL_<KEY>_FD=3 in the environment: $exec_env"
    DAX_SIZE=$(dra-env-value "$exec_env" "CXL_${DAX_KEY}_SIZE")
    DAX_RO=$(dra-env-value "$exec_env" "CXL_${DAX_KEY}_READ_ONLY")
    serial=$(dra-env-value "$exec_env" "CXL_${DAX_KEY}_SERIAL")
    dra-env-assert "--exec of lease $DAX_LEASE" "$exec_env" "$DAX_KEY" \
        SERIAL="0x${DAX_KEY,,}" SHARED=0 MODE=dax SIZE="$DAX_SIZE" FD=3 READ_ONLY="$DAX_RO"
    dra-lease-new "$DAX_LEASE"
    # cxl-request env prints the same environment with the device, as a
    # claim gives it to its containers.
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" env --lease "$DAX_LEASE" ||
        command-error "cxl-request env --lease $DAX_LEASE failed"
    dra-env-assert "env --lease $DAX_LEASE" "$COMMAND_OUTPUT" "$DAX_KEY" \
        SERIAL="$serial" SHARED=0 MODE=dax SIZE="$DAX_SIZE" "DAX=/dev/dax*" READ_ONLY="$DAX_RO"
    DAX_PATH=$(dra-env-value "$COMMAND_OUTPUT" "CXL_${DAX_KEY}_DAX")
    echo "lease $DAX_LEASE: $DAX_PATH of $serial, $DAX_SIZE bytes, read-only $DAX_RO"
}

dax-fd-read() {
    # Usage: dax-fd-read LEASE EXPECTED
    #
    # Read the string at offset 1M of the dax fd of lease LEASE again
    # ("cxl-request env --lease LEASE --exec"), and fail unless it is
    # EXPECTED.
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" env --lease "$1" --exec python3 /tools/dax-rw.py --fd 3 read 1M ||
        command-error "cxl-request env --lease $1 --exec ... read failed"
    [ "$(tail -n 1 <<< "$COMMAND_OUTPUT")" == "$2" ] || error "lease $1: read '$(tail -n 1 <<< "$COMMAND_OUTPUT")', expected '$2'"
}

release() {
    # Usage: release LEASE
    #
    # Container main of the requester gives lease LEASE back.
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" release --lease "$1" ||
        command-error "cxl-request release --lease $1 failed"
    dra-assert "$(dra-last-json "$COMMAND_OUTPUT")" "j.get('lease') == '$1' and j.get('state') == 'Released'"
}

companion-check() {
    # Usage: companion-check LEASE CLASS REQUESTED CONSUMED MODE [ro]
    #
    # The objects of lease LEASE of local memory: the companion pod and its
    # claim cxl-lease-LEASE, both owned by the requester, labeled and
    # annotated with the lease and its requester. The claim asks for
    # REQUESTED bytes of DeviceClass CLASS, is allocated to the local
    # device on this node, consumes CONSUMED bytes of it, and tells the
    # driver cxl.nri.io how it prepares the device: one
    # MemoryConfig with a device section only, of MODE, read-only with ro.
    # The pool attaches nothing
    # for it. COMPANION_UID is the uid of the claim.
    local lease="$1" class="$2" size="$3" consumed="$4" mode="$5" ro=False name="cxl-lease-$1" pod_json claim_json uid
    [ "$6" != ro ] || ro=True
    dra-pod-wait "$VM2" "$name" Running 60
    dra-json "$VM2" pod -n "$DRA_NS" "$name"
    pod_json="$COMMAND_OUTPUT"
    dra-assert "$pod_json" "[(o['kind'], o['name'], o['uid']) for o in j['metadata']['ownerReferences']] == [('Pod', 'requester', '$REQUESTER_UID')]"
    dra-companion-meta-check "$pod_json" "$lease" "$REQUESTER_UID" "$VM2_NAME" "$DRA_NS/requester/main"
    dra-assert "$pod_json" "j['spec']['nodeName'] == '$VM2_NAME' and j['spec']['resourceClaims'] == [{'name': 'lease', 'resourceClaimName': '$name'}]"
    dra-assert "$pod_json" "[c['image'] for c in j['spec']['containers']] == ['$DRA_COMPANION_IMAGE'] and not j['spec']['containers'][0]['resources'].get('claims')"
    dra-json "$VM2" resourceclaim -n "$DRA_NS" "$name"
    claim_json="$COMMAND_OUTPUT"
    dra-assert "$claim_json" "[(o['kind'], o['name'], o['uid'], o.get('controller')) for o in j['metadata']['ownerReferences']] == [('Pod', 'requester', '$REQUESTER_UID', True)]"
    dra-companion-meta-check "$claim_json" "$lease" "$REQUESTER_UID" "$VM2_NAME" "$DRA_NS/requester/main"
    dra-assert "$claim_json" "len(j['spec']['devices']['requests']) == 1 and j['spec']['devices']['requests'][0]['exactly']['deviceClassName'] == '$class'"
    dra-assert "$claim_json" "quantity(j['spec']['devices']['requests'][0]['exactly']['capacity']['requests']['memory']) == $size"
    dra-assert "$claim_json" "[(r['driver'], r['device']) for r in results()] == [('$DRA_LOCAL_DRIVER', '$DRA_LOCAL_DEVICE')] and allocated_node() == '$VM2_NAME' and consumed() == $consumed"
    dra-assert "$claim_json" "[r['name'] for r in j['status']['reservedFor']] == ['$name']"
    # The opaque config for the driver: how it prepares the device.
    dra-assert "$claim_json" "[(c['driver'], c['parameters'].get('apiVersion'), c['parameters'].get('kind'), sorted(set(c['parameters']) - {'apiVersion', 'kind'}), config_section(c, 'device').get('mode'), bool(config_section(c, 'device').get('readOnly'))) for c in configs()] == [('$DRA_LOCAL_DRIVER', '$DRA_CONFIG_API', '$DRA_CONFIG_KIND', ['device'], '$mode', $ro)]"
    uid=$(dra-value "$claim_json" "j['metadata']['uid']")
    COMPANION_UID="$uid"
    if dra-pool-attachments "$VM2_NAME" | grep -qF "$uid"; then
        error "lease $lease: the pool attached a device for the claim of a local lease"
    fi
    # The companion pod uses nothing of its claim: no CDI device.
    [ -z "$(dra-cdi-devices-with "$VM2" "$uid")" ] ||
        error "lease $lease: the companion claim has CDI devices: $(dra-cdi-devices-with "$VM2" "$uid")"
}

numa-slice() {
    # Usage: numa-slice LABEL CLAIM-UID BYTES
    #
    # Record in NUMA_SLICES the slice of the NUMA claim CLAIM-UID (LABEL:
    # its pod or lease), as the state of the driver names it, and
    # check that sysfs has it as a kmem slice of BYTES.
    local dax
    dax=$(claim-slice "$VM2" "$2") ||
        error "$1: the driver has no slice of claim $2 in $DRA_STATE"
    NUMA_SLICES[$1]="$dax $3"
    dra-memdev-wait "$VM2" "slice-$1" "$DRA_LOCAL_SERIAL" \
        "[(d['driver'], d['size']) for d in dax_slices() if d['name'] == '$dax'] == [('kmem', $3)]" 10
    echo "$1: NUMA slice $dax, $3 bytes"
}

numa-slice-gone() {
    # Usage: numa-slice-gone LABEL
    #
    # Wait until the slice of LABEL (see numa-slice) is gone or has size
    # 0, while the slices of the other NUMA claims in NUMA_SLICES are kmem
    # slices of their sizes. Forget LABEL.
    local gone="${NUMA_SLICES[$1]%% *}" other dax size expr=""
    [ -n "$gone" ] || error "numa-slice-gone: no slice of $1"
    unset "NUMA_SLICES[$1]"
    for other in "${!NUMA_SLICES[@]}"; do
        read -r dax size <<< "${NUMA_SLICES[$other]}"
        expr+=" and [(d['driver'], d['size']) for d in dax_slices() if d['name'] == '$dax'] == [('kmem', $size)]"
    done
    dra-memdev-wait "$VM2" "slice-$1-gone" "$DRA_LOCAL_SERIAL" \
        "'$gone' not in [d['name'] for d in dax_slices()]$expr" 30
    echo "$1: NUMA slice $gone gone, the others (${!NUMA_SLICES[*]}) kept"
}

main-mems() {
    # Usage: main-mems
    #
    # Set MAIN_MEMS to cpuset.mems of container main of the requester. Fail
    # the test if it cannot be read.
    MAIN_MEMS=$(dra-container-read "$VM2" requester main cpuset.mems) ||
        error "cannot read cpuset.mems of container main of the requester"
}

lent-check() {
    # Usage: lent-check NODES LABEL [NS/POD/CONTAINER...]
    #
    # Check that NUMA nodes NODES (a node list) are lent to container main
    # of the requester and to nobody else: cpuset.mems of main is the DRAM
    # nodes and NODES (the driver wrote it through NRI), cpuset.mems
    # of container idle is as it was at start (see mems-once), and of the
    # running containers of the node only main and the listed containers
    # (those with claims of the nodes) have the nodes.
    local nodes="$1" label="$2" node effective idle_allowed
    shift 2
    main-mems
    effective=$(dra-container-read "$VM2" requester main cpuset.mems.effective) ||
        error "$label: cannot read cpuset.mems.effective of container main of the requester"
    dra-numa-nodes-eq "$MAIN_MEMS" "$DRAM_NODES,$nodes" ||
        error "$label: cpuset.mems of container main is '$MAIN_MEMS', expected $DRAM_NODES,$nodes"
    for node in ${nodes//,/ }; do
        dra-numa-nodes-has "$effective" "$node" ||
            error "$label: node $node is not in cpuset.mems.effective of container main"
        dra-numa-nodes-has "$(dra-mems-allowed "$VM2" requester/main)" "$node" ||
            error "$label: node $node is not in Mems_allowed_list of container main"
        dra-mems-fenced "$VM2" "$node" "$DRA_NS/requester/main" "$@"
    done
    dra-mems-once "$label" "$VM2" requester idle "$DRAM_NODES" IDLE_MEMS
    idle_allowed=$(dra-mems-allowed "$VM2" requester/idle)
    for node in ${nodes//,/ }; do
        dra-numa-nodes-has "$idle_allowed" "$node" &&
            error "$label: node $node is in Mems_allowed_list of container idle"
    done
    echo "$label: nodes $nodes lent to container main: cpuset.mems $MAIN_MEMS"
}

not-lent-check() {
    # Usage: not-lent-check LABEL [NS/POD/CONTAINER...]
    #
    # Check that container main of the requester has its DRAM nodes only,
    # and no memory left on NUMA_NODE: the pages of numa-touch.py, which
    # still runs, moved off the node. Of the running containers of the
    # node only the listed containers (those with claims of the node) have
    # NUMA_NODE.
    local label="$1"
    shift
    retry-until --timeout 30 --interval 1 --message "$label: cpuset.mems of container main back to $DRAM_NODES" \
        'main-mems && dra-numa-nodes-eq "$MAIN_MEMS" "$DRAM_NODES"' ||
        error "$label: cpuset.mems of container main is '$MAIN_MEMS', not back to $DRAM_NODES"
    dra-numa-nodes-eq "$(dra-mems-allowed "$VM2" requester/main)" "$DRAM_NODES" || error "$label: Mems_allowed_list of container main is not back to $DRAM_NODES"
    retry-until --timeout 30 --interval 1 --message "$label: no anonymous memory of container main on node $NUMA_NODE" \
        '[ "$(dra-container-anon "$VM2" requester main "$NUMA_NODE")" == 0 ]' ||
        error "$label: container main still has memory on node $NUMA_NODE"
    dra-touch-pages "$NUMA_NODE"
    [ "$DRA_TOUCH_PAGES" == 0 ] || error "$label: numa-touch.py still has $DRA_TOUCH_PAGES pages on node $NUMA_NODE"
    echo "$label: numa-touch.py (pid $DRA_TOUCH_PID) lives, its pages moved off node $NUMA_NODE"
    dra-mems-fenced "$VM2" "$NUMA_NODE" "$@"
    dra-mems-once "$label" "$VM2" requester idle "$DRAM_NODES" IDLE_MEMS
}

pool-not-local() {
    # Usage: pool-not-local LABEL
    #
    # Check that the driver, since it started, has never published
    # pooled0 as a local memdev (device memdev-<serial of pooled0>).
    local n
    n=$(dra-driver-log "$VM2" | grep -c "discovered local memdev memdev-${POOLED0_SERIAL#0x}")
    echo "$1: nri-dra-driver-cxl published memdev-${POOLED0_SERIAL#0x} $n times"
    [ "$n" == 0 ] ||
        error "$1: nri-dra-driver-cxl published the pool device pooled0 as local device memdev-${POOLED0_SERIAL#0x}"
}

nothing-left() {
    # Usage: nothing-left LABEL [no-requester]
    #
    # Wait until no lease and no static claim has anything left anywhere:
    # no companion pods, no cxl-lease-* claims, no CXL regions in the VM,
    # no device of the pool attached for a claim, (unless no-requester) no
    # leases in "cxl-request list" of the requester, and the local device in
    # its initial state with no memory on NUMA_NODE (see
    # dra-local-initial-state).
    local label="$1"
    dra-wait "$VM2" "$label-no-companions" "pods -n $DRA_NS -l $DRA_LEASE_LABEL" "items == []" 90
    dra-wait "$VM2" "$label-no-lease-claims" "resourceclaims -n $DRA_NS" "not any(n.startswith('cxl-lease-') for n in names())" 90
    pool-cxl-wait "$VM2" "$label" \
        "regions == [] and by_serial($DRA_LOCAL_SERIAL) is not None and enabled(by_serial($DRA_LOCAL_SERIAL)) and by_serial($POOLED0_SERIAL) is None" 90
    retry-until --timeout 60 --message "no attachments of the pool controller" \
        '[ -z "$(dra-pool-attachments "$VM2_NAME")" ]' ||
        error "$label: the pool still has attachments: $(dra-pool-attachments "$VM2_NAME")"
    pool-host-client -o json devices pooled0 || command-error "cannot get pooled0"
    pool-assert "$COMMAND_OUTPUT" "j['state'] == 'free' and j['attachments'] in (None, [])"
    if [ "$2" != no-requester ]; then
        dra-leases "$VM2" requester/main
        dra-assert "$DRA_LEASES" "j == []"
    fi
    dra-local-initial-state "$VM2" "$label-local" "$NUMA_NODE"
    echo "$label: nothing left"
}

claim-slice() {
    # Usage: claim-slice VMDIR CLAIM-UID
    #
    # Print the dax device (daxX.Y) of the slice of the claim with
    # CLAIM-UID in the state of the driver in the VM of VMDIR: of the
    # first grant of the claim that uses a local memdev ($DRA_STATE, see
    # claim_uses() of dra-py). Return 1 if the claim has no slice there.
    local json dax
    json=$(dra-state-json "$1") || return 1
    dax=$(dra-py eval "$json" "next((u[0]['got']['dax'] for u in (claim_uses(m, '$2') for m in local_memdevs()) if u and (u[0].get('got') or {}).get('dax')), '')") || return 1
    [ -n "$dax" ] || return 1
    echo "$dax"
}

echo "### preconditions: patched qemu, hdm_for_passthrough=on, kernel with CONFIG_FS_DAX"
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"

echo "### preconditions: cxl, daxctl and cxl-dump in the VM, no CXL memory devices in VM2 and VM1"
pool-vm-tools-install "$VM2"
pool-vm-reset "$VM2"
pool-vm-reset "$VM1"
pool-cxl-dump "$VM2" no-devices
cxl-assert 'memdevs == [] and regions == [] and endpoints == []'
CPU_NODES=$(cxl-value 'len(nodes)')
vm-command "cat /sys/devices/system/node/has_memory" || command-error "cannot read the memory nodes of the VM"
DRAM_NODES="$COMMAND_OUTPUT"
dra-memmap-on-memory-off "$VM2"

echo "### preconditions: fake-cxl-pool-server on the host, with one unshared device"
pool-server-start "
  - name: pooled0
    size: 512Mi
    shared: false
    serial: $POOLED0_SERIAL"
pool-client-install "$VM2"

echo "### preconditions: the pool controller and the driver (with cxl-request) run in the cluster"
dra-build
dra-install
dra-pull-images "$VM2" "$DRA_TOOLS_IMAGE" "$DRA_COMPANION_IMAGE"
dra-tools-install "$VM2"
dra-trace-start "$VM2"

echo "### preconditions: the local device, and a pod that may want memory later"
dra-local-device "$VM2"
LD="$DRA_LOCAL_LD"             # the device in the ResourceSlices (dra-py)
LREGIONS="$DRA_LOCAL_REGIONS"  # its regions in cxl-dump (cxl-py)
NUMA_NODE=""           # the NUMA node of the device, known when memory of it is online
# Sizes in memory blocks: static-a 1, the NUMA leases 1 and half a block
# (it consumes 1), the dax leases half a block (1) and 1, static-b 2: 7
# blocks of the device at the same time.
BS="$DRA_LOCAL_BLOCK_SIZE"
dra-local-blocks-require "$VM2" "$(basename "$TOPOLOGY_DIR")" 7
STATIC_A=$BS
LEASE1=$BS
DAX1_REQ=$(( BS / 2 ))
DAX2=$BS
STATIC_B=$(( 2 * BS ))
LEASE2_REQ=$(( BS / 2 ))
RO_REQ=$(( BS / 8 ))
ASYNC_REQ=$(( BS / 2 ))
TOUCH_MIB=$(( BS / 2 / MiB ))
dra-json "$VM2" resourceslices
dra-assert "$COMMAND_OUTPUT" "attr($LD, 'type') == 'memdev' and attr($LD, 'source') == 'local' and attr($LD, 'shared') == False and attr($LD, 'memoryBlockSize') == $BS and attr($LD, 'mode') is None"
dra-apply "$VM2" <<< "$(dra-lease-template-yaml)
---
$(dra-pod-yaml requester "$VM2_NAME" cxl=template:cxl-lease -- main=cxl idle)"
dra-pod-wait "$VM2" requester Running 60
dra-json "$VM2" pod -n "$DRA_NS" requester
REQUESTER_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
LEASE_CLAIM=$(dra-pod-claim "$VM2" requester) || error "the requester has no lease claim"
dra-json "$VM2" resourceclaim -n "$DRA_NS" "$LEASE_CLAIM"
LEASE_CLAIM_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "j == []"
dra-numa-nodes-eq "$(dra-mems-allowed "$VM2" requester/main)" "$DRAM_NODES" || error "container main: Mems_allowed_list is not $DRAM_NODES"
IDLE_MEMS=$(dra-container-read "$VM2" requester idle cpuset.mems) ||
    error "cannot read cpuset.mems of container idle of the requester"
main-mems
MAIN_MEMS0="$MAIN_MEMS"
echo "cpuset.mems of container main: '$MAIN_MEMS0', of container idle: '$IDLE_MEMS'"
[ -z "$(dra-pool-attachments "$VM2_NAME")" ] || error "the pool has attachments before any lease"

echo "### A: a pod with a static NUMA claim of the local device"
dra-claim-pod "$VM2" static-a "$LOCAL_CLASS" "$(dra-mi "$STATIC_A")"
dra-pod-wait "$VM2" static-a Running 120
dra-local-claim-check "$VM2" static-a "$STATIC_A"
dra-local-slices-wait "$VM2" static-a "$NUMA_NODE" "numa:$STATIC_A"
NUMA_NODE=$(dra-value "$DRA_MEMDEV" "dax_slices('kmem')[0]['node']")
(( NUMA_NODE >= CPU_NODES )) || error "the NUMA slice of static-a is on node $NUMA_NODE, a CPU node"
echo "the local device is NUMA node $NUMA_NODE of the VM"
numa-slice static-a "$DRA_CLAIM_UID" "$STATIC_A"
dra-local-numa-node-size "$VM2" static-a "$NUMA_NODE" "$STATIC_A"
# The device is published as before, and its region is not published as
# node-local CXL memory (a region device).
dra-local-unchanged "$VM2"
dra-assert "$COMMAND_OUTPUT" "not any(attr(d, 'type') == 'region' for d in devices('$DRA_LOCAL_DRIVER', '$VM2_NAME'))"
dra-mems-wait "$VM2" static-a main "$NUMA_NODE"
# The node is static-a's only: it is not lent to the requester, and every
# other container of the node is kept off it. An empty cpuset.mems of a
# container of the requester becomes its DRAM nodes.
dra-mems-fenced "$VM2" "$NUMA_NODE" "$DRA_NS/static-a/main"
dra-mems-once A "$VM2" requester main "$DRAM_NODES" MAIN_MEMS0
dra-mems-once A "$VM2" requester idle "$DRAM_NODES" IDLE_MEMS

echo "### B: the running container leases a NUMA slice of the same device"
local-numa-lease "$LEASE1"
NUMA_LEASE1="$LEASE"
numa-slice lease-1 "$COMPANION_UID" "$LEASE1"
dra-local-slices-wait "$VM2" lease-1 "$NUMA_NODE" "numa:$STATIC_A" "numa:$LEASE1"
dra-local-numa-node-size "$VM2" lease-1 "$NUMA_NODE" $(( STATIC_A + LEASE1 ))
lent-check "$NUMA_NODE" B "$DRA_NS/static-a/main"
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "[(l['lease'], l['state'], l.get('mode'), bool(l.get('share')), l.get('source'), l.get('serial'), int(l.get('size') or 0)) for l in j] == [('$NUMA_LEASE1', 'Ready', 'numa', False, 'local', '$DRA_LOCAL_SERIAL', $LEASE1)]"
# Container main uses the memory, it keeps it until D.
dra-touch-start "$VM2" requester/main "$TOUCH_MIB" "$NUMA_NODE"
pages=$(dra-touch-log-pages "$DRA_TOUCH_LOG" "$NUMA_NODE")
[ "$pages" -ge $(( (TOUCH_MIB - 8) << 8 )) ] || error "only $pages pages of numa-touch.py are on node $NUMA_NODE"
anon=$(dra-container-anon "$VM2" requester main "$NUMA_NODE")
echo "memory.numa_stat of container main: anon N$NUMA_NODE=$anon"
[ "${anon:-0}" -ge $(( (TOUCH_MIB - 8) * MiB )) ] ||
    error "container main has ${anon:-0} bytes anon on node $NUMA_NODE, expected >= $(( TOUCH_MIB - 8 )) MiB"
dra-mems-wait "$VM2" static-a main "$NUMA_NODE"
[ -z "$(dra-pool-attachments "$VM2_NAME")" ] || error "a local lease made the pool attach a device"

echo "### C: dax leases of the same device while the NUMA lease is Ready"
TAG1="$TAG_BASE: dax lease 1"
TAG2="$TAG_BASE: dax lease 2"
# Half a block: the slice is a whole block, and so is CXL_<KEY>_SIZE.
dax-lease "$(dra-mi "$DAX1_REQ")" "--source local" 'python3 /tools/dax-rw.py --fd 3 write 1M "$1"' "$TAG1"
DAX1_LEASE="$DAX_LEASE"
DAX1_PATH="$DAX_PATH"
[ "$DAX_KEY $DAX_SIZE $DAX_RO" == "$DRA_LOCAL_HEX $(dra-local-round "$DAX1_REQ") 0" ] || error "lease $DAX_LEASE: device $DAX_KEY size $DAX_SIZE read-only $DAX_RO, expected $DRA_LOCAL_HEX $(dra-local-round "$DAX1_REQ") 0"
dax-lease "$(dra-mi "$DAX2")" "--source local" 't=$(python3 /tools/dax-rw.py --fd 3 read 1M) || exit 1; echo "before: $t"; python3 /tools/dax-rw.py --fd 3 write 1M "$1"' "$TAG2"
DAX2_LEASE="$DAX_LEASE"
DAX2_PATH="$DAX_PATH"
[ "$DAX_KEY $DAX_SIZE $DAX_RO" == "$DRA_LOCAL_HEX $DAX2 0" ] || error "lease $DAX_LEASE: device $DAX_KEY size $DAX_SIZE read-only $DAX_RO, expected $DRA_LOCAL_HEX $DAX2 0"
[ "$DAX2_PATH" != "$DAX1_PATH" ] || error "both dax leases have $DAX1_PATH"
grep -qxF "before: $TAG1" <<< "$DAX_OUT" && error "lease $DAX2_LEASE sees what lease $DAX1_LEASE wrote: not a slice of its own"
companion-check "$DAX1_LEASE" "$LOCAL_CLASS" "$DAX1_REQ" "$(dra-local-round "$DAX1_REQ")" dax
companion-check "$DAX2_LEASE" "$LOCAL_CLASS" "$DAX2" "$DAX2" dax
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "sorted((l['lease'], l['state'], l.get('mode'), l.get('source'), int(l.get('size') or 0), l.get('dax')) for l in j if l['lease'] != '$NUMA_LEASE1') == sorted([('$DAX1_LEASE', 'Ready', 'dax', 'local', $(dra-local-round "$DAX1_REQ"), '$DAX1_PATH'), ('$DAX2_LEASE', 'Ready', 'dax', 'local', $DAX2, '$DAX2_PATH')])"
dra-assert "$DRA_LEASES" "[l['state'] for l in j if l['lease'] == '$NUMA_LEASE1'] == ['Ready']"
dra-local-slices-wait "$VM2" dax-leases "$NUMA_NODE" "numa:$STATIC_A" "numa:$LEASE1" "dax:$(dra-local-round "$DAX1_REQ")" "dax:$DAX2"
dra-assert "$DRA_MEMDEV" "sorted(d['name'] for d in dax_slices('device_dax')) == sorted(['${DAX1_PATH#/dev/}', '${DAX2_PATH#/dev/}'])"
# Dax slices add nothing to the node.
dra-local-numa-node-size "$VM2" dax-leases "$NUMA_NODE" $(( STATIC_A + LEASE1 ))
dra-local-unchanged "$VM2"
lent-check "$NUMA_NODE" C "$DRA_NS/static-a/main"
# The fds again, for other programs: each lease has what was written in it.
dax-fd-read "$DAX1_LEASE" "$TAG1"
dax-fd-read "$DAX2_LEASE" "$TAG2"

echo "### D: more NUMA memory on the node, the leases given back one by one"
dra-claim-pod "$VM2" static-b "$LOCAL_CLASS" "$(dra-mi "$STATIC_B")" numa
dra-pod-wait "$VM2" static-b Running 120
dra-local-claim-check "$VM2" static-b "$STATIC_B"
numa-slice static-b "$DRA_CLAIM_UID" "$STATIC_B"
dra-local-numa-node-size "$VM2" static-b "$NUMA_NODE" $(( STATIC_A + LEASE1 + STATIC_B ))
dra-mems-wait "$VM2" static-b main "$NUMA_NODE"
# A second NUMA lease of half a block: one block more on the same node,
# the node lent to the container by two leases.
local-numa-lease "$LEASE2_REQ"
NUMA_LEASE2="$LEASE"
numa-slice lease-2 "$COMPANION_UID" "$(dra-local-round "$LEASE2_REQ")"
dra-local-numa-node-size "$VM2" lease-2 "$NUMA_NODE" $(( STATIC_A + LEASE1 + STATIC_B + $(dra-local-round "$LEASE2_REQ") ))
dra-local-slices-wait "$VM2" lease-2 "$NUMA_NODE" "numa:$STATIC_A" "numa:$LEASE1" "dax:$(dra-local-round "$DAX1_REQ")" "dax:$DAX2" "numa:$STATIC_B" "numa:$(dra-local-round "$LEASE2_REQ")"
lent-check "$NUMA_NODE" D "$DRA_NS/static-a/main" "$DRA_NS/static-b/main"
# Give back the first lease: its block, and no other, goes offline, the
# pages on it move, and the node stays lent: the second lease still holds
# it.
release "$NUMA_LEASE1"
dra-local-numa-node-size "$VM2" lease-1-released "$NUMA_NODE" $(( STATIC_A + STATIC_B + $(dra-local-round "$LEASE2_REQ") ))
numa-slice-gone lease-1
dra-wait "$VM2" lease-1-gone "pods,resourceclaims -n $DRA_NS -l $DRA_LEASE_LABEL=$NUMA_LEASE1" "items == []" 60
lent-check "$NUMA_NODE" D-lease-1-released "$DRA_NS/static-a/main" "$DRA_NS/static-b/main"
dra-touch-pages "$NUMA_NODE"
echo "numa-touch.py (pid $DRA_TOUCH_PID) lives, $DRA_TOUCH_PAGES of its pages on node $NUMA_NODE"
# Give back the second lease: the node is not lent any more, the pages
# of the container move back to DRAM, the static pods keep the node.
release "$NUMA_LEASE2"
dra-local-numa-node-size "$VM2" lease-2-released "$NUMA_NODE" $(( STATIC_A + STATIC_B ))
numa-slice-gone lease-2
not-lent-check D-lease-2-released "$DRA_NS/static-a/main" "$DRA_NS/static-b/main"
dra-wait "$VM2" lease-2-gone "pods,resourceclaims -n $DRA_NS -l $DRA_LEASE_LABEL=$NUMA_LEASE2" "items == []" 60
dra-touch-stop
dra-mems-wait "$VM2" static-a main "$NUMA_NODE"
dra-mems-wait "$VM2" static-b main "$NUMA_NODE"
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "sorted(l['lease'] for l in j) == sorted(['$DAX1_LEASE', '$DAX2_LEASE'])"
dra-delete-pod "$VM2" static-a
dra-local-numa-node-size "$VM2" static-a-deleted "$NUMA_NODE" "$STATIC_B"
numa-slice-gone static-a
# The dax leases still have their data.
dax-fd-read "$DAX1_LEASE" "$TAG1"
dax-fd-read "$DAX2_LEASE" "$TAG2"
dra-local-slices-wait "$VM2" static-a-deleted "$NUMA_NODE" "dax:$(dra-local-round "$DAX1_REQ")" "dax:$DAX2" "numa:$STATIC_B"
# A read-only slice: the fd is opened read-only, a writable mapping is
# refused.
dax-lease "$(dra-mi "$RO_REQ")" "--source local --read-only" 'python3 /tools/dax-rw.py --fd 3 check-ro 0'
RO_LEASE="$DAX_LEASE"
[ "$DAX_KEY $DAX_SIZE $DAX_RO" == "$DRA_LOCAL_HEX $(dra-local-round "$RO_REQ") 1" ] || error "lease $DAX_LEASE: device $DAX_KEY size $DAX_SIZE read-only $DAX_RO, expected $DRA_LOCAL_HEX $(dra-local-round "$RO_REQ") 1"
grep -qxF "writable mapping refused: EACCES" <<< "$DAX_OUT" || error "the read-only lease $RO_LEASE could be mapped for writing"
companion-check "$RO_LEASE" "$LOCAL_CLASS" "$RO_REQ" "$(dra-local-round "$RO_REQ")" dax ro
dra-local-slices-wait "$VM2" read-only "$NUMA_NODE" "dax:$(dra-local-round "$DAX1_REQ")" "dax:$DAX2" "numa:$STATIC_B" "dax:$(dra-local-round "$RO_REQ")"
# A request that does not wait (--wait 0s) returns the lease before it is
# Ready; "cxl-request env --wait" waits until it is Ready and runs the
# program with its fd.
TAG3="$TAG_BASE: dax lease 3"
dra-request "$VM2" requester/main request --source local --size "$(dra-mi "$ASYNC_REQ")" --mode dax --wait 0s
[ "$DRA_REQ_STATUS" == 0 ] || error "cxl-request request --wait 0s exited $DRA_REQ_STATUS"
dra-assert "$DRA_REQ_JSON" "j.get('state') in ('Creating', 'Pending', 'Lending') and j.get('mode') == 'dax' and j.get('source') == 'local'"
ASYNC_LEASE=$(dra-value "$DRA_REQ_JSON" "j['lease']")
dra-lease-new "$ASYNC_LEASE"
dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" env --lease "$ASYNC_LEASE" --wait 60s --exec python3 /tools/dax-rw.py --fd 3 write 1M "$TAG3" ||
    command-error "cxl-request env --lease $ASYNC_LEASE --wait 60s --exec ... write failed"
dax-fd-read "$ASYNC_LEASE" "$TAG3"
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "[(l['state'], int(l.get('size') or 0)) for l in j if l['lease'] == '$ASYNC_LEASE'] == [('Ready', $(dra-local-round "$ASYNC_REQ"))]"
companion-check "$ASYNC_LEASE" "$LOCAL_CLASS" "$ASYNC_REQ" "$(dra-local-round "$ASYNC_REQ")" dax
dra-local-slices-wait "$VM2" async "$NUMA_NODE" "dax:$(dra-local-round "$DAX1_REQ")" "dax:$DAX2" "numa:$STATIC_B" "dax:$(dra-local-round "$RO_REQ")" "dax:$(dra-local-round "$ASYNC_REQ")"
dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" release --all ||
    command-error "cxl-request release --all failed"
dra-wait "$VM2" dax-leases-gone "pods -n $DRA_NS -l $DRA_LEASE_LABEL" "items == []" 60
dra-local-slices-wait "$VM2" dax-leases-released "$NUMA_NODE" "numa:$STATIC_B"
dra-local-unchanged "$VM2"
dra-delete-pod "$VM2" static-b
dra-local-numa-node-size "$VM2" static-b-deleted "$NUMA_NODE" 0
numa-slice-gone static-b
nothing-left released

echo "### D2: a container with a static NUMA claim that is not steered takes a NUMA lease"
# Steering is opt-in: the static claim of container main of pod mixed has
# no container section, so the driver only fences the container, the node
# of its claim in its cpuset.mems, and lends it more of the node as to any
# container that is not steered. The container has the lease claim too.
dra-apply "$VM2" <<< "$(dra-claim-yaml template mixed-static "$LOCAL_CLASS" "$(dra-mi "$STATIC_A")")
---
$(dra-pod-yaml mixed "$VM2_NAME" mem=template:mixed-static cxl=template:cxl-lease)"
dra-pod-wait "$VM2" mixed Running 120
dra-memory-wait "$VM2" mixed main fenced "none (default)"
dra-mems-wait "$VM2" mixed main "$NUMA_NODE"
dra-local-numa-node-size "$VM2" mixed-static "$NUMA_NODE" "$STATIC_A"
dra-exec "$VM2" mixed/main "$DRA_REQUEST_CTR_BIN" request --source local --size "$(dra-mi "$LEASE1")" --wait 120s ||
    command-error "cxl-request request in mixed/main failed: a container with a static NUMA claim that is not steered must get a NUMA lease"
DRA_REQ_JSON=$(dra-last-json "$COMMAND_OUTPUT") || error "cxl-request request in mixed/main printed no JSON response"
dra-assert "$DRA_REQ_JSON" "j.get('state') == 'Ready' and j.get('mode') == 'numa' and j.get('source') == 'local' and int(j['numaNode']) == $NUMA_NODE and int(j['size']) == $LEASE1"
MIXED_LEASE=$(dra-value "$DRA_REQ_JSON" "j['lease']")
dra-lease-new "$MIXED_LEASE"
dra-local-numa-node-size "$VM2" mixed-lease "$NUMA_NODE" $(( STATIC_A + LEASE1 ))
dra-mems-fenced "$VM2" "$NUMA_NODE" "$DRA_NS/mixed/main"
# The lease given back: the node stays, it is the node of the static
# claim; the container is still not steered.
dra-exec "$VM2" mixed/main "$DRA_REQUEST_CTR_BIN" release --lease "$MIXED_LEASE" ||
    command-error "cxl-request release --lease $MIXED_LEASE in mixed/main failed"
dra-local-numa-node-size "$VM2" mixed-lease-released "$NUMA_NODE" "$STATIC_A"
dra-wait "$VM2" mixed-lease-gone "pods,resourceclaims -n $DRA_NS -l $DRA_LEASE_LABEL=$MIXED_LEASE" "items == []" 60
dra-mems-fenced "$VM2" "$NUMA_NODE" "$DRA_NS/mixed/main"
high=$(dra-container-read "$VM2" mixed main memory.high) || error "cannot read memory.high of mixed/main"
[ "$high" == max ] || error "mixed/main: memory.high is '$high' after the lease, expected max: not steered"
dra-delete-pod "$VM2" mixed
dra-local-numa-node-size "$VM2" mixed-deleted "$NUMA_NODE" 0
nothing-left released-mixed

echo "### E: without --source: local memory first, the pool when the local device is full"
# The whole local device as one lease.
numa-lease "$(dra-mi "$DRA_LOCAL_SIZE")"
ANY1_LEASE="$LEASE"
ANY1_NODE="$LEASE_NODE"
dra-assert "$DRA_REQ_JSON" "j.get('source') == 'local' and j.get('serial') == '$DRA_LOCAL_SERIAL' and int(j['numaNode']) == $NUMA_NODE and int(j['size']) == $DRA_LOCAL_SIZE"
dra-local-numa-node-size "$VM2" any-local "$NUMA_NODE" "$DRA_LOCAL_SIZE"
dra-pod-wait "$VM2" "cxl-lease-$ANY1_LEASE" Running 60
dra-json "$VM2" resourceclaim -n "$DRA_NS" "cxl-lease-$ANY1_LEASE"
ANY_SUBREQUESTS="[(s['name'], s['deviceClassName'], quantity(s['capacity']['requests']['memory'])) for s in subrequests()]"
dra-assert "$COMMAND_OUTPUT" "[s['name'] for s in j['spec']['devices']['requests'][0]['firstAvailable']] == ['local', 'pool']"
dra-assert "$COMMAND_OUTPUT" "$ANY_SUBREQUESTS == [('lease/local', '$LOCAL_CLASS', $DRA_LOCAL_SIZE), ('lease/pool', 'cxl-pool', $DRA_LOCAL_SIZE)]"
dra-assert "$COMMAND_OUTPUT" "[(r['request'], r['driver'], r['device']) for r in results()] == [('lease/local', '$DRA_LOCAL_DRIVER', '$DRA_LOCAL_DEVICE')] and consumed() == $DRA_LOCAL_SIZE"
# A MemoryConfig for each driver, each for its own subrequest, of the one
# apiVersion of both drivers.
ANY_CONFIGS="sorted((c['driver'], c['parameters']['apiVersion'], c['parameters']['kind'], config_section(c, 'device').get('mode'), c['requests']) for c in configs())"
dra-assert "$COMMAND_OUTPUT" "$ANY_CONFIGS == [('$DRA_POOL_DRIVER', '$DRA_CONFIG_API', '$DRA_CONFIG_KIND', 'numa', ['lease/pool']), ('$DRA_LOCAL_DRIVER', '$DRA_CONFIG_API', '$DRA_CONFIG_KIND', 'numa', ['lease/local'])]"
dra-companion-meta-check "$COMMAND_OUTPUT" "$ANY1_LEASE" "$REQUESTER_UID" "$VM2_NAME" "$DRA_NS/requester/main"
[ -z "$(dra-pool-attachments "$VM2_NAME")" ] || error "a request without --source with room on the local device made the pool attach a device"
# The local device has no room left: the next lease comes from the pool.
numa-lease 256Mi
ANY2_LEASE="$LEASE"
ANY2_NODE="$LEASE_NODE"
dra-assert "$DRA_REQ_JSON" "j.get('source') == 'pool' and j.get('serial') == '$POOLED0_SERIAL' and int(j['size']) >= 256 * MiB"
dra-pod-wait "$VM2" "cxl-lease-$ANY2_LEASE" Running 60
dra-json "$VM2" resourceclaim -n "$DRA_NS" "cxl-lease-$ANY2_LEASE"
CLAIM_JSON="$COMMAND_OUTPUT"
dra-assert "$CLAIM_JSON" "[s['name'] for s in j['spec']['devices']['requests'][0]['firstAvailable']] == ['local', 'pool']"
dra-assert "$CLAIM_JSON" "$ANY_SUBREQUESTS == [('lease/local', '$LOCAL_CLASS', 256 * MiB), ('lease/pool', 'cxl-pool', 256 * MiB)]"
dra-assert "$CLAIM_JSON" "[(r['request'], r['driver'], r['device']) for r in results()] == [('lease/pool', '$DRA_POOL_DRIVER', 'pooled0')]"
dra-assert "$CLAIM_JSON" "$ANY_CONFIGS == [('$DRA_POOL_DRIVER', '$DRA_CONFIG_API', '$DRA_CONFIG_KIND', 'numa', ['lease/pool']), ('$DRA_LOCAL_DRIVER', '$DRA_CONFIG_API', '$DRA_CONFIG_KIND', 'numa', ['lease/local'])]"
dra-assert "$CLAIM_JSON" "condition('$DRA_POOL_DRIVER/Attached') == 'True' and allocated_node() == '$VM2_NAME'"
ANY2_UID=$(dra-value "$CLAIM_JSON" "j['metadata']['uid']")
pool-host-client -o json devices pooled0 || command-error "cannot get pooled0"
pool-assert "$COMMAND_OUTPUT" "[(a['host'], a.get('owner'), a['state']) for a in j['attachments']] == [('$VM2_NAME', 'k8s:resourceclaim/$ANY2_UID', 'attached')]"
pool-cxl-wait "$VM2" any-two-regions \
    "len(regions) == 2 and by_serial($POOLED0_SERIAL) is not None and [(r['numaNode'], online_size(r)) for r in $LREGIONS] == [($ANY1_NODE, $DRA_LOCAL_SIZE)] and [(r['numaNode'], online_size(r)) for r in regions if sorted(r['memdevs']) == [by_serial($POOLED0_SERIAL)['name']]] == [($ANY2_NODE, $POOLED0_SIZE)]"
echo "lease $ANY1_LEASE: local node $ANY1_NODE; lease $ANY2_LEASE: pooled0, node $ANY2_NODE"
# The pool slot of pooled0 is under host bridge 0, like the local device:
# both regions are on the same NUMA node, which both leases lend, and the
# node has the memory of both.
[ "$ANY1_NODE" == "$ANY2_NODE" ] ||
    error "pooled0 is on node $ANY2_NODE, the local device on node $ANY1_NODE: two leases of one node are not exercised"
dra-local-numa-node-size "$VM2" any-pool "$NUMA_NODE" $(( DRA_LOCAL_SIZE + POOLED0_SIZE )) "$DRA_LOCAL_SIZE"
dra-json "$VM2" resourceslices
dra-assert "$COMMAND_OUTPUT" "not any(attr(d, 'type') == 'region' for d in devices('$DRA_LOCAL_DRIVER', '$VM2_NAME'))"
# A pool device is never a local memdev, also while it is attached but
# not prepared yet.
pool-not-local E
lent-check "$ANY1_NODE,$ANY2_NODE" E
# Give the pool memory back: the local lease keeps its node, also when
# both regions are under host bridge 0 and so on the same node.
release "$ANY2_LEASE"
pool-cxl-wait "$VM2" any-pool-released "by_serial($POOLED0_SERIAL) is None and len(regions) == 1" 90
dra-local-numa-node-size "$VM2" any-pool-released "$NUMA_NODE" "$DRA_LOCAL_SIZE"
retry-until --timeout 60 --message "pooled0 detached" '[ -z "$(dra-pool-attachments "$VM2_NAME")" ]' ||
    error "pooled0 is still attached: $(dra-pool-attachments "$VM2_NAME")"
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "[(l['lease'], l['state'], l.get('source')) for l in j] == [('$ANY1_LEASE', 'Ready', 'local')]"
lent-check "$ANY1_NODE" E-local-left
release "$ANY1_LEASE"
dra-local-numa-node-size "$VM2" any-local-released "$NUMA_NODE" 0
nothing-left released-any

echo "### F: delete the pod while it has leases"
# A NUMA lease of local memory with memory in use, a dax lease of the
# local device and a dax lease of the pool: the companions are owned by
# the requester, and the driver releases the leases of the pod when
# its lease claim is unprepared.
local-numa-lease "$BS"
dra-touch-start "$VM2" requester/main "$TOUCH_MIB" "$NUMA_NODE"
dax-lease "$(dra-mi "$BS")" "--source local" 'python3 /tools/dax-rw.py --fd 3 write 1M "$1"' "$TAG1"
dra-request "$VM2" requester/main request --source pool --size 64Mi --mode dax --wait 120s
[ "$DRA_REQ_STATUS" == 0 ] || error "cxl-request request --source pool --size 64Mi --mode dax exited $DRA_REQ_STATUS"
dra-assert "$DRA_REQ_JSON" "j.get('state') == 'Ready' and j.get('source') == 'pool' and j.get('serial') == '$POOLED0_SERIAL' and j.get('dax', '').startswith('/dev/dax')"
dra-lease-new "$(dra-value "$DRA_REQ_JSON" "j['lease']")"
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "sorted((l['state'], l.get('source'), l.get('mode')) for l in j) == [('Ready', 'local', 'dax'), ('Ready', 'local', 'numa'), ('Ready', 'pool', 'dax')]"
[ -n "$(dra-pool-attachments "$VM2_NAME")" ] || error "the dax lease of the pool has no attachment"
dra-local-slices-wait "$VM2" three-leases "$NUMA_NODE" "numa:$BS" "dax:$BS"
dra-local-numa-node-size "$VM2" three-leases "$NUMA_NODE" "$BS"
dra-delete "$VM2" pod requester --grace-period=2 --wait=false
dra-touch-forget    # numa-touch.py goes with the pod
dra-wait "$VM2" requester-gone "pods -n $DRA_NS" "'requester' not in names()" 90
nothing-left pod-deleted no-requester
dra-wait "$VM2" lease-claim-gone "resourceclaims -n $DRA_NS" "names() == []" 90
vm-command "test -e $DRA_LEASE_DIR/$LEASE_CLAIM_UID" &&
    error "the lease directory of claim $LEASE_CLAIM is still in $DRA_LEASE_DIR"
vm-command "test -e $DRA_LEASE_FS_DIR/$LEASE_CLAIM_UID" &&
    error "the filesystem directory of claim $LEASE_CLAIM is still in $DRA_LEASE_FS_DIR"
echo "pod deleted with 3 active leases: everything cleaned up"
pool-not-local F
echo "leases of the test: ${DRA_LEASE_IDS[*]}"

echo "### G: what every component did, in order"
# Every slice is whole memory blocks: the scheduler rounded every claim
# up, and the kernel's first-fit kept every slice block-aligned.
dra-local-slices-aligned "$VM2"
# The templates of the claims that went with their pods (dra-claim-pod)
# and of the lease claims.
dra-delete "$VM2" resourceclaimtemplates --all --wait=true --timeout=60s
dra-assert-clean "$VM2" "$VM1"

dra-trace-stop
dra-trace-summary

echo "### cleanup"
dra-cleanup
dra-local-detach "$VM2"
pool-cleanup
trap - EXIT
pool-port-busy && error "port $POOL_PORT is still busy after stopping the server"
echo "trace: $TEST_OUTPUT_DIR/trace.txt, summary: $TEST_OUTPUT_DIR/trace-summary.txt"
