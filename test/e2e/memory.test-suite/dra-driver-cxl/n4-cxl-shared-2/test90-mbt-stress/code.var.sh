# Model-based reliability test of the CXL DRA driver and the pool
# controllers with gofmbt (contract.txt).
#
# The cluster of test19-many-pools (three nodes, three pool servers, three
# pool controllers) with the local memdev of every VM attached, where a
# model (model/, the Go program mbt-cxl, built here with the
# github.com/askervin/gofmbt of model/go.mod) chooses the steps of a long
# run: pods with CXL memory of every kind (unshared and shared pool
# devices in numa and dax mode, read-only shares, local slices, steering,
# leases, no claim) created, deleted and crashed, data written and read,
# persistent pool devices (amendments A22-A27: a block writer, erofs
# readers direct and cached, read-only block readers, persistent leases
# of block and fs) written, read, held and wiped, and the faults the
# driver must survive (the driver killed in place or restarted, also at
# a chosen point of a persistent Prepare, a pool controller killed,
# started and restarted, a pool server restarted on its state, kubelet
# and containerd restarted, with MBT_REBOOT a worker node rebooted). After
# the steps that change the cluster a "settle" observes the pods, claims,
# pool attachments, the CXL devices of the VMs, the driver's state files
# and CDI specs and the cpusets of the containers, and the model compares
# that with its prediction (contract.txt: the actions, the protocol,
# the observations). A failure stops the run; the steps are in
# mbt-steps.txt for a replay.
#
# A long test: run_tests.sh skips it unless skip_long_tests=no.
#
# Modes (MBT_MODE):
#   online   mbt-cxl runs as a coprocess and sends one step at a time,
#            planning the next one meanwhile (default)
#   replay   the same, the steps of MBT_REPLAY (an mbt-steps.txt)
#   offline  the steps and predictions of MBT_SCRIPT, a code.gen.sh of
#            "mbt-cxl generate", checked by "mbt-cxl check"
# Parameters (environment of run_tests.sh; contract 8): MBT_SEED (default
# time-based), MBT_DURATION (20m), MBT_STEPS (0: no limit), MBT_DEPTH (3),
# MBT_PROFILE (fullp), MBT_UNSETTLED (3), MBT_SETTLE_TIMEOUT (180 s),
# MBT_POLL (3 s), MBT_AVOID (verbs or verb:arg to leave out),
# MBT_FENCING (the fencing mode of the drivers, amendments A36, A37:
# allowed-mems, the driver's default; none, the driver configuration has
# "defaults: {container: {fencing: none}}"; switch, the default, starts in
# allowed-mems and the model may switch the mode of every driver with the
# step fencing-switch; the model gets --fencing=MODE; a
# replay and an off-line run take it from the header of MBT_REPLAY or
# MBT_SCRIPT: set, it must be the header's, else the test fails),
# MBT_REBOOT (the verb node-reboot of vm0 and vm1, rules A41-A44:
# guest, a guest reboot, systemctl reboot, qemu
# keeps running with its devices; qemu, a restart of the qemu process, a
# power cycle after which the pool controllers attach their devices again
# and the executor the local memdev; empty, the default: none; a reboot
# step takes far longer than any other step; the model gets --reboot=MODE; a replay and an
# off-line run take it from the header of their file as MBT_FENCING (set,
# also empty, it must be the header's); guest needs, in the qemu processes
# of vm0 and vm1, a qemu whose cxl-type3 device resets its CXL DVSEC at a guest reset (ct3d_reset_device_dvsec of hw/mem/cxl_type3.c), checked at the start),
# MBT_STALL_WINDOW (--stall-window, A28: the model's default, 6m, unless
# set), MBT_REPLAY,
# MBT_SCRIPT, MBT_ARGS (more flags of mbt-cxl, for
# instance "--plan-ahead=false"). MBT_POLL is the executor's: the
# seconds between settle observations. Executor: MBT_TRACE (1: the
# dra-trace of every component, 0: none), MBT_OBS_KEEP (30 observations
# kept in mbt-obs/), MBT_BIN (another generator than the model), MBT_KILLAT_TIMEOUT (60 s: a
# driver-kill-at watcher gives up), MBT_REBOOT_WAIT (180 s: node-reboot
# waits that long for ssh with a new boot id), MBT_LEASE_PUT_WAIT (20s: lease-put of
# persistent leases waits that long for Released).
#
# Persistent pool devices (A22): pool-c has c-pmem0 (256Mi, shared,
# 0xc1ae2002) and c-pmem1 (256Mi, exclusive, 0xc1ee2002) in every profile,
# their backing files new (removed before the server starts and at the
# end: host RAM); the settle observation reads their content from those
# files ("pdevs"). The pods write and read them with pmem-rw.py and the
# erofs template hello.erofs.hex (ConfigMap tools at /tools): no
# mkfs.erofs, no image build.
#
# Output in the test output directory: mbt-steps.txt (the steps, for
# MBT_REPLAY), mbt-cxl.log (the generator), mbt-fail.txt (the generator's
# report of a failure), mbt-exec.log (every command of every step and its
# output), mbt-times.tsv and mbt-obs-times.tsv (how long each step and
# observation took), mbt-obs/ (the last observations, fail-N.json),
# trace.txt and trace-summary.txt (dra-trace), and what test19 leaves.
#
# Run test00-up of n4-cxl-shared-1, -0 and -2 first, build fake-cxl-pool
# and the image of the driver (see test19-many-pools).

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

