# One exclusive writer, many read-only readers of shared CXL memory, on two
# nodes of one Kubernetes cluster
#
# Single-writer data sharing: a writer gets a shared CXL memory device of
# the pool for itself, writes new contents into it and exits. Then several
# readers on several nodes use the same memory at the same time, read-only.
# All of them lease the memory while they run (test12): "cxl-request
# request --serial SERIAL" in the container, the driver passes a dax
# device fd.
#
# - Exclusive: "cxl-request request --serial SERIAL" without --share asks
#   for no "shares" of the device, which takes all of them (1M in the pool
#   ResourceSlice, without a requestPolicy): the device exclusively.
#   While the writer has it, the scheduler cannot allocate the device to
#   any other claim: the readers' requests stay Pending, and
#   fake-cxl-pool-server refuses to attach the device to any other host
#   (the attachment is exclusive, which also keeps out other clusters
#   sharing the pool).
# - Read-only: "cxl-request request --serial SERIAL --share --read-only"
#   takes one share of the device (shares: "1") and gets an fd opened
#   O_RDONLY: the reader can map the memory for reading only, a writable
#   mapping is refused (EACCES) whatever the capabilities of the
#   container. A claim of a pod with the opaque config MemoryConfig
#   device.readOnly: true gets a device node that the container can only
#   open for reading.
#
# The cluster: the VM of n4-cxl-shared-2 (VM2, the VM of the test) is the
# control plane, the VMs of n4-cxl-shared-1 (VM1) and n4-cxl-shared-0
# (VM0) workers, over the VM network "cxl-pool" (vm_network.var.sh);
# test00-up of n4-cxl-shared-2 makes it. The pool is cabled to VM1 and VM2
# only, and the test uses those two nodes. The pool controller runs once,
# on VM2, the driver on every node: one DaemonSet, installed with helm on
# VM2, whose pod on VM1 starts at once
# with the image that dra-install imported there.
#
# What you will see: the writer on VM1 holding shared0 exclusively, three
# readers (two on VM2, one on VM1) asking meanwhile and waiting, the
# writer exiting and all three readers getting the device at once, reading
# what the writer wrote, holding the memory at the same time and failing
# to map it for writing; a late exclusive request that times out without
# leftovers; a fourth reader with a read-only claim from its creation;
# and the trace of VM2 and VM1.
#
# Run test00-up of n4-cxl-shared-1, -0 and -2 first: they create the VMs and
# make them one cluster. The DRA driver comes from its image, build it
# first:
#   make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

echo "### preconditions: one cluster, VM2 control plane, VM1 and VM0 workers (test00-up)"
pool-cluster-require

VM1="$POOL_VM1_DIR"    # worker
VM2="$OUTPUT_DIR"      # control plane, the VM of this test
VM0_NAME="$POOL_VM0_NAME"   # the other worker, not cabled to the pool
VM1_NAME="$POOL_VM1_NAME"
VM2_NAME="$POOL_VM2_NAME"
SHARED_SERIAL=0xc1ae0001    # 0xc1ae....: shared pool devices
SHARED_HEX=C1AE0001         # the serial in environment variable names
TAG="dra-one-writer $(date +%Y-%m-%dT%H:%M:%S.%N)"
TEXT="$TAG: written by the exclusive writer"
READERS=(reader-a reader-b reader-c)
declare -A READER_VM=([reader-a]="$VM2" [reader-b]="$VM2" [reader-c]="$VM1")

# shellcheck disable=SC2001 # sed indents every line of SCRIPT
lease-pod-yaml() {
    # Usage: lease-pod-yaml NAME VMDIR SCRIPT
    #
    # Pod NAME on the node of VMDIR, restartPolicy Never, with a lease claim
    # of its own (template cxl-lease) and the dax-rw tool in /tools, running
    # the shell SCRIPT in its container "main". The pod ends with SCRIPT,
    # unlike one of dra-script-pod-yaml.
    local name="$1" node script="$3"
    node=$(basename "$2")
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
spec:
  nodeSelector:
    kubernetes.io/hostname: $node
  terminationGracePeriodSeconds: 2
  restartPolicy: Never
  resourceClaims:
  - name: cxl
    resourceClaimTemplateName: cxl-lease
  volumes:
  - name: tools
    configMap:
      name: tools
  containers:
  - name: main
    image: $DRA_TOOLS_IMAGE
    imagePullPolicy: IfNotPresent
    command:
    - sh
    - -c
    - |
$(sed 's/^/      /' <<< "$script")
    volumeMounts:
    - name: tools
      mountPath: /tools
    resources:
      claims:
      - name: cxl
EOF
}

