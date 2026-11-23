# Several CXL memory pools in one Kubernetes cluster with DRA
#
# In a data centre a CXL memory pool appliance serves the servers of its
# rack, some eight of them, and a cluster spans racks: the cluster has
# several pools, and each node reaches only the pools it is cabled to. A
# pod gets pool memory only on a node that reaches a pool with a free
# device for its claim, so the scheduler must know which nodes reach which
# pool.
#
# Here every pool is a fake-cxl-pool-server on the host with a pool
# controller of its own in the cluster: three servers, three controllers
# (systemd units fake-cxl-pool-controller for the first one,
# fake-cxl-pool-controller-<name> for the others), one DRA driver
# cxl-pool.nri.io. Each controller publishes the devices of its server as a
# DRA pool named after the server, in a ResourceSlice whose nodeSelector
# lists the nodes the server reaches (kubernetes.io/hostname In [...]), and
# confines itself to its own pool: the controllers do not remove each
# other's slices.
# - pool-a, the rack of VM1 and VM2: QMP, the slots of host bridge 0.
# - pool-b, the rack of VM0 (n4-cxl-shared-0): QMP, host bridge 0.
# - pool-c, cabled to every VM on host bridge 1: data shared across racks.
#   A VM gives one QMP client at a time, and pool-a and pool-b hold the QMP
#   sockets, so pool-c uses HMP (monitor.sock, a connection per command,
#   device removals polled).
# Serials are unique in the cluster: 0xc1aeP... shared and 0xc1eeP...
# unshared devices of pool index P (a 0, b 1, c 2); device names are
# unique across the servers of a VM.
#
# What you will see: the three pools and their node selectors; a pod
# without a nodeSelector going to VM0 because its claim names a device of
# pool-b, and one going to VM1 or VM2 for a device of pool-a; a claim for
# "any 512Mi of pool memory" landing on VM0 because pool-b has the only
# free one; a pod pinned to VM0 that asks for a device of pool-a staying
# Pending; one pod with devices of two pools (pool-b over QMP, pool-c over
# HMP) writing to both, and nothing of them on VM2; a pod on VM2 reading
# what VM0 wrote into pool-c, its claim without a configuration (mode dax);
# VM0 letting go while VM2 still reads; and every device back in its pool
# at the end.
#
# Run test00-up of n4-cxl-shared-1, -0 and -2 first: they create the VMs
# and make them one cluster of three nodes. Build fake-cxl-pool with the
# several-pools support (make -C scripts/testing/fake-cxl-pool) and the DRA
# driver image:
#   make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

echo "### 0: preconditions: one cluster, VM2 control plane, VM1 and VM0 workers (test00-up)"
pool-cluster-require

VM0="$POOL_VM0_DIR"    # worker, the rack of pool-b
VM1="$POOL_VM1_DIR"    # worker, the rack of pool-a
VM2="$OUTPUT_DIR"      # control plane, the rack of pool-a, the VM of this test
VM0_NAME="$POOL_VM0_NAME"
VM1_NAME="$POOL_VM1_NAME"
VM2_NAME="$POOL_VM2_NAME"
VMS=("$VM2" "$VM1" "$VM0")
POOLS=(pool-a pool-b pool-c)
declare -A POOL_HOSTS=([pool-a]="$VM1 $VM2" [pool-b]="$VM0" [pool-c]="$VM0 $VM1 $VM2")
declare -A POOL_CONTROL=([pool-a]=qmp [pool-b]=qmp [pool-c]=hmp)
declare -A POOL_BRIDGE=([pool-a]=cxlhb0 [pool-b]=cxlhb0 [pool-c]=cxlhb1)
declare -A POOL_UNIT=([pool-a]="$DRA_CONTROLLER_UNIT" [pool-b]="$DRA_CONTROLLER_UNIT-pool-b" [pool-c]="$DRA_CONTROLLER_UNIT-pool-c")
A_POOLED0_SERIAL=0xc1ee0001   # 512Mi, exclusive
A_SHARED0_SERIAL=0xc1ae0001   # 256Mi, shared
B_POOLED0_SERIAL=0xc1ee1001   # 512Mi, exclusive
B_POOLED0_HEX=C1EE1001        # the serial in environment variable names
B_SHARED0_SERIAL=0xc1ae1001   # 256Mi, shared
B_SHARED0_HEX=C1AE1001
C_SHARED0_SERIAL=0xc1ae2001   # 256Mi, shared
C_SHARED0_HEX=C1AE2001
C_POOLED0_SERIAL=0xc1ee2001   # 256Mi, exclusive
TAG="many-pools $(date +%Y-%m-%dT%H:%M:%S.%N)"
TEXT_B="$TAG: b-shared0 written on VM0"
TEXT_C="$TAG: c-shared0 written on VM0"

