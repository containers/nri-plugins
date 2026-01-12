# Test balloons that are composed of other balloons.

helm-terminate
helm_config=$TEST_DIR/balloons-composite.cfg helm-launch balloons

cleanup() {
    vm-command "kubectl delete -n kube-system pod pod2 --now; kubectl delete pods --all --now"
}

verify-nrt() {
    jqquery="$1"
    expected="$2"
    vm-command "kubectl get -n kube-system noderesourcetopologies.topology.node.k8s.io -o json | jq -r '$jqquery'"
    if [[ -n "$expected" ]]; then
        if [[ "$expected" != "$COMMAND_OUTPUT" ]]; then
            command-error "invalid output, expected: '$expected'"
        fi
    fi
}

cleanup

CPUREQ="500m" MEMREQ="100M" CPULIM="500m" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: balance-all-nodes" CONTCOUNT=2 create balloons-busybox
report allowed
verify 'len(cpus["pod0c0"]) == 4' \
       'len(cpus["pod0c1"]) == 4' \
       'nodes["pod0c0"] == nodes["pod0c1"] == {"node0", "node1", "node2", "node3"}'

verify-nrt '.items[0].zones[] | select (.name == "balance-all-nodes[0]")' # no check, print for debugging
verify-nrt '.items[0].zones[] | select (.name == "balance-all-nodes[0]") .attributes[] | select (.name == "excess cpus") .value' 3000m

# Balance a large workload on all NUMA nodes
CPUREQ="9" MEMREQ="100M" CPULIM="" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: balance-all-nodes" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod1c0"]) == 12' \
       'cpus["pod0c0"] == cpus["pod0c1"] == cpus["pod1c0"]' \
       'len(set.intersection(cpus["pod1c0"], {"cpu00", "cpu01", "cpu02", "cpu03"})) == 3' \
       'len(set.intersection(cpus["pod1c0"], {"cpu04", "cpu05", "cpu06", "cpu07"})) == 3' \
       'len(set.intersection(cpus["pod1c0"], {"cpu08", "cpu09", "cpu10", "cpu11"})) == 3' \
       'len(set.intersection(cpus["pod1c0"], {"cpu12", "cpu13", "cpu14", "cpu15"})) == 3' \
       'len(set.intersection(cpus["pod1c0"], {"cpu06", "cpu07"})) == 1' # cpu06 or cpu07 is reserved

verify-nrt '.items[0].zones[] | select (.name == "balance-all-nodes[0]")' # no check, print for debugging

CPUREQ="100m" MEMREQ="" CPULIM="100m" MEMLIM=""
namespace=kube-system create balloons-busybox
report allowed
verify 'cpus["pod2c0"].issubset({"cpu06", "cpu07"})' # allow either/both hyperthreads sharing the L0 cache

# Remove large pod. The size of the balanced-all-nodes[0] should drop from 12 to 4 CPUs.
# Verify the balance is still there.
vm-command "kubectl delete pod pod1 --now"
report allowed
verify 'len(cpus["pod0c0"]) == 4' \
       'cpus["pod0c0"] == cpus["pod0c1"]' \
       'len(set.intersection(cpus["pod0c0"], {"cpu00", "cpu01", "cpu02", "cpu03"})) == 1' \
       'len(set.intersection(cpus["pod0c0"], {"cpu04", "cpu05", "cpu06", "cpu07"})) == 1' \
       'len(set.intersection(cpus["pod0c0"], {"cpu08", "cpu09", "cpu10", "cpu11"})) == 1' \
       'len(set.intersection(cpus["pod0c0"], {"cpu12", "cpu13", "cpu14", "cpu15"})) == 1'

# Delete all pods. balanced-all-nodes[0] should stay, because of MinBalloons:1.
vm-command "kubectl delete pods --all --now; kubectl delete pod pod2 -n kube-system"

