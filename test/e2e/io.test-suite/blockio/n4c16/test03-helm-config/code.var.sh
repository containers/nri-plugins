# Configuration rollouts affect new containers while existing workloads retain
# their settings. Private runtime comparisons remain separate.
blockio-setup
blockio_helm_test=1
helm_config="$TEST_OUTPUT_DIR/helm-config.yaml"
vm-command-q 'source /var/tmp/nri-blockio-e2e.sh; blockio-helm-config 400 1000000' > "$helm_config" ||
    command-error 'generating initial Helm values failed'
helm_config="$helm_config" helm-launch blockio

vm-command "kubectl run blockio-live --image=docker.io/library/busybox:1.37 --restart=Never \
    --labels=app=nri-blockio-e2e --annotations=blockio.resources.beta.kubernetes.io/pod=gold \
    --command -- sleep 3600" ||
    command-error 'creating Helm test workload failed'
vm-command 'kubectl wait pod/blockio-live --for=condition=Ready --timeout=120s' ||
    command-error 'waiting for Helm test workload failed'
blockio-vm blockio-helm-check 400 1000000 || command-error 'initial class was not applied'

# Change the class, then explicitly remove its throttle for new containers.
# The original workload keeps its container ID, weight, and throttle throughout.
for settings in '700 2000000' '100 max'; do
    read -r weight rate <<< "$settings"
    config="$TEST_OUTPUT_DIR/helm-config-$weight.yaml"
    vm-command-q "source /var/tmp/nri-blockio-e2e.sh; blockio-helm-config $weight $rate" > "$config" ||
        command-error 'generating replacement Helm values failed'
    helm-reconfigure blockio "$config"
    vm-command 'kubectl rollout status -n kube-system ds/nri-blockio --timeout=60s' ||
        command-error 'configuration rollout failed'
    blockio-vm blockio-helm-check 400 1000000 ||
        command-error 'configuration rollout changed an existing container'

    pod="blockio-new-$weight"
    vm-command "kubectl run $pod --image=docker.io/library/busybox:1.37 --restart=Never \
        --labels=app=nri-blockio-e2e --annotations=blockio.resources.beta.kubernetes.io/pod=gold \
        --command -- sleep 3600" || command-error 'creating replacement configuration workload failed'
    vm-command "kubectl wait pod/$pod --for=condition=Ready --timeout=120s" ||
        command-error 'waiting for replacement configuration workload failed'
    blockio-vm blockio-helm-check "$weight" "$rate" "$pod" ||
        command-error 'new container did not use the updated class'
done
blockio-vm blockio-helm-check 400 1000000 || command-error 'original container settings changed'
blockio-vm blockio-helm-check 700 2000000 blockio-new-700 ||
    command-error 'later rollout changed an existing container'
