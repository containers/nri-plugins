helm-terminate
helm_config=$TEST_DIR/balloons.cfg helm-launch balloons

sleep 1

wait-config-status Success

host-command "$SCP $TEST_DIR/broken-balloons-config.yaml ${VM_HOSTNAME}:"
vm-command "kubectl apply -f broken-balloons-config.yaml"

sleep 1

wait-config-status Failure

helm-terminate
