# Attaching pooled CXL memory to a running container (leases)
#
# test10 and test19 give a pod CXL memory of the pool when the pod is
# created: the claim is in the pod spec, and the pod waits until the
# memory is there. A pod that runs already cannot get new claims: neither
# the scheduler nor kubelet allocate or prepare devices for a running pod.
# Here a running container asks for memory itself, and gives it back when
# it is done, while it keeps running.
#
# How it works (docs/memory/cxl/leases.md):
# - The pod opts in when it is created: it claims the device "lease" of
#   the node's cxl.nri.io ResourceSlice (DeviceClass cxl-lease). The
#   device has no memory, and any number of pods may claim it. Preparing
#   that claim, nri-dra-driver-cxl mounts a unix socket and the CLI
#   cxl-request into the container (/run/cxl/cxl.sock in a read-only
#   directory, /usr/local/bin/cxl-request), and an empty read-only directory
#   (/run/cxl/fs) where the filesystems of fs leases appear (test16).
#   Nothing else changes for the pod.
# - "cxl-request request --source pool --size 512Mi" in the container
#   asks the driver over the socket. The driver knows the requester by
#   the socket (the pod of the claim) and by the pid of the caller (the
#   container). It creates a lease: a ResourceClaim for pool memory and a
#   companion pause pod that holds the claim, both called
#   cxl-lease-<lease id>, both owned by the requesting pod and labeled
#   cxl.nri.io/lease=<lease id>. From there it is test10: the scheduler
#   allocates a device of the pool, fake-cxl-pool-controller attaches it
#   to this node, kubelet asks the driver to prepare the claim of the
#   companion, and the memory becomes a new NUMA node. Then the node
#   plugin lends the node to the requesting container: it adds the node
#   to cpuset.mems of that container (only), through NRI. Every other
#   container of the node is kept off the node, whatever its cpuset.mems.
#   "cxl-request env --lease ID" prints the environment that a claim of
#   the same memory gives a container (CXL_<KEY>_NUMA_NODE and the rest).
# - "cxl-request request --serial 0xc1ae0001 --share --exec CMD" joins a
#   shared device (test19). The container gets no device node: the driver
#   opens the dax device and passes the open file descriptor over the
#   socket. cxl-request runs CMD with it as fd 3, CXL_<KEY>_FD=3.
# - "cxl-request release --lease ID" (or --all) gives it back: the node is
#   removed from cpuset.mems (the kernel migrates the pages to DRAM, the
#   process lives on), the companion pod and its claim are deleted, and
#   the device goes back to the pool, as in test10.
#
# What you will see: a pod that starts without any CXL memory, requests
# that cxl-request and the driver refuse, 512Mi of pooled memory as
# a new NUMA node in one container (not in the other container of the
# same pod, nor in any other container of the node), 256 MiB of its
# memory there, the environment of the lease, which a claim of the same
# device gives too, the memory given back without stopping, shared CXL
# memory written and read through a file descriptor without any /dev/dax
# device, nothing left after each release, and the pod deleted at the
# end (test15 deletes one with leases in use). At the end the trace
# summary of what every component did, in order.
#
# Everything happens on VM2, the VM of the test (n4-cxl-shared-2), the
# control plane of the cluster of VM2 and VM1 (n4-cxl-shared-1): the pods
# are pinned to it, the companions follow their requester. Run test00-up of
# n4-cxl-shared-1, -0 and -2 first: they create the VMs and make them one
# cluster. The DRA driver comes from its image, which has cxl-request too,
# build it first:
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
POOLED0_HEX=C1EE0001        # the serial in environment variable names
POOLED0_SIZE=$(( 512 << 20 ))
SHARED_SERIAL=0xc1ae0001    # 0xc1ae....: shared pool devices
SHARED_HEX=C1AE0001
SHARED_SIZE=$(( 256 << 20 ))
TOUCH_MIB=256               # memory that the container puts on the lent node
TAG="dra-live-attach $(date +%Y-%m-%dT%H:%M:%S.%N)"
LEASE_ENV=""                # cxl-request env of the lease of pooled0
LEASE_NODE=""               # and its NUMA node

cgroup-mems() {
    # Usage: cgroup-mems CONTAINER [FILE]
    #
    # Print cpuset.mems.effective (or FILE, for instance cpuset.mems) of the
    # cgroup of container CONTAINER of the requester, read in the VM.
    local file="${2:-cpuset.mems.effective}" mems
    mems=$(dra-container-read "$VM2" requester "$1" "$file") ||
        error "cannot read $file of container $1 of the requester"
    echo "$file of container $1: $mems" >&2
    echo "$mems"
}

