blockio-setup
helm_config=$(BLOCKIO_RATE=1000000 instantiate blockio.helm-config.yaml) helm-launch blockio

verify-pod() {
    local pod=$1 weight=$2 rate=$3 limits=''
    [ "$rate" = 0 ] || limits="${blockio_devnums[0]} rbps=$rate wbps=max riops=max wiops=max"
    verify-blockio "$pod" "${pod}c0" io.bfq.weight "default $weight"
    verify-blockio "$pod" "${pod}c0" io.max "$limits"
}

ANN0='blockio.resources.beta.kubernetes.io/pod: gold' \
LABEL0='blockio-test: blockio' NAME=blockio-live create besteffort
verify-pod blockio-live 400 1000000

# Rollouts change new containers while existing containers retain their settings.
for settings in '700 2000000' '100 0'; do
    read -r weight rate <<< "$settings"
    config=$(BLOCKIO_WEIGHT=$weight BLOCKIO_RATE=$rate instantiate blockio.helm-config.yaml)
    helm-reconfigure blockio "$config"
    vm-command 'kubectl rollout status -n kube-system ds/nri-blockio --timeout=60s' ||
        command-error 'configuration rollout failed'
    verify-pod blockio-live 400 1000000

    ANN0='blockio.resources.beta.kubernetes.io/pod: gold' \
    LABEL0='blockio-test: blockio' NAME="blockio-new-$weight" create besteffort
    verify-pod "blockio-new-$weight" "$weight" "$rate"
done
verify-pod blockio-live 400 1000000
verify-pod blockio-new-700 700 2000000
