# Test resource allocation / free on different container exit and
# restart scenarios.

terminate nri-resource-policy
launch nri-resource-policy

# Make sure all the pods in default namespace are cleared so we get a fresh start
vm-command "kubectl delete pods --all --now"

CONTCOUNT=1 CPU=1000m MEM=64M create guaranteed
report allowed
verify 'len(cpus["pod0c0"]) == 1'
pyexec "assert \"$(get-ctr-id pod0 pod0c0)\" in allocations"

out '### Crash and restart pod0c0'
vm-command "kubectl get pods pod0"
vm-command "kill -KILL \$(pgrep -f 'echo pod0c0')"
sleep 1
vm-command 'kubectl wait --for=condition=Ready pods/pod0'
vm-run-until --timeout 30 "pgrep -f 'echo pod0c0' > /dev/null 2>&1"
vm-command "kubectl get pods pod0"
report allowed
verify 'len(cpus["pod0c0"]) == 1'
pyexec "assert \"$(get-ctr-id pod0 pod0c0)\" in allocations"

out '### Exit and complete pod0c0 by killing "sleep inf"'
out '### => sh (the init process in the container) will exit with status 0'
vm-command "kubectl get pods pod0"
vm-command "kill -KILL \$(pgrep --parent \$(pgrep -f 'echo pod0c0' | head -1))"
sleep 1
vm-command "kubectl get pods pod0"
# pod0c0 process is not on vm anymore
verify '"pod0c0" not in cpus'
# pod0c0 is not allocated any resources on CRI-RM
( verify "\"$(get-ctr-id pod0 pod0c0)\" not in allocations") || {
    # pretty-print allocations contents
    pp allocations
    error "pod0c0 expected to disappear from allocations"
}

terminate nri-resource-policy