companion-check() {
    # Usage: companion-check LEASE DEVICE CLASS MODE
    #
    # The objects of lease LEASE: the companion pod and its claim
    # cxl-lease-LEASE, both owned by the requester, labeled and annotated
    # with the lease and its requester; the claim asks for a device of
    # DeviceClass CLASS, has a MemoryConfig of the pool driver with a
    # device section of mode MODE only and no other config, and is
    # allocated to pool device DEVICE on this node and attached by the
    # pool. CLAIM_JSON is the claim.
    local lease="$1" dev="$2" class="$3" mode="$4" name="cxl-lease-$1" pod_json claim_uid
    dra-pod-wait "$VM2" "$name" Running 30
    dra-json "$VM2" pod -n "$DRA_NS" "$name"
    pod_json="$COMMAND_OUTPUT"
    dra-assert "$pod_json" "[(o['kind'], o['name'], o['uid']) for o in j['metadata']['ownerReferences']] == [('Pod', 'requester', '$REQUESTER_UID')]"
    dra-companion-meta-check "$pod_json" "$lease" "$REQUESTER_UID" "$VM2_NAME" "$DRA_NS/requester/main"
    dra-assert "$pod_json" "j['spec']['nodeName'] == '$VM2_NAME'"
    dra-assert "$pod_json" "j['spec']['affinity']['nodeAffinity']['requiredDuringSchedulingIgnoredDuringExecution']['nodeSelectorTerms'][0]['matchFields'] == [{'key': 'metadata.name', 'operator': 'In', 'values': ['$VM2_NAME']}]"
    dra-assert "$pod_json" "{'operator': 'Exists'} in j['spec']['tolerations']"
    dra-assert "$pod_json" "j['spec']['resourceClaims'] == [{'name': 'lease', 'resourceClaimName': '$name'}]"
    # The companion only holds the claim: a pause container that uses
    # nothing of it, Guaranteed with 100m CPU and 16Mi memory.
    dra-assert "$pod_json" "[c['image'] for c in j['spec']['containers']] == ['$DRA_COMPANION_IMAGE'] and not j['spec']['containers'][0]['resources'].get('claims')"
    dra-assert "$pod_json" "j['spec']['containers'][0]['resources']['requests'] == j['spec']['containers'][0]['resources']['limits'] == {'cpu': '100m', 'memory': '16Mi'}"
    dra-json "$VM2" resourceclaim -n "$DRA_NS" "$name"
    CLAIM_JSON="$COMMAND_OUTPUT"
    dra-claim-conditions "$VM2" "$name"
    dra-assert "$CLAIM_JSON" "condition('$DRA_POOL_DRIVER/Attached') == 'True'"
    dra-assert "$CLAIM_JSON" "[(r['driver'], r['device']) for r in results()] == [('$DRA_POOL_DRIVER', '$dev')] and allocated_node() == '$VM2_NAME'"
    dra-assert "$CLAIM_JSON" "[r['name'] for r in j['status']['reservedFor']] == ['$name']"
    dra-assert "$CLAIM_JSON" "[(o['kind'], o['name'], o['uid'], o.get('controller')) for o in j['metadata']['ownerReferences']] == [('Pod', 'requester', '$REQUESTER_UID', True)]"
    # The driver knows the claim of the companion by its label, the
    # requester by the lease. The claim says what was asked: the class and
    # the mode.
    dra-companion-meta-check "$CLAIM_JSON" "$lease" "$REQUESTER_UID" "$VM2_NAME" "$DRA_NS/requester/main"
    dra-assert "$CLAIM_JSON" "[s['deviceClassName'] for s in subrequests()] == ['$class']"
    dra-assert "$CLAIM_JSON" "[(c['driver'], c['parameters'].get('apiVersion'), c['parameters'].get('kind'), sorted(set(c['parameters']) - {'apiVersion', 'kind'}), config_section(c, 'device').get('mode'), bool(config_section(c, 'device').get('readOnly'))) for c in configs()] == [('$DRA_POOL_DRIVER', '$DRA_CONFIG_API', '$DRA_CONFIG_KIND', ['device'], '$mode', False)]"
    claim_uid=$(dra-value "$CLAIM_JSON" "j['metadata']['uid']")
    pool-host-client -o json devices "$dev" || command-error "cannot get $dev"
    pool-assert "$COMMAND_OUTPUT" "[(a['host'], a.get('owner'), a['state']) for a in j['attachments']] == [('$VM2_NAME', 'k8s:resourceclaim/$claim_uid', 'attached')]"
    # The companion pod uses nothing of its claim: no CDI device.
    [ -z "$(dra-cdi-devices-with "$VM2" "$claim_uid")" ] ||
        error "lease $lease: the companion claim has CDI devices: $(dra-cdi-devices-with "$VM2" "$claim_uid")"
}

