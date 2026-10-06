# This test verifies that the balloons policy publishes instances of balloon
# types with the dra option as DRA devices, that a prepared claim inflates
# its balloon and pins the containers using it there, that containers
# sharing a claim split its CPU shares, that a DRA balloon without minCPUs
# holds CPUs only while it has claims, that DRA and regular containers
# never share balloons, that contradicting requests fail, and that claims
# survive a plugin restart.

cleanup() {
    vm-command "kubectl delete pods --all --now --wait" || :
    vm-command "kubectl delete resourceclaimtemplates --all --now" || :
    helm-terminate || :
    remove-policy-cache
}

dra-devices() {
    # Usage: dra-devices
    #
    # Print the sorted names of the devices the plugin publishes.
    vm-command-q "kubectl get resourceslices -o json | jq -r '[.items[] |
                      select(.spec.driver == \"balloons.nri.io\") | .spec.devices[].name] |
                      sort | join(\" \")'"
}

dra-device() {
    # Usage: dra-device FIELD NAME
    #
    # Print FIELD of the published device NAME.
    vm-command-q "kubectl get resourceslices -o json | jq -r '.items[] |
                      select(.spec.driver == \"balloons.nri.io\") | .spec.devices[] |
                      select(.name == \"$2\") | .$1'"
}

container-env() {
    # Usage: container-env POD CONTAINER VAR
    #
    # Print the value of VAR in the environment of CONTAINER of POD.
    vm-command-q "kubectl exec $1 -c $2 -- env" | sed -n "s/^$3=//p"
}

check-env() {
    # Usage: check-env POD CONTAINER VAR VALUE
    #
    # Check that VAR in the environment of CONTAINER of POD is VALUE.
    local value
    value=$(container-env "$1" "$2" "$3")
    [ "$value" == "$4" ] ||
        error "$1/$2 has $3=\"$value\", expected \"$4\""
    echo "$1/$2 has $3=$value"
}

container-cpus() {
    # Usage: container-cpus POD CONTAINER
    #
    # Print the CPUs CONTAINER of POD is allowed to run on.
    vm-command-q "kubectl exec $1 -c $2 -- grep Cpus_allowed_list /proc/self/status" |
        awk '{print $2}'
}

container-weight() {
    # Usage: container-weight POD CONTAINER
    #
    # Print the cgroup v2 CPU weight of CONTAINER of POD.
    vm-command-q "kubectl exec $1 -c $2 -- cat /sys/fs/cgroup/cpu.weight"
}

expected-weights() {
    # Usage: expected-weights MILLICPUS
    #
    # Print the cgroup v2 CPU weights that runtimes set for the CPU shares
    # of a container with MILLICPUS worth of CPU: the quadratic conversion
    # of runc 1.3 and later, and the linear conversion of older runtimes.
    local shares=$(( $1 * 1024 / 1000 ))
    awk -v s="$shares" 'BEGIN {
        l = log(s) / log(2);
        e = (l * l + 125 * l) / 612.0 - 7.0 / 34.0;
        w = int(exp(e * log(10)) * 1e6 + 0.5) / 1e6;
        quadratic = (w == int(w)) ? w : int(w) + 1;
        linear = 1 + int((s - 2) * 9999 / 262142);
        printf "%d %d\n", quadratic, linear
    }'
}

check-weight() {
    # Usage: check-weight POD CONTAINER MILLICPUS
    #
    # Check that CONTAINER of POD has the CPU weight of MILLICPUS.
    local weight expected
    weight=$(container-weight "$1" "$2")
    expected=$(expected-weights "$3")
    [[ " $expected " == *" $weight "* ]] ||
        error "$1/$2 has CPU weight $weight, expected one of $expected ($3 mCPU)"
    echo "$1/$2 has CPU weight $weight ($3 mCPU)"
}

restart-plugin() {
    # Usage: restart-plugin
    #
    # Restart the plugin, keeping its cache, and keep collecting its logs.
    local ds=ds/nri-resource-policy-balloons
    vm-command "kubectl rollout restart -n kube-system $ds &&
                kubectl rollout status -n kube-system $ds --timeout=60s" ||
        error "failed to restart the plugin"
    vm-stop-log-collection
    vm-command "kubectl logs -f -n kube-system $ds >>nri-resource-policy.output.txt 2>&1 &"
    vm-port-forward-enable
}

# Native CPU requests are given explicitly where wanted.
CPUREQ=""
CPULIM=""

# Containerd passes CDI devices to NRI plugins since 2.3.0.
vm-command "containerd --version"
if ! awk '{split($3, v, "."); sub("^v", "", v[1]); exit !(v[1] > 2 || (v[1] == 2 && v[2] >= 3))}' <<< "$COMMAND_OUTPUT"; then
    echo "Test verdict: SKIP (containerd older than 2.3.0 does not pass CDI devices to NRI)"
    exit 0
fi

cleanup
relaunch-policy balloons "$TEST_DIR/balloons-dra.cfg"

retry-until --timeout 30 --message "the plugin to publish its devices" \
    '[ "$(dra-devices)" == "fast-0 fast-1 lazy-0 solo-0" ]' ||
    error "published devices are \"$(dra-devices)\", expected \"fast-0 fast-1 lazy-0 solo-0\""

# A DRA balloon type without minCPUs is published with its maxCPUs
# capacity, but its instance holds no CPUs before a claim is prepared.
assert-log-contains 'lazy\[0\]\{cpus:"", mems:""\}' "lazy[0] should have started without CPUs"

# Without DRAConsumableCapacity the API server drops the field which lets
# claims share a device.
if [ "$(dra-device allowMultipleAllocations fast-0)" != "true" ]; then
    cleanup
    echo "Test verdict: SKIP (DRAConsumableCapacity is not enabled)"
    exit 0
fi

for dev_cap_type_inst in fast-0:4:fast:0 fast-1:4:fast:1 lazy-0:2:lazy:0 solo-0:2:solo:0; do
    IFS=: read -r dev cap btype inst <<< "$dev_cap_type_inst"
    [ "$(dra-device capacity.cpu.value "$dev")" == "$cap" ] ||
        error "device $dev has cpu capacity \"$(dra-device capacity.cpu.value "$dev")\", expected $cap"
    [ "$(dra-device attributes.balloonType.string "$dev")" == "$btype" ] ||
        error "device $dev has balloonType \"$(dra-device attributes.balloonType.string "$dev")\", expected $btype"
    [ "$(dra-device attributes.instance.int "$dev")" == "$inst" ] ||
        error "device $dev has instance \"$(dra-device attributes.instance.int "$dev")\", expected $inst"
done

NAME=fast-whole BTYPE=fast wait="" create dra-claim
NAME=fast-two BTYPE=fast CPUS=2 wait="" create dra-claim
NAME=solo-whole BTYPE=solo wait="" create dra-claim

# pod0: a claim without a CPU amount takes a whole fast balloon.
NAME=pod0 CLAIM=fast-whole create dra-pod
dev0=$(container-env pod0 pod0c0 DRA_BALLOON)
case "$dev0" in
    fast-0) dev1=fast-1;;
    fast-1) dev1=fast-0;;
    *) error "pod0c0 has DRA_BALLOON=\"$dev0\", expected fast-0 or fast-1";;
