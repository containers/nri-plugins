# An LLM shared through persistent pooled CXL memory: one writer, six
# readers of an erofs on two nodes of one Kubernetes cluster
#
# A writer flashes an erofs image of a small language model (Qwen3-0.6B, a
# 378 MiB GGUF file) into a shared persistent CXL memory device of the
# pool, and stock llama.cpp servers on two nodes answer a prompt with the
# model mapped from the file system on that device: direct readers straight
# from the CXL memory (DAX), cached readers through the page cache of their
# node.
#
# - The device llm0 is persistent: fake-cxl-pool backs it with a data file
#   and a label storage area (LSA) file on the host, shared by the qemu of
#   both VMs. Class cxl-pool-persistent selects it, cxl-pool-shared does
#   not.
# - The writer claims it without a "shares" request, which takes all of
#   them (exclusive, writable), MemoryConfig device {mode: block}: the
#   driver creates a pmem region and an fsdax namespace and gives the
#   container the block device /dev/pmemK.
#   The writer runs on VM1, the only node with the model; VM1 builds the
#   erofs image with "mkfs.erofs -b4096 --dsunit=512 -Enoinline_data", the
#   writer writes it with dd and exits. The host finds the model in the data
#   file at a 2 MiB-aligned offset, and labels in the LSA file.
# - The readers claim it with one share each (shares: "1"), MemoryConfig
#   device {mode: fs, readOnly: true, access: direct|cached}: the driver
#   mounts the erofs on the node once per access (direct: the pmem block
#   device with dax=always; cached: a read-only direct-I/O loop device over
#   it) and binds the mount read-only into the container, with no device
#   node. Three direct readers (two on VM2, one on VM1) start first, then
#   three cached readers in the same places, one at a time, while the
#   direct ones still run.
# - Two readers mount the device dynamically, into their running
#   container: direct-b on VM2 and cached-c on VM1 start with a lease
#   claim only (DeviceClass cxl-lease), and the container asks for the
#   erofs itself with "cxl-request request --serial 0xc1ae0001 --share
#   --read-only --mode fs --access direct|cached [--name models] --exec
#   sh /tmp/reader.sh". The driver creates a companion claim of class
#   cxl-pool-persistent for the lease, prepares it on the node like the
#   claim of a reader (the same node mount), and binds the node mount at
#   /run/cxl/fs/<name> into the container: the lease claim gave the
#   container a read-only slave mount of a host directory of the driver
#   there, and the bind propagates into it. So VM2 has a static and a
#   dynamic mount of the same device and access, VM1 a static direct and
#   a dynamic cached one.
# - With the claim of direct-a gone, the direct node mount of VM2 has one
#   user left, the lease of direct-b. Its release is refused while
#   llama-server maps the model: the driver cannot unmount the bind (the
#   lease stays Releasing, the companion pod Terminating, the model keeps
#   serving). Once llama-server is killed, the lease, its bind and the
#   direct node mount go, and the region stays for the cached readers of
#   VM2: the device is released only when every reference, static and
#   dynamic, is gone.
#
# Proofs: every reader answers "hello" with the expected text, asked from
# its node on its pod IP. Direct readers: every page of llama-server's
# mapping of the model is in "CXL Window N > Persistent Memory > regionM >
# namespaceM.0" (/proc/PID/pagemap and /proc/iomem on the node), the
# anonymous memory of the server is less than half the model, the page
# cache charged to its pod is less than 5% of the model and that of the
# node grows by less than half of it: no copy of the weights exists.
# Cached readers: the first one on a node is charged the model (90% at
# least) and pulls it from CXL through the loop device (90% at least of its
# block reads), and the page cache of its node grows by less than 150% of
# the model; the second one on VM2 is charged less than 5%, grows the page
# cache by less than half the model and reads less than 5% from CXL (one
# copy per node); the model is resident in the page cache of the node
# (fincore), and while a cached reader generates 128 tokens the present
# pages of its mappings are all in System RAM and its loop device reads
# less than 1 MiB: inference is served from the page cache of the node, not
# from CXL. (Presence of every page is not asserted for cached readers:
# khugepaged may unmap page-cached ranges at any time; block reads of the
# only path to CXL are the measure.) The mounts, loop devices, environment
# and CDI devices are what the driver promises, writes under the mount
# fail, the readers change neither the data nor the labels of the device,
# and nothing is left at the end. The leases of the dynamic readers, their
# companion objects, the CDI device of their lease claim and their binds on
# the node are what the lease protocol promises. In qemu, CXL memory is
# host RAM mapped into the VMs: timing cannot tell CXL from DRAM, page
# frames and page cache counters can.
#
# What you will see: VM1 building the erofs image (the first time), the
# writer on VM1 flashing it into llm0, the host finding the model in the
# data file, three direct and three cached llama.cpp servers on two nodes
# answering "hello" from the one copy in CXL memory or from one page cache
# copy per node, two of them on a filesystem that appeared in their running
# container, the frames of their model mappings, the page cache deltas, a
# release refused while the model is in use and done once its user is
# gone, and the trace of both nodes.
#
# The "shares" capacity of llm0 is 1M without a requestPolicy: every reader
# claim, and every companion claim of a lease, takes one share of it,
# whatever its node, so the six readers fit at once; the writer, with no
# shares request, takes all of them.
#
# The cluster: the VM of n4-cxl-shared-2 (VM2, the VM of the test) is the
# control plane, the VMs of n4-cxl-shared-1 (VM1) and n4-cxl-shared-0
# (VM0) workers, over the VM network "cxl-pool" (vm_network.var.sh);
# test00-up of n4-cxl-shared-2 makes it. The pool is cabled to VM1 and VM2
# only, and the test uses those two nodes. The pool controller runs once,
# on VM2, the driver on every node: one DaemonSet, installed with helm on
# VM2.
#
# Host prerequisites: curl, and through http_proxy and https_proxy
# huggingface.co (the model). The first run downloads the model (378 MiB)
# into $CACHE_DIR/llm. The VMs pull $LLM_SERVER_IMAGE
# (ghcr.io/ggml-org/llama.cpp:server-b11681, about 310 MB) and the image of
# the companion pods, and VM1 installs erofs-utils with dnf, through their
# own proxies. The VMs keep
# /usr/local/bin/cxl-where.py, VM1 also the model in /opt/cxl-llm/model and
# its erofs image /opt/cxl-llm/llm.erofs. The data and LSA files of llm0
# (/tmp/fake-cxl-pool/llm0.raw and llm0.lsa, host RAM) are removed at the
# end, also after a failure.
#
# Configuration (environment variable):
#   llm_model=PATH     a local copy of the model instead of the download
#                      into $CACHE_DIR/llm
#
# Run test00-up of n4-cxl-shared-1, -0 and -2 first: they create the VMs,
# boot them on the CXL kernel (with CONFIG_EROFS_FS) and make them one
# cluster. The DRA driver comes from its image, build it first:
#   make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

echo "### preconditions: one cluster, VM2 control plane, VM1 and VM0 workers (test00-up)"
pool-cluster-require

LLM_THREADS=2   # of each reader
# The answer of the pinned model, which answers greedily.
LLM_EXPECT="Hello! How can I assist you today?"

