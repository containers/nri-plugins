# Test balloons with certain CPU c-states disabled

helm-terminate
helm_config=$TEST_DIR/balloons-cstates.cfg helm-launch balloons

# cpuids-of-container returns CPU ids a container is allowed to use, e.g. "1 2 4"
cpuids-of() {
    local ctr=$1 # e.g. pod0c0
    # return only cpu ids without zero-fill: replace cpu01 -> 1, cpu11 -> 11
    pyexec "for cpu in cpus['$ctr']: print(cpu.replace('cpu0','').replace('cpu',''))"
}

# verify-cstates checks the last writes to "disable" files in the
# override fs.
verify-cstates() {
    local cpu_ids=$1          # e.g. "1 2 4"
    local enabled_cstates=$2  # e.g. "C1E C2"
    local disabled_cstates=$3 # e.g. "C6 C8"
    local last_n_writes=$4    # expect the write within last N writes, e.g. 6

    vm-command "kubectl -n kube-system logs ds/nri-resource-policy-balloons | nl | grep 'cstates override fs: wrote' | tail -n $last_n_writes | nl"
    for cpu_id in $cpu_ids; do
        for cstate in $enabled_cstates; do
            echo "verify last write to cpu$cpu_id cstate=$cstate disable is 0"
            grep "cpu${cpu_id} cstate=$cstate disable=" <<< $COMMAND_OUTPUT | tail -n 1 | grep -q 'disable="0"' || {
                command-error "expected write 0 not found"
            }
        done
        for cstate in $disabled_cstates; do
            echo "verify last write to cpu$cpu_id cstate=$cstate disable is 1"
            grep "cpu${cpu_id} cstate=$cstate disable=" <<< $COMMAND_OUTPUT | tail -n 1 | grep -q 'disable="1"' || {
                command-error "expected write 1 not found"
            }
        done
    done
}

verify-sched() {
    local podXcY=$1
    vm-command "cat /proc/\$(pgrep -f $podXcY)/sched" || command-error "cannot get /proc/PID/sched for $podXcY"

    if [ "$expected_policy" != "" ]; then
        echo "verify scheduling policy of $podXcY is $expected_policy"
        grep -q -E "policy .* $expected_policy" <<< $COMMAND_OUTPUT ||
            error "expected policy $expected_policy not found"

    fi

    if [ "$expected_prio" != "" ]; then
        echo "verify scheduling priority of $podXcY is $expected_prio"
        grep -q -E "prio .* $expected_prio" <<< $COMMAND_OUTPUT ||
            error "expected priority $expected_prio not found"
    fi
}

# verify-cstates-no-writes checks that any c-states of given CPUs have not been written
verify-cstates-no-writes() {
    local cpu_ids=$1       # e.g. "1 2 4"
    local last_n_writes=$2 # e.g. 100
    echo "verify no writes to c-states of CPUs $cpu_ids"
    cpu_ids="(${cpu_ids// /|})"
    vm-command "kubectl -n kube-system logs ds/nri-resource-policy-balloons | nl | grep -E 'cstates override fs: wrote cpu${cpu_ids} cstate='"
    grep -q wrote <<< $COMMAND_OUTPUT && {
        command-error "writes to forbidden CPUs found"
    }
}

cleanup() {
    vm-command "kubectl delete pods --all --now"
}

echo "verify that all c-states of all available CPUs are enabled"
verify-cstates "2 3 4 5 6 7 11 12 13" "C1E C2 C4 C8" "" 40

echo "verify that c-states of CPUs outside AvailableResources have not been written"
verify-cstates-no-writes "0 1 8 9 14 15"

CPUREQ="750m" MEMREQ="100M" CPULIM="750m" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: lowlatency-bln" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod0c0"]) == 1'
echo "verify that CPUs of low-latency pod0 cannot enter C4 or C8"
verify-cstates "$(cpuids-of pod0c0)" "C1E C2" "C4 C8" 4
expected_policy=1 expected_prio=$((99 - 42)) verify-sched pod0c0 # expect SCHED_FIFO, prio 56

CPUREQ="3" MEMREQ="100M" CPULIM="" MEMLIM=""
POD_ANNOTATION=(
    "balloon.balloons.resource-policy.nri.io: lowlatency-bln"
    "scheduling-class.resource-policy.nri.io: run-when-free"
)
CONTCOUNT=1 create balloons-busybox
report allowed
verify 'cpus["pod0c0"] == cpus["pod1c0"]' \
       'len(cpus["pod0c0"]) == 4'
echo "verify that CPUs of low-latency pods pod0 and pod1 cannot enter C4 or C8"
verify-cstates "$(cpuids-of pod1c0)" "C1E C2" "C4 C8" 16

expected_policy=5 expected_prio=$((120 + 17)) verify-sched pod1c0 # expect SCHED_IDLE, prio 137
vm-command "ionice -p \$(pgrep -f pod1c0)" ||
    command-error "cannot get ionice for pod1c0"
expected_ionice="best-effort: prio 6"
[[ "$COMMAND_OUTPUT" == "$expected_ionice" ]] ||
    command-error "expected ionice output '$expected_ionice'"

# store CPU ids of maximal cpuset before deleting pods
max_lowlatency_cpus="$(echo $(cpuids-of pod1c0) )"

vm-command 'kubectl delete pod pod1'
report allowed
verify 'len(cpus["pod0c0"]) == 1'

# spaces around each id helps ensuring grep " 1 " never matches cpu 11 but always matches cpu 1
pod0cpus=" $(echo $(cpuids-of pod0c0) ) "

echo "verify that c-states of freed CPUs are enabled again after balloon was deflated"
freed_cpus=""
for cpu_id in $max_lowlatency_cpus; do
    grep -q " $cpu_id " <<< $pod0cpus || freed_cpus+=" $cpu_id"
done
echo "verify that all c-states of freed CPUs $freed_cpus (= {$max_lowlatency_cpus} - {$pod0cpus}) are enabled after the balloon got deflated"
verify-cstates "$freed_cpus" "C1E C2 C4 C8" "" 24

echo "verify that c-states of the remaining CPU $(cpuids-of pod0c0) are still configured for low-latency"
verify-cstates "$(cpuids-of pod0c0)" "C1E C2" "C4 C8" 16

vm-command 'kubectl delete pod pod0'
report allowed

echo "verify that after all containers are gone and their destroyed, c-states of all used CPUs are enabled again"
verify-cstates "$max_lowlatency_cpus" "C1E C2 C4 C8" "" 32
