# CXL memory pooling with Dynamic Resource Allocation (DRA)
#
# A CXL memory pool is a box of memory devices that is cabled to many
# servers. The pool attaches a device to one server when that server needs
# more memory, and detaches it when the server is done: memory moves to
# where it is needed, without opening any server.
#
# Here the pool is fake-cxl-pool-server on the host. It hotplugs CXL
# memory devices into the qemu of this VM (n4-cxl-shared-2, VM2), the
# control plane of the Kubernetes cluster of the test, where the pod runs.
# (VM1 and VM0, the VMs of n4-cxl-shared-1 and -0, are the other nodes of
# the cluster. The pool is cabled to VM1 and VM2 only.)
# Kubernetes asks for the memory with DRA:
# - fake-cxl-pool-controller (in the VM) publishes the devices of the pool
#   in a ResourceSlice of driver cxl-pool.nri.io, attaches the device that
#   the scheduler allocated for a ResourceClaim to the node the scheduler
#   picked, and writes the condition cxl-pool.nri.io/Attached to the claim.
# - kube-scheduler allocates a device for the claim and waits for that
#   binding condition before it binds the pod to the node.
# - nri-dra-driver-cxl (the CXL DRA driver, its pod on the node of the VM)
#   prepares the claim: it finds the hotplugged memory device by its
#   serial number, creates a region on it and onlines its memory. The
#   memory becomes a new NUMA node, and the container gets its id in the
#   environment variable CXL_<KEY>_NUMA_NODE, next to CXL_<KEY>_MODE=numa,
#   _SIZE, _SERIAL and _SHARED, and the node in its cpuset.mems. No other
#   container of the node has the node. When the pod is gone, the node
#   plugin releases the device, the controller detaches it, and the memory
#   is back in the pool.
# The claim configures all of them with one opaque MemoryConfig: its pool
# section for the pool controller (pool.opaque.fake-cxl-pool: slot,
# numaNode, where the pool attaches the device), its device section for
# the kubelet plugin, its container section for the NRI plugin of the driver.
# Steering is opt-in, as for local memory: without a steering policy the
# container is only fenced, the workload places its memory itself; with
# container.steering.policy auto the driver steers the memory of the
# container to the pool node.
#
# What you will see: the pool in a ResourceSlice, a claim of 512Mi that
# waits for the attachment, a new 512 MB NUMA node in the VM and in the
# pod, not steered, the memory going back to the pool; a claim that opts in
# to steering and asks the pool for a slot near NUMA node 1, its device
# under the other host bridge, a NUMA node of its own, the memory of the
# workload there; pooled0 as a dax device, written by the pod and read on
# the host; a claim with an invalid pool section that the controller
# refuses; and at the end the trace summary of what every component did, in
# order.
#
# Run test00-up of n4-cxl-shared-1, -0 and -2 first (see there for the
# patched qemu): they create the VMs and make them one cluster. The DRA
# driver comes from its image, build it first:
#   make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

echo "### preconditions: one cluster, VM2 control plane, VM1 and VM0 workers (test00-up)"
pool-cluster-require

VM1="$POOL_VM1_DIR"    # a worker of the cluster
VM2="$OUTPUT_DIR"      # the VM of this test, the control plane
VM1_NAME="$POOL_VM1_NAME"
VM2_NAME="$POOL_VM2_NAME"
POOLED0_SERIAL=0xc1ee0001   # 0xc1ee....: unshared pool devices
POOLED0_HEX=C1EE0001        # the serial in environment variable names
POOLED0_SIZE=$(( 512 << 20 ))
POOLED1_SERIAL=0xc1ee0002
POOLED1_HEX=C1EE0002
POOLED1_SIZE=$(( 256 << 20 ))
TOUCH_MIB=64                # what the steered workload touches, within its claim
TAG="dra-pooling $(date +%Y-%m-%dT%H:%M:%S.%N)"
TEXT_DAX="$TAG: pooled0 through a dax claim"

echo "### preconditions: patched qemu, hdm_for_passthrough=on, kernel with CONFIG_FS_DAX"
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"

echo "### preconditions: cxl, daxctl and cxl-dump in the VM, no CXL memory devices in VM2 and VM1"
pool-vm-tools-install "$VM2"
pool-vm-reset "$VM2"
pool-vm-reset "$VM1"
pool-cxl-dump "$VM2" no-devices
cxl-assert 'memdevs == [] and regions == [] and endpoints == []'
CPU_NODES=$(cxl-value 'len(nodes)')

