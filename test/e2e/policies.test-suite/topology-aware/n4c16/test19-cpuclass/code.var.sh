cleanup() {
    delete-pods --all
    helm-terminate
}

# Restrict the log assertions to the cpuclass-related log lines.
plugin_log_filter='   *cpuclass   *'

# get-ext <resource-name> stores the given extended resource
# capacity (or the string "missing") in COMMAND_OUTPUT.
get-ext() {
    local name=$1
    vm-command "kubectl get nodes -o json | jq -r '.items[] | (.status.capacity[\"$name\"] // \"missing\")'"
}

# get-ext-exclusive stores the exclusive CPU class extended resource capacity (or the
# string "missing") in COMMAND_OUTPUT.
get-ext-exclusive() {
    get-ext "cpuclass.resource-policy.nri.io/exclusive"
}

# wait-ext-exclusive <want> <message> [timeout=30] [interval=2]
# Polls until the exclusive CPU class extended resource equals <want> (a number
# or the string "missing"), or fails with <message> on timeout.
wait-ext-exclusive() {
    local want=$1 msg=$2 timeout=${3:-30} interval=${4:-2} elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        get-ext-exclusive
        [ "$COMMAND_OUTPUT" == "$want" ] && return 0
        sleep "$interval"
        elapsed=$((elapsed + interval))
    done
    get-ext-exclusive
    command-error "$msg (expected '$want', got '$COMMAND_OUTPUT')"
}

# ctr-cpu-ids <pod> <container>
# Inspect the container of the given pod and report the cpuset it is pinned to.
ctr-cpu-ids() {
    local pod=$1 ctr=$2
    local result=""
    $SSH -oConnectTimeout=1 node \
         "kubectl exec $pod -c $ctr -- grep Cpus_allowed_list /proc/1/status" | \
        tr -d '\t ' | cut -d ':' -f 2
}

# assert-cpu-clos <container> <cpus> <clos-id>
# Polls the log until a default timeout to verify that the given CPUs are
# associated to the given CLOS.
assert-cpu-clos() {
    local ctr="$1" cpus="$2" clos="$3"
    wait-assert-log-contains "associated cpus $cpus to $clos" \
        "Missing CPU ($cpus) association for $ctr (expected to $clos)"
}

# assert-cpu-freq <container> <cpus> <clos-id>
# Polls the log until a default timeout to verify that the given CPU's
# frequency is reprogrammed according to the given class.
assert-cpu-freq() {
    local user="$1" cpus="$2" class="$3"
    local cpu=""

    for cpu in $(expand-cpulist "$cpus"); do
        wait-assert-log-contains "enforcing cpu frequency from class .$class@.* on cpu $cpu" \
             "Missing CPU frequency class $class enforcement on cpu $cpu for $user"
    done
}

# expand-cpulist "0-2,5" prints "0 1 2 5"
expand-cpulist() {
    local cpus="$1"

    if [ "${cpus//-/}" == "$cpus" ] && [ "${cpus//,/}" == "$cpus" ]; then
        echo $cpus
        return 0
    fi

    python3 -c '
import sys
r = set()
for part in sys.argv[1].split(","):
    if not part:
        continue
    if "-" in part:
        a, b = part.split("-")
        r.update(range(int(a), int(b) + 1))
    else:
        r.add(int(part))
print(" ".join(str(x) for x in sorted(r)))
' "$cpus"
}

OVERRIDE_SYS_CPUFREQ='[{"cpus": "0-15", "base": 2900000, "min": 800000, "max": 3800000}]'
OVERRIDE_SST='{"supported": true, "clos_count": 4, "packages": [{"id": 0, "cpus": "0-7", "tf_supported": true, "cp_supported": true, "max_hp_cpus": 2}, {"id": 1, "cpus": "8-15", "tf_supported": true, "cp_supported": true, "max_hp_cpus": 2}]}'
OVERRIDE_SST_STATE_DIR="/tmp/nri-pct-mock"

CPU_CLASSES="[
  { name: shared   , minFreq: min  , maxFreq: base , pctPriority: low },
  { name: reserved , minFreq: min  , maxFreq: turbo },
  { name: exclusive, minFreq: turbo, maxFreq: turbo, pctPriority: high, publishExtendedResource: true },
  { name: class1   , minFreq: min  , maxFreq: base },
  { name: class2   , minFreq: min  , maxFreq: base } ]"
DEFAULT_EXCLUSIVE_CPUCLASS="exclusive"
SHARED_CPUCLASS="shared"
RESERVED_CPUCLASS="reserved"
DEBUG_LOGGERS="agent cpu cpuclass"

cleanup