# shellcheck disable=SC2001 # sed indents every line of SCRIPT
pod-yaml() {
    # Usage: pod-yaml NAME NODE SCRIPT CLAIM...
    #
    # Print pod NAME, restartPolicy Never, on Kubernetes node NODE ("": no
    # nodeSelector, the scheduler picks the node) with the ResourceClaims
    # CLAIM as its claims c0, c1, ... and the tools of dra-tools-install
    # in /tools, running the shell SCRIPT in its container "main", then
    # idling until it is deleted.
    local name="$1" node="$2" script="$3" claims="" uses="" n=0 claim selector=""
    shift 3
    for claim in "$@"; do
        claims+="
  - name: c$n
    resourceClaimName: $claim"
        uses+="
      - name: c$n"
        n=$(( n + 1 ))
    done
    [ -z "$node" ] || selector="
  nodeSelector:
    kubernetes.io/hostname: $node"
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
spec:$selector
  terminationGracePeriodSeconds: 2
  restartPolicy: Never
  resourceClaims:$claims
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
      trap "exit 0" TERM; sleep infinity & wait
    resources:
      claims:$uses
    volumeMounts:
    - name: tools
      mountPath: /tools
EOF
}

pool-devices() {
    # Usage: pool-devices POOL
    #
    # Print the dra-py expression of the sorted (name, serial, shared) of
    # the devices of the slices of DRA pool POOL.
    echo "sorted((d['name'], attr(d, 'serial'), attr(d, 'shared')) for s in pool_slices('$1') for d in s['spec']['devices'])"
}

pod-node() {
    # Usage: pod-node POD
    #
    # Print the node of pod POD in $DRA_NS.
    vmdir-command-q "$VM2" "kubectl get pod -n $DRA_NS $1 -o jsonpath={.spec.nodeName}"
}

pool-attachments-of() {
    # Usage: pool-attachments-of POOL
    #
    # Print the attachments of the server POOL as JSON, and keep them in
    # COMMAND_OUTPUT.
    pool-host-client --pool "$1" -o json attachments || command-error "cannot list the attachments of $1"
}

pool-free-wait() {
    # Usage: pool-free-wait POOL
    #
    # Wait until the server POOL has no attachments, 60 s at most.
    local pool="$1"
    # shellcheck disable=SC2016 # evaluated by retry-until
    retry-until --timeout 60 --message "no attachments on $pool" \
        'pool-host-client --pool "$pool" -o json attachments >/dev/null && pool-py assert "$COMMAND_OUTPUT" "j in (None, [])"' ||
        error "$pool still has attachments: $COMMAND_OUTPUT"
}

monitors-answer() {
    # Usage: monitors-answer VMDIR...
    #
    # Fail the test unless the human monitor of the qemu of every VMDIR
    # answers "info qtree -b" with its cxl-downstream ports, while pool-c
    # uses the same monitor socket.
    local vmdir out
    for vmdir in "$@"; do
        out=$(pool-vm-monitor "$vmdir" "info qtree -b")
        grep -q 'dev: cxl-downstream' <<< "$out" ||
            error "the monitor of $(vmdir-label "$vmdir") did not answer info qtree -b: $(head -c 500 <<< "$out")"
        MONITOR_ANSWERS=$(( MONITOR_ANSWERS + 1 ))
    done
}

echo "### 0: preconditions: patched qemu, hdm_for_passthrough=on, CONFIG_FS_DAX in the three VMs"
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"
pool-vm-require "$VM1" n4-cxl-shared-1
pool-vm-require "$VM0" n4-cxl-shared-0

