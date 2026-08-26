# test20-dra: DRA (KEP-5075/KEP-5517) integration for the topology-aware
# policy. See docs/dra/plan.md's Step 10 for background.
#
# This is the initial skeleton (plan Task 3): mock+launch, feature-gate
# probe, and skip path only. The ResourceClaim/pod manifests and the
# remaining assertions (CLOS association, env vars, allocatable
# deduction) are added by later tasks.

cleanup() {
    vm-command "kubectl delete pods --all --now"
    vm-command "kubectl delete resourceclaims --all --now"
    helm-terminate
}

# fetch-log fetches the latest log lines matching a given pattern.
# Copied verbatim from test19-cpuclass/code.var.sh (test-local
# convention -- already duplicated in balloons/n4c16/test19-pct, not
# worth extracting to a shared lib for a third copy).
fetch-log() {
    local last_n=${1:-200} pattern=${2:-'   *cpuclass   *'}
    vm-command "kubectl -n kube-system logs ds/nri-resource-policy-topology-aware | grep -E \"$pattern\" | tail -n $last_n"
}

# assert-log-contains <regex> <message>
assert-log-contains() {
    local pat=$1
    local msg=$2
    fetch-log 500
    grep -E -q "$pat" <<< "$COMMAND_OUTPUT" || command-error "$msg (pattern: $pat)"
}

# wait-assert-log-contains <regex> <message> [timeout=5]
# Polls the policy log every 1s until <regex> matches or <timeout>
# seconds pass. On timeout, defers to assert-log-contains so the
# resulting command-error carries the captured log output.
wait-assert-log-contains() {
    local pat=$1
    local msg=$2
    local timeout=${3:-5}
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        fetch-log 500
        grep -E -q "$pat" <<< "$COMMAND_OUTPUT" && return 0
        sleep 1
        elapsed=$((elapsed + 1))
    done
    assert-log-contains "$pat" "$msg"
}

