# The executor of test90-mbt-stress (contract.txt): the
# actions of the model mbt-cxl on the cluster of test19-many-pools, the
# settle observation, and the line protocol with mbt-cxl, on-line (a
# coprocess) and off-line (a generated code.gen.sh).
#
# The framework sources this file for this test only (a test-level
# *.source* file), after dra.source.sh and pool.source.sh.
#
# Names (contract 1-3): the VM ids vm2 (the VM of the test, the control
# plane), vm1 and vm0; the pool ids a, b and c (servers pool-a, pool-b
# and pool-c); slots KIND@VM#I, KIND one of the kinds of contract 2, VM a
# VM id or "any"; a slot holds at most one pod, mbt-KIND-VM-I-g<gen>, with
# the label mbt.slot=KIND_VM_I and the claim c0 from the
# ResourceClaimTemplate mbt-KIND (dnuma, amendment A39: c0 from mbt-lnuma
# and c1 from mbt-xnuma-c).
#
# Every action is a function mbt-act-<verb> that sets MBT_STATUS (via its
# return value), MBT_RESULT (the done JSON of contract 6.2) and MBT_NOTE
# (a word for the test output); mbt-run runs one, mbt-observe makes the
# settle observation (mbt-observe.py in each VM and on the host). Output:
# one "### mbt step N: VERB ARGS" line and one result line per step; the
# commands and their output go to $TEST_OUTPUT_DIR/mbt-exec.log, the
# observations to $TEST_OUTPUT_DIR/mbt-obs/ (the last MBT_OBS_KEEP, and
# fail-N.json of a failed step), the time of every step to
# mbt-times.tsv and of every observation to mbt-obs-times.tsv, the stacks
# of a VM's part of an observation that timed out to mbt-obs-stacks/.

MBT_HELPER_DIR="$(dirname "${BASH_SOURCE[0]}")"
MBT_OBSERVER="$MBT_HELPER_DIR/mbt-observe.py"   # the observer, on the host
MBT_OBSERVER_VM=/usr/local/bin/mbt-observe.py   # and in the VMs (mbt-observer-install)
MBT_SLOT_LABEL=mbt.slot
MBT_LOCAL_SERIAL=0xc100e2e1                     # the local memdev of every VM
MBT_LOCAL_HEX=C100E2E1
MBT_LOCAL_SIZE=128Mi                                 # a local claim or lease
MBT_LEASE_WAIT=60s                              # cxl-request request --wait
MBT_EXEC_TIMEOUT=150                            # seconds of a kubectl exec in a pod
MBT_EXEC_RETRY=30                               # seconds to retry a kubectl exec that cannot connect
MBT_READ_TIMEOUT="${MBT_READ_TIMEOUT:-900}"     # seconds to wait for a line of the generator
MBT_OBS_KEEP="${MBT_OBS_KEEP:-30}"              # observations kept in mbt-obs/
MBT_OBS_TIMEOUT="${MBT_OBS_TIMEOUT:-60}"        # seconds of one VM's part of an observation
MBT_OBS_STACKS_MARKER="mbt-observe: the observation is still running, stacks follow"  # of mbt-observe.py --stacks-after
MBT_NRI_RESTART_WAIT=60                         # containerd-restart: seconds to wait for the driver's restart
MBT_DRIVER_READY_WAIT="${MBT_DRIVER_READY_WAIT:-120}"   # seconds to wait for a driver to be ready (mbt-driver-ready)
MBT_LENDS_RESTORED="NRI fence: lends restored"  # the driver's log line when NRI and lending are up
MBT_FENCING_LOG="defaults.container.fencing: "  # the driver's start line of its fencing mode (A36)
MBT_FENCING_CM=nri-dra-driver-cxl-config        # the ConfigMap of the driver configuration (config.yaml), in kube-system
MBT_FENCING_RUN=""                 # the fencing mode of the run (MBT_FENCING of code.var.sh): allowed-mems, none or switch
MBT_FENCING_NOW=""                 # the mode of the drivers now: allowed-mems or none (mbt-fencing-check, fencing-switch)
MBT_FENCING_GOT=""                 # the mode that the last mbt-fencing-wait read in a driver's log, empty if none
MBT_POLL="${MBT_POLL:-3}"                       # seconds between settle observations
MBT_SETTLE_TIMEOUT="${MBT_SETTLE_TIMEOUT:-180}" # off-line: seconds of a settle (on-line: Go's)
MBT_VMIDS=(vm2 vm1 vm0)
MBT_POOL_IDS=(a b c)
declare -A MBT_POOL_SERVER=([a]=pool-a [b]=pool-b [c]=pool-c)
declare -A MBT_POOLED_SERIAL=([a]=0xc1ee0001 [b]=0xc1ee1001 [c]=0xc1ee2001)
declare -A MBT_SHARED_SERIAL=([a]=0xc1ae0001 [b]=0xc1ae1001 [c]=0xc1ae2001)
# Persistent pool devices (amendment A22): only pool c has them, P-pmem0
# shared, P-pmem1 exclusive.
declare -A MBT_PMEM_SERIAL=([c]=0xc1ae2002)
declare -A MBT_XPMEM_SERIAL=([c]=0xc1ee2002)
declare -A MBT_PDEV_POOL=([c-pmem0]=c [c-pmem1]=c)   # persistent device -> its pool id
declare -A MBT_PDEV_FILES=()       # persistent device -> DATA:LSA, its backing files on the host (mbt-pdev-files)
MBT_PDEV_DIR="/tmp/fake-cxl-pool-${MBT_POOL_SERVER[c]}"   # their directory (pool-server-start --name pool-c)
MBT_PMEM_TOOL=/tools/pmem-rw.py    # pmem-rw.py in the pods (dra-tools-install)
MBT_HOLD_STOP=/tmp/mbt-hold.stop   # hold/unhold: the stop file of the holder in the container,
MBT_HOLD_LOG=/tmp/mbt-hold.log     # and its output
# The command line of a holder (pgrep -f): anchored, so that it matches
# no shell whose script names the tool.
MBT_HOLDER_RE='^python3 [^ ]*pmem-rw\.py (lease fs )?hold '
MBT_FS_LEASE_NAME=mbt              # lease-get pfd: --name
MBT_LEASE_PUT_WAIT="${MBT_LEASE_PUT_WAIT:-20s}"   # lease-put of plse: cxl-request release --wait
MBT_KILLAT_TIMEOUT="${MBT_KILLAT_TIMEOUT:-60}"      # driver-kill-at: seconds until the watcher gives up
MBT_KILLAT_STATE=/run/mbt-killat/state.json         # its state in the VM
MBT_KILLAT_VM=""                   # the VM id of an armed driver-kill-at watcher not reported done yet
MBT_REBOOT_RUN=""                  # the reboot variant of the run (MBT_REBOOT of code.var.sh, A41): guest, qemu or none
MBT_REBOOT_WAIT="${MBT_REBOOT_WAIT:-180}"           # node-reboot: seconds to wait for ssh with a new boot id
MBT_KERNEL_STATE=/run/mbt-observe/kernel.json       # the observer's state of the kernel log in the VM (mbt-observe.py kernel)
declare -A MBT_VMDIR=()            # VM id -> VMDIR (mbt-setup-ids)
declare -A MBT_NODE=()             # VM id -> Kubernetes node name
declare -A MBT_POOL_UNIT=()        # pool id -> systemd unit of its controller
MBT_NODES=""                       # "vm2=NODE,vm1=NODE,vm0=NODE"
MBT_UNITS=""                       # "a=UNIT,b=UNIT,c=UNIT"
MBT_POOLS=""                       # "a=pool-a,b=pool-b,c=pool-c": pool id -> DRA pool (server) name
declare -A MBT_GEN=()              # slot -> generation of its last pod, 0: none yet
declare -A MBT_DRIVER_SEEN=()      # VM id -> "POD RESTARTS" of its driver at the last observation
declare -A MBT_TRACE_REFOLLOW=()   # VM id -> time (RFC 3339) to follow the driver log from again
MBT_TMP=""                         # work files
MBT_OBS_DIR=""                     # observations
MBT_OBS_FILES=()                   # the files of the observations kept, oldest first
MBT_EXEC_LOG=""                    # the commands of every step and their output
MBT_TIMES=""                       # step, verb, args, status, seconds, observations
MBT_OBS_TIMES=""                   # step, poll, seconds of every observation
MBT_LOG=""                         # stderr of the generator (mbt-cxl.log)
MBT_BIN="${MBT_BIN:-}"            # the generator: mbt-cxl, or what the user exported
MBT_GEN_PID=""                     # the coprocess MBTGEN: its pid
MBT_RFD=""                         # and a copy of its stdout that outlives it
MBT_LINE=""                        # mbt-recv: the last line of the generator
MBT_STATUS=0                       # mbt-run: the exit status of the step,
MBT_RESULT='{}'                    # its done JSON,
MBT_NOTE=""                        # and a note for the output
MBT_OUT=""                         # mbt-sh: stdout of the command,
MBT_ERR=""                         # and its stderr
MBT_OBS=""                         # mbt-observe: the settle observation (one JSON line),
MBT_OBS_FILE=""                    # its file,
MBT_OBS_SECONDS=""                 # and how long it took
MBT_DRIVER_NEW=""                  # mbt-driver-replace: the new driver pod
MBT_KIND="" MBT_BASE="" MBT_POOL="" MBT_SVM="" MBT_SIDX="" MBT_SLABEL="" MBT_SPOD=""  # mbt-slot
MBT_LAST_STEP="" MBT_LAST_VERB="" MBT_LAST_DONE=""   # off-line: the last mbt-step,
MBT_LAST_STATUS=0 MBT_LAST_CHECKED=1                 # its status, and whether an mbt-expect checked it

# The main process of every pod: a shell whose child is the workload. The
# shell is PID 1 of the container, which ignores SIGSEGV from outside, so a
# crash is a SIGSEGV to the child: the shell exits with its status, 139,
# and the container restarts (restartPolicy Always). SIGTERM (pod deleted)
# ends it with 0.
MBT_WORKLOAD='trap "exit 0" TERM
sleep infinity &
echo $! > /tmp/workload.pid
wait $!
exit $?'

###
### Names
###

mbt-controller-start() {
    # Usage: mbt-controller-start UNIT
    #
    # Start the pool controller UNIT of dra-install (one of
    # DRA_CONTROLLER_UNITS) again after it stopped or was killed: the unit
    # is transient with Restart=no, so a killed one stays failed until
    # "systemctl reset-failed", which also removes it, and systemd-run
    # creates it again with the command line of dra-install
    # (dra-controller-run). Wait until it is active, 10 s at most. Return
    # non-zero if it does not start, for instance because it runs already.
    local unit="$1"
    vmdir-command "$OUTPUT_DIR" "systemctl reset-failed $unit 2>/dev/null; systemctl is-active $unit" &&
        { echo "mbt-controller-start: $unit is active already" >&2; return 1; }
    dra-controller-run "$unit" || return 1
    retry-until --timeout 10 --interval 1 --message "$unit active" \
        'vmdir-command-q "$OUTPUT_DIR" "systemctl -q is-active $unit"'
}

mbt-controller-stop() {
    # Usage: mbt-controller-stop UNIT
    #
    # Stop the pool controller UNIT of dra-install gracefully (systemctl
    # stop: SIGTERM, the controller deletes the ResourceSlices of its pool
    # before it exits) and remove the unit (reset-failed). Return non-zero
    # if it is still active.
    vmdir-command "$OUTPUT_DIR" "systemctl stop $1; systemctl reset-failed $1 2>/dev/null; ! systemctl -q is-active $1"
}

mbt-controller-kill() {
    # Usage: mbt-controller-kill UNIT
    #
    # Kill the pool controller UNIT of dra-install with SIGKILL: it leaves
    # its ResourceSlices behind and, with Restart=no, its unit stays failed
    # until mbt-controller-start. Wait until it is not active any more, 5 s
    # at most. Return non-zero if it was not active or is still active.
    vmdir-command "$OUTPUT_DIR" "systemctl -q is-active $1 || { echo '$1 is not active'; exit 1; }
systemctl kill --signal=SIGKILL $1 || exit 1
for i in \$(seq 50); do systemctl -q is-active $1 || break; sleep 0.1; done
systemctl is-active $1; ! systemctl -q is-active $1"
}

mbt-driver-kill() {
    # Usage: mbt-driver-kill VMDIR
    #
    # Kill the driver on the node of the VM of VMDIR in place: SIGKILL to
    # the process of its container (found with crictl in the VM), so that
    # kubelet starts the container again in the same pod (restartPolicy
    # Always of the DaemonSet, with the back-off of a crashing container),
    # unlike dra-driver-restart, which replaces the pod. Do not wait for the
    # restart. COMMAND_OUTPUT has "killed POD CONTAINER-ID pid PID". Return
    # non-zero if no container of the driver runs there or the kill fails.
    vmdir-command "$1" "id=\$(crictl ps -q --state running --namespace '^kube-system\$' --name '^$DRA_CONTAINER\$' 2>/dev/null | head -n 1)
[ -n \"\$id\" ] || { echo 'no running container $DRA_CONTAINER'; exit 1; }
set -- \$(crictl inspect -o go-template --template '{{index .status.labels \"io.kubernetes.pod.name\"}} {{.info.pid}}' \$id 2>/dev/null)
[ -n \"\$2\" ] && [ \"\$2\" != 0 ] || { echo \"no pid of container \$id\"; exit 1; }
kill -KILL \"\$2\" && echo \"killed \$1 \$id pid \$2\""
}

mbt-node-service-restart() {
    # Usage: mbt-node-service-restart VMDIR kubelet|containerd [TIMEOUT]
    #
    # Restart kubelet or containerd in the VM of VMDIR (systemctl restart;
    # the containers keep running) and wait until the service is active
    # and answers again (kubelet: its healthz endpoint, containerd: crictl
    # info) and its node is Ready in the cluster of the VM of the test,
    # TIMEOUT seconds at most (default 120). Return non-zero on failure or
    # timeout.
    local vmdir="$1" service="$2" tmo="${3:-120}" name check
    name=$(vmdir-name "$vmdir")
    # shellcheck disable=SC2034 # check is used in the snippet of retry-until
    case "$service" in
        kubelet) check="curl -sf --max-time 2 http://127.0.0.1:10248/healthz >/dev/null";;
        containerd) check="crictl info >/dev/null 2>&1";;
        *) echo "mbt-node-service-restart: unknown service '$service' (kubelet or containerd)" >&2; return 2;;
    esac
    vmdir-command "$vmdir" "systemctl restart $service" || return 1
    retry-until --timeout "$tmo" --interval 1 --message "$(vmdir-label "$vmdir"): $service active and answering, node $name Ready" \
        'vmdir-command-q "$vmdir" "systemctl -q is-active $service && $check" && mbt-node-ready "$vmdir"'
}

mbt-node-ready() {
    # Usage: mbt-node-ready VMDIR
    #
    # Return 0 if the node of the VM of VMDIR is Ready in the cluster of the
    # VM of the test.
    local status
    status=$(vmdir-command-q "$OUTPUT_DIR" "kubectl get node $(vmdir-name "$1") -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'") || return 1
    [ "$status" == True ]
}