echo "### 0: preconditions: cxl, daxctl and cxl-dump in the VMs, no CXL memory devices"
for vm in "${VMS[@]}"; do
    pool-vm-tools-install "$vm"
done
for vm in "${VMS[@]}"; do
    pool-vm-reset "$vm"
    pool-cxl-dump "$vm" no-devices
    cxl-assert 'memdevs == [] and regions == [] and endpoints == []'
done
VM0_CPU_NODES=$(cxl-value 'len(nodes)')   # the last dump is of VM0

echo "### 0: preconditions: three pool servers on the host"
pool-server-start --name pool-a --hosts "$VM1 $VM2" --index 0 --slots "*hb0" "
  - name: a-pooled0
    size: 512Mi
    shared: false
    serial: $A_POOLED0_SERIAL
  - name: a-shared0
    size: 256Mi
    shared: true
    serial: $A_SHARED0_SERIAL"
pool-server-start --name pool-b --port 9910 --hosts "$VM0" --index 1 --slots "*hb0" "
  - name: b-pooled0
    size: 512Mi
    shared: false
    serial: $B_POOLED0_SERIAL
  - name: b-shared0
    size: 256Mi
    shared: true
    serial: $B_SHARED0_SERIAL"
pool-server-start --name pool-c --port 9911 --hosts "$VM0 $VM1 $VM2" --index 2 --control hmp --slots "*hb1" --local-devices no "
  - name: c-shared0
    size: 256Mi
    shared: true
    serial: $C_SHARED0_SERIAL
  - name: c-pooled0
    size: 256Mi
    shared: false
    serial: $C_POOLED0_SERIAL"
# Each server knows exactly the VMs it is cabled to, through its own
# monitor protocol, and only the slots of its host bridge.
for pool in "${POOLS[@]}"; do
    want=""
    for vm in ${POOL_HOSTS[$pool]}; do
        want+="'$(basename "$vm")', "
    done
    pool-host-client --pool "$pool" hosts || command-error "cannot list the hosts of $pool"
    pool-host-client --pool "$pool" -o json hosts || command-error "cannot list the hosts of $pool"
    pool-assert "$COMMAND_OUTPUT" "sorted(h['name'] for h in j) == sorted([$want])"
    pool-assert "$COMMAND_OUTPUT" "all(h.get('control') == '${POOL_CONTROL[$pool]}' and h.get('state') == 'running' for h in j)"
    pool-assert "$COMMAND_OUTPUT" "all(len(h['slots']) == 4 and all(s['hostBridge'] == '${POOL_BRIDGE[$pool]}' for s in h['slots']) for h in j)"
done
pool-client-install "$VM2" "$VM1" "$VM0"

echo "### 0: preconditions: three pool controllers, the driver on every node"
dra-build
dra-install
[ "${DRA_CONTROLLER_UNITS[*]}" == "${POOL_UNIT[pool-a]} ${POOL_UNIT[pool-b]} ${POOL_UNIT[pool-c]}" ] ||
    error "dra-install started the controllers ${DRA_CONTROLLER_UNITS[*]}, expected one per pool"
vmdir-command "$VM2" "systemctl is-active ${DRA_CONTROLLER_UNITS[*]}" ||
    command-error "the pool controllers do not all run"
for pool in "${POOLS[@]}"; do
    vmdir-command "$VM2" "journalctl --no-pager -o cat -u ${POOL_UNIT[$pool]} | grep -m1 -F 'pool $pool (from the server)'" ||
        command-error "${POOL_UNIT[$pool]} did not take the pool name $pool from its server"
done
for vm in "${VMS[@]}"; do
    dra-pull-images "$vm" "$DRA_TOOLS_IMAGE"
done
dra-tools-install "$VM2"
dra-trace-start "$VM2" "$VM1" "$VM0"

