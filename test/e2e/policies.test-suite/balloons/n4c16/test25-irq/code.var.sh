# Test balloons IRQ CPU affinity: irqClaim and irqMode (sink, isolate).

# expand-cpulist "0-2,5" prints "0 1 2 5"
expand-cpulist() {
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
' "$1"
}

# ids-difference "1 2 3 4" "2 3" prints "1 4"
ids-difference() {
    python3 -c 'import sys
allc = set(int(x) for x in sys.argv[1].split())
iso = set(int(x) for x in sys.argv[2].split())
print(" ".join(str(x) for x in sorted(allc - iso)))
' "$1" "$2"
}

# ctr-cpu-ids podXcY prints sorted CPU ids allowed for the container,
# e.g. "0 1". Requires a preceding "verify" to refresh the state.
ctr-cpu-ids() {
    pyexec "print(' '.join(str(i) for i in sorted(cpu_ids(cpus['$1']))))"
}

# irq-cpu-ids IRQNUM prints sorted CPU ids in the affinity of the IRQ.
irq-cpu-ids() {
    expand-cpulist "$(vm-command-q "cat /proc/irq/$1/smp_affinity_list" | tr -d '[:space:]')"
}

# set-irq-cpus IRQNUM CPULIST sets the affinity of the IRQ, e.g. "0-15".
set-irq-cpus() {
    vm-command "echo $2 > /proc/irq/$1/smp_affinity_list" ||
        command-error "failed to set affinity of irq $1"
}

# verify-irq-cpus IRQNUM EXPECTED waits until the affinity of the IRQ
# equals EXPECTED (sorted space-separated CPU ids), or fails after a
# timeout.
verify-irq-cpus() {
    local irqnum=$1 expected=$2 got tries=20
    while [ "$tries" -gt 0 ]; do
        got=$(irq-cpu-ids "$irqnum")
        if [ "$got" == "$expected" ]; then
            echo "irq $irqnum affinity is '$got' as expected"
            return 0
        fi
        tries=$((tries - 1))
        sleep 1
    done
    error "irq $irqnum affinity: expected CPUs '$expected', got '$got'"
}

# Detect ttyS0 and rtc0 is IRQ numbers on vm.
vm-command "awk '/ ttyS0/{print \$1}' < /proc/interrupts | sed 's/://g' | head -n 1"
TTYS0_IRQ=$COMMAND_OUTPUT
vm-command "awk '/ rtc0/{print \$1}' < /proc/interrupts | sed 's/://g' | head -n 1"
RTC0_IRQ=$COMMAND_OUTPUT
vm-command "awk '/ acpi/{print \$1}' < /proc/interrupts | sed 's/://g' | head -n 1"
ACPI_IRQ=$COMMAND_OUTPUT

ALL_CPUS="$(expand-cpulist 0-15)"

cleanup() {
    vm-command "kubectl delete pods --all --now"
    vm-command "for f in /proc/irq/*/smp_affinity_list ; do echo 0-15 | tee $f >&/dev/null; done"
}

cleanup

# Test irqClaim. CPUs of the claimer balloon handle the claimed
# IRQs (ttyS0 and rtc0).
helm-terminate
helm_config=${TEST_DIR}/balloons-irq-claim.cfg helm-launch balloons

POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: claimer" CONTCOUNT=1 create balloons-busybox
report allowed

claimer_cpus=$(ctr-cpu-ids pod0c0)
echo "claimer CPUs: $claimer_cpus"
verify-irq-cpus "$TTYS0_IRQ" "$claimer_cpus"
verify-irq-cpus "$RTC0_IRQ" "$claimer_cpus"
verify-irq-cpus "$ACPI_IRQ" "$ALL_CPUS"

# Test irqMode isolate. CPUs of the isolate balloon are removed from
# the affinity of unclaimed IRQs. Preset a full affinity so that the
# removal is observable.
helm-terminate
helm_config=${TEST_DIR}/balloons-irq-isolate.cfg helm-launch balloons
cleanup

POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: isolate" CONTCOUNT=1 create balloons-busybox
report allowed

isolate_cpus=$(ctr-cpu-ids pod1c0)
echo "isolate CPUs: $isolate_cpus"

# expected_isolate=$(python3 -c 'import sys
# allc = set(int(x) for x in sys.argv[1].split())
# iso = set(int(x) for x in sys.argv[2].split())
# print(" ".join(str(x) for x in sorted(allc - iso)))
# ' "$ALL_CPUS" "$isolate_cpus")
expected_isolate=$(ids-difference "$ALL_CPUS" "$isolate_cpus")
verify-irq-cpus "$RTC0_IRQ" "$expected_isolate"


# Test irqMode sink. CPUs of the sink balloon handle unclaimed IRQs.
helm-terminate
helm_config=${TEST_DIR}/balloons-irq-sink.cfg helm-launch balloons
cleanup

# no sink, no claimer present
verify-irq-cpus "$TTYS0_IRQ" "$ALL_CPUS"

POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: sink" CONTCOUNT=1 create balloons-busybox
report allowed
verify
sink_cpus=$(ctr-cpu-ids pod2c0)
echo "sink CPUs: $sink_cpus"
verify-irq-cpus "$TTYS0_IRQ" "$sink_cpus"
verify-irq-cpus "$RTC0_IRQ" "$sink_cpus"
verify-irq-cpus "$ACPI_IRQ" "$sink_cpus"

POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: claimer" CONTCOUNT=1 create balloons-busybox
report allowed
sink_cpus=$(ctr-cpu-ids pod2c0)
claimer_cpus=$(ctr-cpu-ids pod3c0)
echo "claimer CPUs: $claimer_cpus"
echo "sink CPUs: $sink_cpus"
verify-irq-cpus "$TTYS0_IRQ" "$claimer_cpus"
verify-irq-cpus "$RTC0_IRQ" "$claimer_cpus"
verify-irq-cpus "$ACPI_IRQ" "$sink_cpus"

vm-command 'kubectl delete pod pod2 --now'
# no sink present anymore
verify-irq-cpus "$TTYS0_IRQ" "$claimer_cpus"
verify-irq-cpus "$RTC0_IRQ" "$claimer_cpus"
verify-irq-cpus "$ACPI_IRQ" "$ALL_CPUS"

# Test irqMode sink. CPUs of the sink balloon handle unclaimed IRQs.
helm-terminate
helm_config=${TEST_DIR}/balloons-irq-dedicated-claim.cfg helm-launch balloons
cleanup

POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: dedicated-claimer" CONTCOUNT=1 create balloons-busybox
report allowed

d_claimer_cpus=$(ctr-cpu-ids pod4c0)
echo "dedicated claimer CPUs: $d_claimer_cpus"
verify-irq-cpus "$RTC0_IRQ" "$d_claimer_cpus"

expected_isolate=$(ids-difference "$ALL_CPUS" "$d_claimer_cpus")
verify-irq-cpus "$TTYS0_IRQ" "$expected_isolate"
verify-irq-cpus "$ACPI_IRQ" "$expected_isolate"

cleanup
helm-terminate

# Test restricting interrupts control and verify that it takes effect.
expect_error=1 helm_config=$TEST_DIR/balloons-denied-irq-claim.cfg helm-launch balloons
vm-run-until --timeout 10 "kubectl -n kube-system logs ds/nri-resource-policy-balloons 2>/dev/null | grep -q 'denied interrupt: .* denied but matched by user pattern .*'" || \
    command-error "expected error of IRQ claim referencing denied IRQ not reported"
helm-terminate || true

cleanup
