# Test CPU and memory affinity to devices using extended resources
# published by a fake device plugin and topology hints acquired from
# the pod resource API.

cleanup() {
    stop-fake-device-plugins
    delete-pods --all
    helm-terminate || :
}

cleanup

# A single FPGA on NUMA node 3: whoever gets it has to run there.
start-fake-device-plugin fake-fpga 'resourceName: acme.com/fpga
devices:
- id: acmefpga-0-numa3
  numaNodes: [3]'

# One TPU per NUMA node, so that four containers asking for one each get four
# devices on four different nodes.
start-fake-device-plugin fake-tpu 'resourceName: tech.com/tpu
devices:
- id: tcomtpu-0-numa0
  numaNodes: [0]
- id: tcomtpu-1-numa1
  numaNodes: [1]
- id: tcomtpu-2-numa2
  numaNodes: [2]
- id: tcomtpu-3-numa3
  numaNodes: [3]'

# A single NIC on the two NUMA nodes of package 0. One device rather than one
# per package for the same reason as the FPGA above: with a device on each
# package the test could not name the one the container has to land on.
start-fake-device-plugin fake-nic 'resourceName: telco.com/nic
devices:
- id: telconic-0-numas01
  numaNodes: [0,1]'

helm_config=$(POD_RESOURCE_API=true instantiate helm-config.yaml) helm-launch topology-aware

wait-node-resource --allocatable --timeout 60 --interval 1 acme.com/fpga 1 \
    "the fake FPGA device plugin did not publish its device"
wait-node-resource --allocatable --timeout 60 --interval 1 tech.com/tpu 4 \
    "the fake TPU device plugin did not publish its devices"
wait-node-resource --allocatable --timeout 60 --interval 1 telco.com/nic 1 \
    "the fake NIC device plugin did not publish its device"

# One container with the only FPGA there is, picking its CPU by the hint of
# that device. The device is on NUMA node 3, so the container has to be there.
ANN0='pick-resources-by-hints.resource-policy.nri.io/pod: "true"' \
    EXTREQ='acme.com/fpga: "1"' EXTLIM='acme.com/fpga: "1"' \
    CONTCOUNT=1 CPU=1 MEM=100M create guaranteed-extres
report allowed

verify 'node_ids(nodes["pod0c0"]) == {3}'

# The hints the policy scored the pools with have to be the ones the pod
# resources API provided.
plugin-log 'hints: map\[podresourceapi:' > /dev/null ||
    command-error "the policy scored no pod resource API hints"

# The CPUs were picked by those hints and not just taken from the pool the
# hints chose.
plugin-log 'effective pick resources by hints preference true' > /dev/null ||
    command-error "the pick-resources-by-hints annotation was not honoured"

# Four containers, one TPU each. The four TPUs are on four different NUMA
# nodes, so the containers have to end up on four different nodes too.
ANN0='pick-resources-by-hints.resource-policy.nri.io/pod: "true"' \
    EXTREQ='tech.com/tpu: "1"' EXTLIM='tech.com/tpu: "1"' \
    CONTCOUNT=4 CPU=1 MEM=100M create guaranteed-extres
report allowed

verify 'len(nodes["pod1c0"]) == 1' \
       'len(nodes["pod1c1"]) == 1' \
       'len(nodes["pod1c2"]) == 1' \
       'len(nodes["pod1c3"]) == 1' \
       'disjoint_sets(nodes["pod1c0"], nodes["pod1c1"], nodes["pod1c2"], nodes["pod1c3"])'

# A container with the only NIC spanning the two NUMA nodes of package 0,
# and two CPUs to pick out of the CPUs that hint names. Both CPUs have to
# come from that package.
ANN0='pick-resources-by-hints.resource-policy.nri.io/pod: "true"' \
    EXTREQ='telco.com/nic: "1"' EXTLIM='telco.com/nic: "1"' \
    CONTCOUNT=1 CPU=2 MEM=100M create guaranteed-extres
report allowed

verify 'node_ids(nodes["pod2c0"]).issubset({0, 1})' \
       'len(cpus["pod2c0"]) == 2'

cleanup