echo "### A: the pools in Kubernetes: one slice each, for the nodes it reaches"
dra-kubectl "$VM2" "get nodes -o wide"
dra-kubectl "$VM2" "get resourceslices"
dra-json "$VM2" resourceslices
SLICES="$COMMAND_OUTPUT"
dra-assert "$SLICES" "len(slices('$DRA_POOL_DRIVER')) == 3 and sorted(s['spec']['pool']['name'] for s in slices('$DRA_POOL_DRIVER')) == ['pool-a', 'pool-b', 'pool-c']"
dra-assert "$SLICES" "not any(s['spec'].get('allNodes') for s in slices('$DRA_POOL_DRIVER'))"
dra-assert "$SLICES" "pool_nodes('pool-a') == sorted(['$VM1_NAME', '$VM2_NAME'])"
dra-assert "$SLICES" "pool_nodes('pool-b') == ['$VM0_NAME']"
dra-assert "$SLICES" "pool_nodes('pool-c') == sorted(['$VM0_NAME', '$VM1_NAME', '$VM2_NAME'])"
dra-assert "$SLICES" "$(pool-devices pool-a) == [('a-pooled0', '$A_POOLED0_SERIAL', False), ('a-shared0', '$A_SHARED0_SERIAL', True)]"
dra-assert "$SLICES" "$(pool-devices pool-b) == [('b-pooled0', '$B_POOLED0_SERIAL', False), ('b-shared0', '$B_SHARED0_SERIAL', True)]"
dra-assert "$SLICES" "$(pool-devices pool-c) == [('c-pooled0', '$C_POOLED0_SERIAL', False), ('c-shared0', '$C_SHARED0_SERIAL', True)]"
dra-assert "$SLICES" "all(slices('$DRA_LOCAL_DRIVER', n) for n in ['$VM0_NAME', '$VM1_NAME', '$VM2_NAME'])"
# The slices of the three controllers, to show at the end that none of
# them removed or replaced the slice of another pool.
POOL_SLICE_IDS=$(dra-value "$SLICES" "sorted((s['spec']['pool']['name'], s['metadata']['uid']) for s in slices('$DRA_POOL_DRIVER'))")

