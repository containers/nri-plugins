OTEL_LOGS=/tmp/otel/data/otel-export.out

cleanup() {
    vm-command "kubectl delete -f otel-collector.yaml" || :
    vm-command "kubectl delete pods --all" || :
    helm-terminate || :
    vm-command "mkdir -p /tmp/otel/data && chmod a+rw /tmp/otel/data"
    vm-command "rm -f $OTEL_LOGS && touch -f $OTEL_LOGS && chmod a+rw $OTEL_LOGS"
}

cleanup

vm-put-file $(instantiate otel-collector.yaml) otel-collector.yaml
vm-command "kubectl apply -f otel-collector.yaml"

helm_config=$(instantiate custom-config.yaml) helm-launch topology-aware

CONTCOUNT=4 create besteffort
vm-command 'kubectl wait --timeout=5s --for=condition=Ready pods/pod0'

pod=pod0
for ctr in ${pod}c0 ${pod}c1 ${pod}c2 ${pod}c3; do
    echo "verifying logs for default/$pod/$ctr..."
    vm-command-q "cat $OTEL_LOGS" | \
        jq '.resourceLogs[].scopeLogs[].logRecords[].body.stringValue' | \
            grep -q "CreateContainer default/$pod/$ctr" || \
        error "expected CreateContainer log record not found for $ctr"
done

cleanup