esac
echo "pod0 got $dev0"
report allowed
verify 'len(cpus["pod0c0"]) == 4'

# pod1, pod2: two claims of 2 CPUs share the other fast balloon, which
# inflates to hold both.
NAME=pod1 CLAIM=fast-two create dra-pod
check-env pod1 pod1c0 DRA_BALLOON "$dev1"
report allowed
verify 'len(cpus["pod1c0"]) == 2' \
       'disjoint_sets(cpus["pod1c0"], cpus["pod0c0"])'
NAME=pod2 CLAIM=fast-two create dra-pod
check-env pod2 pod2c0 DRA_BALLOON "$dev1"
report allowed
verify 'cpus["pod1c0"] == cpus["pod2c0"]' \
       'len(cpus["pod2c0"]) == 4' \
       'disjoint_sets(cpus["pod2c0"], cpus["pod0c0"])'

# pod3: no fast balloon has capacity left, so the scheduler must not place
# the claim until a pod gives its CPUs back. Both containers of pod3 share
# the claim.
NAME=pod3 CLAIM=fast-two CONTCOUNT=2 wait="" create dra-pod
vm-command "kubectl wait --for=condition=PodScheduled=false pod/pod3 --timeout=60s" ||
    error "pod3 was scheduled with no fast capacity left"
vm-command "kubectl delete pod pod1 --now --wait"
vm-command "kubectl wait --for=condition=Ready pod/pod3 --timeout=120s" ||
    error "pod3 did not start after pod1 gave its CPUs back"
check-env pod3 pod3c0 DRA_BALLOON "$dev1"
check-env pod3 pod3c1 DRA_BALLOON "$dev1"
report allowed
verify 'cpus["pod3c0"] == cpus["pod2c0"]' \
       'cpus["pod3c1"] == cpus["pod2c0"]' \
       'len(cpus["pod3c0"]) == 4' \
       'disjoint_sets(cpus["pod3c0"], cpus["pod0c0"])'
# The claim of pod3 is divided between its two containers, so the CPU
# shares of pod2c0, pod3c0 and pod3c1 are in ratio 2:1:1. pod3c0 got the
# whole claim when it was created, and was updated when pod3c1 came.
check-weight pod2 pod2c0 2000
check-weight pod3 pod3c0 1000
check-weight pod3 pod3c1 1000

# pod12: a claim gives the lazy balloon its CPUs, and releasing the claim
# takes them back. This runs while the node still has free CPUs: the
# policy can inflate the balloon only from CPUs no balloon holds.
NAME=lazy-whole BTYPE=lazy wait="" create dra-claim
NAME=pod12 CLAIM=lazy-whole create dra-pod
check-env pod12 pod12c0 DRA_BALLOON lazy-0
report allowed
verify 'len(cpus["pod12c0"]) == 2' \
       'disjoint_sets(cpus["pod12c0"], cpus["pod0c0"], cpus["pod2c0"])'