VM1="$POOL_VM1_DIR"    # worker, the node of the writer
VM2="$OUTPUT_DIR"      # control plane, the VM of this test
VM0_NAME="$POOL_VM0_NAME"   # the other worker, not cabled to the pool
VM1_NAME="$POOL_VM1_NAME"
VM2_NAME="$POOL_VM2_NAME"
LLM_DEVICE=llm0
LLM_SERIAL=0xc1ae0001   # 0xc1ae....: shared pool devices
LLM_HEX=C1AE0001        # the serial in environment variable names
LLM_MOUNT_ID=c1ae0001   # the serial in the names of the node mounts
LLM_DEV_SIZE=$(( 512 << 20 ))
LLM_DATA="$POOL_DIR/$LLM_DEVICE.raw"   # the files of the device on the host
LLM_LSA="$POOL_DIR/$LLM_DEVICE.lsa"
LLM_LSA_SIZE=262144
LLM_NS_SIZE=""          # the size of the fsdax namespace, as the writer got it
LLM_DEFAULT_PATH="$DRA_FS_CTR_DIR/$LLM_HEX"   # the mount in a container without "path"
LLM_FIVE_PCT=$(( LLM_MODEL_SIZE * 5 / 100 ))
LLM_NINETY_PCT=$(( LLM_MODEL_SIZE * 90 / 100 ))
LLM_HALF=$(( LLM_MODEL_SIZE / 2 ))
LLM_ONE_HALF=$(( LLM_MODEL_SIZE * 3 / 2 ))
# The model mapping of llama.cpp: it unmaps the GGUF header part of its
# whole-file mapping, so the mappings cover most of the model, not all.
LLM_MAPPED_MIN=$(( LLM_MODEL_SIZE * 90 / 100 ))
CHAIN_CXL='^CXL Window [0-9]+ > Persistent Memory > region[0-9]+ > namespace[0-9]+\.[0-9]+$'
CHAIN_RAM='^System RAM$'
DIRECT=(direct-a direct-b direct-c)
CACHED=(cached-a cached-b cached-c)
declare -A READER_VM=([direct-a]="$VM2" [direct-b]="$VM2" [direct-c]="$VM1"
                      [cached-a]="$VM2" [cached-b]="$VM2" [cached-c]="$VM1")
declare -A READER_ACCESS=([direct-a]=direct [direct-b]=direct [direct-c]=direct
                          [cached-a]=cached [cached-b]=cached [cached-c]=cached)
# Lease readers mount the device dynamically: their pod has a claim from
# the ResourceClaimTemplate $LEASE_TEMPLATE, and their container leases the
# erofs (llm-lease-reader-script) at /run/cxl/fs/<name>, <name> from
# READER_NAME or the lease id. The others are claim readers.
declare -A READER_KIND=([direct-b]=lease [cached-c]=lease)
declare -A READER_NAME=([direct-b]=models)
# The path of the erofs in a reader: claim readers $LLM_DEFAULT_PATH unless
# set here; lease readers the path of their lease (lease-reader-found).
declare -A READER_PATH=([cached-b]=/models)
declare -A READER_LEASE=()     # lease reader -> its lease id,
declare -A READER_UID=()       # the uid of its pod,
declare -A READER_CLAIM=()     # and the name and uid of its lease claim
declare -A READER_CLAIM_UID=()
LEASE_TEMPLATE=cxl-lease
CACHE_DELTA=""            # reader-start: the growth of the page cache of the node in bytes,
POD_FILE=""               # and the page cache charged to the pod of the reader in bytes,
LOOP_READ=""              # and, for a cached reader, the bytes read meanwhile through the loop device
declare -A READER_LOOP=() # cached reader -> the loop device (loopN) over pmem on its node
LLM_SERVE_TOKENS=128      # the tokens of the long request to each cached reader
LLM_MIB=$(( 1 << 20 ))

llm0-free() {
    # Usage: llm0-free
    #
    # Wait until llm0 is free and nothing of it is left in VM2 and VM1
    # (fs-device-free).
    fs-device-free "$LLM_DEVICE" "$VM2" "$VM1"
}

exit-cleanup() {
    # Usage: exit-cleanup
    #
    # The EXIT trap after dra-install: fs-exit-cleanup of llm0.
    fs-exit-cleanup "$LLM_DEVICE" "guest disable $LLM_SERIAL" "$VM2 $VM1" "$LLM_DATA" "$LLM_LSA"
}

reader-kind() {
    # Usage: reader-kind READER
    #
    # Print "lease" for a lease reader, else "claim".
    echo "${READER_KIND[$1]:-claim}"
}

reader-path() {
    # Usage: reader-path READER
    #
    # Print the path of the erofs mount in the container of READER.
    echo "${READER_PATH[$1]:-$LLM_DEFAULT_PATH}"
}

lease-reader-found() {
    # Usage: lease-reader-found READER
    #
    # Set READER_LEASE and READER_PATH of the lease reader READER from its
    # only lease ("cxl-request list" in it), READER_UID from its pod, and
    # READER_CLAIM and READER_CLAIM_UID from its lease claim. Fail the test
    # unless the lease is Ready at /run/cxl/fs/<name>, <name> its
    # READER_NAME or else the lease id, and the lease claim is allocated
    # the lease device of its node.
    local r="$1" node lease
    node=$(basename "${READER_VM[$r]}")
    dra-leases "$VM2" "$r/main"
    dra-assert "$DRA_LEASES" "len(j) == 1 and j[0].get('state') == 'Ready' and j[0].get('path')"
    lease=$(dra-value "$DRA_LEASES" "j[0]['lease']")
    [[ "$lease" =~ ^[0-9a-f]{8}$ ]] || error "$r: lease id '$lease' is not 8 hex digits"
    READER_LEASE[$r]=$lease
    READER_PATH[$r]=$(dra-value "$DRA_LEASES" "j[0]['path']")
    [ "${READER_PATH[$r]}" == "$DRA_LEASE_FS_CTR_DIR/${READER_NAME[$r]:-$lease}" ] ||
        error "$r: lease $lease has the path ${READER_PATH[$r]}, expected $DRA_LEASE_FS_CTR_DIR/${READER_NAME[$r]:-$lease}"
    dra-json "$VM2" pod -n "$DRA_NS" "$r"
    READER_UID[$r]=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
    READER_CLAIM[$r]=$(dra-value "$COMMAND_OUTPUT" "[s['resourceClaimName'] for s in j['status']['resourceClaimStatuses'] if s['name'] == 'lease'][0]")
    dra-json "$VM2" resourceclaim -n "$DRA_NS" "${READER_CLAIM[$r]}"
    dra-assert "$COMMAND_OUTPUT" "[(x['driver'], x['device']) for x in results()] == [('$DRA_LOCAL_DRIVER', 'lease')] and allocated_node() == '$node'"
    READER_CLAIM_UID[$r]=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
    echo "$r: lease $lease at ${READER_PATH[$r]}, lease claim ${READER_CLAIM[$r]} (uid ${READER_CLAIM_UID[$r]})"
}