echo "### preconditions: fake-cxl-pool-server on the host, with two unshared devices"
pool-server-start "
  - name: pooled0
    size: 512Mi
    shared: false
    serial: $POOLED0_SERIAL
  - name: pooled1
    size: 256Mi
    shared: false
    serial: $POOLED1_SERIAL"
pool-client-install "$VM2"

echo "### preconditions: the pool controller and the driver run in the cluster"
dra-build
dra-install
dra-pull-images "$VM2" docker.io/library/busybox:latest "$DRA_TOOLS_IMAGE"
dra-tools-install "$VM2"
dra-trace-start "$VM2"

echo "### A: the pool is visible in Kubernetes"
# The controller publishes the devices of the pool in one ResourceSlice
# for the nodes that the pool can reach: a nodeSelector of their
# kubernetes.io/hostname, VM1 and VM2, not VM0 and never all nodes. The
# driver publishes the memory of the node (DRAM, and CXL memory that
# is already in the node) in a slice of its own.
dra-kubectl "$VM2" "get resourceslices"
dra-json "$VM2" resourceslices
SLICES="$COMMAND_OUTPUT"
dra-assert "$SLICES" "len(slices('$DRA_POOL_DRIVER')) == 1 and not slices('$DRA_POOL_DRIVER')[0]['spec'].get('allNodes')"
dra-assert "$SLICES" "slices('$DRA_POOL_DRIVER')[0]['spec'].get('nodeSelector') == {'nodeSelectorTerms': [{'matchExpressions': [{'key': 'kubernetes.io/hostname', 'operator': 'In', 'values': sorted(['$VM1_NAME', '$VM2_NAME'])}]}]}"
dra-assert "$SLICES" "sorted(d['name'] for d in devices('$DRA_POOL_DRIVER')) == ['pooled0', 'pooled1']"
P0="device('pooled0', '$DRA_POOL_DRIVER')"
P1="device('pooled1', '$DRA_POOL_DRIVER')"
# Every pool device is a memdev of source pool, with its serial, size,
# whether hosts may share it (the attributes of a local memdev too),
# whether its memory is persistent, and whether it is coherent between
# hosts (fake-cxl-pool devices are: qemu maps the same host memory).
dra-assert "$SLICES" "attr($P0, 'type') == 'memdev' and attr($P0, 'source') == 'pool' and attr($P0, 'shared') == False and attr($P0, 'persistent') == False and attr($P0, 'size') == $POOLED0_SIZE and attr($P0, 'serial') == '$POOLED0_SERIAL'"
dra-assert "$SLICES" "attr($P0, 'coherent') == True and attr($P1, 'coherent') == True"
dra-assert "$SLICES" "sorted(k.split('/')[-1] for k in ${P0}['attributes']) == ['coherent', 'persistent', 'serial', 'shared', 'size', 'source', 'type']"
dra-assert "$SLICES" "capacity($P0, 'memory') == '512Mi'"
dra-assert "$SLICES" "attr($P1, 'type') == 'memdev' and attr($P1, 'shared') == False and attr($P1, 'size') == $POOLED1_SIZE and capacity($P1, 'memory') == '256Mi'"
# The device is not on any node yet: the scheduler picks the node
# (bindsToNode), and waits until the pool has attached the device there.
dra-assert "$SLICES" "${P0}['bindsToNode'] == True and ${P0}['bindingConditions'] == ['$DRA_POOL_DRIVER/Attached']"
dra-assert "$SLICES" "${P0}['bindingFailureConditions'] == ['$DRA_POOL_DRIVER/AttachFailed']"
dra-assert "$SLICES" "len(slices('$DRA_LOCAL_DRIVER', '$VM2_NAME')) == 1"
DRAM="device('dram', '$DRA_LOCAL_DRIVER', '$VM2_NAME')"
dra-assert "$SLICES" "attr($DRAM, 'type') == 'dram' and attr($DRAM, 'source') == 'local' and attr($DRAM, 'size') > 0 and attr($DRAM, 'memoryNUMANodes')"
dra-assert "$SLICES" "not any(attr(d, 'type') == 'region' for d in devices('$DRA_LOCAL_DRIVER', '$VM2_NAME'))"
# The pool itself says the same.
pool-client "$VM2" devices || command-error "cannot list the devices of the pool"
pool-client "$VM2" -o json devices || command-error "cannot list the devices of the pool"
pool-assert "$COMMAND_OUTPUT" "sorted((d['name'], d['state']) for d in j if d['name'].startswith('pooled')) == [('pooled0', 'free'), ('pooled1', 'free')]"