MBT_MODE="${MBT_MODE:-online}"
MBT_SEED="${MBT_SEED:-$(date +%s)}"
MBT_DURATION="${MBT_DURATION:-20m}"
MBT_STEPS="${MBT_STEPS:-0}"
MBT_DEPTH="${MBT_DEPTH:-3}"
MBT_PROFILE="${MBT_PROFILE:-fullp}"
MBT_UNSETTLED="${MBT_UNSETTLED:-3}"
MBT_AVOID="${MBT_AVOID:-}"
MBT_REPLAY="${MBT_REPLAY:-}"
MBT_SCRIPT="${MBT_SCRIPT:-}"
MBT_TRACE="${MBT_TRACE:-1}"
NUMA_NODE_HB0=2   # the NUMA node of the CXL memory under host bridge 0 (local memdev, pool-a, pool-b)
NUMA_NODE_HB1=3   # and under host bridge 1 (pool-c)

case "$MBT_MODE" in
    online) ;;
    replay) [ -f "$MBT_REPLAY" ] || error "MBT_MODE=replay needs MBT_REPLAY, a file of steps (mbt-steps.txt), not '$MBT_REPLAY'";;
    offline) [ -f "$MBT_SCRIPT" ] || error "MBT_MODE=offline needs MBT_SCRIPT, a code.gen.sh of mbt-cxl generate, not '$MBT_SCRIPT'";;
    *) error "MBT_MODE is $MBT_MODE: online, replay or offline";;
esac
# The fencing mode (A36) and the reboot variant (A41) configure both the
# cluster (the driver configuration, the node-reboot verb) and the model:
# a replay and an off-line run take them from the header of their file
# (MBT_REPLAY, MBT_SCRIPT). An MBT_FENCING or MBT_REBOOT that the user set
# (also set to "": MBT_REBOOT= is no reboot) must be the header's, or the
# test fails here (the model refuses such a contradiction too). Edit the
# header to run a file in another mode. A file without a header has no
# mode: the MBT_* or their defaults.
mbt_mode_file=""
case "$MBT_MODE" in
    replay) mbt_mode_file=$MBT_REPLAY;;
    offline) mbt_mode_file=$MBT_SCRIPT;;
esac
if [ -n "$mbt_mode_file" ] && grep -q '^# --' "$mbt_mode_file"; then
    hdr_fencing=$(mbt-header-mode "$mbt_mode_file" fencing) ||
        error "the header of $mbt_mode_file has no --fencing: not a file of this mbt-cxl"
    hdr_reboot=$(mbt-header-mode "$mbt_mode_file" reboot) ||
        error "the header of $mbt_mode_file has no --reboot: not a file of this mbt-cxl"
    [ -z "${MBT_FENCING+x}" ] || [ "${MBT_FENCING:-allowed-mems}" == "$hdr_fencing" ] ||
        error "MBT_FENCING=$MBT_FENCING, but $mbt_mode_file is of --fencing $hdr_fencing: unset MBT_FENCING, or edit the header of the file"
    [ -z "${MBT_REBOOT+x}" ] || [ "$MBT_REBOOT" == "$hdr_reboot" ] ||
        error "MBT_REBOOT='$MBT_REBOOT', but $mbt_mode_file is of --reboot='$hdr_reboot': unset MBT_REBOOT, or edit the header of the file"
    MBT_FENCING=$hdr_fencing MBT_REBOOT=$hdr_reboot
    echo "mbt: the header of $mbt_mode_file: --fencing $MBT_FENCING --reboot='$MBT_REBOOT'"
