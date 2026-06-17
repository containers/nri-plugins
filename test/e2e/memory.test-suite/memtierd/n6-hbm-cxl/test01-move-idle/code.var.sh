helm-terminate

helm_config=$(instantiate memtierd-move-idle.cm.yaml) helm-launch memtierd

# Replace the chart's hardcoded class configmap with one that defines
# a single class "move-idle-to-node2": memtierd moves idle pages of
# managed containers to NUMA node 2 (CPU-less, far from CPU nodes).
# No swap is used so this also works on the test VM which has swap
# disabled.
vm-put-file "$(instantiate memtierd-move-idle.cm.yaml)" memtierd-move-idle.cm.yaml
vm-command "kubectl -n kube-system apply -f memtierd-move-idle.cm.yaml" || \
    command-error "applying memtierd custom classes ConfigMap failed"
vm-command "kubectl -n kube-system rollout restart ds/nri-memtierd" || \
    command-error "rolling out memtierd ds restart failed"
vm-command "kubectl -n kube-system rollout status ds/nri-memtierd --timeout=60s" || \
    command-error "memtierd ds did not become ready after restart"

# Three containers, two annotated with the migrate-idle class, one
# explicitly unmanaged via the per-container empty-class annotation.
ANN0='class.memtierd.nri.io: move-idle-to-node2' \
ANN1='class.memtierd.nri.io/c2-unmanaged: ""' \
NAME=pod0 \
    create memtierd-test-pod

# n2_pages CONTAINER -> sets n2_pages_result to total node-2 pages of
# PID 1 inside the container. Uses vm-command (verbose) so numa_maps
# rows are visible in the test log for debugging.
n2_pages() {
    local ctr="$1"
    vm-command "kubectl exec pod0 -c $ctr -- grep -o ' N2=[0-9]*' /proc/1/numa_maps || true"
    n2_pages_result=$(awk -F= 'BEGIN{s=0} /N2=/ {s+=$2} END {print s}' <<<"$COMMAND_OUTPUT")
}

# Poll memtierd's progress instead of sleeping a fixed amount. 10 MB
# random data, ~13.5 MB when base64 encoded => ~3400 4 KiB pages.
# Wait until both managed containers have at least 2500 pages (~10 MB)
# on node 2; abort the wait after ~25 s. The unmanaged container is
# verified afterwards (it must stay near zero).
deadline=$(( $(date +%s) + 25 ))
managed0=0; managed1=0
while (( $(date +%s) < deadline )); do
    n2_pages c0-managed; managed0=$n2_pages_result
    n2_pages c1-managed; managed1=$n2_pages_result
    echo "poll: node2 pages c0-managed=$managed0 c1-managed=$managed1"
    if (( managed0 >= 2500 && managed1 >= 2500 )); then
        break
    fi
    sleep 2
done
n2_pages c2-unmanaged; unmanaged=$n2_pages_result

echo "final node2 pages: c0-managed=$managed0 c1-managed=$managed1 c2-unmanaged=$unmanaged"

(( managed0  >= 2500 )) || \
    error "c0-managed: only $managed0 pages migrated to node2 within 25s, expected >= 1000"
(( managed1  >= 2500 )) || \
    error "c1-managed: only $managed1 pages migrated to node2 within 25s, expected >= 1000"
(( unmanaged <=  200 )) || \
    error "c2-unmanaged: $unmanaged pages on node2, expected <= 200 (should be unmanaged)"

vm-command "kubectl delete pod pod0 --wait=false --grace-period=1"
helm-terminate