# Create two pods in separate balance-pkg1-nodes balloons, consuming all 3+3 free CPUs in node3+node4.
CPUREQ="1" MEMREQ="100M" CPULIM="" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: balance-pkg1-nodes" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod3c0"]) == 2' \
       'len(set.intersection(cpus["pod3c0"], {"cpu08", "cpu09", "cpu10", "cpu11"})) == 1' \
       'len(set.intersection(cpus["pod3c0"], {"cpu12", "cpu13", "cpu14", "cpu15"})) == 1'
verify-nrt '.items[0].zones[] | select (.name == "balance-pkg1-nodes[0]")' # no check, print for debugging

CPUREQ="4" MEMREQ="100M" CPULIM="" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: balance-pkg1-nodes" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod4c0"]) == 4' \
       'len(set.intersection(cpus["pod4c0"], {"cpu08", "cpu09", "cpu10", "cpu11"})) == 2' \
       'len(set.intersection(cpus["pod4c0"], {"cpu12", "cpu13", "cpu14", "cpu15"})) == 2' \
       'disjoint_sets(cpus["pod4c0"], cpus["pod3c0"])'
verify-nrt '.items[0].zones[] | select (.name == "balance-pkg1-nodes[1]")' # no check, print for debugging

# Remove pods. Now composite balloons balance-pkg1-nodes[0] and
# balance-pkg1-nodes[1] should be deleted completely (in contrast to
# previously only downsizing balance-all-nodes), so
# balance-all-nodes[0] should be able to grow again.
vm-command "kubectl delete pods --all --now"

# Inflate balance-all-nodes[0] to the max.
CPUREQ="12" MEMREQ="100M" CPULIM="" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: balance-all-nodes" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod5c0"]) == 12' \
       'len(set.intersection(cpus["pod5c0"], {"cpu00", "cpu01", "cpu02", "cpu03"})) == 3' \
       'len(set.intersection(cpus["pod5c0"], {"cpu04", "cpu05", "cpu06", "cpu07"})) == 3' \
       'len(set.intersection(cpus["pod5c0"], {"cpu08", "cpu09", "cpu10", "cpu11"})) == 3' \
       'len(set.intersection(cpus["pod5c0"], {"cpu12", "cpu13", "cpu14", "cpu15"})) == 3'

cleanup
reset counters

# ComponentCreation enforces node-assigment in order 2-3-0-1
CPUREQ="1" MEMREQ="100M" CPULIM="" MEMLIM=""
CONTCOUNT=1
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: nodes2301"
create balloons-busybox
report allowed
verify 'nodes["pod0c0"] == {"node2"}'

create balloons-busybox
report allowed
verify 'nodes["pod1c0"] == {"node3"}'

create balloons-busybox
report allowed
verify 'nodes["pod2c0"] == {"node0"}'

create balloons-busybox
report allowed
verify 'nodes["pod3c0"] == {"node1"}'

vm-command "kubectl delete pod pod1 --now" # pod1 was running in balloon "node3"

create balloons-busybox
report allowed
verify 'nodes["pod4c0"] == {"node3"}'

vm-command "kubectl delete pods --all --now" # make sure all nodes have free CPUs

POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: new-on-node-1"
create balloons-busybox
report allowed
verify 'nodes["pod5c0"] == {"node1"}'

POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: balance-both-nodes-on-either-pkg"
# "node1" balloon is instantiated (once) indepently for pod5 as
# new-on-node-1's component balloon. Therefore "balance-balloons"
# componentCreation strategy of balance-both-nodes-on-either-pkg is
# expected to use component balance-pkg1-nodes instead of
# balance-pkg0-nodes: the latter would use "node0" and "node1"
# component balloons that already have one instance.
create balloons-busybox
report allowed
verify 'nodes["pod6c0"] == {"node2", "node3"}'

create balloons-busybox
report allowed
verify 'nodes["pod7c0"] == {"node0", "node1"}'

create balloons-busybox
report allowed
verify 'nodes["pod8c0"] == {"node2", "node3"}'

cleanup
reset counters
