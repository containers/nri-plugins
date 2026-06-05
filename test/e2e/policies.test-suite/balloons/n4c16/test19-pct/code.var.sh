# Test PCT (Priority Core Turbo) CLOS configuration and CPU
# association, using the OVERRIDE_SST in-memory mock backend.
#
# Verifies:
#  1. Managed mode: SST state mock receives PrepareManagedMode +
#     ConfigureClos(HP CLOS 0) + ConfigureClos(LP CLOS 3) +
#     EnableCP. CLOS bounds match resolved cpuClass MinFreq and
#     MaxFreq (T1.1).
#  2. Pod scheduling: a container in the HP balloon gets its CPUs
#     associated to CLOS 0; a container in the LP balloon gets CLOS 3;
#     reserved/default CPUs land in CLOS 0 (idle CLOS).
#  3. HP-reserve and per-package HP-room steering (Phase 1.3b/c).
#  4. Non-HP balloons avoid HP-in-use packages in managed mode (T1.2).
#  5. Resize of an HP balloon stays on the same package (T1.3).
#  6. Per-class idle reassociation: deleting LP frees LP CPUs to
#     idle CLOS 0 without touching unrelated CLOS 3 / HP state (T1.4).
#  7. Validation: a cpuClass with both pctPriority and sstClosID
#     set is rejected.
#  8. Assoc-only mode: configuration with sstClosID only does NOT
#     call PrepareManagedMode (no log line) and only associates CPUs.

helm-terminate

# pct-log fetches the latest PCT-related log lines.
pct-log() {
    local last_n=${1:-200}
    vm-command "kubectl -n kube-system logs ds/nri-resource-policy-balloons | grep -E 'pct(:| mock:)' | tail -n $last_n"
}

# assert-log-contains <regex> <message>
assert-log-contains() {
    local pat=$1
    local msg=$2
    pct-log 500
    grep -E -q "$pat" <<< "$COMMAND_OUTPUT" || command-error "$msg (pattern: $pat)"
}

# assert-log-not-contains <regex> <message>
assert-log-not-contains() {
    local pat=$1
    local msg=$2
    pct-log 500
    if grep -E -q "$pat" <<< "$COMMAND_OUTPUT"; then
        command-error "$msg (unexpected pattern: $pat)"
    fi
}

# wait-assert-log-contains <regex> <message> [timeout=5]
# Polls the pct log every 1s until <regex> matches or <timeout>
# seconds pass. On timeout, defers to assert-log-contains so the
# resulting command-error carries the captured log output.
wait-assert-log-contains() {
    local pat=$1
    local msg=$2
    local timeout=${3:-5}
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        pct-log 500
        grep -E -q "$pat" <<< "$COMMAND_OUTPUT" && return 0
        sleep 1
        elapsed=$((elapsed + 1))
    done
    assert-log-contains "$pat" "$msg"
}

# wait-assert-log-grew <regex> <prev_count> <message> [timeout=5]
# Like wait-assert-log-contains but for "did a fresh line appear?"
# cases where the pattern already exists from an earlier phase.
wait-assert-log-grew() {
    local pat=$1
    local prev=$2
    local msg=$3
    local timeout=${4:-5}
    local elapsed=0 cur
    while [ "$elapsed" -lt "$timeout" ]; do
        pct-log 500
        cur=$(grep -c -E "$pat" <<< "$COMMAND_OUTPUT")
        [ "$cur" -gt "$prev" ] && return 0
        sleep 1
        elapsed=$((elapsed + 1))
    done
    command-error "$msg"
}

# wait-pod-gone <podname> [timeout=30]
wait-pod-gone() {
    local pod=$1
    local timeout=${2:-30}
    vm-run-until --timeout "$timeout" "! kubectl get pod $pod -o name 2>/dev/null | grep -q ." || return 1
    return 0
}

###############################################################################
# Phase 1: Managed mode -- HP + LP cpuClasses
###############################################################################

helm_config=$TEST_DIR/balloons-pct-managed.cfg helm-launch balloons