reader-start() {
    # Usage: reader-start READER
    #
    # Create the pod READER on its node and wait until llama-server serves:
    # a claim reader with a ResourceClaim of its own, a read-only fs claim
    # of the device with its access and path; a lease reader with a claim
    # from $LEASE_TEMPLATE, leasing the erofs of the device with its access
    # and name (lease-reader-found). Print the page cache of the node
    # before and after and the cgroups it was charged to meanwhile, and set
    # CACHE_DELTA to its growth in bytes. Ask the reader for its answer,
    # then set POD_FILE to the page cache charged to its pod. For a cached
    # reader set READER_LOOP to the loop device over pmem on its node and
    # LOOP_READ to the bytes read through it meanwhile.
    local r="$1" vm="${READER_VM[$1]}" params before after loops0="" loop now base
    params="mode: fs, readOnly: true, access: ${READER_ACCESS[$r]}"
    [ -z "${READER_PATH[$r]}" ] || params+=", path: ${READER_PATH[$r]}"
    if [ "${READER_ACCESS[$r]}" == cached ]; then
        # Loop devices keep their counters when they are reused: the
        # baseline is that of every loop device now.
        loops0=$(llm-loop-reads "$vm") || error "cannot read the loop devices of $(vmdir-label "$vm")"
    fi
    before=$(llm-page-cache "$vm")
    if [ "$(reader-kind "$r")" == lease ]; then
        dra-apply "$VM2" <<< "$(llm-server-pod-yaml "$r" "$vm" lease "$LEASE_TEMPLATE" \
            "$(llm-lease-reader-script "$LLM_HEX" "${READER_ACCESS[$r]}" "${READER_NAME[$r]}" "$LLM_THREADS")")"
    else
        dra-apply "$VM2" <<< "$(dra-shared-claim-yaml "$r" "$LLM_SERIAL" 1 "$params" cxl-pool-persistent)
---
$(llm-server-pod-yaml "$r" "$vm" claim "$r" "$(llm-reader-script "$LLM_HEX" "$LLM_THREADS")")"
    fi
    llm-server-wait "$vm" "$r"
    after=$(llm-page-cache "$vm")
    CACHE_DELTA=$(( (after - before) * 1024 ))
    [ "$(reader-kind "$r")" != lease ] || lease-reader-found "$r"
    echo "page cache of $(vmdir-label "$vm") around $r: $before kB -> $after kB, $(( CACHE_DELTA >> 10 )) kB, the model $(( LLM_MODEL_SIZE >> 10 )) kB"
    llm-ask "$vm" "$r"
    POD_FILE=$(llm-pod-file "$vm" "$r") || error "$r: no page cache counter of its pod"
    echo "page cache charged to the pod $r: $(( POD_FILE >> 10 )) kB"
    [ "${READER_ACCESS[$r]}" == cached ] || return 0
    loop=$(dra-pmem-loops "$vm") || error "cannot list the loop devices of $(vmdir-label "$vm")"
    [ "$(wc -l <<< "$loop")" == 1 ] && [ -n "$loop" ] || error "$(vmdir-label "$vm") has not one loop device over pmem: $loop"
    loop=${loop%% *}
    READER_LOOP[$r]=$loop
    now=$(llm-loop-reads "$vm" "$loop") || error "cannot read $loop of $(vmdir-label "$vm")"
    base=$(awk -v l="$loop" '$1 == l {print $2}' <<< "$loops0")
    LOOP_READ=$(( ${now#* } - ${base:-0} ))
    echo "read through $loop of $(vmdir-label "$vm") (from CXL) around $r: $LOOP_READ bytes, the model $LLM_MODEL_SIZE"
}

cache-cmp() {
    # Usage: cache-cmp VALUE -lt|-ge LIMIT
    #
    # Return 0 if VALUE is less than (-lt) or at least (-ge) LIMIT: the
    # operator of cache-assert as a word.
    case "$2" in
        -lt) (( $1 < $3 ));;
        -ge) (( $1 >= $3 ));;
        *) error "cache-cmp: unknown operator $2";;
    esac
}

cache-assert() {
    # Usage: cache-assert READER POD-OP POD-LIMIT NODE-OP NODE-LIMIT WHAT
    #
    # Fail the test unless POD_FILE POD-OP POD-LIMIT and CACHE_DELTA
    # NODE-OP NODE-LIMIT hold (cache-cmp) for READER, saying WHAT is wrong
    # otherwise.
    local r="$1" vm
    vm=$(vmdir-label "${READER_VM[$1]}")
    cache-cmp "$POD_FILE" "$2" "$3" ||
        error "$r: the page cache charged to its pod is $POD_FILE bytes, not $2 $3: $6"
    cache-cmp "$CACHE_DELTA" "$4" "$5" ||
        error "$r: the page cache of $vm grew by $CACHE_DELTA bytes, not $4 $5: $6"
    echo "page cache ok: $r: pod $POD_FILE $2 $3, node +$CACHE_DELTA $4 $5"
}

reader-check() {
    # Usage: reader-check READER CHAIN
    #
    # Fail the test unless READER answers the expected text, got the
    # environment and the read-only mount of an fs claim of the device with
    # its access and path and no pmem device, could not write under the
    # mount, maps the model with every page in resources of /proc/iomem
    # that match CHAIN, and has not copied the weights into its anonymous
    # memory; a claim reader its claim and CDI device (claim-check), a
    # lease reader its lease, the companion objects, the CDI device of its
    # lease claim and the bind on its node (lease-check).
    local r="$1" chain="$2" vm="${READER_VM[$1]}" access="${READER_ACCESS[$1]}" path log anon
    path=$(reader-path "$r")
    log=$(dra-pod-log "$VM2" "$r")
    echo "$log" > "$TEST_OUTPUT_DIR/$r.log"
    echo "### $r on $(vmdir-label "$vm"), $access, $(reader-kind "$r") reader, at $path (log: $TEST_OUTPUT_DIR/$r.log):"
    grep -E '^(CXL_|mount: |files: |no pmem devices$|pmem devices: |write under |cxl-request )' <<< "$log"
    dra-env-assert "$r" "$log" "$LLM_HEX" \
        SERIAL="$LLM_SERIAL" SHARED=1 MODE=fs SIZE="$LLM_NS_SIZE" PATH="$path" ACCESS="$access" READ_ONLY=1 FS_TYPE=erofs
    grep -qx "no pmem devices" <<< "$log" ||
        error "$r has pmem devices: $(grep '^pmem devices:' <<< "$log")"
    grep -qE "^write under $path refused: .*Read-only file system$" <<< "$log" ||
        error "$r: a write under $path did not fail with \"Read-only file system\": $(grep "^write under " <<< "$log")"
    # The container sees a read-only erofs at the path.
    grep -qE "^mount: .* $path ro[, ].* - erofs " <<< "$log" ||
        error "$r: $path is not a read-only erofs mount in the container: $(grep '^mount:' <<< "$log")"
    llm-ask "$vm" "$r"
    [ "$LLM_CONTENT" == "$LLM_EXPECT" ] || error "$r answered '$LLM_CONTENT', expected '$LLM_EXPECT'"
    if [ "$(reader-kind "$r")" == lease ]; then
        lease-check "$r" "$log"
    else
        claim-check "$r"
    fi
    # Where the weights are. Direct: every page of the model mappings in
    # the CXL window. Cached: the present pages in System RAM; khugepaged
    # may unmap page-cached ranges of a mapping at any time (until the
    # process touches them again), so presence is not a property to assert
    # there: residency in the page cache and the block reads of the loop
    # device, the only path to CXL, are (cached-serve-check).
    if [ "$access" == direct ]; then
        llm-where-assert "$vm" "$r" "$path/$LLM_MODEL_FILE" "$LLM_MAPPED_MIN" "$chain" all
    else
        llm-where-assert "$vm" "$r" "$path/$LLM_MODEL_FILE" "$LLM_MAPPED_MIN" "$chain" present
        cached-serve-check "$r" "$path"
    fi
    # No copy of the weights in the process: a copy (llama.cpp reading the
    # tensors instead of mapping them, or repacking them) is at least the
    # model, or 205 MiB more for repacking.
    anon=$(llm-rss-anon "$vm" "$r") || error "$r: no RssAnon of llama-server"
    [ $(( anon * 1024 )) -lt $(( LLM_MODEL_SIZE / 2 )) ] ||
        error "$r has $anon kB of anonymous memory, not less than half the model ($(( LLM_MODEL_SIZE / 2048 )) kB): the weights were copied into the process"
}