mbt-setup-ids() {
    # Usage: mbt-setup-ids VM2DIR VM1DIR VM0DIR
    #
    # Map the VM ids of the model to the VMDIRs and the Kubernetes node
    # names, the pool ids to the units of their controllers (as
    # dra-install names them for pool-a, pool-b and pool-c started in this
    # order), and create the work and output files of the executor in
    # $TEST_OUTPUT_DIR (those of an earlier run are removed).
    local vm
    MBT_VMDIR=([vm2]="$1" [vm1]="$2" [vm0]="$3")
    MBT_NODES=""
    for vm in "${MBT_VMIDS[@]}"; do
        MBT_NODE[$vm]=$(vmdir-name "${MBT_VMDIR[$vm]}")
        MBT_NODES+="${MBT_NODES:+,}$vm=${MBT_NODE[$vm]}"
    done
    MBT_POOL_UNIT=([a]="$DRA_CONTROLLER_UNIT" [b]="$DRA_CONTROLLER_UNIT-pool-b" [c]="$DRA_CONTROLLER_UNIT-pool-c")
    MBT_UNITS="a=${MBT_POOL_UNIT[a]},b=${MBT_POOL_UNIT[b]},c=${MBT_POOL_UNIT[c]}"
    MBT_POOLS="a=${MBT_POOL_SERVER[a]},b=${MBT_POOL_SERVER[b]},c=${MBT_POOL_SERVER[c]}"
    MBT_TMP="$TEST_OUTPUT_DIR/mbt-tmp"
    MBT_OBS_DIR="$TEST_OUTPUT_DIR/mbt-obs"
    MBT_EXEC_LOG="$TEST_OUTPUT_DIR/mbt-exec.log"
    MBT_TIMES="$TEST_OUTPUT_DIR/mbt-times.tsv"
    MBT_OBS_TIMES="$TEST_OUTPUT_DIR/mbt-obs-times.tsv"
    MBT_LOG="$TEST_OUTPUT_DIR/mbt-cxl.log"
    # The output directory of a test is reused: files of an earlier run
    # would pass for this one's.
    rm -rf "$MBT_TMP" "$MBT_OBS_DIR" "$TEST_OUTPUT_DIR/mbt-fail.txt" "$TEST_OUTPUT_DIR/mbt-steps.txt" \
        "$TEST_OUTPUT_DIR/$DRA_CONTAINER".*.log
    mkdir -p "$MBT_TMP" "$MBT_OBS_DIR" || error "mbt-setup-ids: cannot create $MBT_TMP and $MBT_OBS_DIR"
    : > "$MBT_TMP/inspected"
    : > "$MBT_EXEC_LOG"
    : > "$MBT_LOG"
    printf 'step\tverb\targs\tstatus\tseconds\tobservations\n' > "$MBT_TIMES"
    printf 'step\tpoll\tseconds\n' > "$MBT_OBS_TIMES"
    MBT_GEN=() MBT_DRIVER_SEEN=() MBT_TRACE_REFOLLOW=() MBT_OBS_FILES=()
}

mbt-slot() {
    # Usage: mbt-slot SLOT
    #
    # Parse SLOT (KIND@VM#I) into MBT_KIND (sdax-a), MBT_BASE (sdax),
    # MBT_POOL (a, "" for kinds without a pool), MBT_SVM (vm1, or any),
    # MBT_SIDX (I), MBT_SLABEL (its label value, sdax-a_vm1_0) and MBT_SPOD
    # (its pod name without the generation, mbt-sdax-a-vm1-0). Return 2 if
    # SLOT is not a slot.
    local re='^((xnuma|xdax|sdax|sro|sexcl|pwr|pfsd|pfsc|pbro|pbad|xpb|xpf)-([abc])|lnuma|ldax|lsteer|dnuma|lease|plse|plain)@(vm[012]|any)#([0-9]+)$'
    [[ "$1" =~ $re ]] || { MBT_ERR="not a slot: '$1'"; return 2; }
    MBT_KIND=${BASH_REMATCH[1]}
    MBT_POOL=${BASH_REMATCH[3]}
    MBT_BASE=${BASH_REMATCH[2]:-$MBT_KIND}
    MBT_SVM=${BASH_REMATCH[4]}
    MBT_SIDX=${BASH_REMATCH[5]}
    MBT_SLABEL="${MBT_KIND}_${MBT_SVM}_${MBT_SIDX}"
    MBT_SPOD="mbt-$MBT_KIND-$MBT_SVM-$MBT_SIDX"
    if [ "$(mbt-pclass)" != none ] && [ -n "$MBT_POOL" ] && [ -z "${MBT_PMEM_SERIAL[$MBT_POOL]:-}" ]; then
        MBT_ERR="not a slot: '$1': pool $MBT_POOL has no persistent devices"
        return 2
    fi
}

mbt-pclass() {
    # Usage: mbt-pclass
    #
    # Print the class of the kind of the last mbt-slot for the verbs of
    # persistent devices (amendment A23): block (pwr, xpb, pbad: a block
    # device that the claim may write), block-ro (pbro), fs (pfsd, pfsc,
    # xpf), plse (the lease kind of persistent leases), none (the others).
    case "$MBT_BASE" in
        pwr|xpb|pbad) echo block;;
        pbro) echo block-ro;;
        pfsd|pfsc|xpf) echo fs;;
        plse) echo plse;;
        *) echo none;;
    esac
}

mbt-pmem-key() {
    # Usage: mbt-pmem-key
    #
    # Print the key of the environment of the persistent device of the kind
    # of the last mbt-slot (CXL_<KEY>_PATH): P-pmem0 for pwr, pfsd, pfsc,
    # pbro and pbad, P-pmem1 for xpb and xpf. Return 2 for other kinds.
    local serial
    case "$MBT_BASE" in
        pwr|pfsd|pfsc|pbro|pbad) serial=${MBT_PMEM_SERIAL[$MBT_POOL]:-};;
        xpb|xpf) serial=${MBT_XPMEM_SERIAL[$MBT_POOL]:-};;
        *) MBT_ERR="$MBT_KIND has no persistent device"; return 2;;
    esac
    [ -n "$serial" ] || { MBT_ERR="pool $MBT_POOL has no persistent device"; return 2; }
    serial=${serial#0x}
    echo "${serial^^}"
}

mbt-slot-pod() {
    # Usage: mbt-slot-pod SLOT
    #
    # mbt-slot SLOT, and print the name of the pod of its last generation.
    # Return 2 if SLOT is not a slot or has had no pod yet (in $(...) the
    # reason is lost: mbt-slot-has-pod sets MBT_ERR).
    mbt-slot-has-pod "$1" || return 2
    echo "$MBT_SPOD-g${MBT_GEN[$1]}"
}

mbt-slot-has-pod() {
    # Usage: mbt-slot-has-pod SLOT
    #
    # mbt-slot SLOT; return 2, with MBT_ERR, if it is not a slot or has had
    # no pod yet.
    mbt-slot "$1" || return 2
    [ "${MBT_GEN[$1]:-0}" -gt 0 ] || { MBT_ERR="slot $1 has had no pod"; return 2; }
}

mbt-vm-arg() {
    # Usage: mbt-vm-arg VM
    #
    # Return 2 unless VM is a VM id.
    [ -n "${MBT_VMDIR[$1]:-}" ] || { MBT_ERR="not a VM id: '$1'"; return 2; }
}

mbt-pool-arg() {
    # Usage: mbt-pool-arg POOL
    #
    # Return 2 unless POOL is a pool id.
    [ -n "${MBT_POOL_SERVER[$1]:-}" ] || { MBT_ERR="not a pool id: '$1'"; return 2; }
}

mbt-dax-key() {
    # Usage: mbt-dax-key
    #
    # Print the key of the environment of the device of the kind of the
    # last mbt-slot (CXL_<KEY>_DAX), for the kinds with a dax device.
    # Return 2 for the others.
    local serial
    case "$MBT_BASE" in
        xdax) serial=${MBT_POOLED_SERIAL[$MBT_POOL]};;
        sdax|sro|sexcl) serial=${MBT_SHARED_SERIAL[$MBT_POOL]};;
        ldax) serial=$MBT_LOCAL_SERIAL;;
        *) MBT_ERR="$MBT_KIND has no dax device"; return 2;;
    esac
    serial=${serial#0x}
    echo "${serial^^}"
}

###
### YAML
###

mbt-claim-to-template() {
    # Usage: mbt-claim-to-template NAME <<< CLAIM-YAML
    #
    # Print the ResourceClaimTemplate NAME whose claims are the
    # ResourceClaim of CLAIM-YAML (dra-shared-claim-yaml, for instance).
    awk -v name="$1" '
        /^kind: ResourceClaim$/ { print "kind: ResourceClaimTemplate"; next }
        /^metadata:$/ { meta = 1; print; next }
        meta && /^  name:/ { print "  name: " name; meta = 0; next }
        /^spec:$/ { print "spec:"; print "  spec:"; spec = 1; next }
        spec { print "  " $0; next }
        { print }'
}

mbt-template-yaml() {
    # Usage: mbt-template-yaml KIND
    #
    # Print the ResourceClaimTemplate mbt-KIND of the claim of the kind
    # KIND (contract 2): pool devices by serial, local slices of
    # $MBT_LOCAL_SIZE of class cxl-local, the lease claim.
    local kind="$1" pool="${1##*-}" pc=cxl-pool-persistent
    case "$kind" in
        xnuma-?) dra-shared-claim-yaml x "${MBT_POOLED_SERIAL[$pool]}" "" "" cxl-pool | mbt-claim-to-template "mbt-$kind";;
        xdax-?) dra-shared-claim-yaml x "${MBT_POOLED_SERIAL[$pool]}" "" "mode: dax" cxl-pool | mbt-claim-to-template "mbt-$kind";;
        sdax-?) dra-shared-claim-yaml x "${MBT_SHARED_SERIAL[$pool]}" 1 "mode: dax" | mbt-claim-to-template "mbt-$kind";;
        sro-?) dra-shared-claim-yaml x "${MBT_SHARED_SERIAL[$pool]}" 1 "mode: dax, readOnly: true" | mbt-claim-to-template "mbt-$kind";;
        sexcl-?) dra-shared-claim-yaml x "${MBT_SHARED_SERIAL[$pool]}" "" "mode: dax" | mbt-claim-to-template "mbt-$kind";;
        # Persistent devices (A22), class cxl-pool-persistent: the writer
        # takes every share, the readers one each.
        pwr-?) dra-shared-claim-yaml x "${MBT_PMEM_SERIAL[$pool]}" "" "mode: block" $pc | mbt-claim-to-template "mbt-$kind";;
        pfsd-?) dra-shared-claim-yaml x "${MBT_PMEM_SERIAL[$pool]}" 1 "mode: fs, readOnly: true" $pc | mbt-claim-to-template "mbt-$kind";;
        pfsc-?) dra-shared-claim-yaml x "${MBT_PMEM_SERIAL[$pool]}" 1 "mode: fs, readOnly: true, access: cached, path: /models" $pc | mbt-claim-to-template "mbt-$kind";;
        pbro-?) dra-shared-claim-yaml x "${MBT_PMEM_SERIAL[$pool]}" 1 "mode: block, readOnly: true" $pc | mbt-claim-to-template "mbt-$kind";;
        pbad-?) dra-shared-claim-yaml x "${MBT_PMEM_SERIAL[$pool]}" 1 "mode: block" $pc | mbt-claim-to-template "mbt-$kind";;
        xpb-?) dra-shared-claim-yaml x "${MBT_XPMEM_SERIAL[$pool]}" "" "mode: block" $pc | mbt-claim-to-template "mbt-$kind";;
        xpf-?) dra-shared-claim-yaml x "${MBT_XPMEM_SERIAL[$pool]}" "" "mode: fs, readOnly: true" $pc | mbt-claim-to-template "mbt-$kind";;
        lnuma) dra-claim-yaml template mbt-lnuma cxl-local "$MBT_LOCAL_SIZE";;
        ldax) dra-claim-yaml template mbt-ldax cxl-local "$MBT_LOCAL_SIZE" dax;;
        lsteer) dra-claim-yaml template mbt-lsteer cxl-local "$MBT_LOCAL_SIZE" "container=steering: {policy: auto}";;
        lease) dra-lease-template-yaml mbt-lease;;
        *) return 2;;
    esac
}

mbt-templates-apply() {
    # Usage: mbt-templates-apply
    #
    # Create the ResourceClaimTemplates of every kind and pool in $DRA_NS
    # (26 of them, mbt-xnuma-a ... mbt-lease, the kinds of persistent
    # devices for the pools that have them; plse uses mbt-lease). Fail the
    # test if kubectl fails.
    local yaml="" kind pool base
    for pool in "${MBT_POOL_IDS[@]}"; do
        for base in xnuma xdax sdax sro sexcl; do
            yaml+="${yaml:+
---
}$(mbt-template-yaml "$base-$pool")"
        done
        [ -n "${MBT_PMEM_SERIAL[$pool]:-}" ] || continue
        for base in pwr pfsd pfsc pbro pbad xpb xpf; do
            yaml+="
---
$(mbt-template-yaml "$base-$pool")"
        done
    done
    for kind in lnuma ldax lsteer lease; do
        yaml+="
---
$(mbt-template-yaml "$kind")"
    done
    dra-apply "$OUTPUT_DIR" <<< "$yaml"
}

# shellcheck disable=SC2001 # sed indents every line of the workload
mbt-pod-yaml() {
    # Usage: mbt-pod-yaml SLOT NAME
    #
    # Print pod NAME of SLOT (contract 2): label mbt.slot, nodeSelector of
    # the node of its VM unless the slot is "any", restartPolicy Always,
    # terminationGracePeriodSeconds 2, its claim c0 from the template of
    # its kind (none for plain), image $DRA_TOOLS_IMAGE with the tools of
    # dra-tools-install in /tools, main process $MBT_WORKLOAD. dnuma
    # (A39): two claims that its container uses, c0 from mbt-lnuma (a
    # local numa slice) and c1 from mbt-xnuma-c (c-pooled0 in numa).
    local slot="$1" name="$2" selector="" claims="" uses="" template
    mbt-slot "$slot" || return 2
    [ "$MBT_SVM" == any ] || selector="
  nodeSelector:
    kubernetes.io/hostname: ${MBT_NODE[$MBT_SVM]}"
    template="mbt-$MBT_KIND"
    [ "$MBT_KIND" != plse ] || template=mbt-lease   # A22: persistent leases, the lease claim of lease
    if [ "$MBT_KIND" == dnuma ]; then
        claims="
  resourceClaims:
  - name: c0
    resourceClaimTemplateName: mbt-lnuma
  - name: c1
    resourceClaimTemplateName: mbt-xnuma-c"
        uses="
    resources:
      claims:
      - name: c0
      - name: c1"
    elif [ "$MBT_KIND" != plain ]; then
        claims="
  resourceClaims:
  - name: c0
    resourceClaimTemplateName: $template"
        uses="
    resources:
      claims:
      - name: c0"
    fi
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  labels:
    $MBT_SLOT_LABEL: $MBT_SLABEL
spec:$selector
  terminationGracePeriodSeconds: 2
  restartPolicy: Always$claims
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
$(sed 's/^/      /' <<< "$MBT_WORKLOAD")
    volumeMounts:
    - name: tools
      mountPath: /tools$uses
EOF
}

###
### Commands
###

mbt-sh() {
    # Usage: mbt-sh VM COMMAND
    #
    # Run COMMAND as root in the VM of the VM id VM, quietly. MBT_OUT has
    # its stdout, MBT_ERR its stderr; both go to the executor log with the
    # command. Return its exit status.
    local vm="$1" cmd="$2" rc
    MBT_OUT=$(vmdir-command-q "${MBT_VMDIR[$vm]}" "$cmd" 2>"$MBT_TMP/stderr")
    rc=$?
    MBT_ERR=$(<"$MBT_TMP/stderr")
    {
        echo "[$vm] \$ $cmd"
        [ -z "$MBT_OUT" ] || echo "$MBT_OUT"
        [ -z "$MBT_ERR" ] || echo "(stderr) $MBT_ERR"
        echo "(exit status $rc)"
    } >> "$MBT_EXEC_LOG"
    return "$rc"
}

mbt-exec-in() {
    # Usage: mbt-exec-in SLOT COMMAND [ARGS...]
    #
    # Run COMMAND with ARGS in container main of the pod of SLOT (kubectl
    # exec in the VM of the test, $MBT_EXEC_TIMEOUT s at most), see mbt-sh.
    # Return 2 if SLOT has no pod, else the exit status of kubectl exec.
    # A kubelet that has just restarted reports its node Ready before it
    # knows its pods again, and kubectl exec fails at the connection
    # ("unable to upgrade connection: pod does not exist"): such errors of
    # kubectl, not of COMMAND, are retried for MBT_EXEC_RETRY s.
    local pod rc deadline=$(( SECONDS + MBT_EXEC_RETRY ))
    mbt-slot-has-pod "$1" || return 2
    pod=$(mbt-slot-pod "$1") || return 2
    shift
    while :; do
        mbt-sh vm2 "timeout $MBT_EXEC_TIMEOUT kubectl exec -n $DRA_NS $pod -c main --$(printf ' %q' "$@")"
        rc=$?
        (( rc != 0 && SECONDS < deadline )) || return "$rc"
        grep -qE '^error: unable to upgrade connection|^Error from server: error dialing backend' <<< "$MBT_ERR" || return "$rc"
        sleep 2
    done
}