fi
MBT_FENCING="${MBT_FENCING:-switch}"
case "$MBT_FENCING" in
    allowed-mems|switch) MBT_FENCING_START=allowed-mems;;
    none)
        MBT_FENCING_START=none
        # The driver configuration of dra-install (dra-helm-values): the
        # node default of every container, with what else is configured.
        ! grep -q '^defaults:' <<< "$DRA_DRIVER_CONFIG" ||
            error "MBT_FENCING=none: DRA_DRIVER_CONFIG has defaults already: $DRA_DRIVER_CONFIG"
        DRA_DRIVER_CONFIG+="${DRA_DRIVER_CONFIG:+
}defaults: {container: {fencing: none}}";;
    *) error "MBT_FENCING is $MBT_FENCING: allowed-mems, none or switch";;
esac
MBT_FENCING_RUN=$MBT_FENCING
MBT_REBOOT="${MBT_REBOOT:-}"
case "$MBT_REBOOT" in
    ""|guest|qemu) ;;
    *) error "MBT_REBOOT is $MBT_REBOOT: guest, qemu or empty (no node-reboot)";;
esac
MBT_REBOOT_RUN=$MBT_REBOOT
echo "### mbt: mode $MBT_MODE, seed $MBT_SEED, duration $MBT_DURATION, steps $MBT_STEPS, profile $MBT_PROFILE, depth $MBT_DEPTH, fencing $MBT_FENCING, reboot ${MBT_REBOOT:-none}"

echo "### 0: the generator"
# Built before the cluster is set up: a model that does not build stops the
# test at once.
if [ -n "$MBT_BIN" ]; then
    echo "MBT_BIN is set: the generator is $MBT_BIN"
else
    # The model is a module of its own (model/go.mod), built without the
    # main module's go.work.
    host-command "cd $(printf %q "$TEST_DIR/model") && GOWORK=off go build -o $(printf %q "$TEST_OUTPUT_DIR/mbt-cxl") . && go version -m $(printf %q "$TEST_OUTPUT_DIR/mbt-cxl") | grep -w github.com/askervin/gofmbt" ||
        command-error "cannot build the model in $TEST_DIR/model"
    MBT_BIN="$TEST_OUTPUT_DIR/mbt-cxl"
fi
[ -x "$MBT_BIN" ] || error "the generator $MBT_BIN is not executable"
echo "the generator: $MBT_BIN"

echo "### 0: preconditions: one cluster, VM2 control plane, VM1 and VM0 workers (test00-up)"
pool-cluster-require

VM0="$POOL_VM0_DIR"    # worker, the rack of pool-b
VM1="$POOL_VM1_DIR"    # worker, the rack of pool-a
VM2="$OUTPUT_DIR"      # control plane, the rack of pool-a, the VM of this test
VMS=("$VM2" "$VM1" "$VM0")
POOLS=(pool-a pool-b pool-c)
declare -A POOL_HOSTS=([pool-a]="$VM1 $VM2" [pool-b]="$VM0" [pool-c]="$VM0 $VM1 $VM2")
declare -A POOL_CONTROL=([pool-a]=qmp [pool-b]=qmp [pool-c]=hmp)
declare -A POOL_BRIDGE=([pool-a]=cxlhb0 [pool-b]=cxlhb0 [pool-c]=cxlhb1)
# The server that knows the local memdev of each VM: the servers with
# local devices (pool-a and pool-b; pool-c has none) that the VM is cabled
# to.
declare -A LOCAL_SERVER=(["$VM2"]=pool-a ["$VM1"]=pool-a ["$VM0"]=pool-b)

echo "### 0: preconditions: patched qemu, hdm_for_passthrough=on, CONFIG_FS_DAX in the three VMs"
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"
pool-vm-require "$VM1" n4-cxl-shared-1
pool-vm-require "$VM0" n4-cxl-shared-0
if [ "$MBT_REBOOT" == guest ]; then
    echo "### 0: preconditions: MBT_REBOOT=guest: the qemu of vm1 and vm0 resets the CXL DVSEC at a guest reset"
    mbt-qemu-fixed-require "$VM1" "$VM0"
fi
echo "### 0: preconditions: the kernel features of persistent pool devices (pmem, fsdax, erofs, loop) in the three VMs"
for vm in "${VMS[@]}"; do
    mbt-kernel-require "$vm"