lease-of() {
    # Usage: lease-of POD
    #
    # Print the lease id that pod POD logged ("lease ID ...").
    dra-pod-log "$VM2" "$1" | sed -n 's/^lease \([0-9a-f]\{8\}\) .*/\1/p' | tail -n 1
}

companions-of() {
    # Usage: companions-of POD
    #
    # Print "NAME PHASE" of the companion pods of the leases of pod POD.
    local uid
    uid=$(vmdir-command-q "$VM2" "kubectl get pod -n $DRA_NS $1 -o jsonpath={.metadata.uid}")
    vmdir-command-q "$VM2" "kubectl get pods -n $DRA_NS -l $DRA_REQUESTER_LABEL=$uid -o jsonpath='{range .items[*]}{.metadata.name}{\" \"}{.status.phase}{\"\\n\"}{end}'"
}

echo "### preconditions: patched qemu, hdm_for_passthrough=on, CONFIG_FS_DAX"
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"
pool-vm-require "$VM1" n4-cxl-shared-1

echo "### preconditions: cxl, daxctl and cxl-dump in both VMs, no CXL memory devices"
pool-vm-tools-install "$VM2"
pool-vm-tools-install "$VM1"
pool-vm-reset "$VM2"
pool-vm-reset "$VM1"
for vm in "$VM2" "$VM1"; do
    pool-cxl-dump "$vm" no-devices
    cxl-assert 'memdevs == [] and regions == [] and endpoints == []'
done

echo "### preconditions: fake-cxl-pool-server with shared0"
pool-server-start "
  - name: shared0
    size: 256Mi
    shared: true
    serial: $SHARED_SERIAL"
pool-client-install "$VM2" "$VM1"

echo "### preconditions: the pool controller on the control plane, the driver on every node"
dra-build
dra-install
for vm in "$VM2" "$VM1"; do
    dra-pull-images "$vm" "$DRA_TOOLS_IMAGE" "$DRA_COMPANION_IMAGE"
done
dra-tools-install "$VM2"
dra-apply "$VM2" <<< "$(dra-lease-template-yaml)"
dra-trace-start "$VM2" "$VM1"

echo "### A: one cluster of three nodes, one pool for VM1 and VM2, the lease device on every node"
dra-kubectl "$VM2" "get nodes -o wide"
dra-json "$VM2" nodes
dra-assert "$COMMAND_OUTPUT" "sorted(n['metadata']['name'] for n in j['items']) == sorted(['$VM0_NAME', '$VM1_NAME', '$VM2_NAME'])"
dra-kubectl "$VM2" "get resourceslices"
dra-json "$VM2" resourceslices
S0="device('shared0', '$DRA_POOL_DRIVER')"
dra-assert "$COMMAND_OUTPUT" "len(slices('$DRA_POOL_DRIVER')) == 1 and all(slices('$DRA_LOCAL_DRIVER', n) for n in ['$VM0_NAME', '$VM1_NAME', '$VM2_NAME'])"
dra-assert "$COMMAND_OUTPUT" "all(attr(device('lease', '$DRA_LOCAL_DRIVER', n), 'type') == 'lease' for n in ['$VM0_NAME', '$VM1_NAME', '$VM2_NAME'])"
# "shares" without a requestPolicy: a claim takes one share ("1") or, with
# no shares request, all of them (exclusive).
dra-assert "$COMMAND_OUTPUT" "sorted(${S0}.get('capacity') or {}) == ['shares'] and quantity(capacity($S0, 'shares')) == $DRA_SHARES and request_policy($S0, 'shares') is None"

