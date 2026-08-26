# Test balloons IRQ CPU affinity: irqClaim and irqMode (sink, isolate).

# Interrupts used in this test, resolved from /proc/interrupts on demand.
TTYS0_IRQ=".* ttyS0.*"
RTC0_IRQ=".* rtc0.*"
ACPI_IRQ=".* acpi.*"

ALL_CPUS="$(expand-cpulist 0-15)"

cleanup() {
    delete-pods --all
    vm-command "for f in /proc/irq/*/smp_affinity_list ; do echo 0-15 | tee $f >&/dev/null; done"
}

cleanup

# Test irqClaim. CPUs of the claimer balloon handle the claimed
# IRQs (ttyS0 and rtc0).
helm-terminate
helm_config=${TEST_DIR}/balloons-irq-claim.cfg helm-launch balloons

POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: claimer" CONTCOUNT=1 create balloons-busybox
report allowed

claimer_cpus=$(allowed-cpu-ids pod0c0)
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

isolate_cpus=$(allowed-cpu-ids pod1c0)
echo "isolate CPUs: $isolate_cpus"

# expected_isolate=$(python3 -c 'import sys
# allc = set(int(x) for x in sys.argv[1].split())
# iso = set(int(x) for x in sys.argv[2].split())
# print(" ".join(str(x) for x in sorted(allc - iso)))
# ' "$ALL_CPUS" "$isolate_cpus")
expected_isolate=$(cpulist-difference "$ALL_CPUS" "$isolate_cpus")
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
sink_cpus=$(allowed-cpu-ids pod2c0)
echo "sink CPUs: $sink_cpus"
verify-irq-cpus "$TTYS0_IRQ" "$sink_cpus"
verify-irq-cpus "$RTC0_IRQ" "$sink_cpus"
verify-irq-cpus "$ACPI_IRQ" "$sink_cpus"

POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: claimer" CONTCOUNT=1 create balloons-busybox
report allowed
sink_cpus=$(allowed-cpu-ids pod2c0)
claimer_cpus=$(allowed-cpu-ids pod3c0)
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

d_claimer_cpus=$(allowed-cpu-ids pod4c0)
echo "dedicated claimer CPUs: $d_claimer_cpus"
verify-irq-cpus "$RTC0_IRQ" "$d_claimer_cpus"

expected_isolate=$(cpulist-difference "$ALL_CPUS" "$d_claimer_cpus")
verify-irq-cpus "$TTYS0_IRQ" "$expected_isolate"
verify-irq-cpus "$ACPI_IRQ" "$expected_isolate"

cleanup
helm-terminate

# Test restricting interrupts control and verify that it takes effect.
expect_error=1 helm_config=$TEST_DIR/balloons-denied-irq-claim.cfg helm-launch balloons
wait-assert-log-contains 'denied interrupt: .* denied but matched by user pattern .*' \
    "expected error of IRQ claim referencing denied IRQ not reported" 10
helm-terminate || true

cleanup