helm_config=$(COLOCATE_PODS=false \
              DEBUG_LOGGERS="$DEBUG_LOGGERS" \
              CPU_CLASSES="$CPU_CLASSES" \
              SHARED_CPUCLASS="$SHARED_CPUCLASS" \
              RESERVED_CPUCLASS="$RESERVED_CPUCLASS" \
              DEFAULT_EXCLUSIVE_CPUCLASS="$DEFAULT_EXCLUSIVE_CPUCLASS" \
              EXTRA_ENV_OVERRIDE_SYS_CPUFREQ="$OVERRIDE_SYS_CPUFREQ" \
              EXTRA_ENV_OVERRIDE_SST="$OVERRIDE_SST" \
              EXTRA_ENV_OVERRIDE_SST_STATE_DIR="$OVERRIDE_SST_STATE_DIR" \
    instantiate helm-config.yaml) helm-launch topology-aware

# Verify cpuclass-related initialization during startup.
wait-assert-log-contains 'PrepareManagedMode done' "managed mode startup missing"
wait-assert-log-contains 'ConfigureClos.*ClosID:0 MinFreq:3800000 MaxFreq:3800000' "HP CLOS 0 not programmed with MinFreq=MaxFreq=turbo (3800000)"
wait-assert-log-contains 'ConfigureClos.*ClosID:3 MinFreq:800000 MaxFreq:2900000' "LP CLOS 3 not programmed with MinFreq=min (800000) MaxFreq=base (2900000)"
wait-assert-log-contains 'EnableCP done' "EnableCP missing"
wait-ext-exclusive 4 "expected 4 PCT HP CPUs published as extended resources"

#
# Reserved pool CPU class assignment
#

cpu0=0
assert-cpu-freq "reserved pool" $cpu0 reserved

#
# Default exclusive CPU class assignment
#

# Create pod with 4 containers eligible for exclusive CPU allocation.
# Verify that each exclusive CPU gets assigned to the default CPU class
# which is configured with high PCT priority.
CONTCOUNT=4 CPU=1 create guaranteed

pod=pod0
cpu0=$(ctr-cpu-ids $pod ${pod}c0)
assert-cpu-clos ${pod}c0 $cpu0 "CLOS 0"
cpu1=$(ctr-cpu-ids $pod ${pod}c1)
assert-cpu-clos ${pod}c1 $cpu1 "CLOS 0"
cpu2=$(ctr-cpu-ids $pod ${pod}c2)
assert-cpu-clos ${pod}c2 $cpu2 "CLOS 0"
cpu3=$(ctr-cpu-ids $pod ${pod}c3)
assert-cpu-clos ${pod}c3 $cpu3 "CLOS 0"

# Delete pod. Verify that each released exclusive CPU gets assigned to
# the shared pool CPU class which is configured with low PCT priority.
vm-command "kubectl delete pod $pod"
assert-cpu-clos ${pod}c0 $cpu0 "CLOS 3"
assert-cpu-clos ${pod}c1 $cpu1 "CLOS 3"
assert-cpu-clos ${pod}c2 $cpu2 "CLOS 3"
assert-cpu-clos ${pod}c3 $cpu3 "CLOS 3"

#
# Container-specific class assignment
#

# Create pod with 4 containers eligible for exclusive CPU allocation,
# 2 of them annotated to have non-default CPU classes. Verify that
# CPUs of the annotated containers get assigned to their respective
# classes and the rest to the default class.
pod=pod1
ANN0="cpu-class.resource-policy.nri.io/container.${pod}c0: class1" \
ANN1="cpu-class.resource-policy.nri.io/container.${pod}c1: class2" \
    CONTCOUNT=4 CPU=2 create guaranteed

cpu0=$(ctr-cpu-ids $pod ${pod}c0)
assert-cpu-freq ${pod}c0 $cpu0 class1
cpu1=$(ctr-cpu-ids $pod ${pod}c1)
assert-cpu-freq ${pod}c1 $cpu1 class2
cpu2=$(ctr-cpu-ids $pod ${pod}c2)
assert-cpu-clos ${pod}c2 $cpu2 "CLOS 0"
cpu3=$(ctr-cpu-ids $pod ${pod}c3)
assert-cpu-clos ${pod}c3 $cpu3 "CLOS 0"

# Delete pod. Verify that each released CPU get assigned to
# the shared pool CPU class.
vm-command "kubectl delete pod $pod"
assert-cpu-clos ${pod}c0 $cpu0 "CLOS 3"
assert-cpu-clos ${pod}c1 $cpu1 "CLOS 3"
assert-cpu-clos ${pod}c2 $cpu2 "CLOS 3"
assert-cpu-clos ${pod}c3 $cpu3 "CLOS 3"

#
# Non-eligible container to a CPU class assignment
#

# Burstable Qos Class

pod=pod2
ANN0="cpu-class.resource-policy.nri.io/container.${pod}c0: class1" \
    wait="" CONTCOUNT=1 create burstable

verify-container-error $pod ${pod}c0 "CPU class .* invalid for non-Guaranteed QoS class"

# BestEffort QoS Class