echo "### B: the writer on VM1 gets shared0 exclusively and writes"
# Without --share, a request of a shared device takes all of its shares.
dra-apply "$VM2" <<< "$(lease-pod-yaml writer "$VM1" "echo \"writer: cxl-request request --serial $SHARED_SERIAL\"
cxl-request request --serial $SHARED_SERIAL --wait 120s --exec sh -c '
echo \"lease \$CXL_LEASE read-only \$CXL_${SHARED_HEX}_READ_ONLY shared \$CXL_${SHARED_HEX}_SHARED fd \$CXL_${SHARED_HEX}_FD\"
python3 /tools/dax-rw.py --fd 3 write 0 \"\$1\" || exit 1
echo \"written\"
while [ ! -f /tmp/done ]; do sleep 0.2; done
echo \"writer done\"' sh \"$TEXT\"
echo \"cxl-request exit \$?\"")"
dra-wait-log "$VM2" writer written 120
WRITER_LEASE=$(lease-of writer)
[ -n "$WRITER_LEASE" ] || error "the writer logged no lease id"
dra-pod-log "$VM2" writer | grep -qx "lease $WRITER_LEASE read-only 0 shared 1 fd 3" ||
    error "the writer has not CXL_${SHARED_HEX}_READ_ONLY=0, _SHARED=1 and _FD=3"
dra-wait "$VM2" writer-companion "resourceclaim -n $DRA_NS cxl-lease-$WRITER_LEASE" \
    "condition('$DRA_POOL_DRIVER/Attached') == 'True'" 30
# The companion asks for the shared device exclusively: one request of
# class cxl-pool-shared without a capacity request, which consumes all
# shares (the serial is that of a shared pool device: no local
# subrequest).
dra-assert "$COMMAND_OUTPUT" "[(s['name'], s['deviceClassName'], s.get('capacity')) for s in subrequests()] == [('lease', 'cxl-pool-shared', None)] and j['spec']['devices']['requests'][0].get('exactly')"
dra-assert "$COMMAND_OUTPUT" "allocated_node() == '$VM1_NAME' and quantity(j['status']['allocation']['devices']['results'][0]['consumedCapacity']['shares']) == $DRA_SHARES"
dra-assert "$COMMAND_OUTPUT" "device_data('shared0')['exclusive'] == True"
dra-assert "$COMMAND_OUTPUT" "[(c['driver'], c['parameters']['apiVersion'], c['parameters']['kind'], config_section(c, 'device').get('mode'), bool(config_section(c, 'device').get('readOnly'))) for c in configs()] == [('$DRA_POOL_DRIVER', '$DRA_CONFIG_API', '$DRA_CONFIG_KIND', 'dax', False)]"
dra-assert "$COMMAND_OUTPUT" "j['metadata']['labels']['$DRA_LEASE_LABEL'] == '$WRITER_LEASE' and j['metadata']['annotations']['$DRA_REQUESTER_ANNOTATION'] == '$DRA_NS/writer/main'"
dra-claim-conditions "$VM2" "cxl-lease-$WRITER_LEASE"
pool-attachments shared0
pool-assert "$COMMAND_OUTPUT" "[(a['host'], a.get('singleHost', False)) for a in j] == [('$VM1_NAME', True)]"
pool-host-client -o json devices shared0 || command-error "cannot get shared0"
SHARED_FILE=$(pool-value "$COMMAND_OUTPUT" 'j["path"]')
found=$(pool-file-string "$SHARED_FILE" 0)
[ "$found" == "$TEXT" ] || error "host: $SHARED_FILE has '$found' at 0, expected '$TEXT'"
echo "host read at 0 of $SHARED_FILE: '$found'"

echo "### C: three readers ask meanwhile, two on VM2 and one on VM1: they wait"
for r in "${READERS[@]}"; do
    dra-apply "$VM2" <<< "$(lease-pod-yaml "$r" "${READER_VM[$r]}" "echo \"$r: cxl-request request --serial $SHARED_SERIAL --share --read-only\"
