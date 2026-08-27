OTEL_LOGS=/tmp/otel/data/otel-export.out

cleanup() {
    always-cleanup
    delete-pods --all
    helm-terminate || :
    vm-command "mkdir -p /tmp/otel/data && chmod a+rw /tmp/otel/data"
    vm-command "rm -f $OTEL_LOGS && touch -f $OTEL_LOGS && chmod a+rw $OTEL_LOGS"
}

# We better always clean up the otel collector. It takes an extra 750m CPU
# request allocation from the reserved/kube-system CPUs which is normally
# enough alone to exhaust our CPU reservation. Otherwise any subsequent
# tests, or typically subsequent test runs against the same VM, might have
# false positives due to the extra lingering CPU allocation.
always-cleanup() {
    vm-command "kubectl delete -f otel-collector.yaml" || :
}

trap always-cleanup EXIT

cleanup

vm-put-file $(instantiate otel-collector.yaml) otel-collector.yaml
vm-command "kubectl apply -f otel-collector.yaml"
vm-command "kubectl -n kube-system wait deployments/otel-collector --for=condition=Available --timeout=300s"

helm_config=$(instantiate custom-config.yaml) helm-launch topology-aware

pod=pod0
CONTCOUNT=4 create besteffort

vm-command "kubectl wait --timeout=5s --for=condition=Ready pods/$pod"

cnt=0
while [ $cnt -lt 5 ]; do
   vm-command "crictl ps | grep ${pod}c3 | grep Running"
   if grep -q Running <<< $COMMAND_OUTPUT; then
       break
   fi
   sleep 1
   let cnt=$cnt+1
done

cnt=0
while [ $cnt -lt 5 ]; do
    missing=""
    for ctr in ${pod}c0 ${pod}c1 ${pod}c2 ${pod}c3; do
        echo "verifying logs for default/$pod/$ctr..."
        vm-command-q "cat $OTEL_LOGS" | \
            jq '.resourceLogs[].scopeLogs[].logRecords[].body.stringValue' | \
                grep -q "CreateContainer default/$pod/$ctr" || {
            echo "----- Collected otel logs -----"
            vm-command-q "cat $OTEL_LOGS"
            echo "-----     End of logs     -----"
            info "expected CreateContainer 'default/$pod/$ctr' log record not found for $ctr"
            missing="${missing:+ }$ctr"
            break
        }
    done
    if [ -z "$missing" ]; then
        break
    fi
    sleep 1
    let cnt=cnt+1
done

if [ -n "$missing" ]; then
    error "expected CreateContainer log record not found for $missing"
fi

cleanup
