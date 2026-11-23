# Node-local CXL memory with Dynamic Resource Allocation: pods share and
# slice a local CXL memory device
#
# test10 and test19 give pods memory of devices that the pool attaches to
# the node on demand. A CXL memory device can also belong to the node
# itself: cabled to it, there from boot. nri-dra-driver-cxl offers such a
# local device whole, as device memdev-<serial> of the node's cxl.nri.io
# ResourceSlice (attributes type memdev, source local, shared false,
# serial, size, numaNode (resource.kubernetes.io) and memoryBlockSize;
# capacity memory in steps of the memory block size of the node, any
# number of claims), and carves
# a slice of the device for every claim of DeviceClass cxl-local that it
# prepares, a multiple of the block size, in the mode of the device section
# of the claim's opaque MemoryConfig:
# - mode numa (the default): the slice becomes movable system memory of
#   the NUMA node of the device. The node grows by the memory blocks of
#   every such claim and shrinks by them when the claim goes. The
#   containers of the claims get the node in their cpuset.mems and in
#   CXL_<KEY>_NUMA_NODE. No other container of the node has the node, also
#   after a restart of the driver. Steering is opt-in: without a container
#   section (steering policy none) a container is only fenced, the node in
#   its cpuset.mems, no memory policy, no memory.high; the workload places
#   its memory itself. With container.steering.policy the driver steers
#   the container's memory over its DRAM and CXL claims: it sets the memory
#   policy of the container's processes and their descendants, narrows
#   cpuset.mems to the nodes used so far and steps memory.high.
# - mode dax: the slice is a character device /dev/daxX.Y in the
#   containers of the claim (CXL_<KEY>_DAX), so containers get isolated
#   pieces of one device. Pods that reference the same ResourceClaim share
#   its slice. Dax slices add nothing to the NUMA node.
# Claims of both modes share one device. The first claim creates a region
# on the device, the last one destroys it. The ResourceSlice does not
# change meanwhile: what the device holds is in sysfs, not in its
# attributes.
#
# The local device here is cxl_memdev0 of the topology (1G, host bridge
# 0): 8 memory blocks of 128M in the VM. It is not present at boot: the
# test attaches it through fake-cxl-pool-server, where it is a device of
# scope "local" of this VM, and detaches it at the end. Claim sizes are
# derived from the block size that the VM reports. The pods run on this VM
# (VM2), the control plane of the cluster of the test; VM1
# (n4-cxl-shared-1) and VM0 (n4-cxl-shared-0), its workers, have no local
# device attached.
#
# What you will see: the device in the ResourceSlice; NUMA and dax
# claims on it at the same time, the NUMA node appearing with the first
# NUMA claim and growing with the next one by exactly its blocks, while
# dax claims (one rounded up to a block, one shared by two pods) take
# slices next to them, with data isolated between the slices; the node
# only in the containers of the NUMA claims, which are not steered,
# before and after a restart of the driver, and not in a pod created
# after the restart; the node shrinking block by block as the NUMA pods
# go, while the dax pods keep their data; the device back in its initial
# state (no region, memdev enabled, published as at the start); claims
# with invalid device and container sections that fail their Prepare; a
# read-only slice; a dax claim without an amount that gets all of the
# device; containers that opt in to steering, their memory placed for
# workloads that know nothing of NUMA: CXL only, also past its claim,
# DRAM only, DRAM and CXL interleaved, DRAM first along waypoints in
# small steps; containers whose seccomp profile refuses memory policies,
# not steered; one claim whose numa and dax results go to two
# containers, each getting only its own; a container whose claims
# disagree on the policy not created; and the trace summary of what
# every component did, in order.
#
# Run test00-up of n4-cxl-shared-1, -0 and -2 first: they create the VMs and
# make them one cluster. The DRA driver comes from its image, and its
# DeviceClasses from its chart; build the image first:
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
MiB=$(( 1 << 20 ))
LOCAL_CLASS=cxl-local  # a slice of a local memdev: numa (default) or dax (MemoryConfig device.mode)
DRAM_CLASS=cxl-dram    # DRAM of the node, for steering
TAG="dra-local-memdev $(date +%Y-%m-%dT%H:%M:%S.%N)"

no-region-device() {
    # Usage: no-region-device JSON
    #
    # Check that the ResourceSlices in JSON have no region device of this
    # node: the region of the local device is not published again as
    # node-local CXL memory.
    dra-assert "$1" "not any(attr(d, 'type') == 'region' for d in devices('$DRA_LOCAL_DRIVER', '$VM2_NAME'))"
}

initial-state() {
    # Usage: initial-state LABEL
    #
    # dra-local-initial-state with NUMA_NODE, and no CXL region at all in
    # the VM.
    dra-local-initial-state "$VM2" "$1" "$NUMA_NODE"
    cxl-assert "regions == []"
}

claim-env() {
    # Usage: claim-env POD
    #
    # Set CLAIM_ENV to the CXL_ variables of container main of pod POD (or
    # of POD/CONTAINER), and check that one of them is the marker of an
    # allocation result of a claim (CXL_CLAIM_<uid>_<n>=1).
    # shellcheck disable=SC2016 # expanded by sh in the container
    dra-exec "$VM2" "$1" sh -c 'env | grep ^CXL_ | sort' ||
        command-error "cannot read the environment of $1"
    CLAIM_ENV="$COMMAND_OUTPUT"
    [ "$(grep -cE '^CXL_CLAIM_[0-9a-f_]+_[0-9]+=1$' <<< "$CLAIM_ENV")" == 1 ] ||
        error "$1 has not one claim marker CXL_CLAIM_<uid>_<n>=1"
}

numa-env-check() {
    # Usage: numa-env-check POD BYTES
    #
    # Check the environment of the NUMA slice of the claim of pod POD: the
    # serial of the local device, its NUMA node NUMA_NODE, the size BYTES
    # of the slice, mode numa, not shared, and the claim marker, nothing
    # else.
    claim-env "$1"
    dra-env-assert "$1" "$CLAIM_ENV" "$DRA_LOCAL_HEX" \
        SERIAL="$DRA_LOCAL_SERIAL" SHARED=0 MODE=numa SIZE="$2" NUMA_NODE="$NUMA_NODE"
    [ "$(grep -c '^CXL_' <<< "$CLAIM_ENV")" == 6 ] || error "$1 has other CXL_ variables: $CLAIM_ENV"
}