cxl-request request --serial $SHARED_SERIAL --share --read-only --wait 300s --exec sh -c '
echo \"lease \$CXL_LEASE read-only \$CXL_${SHARED_HEX}_READ_ONLY\"
python3 /tools/dax-rw.py --fd 3 check-ro 0 || exit 1
exec python3 /tools/dax-rw.py --fd 3 watch 0 /tmp/stop'
echo \"cxl-request exit \$?\"
cxl-request release --all
trap 'exit 0' TERM; sleep infinity & wait")"
done
# The companion of each reader waits: the scheduler finds the device
# taken by the writer (PodScheduled False, reason Unschedulable), and
# its claim has no allocation.
for r in "${READERS[@]}"; do
    dra-pod-wait "$VM2" "$r" Running 120
    # shellcheck disable=SC2016 # evaluated by retry-until
    retry-until --timeout 60 --interval 1 --message "$r: a Pending companion" \
        '[ "$(companions-of "$r" | awk "{print \$2}")" == Pending ]' ||
        error "$r has no Pending companion pod: $(companions-of "$r")"
    companion=$(companions-of "$r" | awk '{print $1}')
    dra-wait "$VM2" "$r-unschedulable" "pod -n $DRA_NS $companion" \
        "j['status']['phase'] == 'Pending' and [c['reason'] for c in j['status'].get('conditions', []) if c['type'] == 'PodScheduled'] == ['Unschedulable']" 60
    dra-json "$VM2" resourceclaim -n "$DRA_NS" "$companion"
    dra-assert "$COMMAND_OUTPUT" "j['status'].get('allocation') is None"
    dra-leases "${READER_VM[$r]}" "$r"
    dra-assert "$DRA_LEASES" "[(l['state'], bool(l.get('readOnly')), bool(l.get('share')), l.get('mode')) for l in j] == [('Pending', True, True, 'dax')]"
done
pool-attachments shared0
pool-assert "$COMMAND_OUTPUT" "[(a['host'], a.get('singleHost', False)) for a in j] == [('$VM1_NAME', True)]"

echo "### D: the writer exits, the readers get the device"
dra-exec "$VM1" writer touch /tmp/done || command-error "cannot tell the writer to finish"
dra-pod-wait "$VM2" writer Succeeded 60
dra-wait-log "$VM2" writer "cxl-request exit 0" 10
for r in "${READERS[@]}"; do
    dra-wait-log "$VM2" "$r" holding 120
done
retry-until --timeout 60 --interval 1 --message "the companion of the writer is gone" \
    '[ -z "$(vmdir-command-q "$VM2" "kubectl get pods,resourceclaims -n $DRA_NS -l $DRA_LEASE_LABEL=$WRITER_LEASE -o name")" ]' ||
    error "the lease of the writer left $(vmdir-command-q "$VM2" "kubectl get pods,resourceclaims -n $DRA_NS -l $DRA_LEASE_LABEL=$WRITER_LEASE -o name")"

echo "### E: the readers read the same bytes at the same time, read-only"
for r in "${READERS[@]}"; do
    dra-kubectl "$VM2" "logs -n $DRA_NS $r"
    grep -qxF "read: $TEXT" <<< "$COMMAND_OUTPUT" || error "$r did not read '$TEXT'"
    grep -qxF "writable mapping refused: EACCES" <<< "$COMMAND_OUTPUT" ||
        error "$r could map the memory for writing"
    grep -qE '^lease [0-9a-f]{8} read-only 1$' <<< "$COMMAND_OUTPUT" ||
        error "$r has no read-only lease"
    dra-leases "${READER_VM[$r]}" "$r"
    dra-assert "$DRA_LEASES" "[(l['state'], l.get('mode'), bool(l.get('share')), bool(l.get('readOnly')), l.get('serial')) for l in j] == [('Ready', 'dax', True, True, '$SHARED_SERIAL')]"
done
# All three hold their mapping now: none has its stop file yet.
for r in "${READERS[@]}"; do
    dra-pod-log "$VM2" "$r" | grep -q '^final: ' && error "$r stopped holding before the others read"