# assert-cpu-clos <container> <cpus> <clos-id>
# Polls the log until a default timeout to verify that the given CPUs are
# associated to the given CLOS.
assert-cpu-clos() {
    local ctr="$1" cpus="$2" clos="$3"
    wait-assert-log-contains "associated cpus $cpus to $clos" \
        "Missing CPU ($cpus) association for $ctr (expected to $clos)"
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

# compress-cpulist "8 9 10 12" prints "8-10,12" -- the inverse of
# expand-cpulist above, same python3 idiom. Needed because pct.go:786's
# "associated cpus %s to CLOS %d" log line formats its cpuset with
# cpuset.CPUSet.String(), which collapses contiguous ids into ranges,
# so individual/comma-joined ids parsed out of NRI_CPU<N> env vars
# would never match assert-cpu-clos's regex without this.
compress-cpulist() {
    local cpus="$1"

    python3 -c '
import sys
ids = sorted(int(x) for x in sys.argv[1].split())
ranges = []
start = prev = None
for i in ids:
    if start is None:
        start = prev = i
    elif i == prev + 1:
        prev = i
    else:
        ranges.append((start, prev))
        start = prev = i
if start is not None:
    ranges.append((start, prev))
print(",".join(str(a) if a == b else "%d-%d" % (a, b) for a, b in ranges))
' "$cpus"
}

# node-allocatable-cpu-millis stores node.status.allocatable.cpu,
# normalized to millicores, in COMMAND_OUTPUT. The raw resource.Quantity
# value returned by the API can be plain cores ("16") or already in
# millicore form ("16000m") depending on canonicalization, so normalize
# before comparing -- a fragile string diff of "16" vs "16000m" would
# never catch a real deduction.
node-allocatable-cpu-millis() {
    vm-command "kubectl get nodes -o jsonpath='{.items[0].status.allocatable.cpu}'"
    local raw="$COMMAND_OUTPUT"
    if [[ "$raw" == *m ]]; then
        COMMAND_OUTPUT="${raw%m}"
    else
        COMMAND_OUTPUT=$((raw * 1000))
    fi
}

# wait-allocatable-cpu-millis <want-millis> <message> [timeout=30] [interval=2]
# Polls node.status.allocatable.cpu (normalized to millicores) until it
# equals <want>, or fails with <message> on timeout -- allows for
# propagation delay between claim allocation and the node status update.
wait-allocatable-cpu-millis() {
    local want=$1 msg=$2 timeout=${3:-30} interval=${4:-2} elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        node-allocatable-cpu-millis
        [ "$COMMAND_OUTPUT" == "$want" ] && return 0
        sleep "$interval"
        elapsed=$((elapsed + interval))
    done
    node-allocatable-cpu-millis
    command-error "$msg (expected '$want', got '$COMMAND_OUTPUT')"
}

# wait-resourceslice-devices [timeout]
# Polls until the DRA driver's published ResourceSlice(s) report at
# least one device, storing a compact JSON array of
# {allowMultipleAllocations, nodeAllocatableResourceMappings} objects
# (one per device) in COMMAND_OUTPUT. The driver needs a moment after
# helm-launch to publish its ResourceSlice, so this can't be a single
# one-shot check.
wait-resourceslice-devices() {
    local timeout=${1:-30} elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        vm-command "kubectl get resourceslices -o json | \
            jq -c '[.items[].spec.devices[] | {allowMultipleAllocations, nodeAllocatableResourceMappings}]'"
        if [ -n "$COMMAND_OUTPUT" ] && [ "$COMMAND_OUTPUT" != "[]" ]; then
            return 0
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    return 1
}

OVERRIDE_SYS_CPUFREQ='[{"cpus": "0-15", "base": 2900000, "min": 800000, "max": 3800000}]'
OVERRIDE_SST='{"supported": true, "clos_count": 4, "packages": [{"id": 0, "cpus": "0-7", "tf_supported": true, "cp_supported": true, "max_hp_cpus": 2}, {"id": 1, "cpus": "8-15", "tf_supported": true, "cp_supported": true, "max_hp_cpus": 2}]}'
OVERRIDE_SST_STATE_DIR="/tmp/nri-pct-mock"

# NOTE: the plan's literal CPU_CLASSES sketch only lists hp-turbo, but
# Config.Validate() (pkg/apis/config/v1alpha1/resmgr/policy/topologyaware/config.go:270-271)
# hard-requires a shared CPU class whenever cpuClasses is non-empty, or
# helm-launch fails outright ("shared CPU class not specified"). Adding
# a plain (non-PCT) "shared" class and pointing SHARED_CPUCLASS at it
# is the minimal fix -- see plan Task 3 [deviation] note.
CPU_CLASSES="[
  { name: hp-turbo, pctPriority: high, pctMinFreq: turbo, pctMaxFreq: turbo },
  { name: shared   , minFreq: min, maxFreq: base } ]"
SHARED_CPUCLASS="shared"
DEBUG_LOGGERS="agent cpu cpuclass"
DRA_ENABLED=true

cleanup

helm_config=$(COLOCATE_PODS=false \
              DEBUG_LOGGERS="$DEBUG_LOGGERS" \
              CPU_CLASSES="$CPU_CLASSES" \
              SHARED_CPUCLASS="$SHARED_CPUCLASS" \
              DRA_ENABLED="$DRA_ENABLED" \
              EXTRA_ENV_OVERRIDE_SYS_CPUFREQ="$OVERRIDE_SYS_CPUFREQ" \
              EXTRA_ENV_OVERRIDE_SST="$OVERRIDE_SST" \
              EXTRA_ENV_OVERRIDE_SST_STATE_DIR="$OVERRIDE_SST_STATE_DIR" \
    instantiate helm-config.yaml) helm-launch topology-aware

