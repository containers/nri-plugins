helm-terminate
helm_config=$TEST_DIR/balloons-rdt.cfg helm-launch balloons

cleanup() {
    vm-command "kubectl delete pods --all --now"
    helm-terminate
    vm-command "rm -f /var/lib/nri-resource-policy/cache" || true
}
cleanup

# pod0c{0,1,2}: one container per free L2 group
CPUREQ="1500m" MEMREQ="100M" CPULIM="1500m" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io/container.pod0c1: balloon-gold" CONTCOUNT=3 create balloons-busybox
report allowed

breakpoint
echo "DELME: forced to fail to avoid cleanup
exit 1

cleanup