echo "### B: a pod asks for 512Mi of pooled CXL memory"
# Only pooled0 has 512Mi. The pod, on VM2, prints what it got and sleeps.
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
  name: pooled-pod
spec:
  nodeSelector:
    kubernetes.io/hostname: $VM2_NAME
  terminationGracePeriodSeconds: 2
  restartPolicy: Never
  resourceClaims:
  - name: pooled
    resourceClaimName: pooled-memory
  containers:
  - name: main
    image: docker.io/library/busybox:latest
    imagePullPolicy: IfNotPresent
    command:
    - sh
    - -c
    - |
      env | grep ^CXL_ | sort
      echo
      grep -H . /sys/devices/system/node/node*/meminfo | grep MemTotal
      grep Mems_allowed_list /proc/self/status
      trap "exit 0" TERM; sleep infinity & wait
    resources:
      claims:
      - name: pooled
EOF

echo "### C: the scheduler waits for the attachment"
# The scheduler allocates pooled0 for the claim, pins the claim to this
# node, and reports that the binding conditions are pending.
dra-wait "$VM2" binding-conditions-pending "events -n $DRA_NS" \
    "events('BindingConditionsPending', 'pooled-pod')" 60
dra-kubectl "$VM2" "get events -n $DRA_NS --field-selector involvedObject.name=pooled-pod"
# The controller sees the allocation, the pool attaches pooled0 to the VM,
# and the controller tells the scheduler with the condition Attached.
dra-wait "$VM2" attached "resourceclaim -n $DRA_NS pooled-memory" \
    "condition('$DRA_POOL_DRIVER/Attached') == 'True'" 60
CLAIM_JSON="$COMMAND_OUTPUT"
dra-claim-conditions "$VM2" pooled-memory
dra-assert "$CLAIM_JSON" "[(r['driver'], r['device']) for r in results()] == [('$DRA_POOL_DRIVER', 'pooled0')]"
dra-assert "$CLAIM_JSON" "allocated_node() == '$VM2_NAME'"
# What the driver needs to find the device is in the claim status.
dra-assert "$CLAIM_JSON" "device_data('pooled0')['serial'] == '$POOLED0_SERIAL' and device_data('pooled0')['shared'] == False and device_data('pooled0')['size'] == $POOLED0_SIZE"
CLAIM_UID=$(dra-value "$CLAIM_JSON" "j['metadata']['uid']")
pool-client "$VM2" attachments || command-error "cannot list the attachments of the pool"
pool-client "$VM2" -o json attachments || command-error "cannot list the attachments of the pool"
pool-assert "$COMMAND_OUTPUT" "[(a['device'], a['host'], a.get('owner')) for a in j] == [('pooled0', '$VM2_NAME', 'k8s:resourceclaim/$CLAIM_UID')]"

echo "### D: the node makes the memory usable"
# The scheduler binds the pod, kubelet asks the driver to prepare the
# claim: a region on the new memory device, its memory online.
dra-pod-wait "$VM2" pooled-pod Running 120
pool-cxl-wait "$VM2" pooled0-prepared \
    "by_serial($POOLED0_SERIAL) is not None and len(regions) == 1 and enabled(regions[0]) and online_size(regions[0]) == $POOLED0_SIZE"
cxl-assert "sorted(regions[0]['memdevs']) == [by_serial($POOLED0_SERIAL)['name']]"
cxl-assert "regions[0]['numaNode'] >= $CPU_NODES"
CXL_NODE=$(cxl-value "regions[0]['numaNode']")
echo "pooled0 is NUMA node $CXL_NODE of the VM"
vm-command "numactl -H" || command-error "numactl failed"
grep -q "^node $CXL_NODE size: 512 MB" <<< "$COMMAND_OUTPUT" ||
    error "numactl -H does not show node $CXL_NODE with 512 MB"
# The container knows which node has its memory: the environment of the
# device and the marker of its claim, nothing else.
dra-kubectl "$VM2" "logs -n $DRA_NS pooled-pod" || command-error "cannot get the log of pooled-pod"
POD_LOG="$COMMAND_OUTPUT"
dra-env-assert pooled-pod "$POD_LOG" "$POOLED0_HEX" \
    SERIAL="$POOLED0_SERIAL" SHARED=0 MODE=numa SIZE="$POOLED0_SIZE" NUMA_NODE="$CXL_NODE"
