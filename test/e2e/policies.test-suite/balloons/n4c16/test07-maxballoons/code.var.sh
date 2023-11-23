cleanup() {
    vm-command "kubectl delete pods --all --now"
    return 0
}

cleanup

helm-terminate
helm_config=${TEST_DIR}/balloons-maxballoons.cfg helm-launch balloons

# pod0: allocate 1500/2000 mCPUs of the singleton balloon
CPUREQ="1500m" CPULIM="1500m"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: singleton" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod0c0"]) == 2'

# pod1: allocate the rest 500/2000 mCPUs of the singleton balloon
CPUREQ="500m" CPULIM="500m"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: singleton" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'cpus["pod0c0"] == cpus["pod1c0"]'

# pod2: try to fit in the already full singleton balloon
CPUREQ="100m" CPULIM="100m"
( POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: singleton" CONTCOUNT=1 wait_t=5s create balloons-busybox ) && {
    error "creating pod2 succeeded but was expected to fail with balloon allocation error"
}
echo "pod2 creation failed with an error as expected"
vm-command "kubectl describe pod pod2"
if ! grep -q 'no suitable balloon instance available' <<< "$COMMAND_OUTPUT"; then
    error "could not find 'no suitable balloon instance available' in pod2 description"
fi
vm-command "kubectl delete pod pod2 --now"

# pod2: create dynamically the first dynamictwo balloon
CPUREQ="800m" CPULIM="800m"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: dynamictwo" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod2c0"]) == 1'

# pod3: create dynamically the second dynamictwo balloon
CPUREQ="600m" CPULIM="600m"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: dynamictwo" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'disjoint_sets(cpus["pod2c0"], cpus["pod3c0"])'

# pod4: prefering new balloon fails, but this fits in the second dynamictwo balloon
CPUREQ="300m" CPULIM="300m"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: dynamictwo" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'cpus["pod4c0"] == cpus["pod3c0"]'

# pod5: prefering new balloon fails, and fitting to existing dynamictwo balloons fails
CPUREQ="300m" CPULIM="300m"
( POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: dynamictwo" CONTCOUNT=1 wait_t=5s create balloons-busybox ) && {
    error "creating pod6 succeeded but was expected to fail with balloon allocation error"
}
vm-command "kubectl describe pod pod5"
if ! grep -q 'no suitable balloon instance available' <<< "$COMMAND_OUTPUT"; then
    error "could not find 'no suitable balloon instance available' in pod6 description"
fi
vm-command "kubectl delete pod pod5 --now"

cleanup

# Try starting nri-resource-policy with a configuration where MinBalloons and
# MaxBalloons of the same balloon type contradict.
helm-terminate
( helm_config=${TEST_DIR}/balloons-maxballoons-impossible.cfg launch_timeout=5s helm-launch balloons ) && {
    error "starting nri-resource-policy succeeded, but was expected to fail due to impossible static balloons"
}
echo "starting nri-resource-policy with impossible static balloons configuration failed as expected"

helm-terminate