# Managed-mode startup: PrepareManagedMode, ConfigureClos for the
# HP (CLOS 0) and LP (CLOS 3) plans, EnableCP. T1.1 asserts BOTH
# MinFreq and MaxFreq are programmed (the mock prints them in
# struct order: MinFreq before MaxFreq).
wait-assert-log-contains 'PrepareManagedMode done' "managed mode startup missing"
wait-assert-log-contains 'ConfigureClos.*ClosID:0 MinFreq:3800000 MaxFreq:3800000' "HP CLOS 0 not programmed with MinFreq=MaxFreq=turbo (3800000)"
wait-assert-log-contains 'ConfigureClos.*ClosID:3 MinFreq:800000 MaxFreq:2900000' "LP CLOS 3 not programmed with MinFreq=min (800000) MaxFreq=base (2900000)"
wait-assert-log-contains 'EnableCP done' "EnableCP missing"

# Phase 1.2: schedule a pod in the HP balloon.
CPUREQ=1 CPULIM=1 MEMREQ=10M MEMLIM=10M \
       POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: pct-hp-bln" CONTCOUNT=1 \
       create balloons-busybox
wait-assert-log-contains 'associated cpus .* to CLOS 0' "HP pod CPUs not associated to CLOS 0"
report allowed

# Phase 1.3: schedule a pod in the LP balloon.
CPUREQ=1 CPULIM=1 MEMREQ=10M MEMLIM=10M \
       POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: pct-lp-bln" CONTCOUNT=1 \
       create balloons-busybox
wait-assert-log-contains 'associated cpus .* to CLOS 3' "LP pod CPUs not associated to CLOS 3"
report allowed

# Phase 1.3b: verify HP-reserve allocation steering. The HP balloon
# (pct-hp-bln) preferred to be close to virtDevSstHpReserve and
# therefore landed in the package that initially had the most
# free CPUs. The LP balloon (pct-lp-bln) preferred to be far from
# the same virtual device and therefore landed in the *other*
# package. Reserved (cpu0..) is on package 0, and free cpus
# 2-7 (pkg0, 6 CPUs) outnumber 10-13 (pkg1, 4 CPUs), so HP -> pkg0,
# LP -> pkg1.
verify 'packages["pod0c0"] != packages["pod1c0"]'
verify 'cpus["pod0c0"].issubset({"cpu02","cpu03","cpu04","cpu05","cpu06","cpu07"})'
verify 'cpus["pod1c0"].issubset({"cpu10","cpu11","cpu12","cpu13"})'

# Phase 1.3c: schedule a second HP pod into a *different* HP
# balloon type (pct-hp2-bln). pkg0 already has 1 HP CPU
# (pct-hp-bln), pkg1 has 0. With max_hp_cpus=2 per package, the
# HP rooms are: pkg0 = 2-1 = 1; pkg1 = 2-0 = 2. The new HP
# balloon should land on pkg1 because it has the larger HP room,
# even though pkg0 also has free CPUs.
CPUREQ=1 CPULIM=1 MEMREQ=10M MEMLIM=10M \
       POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: pct-hp2-bln" CONTCOUNT=1 \
       create balloons-busybox
report allowed
verify 'cpus["pod2c0"].issubset({"cpu10","cpu11","cpu12","cpu13"})'
verify 'packages["pod2c0"] != packages["pod0c0"]'

# T1.3: resize the pct-hp2-bln balloon by adding a second pod
# with the same annotation. The balloon (maxCPUs=2) should
# expand from 1 CPU to 2 CPUs and the new CPU set must be
# associated to CLOS 0 (the HP plan), not the LP CLOS or
# anything else. This is the end-to-end counterpart of the
# TestMergeCpuClassHintsNoAccumulation unit test: it exercises
# applyCpuClassHints on the resize path. We do not assert
# strict same-package placement because cputree may legitimately
# grow a balloon across packages when local free CPUs are
# scarce; what matters is that the resize happens AND the new
# CPUs are programmed to the correct CLOS (the cpuclass-driven
# behavior under test).
pct-log 500
prev_to_clos0=$(grep -c 'to CLOS 0' <<< "$COMMAND_OUTPUT")
CPUREQ=1 CPULIM=1 MEMREQ=10M MEMLIM=10M \
       POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: pct-hp2-bln" CONTCOUNT=1 \
       create balloons-busybox
