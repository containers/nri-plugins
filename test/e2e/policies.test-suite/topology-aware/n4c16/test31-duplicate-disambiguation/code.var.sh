TESTNS=repro-dup
# Triggering transient duplicates is timing sensitive.
PODS=16
CONTAINERS=3

setup() {
    vm-command "kubectl create namespace $TESTNS"
    # Disable debug logging, otherwise log rotation may prevent finding remap lines.
    helm_config=$(DEBUG_LOGGERS="none" instantiate helm-config.yaml) helm-launch topology-aware
}

cleanup() {
    vm-command "kubectl delete pods -n $TESTNS --all --now || :"
    vm-command 'pkill -9 -f "sleep inf"'
    vm-command 'pkill -9 -f "echo pod"'
    vm-command "kubectl delete namespace $TESTNS --now || :"
    helm-terminate
}

create-containers() {
    n=$PODS CONTCOUNT=$CONTAINERS CPUREQ=10m MEMREQ=50M CPULIM=10m namespace=$TESTNS wait='' create burstable
    # Wait at least a quarter of containers are running to have
    # many ocontainers in many different stages.
    vm-command "nc=0; lnc=-1
	       while (( nc < $(( PODS * CONTAINERS / 4 )) )); do
	           sleep 0.1
                   nc=\$(pgrep -f pod[0-9]+c[0-9]+ | wc -l)
		   [ \$lnc = \$nc ] || echo \$nc podXcY processes running
		   lnc=\$nc
               done"
}

kill-containers() {
    case ${k8scri:-containerd} in
        containerd)
            vm-command 'kill -STOP $(pidof containerd)
                       pkill -9 -f "sleep inf"
		       sleep 1
                       kill -9 $(pidof containerd)'

            ;;
        cri-o)
            vm-command 'kill -STOP $(pidof crio)
                       pkill -9 -f "sleep inf"
		       sleep 1
                       kill -9 $(pidof crio)'
            ;;
        *)
            error "Unknown runtime: $runtime"
            ;;
    esac
}

wait-containers-restart() {
    local statuses=""

    while ! [[ "$statuses" == "Running" ]]; do
        sleep 5
	if vm-command "kubectl logs -n kube-system ds/nri-resource-policy-topology-aware 2>&1 | grep -iE 'remap|keeping|duplicate'"; then
	    break
	fi
        vm-command "kubectl get pods -A --no-headers=true | tr -s '\t' ' '| cut -d ' ' -f4 | sort -u"
        statuses=$COMMAND_OUTPUT
    done
}

check-transient-duplicates-present() {
    # Check that we managed to trigger transient duplicate containers.
    if ! grep -q -E '(remap)|(keeping.*mapped)' <<< $COMMAND_OUTPUT; then
        echo "No remapping of containers found in the logs..."
        return 1
    fi
    return 0
}

check-remap-disambiguation() {
    # Verify that each duplicate was disambiguated by creation time.
    if grep -E '(remap)|(keeping.*mapped)' <<< $COMMAND_OUTPUT | \
            grep -v 'by creation time'; then
        grep -E '(remap)|(keeping.*mapped)' <<< $COMMAND_OUTPUT | \
            grep -v 'by creation time' | sed 's/^/INCORRECT: /g'
        error "Found some incorrectly remapped containers in the logs"
    fi

    echo "Only found correctly remapped containers in the logs..."
    grep -E '(remap)|(keeping.*mapped)' <<< $COMMAND_OUTPUT | sed 's/^/CORRECT: /g'
}

check-no-duplicate-allocations() {
    # Verify that we did not end up with duplicate allocation entries.
    if grep -q -i 'duplicate allocation entries' <<< $COMMAND_OUTPUT; then
        grep -i 'duplicate allocation entries' <<< $COMMAND_OUTPUT | sed 's/^/DUPLICATE: /g'
        error "Found duplicate allocation entries in the logs"
    fi
}

pull-logs() {
    local cnt=0
    while [ $cnt -lt 5 ]; do
        vm-command "kubectl logs -n kube-system ds/nri-resource-policy-topology-aware 2>&1 | grep -iE 'remap|keeping|duplicate|unable'"
        if grep -q 'unable to retrieve container logs for' <<< $COMMAND_OUTPUT; then
            echo "Unable to retrieve policy logs, retrying..."
            sleep 3
            let cnt=$ctn+1
        else
            return 0
        fi
    done
    return 1
}

check-logs() {
    if ! pull-logs; then
        echo "Failed to pull policy logs..."
        return 1
    fi

    if ! check-transient-duplicates-present; then
        return 1
    fi

    check-remap-disambiguation
    check-no-duplicate-allocations

    return 0
}

cleanup
setup
create-containers

retries=0
while true; do
    kill-containers
    wait-containers-restart

    if check-logs; then
        break
    fi

    echo "Retrying..."
    let retries=$retries+1

    if [ $retries -ge 5 ]; then
        echo "Max retries ($retries) reached, could not reproduce the issue, giving up"
        break
    fi

    # Re-create all pods, otherwise CrashLoopBackup prevents finding remaps.
    cleanup
    setup
    create-containers
done

cleanup