nothing-left() {
    # Usage: nothing-left LABEL [no-requester]
    #
    # Wait until no lease has anything left anywhere: no companion pods, no
    # cxl-lease-* claims, no CXL memory devices or regions in the VM, no
    # attachments in the pool, no cxl-type3 device in qemu, and (unless
    # no-requester) no leases in "cxl-request list" of the requester.
    local label="$1"
    dra-wait "$VM2" "$label-no-companions" "pods -n $DRA_NS -l $DRA_LEASE_LABEL" "items == []" 90
    dra-wait "$VM2" "$label-no-lease-claims" "resourceclaims -n $DRA_NS" "not any(n.startswith('cxl-lease-') for n in names())" 90
    pool-cxl-wait "$VM2" "$label" "memdevs == [] and regions == []" 90
    retry-until --timeout 60 --message "no attachments in the pool" \
        'pool-host-client -o json attachments >/dev/null && pool-py assert "$COMMAND_OUTPUT" "j in (None, [])"' ||
        error "$label: the pool still has attachments: $COMMAND_OUTPUT"
    pool-host-client -o json devices || command-error "cannot list the devices of the pool"
    pool-assert "$COMMAND_OUTPUT" "sorted((d['name'], d['state']) for d in j if d['name'] in ('pooled0', 'shared0')) == [('pooled0', 'free'), ('shared0', 'free')]"
    pool-vm-monitor "$VM2" "info qtree -b" | grep -q 'dev: cxl-type3' &&
        error "$label: qemu of the VM still has a CXL memory device"
    if [ "$2" != "no-requester" ]; then
        dra-leases "$VM2" requester/main
        dra-assert "$DRA_LEASES" "j == []"
    fi
    echo "$label: nothing left"
}

lend-memory() {
    # Usage: lend-memory
    #
    # Container main asks for 512Mi of pooled memory and gets pooled0 as
    # NUMA node MEM_NODE, lease MEM_LEASE, in its cpuset.mems only. It puts
    # TOUCH_MIB MiB there (dra-touch-start). The lease has the environment of
    # pooled0 (lease-env-check).
    local json anon pages
    echo "--- cxl-request request --source pool --size 512Mi"
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" request --source pool --size 512Mi --wait 120s ||
        command-error "cxl-request request --source pool --size 512Mi failed"
    json=$(dra-last-json "$COMMAND_OUTPUT") || error "cxl-request printed no JSON response"
    # Exclusive, read-write NUMA memory: the defaults.
    dra-assert "$json" "j.get('state') == 'Ready' and j.get('source') == 'pool' and j.get('mode') == 'numa' and not j.get('share') and not j.get('readOnly')"
    dra-assert "$json" "j.get('serial') == '$POOLED0_SERIAL' and int(j['size']) == $POOLED0_SIZE and int(j['numaNode']) >= $CPU_NODES and not j.get('dax')"
    MEM_LEASE=$(dra-value "$json" "j['lease']")
    MEM_NODE=$(dra-value "$json" "j['numaNode']")
    dra-lease-new "$MEM_LEASE"
    echo "lease $MEM_LEASE, pooled memory is NUMA node $MEM_NODE"
    # The response has the environment that a claim of pooled0 gives.
    dra-assert "$json" "j.get('env') == {'CXL_${POOLED0_HEX}_SERIAL': '$POOLED0_SERIAL', 'CXL_${POOLED0_HEX}_SHARED': '0', 'CXL_${POOLED0_HEX}_MODE': 'numa', 'CXL_${POOLED0_HEX}_SIZE': '$POOLED0_SIZE', 'CXL_${POOLED0_HEX}_NUMA_NODE': '$MEM_NODE'}"
    companion-check "$MEM_LEASE" pooled0 cxl-pool numa
    dra-assert "$CLAIM_JSON" "[quantity(s['capacity']['requests']['memory']) for s in subrequests()] == [$POOLED0_SIZE]"
    # The node is the region of pooled0, onlined as system RAM.
    pool-cxl-wait "$VM2" lent \
        "by_serial($POOLED0_SERIAL) is not None and [(r['numaNode'], online_size(r)) for r in regions if sorted(r['memdevs']) == [by_serial($POOLED0_SERIAL)['name']]] == [($MEM_NODE, $POOLED0_SIZE)]"
    # Lent to container main: the driver wrote its DRAM nodes and the
    # new node to cpuset.mems of main (through NRI, so that containerd keeps
    # it), and nothing to the other container of the pod.
    dra-numa-nodes-eq "$(cgroup-mems main cpuset.mems)" "$DRAM_NODES,$MEM_NODE" ||
        error "cpuset.mems of container main is not $DRAM_NODES,$MEM_NODE"
    dra-numa-nodes-has "$(cgroup-mems main)" "$MEM_NODE" ||
        error "node $MEM_NODE is not in cpuset.mems.effective of container main"
    dra-numa-nodes-has "$(dra-mems-allowed "$VM2" requester/main)" "$MEM_NODE" ||
        error "node $MEM_NODE is not in Mems_allowed_list of container main"
    # No other container has the node: neither container idle of the same
    # pod nor the companion nor any container of the control plane. The
    # driver keeps every container without a claim or a lend of the
    # node off it, also those that inherit their nodes (an empty
    # cpuset.mems).
    dra-mems-fenced "$VM2" "$MEM_NODE" "$DRA_NS/requester/main"
    dra-mems-once lent "$VM2" requester idle "$DRAM_NODES" IDLE_MEMS
    dra-numa-nodes-has "$(dra-mems-allowed "$VM2" requester/idle)" "$MEM_NODE" &&
        error "node $MEM_NODE is in Mems_allowed_list of container idle"
    # The idle container cannot even prefer the node: it is not its node.
    dra-exec "$VM2" requester/idle python3 /tools/numa-touch.py 16 "$MEM_NODE" &&
        error "container idle could allocate memory on node $MEM_NODE"
    grep -q "set_mempolicy.*Invalid argument" <<< "$COMMAND_OUTPUT" ||
        error "numa-touch.py in idle failed for another reason than an invalid NUMA node"
    dra-leases "$VM2" requester/main
    dra-assert "$DRA_LEASES" "[(l['lease'], l['state'], l.get('source'), l.get('mode'), bool(l.get('share'))) for l in j] == [('$MEM_LEASE', 'Ready', 'pool', 'numa', False)]"
    lease-env-check
    # Container main uses the memory: 256 MiB on the lent node.
    dra-touch-start "$VM2" requester/main "$TOUCH_MIB" "$MEM_NODE"
    pages=$(dra-touch-log-pages "$DRA_TOUCH_LOG" "$MEM_NODE")
    [ "${pages:-0}" -ge $(( (TOUCH_MIB - 16) << 8 )) ] ||
        error "only ${pages:-0} pages of numa-touch.py are on node $MEM_NODE"
    anon=$(dra-container-anon "$VM2" requester main "$MEM_NODE")
    echo "memory.numa_stat of container main: anon N$MEM_NODE=$anon"
    [ "${anon:-0}" -gt $(( 200 << 20 )) ] ||
        error "container main has ${anon:-0} bytes anon on node $MEM_NODE, expected > 200 MiB"
}

