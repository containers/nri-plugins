# test20-dra: DRA (KEP-5075/KEP-5517) integration for the topology-aware
# policy. See docs/dra/plan.md's Step 10 for background.
#
# This is the initial skeleton (plan Task 3): mock+launch, feature-gate
# probe, and skip path only. The ResourceClaim/pod manifests and the
# remaining assertions (CLOS association, env vars, allocatable
# deduction) are added by later tasks.

cleanup() {
    vm-command "kubectl delete pods --all --now"
    helm-terminate
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
