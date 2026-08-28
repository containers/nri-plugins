# test20-dra: DRA (KEP-5075/KEP-5517) integration for the topology-aware
# policy.
#
# Full test: launches the plugin with the SST/cpufreq mock and a
# hp-turbo cpuClass, probes the API server for the KEP-5075/KEP-5517
# feature gates (skipping cleanly if either is missing), applies a
# ResourceClaim selecting the HP class by its published nri/pctPriority
# attribute plus a pod consuming it, and asserts the claimed CPUs'
# CDI env vars, their CLOS association in the policy log, and the
# scheduler's pod.status.nodeAllocatableResourceClaimStatuses[] record.

cleanup() {
    vm-command "kubectl delete pods --all --now"
    vm-command "kubectl delete resourceclaims --all --now"
    helm-terminate
}

# compress-cpulist "8 9 10 12" prints "8-10,12" -- range-collapsing,
# same python3 idiom as test19-cpuclass's expand-cpulist (its inverse).
# Needed because pct.go:786's "associated cpus %s to CLOS %d" log line
# formats its cpuset with
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

# wait-node-allocatable-claim-status <pod> <claim> <container> <cpus> [timeout=30] [interval=2]
# Polls pod.status.nodeAllocatableResourceClaimStatuses[] until it
# contains an entry with resourceClaimName == <claim>, <container>
# listed in .containers, and a .mapping[] entry {name: cpu, quantity:
# <cpus>}, or fails with command-error on timeout.
wait-node-allocatable-claim-status() {
    local pod="$1" claim="$2" ctr="$3" cpus="$4" timeout=${5:-30} interval=${6:-2} elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        vm-command "kubectl get pod $pod -o json | jq -c \
            '[(.status.nodeAllocatableResourceClaimStatuses // [])[] | \
              select(.resourceClaimName == \"$claim\" and \
                     (.containers // [] | index(\"$ctr\")) and \
                     ((.mapping // []) | any(.name == \"cpu\" and .quantity == \"$cpus\")))] | length'"
        [ "$COMMAND_OUTPUT" -gt 0 ] 2>/dev/null && return 0
        sleep "$interval"
        elapsed=$((elapsed + interval))
    done
    vm-command "kubectl get pod $pod -o jsonpath='{.status.nodeAllocatableResourceClaimStatuses}'"
    command-error "pod $pod's status.nodeAllocatableResourceClaimStatuses missing entry {resourceClaimName: $claim, containers: [$ctr], mapping: [{name: cpu, quantity: $cpus}]} (got: $COMMAND_OUTPUT)"
}

