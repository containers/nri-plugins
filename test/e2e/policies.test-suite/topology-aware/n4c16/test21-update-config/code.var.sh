# Verify that the policy reconfigures itself with containers running.
# Grants should be reinstated as such when possible. If not possible
# grants should be reallocated within the same pool if it has enough
# capacity.

ALLCPUS=0-15

# Restrict the log assertions to the lines about reconfiguring. A
# reconfiguration dumps every pool and every allocation, so without
# this we'd have too much logs for the expected messages to fit in
# the inspected tail.
plugin_log_filter='updating existing allocations|reinstate|reallocating resources'

cleanup-test --helm
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

# pod0c0 takes exclusive CPUs, pod1c0 a shared portion and pod2c0 reserved
# CPUs, so reconfiguring covers reinstating all three kinds. The kube-system
# pods hold most of the single reserved CPU, leaving about 100m of it, so
# pod2c0 asks only for 50m to still fit in the reserved pool.
CONTCOUNT=1 CPU=2 MEM=100M create guaranteed
CPUREQ=300m CPULIM=600m MEMREQ=100M MEMLIM=100M CONTCOUNT=1 create burstable
ANN0='prefer-reserved-cpus.resource-policy.nri.io/pod: "true"' \
    CONTCOUNT=1 CPU=50m MEM=100M create guaranteed
report allowed

excl_before=$(container-cpus pod0 pod0c0)
shared_before=$(container-cpus pod1 pod1c0)
reserved_before=$(container-cpus pod2 pod2c0)
out "### before reconfiguration: pod0c0=$excl_before pod1c0=$shared_before pod2c0=$reserved_before"

verify 'len(cpus["pod0c0"]) == 2' \
       'cpus["pod2c0"] == {"cpu00"}'

# Reconfigure without available resource changes. Grants should be
# reinstatable as such.
patch-policy-config '{"spec":{"colocatePods":true}}'
wait-config-status Success 30s
wait-assert-log-contains 'updating existing allocations' \
    "the policy did not reconfigure itself" 30
wait-assert-log-contains 'reinstated CPU grant of' \
    "the policy did not reinstate the grants of the running containers" 30
assert-log-not-contains 'failed to reinstate grants verbatim' \
    "the policy failed to reinstate the grants of the running containers"

# Make sure that containers got reinstated successfully and did not get
# restarted (and thus resources reallocated).
vm-command "kubectl get pods pod0 pod1 pod2 -o jsonpath='\
{range .items[*]}{.metadata.name}={.status.containerStatuses[0].restartCount} {end}'"
[ "$(grep -o '=0' <<< "$COMMAND_OUTPUT" | wc -l)" = 3 ] ||
    error "a container restarted across the reconfiguration: $COMMAND_OUTPUT"

report allowed
excl_after=$(container-cpus pod0 pod0c0)
shared_after=$(container-cpus pod1 pod1c0)
reserved_after=$(container-cpus pod2 pod2c0)
out "### after reconfiguration: pod0c0=$excl_after pod1c0=$shared_after pod2c0=$reserved_after"

[ "$excl_after" = "$excl_before" ] ||
    error "pod0c0 moved from $excl_before to $excl_after across a reconfiguration"
[ "$shared_after" = "$shared_before" ] ||
    error "pod1c0 moved from $shared_before to $shared_after across a reconfiguration"
[ "$reserved_after" = "$reserved_before" ] ||
    error "pod2c0 moved from $reserved_before to $reserved_after across a reconfiguration"

# Reconfigure making CPUs pod0c0 is running on unavailable. Reinstating
# the grants should fail, and the containers should be reallocated instead.
patch-policy-config "{\"spec\":{\"availableResources\":\
{\"cpu\":\"cpuset:$(cpulist-difference $ALLCPUS $excl_before | tr ' ' ',')\"}}}"
wait-config-status Success 30s
wait-assert-log-contains 'failed to reinstate grants verbatim' \
    "the policy reinstated a grant which cannot hold any more" 30
wait-assert-log-contains 'reallocating resources' \
    "the policy did not reallocate the containers" 30

report allowed
excl_moved=$(container-cpus pod0 pod0c0)
out "### after the shrinking reconfiguration: pod0c0=$excl_moved"
[ "$excl_moved" != "$excl_before" ] ||
    error "pod0c0 kept CPUs $excl_before which the new configuration took away"

# Try updating to an invalid configuration. It should get refused and
# running containers left exactly as they are.
excl_refused=$(container-cpus pod0 pod0c0)
patch-policy-config '{"spec":{"reservedResources":{"cpu":"750x"}}}'
wait-config-status Failure 30s
[ "$(container-cpus pod0 pod0c0)" = "$excl_refused" ] ||
    error "a refused configuration changed the allocation of pod0c0"

cleanup-test --helm