mbt-json() {
    # Usage: mbt-json KEY VALUE [KEY VALUE...]
    #
    # Print a JSON object of the string VALUEs; the VALUE @null is null,
    # @json:TEXT is the JSON TEXT, @200:TEXT is the first 200 bytes of TEXT.
    python3 -c '
import json, sys
a, out = sys.argv[1:], {}
for k, v in zip(a[0::2], a[1::2]):
    if v == "@null":
        out[k] = None
    elif v.startswith("@json:"):
        out[k] = json.loads(v[6:]) if v[6:].strip() else None
    elif v.startswith("@200:"):
        out[k] = v[5:].encode()[:200].decode(errors="replace")
    else:
        out[k] = v
print(json.dumps(out, sort_keys=True, separators=(",", ":")))' "$@"
}

mbt-utc() {
    # Usage: mbt-utc [SECONDS-AGO]
    #
    # Print the time SECONDS-AGO (default 0) seconds ago in UTC, RFC 3339.
    date -u -d "-${1:-0} seconds" +%Y-%m-%dT%H:%M:%SZ
}

###
### Actions (contract 4)
###

mbt-act-create() {
    # Usage: mbt-act-create SLOT
    #
    # Apply the pod of the next generation of SLOT, return at once.
    local slot="$1" gen name
    mbt-slot "$slot" || return 2
    gen=$(( ${MBT_GEN[$slot]:-0} + 1 ))
    MBT_GEN[$slot]=$gen
    name="$MBT_SPOD-g$gen"
    MBT_NOTE="pod $name"
    mbt-sh vm2 "kubectl apply -n $DRA_NS -f - <<'MBT_EOF'
$(mbt-pod-yaml "$slot" "$name")
MBT_EOF"
}

mbt-act-delete() {
    # Usage: mbt-act-delete SLOT
    #
    # Delete the pod of SLOT gracefully without waiting.
    local pod
    mbt-slot-has-pod "$1" || return 2
    pod=$(mbt-slot-pod "$1") || return 2
    MBT_NOTE="pod $pod"
    mbt-sh vm2 "kubectl delete pod -n $DRA_NS $pod --wait=false"
}

mbt-act-crash() {
    # Usage: mbt-act-crash SLOT
    #
    # Kill the workload of the pod of SLOT with SIGSEGV: the container
    # exits with 139 and restarts.
    # shellcheck disable=SC2016 # expanded by sh in the container
    mbt-exec-in "$1" sh -c 'kill -SEGV "$(cat /tmp/workload.pid)"'
}

mbt-act-write() {
    # Usage: mbt-act-write SLOT STEP
    #
    # Write "mbt-STEP" and a NUL byte at offset 0 of the dax device of the
    # claim of the pod of SLOT (dax-rw.py write).
    local key rc
    mbt-slot "$1" || return 2
    key=$(mbt-dax-key) || return 2
    # shellcheck disable=SC2016 # expanded by sh in the container
    mbt-exec-in "$1" sh -c 'exec python3 /tools/dax-rw.py write "$CXL_'"$key"'_DAX" 0 "$1"' sh "mbt-$2"
    rc=$?
    MBT_NOTE="mbt-$2"
    MBT_RESULT=$(mbt-json text @null stderr "@200:$MBT_ERR")
    return "$rc"
}

mbt-act-read() {
    # Usage: mbt-act-read SLOT
    #
    # Read the string at offset 0 of the dax device of the claim of the pod
    # of SLOT (dax-rw.py read).
    local key rc text=@null
    mbt-slot "$1" || return 2
    key=$(mbt-dax-key) || return 2
    # shellcheck disable=SC2016 # expanded by sh in the container
    mbt-exec-in "$1" sh -c 'exec python3 /tools/dax-rw.py read "$CXL_'"$key"'_DAX" 0'
    rc=$?
    if [ "$rc" == 0 ]; then
        text="$MBT_OUT"
        MBT_NOTE="read '$text'"
    fi
    MBT_RESULT=$(mbt-json text "$text" stderr "@200:$MBT_ERR")
    return "$rc"
}

mbt-forget-inspected() {
    # Usage: mbt-forget-inspected SLOT
    #
    # Inspect the pod of SLOT again at the next settle observation, for
    # instance after its leases changed (the inspect table has
    # UID:RESTARTS:SLOT entries).
    awk -F: -v slot="$1" '$3 != slot' "$MBT_TMP/inspected" > "$MBT_TMP/inspected.forget" &&
        mv "$MBT_TMP/inspected.forget" "$MBT_TMP/inspected"
}

mbt-act-lease-get() {
    # Usage: mbt-act-lease-get SLOT MODE
    #
    # The lease kind (MODE numa|dax): lease 128Mi of local memory in MODE
    # for the container of the pod of SLOT (cxl-request request ... --wait
    # 60s). plse (MODE pbw|pbr|pfd|pfc, amendment A23): lease c-pmem0, the
    # whole device for a block writer (pbw), a read-only share of its block
    # device (pbr), its erofs read-only, direct at
    # /run/cxl/fs/$MBT_FS_LEASE_NAME (pfd) or cached at /run/cxl/fs/<lease
    # id> (pfc). The result has the lease id (null if none). The next
    # settle observation inspects the pod again: its leases changed.
    local slot="$1" mode="$2" rc json args=() serial=${MBT_PMEM_SERIAL[c]}
    mbt-slot "$slot" || return 2
    case "$MBT_BASE:$mode" in
        lease:numa|lease:dax) args=(--source local --mode "$mode" --size "$MBT_LOCAL_SIZE");;
        plse:pbw) args=(--serial "$serial" --mode block);;
        plse:pbr) args=(--serial "$serial" --share --read-only --mode block);;
        plse:pfd) args=(--serial "$serial" --share --read-only --mode fs --name "$MBT_FS_LEASE_NAME");;
        plse:pfc) args=(--serial "$serial" --share --read-only --mode fs --access cached);;
        lease:*|plse:*) MBT_ERR="lease mode '$mode' is not one of $MBT_BASE (lease: numa, dax; plse: pbw, pbr, pfd, pfc)"; return 2;;
        *) MBT_ERR="$MBT_KIND is not a lease kind (lease, plse)"; return 2;;
    esac
    mbt-exec-in "$slot" "$DRA_REQUEST_CTR_BIN" request "${args[@]}" --wait "$MBT_LEASE_WAIT"
    rc=$?
    mbt-forget-inspected "$slot"
    json=$(dra-last-json "$MBT_OUT") || json=""
    MBT_RESULT=$(python3 -c '
import json, sys
r = json.loads(sys.argv[1]) if sys.argv[1] else None
print(json.dumps({"lease": (r or {}).get("lease"),
                  "stderr": sys.argv[2].encode()[:200].decode(errors="replace")}, sort_keys=True, separators=(",", ":")))' "$json" "$MBT_ERR")
    MBT_NOTE="lease $(python3 -c 'import json, sys; print((json.loads(sys.argv[1]) or {}).get("lease"))' "$MBT_RESULT")"
    return "$rc"
}

mbt-act-lease-put() {
    # Usage: mbt-act-lease-put SLOT
    #
    # Release every lease of the pod of the lease kind SLOT (cxl-request
    # release --all). The next settle observation inspects the pod again.
    # plse: with --wait $MBT_LEASE_PUT_WAIT, so that an fs lease whose file
    # a process maps (hold), which stays Releasing, ends the step with exit
    # 3 after the wait instead of the driver's 120 s.
    local rc wait=()
    mbt-slot "$1" || return 2
    case "$MBT_BASE" in
        lease) ;;
        plse) wait=(--wait "$MBT_LEASE_PUT_WAIT");;
        *) MBT_ERR="$MBT_KIND is not a lease kind (lease, plse)"; return 2;;
    esac
    mbt-exec-in "$1" "$DRA_REQUEST_CTR_BIN" release --all "${wait[@]}"
    rc=$?
    mbt-forget-inspected "$1"
    [ "$MBT_BASE" != plse ] || MBT_RESULT=$(mbt-json stderr "@200:$MBT_ERR")
    return "$rc"
}

###
### Actions on persistent devices (amendment A23)
###

mbt-pexec() {
    # Usage: mbt-pexec SLOT SCRIPT [ARGS...]
    #
    # Run the sh SCRIPT with ARGS in container main of the pod of SLOT
    # (mbt-exec-in, after mbt-slot SLOT), with KEY (the env key of its
    # persistent device, "" for plse) and TOOL ($MBT_PMEM_TOOL) set.
    local slot="$1" script="$2" key=""
    shift 2
    if [ "$MBT_BASE" != plse ]; then
        key=$(mbt-pmem-key) || return 2
    fi
    mbt-exec-in "$slot" sh -c "KEY=$key TOOL=$MBT_PMEM_TOOL
$script" sh "$@"
}

mbt-act-pwrite() {
    # Usage: mbt-act-pwrite SLOT img|junk STEP
    #
    # img: write the erofs template whose hello.txt and bytes 0-63 hold
    # the token mbt-STEP at offset 0 of the device of the pod of SLOT, then
    # fsync (pmem-rw.py image); junk: 8192 zero bytes (pmem-rw.py junk).
    # Block kinds: the device node CXL_<KEY>_PATH, opened read-write (a
    # read-only claim fails: EPERM); fs kinds: hello.txt under the mount
    # (fails: EROFS); plse: its newest Ready lease, a block lease through
    # its fd (cxl-request env --exec; a read-only fd fails: EBADF), an fs
    # lease under its path (fails). The result has the token written
    # ("" for junk; null on failure); the status is that of the command in
    # the container.
    local slot="$1" what="$2" token="mbt-$3" rc text=@null op=image
    mbt-slot "$slot" || return 2
    case "$what" in
        img) ;;
        junk) token="" op=junk;;
        *) MBT_ERR="pwrite takes img or junk, not '$what'"; return 2;;
    esac
    # shellcheck disable=SC2016 # expanded by sh in the container
    case "$(mbt-pclass)" in
        block|block-ro) mbt-pexec "$slot" 'eval "dev=\$CXL_${KEY}_PATH"
if [ "$1" = image ]; then exec python3 "$TOOL" image "$dev" "$2"; fi
exec python3 "$TOOL" junk "$dev"' "$op" "$token";;
        fs) mbt-pexec "$slot" 'eval "dir=\$CXL_${KEY}_PATH"; exec python3 "$TOOL" write-file "$dir" "$1"' "$token";;
        plse) mbt-pexec "$slot" 'exec python3 "$TOOL" lease any "$@"' "$op" "$token";;
        *) MBT_ERR="$MBT_KIND has no persistent device"; return 2;;
    esac
    rc=$?
    [ "$rc" != 0 ] || text=$token
    MBT_NOTE="${token:-zeros}"
    MBT_RESULT=$(mbt-json text "$text" stderr "@200:$MBT_ERR")
    return "$rc"
}

mbt-act-pread() {
    # Usage: mbt-act-pread SLOT
    #
    # Read the device of the pod of SLOT: block kinds the string in bytes
    # 0-63 of CXL_<KEY>_PATH, fs kinds hello.txt under CXL_<KEY>_PATH,
    # plse its newest Ready lease (block: through its fd; fs: under its
    # path). The result has the text (null on failure).
    local slot="$1" rc text=@null
    mbt-slot "$slot" || return 2
    # shellcheck disable=SC2016 # expanded by sh in the container
    case "$(mbt-pclass)" in
        block|block-ro) mbt-pexec "$slot" 'eval "dev=\$CXL_${KEY}_PATH"; exec python3 "$TOOL" read "$dev"';;
        fs) mbt-pexec "$slot" 'eval "dir=\$CXL_${KEY}_PATH"; exec python3 "$TOOL" read-file "$dir"';;
        plse) mbt-pexec "$slot" 'exec python3 "$TOOL" lease any read';;
        *) MBT_ERR="$MBT_KIND has no persistent device"; return 2;;
    esac
    rc=$?
    if [ "$rc" == 0 ]; then
        text="$MBT_OUT"
        MBT_NOTE="read '$text'"
    fi
    MBT_RESULT=$(mbt-json text "$text" stderr "@200:$MBT_ERR")
    return "$rc"
}

# The end of the hold script: wait until the holder started in the
# background ($! ) holds ($1 the stop file, $2 its log). The holder is an
# external command started with its redirections: a builtin (eval) in a
# background job would keep saved copies of the exec's stdout and stderr
# open in that job, and kubectl exec would not end until the holder does.
# shellcheck disable=SC2016 # expanded by sh in the container
MBT_HOLD_WAIT='i=0
while [ $i -lt 50 ]; do
    grep -q "^holding " "$2" && { cat "$2"; exit 0; }
    kill -0 $! 2>/dev/null || break
    sleep 0.1
    i=$((i + 1))
done
cat "$2" >&2
exit 1'

mbt-act-hold() {
    # Usage: mbt-act-hold SLOT
    #
    # Start a process in the container of the pod of SLOT, an fs kind or
    # plse with an fs lease, that maps hello.txt of its erofs read-only and
    # keeps it open until unhold (pmem-rw.py hold, in a session of its own:
    # it outlives the kubectl exec; a crash or delete of the container ends
    # it). Return when it says "holding" (5 s at most).
    local slot="$1" hold rc
    mbt-slot "$slot" || return 2
    # shellcheck disable=SC2016 # expanded by sh in the container
    case "$(mbt-pclass)" in
        fs) hold='eval "dir=\$CXL_${KEY}_PATH"
setsid python3 "$TOOL" hold "$dir" "$1" > "$2" 2>&1 < /dev/null &';;
        plse) hold='setsid python3 "$TOOL" lease fs hold "$1" > "$2" 2>&1 < /dev/null &';;
        *) MBT_ERR="hold is for the fs kinds and plse, not $MBT_KIND"; return 2;;
    esac
    mbt-pexec "$slot" "$hold
$MBT_HOLD_WAIT" "$MBT_HOLD_STOP" "$MBT_HOLD_LOG"
    rc=$?
    MBT_NOTE=$(head -c 200 <<< "$MBT_OUT")
    return "$rc"
}

mbt-act-unhold() {
    # Usage: mbt-act-unhold SLOT
    #
    # End the holders of hold in the container of the pod of SLOT: create
    # their stop file and wait until none runs (5 s at most).
    local slot="$1"
    mbt-slot "$slot" || return 2
    case "$(mbt-pclass)" in
        fs|plse) ;;
        *) MBT_ERR="unhold is for the fs kinds and plse, not $MBT_KIND"; return 2;;
    esac
    # shellcheck disable=SC2016 # expanded by sh in the container
    mbt-pexec "$slot" 'touch "$1"
i=0
while [ $i -lt 50 ]; do
    pgrep -f "$3" > /dev/null || { cat "$2" 2>/dev/null; exit 0; }
    sleep 0.1
    i=$((i + 1))
done
echo "a holder still runs: $(pgrep -af "$3")" >&2
exit 1' "$MBT_HOLD_STOP" "$MBT_HOLD_LOG" "$MBT_HOLDER_RE"
}