#
# Feature-gate probe.
#
# The driver (pkg/resmgr/cpuclass/dra.go:279-281) sets
# AllowMultipleAllocations (KEP-5075) and NodeAllocatableResourceMappings
# (KEP-5517) unconditionally on every device it publishes. If the API
# server's alpha gates DRAConsumableCapacity/DRANodeAllocatableResources
# are off, it silently strips both fields on write, so they're absent
# on read back here. This requires the plugin to already be running
# (hence it happens after helm-launch, not before), and with the
# hp-turbo class + SST mock active above so there's at least one real
# device to read back (an empty CPU_CLASSES publishes zero devices).
wait-resourceslice-devices 30 || {
    helm-terminate
    error "no devices found in any ResourceSlice within timeout (not a feature-gate issue -- check DRA_ENABLED/CPU_CLASSES/SST mock config)"
}

if grep -q '"allowMultipleAllocations":true' <<< "$COMMAND_OUTPUT" && \
   grep -q '"nodeAllocatableResourceMappings":{' <<< "$COMMAND_OUTPUT"; then
    echo "DRA feature gates (KEP-5075/KEP-5517) detected as enabled on the API server; continuing."
else
    helm-terminate
    echo "Test verdict: SKIP (KEP-5075/KEP-5517 feature gate missing)"
    exit 0
fi

#
# Claim + pod (Task 4).
#
# A ResourceClaim selecting the HP cpuClass by its published
# nri/pctPriority attribute, additionally pinned to packageID == 1.
# With test19-cpuclass's mock SST layout (package 0 = CPUs 0-7,
# package 1 = CPUs 8-15, max_hp_cpus: 2 each) an unconstrained claim
# could resolve to package 0 and collide with CPU 0, the reserved-pool
# CPU by convention -- PickHpCpus (pct.go:611-652) doesn't exclude the
# reserved pool, only already-claimed/held CPUs. Pinning to package 1
# keeps the claim's 2 CPUs disjoint from the reserved pool, so the
# CLOS-association assertion (Task 5) isn't flaky.
#
# Only "objects create cleanly and the pod reaches Running" is checked
# here; CLOS-association/env-var/allocatable assertions are Task 5.
#

# Bypassing create() also bypasses its image pre-pull step -- pull the
# pod's image explicitly so a freshly created VM doesn't stall/fail on
# kubectl's own on-demand pull.
vm-command "crictl -i unix://${k8scri_sock} pull quay.io/prometheus/busybox" ||
    command-error "failed to pre-pull quay.io/prometheus/busybox"

# Manifests are written to local temp files and copied to the VM with
# vm-put-file, then kubectl apply'd -- this sidesteps vm-command's
# double-quoting/escaping entirely (the YAML below needs literal
# double quotes for its CEL string literals and the "2" capacity
# request), which proved far less fiddly than embedding a
# `cat <<EOF | kubectl apply -f -` heredoc inside a double-quoted
# vm-command argument.
claim_yaml=$(mktemp)
cat <<'EOF' > "$claim_yaml"
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: hp-turbo-cpus
spec:
  devices:
    requests:
    - name: cpus
      exactly:
        deviceClassName: nri.topology-aware.cpu
        capacity:
          requests:
            nri/cpus: "2"
        selectors:
        - cel:
            expression: |
              device.attributes["nri"].pctPriority == "high" && device.attributes["nri"].packageID == 1
EOF
vm-put-file --cleanup "$claim_yaml" hp-turbo-cpus-claim.yaml
vm-command "kubectl apply -f hp-turbo-cpus-claim.yaml" ||
    command-error "failed to create ResourceClaim hp-turbo-cpus"

# Capture the node-allocatable CPU baseline here, in the gap between
# the claim and the pod -- before the pod exists and consumes the
# claim. A baseline captured after the pod reaches Ready (as a quick,
# non-rigorous check during Task 4 did) can't tell a real deduction
# apart from "never happened", since there's nothing to diff against.
node-allocatable-cpu-millis
allocatable_baseline_millis="$COMMAND_OUTPUT"

# The consuming pod. Not routed through create(): create() defaults to
# wait=Ready, which a ResourceClaim never satisfies on its own (it has
# no Ready condition), and guaranteed.yaml.in (the shared template
# create() uses, shared by ~23 other tests) has no
# resourceClaims/resources.claims support -- not modified here.
pod_yaml=$(mktemp)
cat <<'EOF' > "$pod_yaml"
apiVersion: v1
kind: Pod
metadata:
  name: dra-pod0
