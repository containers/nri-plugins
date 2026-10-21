# This test verifies that the plugin survives configuration updates while the
# runtime keeps sending it pod and container events.
#
# A configuration update pushes container updates to the runtime with the
# resource manager locked. The runtime holds its own NRI lock while it relays
# an event to the plugin, and it needs the same lock to apply the updates. If
# an event arrives in between, each side waits for the other until the runtime
# times out the request and drops the plugin, which then restarts.

reconfig_count=${reconfig_count:-50}

# The pods which keep the runtime busy are created and deleted on the VM, out
# of sight of the e2eish recorder, so this test cannot be replayed.
e2eish=0

cleanup() {
    vm-command "touch churn.stop; pkill -f churn.sh; rm -f churn.stop" || :
    delete-pods --all
    helm-terminate || :
}

plugin-restarts() {
    vm-command-q "kubectl -n kube-system get pods \
                      -l app.kubernetes.io/name=$(plugin-daemonset) \
                      -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}'" |
        tr -d '[:space:]'
}

cleanup
started=$(vm-command-q "date +%s")
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

# Keep a few pods of four containers coming and going on the node. Every pod
# makes the runtime send more than 20 events to the plugin. Waiting for each
# deletion keeps the pods from piling up faster than the kubelet removes them.
vm-command "$(cat <<'CHURN'
cat > churn.sh <<'SCRIPT'
i=0
while [ ! -e churn.stop ]; do
    kubectl create -f - >/dev/null <<POD
apiVersion: v1
kind: Pod
metadata:
  name: churn$i
spec:
  terminationGracePeriodSeconds: 1
  containers:
$(for c in 0 1 2 3; do
    echo "  - {name: c$c, image: quay.io/prometheus/busybox, imagePullPolicy: IfNotPresent, command: [sleep, inf]}"
done)
POD
    [ $i -ge 3 ] && kubectl delete pod churn$((i-3)) --now >/dev/null
    i=$((i+1))
done
SCRIPT
setsid nohup bash churn.sh >churn.log 2>&1 </dev/null &
CHURN
)"

retry-until --timeout 60 --message "the churn loop to create pods" \
    'vm-command-q "kubectl get pods -o name" | grep -q churn3' ||
    error "the churn loop did not create pods: $(vm-command-q 'cat churn.log')"

# Flip a setting which leaves the allocations as they are. The resource
# manager still pushes container updates after every reconfiguration, even
# when there are none, and that is enough for the deadlock. Stop at the first
# restart of the plugin.
vm-command "$(cat <<RECONF
for i in \$(seq 1 $reconfig_count); do
    [ \$((i % 2)) = 1 ] && v=true || v=false
    kubectl -n kube-system patch $(config-resource) --type=merge \
        -p "{\"spec\":{\"colocatePods\":\$v}}" >/dev/null || exit 1
    if [ \$((i % 10)) = 0 ]; then
        kubectl -n kube-system get pods -l app.kubernetes.io/name=$(plugin-daemonset) \
            -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' | grep -qx 0 || break
    fi
done
RECONF
)" || command-error "failed to update the configuration"

vm-command "touch churn.stop"

# Wait for the plugin to apply the last update. A deadlocked plugin is dropped
# and restarts, and the restarted plugin applies the update too.
timeout=120s wait-config-node-status $(config-resource)
wait-config-status Success

restarts=$(plugin-restarts)

if [ "$restarts" != "0" ]; then
    plugin-log --previous 'connection to NRI/runtime lost' || :
    vm-command "journalctl -u containerd --since @$started | grep 'closing plugin.*deadline exceeded'" || :
    error "the plugin restarted $restarts time(s) while reconfiguring under load"
fi

cleanup

echo "OK: the plugin survived $reconfig_count configuration updates under load"