mbt-act-pwipe() {
    # Usage: mbt-act-pwipe POOL DEV
    #
    # Make the persistent device DEV of pool POOL fresh again on the host:
    # zeros over its LSA file and the first 4 MiB of its data file (labels,
    # pfn info, the erofs). Refused (status 2) while the server of POOL
    # lists an attachment of DEV (in any state) or does not answer.
    local pool="$1" dev="$2" url json files
    mbt-pool-arg "$pool" || return 2
    [ "${MBT_PDEV_POOL[$dev]:-}" == "$pool" ] || { MBT_ERR="$dev is not a persistent device of pool $pool"; return 2; }
    files=${MBT_PDEV_FILES[$dev]:-}
    [ -n "$files" ] || { MBT_ERR="the backing files of $dev are not known (mbt-pdev-files)"; return 2; }
    url=$(pool-url "${MBT_POOL_SERVER[$pool]}") || { MBT_ERR="no server of pool $pool"; return 2; }
    json=$(curl -sf --max-time 10 "$url/api/v1/attachments") || { MBT_ERR="the server of pool $pool does not answer"; return 2; }
    MBT_OUT=$(python3 -c '
import json, sys
print(" ".join("%s@%s:%s" % (a.get("device"), a.get("host"), a.get("state")) for a in json.loads(sys.argv[1]) or [] if a.get("device") == sys.argv[2]))' "$json" "$dev")
    [ -z "$MBT_OUT" ] || { MBT_ERR="$dev is attached: $MBT_OUT"; return 2; }
    python3 - "${files%%:*}" "${files#*:}" <<'EOF' || { MBT_ERR="cannot wipe $files"; return 1; }
import sys
data, lsa = sys.argv[1:3]
for path, n in ((lsa, None), (data, 4 << 20)):
    with open(path, "r+b") as f:
        size = f.seek(0, 2)
        f.seek(0)
        f.write(bytes(size if n is None else min(n, size)))
EOF
    echo "[host] pwipe $dev: $files" >> "$MBT_EXEC_LOG"
    MBT_NOTE="wiped $files"
}

mbt-act-driver-kill-at() {
    # Usage: mbt-act-driver-kill-at VM MARKER
    #
    # Arm a one-shot watcher in VM (mbt-observe.py kill-at, detached) that
    # SIGKILLs the driver process at MARKER of the next persistent Prepare
    # there: region (a new CXL region of mode pmem), ns (a namespace with a
    # size in its nvdimm region), mount (a new node mount), bind (a new
    # lease bind); it gives up after MBT_KILLAT_TIMEOUT s. It is bound to
    # the driver process it was armed against: if that one ends first, it
    # ends, not fired (reason "driver restarted"); driver-kill,
    # driver-restart and containerd-restart of VM disarm it first
    # (mbt-killat-disarm). Return once it is armed. An armed watcher of
    # an earlier step is stopped first. The settle observations of the VM
    # have its state (killAt).
    local vm="$1" marker="$2" dir=${MBT_KILLAT_STATE%/*}
    mbt-vm-arg "$vm" || return 2
    case "$marker" in
        region|ns|mount|bind) ;;
        *) MBT_ERR="driver-kill-at marker '$marker' is not region, ns, mount or bind"; return 2;;
    esac
    mbt-killat-stop
    MBT_TRACE_REFOLLOW[$vm]=$(mbt-utc 2)
    mbt-sh "$vm" "mkdir -p $dir && rm -f $MBT_KILLAT_STATE
setsid -f python3 $MBT_OBSERVER_VM kill-at --marker $marker --timeout $MBT_KILLAT_TIMEOUT > $dir/watcher.log 2>&1 < /dev/null
for i in \$(seq 100); do [ -s $MBT_KILLAT_STATE ] && break; sleep 0.05; done
cat $MBT_KILLAT_STATE" || return 1
    if ! dra-py assert "$MBT_OUT" "j.get('armed') is True" 2>/dev/null; then
        MBT_ERR="the watcher is not armed: $MBT_OUT"
        return 1
    fi
    MBT_KILLAT_VM=$vm
    MBT_NOTE="armed, driver pid $(dra-py eval "$MBT_OUT" "j.get('pid')")"
}

mbt-killat-disarm() {
    # Usage: mbt-killat-disarm VM VERB
    #
    # Before VERB restarts the driver of VM (driver-kill, driver-restart,
    # containerd-restart, fencing-switch, node-reboot): disarm the driver-kill-at watcher armed there,
    # if any (mbt-observe.py kill-at-disarm): it ends done, not fired,
    # reason "disarmed by VERB", unless it fired or ended before. Never
    # fails.
    [ -n "$MBT_KILLAT_VM" ] && [ "$MBT_KILLAT_VM" == "$1" ] || return 0
    mbt-sh "$1" "python3 $MBT_OBSERVER_VM kill-at-disarm --verb $2" || true
}

mbt-killat-stop() {
    # Usage: mbt-killat-stop
    #
    # Stop an armed driver-kill-at watcher, if any (a later arming, the end
    # of the run). Never fails.
    local vm=$MBT_KILLAT_VM
    [ -n "$vm" ] || return 0
    MBT_KILLAT_VM=""
    vmdir-command-q "${MBT_VMDIR[$vm]}" "pkill -f '$MBT_OBSERVER_VM kill-at'; true" > /dev/null 2>&1
    return 0
}

mbt-pdev-files() {
    # Usage: mbt-pdev-files DEV...
    #
    # Set MBT_PDEV_FILES of the persistent devices DEV from their server
    # (pool-device-files), and fail the test unless the LSA file is 256 KiB
    # of zeros: a fresh device.
    local dev pool
    for dev in "$@"; do
        pool=${MBT_PDEV_POOL[$dev]}
        pool-device-files --pool "${MBT_POOL_SERVER[$pool]}" "$dev"
        pool-assert "$COMMAND_OUTPUT" "j['persistent'] == True"
        [ "$POOL_DEVICE_DATA" == "$MBT_PDEV_DIR/$dev.raw" ] && [ "$POOL_DEVICE_LSA" == "$MBT_PDEV_DIR/$dev.lsa" ] ||
            error "the backing files of $dev are $POOL_DEVICE_DATA and $POOL_DEVICE_LSA, expected $MBT_PDEV_DIR/$dev.raw and .lsa"
        MBT_PDEV_FILES[$dev]="$POOL_DEVICE_DATA:$POOL_DEVICE_LSA"
        python3 -c 'import sys; b = open(sys.argv[1], "rb").read(); sys.exit(0 if b.count(0) == len(b) == 262144 else 1)' "$POOL_DEVICE_LSA" ||
            error "the LSA file $POOL_DEVICE_LSA of $dev is not 256 KiB of zeros"
    done
}

mbt-pdev-files-remove() {
    # Usage: mbt-pdev-files-remove DIR DEV...
    #
    # Remove the data and LSA files (DEV.raw, DEV.lsa) of the persistent
    # devices DEV in the pool directory DIR of a server, a
    # /tmp/fake-cxl-pool-NAME (host RAM).
    local dir="$1" dev
    shift
    [[ "$dir" =~ ^/tmp/fake-cxl-pool-[a-z0-9-]+$ ]] || { echo "mbt-pdev-files-remove: not a pool directory: '$dir'"; return 1; }
    for dev in "$@"; do
        [[ "$dev" =~ ^[a-z0-9-]+$ ]] || continue
        rm -f -- "${dir:?}/${dev:?}.raw" "${dir:?}/${dev:?}.lsa"
    done
}

mbt-pmem-held() {
    # Usage: mbt-pmem-held VMDIR
    #
    # Print what in the VM of VMDIR still holds a persistent device, one per
    # line, quietly: node mounts and lease binds ("mount TARGET") and loop
    # devices over /dev/pmem* ("loop loopN /dev/pmemK"); nothing if free.
    # Return non-zero if the VM cannot be read.
    vmdir-command-q "$1" "awk '\$5 ~ \"^($DRA_POOL_MOUNT_DIR|$DRA_LEASE_FS_DIR)/\" {print \"mount \" \$5}' /proc/self/mountinfo | sort -u
for f in /sys/block/loop*/loop/backing_file; do [ -f \"\$f\" ] || continue; b=\$(cat \"\$f\"); case \"\$b\" in /dev/pmem*) n=\${f#/sys/block/}; echo \"loop \${n%%/*} \$b\";; esac; done
true"
}

mbt-pmem-unmount() {
    # Usage: mbt-pmem-unmount VMDIR
    #
    # The error path, after the driver is gone: unmount the lease binds,
    # then the node mounts that are left in the VM of VMDIR, plainly (a
    # busy one stays: never MNT_DETACH; a cached mount's autoclear loop
    # device goes with it).
    vmdir-command "$1" "for t in \$(awk '\$5 ~ \"^$DRA_LEASE_FS_DIR/\" {print \$5}' /proc/self/mountinfo | sort -ur) \$(awk '\$5 ~ \"^$DRA_POOL_MOUNT_DIR/\" {print \$5}' /proc/self/mountinfo | sort -ur); do
    umount \"\$t\" && echo \"unmounted \$t\" || echo \"cannot unmount \$t\"
done; true"
}

mbt-act-driver-kill() {
    # Usage: mbt-act-driver-kill VM
    #
    # SIGKILL to the driver of VM in place (mbt-driver-kill), return at
    # once: kubelet restarts its container.
    mbt-vm-arg "$1" || return 2
    mbt-killat-disarm "$1" driver-kill
    MBT_TRACE_REFOLLOW[$1]=$(mbt-utc 2)
    mbt-driver-kill "${MBT_VMDIR[$1]}" || return 1
    MBT_NOTE="$COMMAND_OUTPUT"
}

mbt-driver-ready() {
    # Usage: mbt-driver-ready VM
    #
    # Return 0 if the driver of VM is ready (amendment A15): a container of
    # it runs on the node, the log of that container (its log file, and
    # its rotated files) has "$MBT_LENDS_RESTORED" (NRI and lending are up,
    # the last step of the driver's start in every fencing mode), both
    # kubelet plugin sockets
    # (cxl.nri.io and cxl-pool.nri.io dra.sock) exist and the node has its
    # cxl.nri.io ResourceSlice. One ssh. MBT_ERR says what is missing.
    local vmdir=${MBT_VMDIR[$1]} name=${MBT_NODE[$1]}
    MBT_ERR=$(vmdir-command-q "$vmdir" "id=\$(crictl ps -q --state running --namespace '^kube-system\$' --name '^$DRA_CONTAINER\$' 2>/dev/null | head -n 1)
[ -n \"\$id\" ] || { echo 'no running driver container'; exit 1; }
log=\$(crictl inspect -o go-template --template '{{.status.logPath}}' \$id 2>/dev/null)
[ -n \"\$log\" ] || { echo \"no log path of \$id\"; exit 1; }
has() { grep -qF \"\$1\" \"\$log\" 2>/dev/null || zgrep -qF \"\$1\" \"\$log\".* 2>/dev/null; }
has '$MBT_LENDS_RESTORED' ||
    { echo \"no '$MBT_LENDS_RESTORED' in the log of \${id:0:13} yet\"; exit 1; }
for d in $DRA_LOCAL_DRIVER $DRA_POOL_DRIVER; do
    test -S /var/lib/kubelet/plugins/\$d/dra.sock || { echo \"no /var/lib/kubelet/plugins/\$d/dra.sock\"; exit 1; }
done
kubectl get resourceslices -o name --field-selector spec.driver=$DRA_LOCAL_DRIVER,spec.nodeName=$name | grep -q . ||
    { echo 'no $DRA_LOCAL_DRIVER ResourceSlice of $name'; exit 1; }" 2>&1)
}

mbt-driver-ready-wait() {
    # Usage: mbt-driver-ready-wait VM
    #
    # Wait until the driver of VM is ready (mbt-driver-ready),
    # MBT_DRIVER_READY_WAIT s at most. Return non-zero on timeout, with
    # MBT_ERR saying what was missing.
    local vm="$1" why="" t0=$EPOCHREALTIME
    retry-until --timeout "$MBT_DRIVER_READY_WAIT" --interval 1 --message "$vm: the driver is ready (running, '$MBT_LENDS_RESTORED' in its log, dra.sock, its ResourceSlice)" \
        'mbt-driver-ready "$vm" || { why=$MBT_ERR; echo "$(date +%T.%N) $vm: driver not ready: $why" >> "$MBT_EXEC_LOG"; false; }' && {
        echo "$(date +%T.%N) $vm: driver ready after $(printf '%.2f' "$(echo "$EPOCHREALTIME - $t0" | bc)") s" >> "$MBT_EXEC_LOG"
        return 0
    }
    MBT_ERR="the driver of $vm is not ready after ${MBT_DRIVER_READY_WAIT}s: $why"
    return 1
}

mbt-act-driver-restart() {
    # Usage: mbt-act-driver-restart VM
    #
    # Delete the pod of the driver of VM and wait until a new pod of the
    # DaemonSet runs there and is ready (mbt-driver-replace).
    mbt-vm-arg "$1" || return 2
    mbt-driver-replace "$1" driver-restart
}

mbt-driver-replace() {
    # Usage: mbt-driver-replace VM VERB
    #
    # The driver restart of VERB (driver-restart, fencing-switch): disarm a
    # driver-kill-at watcher of VM (mbt-killat-disarm VM VERB), delete the
    # pod of the driver of VM and wait until a new pod of the DaemonSet
    # runs there and is ready (mbt-driver-ready: NRI and lending up, the
    # plugin sockets, the node's cxl.nri.io slice; the pool slices are left
    # out: a restarted controller may not have published them yet). Like
    # dra-driver-restart, but it returns non-zero instead of failing the
    # test. The log of the old pod is
    # $TEST_OUTPUT_DIR/nri-dra-driver-cxl.<vm>.<pod>.log; a running trace
    # follows the new pod. MBT_NOTE is "OLD -> NEW", MBT_DRIVER_NEW the new
    # pod.
    local vm="$1" verb="$2" vmdir label name old new=""
    MBT_DRIVER_NEW=""
    mbt-killat-disarm "$vm" "$verb"
    vmdir=${MBT_VMDIR[$vm]}
    label=$(vmdir-label "$vmdir")
    name=${MBT_NODE[$vm]}
    old=$(dra-driver-pod "$vmdir") || { MBT_ERR="no running pod of $DRA_DAEMONSET on $name"; return 1; }
    vmdir-command-q "$vmdir" "kubectl logs -n kube-system $old -c $DRA_CONTAINER" > "$TEST_OUTPUT_DIR/$DRA_CONTAINER.$label.$old.log" 2>&1
    mbt-sh vm2 "kubectl delete pod -n kube-system $old --wait=true --timeout=60s" || return 1
    retry-until --timeout 120 --interval 1 --message "$label: a new Ready pod of $DRA_DAEMONSET" \
        'new=$(dra-driver-pod "$vmdir") && [ "$new" != "$old" ] && vmdir-command-q "$vmdir" "kubectl wait -n kube-system --for=condition=Ready pod/$new --timeout=1s" >/dev/null 2>&1' ||
        { MBT_ERR="no new Ready pod of $DRA_DAEMONSET on $name"; return 1; }
    mbt-driver-ready-wait "$vm" || return 1
    vmdir-command-q "$vmdir" "test -x $DRA_REQUEST_NODE_BIN" || { MBT_ERR="no $DRA_REQUEST_NODE_BIN on $name"; return 1; }
    MBT_NOTE="$old -> $new"
    MBT_DRIVER_NEW=$new
    MBT_DRIVER_SEEN[$vm]="$new 0"
    unset "MBT_TRACE_REFOLLOW[$vm]"
    [ -z "$DRA_TRACE_RUNNING" ] || dra-trace-driver-follow "$vmdir"
}

mbt-act-ctrl-kill() {
    # Usage: mbt-act-ctrl-kill POOL
    mbt-pool-arg "$1" || return 2
    mbt-controller-kill "${MBT_POOL_UNIT[$1]}"
}

mbt-act-ctrl-start() {
    # Usage: mbt-act-ctrl-start POOL
    mbt-pool-arg "$1" || return 2
    mbt-controller-start "${MBT_POOL_UNIT[$1]}"
}

mbt-act-ctrl-restart() {
    # Usage: mbt-act-ctrl-restart POOL
    mbt-pool-arg "$1" || return 2
    mbt-controller-stop "${MBT_POOL_UNIT[$1]}" && mbt-controller-start "${MBT_POOL_UNIT[$1]}"
}

mbt-act-server-restart() {
    # Usage: mbt-act-server-restart POOL
    mbt-pool-arg "$1" || return 2
    pool-server-restart "${MBT_POOL_SERVER[$1]}"
}

mbt-act-kubelet-restart() {
    # Usage: mbt-act-kubelet-restart VM
    #
    # Restart the kubelet of VM (mbt-node-service-restart), then wait until
    # its driver is ready (mbt-driver-ready-wait, A15). The stream of the
    # trace that follows the driver log through the kubelet ends: follow
    # it again after the next observation.
    mbt-vm-arg "$1" || return 2
    local t0=$EPOCHREALTIME
    MBT_TRACE_REFOLLOW[$1]=$(mbt-utc 2)
    mbt-node-service-restart "${MBT_VMDIR[$1]}" kubelet || return 1
    echo "$(date +%T.%N) $1: kubelet back after $(printf '%.2f' "$(echo "$EPOCHREALTIME - $t0" | bc)") s" >> "$MBT_EXEC_LOG"
    mbt-driver-ready-wait "$1"
}

mbt-driver-restarts() {
    # Usage: mbt-driver-restarts VM
    #
    # Print "POD RESTARTS READY" of the driver pod on the node of VM
    # (READY: True or False), quietly. Return non-zero if there is none.
    local out
    out=$(vmdir-command-q "$OUTPUT_DIR" "kubectl get pods -n kube-system -l $DRA_POD_LABEL --field-selector spec.nodeName=${MBT_NODE[$1]} -o jsonpath='{range .items[*]}{.metadata.name} {.status.containerStatuses[?(@.name==\"$DRA_CONTAINER\")].restartCount} {.status.conditions[?(@.type==\"Ready\")].status}{\"\\n\"}{end}'" 2>/dev/null | head -n 1)
    [ -n "$out" ] || return 1
    echo "$out"
}

mbt-act-containerd-restart() {
    # Usage: mbt-act-containerd-restart VM
    #
    # Restart containerd in VM (mbt-node-service-restart), then wait until
    # the driver of VM has restarted in place and is Ready again,
    # MBT_NRI_RESTART_WAIT s at most: the driver exits when it loses its
    # NRI connection (amendment A8). Then wait until the driver is ready
    # (mbt-driver-ready-wait, A15). Return non-zero if it does not
    # restart or is not ready, MBT_ERR says which.
    local vm="$1" before after pod restarts ready rc
    mbt-vm-arg "$vm" || return 2
    mbt-killat-disarm "$vm" containerd-restart
    MBT_TRACE_REFOLLOW[$vm]=$(mbt-utc 2)
    before=$(mbt-driver-restarts "$vm") || before=""
    mbt-node-service-restart "${MBT_VMDIR[$vm]}" containerd
    rc=$?
    [ "$rc" == 0 ] && [ -n "$before" ] || return "$rc"
    read -r pod restarts ready <<< "$before"
    if retry-until --timeout "$MBT_NRI_RESTART_WAIT" --interval 1 --message "$vm: the driver $pod restarted in place (more than $restarts restarts) and Ready" \
           'after=$(mbt-driver-restarts "$vm") && read -r _ r a <<< "$after" && [ "${r:-0}" -gt "$restarts" ] && [ "$a" == True ]'; then
        MBT_NOTE="driver $pod restarted in place: $restarts -> $(read -r _ r _ <<< "$after"; echo "$r") restarts"
    else
        MBT_ERR="the driver $pod did not restart in place in ${MBT_NRI_RESTART_WAIT}s after containerd restarted: ${after:-$before}"
        return 1
    fi
    mbt-driver-ready-wait "$vm"
}

###
### Node reboot (rules A41-A44)
###

mbt-boot-id() {
    # Usage: mbt-boot-id VMDIR
    #
    # Print the boot id of the VM of VMDIR, quietly, over an ssh connection
    # of its own (never through a ControlMaster, which may still be one to
    # the guest before its reboot), 15 s at most. Return non-zero if ssh
    # does not reach the VM.
    command timeout 15 ssh -F "$1/.ssh-config" -o ConnectTimeout=5 -o ControlMaster=no -o ControlPath=none \
        node cat /proc/sys/kernel/random/boot_id 2>/dev/null
}

mbt-ssh-reset() {
    # Usage: mbt-ssh-reset VMDIR
    #
    # Stop the ssh ControlMaster of the VM of VMDIR, if there is one: the
    # next command opens a new connection. After a reboot the old master
    # may still have its socket and hang. Never fails.
    command timeout 10 ssh -F "$1/.ssh-config" -O exit node > /dev/null 2>&1
    return 0
}

mbt-qemu-fixed-require() {
    # Usage: mbt-qemu-fixed-require VMDIR...
    #
    # MBT_REBOOT=guest: fail the test unless the qemu process of each VM
    # runs a binary whose cxl-type3 device resets its CXL DVSEC at a guest
    # reset (without that a guest reboot leaves every CXL memdev unusable
    # until qemu restarts): the
    # binary it was started from is still there (its /proc/PID/exe is not
    # "(deleted)", the binary of an older build that a rebuild replaced),
    # and it has the function of that reset, ct3d_reset_device_dvsec of
    # hw/mem/cxl_type3.c, by name (in its debug information: the function
    # is inlined; a binary without it has build_dvsecs only). A binary
    # without debug information cannot tell: a warning. The source tree
    # and its times are not looked at (a git checkout touches them).
    local vmdir pid exe bin label
    for vmdir in "$@"; do
        label=$(vmdir-label "$vmdir")
        pid=$(vm-qemu-pid "$vmdir" | head -n 1)
        [ -n "$pid" ] || error "MBT_REBOOT=guest: no qemu process of $label ($vmdir)"
        exe=$(readlink "/proc/$pid/exe") || error "MBT_REBOOT=guest: cannot read /proc/$pid/exe of the qemu of $label"
        bin=${exe% (deleted)}
        [ "$bin" == "$exe" ] ||
            error "MBT_REBOOT=guest: the qemu of $label (pid $pid) runs a binary that is gone ($exe), maybe without the CXL DVSEC reset at a guest reset (a guest reboot leaves every CXL memdev unusable). Restart its qemu (vmdir-restart, with the cluster idle) so that it runs $bin"
        if grep -qaF ct3d_reset_device_dvsec "$bin"; then
            echo "$label: qemu pid $pid runs $bin (built $(date -r "$bin" '+%F %T')), with the CXL DVSEC reset"
        elif grep -qaF build_dvsecs "$bin"; then
            error "MBT_REBOOT=guest: the qemu binary $bin of $label (pid $pid) has no CXL DVSEC reset at a guest reset (no ct3d_reset_device_dvsec): use a qemu whose cxl-type3 device resets its CXL DVSEC at a guest reset (ct3d_reset_device_dvsec of hw/mem/cxl_type3.c) and restart the qemu of the VM"
        else
            echo "WARNING: MBT_REBOOT=guest: cannot tell if the qemu binary $bin of $label (pid $pid) resets the CXL DVSEC at a guest reset (no debug information): a guest reboot without it leaves every CXL memdev unusable"
        fi
    done
}

mbt-trace-vm-again() {
    # Usage: mbt-trace-vm-again VM
    #
    # After a reboot of VM: start the collectors of a running trace in the
    # VM again that the boot ended, udev and kubelet as dra-trace-start
    # does (the streams go on in the same files). The driver log is
    # followed once the driver runs (mbt-act-node-reboot). Nothing if no
    # trace runs.
    local vmdir=${MBT_VMDIR[$1]} label
    [ -n "$DRA_TRACE_RUNNING" ] || return 0
    label=$(vmdir-label "$vmdir")
    dra-trace-collect-vm "$vmdir" "$label.udev" lines "stdbuf -oL udevadm monitor -k -u -s cxl -s dax -s memory -s node"
    dra-trace-collect-vm "$vmdir" "$label.kubelet" lines "journalctl -f -n0 -o cat -u kubelet | grep --line-buffered -iE 'dra|resourceclaim|cxl'"
}

mbt-local-again() {
    # Usage: mbt-local-again VM
    #
    # After a qemu restart of VM: attach the local memdev of the test again,
    # as the setup does (dra-local-device with the server that knows it,
    # LOCAL_SERVER of code.var.sh), once its server has dropped the
    # attachment of the old qemu process (60 s at most; a server that still
    # lists it attached would make dra-local-device wait for a device that
    # is not there). Return non-zero on failure, with MBT_ERR. The output of
    # dra-local-device goes to the executor log; it runs in a subshell, so
    # that its error() fails only the step.
    local vm="$1" vmdir=${MBT_VMDIR[$1]} srv state="" deadline
    srv=${LOCAL_SERVER[$vmdir]:-}
    [ -n "$srv" ] || { MBT_ERR="no server of the local memdev of $vm (LOCAL_SERVER)"; return 1; }
    deadline=$(( SECONDS + 60 ))
    while :; do
        state=$( { dra-local-find --pool "$srv" "$vmdir" >> "$MBT_EXEC_LOG" 2>&1 && echo "$DRA_LOCAL_ATTACHMENT"; } )
        echo "$(date +%T.%N) $vm: the local memdev in $srv: ${state:-not found}" >> "$MBT_EXEC_LOG"
        [ "$state" != detached ] || break
        if [ "$state" == attached ] && vmdir-command-q "$vmdir" "grep -qix '$MBT_LOCAL_SERIAL' /sys/bus/cxl/devices/mem*/serial" 2>/dev/null; then
            break   # in the new qemu already
        fi
        [ "$SECONDS" -lt "$deadline" ] || { MBT_ERR="$srv lists the local memdev of $vm as '${state:-not found}' 60 s after the qemu restart, not detached"; return 1; }
        sleep 1
    done
    ( dra-local-device --pool "$srv" "$vmdir" ) >> "$MBT_EXEC_LOG" 2>&1 ||
        { MBT_ERR="dra-local-device --pool $srv failed for $vm: $(grep '^error' "$MBT_EXEC_LOG" | tail -n 1)"; return 1; }
}

mbt-act-node-reboot() {
    # Usage: mbt-act-node-reboot VM
    #
    # Reboot the node of VM, vm0 or vm1 (amendment A42), the way MBT_REBOOT
    # of the run says: guest, systemctl reboot in the guest (qemu keeps
    # running and keeps its devices); qemu, the qemu process restarted as
    # vmdir-restart does (shutdown -h, vagrant up --no-provision: its
    # devices are gone, the pool controllers attach theirs again), then
    # the local memdev of the test attached again (mbt-local-again).
    # Before: a driver-kill-at watcher of VM disarmed (reason "disarmed by
    # node-reboot"), the log of the driver container saved as
    # $TEST_OUTPUT_DIR/nri-dra-driver-cxl.<vm>.<pod>.r<N>.log (N: its
    # restarts), the boot id noted. After: ssh with a new boot id
    # (MBT_REBOOT_WAIT s at most), auto_online_blocks offline (as
    # pool-vm-reset), what the executor keeps of VM made anew (the ssh
    # ControlMaster, the trace collectors of VM, the restarts of its driver
    # in MBT_DRIVER_SEEN, the observer's kernel log state with the counts
    # of before: the whole log of the new boot is read), and the driver
    # ready (mbt-driver-ready-wait, A15). The result has the new boot id
    # ("boot", null until there is one). Return 2 for a VM that is not
    # vm0 or vm1 or in a run without MBT_REBOOT, 1 on a failure (MBT_ERR).
    local vm="$1" vmdir label old_boot new_boot="" before pod restarts ready since t0=$EPOCHREALTIME
    local qold="" qnew="" t_ssh t_ready deadline notes=""
    case "$vm" in
        vm0|vm1) ;;
        *) MBT_ERR="node-reboot is for vm0 and vm1, not '$vm'"; return 2;;
    esac
    case "$MBT_REBOOT_RUN" in
        guest|qemu) ;;
        *) MBT_ERR="node-reboot needs MBT_REBOOT=guest or qemu, this run has ${MBT_REBOOT_RUN:-none}"; return 2;;
    esac
    vmdir=${MBT_VMDIR[$vm]}
    label=$(vmdir-label "$vmdir")
    MBT_RESULT='{"boot":null}'
    if [ -n "$MBT_KILLAT_VM" ] && [ "$MBT_KILLAT_VM" == "$vm" ]; then
        mbt-killat-disarm "$vm" node-reboot
    fi
    mbt-sh "$vm" "cat /proc/sys/kernel/random/boot_id" ||
        { MBT_ERR="cannot read the boot id of $vm: $MBT_ERR"; return 1; }
    old_boot=$(head -n 1 <<< "$MBT_OUT")
    [ -n "$old_boot" ] || { MBT_ERR="no boot id of $vm"; return 1; }
    if before=$(mbt-driver-restarts "$vm"); then
        read -r pod restarts ready <<< "$before"
        vmdir-command-q "$OUTPUT_DIR" "kubectl logs -n kube-system $pod -c $DRA_CONTAINER" \
            > "$TEST_OUTPUT_DIR/$DRA_CONTAINER.$label.$pod.r$restarts.log" 2>&1
    fi
    MBT_TRACE_REFOLLOW[$vm]=$(mbt-utc 2)
    since=${MBT_TRACE_REFOLLOW[$vm]}
    echo "$(date +%T.%N) $vm: node-reboot ($MBT_REBOOT_RUN), boot $old_boot, driver ${before:-none}" >> "$MBT_EXEC_LOG"
    t0=$EPOCHREALTIME
    case "$MBT_REBOOT_RUN" in
        guest)
            # The ssh returns, or breaks when sshd stops: either is fine.
            command timeout 30 ssh -F "$vmdir/.ssh-config" -o ConnectTimeout=5 -o ControlMaster=no -o ControlPath=none \
                node sudo systemctl reboot >> "$MBT_EXEC_LOG" 2>&1
            echo "$(date +%T.%N) $vm: systemctl reboot: exit status $?" >> "$MBT_EXEC_LOG"
            mbt-ssh-reset "$vmdir";;
        qemu)
            qold=$(vm-qemu-pid "$vmdir" | head -n 1)
            # vmdir-restart fails the test with error(): in a subshell it
            # fails the step only.
            ( vmdir-restart "$vmdir" ) >> "$MBT_EXEC_LOG" 2>&1 ||
                { MBT_ERR="vmdir-restart of $vm failed (see $MBT_EXEC_LOG)"; return 1; }
            qnew=$(vm-qemu-pid "$vmdir" | head -n 1)
            echo "$(date +%T.%N) $vm: qemu pid ${qold:-none} -> ${qnew:-none}" >> "$MBT_EXEC_LOG"
            [ -n "$qnew" ] && [ "$qnew" != "$qold" ] ||
                { MBT_ERR="the qemu of $vm did not restart: pid ${qold:-none} -> ${qnew:-none}"; return 1; }
            notes+=", qemu $qold -> $qnew"
            mbt-ssh-reset "$vmdir";;
    esac
    deadline=$(( SECONDS + MBT_REBOOT_WAIT ))
    while :; do
        new_boot=$(mbt-boot-id "$vmdir") && [ -n "$new_boot" ] && [ "$new_boot" != "$old_boot" ] && break
        new_boot=""
        [ "$SECONDS" -lt "$deadline" ] ||
            { MBT_ERR="no ssh to $vm with a new boot id in ${MBT_REBOOT_WAIT}s (boot $old_boot before)"; return 1; }
        sleep 1
    done
    t_ssh=$(printf '%.1f' "$(echo "$EPOCHREALTIME - $t0" | bc)")
    MBT_RESULT=$(mbt-json boot "$new_boot")
    echo "$(date +%T.%N) $vm: ssh with boot $new_boot after $t_ssh s" >> "$MBT_EXEC_LOG"
    mbt-ssh-reset "$vmdir"
    # The observer's kernel position (A24) from the start: it reads every
    # record of the new boot, the boot's own too.
    mbt-sh "$vm" "echo offline > /sys/devices/system/memory/auto_online_blocks && cat /sys/devices/system/memory/auto_online_blocks
mkdir -p ${MBT_KERNEL_STATE%/*} && echo '{\"seq\": -1}' > $MBT_KERNEL_STATE" ||
        { MBT_ERR="$vm after the boot: cannot set auto_online_blocks offline: $MBT_ERR"; return 1; }
    if [ "$MBT_REBOOT_RUN" == qemu ]; then
        mbt-local-again "$vm" || return 1
        notes+=", local memdev attached again"
    fi
    mbt-trace-vm-again "$vm"
    mbt-driver-ready-wait "$vm" || return 1
    t_ready=$(printf '%.1f' "$(echo "$EPOCHREALTIME - $t0" | bc)")
    echo "$(date +%T.%N) $vm: driver ready $t_ready s after the reboot" >> "$MBT_EXEC_LOG"
    # The driver container restarted in its pod: what the next
    # observation would see as a restart (mbt-drivers-seen) is done here,
    # the log of the old container was saved above.
    if before=$(mbt-driver-restarts "$vm"); then
        read -r pod restarts ready <<< "$before"
        MBT_DRIVER_SEEN[$vm]="$pod $restarts"
        if [ -n "$DRA_TRACE_RUNNING" ]; then
            dra-trace-collect-vm "$vmdir" "$label.driver" lines "kubectl logs -n kube-system -f --since-time=$since -c $DRA_CONTAINER $pod"
        fi
        unset "MBT_TRACE_REFOLLOW[$vm]"
        notes+=", driver $pod restarts $restarts"
    fi
    MBT_NOTE="$MBT_REBOOT_RUN: boot ${old_boot:0:8} -> ${new_boot:0:8}, ssh after $t_ssh s, driver ready after $t_ready s$notes"
}

###
### Fencing modes (amendments A36, A37)
###

mbt-driver-fencing() {
    # Usage: mbt-driver-fencing VM [POD]
    #
    # Print the fencing mode that the driver pod POD of VM (default: its
    # running pod) logged at its start ("defaults.container.fencing: MODE:
    # ...", D/driver.go logDefaults), quietly. Return 1 if there is no such
    # pod or line yet.
    local vm="$1" pod="${2:-}" line
    [ -n "$pod" ] || pod=$(dra-driver-pod "${MBT_VMDIR[$vm]}") || return 1
    line=$(vmdir-command-q "$OUTPUT_DIR" "kubectl logs -n kube-system $pod -c $DRA_CONTAINER 2>/dev/null | grep -m1 -o '${MBT_FENCING_LOG}[a-z-]*'" 2>/dev/null)
    [ -n "$line" ] || return 1
    echo "${line#"$MBT_FENCING_LOG"}"
}

mbt-fencing-wait() {
    # Usage: mbt-fencing-wait VM MODE [POD]
    #
    # Wait until the driver pod POD of VM (default: its running pod) has
    # logged its fencing mode, 30 s at most, and return 0 if that is MODE.
    # MBT_ERR says what it logged otherwise. MBT_FENCING_GOT is the mode
    # it logged, empty if none.
    local vm="$1" want="$2" pod="${3:-}" got=""
    MBT_FENCING_GOT=""
    retry-until --timeout 30 --interval 1 --message "$vm: the driver logged '$MBT_FENCING_LOG...'" \
        'got=$(mbt-driver-fencing "$vm" "$pod")' >/dev/null ||
        { MBT_ERR="the driver ${pod:-pod} of $vm has not logged '$MBT_FENCING_LOG' in 30 s"; return 1; }
    MBT_FENCING_GOT=$got
    echo "$(date +%T.%N) $vm: the driver ${pod:-pod} logged fencing $got" >> "$MBT_EXEC_LOG"
    [ "$got" == "$want" ] || { MBT_ERR="the driver ${pod:-pod} of $vm logged fencing $got, expected $want"; return 1; }
}

mbt-fencing-check() {
    # Usage: mbt-fencing-check MODE
    #
    # After dra-install: fail the test unless the driver of every VM logged
    # the fencing mode MODE (allowed-mems or none) at its start. Set
    # MBT_FENCING_NOW to MODE.
    local vm
    for vm in "${MBT_VMIDS[@]}"; do
        mbt-fencing-wait "$vm" "$1" || error "mbt-fencing-check: $MBT_ERR"
        echo "$vm: the driver $(dra-driver-pod "${MBT_VMDIR[$vm]}") has $MBT_FENCING_LOG$1"
    done
    MBT_FENCING_NOW=$1
}

mbt-fencing-config() {
    # Usage: mbt-fencing-config CONFIG-YAML
    #
    # Print "OLD NEW" and, on the next line, the merge patch of the
    # ConfigMap $MBT_FENCING_CM that switches config.yaml CONFIG-YAML (the
    # driver configuration) to the other fencing mode: OLD is its
    # defaults.container.fencing (allowed-mems when it is not set), NEW the
    # other one; the rest of the configuration is kept. Return 1 if it is
    # not a YAML mapping or has another mode.
    python3 - "$1" <<'EOF'
import json, sys, yaml
c = yaml.safe_load(sys.argv[1]) or {}
if not isinstance(c, dict):
    sys.exit("config.yaml is not a mapping: %r" % sys.argv[1][:200])
ctr = c.setdefault("defaults", {}).setdefault("container", {})
old = ctr.get("fencing") or "allowed-mems"
new = {"allowed-mems": "none", "none": "allowed-mems"}.get(old)
if new is None:
    sys.exit("config.yaml has fencing %r, not allowed-mems or none" % old)
ctr["fencing"] = new
print(old, new)
print(json.dumps({"data": {"config.yaml": yaml.safe_dump(c, default_flow_style=False)}}))
EOF
}

mbt-act-fencing-switch() {
    # Usage: mbt-act-fencing-switch
    #
    # Switch the fencing mode of every driver (A37, only when the run's
    # MBT_FENCING is switch): read config.yaml of the ConfigMap
    # $MBT_FENCING_CM in kube-system, patch it to the other mode
    # (mbt-fencing-config: the rest of the configuration is kept), then
    # restart the driver of vm2, vm1 and vm0 in this order as
    # driver-restart does (mbt-driver-replace, disarming a driver-kill-at
    # watcher with "disarmed by fencing-switch"), each new pod checked to
    # log the new mode. The DaemonSet keeps the checksum/config annotation
    # of helm: the pods are deleted explicitly, and helm uninstall deletes
    # the patched ConfigMap at the cleanup. The driver reads config.yaml
    # from a ConfigMap volume (not subPath) at its start; kubelet fills the
    # volume of a new pod from its own copy of the ConfigMap, which with
    # the default change detection (Watch) is the watch it keeps for the
    # pods of the node that use it: updated by the patch's watch event
    # long before the new pod starts in practice, not at once in principle.
    # So a new pod that logs the old mode is deleted once more (noted in
    # MBT_NOTE) before the verb fails. The result has the new mode
    # ("fencing"). Return 2 if the run is not of mode switch, 1 if a step
    # fails (MBT_ERR).
    local cm modes old new patch vm notes=""
    [ "$MBT_FENCING_RUN" == switch ] || { MBT_ERR="fencing-switch needs MBT_FENCING=switch, this run has MBT_FENCING=${MBT_FENCING_RUN:-(unset)}"; return 2; }
    mbt-sh vm2 "kubectl get configmap -n kube-system $MBT_FENCING_CM -o jsonpath='{.data.config\.yaml}'" ||
        { MBT_ERR="cannot read the ConfigMap $MBT_FENCING_CM: $MBT_ERR"; return 1; }
    cm=$MBT_OUT
    modes=$(mbt-fencing-config "$cm" 2>"$MBT_TMP/stderr") || { MBT_ERR="config.yaml of $MBT_FENCING_CM: $(<"$MBT_TMP/stderr")"; return 1; }
    read -r old new <<< "$(head -n 1 <<< "$modes")"
    patch=$(tail -n +2 <<< "$modes")
    [ "$old" == "$MBT_FENCING_NOW" ] ||
        echo "$(date +%T.%N) fencing-switch: the ConfigMap has $old, the drivers had $MBT_FENCING_NOW" >> "$MBT_EXEC_LOG"
    mbt-sh vm2 "kubectl patch configmap -n kube-system $MBT_FENCING_CM --type merge --patch-file /dev/stdin <<'MBT_EOF'
$patch
MBT_EOF
kubectl get configmap -n kube-system $MBT_FENCING_CM -o jsonpath='{.data.config\.yaml}'" ||
        { MBT_ERR="cannot patch the ConfigMap $MBT_FENCING_CM: $MBT_ERR"; return 1; }
    MBT_FENCING_NOW=$new
    MBT_RESULT=$(mbt-json fencing "$new")
    for vm in vm2 vm1 vm0; do
        mbt-driver-replace "$vm" fencing-switch || { MBT_ERR="$vm: $MBT_ERR"; return 1; }
        notes+=", $vm $MBT_NOTE"
        mbt-fencing-wait "$vm" "$new" "$MBT_DRIVER_NEW" && continue
        # (no mode logged: the driver is broken, no second try)
        [ -n "$MBT_FENCING_GOT" ] || return 1
        echo "$(date +%T.%N) fencing-switch: $MBT_ERR (a stale ConfigMap volume?): deleting it once more" >> "$MBT_EXEC_LOG"
        mbt-driver-replace "$vm" fencing-switch || { MBT_ERR="$vm, again: $MBT_ERR"; return 1; }
        notes+=" -> ${MBT_NOTE#* -> } (it had $MBT_FENCING_GOT)"
        mbt-fencing-wait "$vm" "$new" "$MBT_DRIVER_NEW" || { MBT_ERR="$MBT_ERR (also after a second restart)"; return 1; }
        MBT_ERR=""
    done
    MBT_NOTE="fencing $old -> $new$notes"
}

###
### A step
###

mbt-run() {
    # Usage: mbt-run STEP VERB ARGS...
    #
    # Execute step STEP, the action VERB ARGS (contract 4, not settle):
    # print the progress line and the result line, set MBT_STATUS (2 for
    # an unknown verb or bad arguments) and MBT_RESULT (contract 6.2).
    local step="$1" verb="$2" t0=$EPOCHREALTIME secs nargs=1
    shift 2
    echo "### mbt step $step: $verb${*:+ $*}"
    echo "=== step $step: $verb${*:+ $*} ($(date +%T.%N))" >> "$MBT_EXEC_LOG"
    MBT_RESULT='{}' MBT_NOTE="" MBT_ERR="" MBT_OUT=""
    case "$verb" in
        lease-get|pwrite|pwipe|driver-kill-at) nargs=2;;
    esac
    case "$verb" in
        create|delete|crash|read|lease-put|lease-get|driver-kill|driver-restart|ctrl-kill|ctrl-start|ctrl-restart|server-restart|kubelet-restart|containerd-restart|pread|hold|unhold|pwipe|driver-kill-at|node-reboot)
            if [ $# != "$nargs" ]; then
                MBT_ERR="$verb takes $nargs arguments, got $#: $*"
                MBT_STATUS=2
            else
                "mbt-act-$verb" "$@"
                MBT_STATUS=$?
            fi;;
        write|pwrite)
            # The token of the step: mbt-STEP.
            if [ $# != "$nargs" ]; then
                MBT_ERR="$verb takes $nargs arguments, got $#: $*"
                MBT_STATUS=2
            else
                "mbt-act-$verb" "$@" "$step"
                MBT_STATUS=$?
            fi;;
        fencing-switch)
            if [ $# != 0 ]; then
                MBT_ERR="$verb takes no arguments, got $#: $*"
                MBT_STATUS=2
            else
                mbt-act-fencing-switch
                MBT_STATUS=$?
            fi;;
        *)
            MBT_ERR="unknown verb '$verb'"
            MBT_STATUS=2;;
    esac
    secs=$(printf '%.2f' "$(echo "$EPOCHREALTIME - $t0" | bc)")
    echo "    status $MBT_STATUS, ${secs} s${MBT_NOTE:+, $MBT_NOTE}${MBT_ERR:+, stderr: $(head -c 200 <<< "$MBT_ERR" | tr '\n' ' ')}"
    printf '%s\t%s\t%s\t%s\t%s\t\n' "$step" "$verb" "$*" "$MBT_STATUS" "$secs" >> "$MBT_TIMES"
    return 0
}

###
### Observation (contract 6)
###

mbt-observer-install() {
    # Usage: mbt-observer-install VMDIR...
    #
    # Copy mbt-observe.py into the VMs ($MBT_OBSERVER_VM) and run it once
    # in each: the baseline of its "kernel" part (the kernel log from now
    # on) and of nothing else. Stop driver-kill-at watchers of an earlier
    # run. Fail the test if it does not print an observation.
    local vmdir
    for vmdir in "$@"; do
        vmdir-put-file "$vmdir" "$MBT_OBSERVER" "$MBT_OBSERVER_VM"
        vmdir-command "$vmdir" "pkill -f '$MBT_OBSERVER_VM kill-at'; rm -f /run/mbt-observe/kernel.json $MBT_KILLAT_STATE; chmod 755 $MBT_OBSERVER_VM && python3 $MBT_OBSERVER_VM vm --ns $DRA_NS | head -c 300; echo; exit \${PIPESTATUS[0]}" ||
            command-error "mbt-observe.py does not run in $(vmdir-label "$vmdir")"
    done
}

mbt-observe() {
    # Usage: mbt-observe STEP POLL
    #
    # Make settle observation POLL of step STEP (contract 6.1): in
    # parallel, mbt-observe.py in each VM (with the cluster parts in the
    # VM of the test) and the attachments of each pool server, then
    # mbt-observe.py assemble on the host. MBT_OBS is the observation (one
    # JSON line), MBT_OBS_FILE its file in $MBT_OBS_DIR (the last
    # $MBT_OBS_KEEP are kept). Update the table of inspected pods, and
    # follow the logs of restarted drivers (mbt-drivers-seen). A VM whose
    # part takes longer than MBT_OBS_TIMEOUT s is left out (null, with an
    # error): the observer runs under timeout in the VM, and its ssh is
    # killed on the host 10 s later. 5 s before its timeout the observer
    # writes the state, system call and kernel stack of the threads of the
    # driver and of itself and its Python stacks to its stderr, kept as
    # mbt-obs-stacks/obs-STEP-POLL.VM.txt when the part printed nothing or
    # the stacks. Return non-zero if the observation could not be
    # assembled: MBT_OBS is then an observation with the error only.
    local step="$1" poll="$2" t0=$EPOCHREALTIME vm p pids=() cmd inspected args=() url dev stacks
    inspected=$(paste -sd, "$MBT_TMP/inspected")
    # A24 pdevs from the backing files on the host.
    for dev in "${!MBT_PDEV_FILES[@]}"; do
        args+=(--pdev "$dev=${MBT_PDEV_FILES[$dev]}")
    done
    for vm in "${MBT_VMIDS[@]}"; do
        cmd="timeout -k 5 $MBT_OBS_TIMEOUT python3 $MBT_OBSERVER_VM vm --ns $DRA_NS --stacks-after $(( MBT_OBS_TIMEOUT - 5 ))"
        [ "$vm" != vm2 ] || cmd+=" --cluster --nodes $MBT_NODES --units $MBT_UNITS --pools $MBT_POOLS --inspected '$inspected'"
        vmdir-command-q "${MBT_VMDIR[$vm]}" "$cmd" > "$MBT_TMP/obs.$vm" 2> "$MBT_TMP/obs.$vm.err" &
        pids+=("$!")
        args+=(--vm "$vm=$MBT_TMP/obs.$vm")
    done
    for p in "${MBT_POOL_IDS[@]}"; do
        url=$(pool-url "${MBT_POOL_SERVER[$p]}") || url=""
        curl -s --max-time 10 "$url/api/v1/attachments" > "$MBT_TMP/att.$p" 2>/dev/null &
        pids+=("$!")
        args+=(--attachments "$p=$MBT_TMP/att.$p")
    done
    mbt-wait-pids $(( MBT_OBS_TIMEOUT + 10 )) "${pids[@]}"
    for vm in "${MBT_VMIDS[@]}"; do
        if [ ! -s "$MBT_TMP/obs.$vm" ] || grep -qxF "$MBT_OBS_STACKS_MARKER" "$MBT_TMP/obs.$vm.err"; then
            stacks="mbt-obs-stacks/obs-$(printf '%06d-%03d' "$step" "$poll").$vm.txt"
            mkdir -p "$TEST_OUTPUT_DIR/mbt-obs-stacks"
            cp "$MBT_TMP/obs.$vm.err" "$TEST_OUTPUT_DIR/$stacks"
            echo "    observation $poll of step $step: $vm printed no observation or its stacks, its stderr in $stacks"
        fi
    done
    if MBT_OBS=$(python3 "$MBT_OBSERVER" assemble --step "$step" --poll "$poll" --nodes "$MBT_NODES" "${args[@]}" \
                     --inspected-in "$MBT_TMP/inspected" --inspected-out "$MBT_TMP/inspected.new" \
                     --summary "$MBT_TMP/summary" 2>"$MBT_TMP/assemble.err") && [ -n "$MBT_OBS" ]; then
        mv "$MBT_TMP/inspected.new" "$MBT_TMP/inspected"
    else
        MBT_OBS=$(mbt-json step "@json:$step" poll "@json:$poll" errors "@json:[$(python3 -c 'import json, sys; print(json.dumps("assemble: " + sys.argv[1][-500:]))' "$(cat "$MBT_TMP/assemble.err")")]")
        echo "    observation $poll of step $step failed: $(tail -n 3 "$MBT_TMP/assemble.err")"
    fi
    MBT_OBS_FILE="$MBT_OBS_DIR/obs-$(printf '%06d-%03d' "$step" "$poll").json"
    echo "$MBT_OBS" > "$MBT_OBS_FILE"
    MBT_OBS_FILES+=("$MBT_OBS_FILE")
    while [ "${#MBT_OBS_FILES[@]}" -gt "$MBT_OBS_KEEP" ]; do
        rm -f "${MBT_OBS_FILES[0]}"
        MBT_OBS_FILES=("${MBT_OBS_FILES[@]:1}")
    done
    MBT_OBS_SECONDS=$(printf '%.2f' "$(echo "$EPOCHREALTIME - $t0" | bc)")
    printf '%s\t%s\t%s\n' "$step" "$poll" "$MBT_OBS_SECONDS" >> "$MBT_OBS_TIMES"
    mbt-drivers-seen
}

mbt-wait-pids() {
    # Usage: mbt-wait-pids TIMEOUT PID...
    #
    # Wait until the background jobs PID of this shell have exited,
    # TIMEOUT seconds at most; then kill those that run (and their
    # children: the ssh of a vmdir-command-q) and reap them. Return 1 if
    # some had to be killed.
    local tmo="$1" end pid alive rc=0
    shift
    end=$(( SECONDS + tmo ))
    while :; do
        alive=""
        for pid in "$@"; do
            ! kill -0 "$pid" 2>/dev/null || alive+=" $pid"
        done
        [ -n "$alive" ] && [ "$SECONDS" -lt "$end" ] || break
        sleep 0.05
    done
    for pid in $alive; do
        echo "    mbt-wait-pids: killing background job $pid after ${tmo}s"
        pkill -TERM -P "$pid" 2>/dev/null
        kill -TERM "$pid" 2>/dev/null
        rc=1
    done
    wait "$@" 2>/dev/null
    return "$rc"
}

mbt-drivers-seen() {
    # Usage: mbt-drivers-seen
    #
    # After an observation: for every driver whose container restarted in
    # its pod since the last observation, store the log of the container
    # that ended as $TEST_OUTPUT_DIR/nri-dra-driver-cxl.<vm>.<pod>.r<N>.log
    # (N: its restart count), and while a trace runs, follow the log of the
    # driver again where the stream ended (driver-kill, kubelet-restart,
    # containerd-restart and such restarts).
    local word vm pod restarts ready old_pod old_restarts label since
    [ -f "$MBT_TMP/summary" ] || return 0
    while read -r word vm pod restarts ready; do
        [ "$word" == driver ] && [ -n "${MBT_VMDIR[$vm]:-}" ] || continue
        label=$(vmdir-label "${MBT_VMDIR[$vm]}")
        read -r old_pod old_restarts <<< "${MBT_DRIVER_SEEN[$vm]:-}"
        since=${MBT_TRACE_REFOLLOW[$vm]:-}
        if [ "$pod" == "$old_pod" ] && [ "$restarts" != "$old_restarts" ]; then
            echo "    the driver of $vm restarted in $pod: $old_restarts -> $restarts restarts"
            vmdir-command-q "$OUTPUT_DIR" "kubectl logs -n kube-system $pod -c $DRA_CONTAINER --previous" \
                > "$TEST_OUTPUT_DIR/$DRA_CONTAINER.$label.$pod.r$(( restarts - 1 )).log" 2>&1
            [ -n "$since" ] || since=$(mbt-utc 10)
        elif [ -n "$old_pod" ] && [ "$pod" != "$old_pod" ]; then
            echo "    the driver of $vm is a new pod: $old_pod -> $pod"
            [ -n "$since" ] || since=$(mbt-utc 10)
        fi
        if [ "$ready" == ready ] && [ -n "$since" ]; then
            if [ -n "$DRA_TRACE_RUNNING" ]; then
                dra-trace-collect-vm "${MBT_VMDIR[$vm]}" "$label.driver" lines "kubectl logs -n kube-system -f --since-time=$since -c $DRA_CONTAINER $pod"
            fi
            unset "MBT_TRACE_REFOLLOW[$vm]"
        fi
        MBT_DRIVER_SEEN[$vm]="$pod $restarts"
    done < "$MBT_TMP/summary"
}

###
### The generator: on-line (coprocess) and off-line (code.gen.sh)
###

mbt-coproc-start() {
    # Usage: mbt-coproc-start ARGS...
    #
    # Start "$MBT_BIN ARGS" as the coprocess MBTGEN, its stderr appended to
    # $MBT_LOG. Its fds exist in this shell only (not in ( ... ), not in
    # external commands); MBT_RFD is a copy of its stdout that bash does not
    # close when it reaps the coprocess, so that its last lines are read.
    [ -z "$MBT_GEN_PID" ] || error "mbt-coproc-start: the generator runs already, pid $MBT_GEN_PID"
    coproc MBTGEN { exec "$MBT_BIN" "$@" 2>>"$MBT_LOG"; }
    # shellcheck disable=SC2153,SC2154 # MBTGEN_PID and MBTGEN are of coproc
    MBT_GEN_PID=$MBTGEN_PID
    exec {MBT_RFD}<&"${MBTGEN[0]}"
    echo "mbt: started $MBT_BIN $* (pid $MBT_GEN_PID), its log is $MBT_LOG"
}

mbt-coproc-stop() {
    # Usage: mbt-coproc-stop
    #
    # Close the stdin of the generator, give it 5 s to exit, then stop it
    # (SIGTERM, SIGKILL). Never fails.
    local pid="$MBT_GEN_PID" _
    [ -n "$pid" ] || return 0
    MBT_GEN_PID=""
    [ -z "${MBTGEN[1]:-}" ] || eval "exec ${MBTGEN[1]}>&-"
    for _ in $(seq 50); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
        echo "mbt: the generator (pid $pid) did not exit, stopping it"
        kill -TERM "$pid" 2>/dev/null
        sleep 1
        kill -KILL "$pid" 2>/dev/null
    fi
    [ -z "$MBT_RFD" ] || eval "exec $MBT_RFD<&-"
    MBT_RFD=""
    wait "$pid" 2>/dev/null
    return 0
}

mbt-send() {
    # Usage: mbt-send LINE
    #
    # Send LINE to the generator. Return non-zero if it is gone: SIGPIPE is
    # ignored meanwhile, it would kill this shell without the EXIT trap.
    local rc
    [ -n "${MBTGEN[1]:-}" ] || return 1
    trap '' PIPE
    printf '%s\n' "$1" >&"${MBTGEN[1]}"
    rc=$?
    trap - PIPE
    return "$rc"
}

mbt-recv() {
    # Usage: mbt-recv
    #
    # Read the next line of the generator into MBT_LINE, $MBT_READ_TIMEOUT s
    # at most. Return non-zero on timeout or end of file.
    [ -n "$MBT_RFD" ] || return 1
    IFS= read -r -t "$MBT_READ_TIMEOUT" -u "$MBT_RFD" MBT_LINE
}

mbt-gone() {
    # Usage: mbt-gone WHAT
    #
    # Fail the test: the generator did not send WHAT.
    echo "--- the end of $MBT_LOG:"
    tail -n 20 "$MBT_LOG"
    error "mbt: no $1 from the generator in ${MBT_READ_TIMEOUT}s: it exited or hangs, see $MBT_LOG"
}

mbt-fail() {
    # Usage: mbt-fail STEP MESSAGE
    #
    # Step STEP failed the oracle: print MESSAGE, keep the last observation
    # as $MBT_OBS_DIR/fail-STEP.json, show the start of the report of the
    # generator ($TEST_OUTPUT_DIR/mbt-fail.txt) and fail the test.
    local step="$1" msg="$2"
    echo "### mbt step $step FAILED: $msg"
    if [ -n "$MBT_OBS_FILE" ] && [ -f "$MBT_OBS_FILE" ]; then
        cp "$MBT_OBS_FILE" "$MBT_OBS_DIR/fail-$step.json"
        echo "the last observation: $MBT_OBS_DIR/fail-$step.json"
    fi
    if [ -f "$TEST_OUTPUT_DIR/mbt-fail.txt" ]; then
        echo "--- $TEST_OUTPUT_DIR/mbt-fail.txt (the first 80 lines):"
        head -n 80 "$TEST_OUTPUT_DIR/mbt-fail.txt"
    fi
    echo "steps: $TEST_OUTPUT_DIR/mbt-steps.txt, generator log: $MBT_LOG, executor log: $MBT_EXEC_LOG"
    error "mbt: step $step failed: $msg"
}

mbt-answer() {
    # Usage: mbt-answer STEP
    #
    # Read the answer of the generator to "done STEP ...": return on ok,
    # fail the test on fail (mbt-fail) or any other line.
    local step="$1"
    mbt-recv || mbt-gone "answer to step $step"
    case "$MBT_LINE" in
        "ok $step") return 0;;
        "fail $step"|"fail $step "*) mbt-fail "$step" "${MBT_LINE#"fail $step "}";;
        *) error "mbt: expected ok or fail of step $step from the generator, got: $MBT_LINE";;
    esac
}

mbt-settle-online() {
    # Usage: mbt-settle-online STEP
    #
    # Step STEP is settle: send observations until the generator answers
    # ok (return) or fail (mbt-fail), MBT_POLL s apart while it answers
    # wait.
    local step="$1" poll=0 t0=$EPOCHREALTIME last="" msg secs
    echo "### mbt step $step: settle"
    echo "=== step $step: settle ($(date +%T.%N))" >> "$MBT_EXEC_LOG"
    while :; do
        poll=$(( poll + 1 ))
        mbt-observe "$step" "$poll"
        mbt-send "observe $step $MBT_OBS" || mbt-gone "reader of observation $poll of step $step"
        mbt-recv || mbt-gone "answer to observation $poll of step $step"
        case "$MBT_LINE" in
            "ok $step") break;;
            "wait $step"|"wait $step "*)
                msg=${MBT_LINE#"wait $step"}
                msg=${msg# }
                [ "$msg" == "$last" ] || echo "    observation $poll (${MBT_OBS_SECONDS} s): wait: ${msg:0:300}"
                echo "observation $poll: wait: $msg" >> "$MBT_EXEC_LOG"
                last=$msg
                sleep "$MBT_POLL";;
            "fail $step"|"fail $step "*) mbt-fail "$step" "${MBT_LINE#"fail $step "}";;
            *) error "mbt: expected ok, wait or fail of step $step from the generator, got: ${MBT_LINE:0:500}";;
        esac
    done
    secs=$(printf '%.2f' "$(echo "$EPOCHREALTIME - $t0" | bc)")
    echo "    settled: $poll observations, ${secs} s"
    printf '%s\tsettle\t\t0\t%s\t%s\n' "$step" "$secs" "$poll" >> "$MBT_TIMES"
}

mbt-online() {
    # Usage: mbt-online ARGS...
    #
    # The on-line run (contract 5): start the generator with ARGS
    # (mbt-coproc-start), execute the steps it sends and answer them until
    # it ends the run, then stop it.
    local words step verb
    mbt-coproc-start "$@"
    while :; do
        mbt-recv || mbt-gone "step"
        read -r -a words <<< "$MBT_LINE"
        case "${words[0]:-}" in
            step)
                step=${words[1]:-} verb=${words[2]:-}
                [[ "$step" =~ ^[0-9]+$ ]] && [ -n "$verb" ] ||
                    error "mbt: bad step line from the generator: $MBT_LINE"
                if [ "$verb" == settle ]; then
                    mbt-settle-online "$step"
                else
                    mbt-run "$step" "$verb" "${words[@]:3}"
                    mbt-send "done $step $MBT_STATUS $MBT_RESULT" || mbt-gone "reader of done $step"
                    mbt-answer "$step"
                fi;;
            end)
                echo "### mbt: the generator ended the run:${MBT_LINE#end}"
                break;;
            fail)
                # Between steps: a replay that the model cannot follow
                # (the step of the file is not enabled) fails the next step.
                step=${words[1]:-}
                mbt-fail "$step" "${MBT_LINE#"fail $step "}";;
            *) error "mbt: unexpected line from the generator: ${MBT_LINE:0:500}";;
        esac
    done
    mbt-coproc-stop
}

mbt-header-mode() {
    # Usage: mbt-header-mode FILE FLAG
    #
    # Print the value of --FLAG (fencing, reboot) of the header of FILE, an
    # mbt-steps.txt of mbt-cxl online or a code.gen.sh of mbt-cxl generate
    # (its first line "# --profile P --seed S ..."). Return 1 if its header
    # has no --FLAG.
    python3 -c '
import shlex, sys
for line in open(sys.argv[1]):
    if not line.startswith("# --"):
        continue
    words = shlex.split(line[1:])
    for i, w in enumerate(words):
        name, eq, value = w[2:].partition("=")
        if w.startswith("--") and name == sys.argv[2]:
            if not eq and i + 1 < len(words) and not words[i + 1].startswith("--"):
                value = words[i + 1]
            print(value)
            sys.exit(0)
    break
sys.exit(1)' "$1" "$2"
}

mbt-check() {
    # Usage: mbt-check STEP EXPECT-FILE OBSERVED-FILE [SINCE]
    #
    # Run "$MBT_BIN check" on the prediction and the observation of step
    # STEP; SINCE is the Unix time of its first observation (a settle that
    # waits longer than MBT_SETTLE_TIMEOUT fails). MBT_LINE is what it
    # printed. Return its exit status: 0 ok, 3 wait, 1 fail.
    local since=()
    [ -z "${4:-}" ] || since=(--since="$4" --settle-timeout="${MBT_SETTLE_TIMEOUT}s")
    MBT_LINE=$("$MBT_BIN" check --step="$1" --expect="$2" --observed="$3" "${since[@]}" 2>>"$MBT_LOG")
}

mbt-unchecked() {
    # Usage: mbt-unchecked
    #
    # Off-line: the last step had no mbt-expect (a code.gen.sh of a
    # generator before amendment A14, which writes one for every step):
    # fail the test if it was an action with a nonzero status, as the
    # oracle fails it on-line; a settle without one was not observed.
    [ "$MBT_LAST_CHECKED" == 1 ] && return 0
    MBT_LAST_CHECKED=1
    if [ "$MBT_LAST_VERB" == settle ]; then
        echo "    WARNING: step $MBT_LAST_STEP settle has no mbt-expect: not observed"
        return 0
    fi
    [ "$MBT_LAST_STATUS" == 0 ] ||
        mbt-fail "$MBT_LAST_STEP" "$MBT_LAST_VERB: exit status $MBT_LAST_STATUS (the step has no mbt-expect: a nonzero status fails it)"
}

mbt-step() {
    # Usage: mbt-step STEP VERB ARGS...
    #
    # Off-line (a code.gen.sh): execute step STEP, and append it to
    # mbt-steps.txt. The mbt-expect that follows it checks its status and
    # result (A14), a settle is done by it. Without one, the status of an
    # action is checked here, before the next step (mbt-unchecked).
    mbt-unchecked
    MBT_LAST_STEP="$1" MBT_LAST_VERB="$2" MBT_LAST_DONE="" MBT_LAST_STATUS=0 MBT_LAST_CHECKED=""
    echo "${*:2}" >> "$TEST_OUTPUT_DIR/mbt-steps.txt"
    if [ "$2" == settle ]; then
        return 0
    fi
    mbt-run "$@"
    MBT_LAST_STATUS=$MBT_STATUS
    MBT_LAST_DONE="done $1 $MBT_STATUS $MBT_RESULT"
}

mbt-expect() {
    # Usage: mbt-expect STEP <<'EOF'
    #        PREDICTION
    #        EOF
    #
    # Off-line: check the last step, STEP, against the PREDICTION of the
    # generator with "$MBT_BIN check" (mbt-check). The observed file has
    # the line that the on-line protocol would send: "done STEP STATUS
    # JSON" after an action, "observe STEP JSON" for a settle, which is
    # observed again MBT_POLL s apart while the check says wait, for
    # MBT_SETTLE_TIMEOUT s at most. Fail the test (mbt-fail) on fail or
    # timeout.
    local step="$1" expect="$MBT_TMP/expect.json" observed="$MBT_TMP/observed.txt" rc poll=0 t0=$SECONDS secs last="" since
    cat > "$expect"
    [ "$step" == "$MBT_LAST_STEP" ] || error "mbt-expect $step: the last step was $MBT_LAST_STEP"
    MBT_LAST_CHECKED=1
    if [ "$MBT_LAST_VERB" != settle ]; then
        printf '%s\n' "$MBT_LAST_DONE" > "$observed"
        mbt-check "$step" "$expect" "$observed"
        rc=$?
        case "$rc" in
            0) return 0;;
            1|3) mbt-fail "$step" "${MBT_LINE#* }";;
            *) error "mbt: $MBT_BIN check exited $rc on step $step: $MBT_LINE";;
        esac
    fi
    echo "### mbt step $step: settle"
    echo "=== step $step: settle ($(date +%T.%N))" >> "$MBT_EXEC_LOG"
    since=$(date +%s)
    while :; do
        poll=$(( poll + 1 ))
        mbt-observe "$step" "$poll"
        printf 'observe %s %s\n' "$step" "$MBT_OBS" > "$observed"
        mbt-check "$step" "$expect" "$observed" "$since"
        rc=$?
        case "$rc" in
            0) break;;
            3)
                [ "$MBT_LINE" == "$last" ] || echo "    observation $poll (${MBT_OBS_SECONDS} s): ${MBT_LINE:0:300}"
                last=$MBT_LINE
                [ $(( SECONDS - t0 )) -lt "$MBT_SETTLE_TIMEOUT" ] ||
                    mbt-fail "$step" "not settled in ${MBT_SETTLE_TIMEOUT}s: ${MBT_LINE#wait }"
                sleep "$MBT_POLL";;
            1) mbt-fail "$step" "${MBT_LINE#fail }";;
            *) error "mbt: $MBT_BIN check exited $rc on step $step: $MBT_LINE";;
        esac
    done
    secs=$(( SECONDS - t0 ))
    echo "    settled: $poll observations, ${secs} s"
    printf '%s\tsettle\t\t0\t%s\t%s\n' "$step" "$secs" "$poll" >> "$MBT_TIMES"
}

mbt-offline() {
    # Usage: mbt-offline SCRIPT
    #
    # The off-line run: source SCRIPT, a code.gen.sh of "mbt-cxl generate"
    # (mbt-step and mbt-expect lines).
    [ -f "$1" ] || error "mbt-offline: no script $1"
    echo "mbt: running the steps of $1 ($(grep -c '^mbt-step ' "$1") steps)"
    echo "# off-line: the steps of $1" > "$TEST_OUTPUT_DIR/mbt-steps.txt"
    MBT_LAST_CHECKED=1
    # shellcheck source=/dev/null
    source "$1"
    mbt-unchecked
}

###
### Setup checks and teardown
###

MBT_KERNEL_FEATURES="ZONE_DEVICE CXL_REGION CXL_PMEM LIBNVDIMM BLK_DEV_PMEM ND_PFN NVDIMM_PFN DEV_DAX FS_DAX FS_DAX_PMD BLK_DEV_LOOP EROFS_FS"

mbt-kernel-require() {
    # Usage: mbt-kernel-require VMDIR
    #
    # Fail the test unless the kernel of the VM of VMDIR has every feature
    # that persistent pool devices need (A22),
    # =y or =m in /boot/config-$(uname -r): pmem regions (CXL_PMEM,
    # CXL_REGION, LIBNVDIMM), the fsdax namespace and its block device
    # (BLK_DEV_PMEM, ND_PFN, NVDIMM_PFN, ZONE_DEVICE), erofs (EROFS_FS)
    # with DAX (FS_DAX, FS_DAX_PMD) for access direct and on a loop device
    # (BLK_DEV_LOOP) for access cached, and devdax (DEV_DAX).
    local vmdir="$1" out missing
    out=$(vmdir-command-q "$vmdir" "c=/boot/config-\$(uname -r); test -r \$c || { echo \"no \$c\"; exit 1; }
echo \"kernel \$(uname -r)\"
for f in $MBT_KERNEL_FEATURES; do echo \"\$f=\$(sed -n \"s/^CONFIG_\$f=//p\" \$c)\"; done") ||
        error "cannot read the kernel configuration of $(vmdir-label "$vmdir"): $out"
    missing=$(grep -E '^[A-Z0-9_]+=$|^[A-Z0-9_]+=[^ym]' <<< "$out" | cut -d= -f1 | tr '\n' ' ')
    [ -z "$missing" ] ||
        error "the kernel of $(vmdir-label "$vmdir") ($(head -n 1 <<< "$out")) lacks $missing(CONFIG_*=y or =m): persistent pool devices (block, fs) cannot work. Build it with test/e2e/playbook/files/qemu-cxl-kernel.config (test00-up) and recreate the VM"
    echo "$(vmdir-label "$vmdir"): $(head -n 1 <<< "$out"): $(tail -n +2 <<< "$out" | tr '\n' ' ')"
}

mbt-local-blocks() {
    # Usage: mbt-local-blocks
    #
    # Print the --local-blocks of the generator: vm2=N,vm1=N,vm0=N, N the
    # capacity.memory of the local memdev in the cxl.nri.io ResourceSlice
    # of the node of the VM in blocks of $MBT_LOCAL_SIZE. Return non-zero if a
    # node does not publish it.
    local json vm n out=""
    json=$(vmdir-command-q "$OUTPUT_DIR" "kubectl get resourceslices -o json") || return 1
    for vm in "${MBT_VMIDS[@]}"; do
        n=$(dra-py eval "$json" "quantity(capacity(device('memdev-${MBT_LOCAL_HEX,,}', '$DRA_LOCAL_DRIVER', '${MBT_NODE[$vm]}'), 'memory') or 0) // quantity('$MBT_LOCAL_SIZE')") || return 1
        [[ "$n" =~ ^[0-9]+$ ]] && [ "$n" -gt 0 ] || return 1
        out+="${out:+,}$vm=$n"
    done
    echo "$out"
}

mbt-k8s-attachments() {
    # Usage: mbt-k8s-attachments POOL
    #
    # Print the attachments of the server of POOL that a pool controller
    # owns (owner k8s:...), "device@host state" each, quietly. Return
    # non-zero if the server does not answer.
    local url json
    url=$(pool-url "${MBT_POOL_SERVER[$1]}") || return 1
    json=$(curl -sf --max-time 10 "$url/api/v1/attachments") || return 1
    python3 -c '
import json, sys
for a in json.loads(sys.argv[1]) or []:
    if (a.get("owner") or "").startswith("k8s:"):
        print("%s@%s %s" % (a.get("device"), a.get("host"), a.get("state")))' "$json"
}

mbt-restore() {
    # Usage: mbt-restore
    #
    # Bring back what the faults of the run took away, so that the deletes
    # of the teardown are carried out: the pool servers that do not run
    # (restarted on their state), the pool controllers that are not active
    # (started), and the driver Ready on every node (waited for, 330 s at
    # most: a container that was killed often waits for its back-off).
    # Never fails the test.
    # shellcheck disable=SC2034 # dp is set in the snippet of retry-until
    local p unit vm vmdir name dp
    [ -n "$MBT_TMP" ] || return 0
    for p in "${MBT_POOL_IDS[@]}"; do
        name=${MBT_POOL_SERVER[$p]}
        if [ -n "${POOL_SERVER_PIDS[$name]:-}" ] && ! kill -0 "${POOL_SERVER_PIDS[$name]}" 2>/dev/null; then
            echo "mbt-restore: the server $name does not run, restarting it"
            pool-server-restart "$name" || echo "mbt-restore: cannot restart the server $name"
        fi
    done
    for p in "${MBT_POOL_IDS[@]}"; do
        unit=${MBT_POOL_UNIT[$p]}
        [ -n "${DRA_CONTROLLER_URLS[$unit]:-}" ] || continue
        if ! vmdir-command-q "$OUTPUT_DIR" "systemctl -q is-active $unit"; then
            echo "mbt-restore: $unit is not active, starting it"
            mbt-controller-start "$unit" || echo "mbt-restore: cannot start $unit"
        fi
    done
    [ "${#DRA_INSTALLED_VMS[@]}" -gt 0 ] || return 0
    for vm in "${MBT_VMIDS[@]}"; do
        vmdir=${MBT_VMDIR[$vm]}
        retry-until --timeout 330 --interval 2 --message "mbt-restore: a Ready pod of $DRA_DAEMONSET on ${MBT_NODE[$vm]}" \
            'dp=$(dra-driver-pod "$vmdir") && vmdir-command-q "$vmdir" "kubectl wait -n kube-system --for=condition=Ready pod/$dp --timeout=1s" >/dev/null 2>&1' ||
            echo "mbt-restore: the driver is not Ready on ${MBT_NODE[$vm]}"
    done
    return 0
}

mbt-teardown() {
    # Usage: mbt-teardown
    #
    # The end of a run (the generator has deleted every pod and settled):
    # restore the components (mbt-restore), delete every pod of $DRA_NS
    # gracefully, and fail the test unless the pods and claims are gone,
    # no pool server has an attachment of a pool controller left, and no
    # VM has a CXL region or a memdev other than its local memdev, 120 s
    # at most each.
    local vm p left
    mbt-killat-stop
    mbt-restore
    vmdir-command "$OUTPUT_DIR" "kubectl delete pods --all -n $DRA_NS --grace-period=2 --wait=true --timeout=180s 2>&1 | grep -v 'No resources found'; true"
    retry-until --timeout 120 --interval 2 --message "no pods and ResourceClaims in $DRA_NS" \
        '[ -z "$(vmdir-command-q "$OUTPUT_DIR" "kubectl get pods,resourceclaims -n $DRA_NS -o name")" ]' || {
        vmdir-command "$OUTPUT_DIR" "kubectl get pods,resourceclaims -n $DRA_NS -o wide"
        error "mbt-teardown: pods or ResourceClaims are left in $DRA_NS"
    }
    for p in "${MBT_POOL_IDS[@]}"; do
        retry-until --timeout 120 --interval 2 --message "no attachments of the controller of pool $p" \
            'left=$(mbt-k8s-attachments "$p") && [ -z "$left" ]' ||
            error "mbt-teardown: ${MBT_POOL_SERVER[$p]} still has attachments of its controller: ${left:-the server does not answer}"
    done
    for vm in "${MBT_VMIDS[@]}"; do
        pool-cxl-wait "${MBT_VMDIR[$vm]}" mbt-released "regions == [] and [m['serial'] for m in memdevs] == [$MBT_LOCAL_SERIAL]" 120
        # Persistent devices: no node mount, lease bind or loop device over
        # pmem left, no directory of a lease claim or node mount.
        left=$(mbt-pmem-held "${MBT_VMDIR[$vm]}") || error "mbt-teardown: cannot read the mounts of $vm"
        [ -z "$left" ] || error "mbt-teardown: $vm still has: $(tr '\n' ' ' <<< "$left")"
        retry-until --timeout 60 --interval 2 --message "$vm: nothing under $DRA_LEASE_FS_DIR and $DRA_POOL_MOUNT_DIR" \
            'left=$(vmdir-command-q "${MBT_VMDIR[$vm]}" "find $DRA_LEASE_FS_DIR $DRA_POOL_MOUNT_DIR -mindepth 1 -maxdepth 1 2>/dev/null; true") && [ -z "$left" ]' ||
            error "mbt-teardown: $vm still has $(tr '\n' ' ' <<< "$left")"
    done
}

mbt-exit-trap() {
    # Usage: mbt-exit-trap
    #
    # The EXIT trap of test90 (DRA_EXIT_TRAP of dra-install): stop the
    # generator, restore the components, then the cleanup of every
    # dra-driver-cxl test: dra-cleanup (deletes the pods gracefully, waits
    # for the detaches, uninstalls the driver and the controllers) and
    # pool-cleanup (releases and detaches what is left, the local memdevs
    # too, and stops the servers). A persistent device that a node mount,
    # lease bind or loop device still holds after dra-cleanup is unmounted
    # (plainly: never MNT_DETACH); if one is still held then, releasing its
    # region could crash the guest: only the servers are stopped, and what
    # holds it is printed (as test16 does). The backing files of the
    # persistent devices are removed after pool-cleanup.
    local vmdir held="" left
    mbt-coproc-stop
    echo "### mbt: cleanup after an error or an interruption"
    mbt-killat-stop
    mbt-restore
    dra-cleanup
    for vmdir in "${MBT_VMDIR[@]}"; do
        left=$(mbt-pmem-held "$vmdir") || left="cannot read its mounts"
        [ -n "$left" ] || continue
        echo "mbt-exit-trap: $(vmdir-label "$vmdir") still has: $(tr '\n' ' ' <<< "$left")"
        mbt-pmem-unmount "$vmdir"
        left=$(mbt-pmem-held "$vmdir") || left="cannot read its mounts"
        [ -z "$left" ] || held+="
  $(vmdir-label "$vmdir"): $(tr '\n' ' ' <<< "$left")"
    done
    if [ -n "$held" ]; then
        echo "mbt-exit-trap: persistent pool devices are still held, not releasing or detaching anything:$held
  By hand in each VM above: stop what uses the mount (fuser -vm MOUNT), umount MOUNT (never umount -l;
  the lease binds first), check that no /sys/block/loop*/loop/backing_file names a pmem device, then
  fake-cxl-pool-client --server <pool-c URL> guest disable SERIAL; or let the next test restart the
  VM (pool-vm-reset). Then remove the files: ${MBT_PDEV_FILES[*]}"
        pool-server-stop
        return 0
    fi
    pool-cleanup
    mbt-pdev-files-remove "$MBT_PDEV_DIR" "${!MBT_PDEV_POOL[@]}"
    for vmdir in "${MBT_VMDIR[@]}"; do
        mbt-hollow-regions-remove "$vmdir"
    done
}

mbt-hollow-regions-remove() {
    # Usage: mbt-hollow-regions-remove VMDIR
    #
    # Print and delete the hollow CXL regions of the VM of VMDIR: a region
    # that is not committed and has no target, what a pmem region becomes
    # when the driver forgot it (killed in a Prepare, the claim released
    # before kubelet retried it) and its
    # memdev was detached. Nothing is behind it, but it keeps its range of
    # the CXL window while it exists; cxl destroy-region refuses it ("error locating decoder for
    # target0"): delete_region of its root decoder. Never fails.
    vmdir-command "$1" "for r in /sys/bus/cxl/devices/region[0-9]*; do
    [ -e \"\$r\" ] || continue
    n=\${r##*/}
    [ \"\$(cat \$r/commit)\" = 0 ] && [ -z \"\$(cat \$r/target* 2>/dev/null | tr -d '[:space:]')\" ] || continue
    echo \"hollow region \$n: mode \$(cat \$r/mode) resource \$(cat \$r/resource) size \$(cat \$r/size) uuid \$(cat \$r/uuid 2>/dev/null)\"
    for d in /sys/bus/cxl/devices/root[0-9]*/decoder*; do
        [ -d \"\$d/\$n\" ] && echo \$n > \$d/delete_region && echo \"deleted \$n (\${d##*/})\"
    done
done; true" > /dev/null
    [ -z "$COMMAND_OUTPUT" ] || echo "mbt-exit-trap: $(vmdir-label "$1"): $COMMAND_OUTPUT"
}