spec:
  resourceClaims:
  - name: cpus
    resourceClaimName: hp-turbo-cpus
  containers:
  - name: dra-pod0c0
    image: quay.io/prometheus/busybox
    imagePullPolicy: IfNotPresent
    command:
      - sh
      - -c
      - echo dra-pod0c0 $(sleep inf)
    resources:
      claims:
      - name: cpus
      requests:
        cpu: "1"
        memory: "100M"
      limits:
        cpu: "1"
        memory: "100M"
  terminationGracePeriodSeconds: 1
EOF
vm-put-file --cleanup "$pod_yaml" dra-pod0.yaml
vm-command "kubectl apply -f dra-pod0.yaml" ||
    command-error "failed to create pod dra-pod0"

vm-command "kubectl wait --for=condition=Ready pod/dra-pod0 --timeout=60s" ||
    command-error "pod dra-pod0 did not reach Running"

#
# Task 5: CLOS association, env vars, allocatable deduction, cleanup.
#

# Env vars: CDI writes NRI_CLASS/NRI_CPU<N> into the OCI spec at
# container-creation time (pkg/resmgr/dra/cdi.go:93-95); they're never
# written back to the pod object, so this must be `kubectl exec ... --
# env`, not an `-o json` read of the pod/container status.
vm-command "kubectl exec dra-pod0 -c dra-pod0c0 -- env"
env_output="$COMMAND_OUTPUT"

grep -q '^NRI_CLASS=hp-turbo$' <<< "$env_output" ||
    command-error "missing/incorrect NRI_CLASS env var in dra-pod0c0 (expected hp-turbo)"

# Parse the claimed CPU ids out of the NRI_CPU<N>=1 env vars -- this,
# not ctr-cpu-ids/Cpus_allowed_list, is the source of truth for which
# CPUs were claimed: allocateClaim removes claimed CPUs from pool
# supply (pools.go:1624-1663) rather than assigning them as the
# consuming container's own cpuset, so ctr-cpu-ids on dra-pod0c0 would
# report a disjoint set and never match.
claimed_cpus=$(grep -oE '^NRI_CPU[0-9]+=1$' <<< "$env_output" | \
    sed -E 's/^NRI_CPU([0-9]+)=1$/\1/' | sort -n | tr '\n' ' ')
claimed_cpus="${claimed_cpus% }"

[ -n "$claimed_cpus" ] ||
    command-error "no NRI_CPU<N>=1 env vars found in dra-pod0c0's environment"

claimed_cpu_count=$(wc -w <<< "$claimed_cpus")
[ "$claimed_cpu_count" -eq 2 ] ||
    command-error "expected exactly 2 claimed CPUs via NRI_CPU<N> env vars (claim requested nri/cpus: \"2\"), got $claimed_cpu_count ($claimed_cpus)"

# CLOS association: the pct.go:786 "associated cpus %s to CLOS %d" line
# -- not the mock's startup-time ConfigureClos line, which would pass
# even if the claim were never allocated. The log line formats the
# cpuset with cpuset.CPUSet.String(), which collapses contiguous ids
# into ranges, so compress the env-derived ids into the same form
# before matching.
assert-cpu-clos dra-pod0c0 "$(compress-cpulist "$claimed_cpus")" "CLOS 0"

# Allocatable deduction: node.status.allocatable.cpu should be exactly
# 2 (claimed CPUs) less than the pre-claim baseline, per
# NodeAllocatableResourceMappings/AllocationMultiplier=1
# (pkg/resmgr/cpuclass/dra.go:279-285). Poll rather than check once, to
# allow for propagation delay between claim allocation and the node
# status update.
expected_allocatable_millis=$((allocatable_baseline_millis - 2000))
wait-allocatable-cpu-millis "$expected_allocatable_millis" \
    "node.status.allocatable.cpu was not deducted by 2 CPUs after claim allocation (baseline was ${allocatable_baseline_millis}m)" \
    30 2

cleanup
