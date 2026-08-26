# Test CPU affinity to devices published by device plugins and queried
# from kubelet's PodResourcesAPI.

helm-terminate
helm_config=$TEST_DIR/balloons-podresources.cfg helm-launch balloons

cleanup() {
    vm-command 'pidof fake-device-plugin && kill $(pidof fake-device-plugin) && sleep 1'
    delete-pods --all
}

# verify-podres-locality RESOURCE CONTAINER...
#
# Reads the balloons policy debug log to find, for each CONTAINER, the
# NUMA node(s) of the device instance matching RESOURCE that the
# kubelet device manager assigned to it (the log line is emitted by
# containerDeviceCpus when it resolves a "podresourceapi:" hint). It
# then verifies that all CPUs the container is allowed to run on belong
# to those NUMA node(s), that is, that the balloon's CPUs really landed
# close to the assigned device.
verify-podres-locality() {
    local resource=$1
    shift
    plugin-log "pod-resource device \"$resource\"" > /dev/null \
        || command-error "no device-locality log lines for resource $resource"
    local log="$COMMAND_OUTPUT"
    local ctr
    for ctr in "$@"; do
        # The pod resources API reports a device that spans several
        # NUMA nodes as one entry per node, so collect the union of the
        # NUMA nodes over all log lines mentioning this container.
        local lines
        lines=$(grep "/$ctr matches" <<< "$log")
        [ -n "$lines" ] \
            || command-error "no device-locality log line for container $ctr (resource $resource)"
        local numas
        numas=$(sed -n 's/.*NUMA nodes \([0-9,]*\),* device IDs.*/\1/p' <<< "$lines" | tr ',' '\n' | grep -E '^[0-9]+$' | sort -un | tr '\n' ' ')
        [ -n "$numas" ] \
            || command-error "could not parse NUMA nodes for container $ctr from: $lines"
        local nodeset="" n
        for n in $numas; do
            nodeset="$nodeset\"node$n\","
        done
        out "### container $ctr got $resource on NUMA node(s): $numas"
        verify "len(nodes[\"$ctr\"]) > 0" \
               "nodes[\"$ctr\"].issubset({$nodeset})"
    done
}

# Install and (re)start fake-device-plugins
cleanup
vm-command "command -v fake-device-plugin" || {
    HOST_DEVICE_PLUGIN=$OUTPUT_DIR/fake-device-plugin

    [ -f "$HOST_DEVICE_PLUGIN" ] || \
        GOARCH=amd64 go build -o "$HOST_DEVICE_PLUGIN" "${TEST_DIR%%/test/e2e/*}/scripts/testing/fake-device-plugin/fake-device-plugin.go" || \
        error "failed to build $HOST_DEVICE_PLUGIN"

    vm-put-file "$HOST_DEVICE_PLUGIN" "/usr/local/bin/$(basename "$HOST_DEVICE_PLUGIN")"
}

vm-command "cat > fake-tpu.yaml <<EOF
resourceName: tech.com/tpu
devices:
- id: tcomtpu-0-s1-numa3
  numaNodes: [3]
- id: tcomtpu-1-s0-numa1
  numaNodes: [1]
- id: tcomtpu-2-s0-numa0
  numaNodes: [0]
- id: tcomtpu-3-s1-numa2
  numaNodes: [2]
EOF
" || command-error "failed to create fake-tpu.yaml"

vm-command "fake-device-plugin -config fake-tpu.yaml >& fake-tpu.output &"

vm-command "cat > fake-nic.yaml <<EOF
resourceName: telco.com/nic
devices:
- id: telconics0-numas01
  numaNodes: [0,1]
- id: telconics1-numas23
  numaNodes: [2,3]
EOF
" || command-error "failed to create fake-nic.yaml"

vm-command "fake-device-plugin -config fake-nic.yaml >& fake-nic.output &"
sleep 1


# Wait until both fake device plugins have registered and their
# devices are Allocatable.
wait-node-resource --allocatable --timeout 60 --interval 1 tech.com/tpu 4 \
    "fake TPU device plugin did not publish its devices"
wait-node-resource --allocatable --timeout 60 --interval 1 telco.com/nic 2 \
    "fake NIC device plugin did not publish its devices"