grep -qx "CXL_CLAIM_${CLAIM_UID//-/_}_0=1" <<< "$POD_LOG" ||
    error "pooled-pod has no marker CXL_CLAIM_${CLAIM_UID//-/_}_0=1 of result 0 of its claim"
[ "$(grep -c '^CXL_' <<< "$POD_LOG")" == 6 ] ||
    error "pooled-pod has other CXL_ variables than the five of $POOLED0_SERIAL and the marker of its result: $(grep '^CXL_' <<< "$POD_LOG")"
grep -q "^/sys/devices/system/node/node$CXL_NODE/meminfo:Node $CXL_NODE MemTotal:" <<< "$POD_LOG" ||
    error "pooled-pod does not see the memory of node $CXL_NODE"
echo "pooled-pod: $(grep "node$CXL_NODE/meminfo" <<< "$POD_LOG")"
# The node is for the pod only: its claim puts the node into its
# cpuset.mems from its start, while every other container of the node,
# those of the control plane too, is kept off the node.
MEMS_ALLOWED=$(sed -n 's/^Mems_allowed_list:[[:space:]]*//p' <<< "$POD_LOG")
dra-numa-nodes-has "$MEMS_ALLOWED" "$CXL_NODE" ||
    error "pooled-pod: node $CXL_NODE is not in its Mems_allowed_list '$MEMS_ALLOWED'"
echo "pooled-pod: Mems_allowed_list $MEMS_ALLOWED"
dra-mems-fenced "$VM2" "$CXL_NODE" "$DRA_NS/pooled-pod/main"
# Steering is opt-in: the claim has no MemoryConfig, the container is fenced,
# not steered: no memory policy, no memory.high.
dra-memory-wait "$VM2" pooled-pod main fenced "none (default)"

echo "### D: the pool device is not advertised again as node-local CXL memory"
# The new region is CXL memory of the node, which the driver offers
# as cxl.nri.io devices, but not when it is a prepared pool device. Wait
# until the driver has rescanned and published after it prepared
# pooled0 (udev events of the region trigger the rescan).
rescanned-after-prepare() {
    dra-driver-log "$VM2" |
        awk -v prepared="$DRA_POOL_DRIVER: claim .*: prepared pooled0" \
            '$0 ~ prepared {p = 1}
             p && /publisher: published / {found = 1}
             END {exit !found}'
}
retry-until --timeout 30 --message "driver rescan after preparing pooled0" rescanned-after-prepare ||
    error "nri-dra-driver-cxl did not publish its resources after preparing pooled0"
dra-driver-log "$VM2" | grep -E 'prepared pooled0|ignoring region|publisher: '
dra-json "$VM2" resourceslices
dra-assert "$COMMAND_OUTPUT" "not any(attr(d, 'type') == 'region' for d in devices('$DRA_LOCAL_DRIVER', '$VM2_NAME'))"

echo "### E: delete the pod and the claim, the memory returns to the pool"
# Kubelet unprepares the claim while the pod terminates: the driver
# offlines the memory and releases the device. Then the claim is
# deallocated, and the controller detaches the device.
dra-delete "$VM2" pod pooled-pod --grace-period=2 --wait=true --timeout=60s
dra-delete "$VM2" resourceclaim pooled-memory --wait=true --timeout=60s
dra-wait "$VM2" claim-gone "resourceclaims -n $DRA_NS" "'pooled-memory' not in names()" 60
pool-cxl-wait "$VM2" pooled0-detached "memdevs == []" 60
cxl-assert 'regions == [] and endpoints == []'
retry-until --timeout 30 --message "no attachments in the pool" \
    'pool-client "$VM2" -o json attachments >/dev/null && pool-py assert "$COMMAND_OUTPUT" "j in (None, [])"' ||
    error "pooled0 is still attached: $COMMAND_OUTPUT"
pool-client "$VM2" -o json devices pooled0 || command-error "cannot get pooled0"
pool-assert "$COMMAND_OUTPUT" "j['state'] == 'free' and j['attachments'] in (None, [])"
pool-vm-monitor "$VM2" "info qtree -b" | grep -q 'dev: cxl-type3' &&
    error "qemu of the VM still has a CXL memory device"
dra-driver-log "$VM2" | grep -iE 'unprepar|releas' ||
    error "the driver did not log the release of pooled0"
deleted=$(grep -c 'event DEVICE_DELETED' "$POOL_SERVER_LOG")
[ "$deleted" -ge 1 ] || error "qemu did not report DEVICE_DELETED, see $POOL_SERVER_LOG"
grep -E 'attachment pooled0@.*: (attached|detached)|event DEVICE_DELETED' "$POOL_SERVER_LOG"