done
pool-attachments shared0
pool-assert "$COMMAND_OUTPUT" "sorted((a['host'], a.get('singleHost', False), a['state']) for a in j) == sorted([('$VM1_NAME', False, 'attached'), ('$VM2_NAME', False, 'attached')])"
# The companion claims of the readers ask for read-only devices, one share
# of shared0 each. The readers get read-only fds from the driver
# (check-ro above); the companion pods use nothing of their claims, which
# get no CDI device.
READER_A_LEASE=$(lease-of reader-a)
dra-json "$VM2" resourceclaim -n "$DRA_NS" "cxl-lease-$READER_A_LEASE"
dra-assert "$COMMAND_OUTPUT" "[c['parameters'] for c in configs()] == [{'apiVersion': '$DRA_CONFIG_API', 'kind': '$DRA_CONFIG_KIND', 'device': {'mode': 'dax', 'readOnly': True}}]"
dra-assert "$COMMAND_OUTPUT" "[(s['deviceClassName'], s.get('capacity')) for s in subrequests()] == [('cxl-pool-shared', {'requests': {'shares': '1'}})]"
READER_A_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
[ -z "$(dra-cdi-devices-with "$VM2" "$READER_A_UID")" ] ||
    error "the companion claim of reader-a has CDI devices: $(dra-cdi-devices-with "$VM2" "$READER_A_UID")"
for vm in "$VM2" "$VM1"; do
    pool-cxl-dump "$vm" readers
    cxl-assert "by_serial($SHARED_SERIAL) is not None and len(regions) == 1 and online_size(regions[0]) == 0"
done
found=$(pool-file-string "$SHARED_FILE" 0)
[ "$found" == "$TEXT" ] || error "host: $SHARED_FILE has '$found' at 0, expected '$TEXT'"

echo "### E2: a new exclusive request cannot take the device from the readers"
dra-exec "$VM2" reader-a cxl-request request --serial "$SHARED_SERIAL" --wait 15s
[ "$COMMAND_STATUS" == 3 ] || error "an exclusive request while readers hold shared0 exited $COMMAND_STATUS, expected 3 (not Ready in time)"
json=$(dra-last-json "$COMMAND_OUTPUT") || error "the late exclusive request printed no JSON"
dra-assert "$json" "j['state'] == 'Failed' and 'timeout' in j['error']"
# shellcheck disable=SC2034 # used in the snippet of retry-until
LATE_LEASE=$(dra-value "$json" "j['lease']")
retry-until --timeout 60 --interval 1 --message "nothing left of the late exclusive lease" \
    '[ -z "$(vmdir-command-q "$VM2" "kubectl get pods,resourceclaims -n $DRA_NS -l $DRA_LEASE_LABEL=$LATE_LEASE -o name")" ]' ||
    error "the late exclusive lease left objects behind"
for r in "${READERS[@]}"; do
    dra-pod-log "$VM2" "$r" | grep -q '^final: ' && error "$r lost its memory to the late exclusive request"
done

echo "### F: a fourth reader, on VM1, with a read-only claim from its creation"
dra-apply "$VM2" <<< "$(dra-shared-claim-yaml shared-read-only "$SHARED_SERIAL" 1 "readOnly: true")"
dra-apply "$VM2" <<< "$(dra-script-pod-yaml reader-d "$VM1" shared-read-only "env | grep ^CXL_${SHARED_HEX}_ | sort
python3 /tools/dax-rw.py check-ro \"\$CXL_${SHARED_HEX}_DAX\" 0 || exit 1
echo \"read: \$(python3 /tools/dax-rw.py read \"\$CXL_${SHARED_HEX}_DAX\" 0)\"")"
dra-pod-wait "$VM2" reader-d Running 120
dra-wait-log "$VM2" reader-d "read: $TEXT" 60
dra-kubectl "$VM2" "logs -n $DRA_NS reader-d"
# readOnly without a mode: mode dax, the only mode of a shared device.
dra-env-assert reader-d "$COMMAND_OUTPUT" "$SHARED_HEX" \
    SERIAL="$SHARED_SERIAL" SHARED=1 MODE=dax SIZE=$(( 256 << 20 )) "DAX=/dev/dax*" READ_ONLY=1
