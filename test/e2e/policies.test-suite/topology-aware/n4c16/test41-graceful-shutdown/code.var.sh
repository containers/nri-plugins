# This test verifies that a signalled plugin shuts down in an orderly manner:
# the resource manager stops its subsystems, in particular its NRI connection to
# the runtime, instead of the process vanishing from under them.

# The plugin binary in the container. The pattern is anchored, so that it cannot
# match anything else which happens to mention the plugin on its command line.
plugin_binary='^/bin/nri-resource-policy-topology-aware'

pod=""

cleanup() {
    helm-terminate || :
}

plugin-pod() {
    vm-command-q "kubectl -n kube-system get pods \
                      -l app.kubernetes.io/name=$(plugin-daemonset) \
                      -o jsonpath='{.items[0].metadata.name}'"
}

plugin-container-status() {
    # Usage: plugin-container-status JSONPATH-SUFFIX
    vm-command-q "kubectl -n kube-system get pod $pod \
                      -o jsonpath='{.status.containerStatuses[0].$1}'" | tr -d '[:space:]'
}

cleanup
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

pod=$(plugin-pod)
[ -n "$pod" ] || error "failed to find the pod of the plugin"

# Signal the plugin the way the kubelet does when it terminates the container.
# Signalling the process instead of deleting the pod restarts the container in
# place, which keeps the log of the terminated instance readable.
vm-command "pkill -TERM -f '$plugin_binary'" ||
    command-error "failed to signal the plugin"

retry-until --timeout 60 --message "the plugin to restart" \
    '[ "$(plugin-container-status restartCount)" == "1" ]' ||
    error "the plugin did not restart after being signalled"

# Handling the signal has to take the plugin through stopping the resource
# manager, not just through stopping the config agent.
plugin-log --previous 'received signal terminated' ||
    error "the plugin did not report the signal it was sent"
plugin-log --previous 'resource-manager.*shutting down' ||
    error "the resource manager was not stopped on shutdown"
plugin-log --previous 'nri-plugin.*stopping plugin' ||
    error "the NRI connection was not closed on shutdown"

# A plugin which runs its shutdown path to the end exits normally. One which the
# signal kills instead does not.
exit_code=$(plugin-container-status lastState.terminated.exitCode)
[ "$exit_code" == "0" ] ||
    error "the signalled plugin exited with $exit_code instead of 0"

cleanup

echo "OK: the signalled plugin shut down in an orderly manner"