echo "### F: a claim opts in to steering and asks the pool for a slot near NUMA node 1"
# pooled1 for a claim with a container section (steering policy auto) and
# a pool section: the object of the fake pool controller,
# pool.opaque.fake-cxl-pool, asks for a slot under a host bridge on NUMA
# node 1 of the host (the server prefers such slots); a key of another
# controller implementation is not this controller's business. Without
# numaNode the server takes a slot under host bridge 0, the slot of
# pooled0 in D.
dra-apply "$VM2" <<< "$(dra-shared-claim-yaml pooled-steered "$POOLED1_SERIAL" "" "container: {steering: {policy: auto}}, pool: {opaque: {$DRA_POOL_CONFIG_KEY: {numaNode: 1}, other-pool: {qos: gold}}}" cxl-pool)
---
$(dra-claim-pod-yaml pooled-steered claim pooled-steered "$VM2_NAME")"
dra-wait "$VM2" steered-attached "resourceclaim -n $DRA_NS pooled-steered" \
    "condition('$DRA_POOL_DRIVER/Attached') == 'True'" 60
STEERED_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
pool-client "$VM2" -o json attachments || command-error "cannot list the attachments of the pool"
pool-assert "$COMMAND_OUTPUT" "[(a['device'], a['host'], a.get('owner'), a['slot']['hostBridge'], a['slot']['numaNode']) for a in j] == [('pooled1', '$VM2_NAME', 'k8s:resourceclaim/$STEERED_UID', 'cxlhb1', 1)]"
echo "pooled1 is in slot $(pool-value "$COMMAND_OUTPUT" "j[0]['slot']['bus']") under host bridge cxlhb1 on NUMA node 1"
dra-pod-wait "$VM2" pooled-steered Running 120
pool-cxl-wait "$VM2" pooled1-prepared \
    "by_serial($POOLED1_SERIAL) is not None and len(regions) == 1 and enabled(regions[0]) and online_size(regions[0]) == $POOLED1_SIZE"
POOL_NODE=$(cxl-value "regions[0]['numaNode']")
if (( POOL_NODE < CPU_NODES )) || [ "$POOL_NODE" == "$CXL_NODE" ]; then
    error "pooled1 is NUMA node $POOL_NODE, expected a CXL node other than $CXL_NODE of host bridge 0"
fi
echo "pooled1 is NUMA node $POOL_NODE of the VM, the CXL window of host bridge 1"
# shellcheck disable=SC2016 # expanded by sh in the container
dra-exec "$VM2" pooled-steered sh -c 'env | grep ^CXL_ | sort' ||
    command-error "cannot read the environment of pooled-steered"
dra-env-assert pooled-steered "$COMMAND_OUTPUT" "$POOLED1_HEX" \
    SERIAL="$POOLED1_SERIAL" SHARED=0 MODE=numa SIZE="$POOLED1_SIZE" NUMA_NODE="$POOL_NODE"
# Pool numa memory is steered like local memory: CXL only, policy
# first-cxl, the memory policy of the main process prefers the pool node.
# A workload that runs as a child of the main process and has no NUMA
# preference of its own gets its memory there.
dra-memory-wait "$VM2" pooled-steered main steered "auto from claim $DRA_NS/pooled-steered request mem"
dra-numa-nodes-has "${DRA_MEMPOLICY##*:}" "$POOL_NODE" ||
    error "pooled-steered: the memory policy of the main process is '$DRA_MEMPOLICY', expected node $POOL_NODE"
dra-run "$VM2" pooled-steered "python3 /tools/numa-touch.py $TOUCH_MIB -" ||
    error "numa-touch.py in pooled-steered failed"
pages=$(dra-touch-log-pages "$COMMAND_OUTPUT" "$POOL_NODE")
[ "$pages" -ge $(( (TOUCH_MIB << 8) * 9 / 10 )) ] ||
    error "pooled-steered: only $pages of $(( TOUCH_MIB << 8 )) pages of the workload are on the pool node $POOL_NODE"
dra-mems-fenced "$VM2" "$POOL_NODE" "$DRA_NS/pooled-steered/main"
dra-delete "$VM2" pod pooled-steered --grace-period=2 --wait=true --timeout=60s
dra-delete "$VM2" resourceclaim pooled-steered --wait=true --timeout=60s
pool-device-free-wait pooled1 "$VM2"