for line in "open for writing refused: EPERM" "writable mapping refused: EACCES"; do
    grep -qxF "$line" <<< "$COMMAND_OUTPUT" || error "reader-d did not log '$line'"
done
dra-json "$VM2" resourceclaim -n "$DRA_NS" shared-read-only
dra-assert "$COMMAND_OUTPUT" "allocated_node() == '$VM1_NAME'"
# The pool driver on VM1 prepared the claim as the CDI device of its
# allocation result, claim-<uid>-0 (spec cxl-pool-claims.json), with a read-only
# device node, the environment of shared0 and the marker of the result,
# CXL_CLAIM_<uid>_0.
RO_CLAIM_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
cdi=$(dra-cdi-device "$VM1" "claim-$RO_CLAIM_UID-0")
echo "CDI device claim-$RO_CLAIM_UID-0: $cdi"
[ -n "$cdi" ] || error "no CDI device claim-$RO_CLAIM_UID-0 in VM1"
dra-assert "$cdi" "j['kind'] == '$DRA_POOL_DRIVER/device' and any(n.get('permissions') == 'r' for n in j['deviceNodes'])"
dra-assert "$cdi" "'CXL_${SHARED_HEX}_READ_ONLY=1' in j['env'] and 'CXL_CLAIM_${RO_CLAIM_UID//-/_}_0=1' in j['env']"
[ "$(dra-cdi-devices-with "$VM1" "$RO_CLAIM_UID")" == "claim-$RO_CLAIM_UID-0" ] ||
    error "the claim of reader-d has other CDI devices than claim-$RO_CLAIM_UID-0: $(dra-cdi-devices-with "$VM1" "$RO_CLAIM_UID")"
for r in "${READERS[@]}"; do
    dra-pod-log "$VM2" "$r" | grep -q '^final: ' && error "$r stopped holding before reader-d read"
done
echo "four readers, two on each node, read '$TEXT' at the same time"

echo "### G: the readers finish, the device goes back to the pool"
for r in "${READERS[@]}"; do
    dra-exec "${READER_VM[$r]}" "$r" touch /tmp/stop || command-error "cannot stop $r"
done
for r in "${READERS[@]}"; do
    dra-wait-log "$VM2" "$r" "final: $TEXT" 30
    dra-wait-log "$VM2" "$r" "cxl-request exit 0" 30
done
dra-delete "$VM2" pod reader-d --grace-period=2 --wait=true --timeout=60s
dra-delete "$VM2" resourceclaim shared-read-only --wait=true --timeout=60s
for r in "${READERS[@]}"; do
    retry-until --timeout 60 --interval 1 --message "$r has no leases" \
        '[ -z "$(companions-of "$r")" ]' || error "$r still has companions: $(companions-of "$r")"
done
pool-device-free-wait shared0 "$VM2" "$VM1"
# The writer and the readers, done, and the template of their lease
# claims go too.
for p in writer "${READERS[@]}"; do
    dra-delete-pod "$VM2" "$p"
done
dra-delete "$VM2" resourceclaimtemplate cxl-lease --wait=true --timeout=60s
for vm in "$VM2" "$VM1"; do
    pool-vm-monitor "$vm" "info qtree -b" | grep -q 'dev: cxl-type3' &&
        error "qemu of $(vmdir-label "$vm") still has a CXL memory device"
done

dra-assert-clean "$VM2" "$VM1"

echo "### I: what every component of VM2 and VM1 did, in order"
dra-trace-stop
dra-trace-summary

echo "### cleanup"
dra-cleanup
pool-cleanup
trap - EXIT
pool-port-busy && error "port $POOL_PORT is still busy after stopping the server"
echo "trace: $TEST_OUTPUT_DIR/trace.txt, summary: $TEST_OUTPUT_DIR/trace-summary.txt"