lease-env-check() {
    # Usage: lease-env-check
    #
    # The environment of the NUMA lease MEM_LEASE of pooled0: "cxl-request
    # env" prints it, sorted: the variables of pooled0 that a claim gives
    # (test10) and CXL_LEASE; "env --exec" runs a program with them, no fd.
    # LEASE_ENV and LEASE_NODE are those of the lease.
    local key="CXL_${POOLED0_HEX}_" script
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" env --lease "$MEM_LEASE" ||
        command-error "cxl-request env --lease $MEM_LEASE failed"
    dra-env-assert "lease $MEM_LEASE" "$COMMAND_OUTPUT" "$POOLED0_HEX" \
        SERIAL="$POOLED0_SERIAL" SHARED=0 MODE=numa SIZE="$POOLED0_SIZE" NUMA_NODE="$MEM_NODE"
    [ "$COMMAND_OUTPUT" == "$(LC_ALL=C sort <<< "$COMMAND_OUTPUT")" ] ||
        error "cxl-request env did not print sorted KEY=VALUE lines"
    grep -qx "CXL_LEASE=$MEM_LEASE" <<< "$COMMAND_OUTPUT" ||
        error "cxl-request env did not print CXL_LEASE=$MEM_LEASE"
    [ "$(grep -c . <<< "$COMMAND_OUTPUT")" == 6 ] ||
        error "cxl-request env printed more than the environment of pooled0 and CXL_LEASE"
    LEASE_ENV=$(grep "^$key" <<< "$COMMAND_OUTPUT")
    LEASE_NODE="$MEM_NODE"
    # The script, expanded by sh in the container:
    # echo "lease=$CXL_LEASE node=$CXL_<KEY>_NUMA_NODE size=$CXL_<KEY>_SIZE fd=${CXL_<KEY>_FD:-none}"
    script="echo \"lease=\$CXL_LEASE node=\$${key}NUMA_NODE size=\$${key}SIZE fd=\${${key}FD:-none}\""
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" env --lease "$MEM_LEASE" --exec sh -c "$script" ||
        command-error "cxl-request env --lease $MEM_LEASE --exec failed"
    [ "$(tail -n 1 <<< "$COMMAND_OUTPUT")" == "lease=$MEM_LEASE node=$MEM_NODE size=$POOLED0_SIZE fd=none" ] ||
        error "the program of env --exec does not have lease=$MEM_LEASE node=$MEM_NODE size=$POOLED0_SIZE and no fd"
}

give-back() {
    # Usage: give-back
    #
    # Container main releases lease MEM_LEASE while numa-touch.py holds its
    # memory there. The pages move to DRAM, the process lives on, and the
    # device goes back to the pool.
    echo "--- cxl-request release --lease $MEM_LEASE"
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" release --lease "$MEM_LEASE" ||
        command-error "cxl-request release --lease $MEM_LEASE failed"
    dra-assert "$(dra-last-json "$COMMAND_OUTPUT")" "j.get('lease') == '$MEM_LEASE' and j.get('state') == 'Released'"
    retry-until --timeout 30 --interval 1 --message "no anonymous memory of container main on node $MEM_NODE" \
        '[ "$(dra-container-anon "$VM2" requester main "$MEM_NODE")" == 0 ]' ||
        error "container main still has memory on node $MEM_NODE after the release"
    echo "memory.numa_stat of container main: $(dra-container-read "$VM2" requester main memory.numa_stat | tr '\n' ' ')"
    dra-touch-pages "$MEM_NODE"
    [ "$DRA_TOUCH_PAGES" == 0 ] || error "numa-touch.py still has $DRA_TOUCH_PAGES pages on node $MEM_NODE"
    echo "numa-touch.py (pid $DRA_TOUCH_PID) lives, its pages moved off node $MEM_NODE"
    dra-numa-nodes-eq "$(cgroup-mems main cpuset.mems)" "$DRAM_NODES" ||
        error "cpuset.mems of container main is not back to $DRAM_NODES"
    dra-numa-nodes-eq "$(dra-mems-allowed "$VM2" requester/main)" "$DRAM_NODES" ||
        error "Mems_allowed_list of container main is not back to $DRAM_NODES"
    # Nobody has the node now, and container idle keeps its nodes.
    dra-mems-fenced "$VM2" "$MEM_NODE"
    dra-mems-once given-back "$VM2" requester idle "$DRAM_NODES" IDLE_MEMS
    nothing-left released
    dra-touch-stop
}

