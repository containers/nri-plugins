# Test updating the resources of running containers using in-place
# resizing of resources, using a subresource resize patch. This is
# enabled by default from Kubernetes 1.33.
#
# The tests only resize CPU. Memory is left alone, because shrinking
# a memory limit in place may need a restart depending on the resize
# policy of the container, and a restart would reallocate instead of
# updating the resources.

plugin_log_filter='UpdateContainer|updating .reallocating. container'

cleanup-test --helm
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify 'len(cpus["pod0c0"]) == 1'
cpus_before=$(container-cpus pod0 pod0c0)
out "### before the resize: pod0c0=$cpus_before"

# Grow the container from one exclusive CPU to two.
resize-cpu pod0 pod0c0 2

wait-assert-log-contains 'UpdateContainer with real resource changes' \
    "the runtime did not pass the resource update on to the plugin" 30
wait-assert-log-contains 'updating .reallocating. container' \
    "the policy did not update the container" 30
wait-cpuset-change pod0 pod0c0 "$cpus_before" \
    "pod0c0 kept the cpuset $cpus_before across a resize to 2 CPUs"

report allowed
verify 'len(cpus["pod0c0"]) == 2'
cpus_grown=$(container-cpus pod0 pod0c0)
out "### after growing: pod0c0=$cpus_grown"

# Shrink it back and check the CPU returns.
resize-cpu pod0 pod0c0 1
wait-cpuset-change pod0 pod0c0 "$cpus_grown" \
    "pod0c0 kept the cpuset $cpus_grown across a resize back to 1 CPU"

report allowed
verify 'len(cpus["pod0c0"]) == 1'

cleanup-test --helm