done

echo "### 0: preconditions: cxl, daxctl and cxl-dump in the VMs, no CXL memory devices"
for vm in "${VMS[@]}"; do
    pool-vm-tools-install "$vm"
done
for vm in "${VMS[@]}"; do
    pool-vm-reset "$vm"
    pool-cxl-dump "$vm" no-devices
    cxl-assert 'memdevs == [] and regions == [] and endpoints == []'
    dra-memmap-on-memory-off "$vm"
done

echo "### 0: preconditions: three pool servers on the host"
pool-server-start --name pool-a --hosts "$VM1 $VM2" --index 0 --slots "*hb0" "
  - name: a-pooled0
    size: 512Mi
    shared: false
    serial: ${MBT_POOLED_SERIAL[a]}
  - name: a-shared0
    size: 256Mi
    shared: true
    serial: ${MBT_SHARED_SERIAL[a]}"
pool-server-start --name pool-b --port 9910 --hosts "$VM0" --index 1 --slots "*hb0" "
  - name: b-pooled0
    size: 512Mi
    shared: false
    serial: ${MBT_POOLED_SERIAL[b]}
  - name: b-shared0
    size: 256Mi
    shared: true
    serial: ${MBT_SHARED_SERIAL[b]}"
# The persistent devices of pool-c start fresh: no data, no labels (the
# server creates missing files as zeros, and never rewrites them).
mbt-pdev-files-remove "$MBT_PDEV_DIR" c-pmem0 c-pmem1
pool-server-start --name pool-c --port 9911 --hosts "$VM0 $VM1 $VM2" --index 2 --control hmp --slots "*hb1" --local-devices no "
  - name: c-shared0
    size: 256Mi
    shared: true
    serial: ${MBT_SHARED_SERIAL[c]}
  - name: c-pooled0
    size: 256Mi
    shared: false
    serial: ${MBT_POOLED_SERIAL[c]}
  - name: c-pmem0
    size: 256Mi
    shared: true
    persistent: true
    serial: ${MBT_PMEM_SERIAL[c]}
  - name: c-pmem1
    size: 256Mi
    shared: false
    persistent: true
    serial: ${MBT_XPMEM_SERIAL[c]}"
# Until dra-install sets its trap (mbt-exit-trap): the files go too.
trap 'pool-cleanup; mbt-pdev-files-remove "$MBT_PDEV_DIR" c-pmem0 c-pmem1' EXIT
mbt-pdev-files c-pmem0 c-pmem1
for pool in "${POOLS[@]}"; do
    want=""
    for vm in ${POOL_HOSTS[$pool]}; do
        want+="'$(basename "$vm")', "
    done
    pool-host-client --pool "$pool" -o json hosts || command-error "cannot list the hosts of $pool"
    pool-assert "$COMMAND_OUTPUT" "sorted(h['name'] for h in j) == sorted([$want])"
    pool-assert "$COMMAND_OUTPUT" "all(h.get('control') == '${POOL_CONTROL[$pool]}' and h.get('state') == 'running' for h in j)"
    pool-assert "$COMMAND_OUTPUT" "all(len(h['slots']) == 4 and all(s['hostBridge'] == '${POOL_BRIDGE[$pool]}' for s in h['slots']) for h in j)"
done
pool-client-install "$VM2" "$VM1" "$VM0"

echo "### 0: preconditions: three pool controllers, the driver on every node"
dra-build
# The EXIT trap of dra-install: stop the generator, bring the controllers
# and drivers back, then the usual dra-cleanup and pool-cleanup.
DRA_EXIT_TRAP=mbt-exit-trap
mbt-setup-ids "$VM2" "$VM1" "$VM0"
dra-install
[ "${DRA_CONTROLLER_UNITS[*]}" == "${MBT_POOL_UNIT[a]} ${MBT_POOL_UNIT[b]} ${MBT_POOL_UNIT[c]}" ] ||
    error "dra-install started the controllers ${DRA_CONTROLLER_UNITS[*]}, expected one per pool"
vmdir-command "$VM2" "systemctl is-active ${DRA_CONTROLLER_UNITS[*]}" ||
    command-error "the pool controllers do not all run"
for p in "${MBT_POOL_IDS[@]}"; do
    vmdir-command "$VM2" "journalctl --no-pager -o cat -u ${MBT_POOL_UNIT[$p]} | grep -m1 -F 'pool ${MBT_POOL_SERVER[$p]} (from the server)'" ||
        command-error "${MBT_POOL_UNIT[$p]} did not take the pool name ${MBT_POOL_SERVER[$p]} from its server"