# burstable containers
CPUREQ=2 CPULIM=4 MEMREQ=10M MEMLIM=50M \
       EXTREQ="telco.com/nic: \"1\"" \
       EXTLIM="telco.com/nic: \"1\"" \
       POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: near-nic" \
       CONTCOUNT=2 \
       create balloons-busybox
report allowed

# The two NIC-consuming containers matched "podresourceapi:*.com/nic".
# Each got a different telco.com/nic device, one on NUMA nodes {0,1}
# (package 0) and the other on NUMA nodes {2,3} (package 1). Verify that
# each container's CPUs landed on the NUMA node(s) of its assigned NIC,
# and that the two containers ended up on disjoint (different-package)
# node sets.
verify-podres-locality "telco.com/nic" pod0c0 pod0c1
verify 'disjoint_sets(nodes["pod0c0"], nodes["pod0c1"])'

# # Free the NIC balloons before the next phase so that the TPU pods
# # below can be placed on the NUMA nodes of their own devices without
# # competing for CPUs with the still-running NIC containers. Without
# # this, node2's only usable CPUs (10-11; cpu8 is off, cpu9 reserved)
# # are held by pod0c1's NIC balloon, leaving no room for the node2 TPU.
# vm-command "kubectl delete pod pod0 --now"
# vm-command "kubectl wait --for=delete pod/pod0 --timeout=30s" || true

declare -a EXTREQ=( "tech.com/tpu: \"1\"" "cpuclass.balloons.nri.io/pct-hp: \"1\"" )
declare -a EXTLIM=( "tech.com/tpu: \"1\"" "cpuclass.balloons.nri.io/pct-hp: \"1\"" )
CPUREQ=1 CPULIM=1 MEMREQ=10M MEMLIM=10M \
       POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: hp-near-tpu" \
       CONTCOUNT=4 \
       create balloons-busybox
report allowed

# The four TPU-consuming containers matched "podresourceapi:tech.com/*".
# There are four tech.com/tpu devices, one on each of NUMA nodes 0..3,
# so the four containers get four distinct devices. Verify that each
# HP balloon's single CPU landed on the NUMA node of its assigned TPU,
# and that all four ended up on distinct NUMA nodes.
verify-podres-locality "tech.com/tpu" pod1c0 pod1c1 pod1c2 pod1c3
verify 'disjoint_sets(nodes["pod1c0"], nodes["pod1c1"], nodes["pod1c2"], nodes["pod1c3"])'

# Sanity check (cf. test19-pct): the hp-near-tpu balloons use the
# pct-hp cpuClass, so their CPUs must have been associated to the PCT
# high-priority CLOS 0.
assert-cpu-clos '.*' 'CLOS 0' \
    "hp-near-tpu balloon CPUs were not associated to PCT HP CLOS 0"

vm-command "kubectl delete pods --all --now"

# Create pods that use both a tpu and a nic. Align pod2 near nic, pod3 near tpu.
declare -a EXTREQ=( "tech.com/tpu: \"1\"" "telco.com/nic: \"1\"" "cpuclass.balloons.nri.io/pct-hp: \"2\"" )
declare -a EXTLIM=( "tech.com/tpu: \"1\"" "telco.com/nic: \"1\"" "cpuclass.balloons.nri.io/pct-hp: \"2\"" )
CPUREQ=2 CPULIM=2 MEMREQ=10M MEMLIM=10M \
       POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: hp-near-tpu" \
       CONTCOUNT=1 \
       create balloons-busybox
report allowed
verify-podres-locality "tech.com/tpu" pod2c0

declare -a EXTREQ=( "tech.com/tpu: \"1\"" "telco.com/nic: \"1\"" )
declare -a EXTLIM=( "tech.com/tpu: \"1\"" "telco.com/nic: \"1\"" )
CPUREQ=1 CPULIM=1 MEMREQ=10M MEMLIM=10M \
       POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: near-nic" \
       CONTCOUNT=1 \
       create balloons-busybox
report allowed
verify-podres-locality "telco.com/nic" pod3c0

cleanup
helm-terminate