pod=pod3
ANN0="cpu-class.resource-policy.nri.io/container.${pod}c0: class1" \
    wait="" CONTCOUNT=1 create besteffort

verify-container-error $pod ${pod}c0 "CPU class .* invalid for non-Guaranteed QoS class"

# Guaranteed QoS Class Without Exclusive CPU allocation

pod=pod4
ANN0="cpu-class.resource-policy.nri.io/container.${pod}c0: class1" \
    wait="" CPU=250m CONTCOUNT=1 create guaranteed

verify-container-error $pod ${pod}c0 "CPU class .* invalid without exclusive CPUs"

cleanup

#
# Configuration validation
#

# Non-existent reserved pool CPU class
RESERVED_CPUCLASS=nonexistent
helm_config=$(COLOCATE_PODS=false \
              DEBUG_LOGGERS="$DEBUG_LOGGERS" \
              CPU_CLASSES="$CPU_CLASSES" \
              SHARED_CPUCLASS="$SHARED_CPUCLASS" \
              RESERVED_CPUCLASS="$RESERVED_CPUCLASS" \
              DEFAULT_EXCLUSIVE_CPUCLASS="$DEFAULT_EXCLUSIVE_CPUCLASS" \
              EXTRA_ENV_OVERRIDE_SYS_CPUFREQ="$OVERRIDE_SYS_CPUFREQ" \
              EXTRA_ENV_OVERRIDE_SST="$OVERRIDE_SST" \
              EXTRA_ENV_OVERRIDE_SST_STATE_DIR="$OVERRIDE_SST_STATE_DIR" \
    instantiate helm-config.yaml) launch_timeout=5s expect_error=1 helm-launch topology-aware

vm-command "kubectl -n kube-system get topologyawarepolicies.config.nri/default -ojson | jq '.status.nodes[].errors'"

grep -q 'unknown reserved CPU class \\"nonexistent\\"' <<<$COMMAND_OUTPUT ||
    error "Missing reserved pool CPU class validation error in configuration CR"

RESERVED_CPUCLASS=reserved
helm-terminate

# Non-existent shared pool CPU class
RESERVED_CPUCLASS=reserved
SHARED_CPUCLASS=nonexistent
helm_config=$(COLOCATE_PODS=false \
              DEBUG_LOGGERS="$DEBUG_LOGGERS" \
              CPU_CLASSES="$CPU_CLASSES" \
              SHARED_CPUCLASS="$SHARED_CPUCLASS" \
              RESERVED_CPUCLASS="$RESERVED_CPUCLASS" \
              DEFAULT_EXCLUSIVE_CPUCLASS="$DEFAULT_EXCLUSIVE_CPUCLASS" \
              EXTRA_ENV_OVERRIDE_SYS_CPUFREQ="$OVERRIDE_SYS_CPUFREQ" \
              EXTRA_ENV_OVERRIDE_SST="$OVERRIDE_SST" \
              EXTRA_ENV_OVERRIDE_SST_STATE_DIR="$OVERRIDE_SST_STATE_DIR" \
    instantiate helm-config.yaml) launch_timeout=5s expect_error=1 helm-launch topology-aware

vm-command "kubectl -n kube-system get topologyawarepolicies.config.nri/default -ojson | jq '.status.nodes[].errors'"

grep -q 'unknown shared CPU class \\"nonexistent\\"' <<<$COMMAND_OUTPUT ||
    error "Missing shared pool CPU class validation error in configuration CR"

SHARED_CPUCLASS=shared
helm-terminate

# Non-existent default guaranteed CPU class
DEFAULT_EXCLUSIVE_CPUCLASS="nonexistent"

helm_config=$(COLOCATE_PODS=false \
              DEBUG_LOGGERS="$DEBUG_LOGGERS" \
              CPU_CLASSES="$CPU_CLASSES" \
              SHARED_CPUCLASS="$SHARED_CPUCLASS" \
              RESERVED_CPUCLASS="$RESERVED_CPUCLASS" \
              DEFAULT_EXCLUSIVE_CPUCLASS="$DEFAULT_EXCLUSIVE_CPUCLASS" \
              EXTRA_ENV_OVERRIDE_SYS_CPUFREQ="$OVERRIDE_SYS_CPUFREQ" \
              EXTRA_ENV_OVERRIDE_SST="$OVERRIDE_SST" \
              EXTRA_ENV_OVERRIDE_SST_STATE_DIR="$OVERRIDE_SST_STATE_DIR" \
    instantiate helm-config.yaml) launch_timeout=5s expect_error=1 helm-launch topology-aware

vm-command "kubectl -n kube-system get topologyawarepolicies.config.nri/default -ojson | jq '.status.nodes[].errors'"

grep -q 'unknown default exclusive CPU class \\"nonexistent\\"' <<<$COMMAND_OUTPUT ||
    error "Missing default exclusive CPU class validation error in configuration CR"

cleanup