done
echo "### 0: the fencing mode of every driver: $MBT_FENCING_START (MBT_FENCING=$MBT_FENCING)"
mbt-fencing-check "$MBT_FENCING_START"
trap 'echo "mbt: SIGTERM"; exit 143' TERM

echo "### 0: the local memdev of every VM, attached by the server that knows it"
for vm in "${VMS[@]}"; do
    dra-local-device --pool "${LOCAL_SERVER[$vm]}" "$vm"
    [ "$DRA_LOCAL_SERIAL" == "$MBT_LOCAL_SERIAL" ] ||
        error "the local memdev of $(vmdir-label "$vm") is $DRA_LOCAL_SERIAL, expected $MBT_LOCAL_SERIAL"
done
LOCAL_BLOCKS=$(mbt-local-blocks) ||
    error "a node does not publish the local memdev memdev-${MBT_LOCAL_HEX,,} with its capacity in its cxl.nri.io ResourceSlice"
echo "local memdevs in blocks of $MBT_LOCAL_SIZE: $LOCAL_BLOCKS"

echo "### 0: images, tools, claim templates, the observer"
for vm in "${VMS[@]}"; do
    dra-pull-images "$vm" "$DRA_TOOLS_IMAGE" "$DRA_COMPANION_IMAGE"
done
dra-tools-install "$VM2" "pmem-rw.py=$TEST_DIR/pmem-rw.py" "hello.erofs.hex=$TEST_DIR/hello.erofs.hex"
mbt-templates-apply
vmdir-command "$VM2" "kubectl get resourceclaimtemplates -n $DRA_NS" ||
    command-error "cannot list the ResourceClaimTemplates"
mbt-observer-install "$VM2" "$VM1" "$VM0"
mbt-observe 0 1
echo "a settle observation took ${MBT_OBS_SECONDS} s ($(wc -c < "$MBT_OBS_FILE") bytes): $MBT_OBS_FILE"
dra-py assert "$MBT_OBS" "j['errors'] == [] and j['pods'] == [] and all(j['vms'][v] and j['vms'][v]['errors'] == [] for v in ('vm0', 'vm1', 'vm2'))" ||
    error "the first observation has errors or pods: $(head -c 2000 "$MBT_OBS_FILE")"
# The persistent devices are fresh, and no VM has a trace of an earlier
# run: no pmem region, node mount, lease bind or loop device over pmem.
dra-py assert "$MBT_OBS" "all(j['pdevs'][d]['lsa'] == 'zero' and not j['pdevs'][d]['pfn'] for d in ('c-pmem0', 'c-pmem1')) and all(j['vms'][v][k] == [] for v in ('vm0', 'vm1', 'vm2') for k in ('pmem', 'fsMounts', 'leaseBinds', 'loops'))" ||
    error "the first observation has persistent devices that are not fresh, or pmem regions, mounts or loop devices: $(head -c 2000 "$MBT_OBS_FILE")"
[ "$MBT_TRACE" == 0 ] || dra-trace-start "$VM2" "$VM1" "$VM0"

echo "### 0: the kube-scheduler, whose requeueing the stall window of A19/A28 follows"
sched=$(vmdir-command-q "$VM2" "p=\$(kubectl get pods -n kube-system -l component=kube-scheduler -o jsonpath='{.items[0].metadata.name}')
echo \"image \$(kubectl get pod -n kube-system \$p -o jsonpath='{.spec.containers[0].image}')\"
echo \"version \$(kubectl logs -n kube-system \$p 2>/dev/null | grep -m1 -o 'Starting Kubernetes Scheduler.*version=[^ ]*' | sed 's/.*version=//')\"") ||
    echo "WARNING: cannot read the kube-scheduler of the cluster: $sched"
echo "kube-scheduler: $(tr '\n' ' ' <<< "$sched")"
echo "mbt: stall window: ${MBT_STALL_WINDOW:-the default of the model}"

