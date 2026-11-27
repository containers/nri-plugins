blockio-setup
helm_config=$(instantiate blockio.helm-config.yaml) helm-launch blockio

LABEL0='blockio-test: blockio' create besteffort
verify-blockio pod0 pod0c0 io.bfq.weight 'default 100'
verify-blockio pod0 pod0c0 io.max ''

ANN0='blockio.resources.beta.kubernetes.io/pod: bronze' \
ANN1='blockio.resources.beta.kubernetes.io/container.pod1c1: gold' \
ANN2='blockio.resources.beta.kubernetes.io/container.pod1c2: ""' \
LABEL0='blockio-test: blockio' CONTCOUNT=3 create besteffort

verify-blockio pod1 pod1c0 io.bfq.weight "default 100
${blockio_devnums[0]} 50
${blockio_devnums[1]} 200"
verify-blockio pod1 pod1c0 io.max "${blockio_devnums[0]} rbps=50000000 wbps=10485760 riops=10000 wiops=5000
${blockio_devnums[1]} rbps=25000000 wbps=20000000 riops=2000 wiops=5000"
verify-blockio pod1 pod1c1 io.bfq.weight 'default 400'
verify-blockio pod1 pod1c1 io.max ''
verify-blockio pod1 pod1c2 io.bfq.weight 'default 100'
verify-blockio pod1 pod1c2 io.max ''

for class in unknown empty zero; do
    ANN0="blockio.resources.beta.kubernetes.io/pod: $class" \
    LABEL0='blockio-test: blockio' NAME="blockio-$class" create besteffort
    verify-blockio "blockio-$class" "blockio-${class}c0" io.bfq.weight 'default 100'
    verify-blockio "blockio-$class" "blockio-${class}c0" io.max ''
done
