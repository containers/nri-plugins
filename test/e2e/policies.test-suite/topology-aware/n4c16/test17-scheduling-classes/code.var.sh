cleanup() {
    vm-command "kubectl delete pods --all --now"
    vm-command "kubectl delete namespaces highprio lowprio --now --ignore-not-found"
}

verify-sched() {
    local podXcY=$1
    vm-command "cat /proc/\$(pgrep -f $podXcY)/sched | grep -E '^((policy)|(prio))'" || command-error "cannot get /proc/PID/sched for $podXcY"

    if [ "$expected_policy" != "" ]; then
        echo "verify scheduling policy of $podXcY is $expected_policy"
        grep -q -E "policy .* $expected_policy" <<< $COMMAND_OUTPUT ||
            error "expected policy $expected_policy not found"
    else
        error "missing verify-sched expected_policy for $podXcY"
    fi

    if [ "$expected_prio" != "" ]; then
        echo "verify scheduling priority of $podXcY is $expected_prio"
        grep -q -E "prio .* $expected_prio" <<< $COMMAND_OUTPUT ||
            error "expected priority $expected_prio not found"
    else
        error "missing verify-sched expected_prio for $podXcY"
    fi
}

SCHED_OTHER=0
SCHED_FIFO=1
SCHED_BATCH=3
SCHED_ISO=4
SCHED_IDLE=5
SCHED_DEADLINE=6

SCHEDULING_CLASSES="[
    { name: realtime, policy: fifo,  priority: 42 },
    { name: highprio, policy: fifo,  priority: 10 },
    { name: default,  policy: other, nice:  0 },
    { name: lowprio,  policy: other, nice: 10 },
    { name: idle,     policy: idle,  nice: 17, ioClass: be, ioPriority: 6 }]"
NAMESPACE_SCHEDULING_CLASSES="{ highprio: highprio, lowprio: lowprio }"
PODQOS_SCHEDULING_CLASSES="{ BestEffort: lowprio, Burstable: default, Guaranteed: highprio }"

cleanup
helm-terminate

helm_config=$(COLOCATE_PODS=false \
              SCHEDULING_CLASSES="$SCHEDULING_CLASSES" \
              NAMESPACE_SCHEDULING_CLASSES="$NAMESPACE_SCHEDULING_CLASSES" \
              PODQOS_SCHEDULING_CLASSES="$PODQOS_SCHEDULING_CLASSES" \
    instantiate helm-config.yaml) helm-launch topology-aware

#
# Tests for explicitly annotated namespaces.
#

# Annotated scheduling class for Guaranteed QoS containers with exclusive CPU allocation.
CPU=1 MEM=100M \
    ANN0="scheduling-class.resource-policy.nri.io/container.pod0c0: realtime" \
    ANN1="scheduling-class.resource-policy.nri.io: idle" \
        CONTCOUNT=2 create guaranteed
report allowed

verify 'len(cpus["pod0c0"]) == 1'
expected_policy=$SCHED_FIFO expected_prio=$((99 - 42)) verify-sched pod0c0

verify 'len(cpus["pod0c1"]) == 1'
expected_policy=$SCHED_IDLE expected_prio=$((120 + 17)) verify-sched pod0c1

# Annotated scheduling class for Guaranteed QoS containers without exclusive CPU allocation.
CPU=750m MEM=100M \
    ANN0="scheduling-class.resource-policy.nri.io/container.pod1c0: highprio" \
    ANN1="scheduling-class.resource-policy.nri.io: idle" \
        CONTCOUNT=2 create guaranteed
report allowed

verify 'len(cpus["pod1c0"]) != 1'
expected_policy=$SCHED_FIFO expected_prio=$((99 - 10)) verify-sched pod1c0

verify 'len(cpus["pod1c1"]) != 1'
expected_policy=$SCHED_IDLE expected_prio=$((120 + 17)) verify-sched pod1c1

# Annotated scheduling class on Burstable QoS containers.
CPUREQ=250m CPULIM=750m MEM=100M \
    ANN0="scheduling-class.resource-policy.nri.io/container.pod2c0: highprio" \
    ANN1="scheduling-class.resource-policy.nri.io: lowprio" \
        CONTCOUNT=2 create burstable