share() {
    # Usage: share
    #
    # Container main joins the shared device (one share of it) and writes
    # a string to it through the fd that the driver passed it, reads
    # it back through the fd again, and releases. No /dev/dax device in the
    # container, no change in its device cgroup.
    local tag="$TAG" lease lease_env shared_file found dax devices majmin
    echo "--- cxl-request request --serial $SHARED_SERIAL --share --exec dax-rw.py --fd 3 write"
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" request --serial "$SHARED_SERIAL" --share --wait 120s \
        --exec python3 /tools/dax-rw.py --fd 3 write 0 "$tag" ||
        command-error "cxl-request request --serial $SHARED_SERIAL --share --exec ... write failed"
    dra-leases "$VM2" requester/main
    # --share: mode dax, the default for shared memory, read-write.
    dra-assert "$DRA_LEASES" "[(l['state'], l.get('source'), l.get('mode'), bool(l.get('share')), bool(l.get('readOnly')), l.get('serial')) for l in j] == [('Ready', 'pool', 'dax', True, False, '$SHARED_SERIAL')]"
    lease=$(dra-value "$DRA_LEASES" "j[0]['lease']")
    dra-lease-new "$lease"
    echo "lease $lease"
    companion-check "$lease" shared0 cxl-pool-shared dax
    # One share of the device: --share requests shares: "1" (without it
    # the claim would take all shares, the device exclusively).
    dra-assert "$CLAIM_JSON" "[s.get('capacity') for s in subrequests()] == [{'requests': {'shares': '1'}}] and quantity(j['status']['allocation']['devices']['results'][0].get('consumedCapacity', {}).get('shares', 0)) == 1"
    # In the VM the device is a devdax region, never system RAM.
    pool-cxl-wait "$VM2" shared \
        "by_serial($SHARED_SERIAL) is not None and len(regions) == 1 and enabled(regions[0]) and online_size(regions[0]) == 0"
    # The bytes are in the backing file of shared0 on the host.
    pool-host-client -o json devices shared0 || command-error "cannot get shared0"
    shared_file=$(pool-value "$COMMAND_OUTPUT" 'j["path"]')
    found=$(pool-file-string "$shared_file" 0)
    [ "$found" == "$tag" ] || error "host: $shared_file has '$found' at 0, expected '$tag'"
    echo "host read at 0 of $shared_file: '$found'"
    # The environment of the lease names the dax device of the VM, as a
    # claim of shared0 would (test19): read-write, shared.
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" env --lease "$lease" ||
        command-error "cxl-request env --lease $lease failed"
    lease_env="$COMMAND_OUTPUT"
    dra-env-assert "lease $lease" "$lease_env" "$SHARED_HEX" \
        SERIAL="$SHARED_SERIAL" SHARED=1 MODE=dax SIZE="$SHARED_SIZE" "DAX=/dev/dax*" READ_ONLY=0
    dax=$(dra-env-value "$lease_env" "CXL_${SHARED_HEX}_DAX")
    dra-assert "$DRA_LEASES" "j[0].get('dax') == '$dax'"
    # The fd again, for another program: its environment has the fd 3
    # instead of the device, and the lease.
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" env --lease "$lease" --exec \
        sh -c 'env | grep ^CXL_ | sort; readlink /proc/self/fd/3' ||
        command-error "cxl-request env --lease $lease --exec sh failed"
    dra-env-assert "env --lease $lease --exec" "$COMMAND_OUTPUT" "$SHARED_HEX" \
        SERIAL="$SHARED_SERIAL" SHARED=1 MODE=dax SIZE="$SHARED_SIZE" FD=3 READ_ONLY=0
    grep -qx "CXL_LEASE=$lease" <<< "$COMMAND_OUTPUT" ||
        error "the environment of env --exec has no CXL_LEASE=$lease"
    [ "$(tail -n 1 <<< "$COMMAND_OUTPUT")" == "$dax" ] ||
        error "fd 3 is not $dax"
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" env --lease "$lease" --exec \
        python3 /tools/dax-rw.py --fd 3 read 0 ||
        command-error "cxl-request env --lease $lease --exec ... read failed"
    [ "$(tail -n 1 <<< "$COMMAND_OUTPUT")" == "$tag" ] ||
        error "read '$(tail -n 1 <<< "$COMMAND_OUTPUT")' through the fd, expected '$tag'"
    # Nothing of this is a device of the container.
    # shellcheck disable=SC2016 # expanded by sh in the container
    dra-exec "$VM2" requester/main sh -c 'ls /dev | grep -c "^dax" || true'
    [ "$COMMAND_OUTPUT" == 0 ] || error "container main has /dev/dax* devices"
    devices=$(dra-container-devices "$VM2" requester main)
    echo "device cgroup of container main: $devices"
    [ "$(head -n 1 <<< "$devices")" == "$(head -n 1 <<< "$DEVICES_BEFORE")" ] ||
        error "the device rules of container main changed: $devices, before: $DEVICES_BEFORE"
    # The device cgroup still denies the device: a node made with mknod
    # cannot be opened, while the passed fd works.
    vm-command "cat /sys/bus/dax/devices/${dax#/dev/}/dev" || command-error "no sysfs entry of $dax"
    majmin="$COMMAND_OUTPUT"
    # shellcheck disable=SC2016 # expanded by sh in the container
    dra-exec "$VM2" requester/main sh -c 'mknod "/tmp/$1" c "$2" "$3" && echo "mknod ok" && python3 /tools/dax-rw.py read "/tmp/$1" 0' sh "${dax#/dev/}" "${majmin%:*}" "${majmin#*:}" &&
        error "container main could open $dax ($majmin) through a node of its own"
    { grep -q "^mknod ok" <<< "$COMMAND_OUTPUT" && grep -qE "Operation not permitted|Permission denied" <<< "$DRA_EXEC_STDERR"; } ||
        error "opening $dax through mknod failed for another reason than the device cgroup"
    dra-exec "$VM2" requester/main rm -f "/tmp/${dax#/dev/}"
    echo "the device cgroup of container main denies $dax ($majmin), the fd works"
    dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" release --all ||
        command-error "cxl-request release --all failed"
    nothing-left shared-released
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

echo "### preconditions: fake-cxl-pool-server on the host, one exclusive and one shared device"
pool-server-start "
  - name: pooled0
    size: 512Mi
    shared: false
    serial: $POOLED0_SERIAL
  - name: shared0
    size: 256Mi
    shared: true
    serial: $SHARED_SERIAL"
pool-client-install "$VM2"

echo "### preconditions: the pool controller and the driver (with cxl-request) run in the cluster"
dra-build
dra-install
dra-pull-images "$VM2" "$DRA_TOOLS_IMAGE" "$DRA_COMPANION_IMAGE"
dra-tools-install "$VM2"
dra-trace-start "$VM2"

echo "### A: a pod that may want CXL memory later"
# The lease device: published by the driver with the memory of the
# node, no capacity, any number of claims, only a type.
dra-json "$VM2" deviceclass cxl-lease
dra-assert "$COMMAND_OUTPUT" "'\"lease\"' in j['spec']['selectors'][0]['cel']['expression'] and '\"$DRA_LOCAL_DRIVER\"' in j['spec']['selectors'][0]['cel']['expression']"
dra-json "$VM2" resourceslices
LEASE_DEV="device('lease', '$DRA_LOCAL_DRIVER', '$VM2_NAME')"
dra-assert "$COMMAND_OUTPUT" "$LEASE_DEV is not None and attr($LEASE_DEV, 'type') == 'lease' and ${LEASE_DEV}.get('allowMultipleAllocations') == True and not ${LEASE_DEV}.get('capacity')"
dra-assert "$COMMAND_OUTPUT" "[k.split('/')[-1] for k in ${LEASE_DEV}['attributes']] == ['type']"
dra-apply "$VM2" <<< "$(dra-lease-template-yaml)
---
$(dra-pod-yaml requester "$VM2_NAME" cxl=template:cxl-lease -- main=cxl idle)"
# No pool device is involved: the pod starts right away.
dra-pod-wait "$VM2" requester Running 60
dra-json "$VM2" pod -n "$DRA_NS" requester
REQUESTER_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
LEASE_CLAIM=$(dra-value "$COMMAND_OUTPUT" "j['status']['resourceClaimStatuses'][0]['resourceClaimName']")
dra-json "$VM2" resourceclaim -n "$DRA_NS" "$LEASE_CLAIM"
dra-assert "$COMMAND_OUTPUT" "[(r['driver'], r['device']) for r in results()] == [('$DRA_LOCAL_DRIVER', 'lease')] and allocated_node() == '$VM2_NAME'"
LEASE_CLAIM_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
# The socket of the claim, in the VM and in container main.
vm-command "ls -l $DRA_LEASE_DIR/$LEASE_CLAIM_UID/ && test -S $DRA_LEASE_DIR/$LEASE_CLAIM_UID/cxl.sock" ||
    command-error "no lease socket of claim $LEASE_CLAIM in $DRA_LEASE_DIR/$LEASE_CLAIM_UID"
# shellcheck disable=SC2016 # expanded by sh in the container
dra-exec "$VM2" requester/main sh -c 'echo "$CXL_LEASE_SOCKET"; test -S /run/cxl/cxl.sock && test -x /usr/local/bin/cxl-request && echo ok' ||
    command-error "container main has no lease socket or cxl-request"
[ "$COMMAND_OUTPUT" == "/run/cxl/cxl.sock
ok" ] || error "container main: expected CXL_LEASE_SOCKET=/run/cxl/cxl.sock, a socket there, and cxl-request"
# The CDI device of the claim: the socket directory (read-only), the
# directory of the filesystems of fs leases (read-only, a slave of the
# host's directory, so that a mount the driver makes there later appears
# in the running container) and the CLI.
dra-lease-cdi-assert "$VM2" "$LEASE_CLAIM_UID"
# The container cannot write the host directory of its socket, and the
# socket still works.
dra-exec "$VM2" requester/main touch /run/cxl/x &&
    error "container main could create a file in /run/cxl, the read-only directory of its lease socket"
grep -qF "Read-only file system" <<< "$COMMAND_OUTPUT
$DRA_EXEC_STDERR" || error "creating a file in /run/cxl failed for another reason than \"Read-only file system\": $DRA_EXEC_STDERR"
vm-command "test ! -e $DRA_LEASE_DIR/$LEASE_CLAIM_UID/x" || error "the write in /run/cxl reached $DRA_LEASE_DIR/$LEASE_CLAIM_UID on the node"
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "j == []"
vm-command "ls -la $DRA_LEASE_FS_DIR/$LEASE_CLAIM_UID/" ||
    command-error "no filesystem directory of claim $LEASE_CLAIM in $DRA_LEASE_FS_DIR"
# In container main an empty read-only directory, mounted as a slave.
# shellcheck disable=SC2016 # expanded by sh in the container
dra-exec "$VM2" requester/main sh -c 'test -d "$1" && ls -A "$1" | wc -l && grep " $1 " /proc/self/mountinfo' sh "$DRA_LEASE_FS_CTR_DIR" ||
    command-error "container main has no directory $DRA_LEASE_FS_CTR_DIR"
[ "$(head -n 1 <<< "$COMMAND_OUTPUT" | tr -d ' ')" == 0 ] || error "container main: $DRA_LEASE_FS_CTR_DIR is not empty"
grep -qE " $DRA_LEASE_FS_CTR_DIR ro[, ].* master:[0-9]+" <<< "$COMMAND_OUTPUT" ||
    error "container main: $DRA_LEASE_FS_CTR_DIR is not a read-only slave mount"
dra-exec "$VM2" requester/main touch "$DRA_LEASE_FS_CTR_DIR/x" &&
    error "container main could create a file in $DRA_LEASE_FS_CTR_DIR"
grep -qF "Read-only file system" <<< "$DRA_EXEC_STDERR" ||
    error "creating a file in $DRA_LEASE_FS_CTR_DIR failed for another reason than a read-only file system"
# Container idle has no claim: no socket, no CLI, no filesystem directory.
dra-exec "$VM2" requester/idle sh -c "test -e /run/cxl/cxl.sock || test -e /usr/local/bin/cxl-request || test -e $DRA_LEASE_FS_CTR_DIR" &&
    error "container idle has the lease socket, cxl-request or $DRA_LEASE_FS_CTR_DIR, it has no claim"
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "j == []"
# The container runs on DRAM only, and the pool has given nothing.
dra-numa-nodes-eq "$(dra-mems-allowed "$VM2" requester/main)" "$DRAM_NODES" || error "container main: Mems_allowed_list is not $DRAM_NODES"
IDLE_MEMS=$(cgroup-mems idle cpuset.mems)
echo "cpuset.mems of container idle: '$IDLE_MEMS'"
DEVICES_BEFORE=$(dra-container-devices "$VM2" requester main) ||
    error "cannot read the device cgroup of container main"
echo "device cgroup of container main: $DEVICES_BEFORE"
pool-host-client -o json attachments || command-error "cannot list the attachments of the pool"
pool-assert "$COMMAND_OUTPUT" "j in (None, [])"
pool-cxl-dump "$VM2" requester-running
cxl-assert 'memdevs == [] and regions == []'

echo "### A2: requests that cxl-request refuses, and some that the driver refuses"
# cxl-request checks its options before it connects (exit status 2; its
# unit tests have every check), the driver checks them again and
# knows the devices: pooled0 is not shared, shared0 is not persistent.
# Nothing of a refused request is left.
dra-request-refused "$VM2" requester/main 2 "shared memory is never onlined as RAM: use mode dax" request --share --mode numa --size 64Mi
dra-request-refused "$VM2" requester/main 1 "memdev $POOLED0_SERIAL: not shareable" request --serial "$POOLED0_SERIAL" --share
# shared0 is volatile: it has no filesystem and no block device. mode fs,
# read-only by default or with --read-only=false, passes cxl-request and
# reaches the driver.
dra-request-refused "$VM2" requester/main 1 "memdev $SHARED_SERIAL: mode fs needs a persistent device" request --serial "$SHARED_SERIAL" --share --read-only --mode fs
dra-request-refused "$VM2" requester/main 1 "memdev $SHARED_SERIAL: mode fs needs a persistent device" request --serial "$SHARED_SERIAL" --share --mode fs
dra-request-refused "$VM2" requester/main 1 "memdev $SHARED_SERIAL: mode fs needs a persistent device" request --serial "$SHARED_SERIAL" --share --mode fs --read-only=false --fs-type ext4
dra-request-refused "$VM2" requester/main 1 "memdev $SHARED_SERIAL: mode block needs a persistent device" request --serial "$SHARED_SERIAL" --mode block
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "j == []"
dra-json "$VM2" pods -n "$DRA_NS" -l "$DRA_LEASE_LABEL"
dra-assert "$COMMAND_OUTPUT" "items == []"

echo "### B: ask for 512Mi of pooled memory from inside the container"
lend-memory

echo "### C: give it back while using it"
give-back

echo "### C2: a claim of the same memory gives the same environment"
# A pod with a claim of pooled0 from its creation (test10): its container
# has the variables that cxl-request env printed for the lease of pooled0,
# each with the same value, the NUMA node that of the claim, and the
# marker of the allocation result of the claim.
dra-apply "$VM2" <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: pooled-memory
spec:
  devices:
    requests:
    - name: mem
      exactly:
        deviceClassName: cxl-pool
        capacity:
          requests:
            memory: 512Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: claimer
spec:
  nodeSelector:
    kubernetes.io/hostname: $VM2_NAME
  terminationGracePeriodSeconds: 2
  restartPolicy: Never
  resourceClaims:
  - name: mem
    resourceClaimName: pooled-memory
  containers:
  - name: main
    image: $DRA_TOOLS_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c", "env | grep ^CXL_ | sort; trap 'exit 0' TERM; sleep infinity & wait"]
    resources:
      claims:
      - name: mem
EOF
dra-pod-wait "$VM2" claimer Running 120
pool-cxl-wait "$VM2" claimer \
    "by_serial($POOLED0_SERIAL) is not None and [online_size(r) for r in regions if sorted(r['memdevs']) == [by_serial($POOLED0_SERIAL)['name']]] == [$POOLED0_SIZE]"
CLAIM_NODE=$(cxl-value "[r['numaNode'] for r in regions if sorted(r['memdevs']) == [by_serial($POOLED0_SERIAL)['name']]][0]")
dra-json "$VM2" resourceclaim -n "$DRA_NS" pooled-memory
CLAIMER_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
dra-kubectl "$VM2" "logs -n $DRA_NS claimer" || command-error "cannot get the log of claimer"
grep -qx "CXL_CLAIM_${CLAIMER_UID//-/_}_0=1" <<< "$COMMAND_OUTPUT" ||
    error "claimer has no marker CXL_CLAIM_${CLAIMER_UID//-/_}_0=1 of result 0 of its claim"
[ "$(grep -c '^CXL_' <<< "$COMMAND_OUTPUT")" == 6 ] ||
    error "claimer has other CXL_ variables than the five of $POOLED0_SERIAL and the marker of its result"
CLAIMER_ENV=$(grep "^CXL_${POOLED0_HEX}_" <<< "$COMMAND_OUTPUT")
dra-env-assert claimer "$CLAIMER_ENV" "$POOLED0_HEX" \
    SERIAL="$POOLED0_SERIAL" SHARED=0 MODE=numa SIZE="$POOLED0_SIZE" NUMA_NODE="$CLAIM_NODE"
[ "$(grep -v "_NUMA_NODE=" <<< "$CLAIMER_ENV")" == "$(grep -v "_NUMA_NODE=" <<< "$LEASE_ENV")" ] ||
    error "the claim of pooled0 gives
$CLAIMER_ENV
the lease of pooled0 gave (on node $LEASE_NODE)
$LEASE_ENV"
echo "the claim (node $CLAIM_NODE) and the lease (node $LEASE_NODE) of pooled0 have the same environment"
# The node of the claim is the claimer's only, as a lent node is the
# requester's: container main of the requester has none now.
dra-mems-fenced "$VM2" "$CLAIM_NODE" "$DRA_NS/claimer/main"
dra-delete "$VM2" pod claimer --grace-period=2 --wait=true --timeout=60s
dra-delete "$VM2" resourceclaim pooled-memory --wait=true --timeout=60s
nothing-left claimer-deleted

echo "### D: shared memory as a file descriptor"
share

echo "### E: the pod goes, and the directories of its lease claim"
dra-delete-pod "$VM2" requester
nothing-left pod-deleted no-requester
vm-command "test ! -e $DRA_LEASE_DIR/$LEASE_CLAIM_UID && test ! -e $DRA_LEASE_FS_DIR/$LEASE_CLAIM_UID" ||
    error "a directory of claim $LEASE_CLAIM is left in $DRA_LEASE_DIR or $DRA_LEASE_FS_DIR"
echo "leases of the test: ${DRA_LEASE_IDS[*]}"
dra-delete "$VM2" resourceclaimtemplate cxl-lease --wait=true --timeout=60s

dra-assert-clean "$VM2" "$VM1"

echo "### F: what every component did, in order"
dra-trace-stop
dra-trace-summary

echo "### cleanup"
dra-cleanup
pool-cleanup
trap - EXIT
pool-port-busy && error "port $POOL_PORT is still busy after stopping the server"
echo "trace: $TEST_OUTPUT_DIR/trace.txt, summary: $TEST_OUTPUT_DIR/trace-summary.txt"