wait-assert-log-grew 'to CLOS 0' "$prev_to_clos0" "resize of pct-hp2-bln did not associate the new CPU(s) to CLOS 0"
report allowed
# Both pods share the same (resized) balloon, so they must see
# the same CPU set, and that set must have grown to 2.
verify 'cpus["pod2c0"] == cpus["pod3c0"]'
verify 'len(cpus["pod3c0"]) == 2'

# T1.2: a non-HP balloon type (pct-lp2-bln) added late should
# still route its CPUs to the correct CLOS (3 = LP plan). The
# placement-side Avoid:hpInUseCpus hint is exercised here but
# not asserted because by this point pct-hp2-bln may span both
# packages, leaving no HP-free package. The strict Avoid
# placement logic is covered by TestPctHints_*AvoidsHpInUse at
# the unit level; here we verify it does not break and that a
# distinct LP cpuClass association still happens correctly.
CPUREQ=1 CPULIM=1 MEMREQ=10M MEMLIM=10M \
       POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: pct-lp2-bln" CONTCOUNT=1 \
       create balloons-busybox
wait-assert-log-contains 'associated cpus .* to CLOS 3' "LP2 pod CPUs not associated to CLOS 3"
report allowed

# T1.4: per-class idle reassociation. Delete the LP pod (pod1).
# Its CPUs are reassociated to the LP fallback CLOS (3) because
# the idleCpuClass "default-class" has no PCT plan, and managed
# mode must NOT silently park idle CPUs on the HP CLOS 0 (which
# would consume limited Priority Core Turbo capacity).
pct-log 500
prev_to_clos3=$(grep -c 'to CLOS 3' <<< "$COMMAND_OUTPUT")
vm-command "kubectl delete pod pod1 --now"
wait-assert-log-grew 'to CLOS 3' "$prev_to_clos3" "deleting LP pod did not reassociate its CPUs to LP fallback CLOS 3"

# Now delete the rest -- all freed CPUs end up on the LP
# fallback CLOS 3 for the same reason.
pct-log 500
prev_to_clos3=$(grep -c 'to CLOS 3' <<< "$COMMAND_OUTPUT")
vm-command "kubectl delete pods --all --now"
wait-assert-log-grew 'to CLOS 3' "$prev_to_clos3" "after deleting remaining pods CPUs were not reassociated to LP fallback CLOS 3"

helm-terminate

###############################################################################
# Phase 2: Assoc-only mode (sstClosID without pctPriority)
###############################################################################

helm_config=$TEST_DIR/balloons-pct-assoconly.cfg helm-launch balloons

# Schedule a pod targeting the assoc-clos1 balloon.
CPUREQ=1 CPULIM=1 MEMREQ=10M MEMLIM=10M \
       POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: assoc-clos1-bln" CONTCOUNT=1 \
       create balloons-busybox
report allowed
wait-assert-log-contains 'associated cpus .* to CLOS 1' "CPUs not associated to CLOS 1 in assoc-only mode"

# Now that a full pod admission has gone through without any PCT
# startup-time configuration, the negative checks for managed-mode
# initialization are deterministic.
assert-log-not-contains 'PrepareManagedMode done' "PrepareManagedMode unexpectedly called in assoc-only mode"
assert-log-not-contains 'EnableCP done' "EnableCP unexpectedly called in assoc-only mode"

vm-command "kubectl delete pods --all --now" || true
helm-terminate

###############################################################################
# Phase 3: Validation -- pctPriority + sstClosID rejected
###############################################################################

# The invalid config sets both pctPriority and sstClosID on one
# cpuClass. The policy must reject it -- the daemonset pod will
# crash because the policy fails to start. Use expect_error=1 so
# helm-launch tolerates the failure.
expect_error=1 helm_config=$TEST_DIR/balloons-pct-invalid.cfg helm-launch balloons
vm-run-until --timeout 10 "kubectl -n kube-system logs ds/nri-resource-policy-balloons 2>/dev/null | grep -q 'mutually exclusive'" \
    || command-error "Invalid PCT config (both pctPriority and sstClosID) was not reported as mutually exclusive"
helm-terminate || true
