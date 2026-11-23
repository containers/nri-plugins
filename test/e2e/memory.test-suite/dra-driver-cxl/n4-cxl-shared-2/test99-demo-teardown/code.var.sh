# Demo teardown (demo01-share-write-read, docs/memory/cxl/demo.md):
# "demo teardown" runs this through the e2e framework. Undoes
# test98-demo-setup and leaves both VMs without CXL memory devices, the
# host without the pool server:
# - the demo pods and their leases;
# - the shared local device unconfigured first: the driver restarted
#   without sharedMemdevs destroys its region, so that it
#   can be detached;
# - the trace stopped, its summary in the output of this test;
# - the driver, the controller, the namespace (dra-uninstall);
# - no filesystem mounts, lease binds or loop devices over pmem left in
#   the VMs: else a WARNING with the manual cleanup, and the pool device
#   stays attached (releasing a region under a live filesystem can crash
#   the guest);
# - the local device detached (dra-local-detach), the pool server stopped
#   (pool-cleanup), the files of a persistent pool device removed
#   (POOL_DEVICE_FILES of the state), node labels and /root/demo removed;
# - of the wizard demo also its claims and claim templates, the local
#   devices of both VMs and the second pool server local-pmem with lp0;
# - of the llm-share demo VM0 in all of the above, the model mirror
#   stopped, the pool tmpfs that setup mounted unmounted (bin/pool-tmpfs.sh;
#   bin/demo runs latency.sh off afterwards, which unmounts its own) and
#   the pool directory removed if it is empty and no mount point.
# Works without the state file of setup too (a setup that failed half
# way): then it cleans what it finds.

[ -n "${DEMO_DIR:-}" ] || skip-test "run by bin/demo teardown"

DEMO_STATE_DIR="${DEMO_STATE_DIR:-$DEMO_DIR/run}"
state="$DEMO_STATE_DIR/state.env"
VM2="$OUTPUT_DIR"
VM1="$POOL_VM1_DIR"
VM0_DIR=""   # of the state of llm-share
DRA_NS=cxl-demo
DEMO_CASE=unknown
TRACE_DIR=""
POOL_SERVER_PID=""
POOL_DEVICE_FILES=""
LP_SERVER=local-pmem   # the second server of the wizard demo
LP_PORT=9912
LP_SERVER_PID=""
LLM_MIRROR_PID=""
DEMO_POOL_DIR="${DEMO_POOL_DIR:-}"   # of bin/demo, of the state if there is one
# shellcheck source=/dev/null # the pool tmpfs of llm-share
source "$DEMO_DIR/bin/pool-tmpfs.sh"
if [ -f "$state" ]; then
    # The views stop asking when the state is gone.
    mv "$state" "$state.teardown" || error "cannot move $state"
    # shellcheck source=/dev/null # the state of demo setup
    source "$state.teardown"
    DRA_NS="$NS"
    sleep 2
elif [ -f "$state.teardown" ]; then
    echo "demo teardown: continuing an earlier teardown"
    # shellcheck source=/dev/null # the state of demo setup
    source "$state.teardown"
    DRA_NS="$NS"
else
    echo "demo teardown: no state file $state: cleaning what is there"
    VM0_DIR="$POOL_VM0_DIR"
fi
DEMO_VMS=("$VM2" "$VM1")
[ -z "$VM0_DIR" ] || DEMO_VMS+=("$VM0_DIR")
if [ -n "$POOL_SERVER_PID" ] && ! kill -0 "$POOL_SERVER_PID" 2>/dev/null; then
    POOL_SERVER_PID=""