echo "### B: a pod without a nodeSelector, a claim for b-pooled0: it goes to VM0"
dra-apply "$VM2" <<< "$(dra-shared-claim-yaml b-claim "$B_POOLED0_SERIAL" "" "" cxl-pool)
---
$(pod-yaml b-consumer "" "env | grep ^CXL_ | sort
grep -H . /sys/devices/system/node/node*/meminfo | grep MemTotal" b-claim)"
dra-wait "$VM2" b-attached "resourceclaim -n $DRA_NS b-claim" \
    "condition('$DRA_POOL_DRIVER/Attached') == 'True'" 60
dra-claim-conditions "$VM2" b-claim
dra-assert "$COMMAND_OUTPUT" "[(r['driver'], r['pool'], r['device']) for r in results()] == [('$DRA_POOL_DRIVER', 'pool-b', 'b-pooled0')]"
dra-assert "$COMMAND_OUTPUT" "allocated_node() == '$VM0_NAME'"
B_CLAIM_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
dra-pod-wait "$VM2" b-consumer Running 120
[ "$(pod-node b-consumer)" == "$VM0_NAME" ] || error "b-consumer runs on '$(pod-node b-consumer)', expected $VM0_NAME"
pool-attachments --pool pool-b b-pooled0
pool-assert "$COMMAND_OUTPUT" "[(a['host'], a.get('owner'), a['state']) for a in j] == [('$VM0_NAME', 'k8s:resourceclaim/$B_CLAIM_UID', 'attached')]"
pool-assert "$COMMAND_OUTPUT" "j[0]['qemuDeviceID'].startswith('fcp_b-pooled0.hp') and j[0]['slot']['hostBridge'] == 'cxlhb0'"
for pool in pool-a pool-c; do
    pool-attachments-of "$pool"
    pool-assert "$COMMAND_OUTPUT" "j in (None, [])"
done
pool-cxl-wait "$VM0" b-pooled0-prepared \
    "by_serial($B_POOLED0_SERIAL) is not None and len(regions) == 1 and enabled(regions[0]) and online_size(regions[0]) == $(( 512 << 20 ))"
cxl-assert "regions[0]['numaNode'] >= $VM0_CPU_NODES"
B_NODE=$(cxl-value "regions[0]['numaNode']")
dra-wait-log "$VM2" b-consumer "CXL_${B_POOLED0_HEX}_NUMA_NODE=$B_NODE" 60
dra-kubectl "$VM2" "logs -n $DRA_NS b-consumer" || command-error "cannot get the log of b-consumer"
dra-env-assert b-consumer "$COMMAND_OUTPUT" "$B_POOLED0_HEX" \
    SERIAL="$B_POOLED0_SERIAL" SHARED=0 MODE=numa SIZE=$(( 512 << 20 )) NUMA_NODE="$B_NODE"
echo "b-pooled0 of pool-b is NUMA node $B_NODE of VM0"

echo "### C: a pod without a nodeSelector, a claim for a-pooled0: it goes to VM1 or VM2"
dra-apply "$VM2" <<< "$(dra-shared-claim-yaml a-claim "$A_POOLED0_SERIAL" "" "" cxl-pool)
---
$(pod-yaml a-consumer "" "env | grep ^CXL_ | sort" a-claim)"
dra-wait "$VM2" a-attached "resourceclaim -n $DRA_NS a-claim" \
    "condition('$DRA_POOL_DRIVER/Attached') == 'True'" 60
dra-claim-conditions "$VM2" a-claim
dra-assert "$COMMAND_OUTPUT" "[(r['pool'], r['device']) for r in results()] == [('pool-a', 'a-pooled0')]"
dra-assert "$COMMAND_OUTPUT" "allocated_node() in ['$VM1_NAME', '$VM2_NAME']"
A_NODE_NAME=$(dra-value "$COMMAND_OUTPUT" "allocated_node()")
dra-pod-wait "$VM2" a-consumer Running 120
[ "$(pod-node a-consumer)" == "$A_NODE_NAME" ] || error "a-consumer runs on '$(pod-node a-consumer)', expected $A_NODE_NAME"
pool-attachments --pool pool-a a-pooled0
pool-assert "$COMMAND_OUTPUT" "[(a['host'], a['state']) for a in j] == [('$A_NODE_NAME', 'attached')]"
echo "a-pooled0 of pool-a went to $A_NODE_NAME"

echo "### D: any 512Mi of pool memory: pool-b has the only free one, the pod goes to VM0"
# The 512Mi devices are a-pooled0 (held by a-consumer) and b-pooled0; the
# others have 256Mi or no memory capacity (shared devices). Free
# b-pooled0, and the only way to satisfy the claim is b-pooled0 on VM0.
dra-delete "$VM2" pod b-consumer --grace-period=2 --wait=true --timeout=60s
dra-delete "$VM2" resourceclaim b-claim --wait=true --timeout=60s
pool-device-free-wait --pool pool-b b-pooled0 "$VM0"
dra-apply "$VM2" <<< "$(dra-claim-yaml claim any-claim cxl-pool 512Mi)
---
$(pod-yaml any-consumer "" "env | grep ^CXL_ | sort" any-claim)"
dra-wait "$VM2" any-attached "resourceclaim -n $DRA_NS any-claim" \
    "condition('$DRA_POOL_DRIVER/Attached') == 'True'" 60
dra-claim-conditions "$VM2" any-claim
dra-assert "$COMMAND_OUTPUT" "[(r['pool'], r['device']) for r in results()] == [('pool-b', 'b-pooled0')] and allocated_node() == '$VM0_NAME'"
dra-pod-wait "$VM2" any-consumer Running 120
[ "$(pod-node any-consumer)" == "$VM0_NAME" ] || error "any-consumer runs on '$(pod-node any-consumer)', expected $VM0_NAME"
for pod in any-consumer a-consumer; do
    dra-delete "$VM2" pod "$pod" --grace-period=2 --wait=true --timeout=60s
done
for claim in any-claim a-claim; do
    dra-delete "$VM2" resourceclaim "$claim" --wait=true --timeout=60s
done
pool-device-free-wait --pool pool-b b-pooled0 "$VM0"
pool-device-free-wait --pool pool-a a-pooled0 "$(pool-vm-dir "$A_NODE_NAME")"

echo "### E: a pod pinned to VM0 with a claim for a-shared0 of pool-a: Pending"
# No node both matches the pod and reaches pool-a: the scheduler never
# allocates the claim, so the controller of pool-a never tries to attach
# it to VM0 (no AttachFailed with reason NoHost).
dra-apply "$VM2" <<< "$(dra-shared-claim-yaml e-claim "$A_SHARED0_SERIAL" 1 "mode: dax")
---
$(pod-yaml e-pod "$VM0_NAME" "true" e-claim)"
dra-wait "$VM2" e-unschedulable "events -n $DRA_NS" "events('FailedScheduling', 'e-pod')" 60
dra-kubectl "$VM2" "get events -n $DRA_NS --field-selector involvedObject.name=e-pod"
dra-assert "$(vmdir-command-q "$VM2" "kubectl get events -n $DRA_NS -o json")" \
    "all(e['message'].startswith('0/3 nodes are available') for e in events('FailedScheduling', 'e-pod'))"
# The scheduler does not allocate the claim late either: the controller of
# pool-a is never asked to attach a-shared0 to VM0.
e-pod-unschedulable() {
    dra-json "$VM2" pod -n "$DRA_NS" e-pod >/dev/null &&
        dra-py assert "$COMMAND_OUTPUT" "j['status']['phase'] == 'Pending' and [c['reason'] for c in j['status'].get('conditions', []) if c['type'] == 'PodScheduled'] == ['Unschedulable']" &&
        dra-json "$VM2" resourceclaim -n "$DRA_NS" e-claim >/dev/null &&
        dra-py assert "$COMMAND_OUTPUT" "j['status'].get('allocation') is None"
}
dra-stays 5 "e-pod stays Pending, unschedulable, its claim unallocated" e-pod-unschedulable
pool-attachments --pool pool-a a-shared0
pool-assert "$COMMAND_OUTPUT" "j in (None, [])"
dra-delete "$VM2" pod e-pod --grace-period=2 --wait=true --timeout=60s
dra-delete "$VM2" resourceclaim e-claim --wait=true --timeout=60s

echo "### F: two pools in one pod on VM0: b-shared0 of pool-b and c-shared0 of pool-c"
dra-apply "$VM2" <<< "$(dra-shared-claim-yaml f-claim-b "$B_SHARED0_SERIAL" 1 "mode: dax")
---
$(dra-shared-claim-yaml f-claim-c "$C_SHARED0_SERIAL" 1 "mode: dax")
---
$(pod-yaml f-writer "$VM0_NAME" "env | grep ^CXL_ | sort
python3 /tools/dax-rw.py write \"\$CXL_${B_SHARED0_HEX}_DAX\" 0 \"$TEXT_B\" || exit 1
python3 /tools/dax-rw.py write \"\$CXL_${C_SHARED0_HEX}_DAX\" 0 \"$TEXT_C\" || exit 1
echo written" f-claim-b f-claim-c)"
for claim in f-claim-b f-claim-c; do
    dra-wait "$VM2" "$claim-attached" "resourceclaim -n $DRA_NS $claim" \
        "condition('$DRA_POOL_DRIVER/Attached') == 'True'" 60
    dra-assert "$COMMAND_OUTPUT" "allocated_node() == '$VM0_NAME'"
done
dra-pod-wait "$VM2" f-writer Running 120
dra-wait-log "$VM2" f-writer written 60
dra-kubectl "$VM2" "logs -n $DRA_NS f-writer" || command-error "cannot get the log of f-writer"
F_LOG="$COMMAND_OUTPUT"
dra-env-assert f-writer "$F_LOG" "$B_SHARED0_HEX" \
    SERIAL="$B_SHARED0_SERIAL" SHARED=1 MODE=dax SIZE=$(( 256 << 20 )) "DAX=/dev/dax*" READ_ONLY=0
dra-env-assert f-writer "$F_LOG" "$C_SHARED0_HEX" \
    SERIAL="$C_SHARED0_SERIAL" SHARED=1 MODE=dax SIZE=$(( 256 << 20 )) "DAX=/dev/dax*" READ_ONLY=0
[ "$(dra-env-value "$F_LOG" "CXL_${B_SHARED0_HEX}_DAX")" != "$(dra-env-value "$F_LOG" "CXL_${C_SHARED0_HEX}_DAX")" ] ||
    error "f-writer has the same dax device for both claims"
# pool-b attached its device over QMP under host bridge 0 of VM0, pool-c
# its device over HMP under host bridge 1.
pool-attachments --pool pool-b b-shared0
pool-assert "$COMMAND_OUTPUT" "[(a['host'], a['state'], a['slot']['hostBridge']) for a in j] == [('$VM0_NAME', 'attached', 'cxlhb0')]"
pool-attachments --pool pool-c c-shared0
pool-assert "$COMMAND_OUTPUT" "[(a['host'], a['state'], a['slot']['hostBridge']) for a in j] == [('$VM0_NAME', 'attached', 'cxlhb1')]"
pool-cxl-wait "$VM0" two-pools \
    "by_serial($B_SHARED0_SERIAL) is not None and by_serial($C_SHARED0_SERIAL) is not None and len(regions) == 2 and all(online_size(r) == 0 for r in regions)"
for pool in pool-b pool-c; do
    dev="${pool#pool-}-shared0"
    pool-device-files --pool "$pool" "$dev"
    want="$TEXT_B"
    [ "$pool" == pool-b ] || want="$TEXT_C"
    found=$(pool-file-string "$POOL_DEVICE_DATA" 0)
    [ "$found" == "$want" ] || error "host: $POOL_DEVICE_DATA of $dev has '$found' at 0, expected '$want'"
    echo "host read at 0 of $POOL_DEVICE_DATA ($pool): '$found'"
done
# A node without a claim has nothing of the devices.
pool-cxl-dump "$VM2" f-no-claim
cxl-assert "memdevs == [] and regions == []"

echo "### G: shared across racks: a pod on VM2 reads what VM0 wrote into c-shared0"
# pool-c attaches c-shared0 to VM2 over HMP, on the monitor socket that
# the test uses too: the monitors of VM2 and VM0 keep answering meanwhile.
# g-claim has no configuration: a shared device gets mode dax.
MONITOR_ANSWERS=0
dra-apply "$VM2" <<< "$(dra-shared-claim-yaml g-claim "$C_SHARED0_SERIAL" 1)
---
$(pod-yaml g-reader "$VM2_NAME" "env | grep ^CXL_ | sort
python3 /tools/dax-rw.py read \"\$CXL_${C_SHARED0_HEX}_DAX\" 0 || exit 1" g-claim)"
# shellcheck disable=SC2016 # evaluated by retry-until
retry-until --timeout 60 --message "g-claim attached while the monitors answer" \
    'monitors-answer "$VM2" "$VM0" && dra-json "$VM2" resourceclaim -n "$DRA_NS" g-claim >/dev/null && dra-py assert "$COMMAND_OUTPUT" "condition(\"$DRA_POOL_DRIVER/Attached\") == \"True\""' ||
    error "g-claim was not attached in 60 s"
# At least two rounds of both monitors: pool-c was attaching over HMP
# meanwhile (the attach takes seconds), not done before the first round.
[ "$MONITOR_ANSWERS" -ge 4 ] ||
    error "the monitors answered only $MONITOR_ANSWERS times before g-claim was attached, expected 4 or more (two rounds of VM2 and VM0)"
echo "the monitors of VM2 and VM0 answered $MONITOR_ANSWERS times while pool-c attached c-shared0 to VM2"
dra-assert "$COMMAND_OUTPUT" "allocated_node() == '$VM2_NAME' and [(r['pool'], r['device']) for r in results()] == [('pool-c', 'c-shared0')]"
G_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
dra-pod-wait "$VM2" g-reader Running 120
dra-wait-log "$VM2" g-reader "$TEXT_C" 60
dra-kubectl "$VM2" "logs -n $DRA_NS g-reader" || command-error "cannot get the log of g-reader"
G_LOG="$COMMAND_OUTPUT"
# The environment of the device, read-write, and the marker of result 0
# of the claim, nothing else.
dra-env-assert g-reader "$G_LOG" "$C_SHARED0_HEX" \
    SERIAL="$C_SHARED0_SERIAL" SHARED=1 MODE=dax SIZE=$(( 256 << 20 )) "DAX=/dev/dax*" READ_ONLY=0
grep -qx "CXL_CLAIM_${G_UID//-/_}_0=1" <<< "$G_LOG" ||
    error "g-reader has no marker CXL_CLAIM_${G_UID//-/_}_0=1 of result 0 of its claim"
[ "$(grep -c '^CXL_' <<< "$G_LOG")" == 7 ] ||
    error "g-reader has other CXL_ variables than the six of $C_SHARED0_SERIAL and the marker of its result: $(grep '^CXL_' <<< "$G_LOG")"
pool-attachments --pool pool-c c-shared0
pool-assert "$COMMAND_OUTPUT" "sorted((a['host'], a['state']) for a in j) == sorted([('$VM0_NAME', 'attached'), ('$VM2_NAME', 'attached')])"
echo "VM2 read '$TEXT_C' from c-shared0 of pool-c, attached to VM0 and VM2"

echo "### H: VM0 lets go while VM2 still reads, then every device is back in its pool"
dra-delete "$VM2" pod f-writer --grace-period=2 --wait=true --timeout=60s
for claim in f-claim-b f-claim-c; do
    dra-delete "$VM2" resourceclaim "$claim" --wait=true --timeout=60s
done
pool-cxl-wait "$VM0" f-released "memdevs == [] and regions == []" 60
# shellcheck disable=SC2016 # evaluated by retry-until
retry-until --timeout 60 --message "c-shared0 attached only to VM2" \
    'pool-host-client --pool pool-c -o json attachments --device c-shared0 >/dev/null && pool-py assert "$COMMAND_OUTPUT" "[a[\"host\"] for a in j or []] == [\"$VM2_NAME\"]"' ||
    error "c-shared0 is not attached to VM2 only: $COMMAND_OUTPUT"
dra-exec "$VM2" g-reader/main python3 /tools/dax-rw.py read "$(dra-env-value "$G_LOG" "CXL_${C_SHARED0_HEX}_DAX")" 0 ||
    command-error "cannot read c-shared0 in g-reader"
[ "$COMMAND_OUTPUT" == "$TEXT_C" ] ||
    error "after VM0 let go of c-shared0, VM2 read '$COMMAND_OUTPUT', expected '$TEXT_C'"
dra-delete "$VM2" pod g-reader --grace-period=2 --wait=true --timeout=60s
dra-delete "$VM2" resourceclaim g-claim --wait=true --timeout=60s
for vm in "${VMS[@]}"; do
    pool-cxl-wait "$vm" released "memdevs == [] and regions == []" 60
done
# pool-c sees its detaches over HMP by polling the qemu tree.
for pool in "${POOLS[@]}"; do
    pool-free-wait "$pool"
done
for vm in "${VMS[@]}"; do
    pool-vm-monitor "$vm" "info qtree -b" | grep -q 'dev: cxl-type3' &&
        error "qemu of $(vmdir-label "$vm") still has a CXL memory device"
done
grep -E 'attachment c-shared0@.*: (attached|detached)' "${POOL_SERVER_LOGS[pool-c]}" ||
    error "pool-c logged no attach and detach of c-shared0, see ${POOL_SERVER_LOGS[pool-c]}"
pool-snapshot end
# The three controllers kept their own slices all along.
dra-json "$VM2" resourceslices
dra-assert "$COMMAND_OUTPUT" "sorted((s['spec']['pool']['name'], s['metadata']['uid']) for s in slices('$DRA_POOL_DRIVER')) == $POOL_SLICE_IDS"
dra-assert-clean "$VM2" "$VM1" "$VM0"

dra-trace-stop
dra-trace-summary

echo "### cleanup"
dra-cleanup
pool-cleanup
trap - EXIT
for port in "$POOL_PORT" 9910 9911; do
    pool-port-busy "$port" && error "port $port is still busy after stopping the servers"
done
echo "trace: $TEST_OUTPUT_DIR/trace.txt, summary: $TEST_OUTPUT_DIR/trace-summary.txt"