slice-check() {
    # Usage: slice-check POD BYTES [ro]
    #
    # Check the dax slice of the claim of pod POD: the environment of
    # container main names the device, its size BYTES, the serial of the
    # local device, mode dax, not shared and READ_ONLY=1 with ro, else
    # READ_ONLY=0, and has the claim marker, nothing else; the device is a
    # character device in the container with the numbers of a dax device
    # of BYTES of the local device, bound to device_dax, in the VM.
    # SLICE_DAX is its path, SLICE_DEV its "major:minor".
    local pod="$1" size="$2" ro=0
    [ "$3" != ro ] || ro=1
    claim-env "$pod"
    dra-env-assert "$pod" "$CLAIM_ENV" "$DRA_LOCAL_HEX" \
        SERIAL="$DRA_LOCAL_SERIAL" SHARED=0 MODE=dax SIZE="$size" "DAX=/dev/dax*" READ_ONLY="$ro"
    [ "$(grep -c '^CXL_' <<< "$CLAIM_ENV")" == 7 ] || error "$pod has other CXL_ variables: $CLAIM_ENV"
    SLICE_DAX=$(dra-env-value "$CLAIM_ENV" "CXL_${DRA_LOCAL_HEX}_DAX")
    dra-exec "$VM2" "$pod" python3 -c 'import os, stat, sys; s = os.stat(sys.argv[1]); print("%s %d:%d" % ("c" if stat.S_ISCHR(s.st_mode) else "-", os.major(s.st_rdev), os.minor(s.st_rdev)))' "$SLICE_DAX" ||
        command-error "no $SLICE_DAX in $pod"
    [[ "$COMMAND_OUTPUT" == "c "* ]] || error "$SLICE_DAX is not a character device in $pod"
    SLICE_DEV=${COMMAND_OUTPUT#c }
    dra-memdev-sysfs "$VM2" "$DRA_LOCAL_SERIAL" || error "cannot read the sysfs of the local device"
    dra-assert "$DRA_MEMDEV" "[(d['size'], d['dev'], d['driver']) for d in dax_slices() if d['name'] == '${SLICE_DAX#/dev/}'] == [($size, '$SLICE_DEV', 'device_dax')]"
    echo "$pod: slice $SLICE_DAX ($SLICE_DEV), $size bytes"
}

dax-write() {
    # Usage: dax-write POD DEV TEXT
    #
    # Write TEXT at offset 1M of dax device DEV in pod POD.
    dra-exec "$VM2" "$1" python3 /tools/dax-rw.py write "$2" 1M "$3" ||
        command-error "cannot write to $2 in $1"
}

dax-read() {
    # Usage: dax-read POD DEV EXPECTED
    #
    # Read the string at offset 1M of dax device DEV in pod POD, and fail
    # unless it is EXPECTED.
    dra-exec "$VM2" "$1" python3 /tools/dax-rw.py read "$2" 1M ||
        command-error "cannot read $2 in $1"
    [ "$COMMAND_OUTPUT" == "$3" ] || error "$1 read '$COMMAND_OUTPUT' at 1M of $2, expected '$3'"
}

dram-cxl-template-yaml() {
    # Usage: dram-cxl-template-yaml NAME STEERING
    #
    # Print ResourceClaimTemplate NAME with two requests of driver
    # cxl.nri.io, "dram" for DRAM_REQ bytes of DeviceClass cxl-dram and
    # "cxl" for one memory block of the local device (numa), and one
    # MemoryConfig for both with the container section
    # "steering: {STEERING}".
    cat <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: $1
spec:
  spec:
    devices:
      requests:
      - name: dram
        exactly:
          deviceClassName: $DRAM_CLASS
          capacity:
            requests:
              memory: $(dra-mi "$DRAM_REQ")
      - name: cxl
        exactly:
          deviceClassName: $LOCAL_CLASS
          capacity:
            requests:
              memory: $(dra-mi "$BS")
      config:
      - opaque:
          driver: $DRA_LOCAL_DRIVER
          parameters: $(dra-memory-config "container: {steering: {$2}}")
EOF
}

steered-check() {
    # Usage: steered-check POD POLICY NODES
    #
    # Wait until the driver steers container main of pod POD with the
    # steering POLICY of its claim (see dra-memory-wait), and check that
    # the memory policy that it set on the main process has NUMA nodes and
    # every one of them is in NODES (a node list).
    local claim nodes
    claim=$(dra-pod-claim "$VM2" "$1") || error "pod $1 has no ResourceClaim"
    dra-memory-wait "$VM2" "$1" main steered "$2 from claim $DRA_NS/$claim request "
    [[ "$DRA_MEMPOLICY" == *:* ]] || error "$1: the memory policy of the main process '$DRA_MEMPOLICY' names no nodes"
    nodes=${DRA_MEMPOLICY##*:}
    dra-numa-nodes-subset "$nodes" "$3" ||
        error "$1: the memory policy of the main process is '$DRA_MEMPOLICY', expected nodes of $3"
}

touch-child() {
    # Usage: touch-child POD[/CONTAINER] MIB
    #
    # Run numa-touch.py as a child of the main process of the container
    # (dra-run): MIB MiB with the memory policy that it inherits, no
    # preference of its own. TOUCH_OUT is its output, TOUCH_CXL its pages
    # on NUMA_NODE, TOUCH_DRAM its pages on DRAM_NODES.
    dra-run "$VM2" "$1" "python3 /tools/numa-touch.py $2 -" ||
        error "numa-touch.py in $1 failed"
    TOUCH_OUT="$COMMAND_OUTPUT"
    TOUCH_CXL=$(dra-touch-log-pages "$TOUCH_OUT" "$NUMA_NODE")
    TOUCH_DRAM=$(dra-touch-log-pages "$TOUCH_OUT" "$DRAM_NODES")
    echo "$1: $2 MiB touched by a child of the main process: $(( TOUCH_CXL >> 8 )) MiB on CXL node $NUMA_NODE, $(( TOUCH_DRAM >> 8 )) MiB on DRAM nodes $DRAM_NODES"
}

seccomp-pod-yaml() {
    # Usage: seccomp-pod-yaml NAME ESCALATION
    #
    # Print ResourceClaimTemplate NAME for DRAM_REQ of DeviceClass
    # DRAM_CLASS with steering policy auto, and pod NAME on VM2 whose
    # container main has the seccomp profile RuntimeDefault and
    # allowPrivilegeEscalation ESCALATION (true or false; false gives the
    # process no_new_privs).
    local pod
    pod=$(dra-claim-pod-yaml "$1" template "$1" "$VM2_NAME")
    dra-claim-yaml template "$1" "$DRAM_CLASS" "$(dra-mi "$DRAM_REQ")" "container=steering: {policy: auto}"
    echo "---"
    echo "${pod/"    imagePullPolicy: IfNotPresent"/"    imagePullPolicy: IfNotPresent
    securityContext:
      seccompProfile: {type: RuntimeDefault}
      allowPrivilegeEscalation: $2"}"
}

result-index() {
    # Usage: result-index JSON REQUEST
    #
    # Print the index of the allocation result of REQUEST in the claim JSON.
    dra-value "$1" "[r['request'] for r in results()].index('$2')"
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

echo "### preconditions: the pool controller and the driver run in the cluster"
dra-build
dra-install
dra-pull-images "$VM2" "$DRA_TOOLS_IMAGE"
dra-tools-install "$VM2"
dra-trace-start "$VM2"

echo "### A: the local device is a resource of the node"
# Attaching the local device plugs it into its own slot under host bridge
# 0, as if it had been there from boot. The driver publishes it whole,
# with no region yet.
dra-local-device "$VM2"
LD="$DRA_LOCAL_LD"             # the device in the ResourceSlices (dra-py)
LREGIONS="$DRA_LOCAL_REGIONS"  # its regions in cxl-dump (cxl-py)
NUMA_NODE=""           # the NUMA node of the device, known when memory of it is online
# Every slice is whole memory blocks of the VM. The claims of the test
# are sized in blocks: numa-a 1, dax-1 half a block (it consumes 1),
# numa-b 2, shared-slice 1, all at the same time; two refused claims of
# a quarter block (1 each); the steering claims steer-cxl, steer-mix,
# steer-first and the numa and dax requests of split 1 each. The DRAM
# claims are a block each (DRAM_REQ, the conflicting ones half of it).
BS="$DRA_LOCAL_BLOCK_SIZE"
dra-local-blocks-require "$VM2" "$(basename "$TOPOLOGY_DIR")" 7
NUMA_A=$BS
DAX1_REQ=$(( BS / 2 ))
NUMA_B=$(( 2 * BS ))
SHARED=$BS
RO_REQ=$(( BS / 8 ))
BAD_REQ=$(( BS / 4 ))
TOUCH_MIB=$(( BS / 4 / MiB ))
DRAM_REQ=$BS                           # the DRAM quota of the steered claims
STEER_MIB=$(( BS / 2 / MiB ))          # what a steered workload touches, within its quota
FIRST_STEP=$(( BS / 4 ))               # maxStep of first-dram
FIRST_MIB=$(( (DRAM_REQ + BS / 2) / MiB ))   # steer-first: all of the DRAM quota, half of the CXL quota
PAST_MIB=$(( BS * 3 / 2 / MiB ))       # steer-cxl past its claim of a block
pool-cxl-dump "$VM2" local-attached
cxl-assert "by_serial($DRA_LOCAL_SERIAL) is not None and enabled(by_serial($DRA_LOCAL_SERIAL)) and by_serial($DRA_LOCAL_SERIAL)['ramSize'] == $DRA_LOCAL_SIZE"
cxl-assert "regions == []"
AFFINITY=$(cxl-value "by_serial($DRA_LOCAL_SERIAL)['numaAffinity']")
dra-kubectl "$VM2" "get resourceslices"
dra-json "$VM2" resourceslices
SLICES="$COMMAND_OUTPUT"
# A memdev of source local, with the attributes of a pool memdev (test10)
# except coherent, and those of a node-local device; volatile memory, not
# persistent (test21 has a persistent one). Nothing about its slices: no
# mode, no name of the memdev in the VM.
dra-assert "$SLICES" "attr($LD, 'type') == 'memdev' and attr($LD, 'source') == 'local' and attr($LD, 'shared') == False and attr($LD, 'persistent') == False"
dra-assert "$SLICES" "attr($LD, 'serial') == '$DRA_LOCAL_SERIAL' and attr($LD, 'size') == $DRA_LOCAL_SIZE"
dra-assert "$SLICES" "sorted(k.split('/')[-1] for k in ${LD}['attributes']) == ['memoryBlockSize', 'numaNode', 'persistent', 'serial', 'shared', 'size', 'source', 'type']"
# Host bridge 0 is on CPU node 0 (pxb-cxl numa_node=0).
dra-assert "$SLICES" "attr($LD, 'resource.kubernetes.io/numaNode') == $AFFINITY and $AFFINITY == 0"
# Capacity in steps of the memory block size, the smallest claim one
# block, the whole device for a request without an amount, any number of
# claims.
dra-assert "$SLICES" "attr($LD, 'memoryBlockSize') == $BS"
dra-assert "$SLICES" "quantity(capacity($LD, 'memory')) == $DRA_LOCAL_SIZE"
dra-assert "$SLICES" "quantity((request_policy($LD, 'memory') or {}).get('validRange', {}).get('min', 0)) == $BS and quantity(request_policy($LD, 'memory')['validRange'].get('step', 0)) == $BS"
dra-assert "$SLICES" "quantity(request_policy($LD, 'memory')['default']) == $DRA_LOCAL_SIZE"
dra-assert "$SLICES" "${LD}.get('allowMultipleAllocations') == True"
no-region-device "$SLICES"
dra-assert "$SLICES" "len(slices('$DRA_POOL_DRIVER')) == 1 and [d['name'] for d in devices('$DRA_POOL_DRIVER')] == ['pooled0']"
# The DeviceClasses of the driver select device kinds and carry no config:
# the mode is the MemoryConfig of a claim.
dra-json "$VM2" deviceclasses
dra-assert "$COMMAND_OUTPUT" "sorted(n for n in names() if n.startswith('cxl-')) == sorted([$(printf "'%s', " "${DRA_CLASSES[@]}")])"
dra-assert "$COMMAND_OUTPUT" "'\"memdev\"' in by_name('$LOCAL_CLASS')['spec']['selectors'][0]['cel']['expression'] and '\"$DRA_LOCAL_DRIVER\"' in by_name('$LOCAL_CLASS')['spec']['selectors'][0]['cel']['expression']"
dra-assert "$COMMAND_OUTPUT" "not any(by_name(n)['spec'].get('config') for n in [$(printf "'%s', " "${DRA_CLASSES[@]}")])"
# No memory of the device is online: the VM has only its DRAM nodes.
vm-command "cat /sys/devices/system/node/has_memory" || command-error "cannot read the memory nodes of the VM"
dra-numa-nodes-eq "$COMMAND_OUTPUT" "$DRAM_NODES" || error "memory nodes $COMMAND_OUTPUT before any claim, expected $DRAM_NODES"

echo "### B: NUMA and dax claims share the device, the NUMA node grows with the NUMA claims"
# numa-a has no MemoryConfig: mode numa is the default.
dra-claim-pod "$VM2" numa-a "$LOCAL_CLASS" "$(dra-mi "$NUMA_A")"
dra-pod-wait "$VM2" numa-a Running 120
dra-local-claim-check "$VM2" numa-a "$NUMA_A"
# The first claim creates a region on the device. Its slice goes online
# as movable memory of a NUMA node that the VM did not have: the node of
# the CXL window of host bridge 0.
dra-local-slices-wait "$VM2" numa-a "$NUMA_NODE" "numa:$NUMA_A"
NUMA_NODE=$(dra-value "$DRA_MEMDEV" "dax_slices('kmem')[0]['node']")
(( NUMA_NODE >= CPU_NODES )) || error "the NUMA slice is on node $NUMA_NODE, a CPU node"
echo "the local device is NUMA node $NUMA_NODE of the VM"
dra-local-numa-node-size "$VM2" numa-a "$NUMA_NODE" "$NUMA_A"
pool-cxl-wait "$VM2" numa-a-region "len($LREGIONS) == 1 and ${LREGIONS}[0]['numaNode'] == $NUMA_NODE and len(regions) == 1"
dra-local-unchanged "$VM2"
no-region-device "$COMMAND_OUTPUT"
numa-env-check numa-a "$NUMA_A"
dra-mems-wait "$VM2" numa-a main "$NUMA_NODE"
# The node is numa-a's only: every other container of the node, those of
# the control plane too, is kept off it.
dra-mems-fenced "$VM2" "$NUMA_NODE" "$DRA_NS/numa-a/main"
# Steering is opt-in: the claim of numa-a has no container section, so the
# driver only fences the container (the node in its cpuset.mems, above),
# it sets no memory policy and no memory.high. Default allocations of a
# child of its main process stay on DRAM, although the node is allowed.
dra-memory-wait "$VM2" numa-a main fenced "none (default)"
touch-child numa-a "$TOUCH_MIB"
[ "$TOUCH_CXL" -le $(( (TOUCH_MIB << 8) / 100 )) ] ||
    error "numa-a is not steered, yet $TOUCH_CXL pages of the default allocations of a child of its main process are on node $NUMA_NODE"
# Memory that numa-a allocates on the node is on the node.
dra-touch-start "$VM2" numa-a/main "$TOUCH_MIB" "$NUMA_NODE"
pages=$(dra-touch-log-pages "$DRA_TOUCH_LOG" "$NUMA_NODE")
[ "$pages" -ge $(( (TOUCH_MIB - 4) << 8 )) ] || error "only $pages pages of numa-touch.py in numa-a are on node $NUMA_NODE"
anon=$(dra-container-anon "$VM2" numa-a main "$NUMA_NODE")
echo "memory.numa_stat of numa-a: anon N$NUMA_NODE=$anon"
[ "${anon:-0}" -ge $(( (TOUCH_MIB - 4) * MiB )) ] || error "numa-a has ${anon:-0} bytes anon on node $NUMA_NODE, expected >= $(( TOUCH_MIB - 4 )) MiB"
dra-touch-stop
# A dax claim on the same device while numa-a runs: a slice of its own
# next to the NUMA slice. The scheduler rounds its half block up to a
# block, the step of the capacity, and the slice has that size. It adds
# nothing to the NUMA node.
dra-claim-pod "$VM2" dax-1 "$LOCAL_CLASS" "$(dra-mi "$DAX1_REQ")" dax
dra-pod-wait "$VM2" dax-1 Running 120
dra-local-claim-check "$VM2" dax-1 "$(dra-local-round "$DAX1_REQ")"
slice-check dax-1 "$(dra-local-round "$DAX1_REQ")"
DAX1_DAX="$SLICE_DAX"
DAX1_DEV="$SLICE_DEV"
dra-local-slices-wait "$VM2" numa-a-dax-1 "$NUMA_NODE" "numa:$NUMA_A" "dax:$(dra-local-round "$DAX1_REQ")"
dra-local-numa-node-size "$VM2" dax-1 "$NUMA_NODE" "$NUMA_A"
dra-local-unchanged "$VM2"
no-region-device "$COMMAND_OUTPUT"
# A second NUMA claim, with device.mode numa in its MemoryConfig: the node
# grows by its two blocks, both containers have the node.
dra-claim-pod "$VM2" numa-b "$LOCAL_CLASS" "$(dra-mi "$NUMA_B")" numa
dra-pod-wait "$VM2" numa-b Running 120
dra-local-claim-check "$VM2" numa-b "$NUMA_B"
dra-local-slices-wait "$VM2" numa-b "$NUMA_NODE" "numa:$NUMA_A" "dax:$(dra-local-round "$DAX1_REQ")" "numa:$NUMA_B"
dra-local-numa-node-size "$VM2" numa-b "$NUMA_NODE" $(( NUMA_A + NUMA_B ))
numa-env-check numa-b "$NUMA_B"
# A MemoryConfig with a device section only does not opt in to steering.
dra-memory-wait "$VM2" numa-b main fenced "none (default)"
dra-mems-wait "$VM2" numa-a main "$NUMA_NODE"
dra-mems-wait "$VM2" numa-b main "$NUMA_NODE"
# Both NUMA pods have the node, the dax pod does not.
dra-mems-fenced "$VM2" "$NUMA_NODE" "$DRA_NS/numa-a/main" "$DRA_NS/numa-b/main"
# One ResourceClaim referenced by two pods: prepared once, one slice for
# both.
dra-apply "$VM2" <<< "$(dra-claim-yaml claim shared-slice "$LOCAL_CLASS" "$(dra-mi "$SHARED")" dax)
---
$(dra-claim-pod-yaml dax-s1 claim shared-slice "$VM2_NAME")
---
$(dra-claim-pod-yaml dax-s2 claim shared-slice "$VM2_NAME")"
dra-pod-wait "$VM2" dax-s1 Running 120
dra-pod-wait "$VM2" dax-s2 Running 120
dra-local-claim-check "$VM2" dax-s1 "$SHARED"
dra-assert "$DRA_CLAIM_JSON" "j['metadata']['name'] == 'shared-slice' and sorted(r['name'] for r in j['status']['reservedFor']) == ['dax-s1', 'dax-s2']"
slice-check dax-s1 "$SHARED"
SHARED_DAX="$SLICE_DAX"
SHARED_DEV="$SLICE_DEV"
slice-check dax-s2 "$SHARED"
[ "$SLICE_DAX $SLICE_DEV" == "$SHARED_DAX $SHARED_DEV" ] ||
    error "dax-s2 has $SLICE_DAX ($SLICE_DEV), dax-s1 $SHARED_DAX ($SHARED_DEV): not the same slice"
[ "$SHARED_DEV" != "$DAX1_DEV" ] || error "shared-slice and dax-1 have the same device $SHARED_DEV"
dra-local-slices-wait "$VM2" four-slices "$NUMA_NODE" "numa:$NUMA_A" "dax:$(dra-local-round "$DAX1_REQ")" "numa:$NUMA_B" "dax:$SHARED"
# cxl-dump finds the node of a region of several slices.
pool-cxl-wait "$VM2" four-slices-node "len($LREGIONS) == 1 and ${LREGIONS}[0]['numaNode'] == $NUMA_NODE" 10
# The same offset of both dax slices: what one claim writes, only the
# pods of that claim see.
dax-write dax-s1 "$SHARED_DAX" "$TAG: shared-slice"
dax-write dax-1 "$DAX1_DAX" "$TAG: dax-1"
dax-read dax-s2 "$SHARED_DAX" "$TAG: shared-slice"
dax-read dax-1 "$DAX1_DAX" "$TAG: dax-1"
echo "two dax slices, two different strings at 1M: the slices are isolated, the shared slice is shared"
# Four claims, five pods on one device: the scheduler accounted every
# claim once, in whole blocks, and the slices take exactly that of the
# region (dra-local-slices-wait checked available_size).
TOTAL=$(( NUMA_A + $(dra-local-round "$DAX1_REQ") + NUMA_B + SHARED ))
dra-json "$VM2" resourceclaims -n "$DRA_NS"
LOCAL_CLAIMS="[c for c in items if any(r['device'] == '$DRA_LOCAL_DEVICE' for r in results(c))]"
dra-assert "$COMMAND_OUTPUT" "len($LOCAL_CLAIMS) == 4 and sum(consumed(c) for c in $LOCAL_CLAIMS) == $TOTAL"
dra-local-numa-node-size "$VM2" four-claims "$NUMA_NODE" $(( NUMA_A + NUMA_B ))
vmdir-command "$VM2" "daxctl list -R -D 2>/dev/null; ls -l /dev/dax*; numactl -H" || :

echo "### B2: a restart of the driver keeps the node to the NUMA pods, a new pod is kept off it"
# The new driver knows the node from the prepared NUMA claims. Its own
# container was created while no driver ran, with every node: the driver
# fences it like every other container once it runs. The slices stay.
dra-driver-restart "$VM2"
dra-driver-log-wait "$VM2" "NRI fence: fencing CXL NUMA nodes $NUMA_NODE of prepared claims"
dra-mems-fenced "$VM2" "$NUMA_NODE" "$DRA_NS/numa-a/main" "$DRA_NS/numa-b/main"
dra-local-slices-wait "$VM2" restarted "$NUMA_NODE" "numa:$NUMA_A" "dax:$(dra-local-round "$DAX1_REQ")" "numa:$NUMA_B" "dax:$SHARED"
# A pod without a claim, created after the restart, gets an explicit
# cpuset.mems without the node when its container is created: not even
# its first process has the node.
dra-apply "$VM2" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: plain
spec:
  nodeSelector:
    kubernetes.io/hostname: $VM2_NAME
  terminationGracePeriodSeconds: 2
  restartPolicy: Never
  containers:
  - name: main
    image: $DRA_TOOLS_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c", "grep Mems_allowed_list /proc/self/status; trap 'exit 0' TERM; sleep infinity & wait"]
EOF
dra-pod-wait "$VM2" plain Running 60
dra-driver-log-wait "$VM2" "NRI CreateContainer: ctr=$DRA_NS/plain/main: cpuset.mems="
PLAIN_MEMS=$(dra-driver-log "$VM2" | sed -n "s|.*NRI CreateContainer: ctr=$DRA_NS/plain/main: cpuset.mems=\([0-9,-]*\) .*|\1|p" | tail -n 1)
if [ -z "$PLAIN_MEMS" ] || dra-numa-nodes-has "$PLAIN_MEMS" "$NUMA_NODE"; then
    error "plain: the driver gave it cpuset.mems '$PLAIN_MEMS' at its creation, expected nodes without $NUMA_NODE"
fi
mems=$(dra-container-read "$VM2" plain main cpuset.mems) || error "cannot read cpuset.mems of plain"
dra-numa-nodes-eq "$mems" "$PLAIN_MEMS" ||
    error "plain: cpuset.mems is '$mems', the driver gave it '$PLAIN_MEMS' at its creation"
dra-wait-log "$VM2" plain "Mems_allowed_list:"$'\t'"$mems" 30
echo "plain: cpuset.mems $mems from its creation, also in Mems_allowed_list of its first process"
dra-mems-fenced "$VM2" "$NUMA_NODE" "$DRA_NS/numa-a/main" "$DRA_NS/numa-b/main"
dra-delete-pod "$VM2" plain

echo "### C: the NUMA pods go, the node shrinks by their blocks, the dax slices keep their data"
dra-delete-pod "$VM2" numa-a
dra-local-numa-node-size "$VM2" numa-a-deleted "$NUMA_NODE" "$NUMA_B"
dra-local-slices-wait "$VM2" without-numa-a "$NUMA_NODE" "dax:$(dra-local-round "$DAX1_REQ")" "numa:$NUMA_B" "dax:$SHARED"
dra-mems-wait "$VM2" numa-b main "$NUMA_NODE"
dax-read dax-1 "$DAX1_DAX" "$TAG: dax-1"
dax-read dax-s2 "$SHARED_DAX" "$TAG: shared-slice"
dra-local-unchanged "$VM2"
dra-delete-pod "$VM2" numa-b
dra-local-numa-node-size "$VM2" numa-b-deleted "$NUMA_NODE" 0
# The region stays for the dax slices. cxl-dump finds its node without
# any memory online, from the dax devices.
dra-local-slices-wait "$VM2" without-numa "$NUMA_NODE" "dax:$(dra-local-round "$DAX1_REQ")" "dax:$SHARED"
pool-cxl-wait "$VM2" without-numa-node "len($LREGIONS) == 1 and ${LREGIONS}[0]['numaNode'] == $NUMA_NODE" 10
dra-local-unchanged "$VM2"
no-region-device "$COMMAND_OUTPUT"
dax-read dax-1 "$DAX1_DAX" "$TAG: dax-1"
dax-read dax-s2 "$SHARED_DAX" "$TAG: shared-slice"
# The shared slice stays while a pod of its claim runs.
dra-delete-pod "$VM2" dax-s1
dra-wait "$VM2" shared-slice-reserved "resourceclaim -n $DRA_NS shared-slice" \
    "[r['name'] for r in j['status']['reservedFor']] == ['dax-s2']" 60
dra-local-slices-wait "$VM2" with-dax-s2 "$NUMA_NODE" "dax:$(dra-local-round "$DAX1_REQ")" "dax:$SHARED"
dax-read dax-s2 "$SHARED_DAX" "$TAG: shared-slice"
dra-delete-pod "$VM2" dax-1
dra-local-slices-wait "$VM2" without-dax-1 "$NUMA_NODE" "dax:$SHARED"
dax-read dax-s2 "$SHARED_DAX" "$TAG: shared-slice"
dra-delete-pod "$VM2" dax-s2
dra-delete "$VM2" resourceclaim shared-slice --wait=true --timeout=60s
# The last slice destroys the region.
initial-state released

echo "### D: refused configs, a read-only slice, a slice of the whole device"
# Claims whose MemoryConfig the driver refuses, one of a device
# section (readOnly is for dax slices) and one of a container section
# (fencing is set by the driver configuration only); the unit tests have
# the other refusals. The scheduler allocates the device, the Prepare
# fails, the user sees why in an Event, the pods stay ContainerCreating,
# and the device stays free.
BAD_PODS=(bad-config bad-fencing)
declare -A BAD_TEXT=(
    [bad-config]='MemoryConfig: device.readOnly needs mode dax'
    [bad-fencing]='MemoryConfig: container.fencing: set by the driver configuration only (defaults.container.fencing)'
)
dra-claim-pod "$VM2" bad-config "$LOCAL_CLASS" "$(dra-mi "$BAD_REQ")" numa read-only
dra-claim-pod "$VM2" bad-fencing "$LOCAL_CLASS" "$(dra-mi "$BAD_REQ")" "container=fencing: none"
for pod in "${BAD_PODS[@]}"; do
    dra-wait "$VM2" "$pod-event" "events -n $DRA_NS --field-selector involvedObject.name=$pod" \
        "any($(python3 -c 'import sys; print(repr(sys.argv[1]))' "${BAD_TEXT[$pod]}") in (e.get('message') or '') for e in items)" 120
    dra-kubectl "$VM2" "get events -n $DRA_NS --field-selector involvedObject.name=$pod"
    dra-wait "$VM2" "$pod-creating" "pod -n $DRA_NS $pod" \
        "[(cs.get('state') or {}).get('waiting', {}).get('reason') for cs in j['status'].get('containerStatuses') or []] == ['ContainerCreating']" 60
done
pool-cxl-dump "$VM2" bad-config
cxl-assert "regions == []"
dra-local-unchanged "$VM2"
for pod in "${BAD_PODS[@]}"; do
    dra-delete-pod "$VM2" "$pod"
done
initial-state bad-config-deleted
# A read-only slice: the claim config says mode dax and readOnly, the
# device node of the container can only be opened for reading. An eighth
# of a block consumes a block.
dra-claim-pod "$VM2" dax-ro "$LOCAL_CLASS" "$(dra-mi "$RO_REQ")" dax read-only
dra-pod-wait "$VM2" dax-ro Running 120
dra-local-claim-check "$VM2" dax-ro "$(dra-local-round "$RO_REQ")"
RO_UID="$DRA_CLAIM_UID"
slice-check dax-ro "$(dra-local-round "$RO_REQ")" ro
RO_DAX="$SLICE_DAX"
RO_DEV="$SLICE_DEV"
dra-local-slices-wait "$VM2" read-only "$NUMA_NODE" "dax:$(dra-local-round "$RO_REQ")"
dra-local-unchanged "$VM2"
dra-exec "$VM2" dax-ro python3 /tools/dax-rw.py check-ro "$RO_DAX" 0 ||
    error "dax-ro could open or map $RO_DAX for writing"
for line in "open for writing refused: EPERM" "writable mapping refused: EACCES"; do
    grep -qxF "$line" <<< "$COMMAND_OUTPUT" || error "dax-ro: check-ro did not print '$line'"
done
devices=$(dra-container-devices "$VM2" dax-ro main) || error "cannot read the device cgroup of dax-ro"
echo "device cgroup of dax-ro: $devices"
dra-assert "$(sed -n 's/^spec: //p' <<< "$devices")" \
    "[d.get('access') for d in j if d.get('allow') and d.get('major') == ${RO_DEV%:*} and d.get('minor') == ${RO_DEV#*:}] == ['r']"
# The CDI device of the allocation result of the claim, claim-<uid>-0 in
# the spec cxl-claims.json, has the device node with permissions r, the
# environment of the slice and the marker of the result,
# CXL_CLAIM_<uid>_0. There is no CDI device of the claim as a whole.
cdi=$(dra-cdi-device "$VM2" "claim-$RO_UID-0")
echo "CDI device claim-$RO_UID-0: $cdi"
[ -n "$cdi" ] || error "no CDI device claim-$RO_UID-0 in the VM"
dra-assert "$cdi" "j['kind'] == '$DRA_LOCAL_DRIVER/device' and any(n.get('permissions') == 'r' for n in j['deviceNodes'])"
dra-assert "$cdi" "'CXL_${DRA_LOCAL_HEX}_READ_ONLY=1' in j['env'] and 'CXL_CLAIM_${RO_UID//-/_}_0=1' in j['env']"
[ "$(dra-cdi-devices-with "$VM2" "$RO_UID")" == "claim-$RO_UID-0" ] ||
    error "the claim of dax-ro has other CDI devices than claim-$RO_UID-0: $(dra-cdi-devices-with "$VM2" "$RO_UID")"
dra-delete-pod "$VM2" dax-ro
initial-state ro-deleted
# A dax claim without an amount consumes the whole device (the default of
# the requestPolicy): one slice of all of it.
dra-claim-pod "$VM2" dax-whole "$LOCAL_CLASS" "" dax
dra-pod-wait "$VM2" dax-whole Running 120
dra-local-claim-check "$VM2" dax-whole "$DRA_LOCAL_SIZE"
slice-check dax-whole "$DRA_LOCAL_SIZE"
dra-local-slices-wait "$VM2" whole "$NUMA_NODE" "dax:$DRA_LOCAL_SIZE"
dra-delete-pod "$VM2" dax-whole
initial-state whole-deleted

echo "### E: steering is opt-in: CXL only, DRAM only, DRAM and CXL, DRAM first along waypoints"
# A container section with a steering policy in a claim opts in. Policy
# auto picks by the memory of the claims of the container: CXL only
# first-cxl, DRAM only first-dram, both start-interleaved. The driver sets
# the memory policy of the processes of the container when it starts and
# at every step, and their children inherit it: the workloads here run as
# children of the main process (dra-run), allocate without a NUMA
# preference of their own, and land where the plan of the driver says.
dra-claim-pod "$VM2" steer-cxl "$LOCAL_CLASS" "$(dra-mi "$BS")" "container=steering: {policy: auto, maxStep: $(dra-mi "$FIRST_STEP")}"
dra-claim-pod "$VM2" steer-dram "$DRAM_CLASS" "$(dra-mi "$DRAM_REQ")" "container=steering: {policy: auto}"
dra-apply "$VM2" <<< "$(dram-cxl-template-yaml steer-mix "policy: auto")
---
$(dra-claim-pod-yaml steer-mix template steer-mix "$VM2_NAME")
---
$(dram-cxl-template-yaml steer-first "policy: waypoints, maxStep: $(dra-mi "$FIRST_STEP"), waypoints: [{targetUsages: [{memoryType: DRAM, usage: $(dra-mi "$DRAM_REQ")}]}, {targetUsages: [{memoryType: CXL, usage: $(dra-mi "$BS")}]}]")
---
$(dra-claim-pod-yaml steer-first template steer-first "$VM2_NAME")"
for pod in steer-cxl steer-dram steer-mix steer-first; do
    dra-pod-wait "$VM2" "$pod" Running 120
done
dra-local-slices-wait "$VM2" steering "$NUMA_NODE" "numa:$BS" "numa:$BS" "numa:$BS"
dra-local-numa-node-size "$VM2" steering "$NUMA_NODE" $(( 3 * BS ))
# CXL only (first-cxl): the policy prefers the CXL node, the memory of the
# workload is there.
steered-check steer-cxl auto "$NUMA_NODE"
dra-mems-wait "$VM2" steer-cxl main "$NUMA_NODE"
touch-child steer-cxl "$STEER_MIB"
[ "$TOUCH_CXL" -ge $(( (STEER_MIB << 8) * 9 / 10 )) ] ||
    error "steer-cxl: only $TOUCH_CXL of $(( STEER_MIB << 8 )) pages of the workload are on CXL node $NUMA_NODE"
# DRAM only (first-dram): the policy names DRAM nodes, and the driver
# narrows cpuset.mems to them; the memory of the workload is on DRAM.
steered-check steer-dram auto "$DRAM_NODES"
mems=$(dra-container-read "$VM2" steer-dram main cpuset.mems) || error "cannot read cpuset.mems of steer-dram"
dra-numa-nodes-subset "$mems" "$DRAM_NODES" ||
    error "steer-dram: cpuset.mems is '$mems', expected it narrowed to DRAM nodes of $DRAM_NODES"
echo "steer-dram: cpuset.mems $mems"
touch-child steer-dram "$STEER_MIB"
[ "$TOUCH_DRAM" -ge $(( (STEER_MIB << 8) * 9 / 10 )) ] && [ "$TOUCH_CXL" -le $(( (STEER_MIB << 8) / 100 )) ] ||
    error "steer-dram: $TOUCH_DRAM pages of the workload on DRAM, $TOUCH_CXL on CXL node $NUMA_NODE, expected all on DRAM"
# DRAM and CXL (start-interleaved): the policy interleaves over DRAM and
# the CXL node, the memory of the workload is on both.
steered-check steer-mix auto "$DRAM_NODES,$NUMA_NODE"
if ! dra-numa-nodes-has "${DRA_MEMPOLICY##*:}" "$NUMA_NODE" || dra-numa-nodes-eq "${DRA_MEMPOLICY##*:}" "$NUMA_NODE"; then
    error "steer-mix: the memory policy of the main process is '$DRA_MEMPOLICY', expected DRAM nodes and CXL node $NUMA_NODE"
fi
touch-child steer-mix "$STEER_MIB"
[ "$TOUCH_DRAM" -ge $(( (STEER_MIB << 8) / 5 )) ] && [ "$TOUCH_CXL" -ge $(( (STEER_MIB << 8) / 5 )) ] ||
    error "steer-mix: $TOUCH_DRAM pages of the workload on DRAM, $TOUCH_CXL on CXL node $NUMA_NODE, expected at least a fifth on each"
# DRAM first along waypoints (the DRAM quota, then the CXL quota), in
# steps of FIRST_STEP: before the workload the policy and cpuset.mems name
# DRAM only. The workload takes the DRAM quota and goes on on CXL: the
# driver adds the node to cpuset.mems and moves the policy to it when the
# DRAM quota is used. The DRAM of the workload stays within the quota and
# one step.
steered-check steer-first waypoints "$DRAM_NODES"
mems=$(dra-container-read "$VM2" steer-first main cpuset.mems) || error "cannot read cpuset.mems of steer-first"
dra-numa-nodes-subset "$mems" "$DRAM_NODES" ||
    error "steer-first: cpuset.mems is '$mems' before the workload, expected DRAM nodes of $DRAM_NODES only"
touch-child steer-first "$FIRST_MIB"
# The plan counts the DRAM waypoint reached within one step (minStep =
# maxStep = FIRST_STEP), and the runner and python use about 10 MiB of the
# DRAM quota first: the workload gets the quota less 10-42 MiB on DRAM (98
# and 118 MiB in VM2), the rest on CXL (94 and 74 MiB). Bounds: DRAM at
# least the quota less a step and 16 MiB, CXL at least 3/4 of what the
# workload touches beyond the quota.
[ $(( TOUCH_DRAM >> 8 )) -ge $(( (DRAM_REQ - FIRST_STEP) / MiB - 16 )) ] && [ $(( TOUCH_CXL >> 8 )) -ge $(( (FIRST_MIB - DRAM_REQ / MiB) * 3 / 4 )) ] ||
    error "steer-first: $(( TOUCH_DRAM >> 8 )) MiB of the workload on DRAM, $(( TOUCH_CXL >> 8 )) MiB on CXL node $NUMA_NODE, expected the DRAM quota $(( DRAM_REQ / MiB )) MiB filled first, the rest on CXL"
[ $(( TOUCH_DRAM >> 8 )) -le $(( (DRAM_REQ + FIRST_STEP) / MiB )) ] ||
    error "steer-first: $(( TOUCH_DRAM >> 8 )) MiB of the workload on DRAM, more than the DRAM quota and a step, $(( (DRAM_REQ + FIRST_STEP) / MiB )) MiB"
dra-mems-wait "$VM2" steer-first main "$NUMA_NODE"

# Past its claim: steer-cxl holds PAST_MIB, more than its CXL quota of a
# block. Past the last waypoint of its plan its memory goes on to the CXL
# node, without a memory.high bound. When the memory is freed, the usage
# is below the last waypoint again by more than a step (FIRST_STEP) and
# the steering resumes with a bound.
dra-run "$VM2" steer-cxl "setsid python3 /tools/numa-touch.py --hold $PAST_MIB - > /tmp/hold.log 2>&1 < /dev/null &" ||
    error "cannot start numa-touch.py in steer-cxl"
# shellcheck disable=SC2016 # evaluated by retry-until
retry-until --timeout 120 --interval 1 --message "steer-cxl holds $PAST_MIB MiB" \
    'vmdir-command-q "$VM2" "kubectl exec -n $DRA_NS steer-cxl -c main -- cat /tmp/hold.log" | grep -qE "^numa-touch: (ready|.*failed)"' ||
    error "steer-cxl: numa-touch.py did not touch $PAST_MIB MiB"
dra-exec "$VM2" steer-cxl/main cat /tmp/hold.log
grep -q "^numa-touch: ready" <<< "$COMMAND_OUTPUT" || error "steer-cxl: numa-touch.py failed: $COMMAND_OUTPUT"
past=$(dra-touch-log-pages "$COMMAND_OUTPUT" "$NUMA_NODE")
[ "$past" -ge $(( (PAST_MIB << 8) * 9 / 10 )) ] ||
    error "steer-cxl: only $past of $(( PAST_MIB << 8 )) pages past its claim are on CXL node $NUMA_NODE"
dra-driver-log-wait "$VM2" "$DRA_NS/steer-cxl/main: past the last waypoint"
dra-memory-state "$VM2" steer-cxl main || error "cannot read memory.high of steer-cxl"
[ "$DRA_MEM_HIGH" == max ] || error "steer-cxl: memory.high is '$DRA_MEM_HIGH' past the last waypoint, expected max"
echo "steer-cxl: $(( past >> 8 )) MiB on CXL node $NUMA_NODE past its claim of $(( BS / MiB )) MiB, memory.high $DRA_MEM_HIGH"
dra-exec "$VM2" steer-cxl/main pkill -f numa-touch.py || error "cannot stop numa-touch.py in steer-cxl"
# shellcheck disable=SC2016 # evaluated by retry-until
retry-until --timeout 30 --interval 1 --message "steer-cxl is steered with a bound again" \
    'dra-memory-state "$VM2" steer-cxl main && [ "$DRA_MEM_HIGH" != max ]' ||
    error "steer-cxl: memory.high is '$DRA_MEM_HIGH' after its usage dropped below the last waypoint, expected a bound"
dra-driver-log-wait "$VM2" "$DRA_NS/steer-cxl/main: steering resumes"

# A process that refuses memory policies: the seccomp profile RuntimeDefault
# of containerd allows set_mempolicy only with CAP_SYS_NICE. The profile
# applies before the start of the container, or (no_new_privs) right
# before the exec of its workload. Either way the container is not
# steered: its steering does not start, or ends at the next step, which a
# workload beyond the DRAM quota brings. It is fenced, with memory.high
# max.
dra-apply "$VM2" <<< "$(seccomp-pod-yaml steer-seccomp true)
---
$(seccomp-pod-yaml steer-seccomp-nnp false)"
for pod in steer-seccomp steer-seccomp-nnp; do
    dra-pod-wait "$VM2" "$pod" Running 120
    touch-child "$pod" $(( (DRAM_REQ + BS / 4) / MiB ))
    # shellcheck disable=SC2016 # evaluated by retry-until
    retry-until --timeout 60 --interval 1 --message "$pod: steering refused" \
        'dra-driver-log "$VM2" | grep -qE "ctr=$DRA_NS/$pod/main id=.*: (failed to start memory steering|memory steering ended): .*operation not permitted; memory fenced"' ||
        error "$pod: the driver did not log that the steering of its container failed"
    dra-driver-log "$VM2" | grep -E "ctr=$DRA_NS/$pod/main id=.*(failed to start memory steering|memory steering ended)"
    dra-memory-state "$VM2" "$pod" main || error "cannot read memory.high of $pod"
    [ "$DRA_MEM_HIGH" == max ] || error "$pod: memory.high is '$DRA_MEM_HIGH', expected max: not steered"
    mems=$(dra-container-read "$VM2" "$pod" main cpuset.mems) || error "cannot read cpuset.mems of $pod"
    ! dra-numa-nodes-has "$mems" "$NUMA_NODE" || error "$pod: cpuset.mems '$mems' has CXL node $NUMA_NODE, expected it fenced"
    echo "$pod: not steered, fenced: cpuset.mems $mems, memory.high $DRA_MEM_HIGH"
    dra-delete-pod "$VM2" "$pod"
done

echo "### F: one claim, two requests, two containers: each gets only the result of its request"
# Claim split has a request numa and a request dax, both of the local
# device, and opts in to steering for both. Container c-numa uses only the
# result of numa, c-dax only that of dax (resources.claims[].request):
# kubelet gives each only the CDI device of its result, with the marker of
# that result, CXL_CLAIM_<uid>_<n>. The driver attributes each container
# only its result: c-numa is steered to the node; c-dax has no memory to
# steer and is fenced like a container without claims, without the node.
dra-apply "$VM2" <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: split
spec:
  devices:
    requests:
    - name: numa
      exactly:
        deviceClassName: $LOCAL_CLASS
        capacity:
          requests:
            memory: $(dra-mi "$BS")
    - name: dax
      exactly:
        deviceClassName: $LOCAL_CLASS
        capacity:
          requests:
            memory: $(dra-mi "$BS")
    config:
    - requests: [dax]
      opaque:
        driver: $DRA_LOCAL_DRIVER
        parameters: $(dra-memory-config "device: {mode: dax}")
    - opaque:
        driver: $DRA_LOCAL_DRIVER
        parameters: $(dra-memory-config "container: {steering: {policy: auto}}")
---
$(dra-pod-yaml split "$VM2_NAME" mem=claim:split -- c-numa=mem/numa c-dax=mem/dax)
EOF
dra-pod-wait "$VM2" split Running 120
dra-json "$VM2" resourceclaim -n "$DRA_NS" split
SPLIT_JSON="$COMMAND_OUTPUT"
# Both results are slices of the one local device.
dra-assert "$SPLIT_JSON" "sorted((r['request'], r['driver'], r['device']) for r in results()) == [('dax', '$DRA_LOCAL_DRIVER', '$DRA_LOCAL_DEVICE'), ('numa', '$DRA_LOCAL_DRIVER', '$DRA_LOCAL_DEVICE')] and consumed() == $(( 2 * BS ))"
SPLIT_UID=$(dra-value "$SPLIT_JSON" "j['metadata']['uid']")
SPLIT_NUMA=$(result-index "$SPLIT_JSON" numa)
SPLIT_DAX=$(result-index "$SPLIT_JSON" dax)
# The environment key of a device is its serial; the second result of the
# same device in one claim, in result order, has the key <serial>_2.
NUMA_KEY="$DRA_LOCAL_HEX" DAX_KEY="${DRA_LOCAL_HEX}_2"
if [ "$SPLIT_DAX" -lt "$SPLIT_NUMA" ]; then
    NUMA_KEY="${DRA_LOCAL_HEX}_2" DAX_KEY="$DRA_LOCAL_HEX"
fi
dra-local-slices-wait "$VM2" split "$NUMA_NODE" "numa:$BS" "numa:$BS" "numa:$BS" "numa:$BS" "dax:$BS"
dra-local-numa-node-size "$VM2" split "$NUMA_NODE" $(( 4 * BS ))
claim-env split/c-numa
dra-env-assert split/c-numa "$CLAIM_ENV" "$NUMA_KEY" \
    SERIAL="$DRA_LOCAL_SERIAL" SHARED=0 MODE=numa SIZE="$BS" NUMA_NODE="$NUMA_NODE"
[ "$(grep '^CXL_CLAIM_' <<< "$CLAIM_ENV")" == "CXL_CLAIM_${SPLIT_UID//-/_}_$SPLIT_NUMA=1" ] && [ "$(grep -c '^CXL_' <<< "$CLAIM_ENV")" == 6 ] ||
    error "split/c-numa has other CXL_ variables than those of result $SPLIT_NUMA (numa) of claim split: $CLAIM_ENV"
claim-env split/c-dax
dra-env-assert split/c-dax "$CLAIM_ENV" "$DAX_KEY" \
    SERIAL="$DRA_LOCAL_SERIAL" SHARED=0 MODE=dax SIZE="$BS" "DAX=/dev/dax*" READ_ONLY=0
[ "$(grep '^CXL_CLAIM_' <<< "$CLAIM_ENV")" == "CXL_CLAIM_${SPLIT_UID//-/_}_$SPLIT_DAX=1" ] && [ "$(grep -c '^CXL_' <<< "$CLAIM_ENV")" == 7 ] ||
    error "split/c-dax has other CXL_ variables than those of result $SPLIT_DAX (dax) of claim split: $CLAIM_ENV"
dra-memory-wait "$VM2" split c-numa steered "auto from claim $DRA_NS/split request numa"
dra-mems-wait "$VM2" split c-numa "$NUMA_NODE"
dra-driver-log-wait "$VM2" "NRI CreateContainer: ctr=$DRA_NS/split/c-dax: steering policy auto: no numa memory to steer"
dra-memory-wait "$VM2" split c-dax fenced
# Of all containers of the node only those with CXL memory to use have the
# node: steer-first since its workload went on to CXL; not steer-dram, not
# c-dax.
dra-mems-fenced "$VM2" "$NUMA_NODE" "$DRA_NS/steer-cxl/main" "$DRA_NS/steer-mix/main" "$DRA_NS/steer-first/main" "$DRA_NS/split/c-numa"
for pod in steer-cxl steer-dram steer-mix steer-first split; do
    dra-delete-pod "$VM2" "$pod"
done
dra-delete "$VM2" resourceclaim split --wait=true --timeout=60s
initial-state steering-deleted

echo "### G: claims of one container that disagree on the steering policy: the container is not created"
# Both claims prepare, each MemoryConfig is valid alone; combined for the
# container they conflict, and the driver fails its CreateContainer.
dra-apply "$VM2" <<< "$(dra-claim-yaml claim conflict-a "$DRAM_CLASS" "$(dra-mi $(( DRAM_REQ / 2 )))" "container=steering: {policy: first-dram}")
---
$(dra-claim-yaml claim conflict-b "$DRAM_CLASS" "$(dra-mi $(( DRAM_REQ / 2 )))" "container=steering: {policy: none}")
---
$(dra-pod-yaml conflict "$VM2_NAME" a=claim:conflict-a b=claim:conflict-b)"
dra-wait "$VM2" conflict-event "events -n $DRA_NS --field-selector involvedObject.name=conflict" \
    "any(all(t in (e.get('message') or '') for t in ['container $DRA_NS/conflict/main: container.steering.policy: ', 'claim $DRA_NS/conflict-a request mem says first-dram', 'claim $DRA_NS/conflict-b request mem says none']) for e in items)" 120
dra-kubectl "$VM2" "get events -n $DRA_NS --field-selector involvedObject.name=conflict"
dra-wait "$VM2" conflict-not-created "pod -n $DRA_NS conflict" \
    "[(cs.get('state') or {}).get('waiting', {}).get('reason') for cs in j['status'].get('containerStatuses') or []] == ['CreateContainerError']" 60
dra-delete-pod "$VM2" conflict
dra-delete "$VM2" resourceclaim conflict-a --wait=true --timeout=60s
dra-delete "$VM2" resourceclaim conflict-b --wait=true --timeout=60s

echo "### H: what every component did, in order"
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