echo "### 1: the run"
export MBT_OUT="$TEST_OUTPUT_DIR"
# The MBT_*, the facts of the cluster, MBT_ARGS. A replayed file's header
# wins over the MBT_* for the flags of the model (it says so on its
# stderr, A16); its fencing and reboot are the run's already.
MBT_FLAGS=()
if [ "$MBT_MODE" == replay ]; then
    hdr_blocks=$(sed -n 's/^# .*--local-blocks[ =]\([^ ]*\).*/\1/p' "$MBT_REPLAY" | head -n 1)
    [ -z "$hdr_blocks" ] || [ "$hdr_blocks" == "$LOCAL_BLOCKS" ] ||
        echo "WARNING: $MBT_REPLAY was recorded with --local-blocks $hdr_blocks, the cluster has $LOCAL_BLOCKS: the replay may diverge"
    hdr_stall=$(sed -n 's/^# .*--stall-window[ =]\([^ ]*\).*/\1/p' "$MBT_REPLAY" | head -n 1)
    [ -z "$hdr_stall" ] || [ "$hdr_stall" == "${MBT_STALL_WINDOW:-6m}" ] ||
        echo "WARNING: $MBT_REPLAY was recorded with --stall-window $hdr_stall, this run has ${MBT_STALL_WINDOW:-the default}: the replay may diverge"
fi
MBT_FLAGS+=(
    --seed="$MBT_SEED"
    --depth="$MBT_DEPTH"
    --profile="$MBT_PROFILE"
    --unsettled="$MBT_UNSETTLED"
    --fencing="$MBT_FENCING"
    --reboot="$MBT_REBOOT"
    --duration="$MBT_DURATION"
    --steps="$MBT_STEPS"
    --settle-timeout="${MBT_SETTLE_TIMEOUT}s"
    --local-blocks="$LOCAL_BLOCKS"
    --numa-node-hb0="$NUMA_NODE_HB0"
    --numa-node-hb1="$NUMA_NODE_HB1"
    --out="$TEST_OUTPUT_DIR"
)
[ -z "$MBT_AVOID" ] || MBT_FLAGS+=(--avoid="$MBT_AVOID")
# The stall window follows the cluster's kube-scheduler: the model's
# default, or the user's.
[ -z "${MBT_STALL_WINDOW:-}" ] || MBT_FLAGS+=(--stall-window="$MBT_STALL_WINDOW")
# More flags of mbt-cxl, words: for instance --plan-ahead=false.
read -r -a words <<< "${MBT_ARGS:-}"
MBT_FLAGS+=("${words[@]}")
case "$MBT_MODE" in
    online) mbt-online online "${MBT_FLAGS[@]}";;
    replay) mbt-online online --replay="$MBT_REPLAY" "${MBT_FLAGS[@]}";;
    offline) mbt-offline "$MBT_SCRIPT";;
esac
echo "### mbt: $(( $(wc -l < "$MBT_TIMES") - 1 )) steps, $(( $(wc -l < "$MBT_OBS_TIMES") - 1 )) observations; times in $MBT_TIMES and $MBT_OBS_TIMES"

echo "### 2: teardown: every pod deleted, every device back"
mbt-teardown
for vm in "${VMS[@]}"; do
    pool-vm-monitor "$vm" "info qtree -b" | grep -c 'dev: cxl-type3' | grep -qx 1 ||
        error "qemu of $(vmdir-label "$vm") does not have exactly its local CXL memory device: $(pool-vm-monitor "$vm" "info qtree -b" | grep -c 'dev: cxl-type3') devices"
done
if [ -n "$DRA_TRACE_RUNNING" ]; then
    dra-trace-stop
    dra-trace-summary > /dev/null
fi

echo "### cleanup"
dra-cleanup
for vm in "${VMS[@]}"; do
    dra-local-detach --pool "${LOCAL_SERVER[$vm]}" "$vm"
done
pool-cleanup
mbt-pdev-files-remove "$MBT_PDEV_DIR" c-pmem0 c-pmem1
trap - EXIT
trap - TERM
for port in "$POOL_PORT" 9910 9911; do
    pool-port-busy "$port" && error "port $port is still busy after stopping the servers"
done
for vm in "${VMS[@]}"; do
    pool-vm-monitor "$vm" "info qtree -b" | grep -q 'dev: cxl-type3' &&
        error "qemu of $(vmdir-label "$vm") still has a CXL memory device"
done
[ "$MBT_TRACE" == 0 ] || echo "trace: $TEST_OUTPUT_DIR/trace.txt, summary: $TEST_OUTPUT_DIR/trace-summary.txt"
echo "mbt: steps $TEST_OUTPUT_DIR/mbt-steps.txt, executor log $MBT_EXEC_LOG, generator log $MBT_LOG"