claim-check() {
    # Usage: claim-check READER
    #
    # Fail the test unless the claim of the claim reader READER has one
    # share of the device on the node of READER, persistent and not
    # exclusive, and its CDI device claim-<uid>-0 of the pool driver is a
    # read-only bind of the node mount of its access at its path, without
    # device nodes (fs-cdi-assert).
    local r="$1" vm="${READER_VM[$1]}" access="${READER_ACCESS[$1]}" path
    path=$(reader-path "$r")
    dra-json "$VM2" resourceclaim -n "$DRA_NS" "$r"
    dra-assert "$COMMAND_OUTPUT" "allocated_node() == '$(basename "$vm")' and quantity(j['status']['allocation']['devices']['results'][0]['consumedCapacity']['shares']) == 1"
    dra-assert "$COMMAND_OUTPUT" "device_data('$LLM_DEVICE').get('persistent') == True and not device_data('$LLM_DEVICE').get('exclusive')"
    fs-cdi-assert "$vm" "$r" "$DRA_POOL_DRIVER/device" "$DRA_POOL_MOUNT_DIR/$LLM_MOUNT_ID-$access" "$path" "['bind', 'ro', 'nosuid', 'nodev']"
}

lease-check() {
    # Usage: lease-check READER LOG
    #
    # Fail the test unless the lease reader READER, whose log is LOG, has
    # exactly its lease in "cxl-request list": Ready, a read-only share of
    # the persistent pool device in mode fs with its access, the detected
    # filesystem type erofs, the namespace size, its path and the
    # environment of the log; "cxl-request env" prints that environment
    # and CXL_LEASE; and the companion objects (lease-companion-check), the
    # CDI device of its lease claim (dra-lease-cdi-assert) and the bind on
    # its node (lease-bind-assert) are those of the lease.
    local r="$1" log="$2" vm="${READER_VM[$1]}" access="${READER_ACCESS[$1]}" lease="${READER_LEASE[$1]}" path="${READER_PATH[$1]}" env want
    [ "$(dra-env-value "$log" CXL_LEASE)" == "$lease" ] ||
        error "$r: the command of cxl-request has CXL_LEASE=$(dra-env-value "$log" CXL_LEASE), expected $lease"
    env=$(grep -E "^CXL_${LLM_HEX}_[A-Z][A-Z_]*=" <<< "$log" | LC_ALL=C sort)
    dra-leases "$VM2" "$r/main"
    dra-assert "$DRA_LEASES" "[(l['lease'], l['state'], l.get('source'), l.get('mode'), l.get('access'), l.get('fsType'), l.get('share'), l.get('readOnly'), l.get('serial'), int(l.get('size', 0)), l.get('path')) for l in j] == [('$lease', 'Ready', 'pool', 'fs', '$access', 'erofs', True, True, '$LLM_SERIAL', $LLM_NS_SIZE, '$path')]"
    dra-assert "$DRA_LEASES" "'numaNode' not in j[0] and 'dax' not in j[0]"
    dra-assert "$DRA_LEASES" "j[0].get('env') == $(python3 -c 'import json, sys; print(json.dumps(dict(l.split("=", 1) for l in sys.argv[1].splitlines())))' "$env")"
    # The environment again, with the lease, and no fd: the lease has none.
    dra-exec "$VM2" "$r/main" "$DRA_REQUEST_CTR_BIN" env --lease "$lease" ||
        command-error "$r: cxl-request env --lease $lease failed"
    want=$(LC_ALL=C sort <<< "$env
CXL_LEASE=$lease")
    [ "$COMMAND_OUTPUT" == "$want" ] ||
        error "$r: cxl-request env --lease $lease printed
$COMMAND_OUTPUT
expected
$want"
    lease-companion-check "$r"
    dra-lease-cdi-assert "$vm" "${READER_CLAIM_UID[$r]}"
    lease-bind-assert "$r"
}

lease-companion-check() {
    # Usage: lease-companion-check READER
    #
    # Fail the test unless the companion pod and claim cxl-lease-<id> of the
    # lease of the lease reader READER are owned by the pod of READER and
    # labeled and annotated with the lease and its requester (container
    # main), the pod runs on the node of READER and holds the claim, and
    # the claim asks for one share of the device in class
    # cxl-pool-persistent with a MemoryConfig of the pool driver, device
    # section {mode: fs, readOnly: true, access} (no fsType: the lease
    # names none) and no path, is allocated the device on
    # that node, attached, persistent and not exclusive, and has no CDI
    # device.
    local r="$1" vm="${READER_VM[$1]}" access="${READER_ACCESS[$1]}" lease="${READER_LEASE[$1]}" uid="${READER_UID[$1]}" node name json claim_uid cdis
    node=$(basename "$vm")
    name="cxl-lease-$lease"
    dra-pod-wait "$VM2" "$name" Running 30
    dra-json "$VM2" pod -n "$DRA_NS" "$name"
    json="$COMMAND_OUTPUT"
    dra-assert "$json" "[(o['kind'], o['name'], o['uid']) for o in j['metadata']['ownerReferences']] == [('Pod', '$r', '$uid')]"
    dra-companion-meta-check "$json" "$lease" "$uid" "$node" "$DRA_NS/$r/main"
    dra-assert "$json" "j['spec']['nodeName'] == '$node' and j['spec']['resourceClaims'] == [{'name': 'lease', 'resourceClaimName': '$name'}]"
    dra-assert "$json" "[c['image'] for c in j['spec']['containers']] == ['$DRA_COMPANION_IMAGE'] and not j['spec']['containers'][0]['resources'].get('claims')"
    dra-json "$VM2" resourceclaim -n "$DRA_NS" "$name"
    json="$COMMAND_OUTPUT"
    dra-claim-conditions "$VM2" "$name"
    dra-assert "$json" "[(o['kind'], o['name'], o['uid'], o.get('controller')) for o in j['metadata']['ownerReferences']] == [('Pod', '$r', '$uid', True)]"
    dra-companion-meta-check "$json" "$lease" "$uid" "$node" "$DRA_NS/$r/main"
    dra-assert "$json" "[(s['deviceClassName'], [x['cel']['expression'] for x in s.get('selectors') or []], s.get('capacity')) for s in subrequests()] == [('cxl-pool-persistent', ['device.attributes[\"$DRA_POOL_DRIVER\"].serial == \"$LLM_SERIAL\"'], {'requests': {'shares': '1'}})]"
    dra-assert "$json" "[(c['driver'], c['parameters']) for c in configs()] == [('$DRA_POOL_DRIVER', {'apiVersion': '$DRA_CONFIG_API', 'kind': '$DRA_CONFIG_KIND', 'device': {'mode': 'fs', 'readOnly': True, 'access': '$access'}})]"
    dra-assert "$json" "[(x['driver'], x['device']) for x in results()] == [('$DRA_POOL_DRIVER', '$LLM_DEVICE')] and allocated_node() == '$node'"
    dra-assert "$json" "condition('$DRA_POOL_DRIVER/Attached') == 'True' and quantity(results()[0]['consumedCapacity']['shares']) == 1"
    dra-assert "$json" "device_data('$LLM_DEVICE').get('persistent') == True and not device_data('$LLM_DEVICE').get('exclusive')"
    # The companion pod uses nothing of its claim: no CDI device.
    claim_uid=$(dra-value "$json" "j['metadata']['uid']")
    cdis=$(dra-cdi-devices-with "$vm" "$claim_uid") || error "cannot read the CDI specs of $(vmdir-label "$vm")"
    [ -z "$cdis" ] || error "$r: the companion claim $name has CDI devices: $cdis"
}

lease-bind-assert() {
    # Usage: lease-bind-assert READER
    #
    # Fail the test unless the node of the lease reader READER has exactly
    # one mount at $DRA_LEASE_FS_DIR/<lease claim uid>/<name of its path>:
    # a read-only bind of the erofs of the node mount of its access
    # (fs-lease-bind-assert).
    local r="$1"
    fs-lease-bind-assert "${READER_VM[$r]}" "$DRA_LEASE_FS_DIR/${READER_CLAIM_UID[$r]}/${READER_PATH[$r]##*/}" \
        "$DRA_POOL_MOUNT_DIR/$LLM_MOUNT_ID-${READER_ACCESS[$r]}" erofs ro
}

cached-serve-check() {
    # Usage: cached-serve-check READER PATH
    #
    # Fail the test unless, on the node of the cached READER, the model in
    # the cached node mount is resident in the page cache (90% at least,
    # fincore), and a request of $LLM_SERVE_TOKENS tokens to READER
    # (llm-serve-sampled) gives that many tokens while two samples at least
    # of its mappings of PATH/<model> have present pages, all in System
    # RAM, and its loop device over pmem reads less than 1 MiB from CXL.
    local r="$1" path="$2" vm="${READER_VM[$1]}" label res tokens weights
    label=$(vmdir-label "$vm")
    res=$(llm-fincore "$vm" "$DRA_POOL_MOUNT_DIR/$LLM_MOUNT_ID-cached/$LLM_MODEL_FILE") ||
        error "$r: fincore of the model in the cached node mount of $label failed"
    echo "$r: page cache of $label holds $res bytes of the model (fincore), the model $LLM_MODEL_SIZE"
    [ "$res" -ge "$LLM_NINETY_PCT" ] ||
        error "$r: only $res bytes of the model are in the page cache of $label, less than 90% ($LLM_NINETY_PCT)"
    llm-serve-sampled "$vm" "$r" "$path/$LLM_MODEL_FILE" "${READER_LOOP[$r]}" "$LLM_SERVE_TOKENS"
    tokens=$(llm-answer-value 'j["tokens_predicted"]')
    [ "$tokens" == "$LLM_SERVE_TOKENS" ] || error "$r: the long request gave $tokens tokens, not $LLM_SERVE_TOKENS"
    python3 -c '
import json, sys
print("%d samples while it served: %s" % (len(json.loads(sys.argv[1])), "; ".join(
    "%d of %d pages present" % (sum(m["present"] for m in s), sum(m["pages"] for m in s)) for s in json.loads(sys.argv[1]))))' "$LLM_SAMPLES"
    [ "$(python3 -c 'import json, sys; print(len(json.loads(sys.argv[1])))' "$LLM_SAMPLES")" -ge 2 ] ||
        error "$r: fewer than two samples of its mappings while it served $LLM_SERVE_TOKENS tokens"
    python3 -c 'import json, sys; [print(json.dumps(s)) for s in json.loads(sys.argv[1])]' "$LLM_SAMPLES" | while read -r sample; do
        llm-where-check "$sample" "$LLM_MAPPED_MIN" "$CHAIN_RAM" present >/dev/null || exit 1
    done || error "$r: a sample while it served has no present page, or frames outside System RAM: $LLM_SAMPLES"
    [ "$LLM_LOOP_READ" -lt "$LLM_MIB" ] ||
        error "$r: ${READER_LOOP[$r]} read $LLM_LOOP_READ bytes from CXL while it served, not less than 1 MiB"
    weights=$(( tokens * LLM_MODEL_SIZE ))
    echo "$r on $label: $tokens tokens, ~$(( weights / 1000000000 )).$(( weights / 100000000 % 10 )) GB of weights read, $LLM_LOOP_READ bytes from CXL (${READER_LOOP[$r]})"
}

mounts-assert() {
    # Usage: mounts-assert VMDIR ACCESS...
    #
    # Fail the test unless the node mounts of the VM of VMDIR under
    # $DRA_POOL_MOUNT_DIR are exactly one read-only erofs of llm0 per
    # ACCESS (fs-node-mounts-assert).
    fs-node-mounts-assert "$1" "$DRA_POOL_MOUNT_DIR" "$LLM_MOUNT_ID" erofs ro "" "${@:2}"
}

readers-stop() {
    # Usage: readers-stop [--keep] READER...
    #
    # Delete the pods of the READERs, then the claims of the claim readers,
    # wait until the lease claims and the companion objects of the lease
    # readers are gone, 90 s at most, and, unless --keep, until the device
    # is free again (llm0-free).
    local r free=1 gone=()
    if [ "$1" == --keep ]; then
        free=""
        shift
    fi
    vmdir-command "$VM2" "kubectl delete pod -n $DRA_NS --grace-period=2 --wait=true --timeout=120s $*" ||
        command-error "cannot delete the readers"
    for r in "$@"; do
        if [ "$(reader-kind "$r")" == lease ]; then
            gone+=("'${READER_CLAIM[$r]}'" "'cxl-lease-${READER_LEASE[$r]}'")
        else
            dra-delete "$VM2" resourceclaim "$r" --wait=true --timeout=120s
        fi
    done
    if [ "${#gone[@]}" -gt 0 ]; then
        dra-wait "$VM2" "leases-gone-$(IFS=-; echo "$*")" "pods,resourceclaims -n $DRA_NS" \
            "not set(names()) & {$(IFS=,; echo "${gone[*]}")}" 90
    fi
    [ -z "$free" ] || llm0-free
}

lease-dirs-gone() {
    # Usage: lease-dirs-gone READER...
    #
    # Wait until the node of each lease reader READER has neither the
    # socket directory nor the filesystem directory of its lease claim,
    # $DRA_LEASE_DIR/<uid> and $DRA_LEASE_FS_DIR/<uid>, 60 s at most. Fail
    # the test on timeout.
    local r vm dirs
    for r in "$@"; do
        vm="${READER_VM[$r]}"
        dirs="$DRA_LEASE_DIR/${READER_CLAIM_UID[$r]} $DRA_LEASE_FS_DIR/${READER_CLAIM_UID[$r]}"
        retry-until --timeout 60 --interval 1 --message "$(vmdir-label "$vm") has no $dirs" \
            'vmdir-command-q "$vm" "for d in $dirs; do test ! -e \$d || exit 1; done"' ||
            error "$r: $(vmdir-label "$vm") still has $(vmdir-command-q "$vm" "ls -d $dirs 2>/dev/null; findmnt -rn -o TARGET | grep -F $DRA_LEASE_FS_DIR/")"
        echo "$r: $(vmdir-label "$vm") has no directories of its lease claim"
    done
}

vm-region-assert() {
    # Usage: vm-region-assert VMDIR
    #
    # Fail the test unless the VM of VMDIR has a CXL region.
    local regions
    regions=$(vmdir-command-q "$1" "ls /sys/bus/cxl/devices/") ||
        error "cannot list the CXL devices of $(vmdir-label "$1")"
    grep -q '^region[0-9]' <<< "$regions" || error "$(vmdir-label "$1") has no CXL region"
    echo "$(vmdir-label "$1") has the region $(grep '^region[0-9]' <<< "$regions" | tr '\n' ' ')"
}

lease-fs-empty() {
    # Usage: lease-fs-empty VMDIR
    #
    # Fail the test unless the VM of VMDIR has no mount and no directory
    # under $DRA_LEASE_FS_DIR, waiting 60 s at most for the directories.
    local vm="$1" label
    label=$(vmdir-label "$vm")
    dra-lease-fs-mounts "$vm" || error "cannot read the lease mounts of $label"
    [ "$COMMAND_OUTPUT" == "[]" ] || error "$label still has mounts under $DRA_LEASE_FS_DIR: $COMMAND_OUTPUT"
    retry-until --timeout 60 --interval 1 --message "$label has nothing under $DRA_LEASE_FS_DIR" \
        '[ "$(vmdir-command-q "$vm" "{ test ! -d $DRA_LEASE_FS_DIR || find $DRA_LEASE_FS_DIR -mindepth 1; } && echo end")" == end ]' ||
        error "$label still has under $DRA_LEASE_FS_DIR: $(vmdir-command-q "$vm" "find $DRA_LEASE_FS_DIR -mindepth 1")"
}

echo "### preconditions: patched qemu, hdm_for_passthrough=on, CONFIG_FS_DAX, CONFIG_EROFS_FS"
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"
pool-vm-require "$VM1" n4-cxl-shared-1
fs-kernel-require "$VM2" "$(basename "$TOPOLOGY_DIR")" EROFS_FS
fs-kernel-require "$VM1" n4-cxl-shared-1 EROFS_FS

echo "### preconditions: the model on the host"
llm-model-fetch

echo "### preconditions: cxl, daxctl and cxl-dump in both VMs, no CXL memory devices"
pool-vm-tools-install "$VM2"
pool-vm-tools-install "$VM1"
pool-vm-reset "$VM2"
pool-vm-reset "$VM1"
for vm in "$VM2" "$VM1"; do
    pool-cxl-dump "$vm" no-devices
    cxl-assert 'memdevs == [] and regions == [] and endpoints == []'
done

echo "### preconditions: cxl-where.py on both nodes, the model and its erofs image only on VM1"
# Only the VM of the writer has the model: the readers get it from the
# device.
llm-install "$VM1" model
llm-install "$VM2"
llm-image-build "$VM1"
[ "$LLM_IMAGE_SIZE" -le "$LLM_DEV_SIZE" ] || error "the erofs image ($LLM_IMAGE_SIZE bytes) is larger than $LLM_DEVICE"

echo "### preconditions: fake-cxl-pool-server with the persistent device $LLM_DEVICE, its files new"
# New files are all zeros: a writer that writes nothing cannot pass, and
# the labels in the LSA file are the writer's.
rm -f "$LLM_DATA" "$LLM_LSA" || error "cannot remove $LLM_DATA and $LLM_LSA"
pool-server-start "
  - name: $LLM_DEVICE
    size: 512Mi
    shared: true
    persistent: true
    serial: $LLM_SERIAL"
# The files are host RAM (/tmp) and hold the model: remove them on exit,
# also after a failure.
trap 'pool-cleanup; rm -f "$LLM_DATA" "$LLM_LSA"' EXIT
pool-client-install "$VM2" "$VM1"
pool-device-files "$LLM_DEVICE"
pool-assert "$COMMAND_OUTPUT" "j['persistent'] == True and j['shared'] == True"
[ "$POOL_DEVICE_DATA" == "$LLM_DATA" ] || error "the data file of $LLM_DEVICE is $POOL_DEVICE_DATA, expected $LLM_DATA"
[ "$POOL_DEVICE_LSA" == "$LLM_LSA" ] || error "the LSA file of $LLM_DEVICE is $POOL_DEVICE_LSA, expected $LLM_LSA"
[ "$(stat -c %s "$LLM_LSA")" == "$LLM_LSA_SIZE" ] || error "the LSA file $LLM_LSA is not $LLM_LSA_SIZE bytes: $(stat -c %s "$LLM_LSA")"
llm-file-zero "$LLM_LSA" 0 "$LLM_LSA_SIZE" || error "the new LSA file $LLM_LSA is not all zeros"
echo "host: $LLM_DATA and $LLM_LSA ($LLM_LSA_SIZE bytes, all zeros)"

echo "### preconditions: the pool controller on the control plane, the driver on every node"
dra-build
dra-install
trap exit-cleanup EXIT
for vm in "$VM2" "$VM1"; do
    dra-pull-images "$vm" "$DRA_TOOLS_IMAGE" "$LLM_SERVER_IMAGE" "$DRA_COMPANION_IMAGE"
done
dra-tools-install "$VM2"
# The first container of an image on a node reads its files into the page
# cache: do that before the readers, whose page cache deltas count the
# model.
for vm in "$VM2" "$VM1"; do
    llm-warm "$vm"
done
dra-trace-start "$VM2" "$VM1"

echo "### A: one cluster of three nodes, one pool with the persistent device $LLM_DEVICE, its class"
dra-kubectl "$VM2" "get nodes -o wide"
dra-json "$VM2" nodes
dra-assert "$COMMAND_OUTPUT" "sorted(n['metadata']['name'] for n in j['items']) == sorted(['$VM0_NAME', '$VM1_NAME', '$VM2_NAME'])"
dra-kubectl "$VM2" "get resourceslices"
dra-json "$VM2" resourceslices
D0="device('$LLM_DEVICE', '$DRA_POOL_DRIVER')"
dra-assert "$COMMAND_OUTPUT" "len(slices('$DRA_POOL_DRIVER')) == 1 and attr($D0, 'persistent') == True and attr($D0, 'shared') == True and attr($D0, 'serial') == '$LLM_SERIAL' and attr($D0, 'size') == $LLM_DEV_SIZE"
dra-assert "$COMMAND_OUTPUT" "sorted(${D0}.get('capacity') or {}) == ['shares'] and quantity(capacity($D0, 'shares')) == $DRA_SHARES and request_policy($D0, 'shares') is None"
dra-json "$VM2" deviceclass cxl-pool-persistent cxl-pool-shared
dra-assert "$COMMAND_OUTPUT" "names() == ['cxl-pool-persistent', 'cxl-pool-shared']"
# cxl-pool-shared does not match llm0: a pod with a claim of that class for
# it cannot be scheduled, because its claim cannot be allocated.
dra-unmatched unmatched "$LLM_SERIAL" cxl-pool-shared
vmdir-command "$VM2" "test ! -e $LLM_VM_MODEL_DIR && test ! -e $LLM_VM_IMAGE && ls -la $LLM_VM_DIR" ||
    error "VM2 has the model or its image: its readers must get it from CXL memory only"

echo "### B: the writer on VM1 takes $LLM_DEVICE exclusively and flashes the erofs image into it"
dra-apply "$VM2" <<< "$(dra-shared-claim-yaml llm-writer "$LLM_SERIAL" "" "mode: block" cxl-pool-persistent)
---
$(dra-script-pod-yaml writer "$VM1" llm-writer "$(fs-writer-script "$LLM_HEX" "$LLM_VM_IMAGE")" "File:$LLM_VM_IMAGE")"
fs-writer-wait "$VM2" writer 180
dra-env-assert writer "$FS_LOG" "$LLM_HEX" \
    SERIAL="$LLM_SERIAL" SHARED=1 MODE=block "SIZE=[1-9]*" "PATH=/dev/pmem*" READ_ONLY=0
LLM_NS_SIZE=$(dra-env-value "$FS_LOG" "CXL_${LLM_HEX}_SIZE")
[[ "$LLM_NS_SIZE" =~ ^[0-9]+$ ]] && [ "$LLM_NS_SIZE" -ge "$LLM_IMAGE_SIZE" ] && [ "$LLM_NS_SIZE" -le "$LLM_DEV_SIZE" ] ||
    error "the fsdax namespace of the writer has $LLM_NS_SIZE bytes, expected the image ($LLM_IMAGE_SIZE) at least and the device ($LLM_DEV_SIZE) at most"
grep -qE "^block device: /dev/pmem[0-9]+$" <<< "$FS_LOG" || error "the writer has no block device: $(grep 'block device' <<< "$FS_LOG")"
grep -qx "dd exit 0" <<< "$FS_LOG" || error "dd of the writer failed: $(grep '^dd exit' <<< "$FS_LOG")"
grep -qx "device sha256: $LLM_IMAGE_SHA256" <<< "$FS_LOG" ||
    error "the device of the writer does not have the image (sha256 $LLM_IMAGE_SHA256): $(grep '^device sha256:' <<< "$FS_LOG")"
dra-json "$VM2" resourceclaim -n "$DRA_NS" llm-writer
dra-assert "$COMMAND_OUTPUT" "allocated_node() == '$VM1_NAME' and quantity(j['status']['allocation']['devices']['results'][0]['consumedCapacity']['shares']) == $DRA_SHARES"
dra-assert "$COMMAND_OUTPUT" "device_data('$LLM_DEVICE')['exclusive'] == True and device_data('$LLM_DEVICE').get('persistent') == True"
uid=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
cdi=$(dra-cdi-device "$VM1" "claim-$uid-0")
echo "CDI device claim-$uid-0: $cdi"
[ -n "$cdi" ] || error "no CDI device claim-$uid-0 for the writer"
dra-assert "$cdi" "j['kind'] == '$DRA_POOL_DRIVER/device'"
# The device node is the CXL_<hex>_PATH of the writer, a block device.
pmem=$(dra-env-value "$FS_LOG" "CXL_${LLM_HEX}_PATH")
dra-assert "$cdi" "any(n.get('path') == '$pmem' and n.get('type') == 'b' for n in j['deviceNodes'])"
pool-attachments "$LLM_DEVICE"
pool-assert "$COMMAND_OUTPUT" "[(a['host'], a.get('singleHost', False)) for a in j] == [('$VM1_NAME', True)]"
# The host: the labels of the namespace in the LSA file, the model in the
# data file at a 2 MiB-aligned offset (the fsdax namespace data and the
# erofs file data are both 2 MiB aligned).
llm-file-zero "$LLM_LSA" 0 "$LLM_LSA_SIZE" && error "host: the LSA file $LLM_LSA is still all zeros: no namespace labels"
offset=$(llm-file-find "$LLM_DATA" "$LLM_MODEL" $(( 2 << 20 ))) ||
    error "host: the model (sha256 $LLM_MODEL_SHA256) is at no 2 MiB-aligned offset of $LLM_DATA"
echo "host: $LLM_LSA has labels, $LLM_DATA has the model at offset $offset ($(( offset >> 20 )) MiB)"
dra-delete-pod "$VM2" writer
dra-delete "$VM2" resourceclaim llm-writer --wait=true --timeout=60s
llm0-free
SHA_DATA=$(llm-file-sha256 "$LLM_DATA")
SHA_LSA=$(llm-file-sha256 "$LLM_LSA")
echo "host: sha256 of $LLM_DATA $SHA_DATA, of $LLM_LSA $SHA_LSA"

echo "### C: three direct readers map the model from CXL memory, two on VM2 (direct-b dynamically) and one on VM1"
# The lease readers claim the lease device of their node through this
# template: the claim lives and dies with their pod.
dra-apply "$VM2" <<< "$(dra-lease-template-yaml "$LEASE_TEMPLATE")"
# Page cache proofs: the page cache charged to the pod of a reader (its
# first toucher is charged; the warm-up pod already holds the files of the
# image) is the exact measure of what the reader put there. The page cache
# of the whole node is a coarse upper bound only: it moves by tens of MB
# from things outside the test and grows by less than a copy when the node
# reclaims older page cache meanwhile, while a copy of the model is at
# least 90% of it, so a growth below 50% still proves that no copy was
# made.
for r in "${DIRECT[@]}"; do
    reader-start "$r"
    # The weights are not copied anywhere.
    cache-assert "$r" -lt "$LLM_FIVE_PCT" -lt "$LLM_HALF" "a direct reader copied the model into the page cache"
done
for r in "${DIRECT[@]}"; do
    reader-check "$r" "$CHAIN_CXL"
done
for vm in "$VM2" "$VM1"; do
    mounts-assert "$vm" direct
done
pool-attachments "$LLM_DEVICE"
pool-assert "$COMMAND_OUTPUT" "sorted((a['host'], a.get('singleHost', False), a['state']) for a in j) == sorted([('$VM1_NAME', False, 'attached'), ('$VM2_NAME', False, 'attached')])"

echo "### D: three cached readers map the model from the page cache of their node, started one by one (cached-c dynamically)"
for r in "${CACHED[@]}"; do
    vm="${READER_VM[$r]}"
    first=1
    for o in "${CACHED[@]}"; do
        [ "$o" != "$r" ] || break
        [ "${READER_VM[$o]}" != "$vm" ] || first=""
    done
    reader-start "$r"
    if [ -n "$first" ]; then
        # The first cached reader of a node reads the model into the page
        # cache of the node, once, and is charged for it: the charge of
        # its pod is the measure, the growth of the page cache of the node
        # an upper bound only.
        [ "$POD_FILE" -ge "$LLM_NINETY_PCT" ] ||
            error "$r: the page cache charged to its pod is $POD_FILE bytes, less than 90% of the model ($LLM_NINETY_PCT): the first cached reader of the node did not read the model into the page cache"
        # One copy: no second one in the page cache of the pmem block
        # device under the loop device (direct I/O).
        [ "$CACHE_DELTA" -lt "$LLM_ONE_HALF" ] ||
            error "$r: the page cache of $(vmdir-label "$vm") grew by $CACHE_DELTA bytes, not less than 150% of the model ($LLM_ONE_HALF): the model is cached twice"
        echo "page cache ok: $r: pod $POD_FILE -ge $LLM_NINETY_PCT, node +$CACHE_DELTA -lt $LLM_ONE_HALF"
        # It pulled the model from CXL: the loop device is the only path.
        [ "$LOOP_READ" -ge "$LLM_NINETY_PCT" ] ||
            error "$r: ${READER_LOOP[$r]} read $LOOP_READ bytes from CXL, less than 90% of the model ($LLM_NINETY_PCT)"
    else
        # The others of the node share that copy and read nothing from CXL.
        cache-assert "$r" -lt "$LLM_FIVE_PCT" -lt "$LLM_HALF" "a second copy of the model in the page cache of the node"
        [ "$LOOP_READ" -lt "$LLM_FIVE_PCT" ] ||
            error "$r: ${READER_LOOP[$r]} read $LOOP_READ bytes from CXL, not less than 5% of the model ($LLM_FIVE_PCT)"
    fi
done
for r in "${CACHED[@]}"; do
    reader-check "$r" "$CHAIN_RAM"
done
for vm in "$VM2" "$VM1"; do
    mounts-assert "$vm" direct cached
done

echo "### D2: a static and a dynamic mount of the same device on VM2: the device is released only when both are gone"
# Without direct-a, the lease of direct-b is the only user of the direct
# node mount of VM2; the cached readers of VM2 keep the region.
LEASE_B="${READER_LEASE[direct-b]}"
BIND_B="$DRA_LEASE_FS_DIR/${READER_CLAIM_UID[direct-b]}/${READER_PATH[direct-b]##*/}"
readers-stop --keep direct-a
mounts-assert "$VM2" direct cached
lease-bind-assert direct-b
vm-region-assert "$VM2"
llm-ask "$VM2" direct-b
[ "$LLM_CONTENT" == "$LLM_EXPECT" ] || error "direct-b answered '$LLM_CONTENT', expected '$LLM_EXPECT'"
# The release cannot finish while llama-server maps the model: the driver
# cannot unmount the bind (EBUSY), kubelet retries the Unprepare of the
# companion claim, the companion pod stays Terminating.
dra-exec "$VM2" direct-b/main "$DRA_REQUEST_CTR_BIN" release --lease "$LEASE_B" --wait 5s &&
    error "cxl-request release of lease $LEASE_B of direct-b succeeded while llama-server maps the model"
[ "$COMMAND_STATUS" == 3 ] || error "cxl-request release of lease $LEASE_B exited $COMMAND_STATUS, expected 3 (not Released in time)"
dra-assert "$(dra-last-json "$COMMAND_OUTPUT")" "j.get('lease') == '$LEASE_B' and j.get('state') == 'Releasing'"
busy_re="lease $LEASE_B: unmounting ${BIND_B//./\\.}: .*device or resource busy"
retry-until --timeout 60 --interval 1 --message "the driver of VM2 cannot unmount $BIND_B: busy" \
    'dra-driver-log "$VM2" | grep -qE "$busy_re"' ||
    error "the driver of VM2 did not log that it cannot unmount $BIND_B (lease $LEASE_B: unmounting ...: device or resource busy)"
dra-driver-log "$VM2" | grep -E "$busy_re" | tail -n 2
lease-bind-assert direct-b
mounts-assert "$VM2" direct cached
vm-region-assert "$VM2"
llm-ask "$VM2" direct-b
[ "$LLM_CONTENT" == "$LLM_EXPECT" ] || error "direct-b answered '$LLM_CONTENT' while its lease is Releasing, expected '$LLM_EXPECT'"
dra-json "$VM2" pod -n "$DRA_NS" "cxl-lease-$LEASE_B" ||
    error "the companion pod cxl-lease-$LEASE_B is gone while its bind is busy"
dra-assert "$COMMAND_OUTPUT" "j['metadata'].get('deletionTimestamp')"
dra-json "$VM2" resourceclaim -n "$DRA_NS" "cxl-lease-$LEASE_B" ||
    error "the companion claim cxl-lease-$LEASE_B is gone while its bind is busy"
dra-leases "$VM2" direct-b/main
dra-assert "$DRA_LEASES" "[(l['lease'], l['state']) for l in j] == [('$LEASE_B', 'Releasing')]"
pool-attachments "$LLM_DEVICE"
pool-assert "$COMMAND_OUTPUT" "('$VM2_NAME', 'attached') in [(a['host'], a['state']) for a in j]"
# The user of the mount goes: cxl-request passes the SIGKILL of
# llama-server on as its exit status, the container ends and the pod
# fails (restartPolicy Never). The broker releases the leases of the pod,
# the next Unprepare of the companion claim unmounts the bind, the node
# mount it was the last user of, and keeps the region for the cached
# readers.
pid=$(llm-server-pid "$VM2" direct-b) || error "no llama-server runs in direct-b"
vmdir-command "$VM2" "kill -KILL $pid" || command-error "cannot kill llama-server (pid $pid) of direct-b"
dra-pod-wait "$VM2" direct-b Failed 60
dra-json "$VM2" pod -n "$DRA_NS" direct-b
dra-assert "$COMMAND_OUTPUT" "j['status']['containerStatuses'][0]['state']['terminated']['exitCode'] == 137"
dra-wait "$VM2" d2-lease-b-gone "pods,resourceclaims -n $DRA_NS" "'cxl-lease-$LEASE_B' not in names()" 90
dra-lease-fs-mounts "$VM2" || error "cannot read the lease mounts of $(vmdir-label "$VM2")"
[ "$COMMAND_OUTPUT" == "[]" ] || error "$(vmdir-label "$VM2") still has mounts under $DRA_LEASE_FS_DIR: $COMMAND_OUTPUT"
mounts-assert "$VM2" cached
vm-region-assert "$VM2"
for r in cached-a cached-b; do
    llm-ask "$VM2" "$r"
    [ "$LLM_CONTENT" == "$LLM_EXPECT" ] || error "$r answered '$LLM_CONTENT' after direct-b, expected '$LLM_EXPECT'"
done
# VM1 is unchanged: its direct claim and its cached lease.
mounts-assert "$VM1" direct cached
lease-bind-assert cached-c
dra-pod-wait "$VM2" "cxl-lease-${READER_LEASE[cached-c]}" Running 5
# The pod goes with its lease claim; the broker removes the directories of
# the claim once it is closed and no bind is left in them.
dra-delete-pod "$VM2" direct-b
dra-wait "$VM2" d2-lease-claim-b-gone "resourceclaims -n $DRA_NS" "'${READER_CLAIM[direct-b]}' not in names()" 60
lease-dirs-gone direct-b

echo "### E: all readers stop, nothing is left"
readers-stop direct-c "${CACHED[@]}"
dra-delete "$VM2" resourceclaimtemplate "$LEASE_TEMPLATE" --wait=true --timeout=60s
lease-dirs-gone cached-c
dra-assert-clean "$VM2" "$VM1"
for vm in "$VM2" "$VM1"; do
    label=$(vmdir-label "$vm")
    lease-fs-empty "$vm"
    qtree=$(pool-vm-monitor "$vm" "info qtree -b") || error "cannot read the qemu device tree of $label"
    grep -q '^ *bus: ' <<< "$qtree" || error "no qemu device tree of $label: $qtree"
    ! grep -q 'dev: cxl-type3' <<< "$qtree" || error "qemu of $label still has a CXL memory device"
done
# erofs never writes the device, and the readers never touch the labels.
[ "$(llm-file-sha256 "$LLM_DATA")" == "$SHA_DATA" ] || error "host: the readers changed $LLM_DATA"
[ "$(llm-file-sha256 "$LLM_LSA")" == "$SHA_LSA" ] || error "host: the readers changed $LLM_LSA"
echo "no pods, claims, leases, mounts, lease directories, loop devices, CDI devices, memory devices or regions of $LLM_DEVICE left; its data and labels are as the writer left them"

echo "### what every component of VM2 and VM1 did, in order"
dra-trace-stop
dra-trace-summary

echo "### cleanup"
trap - EXIT
exit-cleanup
pool-port-busy && error "port $POOL_PORT is still busy after stopping the server"
[ ! -e "$LLM_DATA" ] && [ ! -e "$LLM_LSA" ] || error "$LLM_DATA or $LLM_LSA is left, see exit-cleanup above"
echo "trace: $TEST_OUTPUT_DIR/trace.txt, summary: $TEST_OUTPUT_DIR/trace-summary.txt"