echo "### G: the unshared device pooled0 as a dax device"
# Mode dax: the driver creates a devdax region on the device, never
# onlined, and gives the container its device node, read-write. 512Mi:
# only pooled0 is that large.
dra-apply "$VM2" <<< "$(dra-claim-yaml claim pooled-dax cxl-pool 512Mi dax)
---
$(dra-script-pod-yaml dax-claimer "$VM2" pooled-dax "env | grep ^CXL_${POOLED0_HEX}_ | sort
python3 /tools/dax-rw.py write \"\$CXL_${POOLED0_HEX}_DAX\" 0 \"$TEXT_DAX\" || exit 1
echo \"read: \$(python3 /tools/dax-rw.py read \"\$CXL_${POOLED0_HEX}_DAX\" 0)\"")"
dra-pod-wait "$VM2" dax-claimer Running 120
dra-wait-log "$VM2" dax-claimer "read: $TEXT_DAX" 60
dra-kubectl "$VM2" "logs -n $DRA_NS dax-claimer"
dra-env-assert dax-claimer "$COMMAND_OUTPUT" "$POOLED0_HEX" \
    SERIAL="$POOLED0_SERIAL" SHARED=0 MODE=dax SIZE="$POOLED0_SIZE" "DAX=/dev/dax*" READ_ONLY=0
pool-cxl-dump "$VM2" pooled0-dax
cxl-assert "by_serial($POOLED0_SERIAL) is not None and len(regions) == 1 and online_size(regions[0]) == 0"
pool-device-files pooled0
found=$(pool-file-string "$POOL_DEVICE_DATA" 0)
[ "$found" == "$TEXT_DAX" ] || error "host: $POOL_DEVICE_DATA has '$found' at 0, expected '$TEXT_DAX'"
dra-delete-pod "$VM2" dax-claimer
dra-delete "$VM2" resourceclaim pooled-dax --wait=true --timeout=60s
pool-device-free-wait pooled0 "$VM2"

echo "### H: an invalid pool section: the controller refuses the claim and attaches nothing"
# A misspelled field in the object of the controller: the controller
# refuses the allocation with the condition AttachFailed, reason
# InvalidConfig, without attaching the device. The scheduler deallocates
# the claim and tries again until the binding times out: the condition
# lasts only until then, the watch sees it, the Event of the controller
# stays.
dra-apply "$VM2" <<< "$(dra-shared-claim-yaml bad-pool "$POOLED0_SERIAL" "" "pool: {opaque: {$DRA_POOL_CONFIG_KEY: {numaNod: 1}}}" cxl-pool)
---
$(dra-claim-pod-yaml bad-pool claim bad-pool "$VM2_NAME")"
dra-claim-condition-watch "$VM2" bad-pool "$DRA_POOL_DRIVER/AttachFailed" InvalidConfig 90
grep -qF 'unknown field "numaNod"' <<< "$COMMAND_OUTPUT" ||
    error "the AttachFailed condition of bad-pool does not name the unknown field numaNod: $COMMAND_OUTPUT"
dra-wait "$VM2" bad-pool-event "events -n $DRA_NS --field-selector involvedObject.name=bad-pool" \
    "any(e['involvedObject'].get('kind') == 'ResourceClaim' and e.get('reason') == 'AttachFailed' and 'numaNod' in (e.get('message') or '') for e in items)" 60
dra-kubectl "$VM2" "get events -n $DRA_NS --field-selector involvedObject.name=bad-pool"
pool-attachments pooled0
pool-assert "$COMMAND_OUTPUT" "j in (None, [])"
dra-json "$VM2" pod -n "$DRA_NS" bad-pool
dra-assert "$COMMAND_OUTPUT" "j['status']['phase'] == 'Pending' and not j['spec'].get('nodeName')"
dra-delete "$VM2" pod bad-pool --grace-period=2 --wait=true --timeout=60s
dra-delete "$VM2" resourceclaim bad-pool --wait=true --timeout=60s
pool-device-free-wait pooled0 "$VM2"

dra-assert-clean "$VM2" "$VM1"

echo "### I: what every component did, in order"
dra-trace-stop
dra-trace-summary

echo "### cleanup"
dra-cleanup
pool-cleanup
trap - EXIT
pool-port-busy && error "port $POOL_PORT is still busy after stopping the server"
echo "trace: $TEST_OUTPUT_DIR/trace.txt, summary: $TEST_OUTPUT_DIR/trace-summary.txt"