verify 'len(cpus["pod2c0"]) != 1'
expected_policy=$SCHED_FIFO expected_prio=$((99 - 10)) verify-sched pod2c0

verify 'len(cpus["pod2c1"]) != 1'
expected_policy=$SCHED_OTHER expected_prio=$((120 + 10)) verify-sched pod2c1

# Annotated scheduling class on BestEffort QoS containers.
ANN0="scheduling-class.resource-policy.nri.io/container.pod3c0: lowprio" \
ANN1="scheduling-class.resource-policy.nri.io: idle" \
        CONTCOUNT=2 create besteffort

verify 'len(cpus["pod3c0"]) != 1'
expected_policy=$SCHED_OTHER expected_prio=$((120 + 10)) verify-sched pod3c0

verify 'len(cpus["pod3c1"]) != 1'
expected_policy=$SCHED_IDLE expected_prio=$((120 + 17)) verify-sched pod3c1

vm-command "kubectl delete pods --all --now"

#
# Tests for namespace default scheduling classes.
#

# First in a namespace with default highprio scheduling class.
vm-command "kubectl create namespace highprio"
ANN0="scheduling-class.resource-policy.nri.io/container.pod4c0: lowprio" \
        CONTCOUNT=2 namespace=highprio create burstable

verify 'len(cpus["pod4c0"]) != 1'
expected_policy=$SCHED_OTHER expected_prio=$((120 + 10)) verify-sched pod4c0

verify 'len(cpus["pod4c1"]) != 1'
expected_policy=$SCHED_FIFO expected_prio=$((99 - 10)) verify-sched pod4c1

# Then in a namespace with default lowprio scheduling class.
vm-command "kubectl create namespace lowprio"
ANN0="scheduling-class.resource-policy.nri.io/container.pod5c0: highprio" \
        CONTCOUNT=2 namespace=lowprio create besteffort

verify 'len(cpus["pod5c0"]) != 1'
expected_policy=$SCHED_FIFO expected_prio=$((99 - 10)) verify-sched pod5c0

verify 'len(cpus["pod5c1"]) != 1'
expected_policy=$SCHED_OTHER expected_prio=$((120 + 10)) verify-sched pod5c1

vm-command "kubectl delete pods --all --now"

#
# Tests for PodQoS default scheduling classes.
#

# Default scheduling class for BestEffort QoS containers.
ANN0="scheduling-class.resource-policy.nri.io/container.pod6c0: idle" \
        CONTCOUNT=2 create besteffort

verify 'len(cpus["pod6c0"]) != 1'
expected_policy=$SCHED_IDLE expected_prio=$((120 + 17)) verify-sched pod6c0

verify 'len(cpus["pod6c1"]) != 1'
expected_policy=$SCHED_OTHER expected_prio=$((120 + 10)) verify-sched pod6c1


# Default scheduling class for Burstable QoS containers.
CPUREQ=250m CPULIM=750m \
    ANN0="scheduling-class.resource-policy.nri.io/container.pod7c0: lowprio" \
        CONTCOUNT=2 create burstable

verify 'len(cpus["pod7c0"]) != 1'
expected_policy=$SCHED_OTHER expected_prio=$((120 + 10)) verify-sched pod7c0

verify 'len(cpus["pod7c1"]) != 1'
expected_policy=$SCHED_OTHER expected_prio=$((120 + 0)) verify-sched pod7c1

# Default scheduling class for Guaranteed QoS containers.
CPU=250m \
    ANN0="scheduling-class.resource-policy.nri.io/container.pod8c0: realtime" \
        CONTCOUNT=2 create guaranteed

verify 'len(cpus["pod8c0"]) != 1'
expected_policy=$SCHED_FIFO expected_prio=$((99 - 42)) verify-sched pod8c0

verify 'len(cpus["pod8c1"]) != 1'
expected_policy=$SCHED_FIFO expected_prio=$((99 - 10)) verify-sched pod8c1

cleanup