vm-command "kubectl delete pod pod12 --now --wait"
wait-assert-log-contains 'released 2000 mCPU in balloon lazy\[0\]\{cpus:""' \
    "lazy[0] did not give its CPUs back when its claim was released" 30

# pod4, pod5: regular containers go to regular balloons only.
CPUREQ="100m" POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: two-cpu" CONTCOUNT=1 \
    NAME=pod4 create balloons-busybox
CPUREQ="100m" CONTCOUNT=1 NAME=pod5 create balloons-busybox
report allowed
verify 'len(cpus["pod4c0"]) == 2' \
       'disjoint_sets(cpus["pod4c0"], cpus["pod0c0"], cpus["pod2c0"])' \
       'disjoint_sets(cpus["pod5c0"], cpus["pod0c0"], cpus["pod2c0"])'

# pod6: the container without the claim is a regular container.
NAME=pod6 CLAIM=solo-whole CONTCOUNT=2 NOCLAIM_CONTAINERS=1 create dra-pod
check-env pod6 pod6c0 DRA_BALLOON solo-0
check-env pod6 pod6c1 DRA_BALLOON ""
report allowed
verify 'len(cpus["pod6c0"]) == 2' \
       'disjoint_sets(cpus["pod6c0"], cpus["pod0c0"], cpus["pod2c0"], cpus["pod4c0"], cpus["pod5c0"])' \
       'disjoint_sets(cpus["pod6c1"], cpus["pod6c0"], cpus["pod0c0"], cpus["pod2c0"])'
vm-command "kubectl delete pod pod6 --now --wait"

# pod7: the annotation contradicts the type of the claimed balloon.
NAME=pod7 CLAIM=solo-whole POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: two-cpu" \
    wait="" create dra-pod
verify-container-error pod7 pod7c0 "requests balloon type" 60
vm-command "kubectl delete pod pod7 --now --wait"

# pod8: one container uses claims of two balloons.
vm-command "kubectl delete pod pod3 --now --wait"
NAME=pod8 CLAIM=fast-two CLAIM2=solo-whole CLAIM2_CONTAINERS=0 wait="" create dra-pod
verify-container-error pod8 pod8c0 "more than one balloon" 60
vm-command "kubectl delete pod pod8 --now --wait"

# pod9: a regular container must not be annotated into a DRA balloon.
CPUREQ="100m" POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: solo" CONTCOUNT=1 \
    NAME=pod9 wait="" create balloons-busybox
verify-container-error pod9 pod9c0 "is published as DRA devices" 60
vm-command "kubectl delete pod pod9 --now --wait"

# A plugin which forgot its claims would deflate the DRA balloons to the
# native requests of their containers.
pod0_cpus=$(container-cpus pod0 pod0c0)
pod2_cpus=$(container-cpus pod2 pod2c0)
restart-plugin
report allowed
verify 'len(cpus["pod0c0"]) == 4' \
       'disjoint_sets(cpus["pod0c0"], cpus["pod2c0"])'
[ "$(container-cpus pod0 pod0c0)" == "$pod0_cpus" ] ||
    error "pod0c0 CPUs changed over the restart from $pod0_cpus to $(container-cpus pod0 pod0c0)"
[ "$(container-cpus pod2 pod2c0)" == "$pod2_cpus" ] ||
    error "pod2c0 CPUs changed over the restart from $pod2_cpus to $(container-cpus pod2 pod2c0)"

# After the restart, claims still land on the right balloons and releasing
# them frees capacity.
NAME=pod10 CLAIM=fast-two create dra-pod
check-env pod10 pod10c0 DRA_BALLOON "$dev1"
report allowed
verify 'cpus["pod10c0"] == cpus["pod2c0"]' \
       'len(cpus["pod10c0"]) == 4' \
       'disjoint_sets(cpus["pod10c0"], cpus["pod0c0"])'
# pod11: two claims of one pod on the fast balloon pod0 gave back. pod11c0
# and pod11c1 share the first claim, pod11c2 has the second one of its own,
# so their CPU shares are in ratio 1:1:2.
vm-command "kubectl delete pod pod0 --now --wait"
NAME=pod11 CLAIM=fast-two CLAIM2=fast-two CONTCOUNT=3 NOCLAIM_CONTAINERS=2 CLAIM2_CONTAINERS=2 create dra-pod
check-env pod11 pod11c0 DRA_BALLOON "$dev0"
check-env pod11 pod11c1 DRA_BALLOON "$dev0"
check-env pod11 pod11c2 DRA_BALLOON "$dev0"
report allowed
verify 'len(cpus["pod11c0"]) == 4' \
       'cpus["pod11c0"] == cpus["pod11c1"] == cpus["pod11c2"]' \
       'disjoint_sets(cpus["pod11c0"], cpus["pod2c0"], cpus["pod4c0"], cpus["pod5c0"])'
check-weight pod11 pod11c0 1000
check-weight pod11 pod11c1 1000
check-weight pod11 pod11c2 2000

vm-command "kubectl get deviceclasses,resourceslices,resourceclaims -o yaml" || :

cleanup

echo "OK: the balloons policy published DRA balloons and placed DRA and regular containers correctly"