fi
if [ -z "$POOL_SERVER_PID" ] && pool-port-busy; then
    POOL_SERVER_PID=$(ss -ltnpH "sport = :$POOL_PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -n 1)
    echo "demo teardown: fake-cxl-pool-server on port $POOL_PORT: pid ${POOL_SERVER_PID:-unknown}"
fi
if [ -n "$LP_SERVER_PID" ] && ! kill -0 "$LP_SERVER_PID" 2>/dev/null; then
    LP_SERVER_PID=""
fi
if [ -z "$LP_SERVER_PID" ] && pool-port-busy "$LP_PORT"; then
    LP_SERVER_PID=$(ss -ltnpH "sport = :$LP_PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -n 1)
    echo "demo teardown: $LP_SERVER on port $LP_PORT: pid ${LP_SERVER_PID:-unknown}"
fi
if [ -n "$LP_SERVER_PID" ]; then
    # pool-cleanup releases, detaches and stops every server of POOL_SERVERS.
    POOL_SERVERS=()
    if [ -n "$POOL_SERVER_PID" ]; then
        POOL_SERVERS+=("$POOL_DEFAULT_NAME")
        POOL_SERVER_PIDS["$POOL_DEFAULT_NAME"]="$POOL_SERVER_PID"
        POOL_SERVER_PORTS["$POOL_DEFAULT_NAME"]="$POOL_PORT"
    fi
    POOL_SERVERS+=("$LP_SERVER")
    POOL_SERVER_PIDS["$LP_SERVER"]="$LP_SERVER_PID"
    POOL_SERVER_PORTS["$LP_SERVER"]="$LP_PORT"
fi
DRA_INSTALLED_VMS=("$VM2" "$VM1" "$POOL_VM0_DIR")

echo "### demo teardown: the trace"
if [ -n "$TRACE_DIR" ] && [ -f "$TRACE_DIR/pids" ]; then
    DRA_TRACE_DIR="$TRACE_DIR"
    DRA_TRACE_RUNNING=1
    DRA_TRACE_VMS=("${DEMO_VMS[@]}")
    dra-trace-stop
    dra-trace-summary > /dev/null
    echo "demo teardown: trace $TEST_OUTPUT_DIR/trace.txt, summary $TEST_OUTPUT_DIR/trace-summary.txt"
fi

echo "### demo teardown: the demo pods and leases"
vmdir-command "$VM2" "kubectl delete pods --all -n $DRA_NS --grace-period=2 --ignore-not-found --wait=true --timeout=60s 2>&1 | grep -v 'No resources found'; true"
# The claims of the wizard and of its setup are no pods' templates.
vmdir-command "$VM2" "kubectl delete resourceclaims,resourceclaimtemplates -n $DRA_NS -l 'app in (cxl-demo-wizard,cxl-demo-setup)' --ignore-not-found --wait=true --timeout=90s 2>&1 | grep -v 'No resources found'; true"
# shellcheck disable=SC2016 # retry-until evaluates the condition
retry-until --timeout 90 --interval 2 --message "no lease companions in $DRA_NS" \
    '[ -z "$(vmdir-command-q "$VM2" "kubectl get pods,resourceclaims -n $DRA_NS -l $DRA_LEASE_LABEL -o name 2>/dev/null")" ]' ||
    echo "demo teardown: WARNING: lease companions remain in $DRA_NS"

echo "### demo teardown: unconfigure the shared local device before detaching it"
local_attached=""
if [ -n "$POOL_SERVER_PID" ]; then
    dra-local-find "$VM2"
    [ "$DRA_LOCAL_ATTACHMENT" == detached ] || local_attached=1
fi
if [ -n "$local_attached" ] && [ "$DEMO_CASE" != wizard ]; then
    gone="by_serial($DRA_LOCAL_SERIAL) is None or not any(by_serial($DRA_LOCAL_SERIAL)['name'] in sorted(r['memdevs']) for r in regions)"
    pool-cxl-dump-json "$VM2" before-unconfigure
    if ! cxl-py assert "$gone" >/dev/null && dra-driver-pod "$VM2" >/dev/null; then
        helm_name="$DRA_HELM_RELEASE" helm-reconfigure "$DRA_CHART" "$DRA_HELPER_DIR/dra-driver-cxl.helm-values.yaml"
        vmdir-command "$VM2" "kubectl rollout status -n kube-system ds/$DRA_DAEMONSET --timeout=120s"
        # shellcheck disable=SC2016 # retry-until evaluates the condition
        retry-until --timeout 90 --interval 2 --message "the driver destroys the region of $DRA_LOCAL_SERIAL" \
            'pool-cxl-dump-json "$VM2" unconfigured && cxl-py assert "$gone" >/dev/null' ||
            echo "demo teardown: WARNING: the region of $DRA_LOCAL_SERIAL is still there after the restart without sharedMemdevs; the guest disable of dra-local-detach destroys it"
    fi
fi

echo "### demo teardown: the driver, the controller, the namespace"
dra-uninstall
if [ -n "$local_attached" ]; then
    dra-local-detach "$VM2"
fi
if [ "$DEMO_CASE" == wizard ] && [ -n "$POOL_SERVER_PID" ]; then
    # The wizard attached the (sliced) local device of VM1 too.
    dra-local-detach "$VM1"
fi
for vm in "${DEMO_VMS[@]}"; do
    dra-state-cleanup "$vm"
done

echo "### demo teardown: no filesystem mounts left in the VMs"
held=""
for vm in "${DEMO_VMS[@]}"; do
    label=$(vmdir-label "$vm")
    if ! dra-fs-mounts "$vm" >/dev/null; then
        held+="
  $label: cannot read its mounts"
    elif [ "$COMMAND_OUTPUT" != "[]" ]; then
        held+="
  $label: mounts $COMMAND_OUTPUT"
    fi
    # The node mounts of persistent local memdevs (lp0 of the wizard).
    if ! dra-fs-mounts "$vm" "$DRA_LOCAL_MOUNT_DIR" >/dev/null; then
        held+="
  $label: cannot read its local mounts"
    elif [ "$COMMAND_OUTPUT" != "[]" ]; then
        held+="
  $label: local mounts $COMMAND_OUTPUT"
    fi
    if ! dra-lease-fs-mounts "$vm" >/dev/null; then
        held+="
  $label: cannot read its lease mounts"
    elif [ "$COMMAND_OUTPUT" != "[]" ]; then
        held+="
  $label: lease mounts $COMMAND_OUTPUT"
    fi
    if ! loops=$(dra-pmem-loops "$vm"); then
        held+="
  $label: cannot list its loop devices"
    elif [ -n "$loops" ]; then
        held+="
  $label: loop devices over pmem: $(tr '\n' ' ' <<< "$loops")"
    fi
done
if [ -n "$held" ]; then
    # Releasing a region under a live filesystem can crash the guest.
    echo "demo teardown: WARNING: a pool device is still mounted, not releasing or detaching it:$held
  To clean up by hand in each VM above: stop what uses the mount (fuser -vm MOUNT),
  umount MOUNT (never umount -l; the lease mounts first), check that no /sys/block/loop*/loop/backing_file
  names the pmem device, then run demo teardown again."
    error "demo teardown: the pool device is still mounted, see the WARNING above"
fi

if [ -n "$LLM_MIRROR_PID" ] && grep -qa http.server "/proc/$LLM_MIRROR_PID/cmdline" 2>/dev/null; then
    echo "### demo teardown: the model mirror"
    kill "$LLM_MIRROR_PID" || error "cannot stop the model mirror, pid $LLM_MIRROR_PID"
fi

echo "### demo teardown: the pool server"
if [ -n "$POOL_SERVER_PID" ] || [ -n "$LP_SERVER_PID" ]; then
    pool-cleanup
fi
if [ -n "$POOL_DEVICE_FILES" ]; then
    # shellcheck disable=SC2086 # the files are separated by spaces
    rm -f $POOL_DEVICE_FILES || error "cannot remove $POOL_DEVICE_FILES"
    echo "demo teardown: removed $POOL_DEVICE_FILES"
fi
if [ -n "$DEMO_POOL_DIR" ] && [ "$(findmnt -n -o SOURCE --mountpoint "$DEMO_POOL_DIR")" == "$POOL_TMPFS_SOURCE" ]; then
    pool-tmpfs-umount "$DEMO_POOL_DIR" "$POOL_TMPFS_SOURCE" || error "cannot unmount the pool tmpfs at $DEMO_POOL_DIR, see above"
fi
if [ -n "$DEMO_POOL_DIR" ] && [ -d "$DEMO_POOL_DIR" ] && ! mountpoint -q "$DEMO_POOL_DIR"; then
    rmdir "$DEMO_POOL_DIR" || echo "demo teardown: $DEMO_POOL_DIR is not empty, left as it is"
fi
pool-port-busy && error "port $POOL_PORT is still busy: a fake-cxl-pool-server that the demo did not start? stop it"
pool-port-busy "$LP_PORT" && error "port $LP_PORT is still busy: a fake-cxl-pool-server $LP_SERVER that the demo did not start? stop it"
vmdir-command "$VM2" "kubectl label node $POOL_VM2_NAME $POOL_VM1_NAME $POOL_VM0_NAME cxl-demo/node- ; rm -rf /root/demo; true"

echo "### demo teardown: the VMs are clean"
for vm in "${DEMO_VMS[@]}"; do
    pool-cxl-dump "$vm" clean
    cxl-assert 'memdevs == [] and regions == []'
done
rm -f "$state.teardown"
echo "demo teardown: done"