# wait-resourceslice-devices [timeout]
# Polls until the DRA driver's published ResourceSlice(s) report at
# least one device, storing a compact JSON array of
# {allowMultipleAllocations, nodeAllocatableResources} objects
# (one per device) in COMMAND_OUTPUT. The driver needs a moment after
# helm-launch to publish its ResourceSlice, so this can't be a single
# one-shot check.
wait-resourceslice-devices() {
    local timeout=${1:-30} elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        vm-command "kubectl get resourceslices -o json | \
            jq -c '[.items[] | select(.spec.driver == \"nri.topology-aware.cpu\") | (.spec.devices // [])[] | {allowMultipleAllocations, nodeAllocatableResources}]'"
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
# If the API server's alpha gates DRAConsumableCapacity/DRANodeAllocatableResources
# are off, it silently strips both fields on write, so they're absent on read back here.
# This requires the plugin to already be running (hence it happens after helm-launch,
# not before), and with the hp-turbo class + SST mock active above so there's
# at least one real device to read back (an empty CPU_CLASSES publishes zero devices).
wait-resourceslice-devices 30 || {
    helm-terminate
    error "no devices found in any ResourceSlice within timeout (not a feature-gate issue -- check DRA_ENABLED/CPU_CLASSES/SST mock config)"
}

# Require both fields on the *same* device object, not merely present
# somewhere in the (possibly multi-device) list -- every device this
# driver publishes gets both fields set unconditionally, so checking
# them independently across the whole list would loosely pass even if,
# say, one device kept allowMultipleAllocations while a different
# device kept nodeAllocatableResources; that's not what either gate
# being enabled actually implies.
if jq -e 'any(.[]; .allowMultipleAllocations == true and (.nodeAllocatableResources != null))' \
    >/dev/null 2>&1 <<< "$COMMAND_OUTPUT"; then
    echo "DRA feature gates (KEP-5075/KEP-5517) detected as enabled on the API server; continuing."
else
    helm-terminate
    echo "Test verdict: SKIP (KEP-5075/KEP-5517 feature gate missing)"
    exit 0
fi

#
# Claim + pod.
#
# A ResourceClaim selecting the HP cpuClass by its published
# nri/pctPriority attribute.
#
# Only "objects create cleanly and the pod reaches Running" is checked
# here

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
              device.attributes["nri"].pctPriority == "high"
EOF
vm-put-file --cleanup "$claim_yaml" hp-turbo-cpus-claim.yaml
vm-command "kubectl apply -f hp-turbo-cpus-claim.yaml" ||
    command-error "failed to create ResourceClaim hp-turbo-cpus"

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
# CLOS association, env vars, allocatable deduction, cleanup.
#

# Env vars: CDI writes NRI_CLASS/NRI_CPU<N> into the OCI spec at
# container-creation time (pkg/resmgr/dra/cdi.go); they're never
# written back to the pod object, so this must be
# `kubectl exec ... -- env`, not an `-o json` read of the pod/container status.
vm-command "kubectl exec dra-pod0 -c dra-pod0c0 -- env"
env_output="$COMMAND_OUTPUT"

grep -q '^NRI_CLASS=hp-turbo$' <<< "$env_output" ||
    command-error "missing/incorrect NRI_CLASS env var in dra-pod0c0 (expected hp-turbo)"

# Parse the claimed CPU ids out of the NRI_CPU<N>=1 env vars.
claimed_cpus=$(grep -oE '^NRI_CPU[0-9]+=1$' <<< "$env_output" | \
    sed -E 's/^NRI_CPU([0-9]+)=1$/\1/' | sort -n | tr '\n' ' ')
claimed_cpus="${claimed_cpus% }"

[ -n "$claimed_cpus" ] ||
    command-error "no NRI_CPU<N>=1 env vars found in dra-pod0c0's environment"

claimed_cpu_count=$(wc -w <<< "$claimed_cpus")
[ "$claimed_cpu_count" -eq 2 ] ||
    command-error "expected exactly 2 claimed CPUs via NRI_CPU<N> env vars (claim requested nri/cpus: \"2\"), got $claimed_cpu_count ($claimed_cpus)"

# The container's actual cpuset must include every claimed CPU id, not
# just the CDI-injected env vars: applyGrant unions the container's
# claimed CPUs into the cpuset it pins, so dra-pod0c0's live cpuset
# (read via Cpus_allowed_list) must be a superset of claimed_cpus.
missing_cpus=$(cpulist-difference "$claimed_cpus" "$(container-cpus dra-pod0 dra-pod0c0)")
[ -z "$missing_cpus" ] ||
    command-error "claimed CPUs ($claimed_cpus) missing from dra-pod0c0's actual cpuset (missing: $missing_cpus)"

# CLOS association: the pct.go "associated cpus %s to CLOS %d" line
# -- not the mock's startup-time ConfigureClos line, which would pass
# even if the claim were never allocated. The log line formats the
# cpuset with cpuset.CPUSet.String(), which collapses contiguous ids
# into ranges, so compress the env-derived ids into the same form
# before matching.
assert-cpu-clos "$(compress-cpulist "$claimed_cpus")" "CLOS 0" \
    "Missing CPU association for dra-pod0c0 (expected CLOS 0)"

# KEP-5517 allocation record: node.status.allocatable.cpu is never
# mutated by DRANodeAllocatableResources. The actual persisted signal
# is pod.status.nodeAllocatableResourceClaimStatuses[], written by the
# scheduler at PreBind. Assert it names the claim, the consuming container,
# and the claimed CPU count.
wait-node-allocatable-claim-status dra-pod0 hp-turbo-cpus dra-pod0c0 "$claimed_cpu_count" 30 2

cleanup
