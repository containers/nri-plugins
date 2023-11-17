source "$(dirname "${BASH_SOURCE[0]}")/command.bash"

VM_PROMPT=${VM_PROMPT-"\e[38;5;11mroot@vm>\e[0m "}

error() {
    (echo ""; echo "error: $1" ) >&2
    exit 1
}

out() {
    if [ -n "$PV" ]; then
        speed=${speed-10}
        echo "$1" | $PV "$speed"
    else
        echo "$1"
    fi
    echo ""
}

vm-create-name() {
    local runtime=$1
    local topology=$2
    local distro=$3

    # Needs topology, distro and container runtime stack.
    case "${runtime}" in
        "containerd")
            ;;
        "crio")
            ;;
        *)
            error "unsupported runtime: \"${runtime}\""
            ;;
    esac

    echo "${topology}-${distro}-${runtime}"
}

vm-setup() {
    local output_dir="$1"
    local vmname="$2"
    local distro="$3"
    local topology_dir="$4"
    local topology="$5"
    local playbook="$output_dir"
    local inventory="$playbook/inventory"
    local vagrantdir="$output_dir"
    local files="$nri_resource_policy_src/test/e2e/files"
    local distro_name=$(printf '%s\n' "$distro" | sed -e 's/[\/&]/\\&/g')

    mkdir -p "$inventory"
    if [ ! -f "$inventory/vagrant.ini" ]; then
	sed "s/SERVER_NAME/$vmname/g" "$files/vagrant.ini.in" > "$inventory/vagrant.ini"
    fi

    VM_QEMU_CPUMEM=$(echo "$topology" | SEPARATED_OUTPUT_VARS=1 "$LIB_DIR/topology2qemuopts.py")
    if [ "$?" -ne  "0" ]; then
        error "error in topology"
    fi

    MACHINE=$(echo $VM_QEMU_CPUMEM | sed 's/MACHINE:-machine \([^|]*\).*/\1/g')
    CPU=$(echo $VM_QEMU_CPUMEM | sed 's/MACHINE:.*CPU:-smp \([^|]*\).*/\1/g')
    MEM=$(echo $VM_QEMU_CPUMEM | sed 's/MACHINE:.*CPU:.*MEM:-m \([^|]*\).*/\1/g')
    EXTRA_ARGS=$(echo $VM_QEMU_CPUMEM | sed 's/MACHINE:.*CPU:.*MEM:.*EXTRA:\([^|]*\).*/\1/g')

    if [ 0 == 1 ]; then
	echo "MACHINE: $MACHINE"
	echo "CPU: $CPU"
	echo "MEM: $MEM"
	echo "EXTRA: $EXTRA_ARGS"
    fi

    if [ ! -f "$vagrantdir/Vagrantfile" ]; then
	sed -e "s/SERVER_NAME/$vmname/g" \
	    -e "s/DISTRO/$distro_name/g" \
	    -e "s/QEMU_MACHINE/$MACHINE/" \
	    -e "s/QEMU_MEM/$MEM/" \
	    -e "s/QEMU_SMP/$CPU/" \
	    -e "s/QEMU_EXTRA_ARGS/$EXTRA_ARGS/" \
	    "$files/Vagrantfile.in" > "$vagrantdir/Vagrantfile.erb"
    fi

    if [ ! -f "$vagrantdir/Makefile" ]; then
	sed -e "s/SERVER_NAME/$vmname/g" -e "s/DISTRO/$distro_name/g" "$files/Makefile.in" > "$vagrantdir/Makefile"
    fi

    if [ ! -f "$vagrantdir/env" ]; then
	# Get a random port between 50023 - 52071 to be used to access the VM
	SSH_PORT="$[ $RANDOM % 2048 + 50023 ]"

	if [ ! -z "$proxy" ]; then
	    ESCAPED_PROXY=$(printf '%s\n' "$proxy" | sed -e 's/[\/&]/\\&/g')

	    sed -e "s/\#PROXY=\"\"/PROXY=\"$ESCAPED_PROXY\"/g" \
		-e "s/\#HTTP/HTTP/g" \
		-e "s/DNS_NAMESERVER=\"\"/DNS_NAMESERVER=\"$dns_nameserver\"/g" \
		-e "s/DNS_SEARCH_DOMAIN=\"\"/DNS_SEARCH_DOMAIN=\"$dns_search_domain\"/g" \
		-e "s/SSH_PORT=/SSH_PORT=$SSH_PORT/g" \
		"$files/env.in" > "$vagrantdir/env"
	else
	    sed -e "s/DNS_NAMESERVER=\"\"/DNS_NAMESERVER=\"$dns_nameserver\"/g" \
		-e "s/DNS_SEARCH_DOMAIN=\"\"/DNS_SEARCH_DOMAIN=\"$dns_search_domain\"/g" \
		-e "s/SSH_PORT=/SSH_PORT=$SSH_PORT/g" \
		"$files/env.in" > "$vagrantdir/env"
	fi
    fi

    (cd "$vagrantdir";
     # Make sure the vagrant plugins are installed
     make install

     if [ ! -d .vagrant ]; then
	 vagrant init --template Vagrantfile $distro || error "failed to vagrant init $distro"
     fi

     # If you want to force provisioning of already provisioned vm,
     # then you can set provision=1 when calling e2e test script.
     # Note that this is not recommended as at least kubeinit
     # cannot be called second time. But this could be used
     # if the provisioning failed before kubernetes was setup.
     if [ ! -z "$provision" ]; then
	 vagrant provision || error "failed to provision VM"
     fi

     vagrant up --provider qemu || error "failed to bring up VM"
     vagrant ssh-config > .ssh-config

     # Add hostname alias to the ssh config so that we can ssh
     # with shorter hostname "node"
     sed -i 's/^Host /Host node /' .ssh-config
    )

    mkdir -p "$COMMAND_OUTPUT_DIR"
    rm -f "$COMMAND_OUTPUT_DIR"/0*
}

vm-play() {
    local vm="$1"
    local playbook="$2"
    local vagrantdir="$3"

    (cd "$vagrantdir";
     ansible-playbook "$playbook" \
	  -i "${vm}," -u vagrant \
	  --private-key=".vagrant/machines/${vm}/libvirt/private_key" \
	  --ssh-common-args "-F .ssh-config" \
	  --extra-vars "cri_runtime=${k8scri} nri_resource_policy_src=${nri_resource_policy_src}"
    )
}

vm-nri-plugin-deploy() {
    local output_dir="$1"
    local vm_name="$2"
    local policy="$3"
    local vagrantdir="$output_dir"
    local playbook="$nri_resource_policy_src/test/e2e/playbook"

     # We do not setup NRI plugin during provisioning because provisioning is
     # only run once but we can execute the tests multiple times and we might
     # have to use newer version of nri plugin.
    vm-play "$vm_name" "$playbook/nri-${policy}-plugin-deploy.yaml" "$vagrantdir"
    if [ $? -ne 0 ]; then
        error "Cannot deploy $policy nri plugin"
    fi
}

vm-command() { # script API
    # Usage: vm-command COMMAND
    #
    # Execute COMMAND on virtual machine as root.
    # Returns the exit status of the execution.
    # Environment variable COMMAND_OUTPUT contains what COMMAND printed
    # in standard output and error.
    #
    # Examples:
    #   vm-command "kubectl get pods"
    #   vm-command "whoami | grep myuser" || command-error "user is not myuser"
    command-start "vm" "$VM_PROMPT" "$1"
    if [ "$2" == "bg" ]; then
        ( $SSH "$VM_HOSTNAME" sudo bash -l <<<"$COMMAND" 2>&1 | command-handle-output ;
          command-end "${PIPESTATUS[0]}"
        ) &
        command-runs-in-bg
    else
        $SSH "$VM_HOSTNAME" sudo bash -l <<<"$COMMAND" 2>&1 | command-handle-output ;
        command-end "${PIPESTATUS[0]}"
    fi
    return "$COMMAND_STATUS"
}

vm-command-q() {
    $SSH "$VM_HOSTNAME" sudo bash -l <<<"$1"
}

vm-run-until() { # script API
    # Usage: vm-run-until [--timeout TIMEOUT] CMD
    #
    # Keep running CMD (string) until it exits successfully.
    # The default TIMEOUT is 30 seconds.
    local cmd timeout invalid
    timeout=30
    while [ "${1#-}" != "$1" ] && [ -n "$1" ]; do
        case "$1" in
            --timeout)
                timeout="$2"
                shift; shift
                ;;
            *)
                invalid="${invalid}${invalid:+,}\"$1\""
                shift
                ;;
        esac
    done
    if [ -n "$invalid" ]; then
        error "invalid options: $invalid"
        return 1
    fi
    cmd="$1"
    if ! vm-command-q "retry=$timeout; until $cmd; do retry=\$(( \$retry - 1 )); [ \"\$retry\" == \"0\" ] && exit 1; sleep 1; done"; then
        error "waiting for command \"$cmd\" to exit successfully timed out after $timeout s"
    fi
}

vm-wait-process() { # script API
    # Usage: vm-wait-process [--timeout TIMEOUT] [--pidfile PIDFILE] PROCESS
    #
    # Wait for a PROCESS (string) to appear in process list (pidof output).
    # If pidfile parameter is given, we also check that the process has that file open.
    # The default TIMEOUT is 30 seconds.
    local process timeout pidfile invalid
    timeout=30
    while [ "${1#-}" != "$1" ] && [ -n "$1" ]; do
        case "$1" in
            --timeout)
                timeout="$2"
                shift 2
                ;;
            --pidfile)
                pidfile="$2"
                shift 2
                ;;
            *)
                invalid="${invalid}${invalid:+,}\"$1\""
                shift
                ;;
        esac
    done
    if [ -n "$invalid" ]; then
        error "invalid options: $invalid"
        return 1
    fi
    process="$1"
    vm-run-until --timeout "$timeout" "pidof \"$process\" > /dev/null" || error "timeout while waiting $process"

    # As we first wait for the process, and then wait for the pidfile (if enabled)
    # we might wait longer than expected. Accept that anomaly atm.
    if [ ! -z "$pidfile" ]; then
	vm-run-until --timeout $timeout "[ ! -z \"\$(fuser $pidfile 2>/dev/null)\" ]" || error "timeout while waiting $pidfile"
	vm-run-until --timeout $timeout "[ \$(fuser $pidfile 2>/dev/null) -eq \$(pidof $process) ]" || error "timeout while waiting $process and $pidfile"
    fi
}

vm-wait-pod-regexp() {
    # Usage: [VAR=VALUE] vm-wait-pod-regexp <pod-name-with-regexp>
    #
    # Wait until pod (found using regexp) is created and ready.
    #
    # Parameters:
    #   pod-name-with-regexp: pod name, for example "nri-resource-policy-"
    #   would find the first pod that contains "nri-resource-policy-" string.
    #
    # Optional parameters (VAR=VALUE):
    #   namespace: namespace to which instances are checked
    #   wait: condition to be waited for (see kubectl wait --for=condition=).
    #         If empty (""), skip waiting. The default is wait="Ready".
    #   wait_t: wait timeout in seconds. The default is wait_t=240.
    local namespace_args
    local wait=${wait-Ready}
    local wait_t=${wait_t-240}

    if [ -n "${namespace:-}" ]; then
        namespace_args="-n $namespace"
    else
        namespace_args=""
    fi

    pod_regexp="$1"

    # Rudimentary wait as "kubectl wait" will timeout immediately if pod is not yet there.
    vm-run-until --timeout "$wait_t" "kubectl get pods $namespace_args | grep -q $pod_regexp" || error "timeout while waiting $pod_regexp"

    local POD="$(vm-command-q "kubectl get pods $namespace_args | awk '/${pod_regexp}/ { print \$1 }'")"
    if [ -z "$POD" ]; then
        command-error "Pod $pod_regexp not found"
    fi

    #vm-command "kubectl wait --timeout=${wait_t}s --for=condition=${wait} $namespace_args pod/$POD" >/dev/null 2>&1 ||
    #    command-error "waiting for ${POD} to become ready timed out"
    vm-command "kubectl wait --timeout=${wait_t}s --for=condition=${wait} $namespace_args pod/$POD" >/dev/null 2>&1
    ret=$?

    echo "$POD"

    return $ret
}

vm-put-file() { # script API
    # Usage: vm-put-file [--cleanup] [--append] SRC-HOST-FILE DST-VM-FILE
    #
    # Copy SRC-HOST-FILE to DST-VM-FILE on the VM, removing
    # SRC-HOST-FILE if called with the --cleanup flag, and
    # appending instead of copying if the --append flag is
    # specified.
    #
    # Example:
    #   src=$(mktemp) && \
    #       echo 'Ahoy, Matey...' > $src && \
    #       vm-put-file --cleanup $src /etc/motd
    local cleanup append invalid
    while [ "${1#-}" != "$1" ] && [ -n "$1" ]; do
        case "$1" in
            --cleanup)
                cleanup=1
                shift
                ;;
            --append)
                append=1
                shift
                ;;
            *)
                invalid="${invalid}${invalid:+,}\"$1\""
                shift
                ;;
        esac
    done
    if [ -n "$cleanup" ] && [ -n "$1" ]; then
        # shellcheck disable=SC2064
        trap "rm -f \"$1\"" RETURN EXIT
    fi
    if [ -n "$invalid" ]; then
        error "invalid options: $invalid"
        return 1
    fi
    [ "$(dirname "$2")" == "." ] || vm-command-q "[ -d \"$(dirname "$2")\" ]" || vm-command "mkdir -p \"$(dirname "$2")\"" ||
        command-error "cannot create vm-put-file destination directory to VM"
    host-command "$SCP \"$1\" ${VM_HOSTNAME}:\"vm-put-file.${1##*/}\"" ||
        command-error "failed to copy file to VM"
    if [ -z "$append" ]; then
        vm-command "mv \"vm-put-file.${1##*/}\" \"$2\"" ||
            command-error "failed to rename file"
    else
        vm-command "touch \"$2\" && cat \"vm-put-file.${1##*/}\" >> \"$2\" && rm -f \"vm-put-file.${1##*/}\"" ||
            command-error "failed to append file"
    fi
}

vm-nri-resource-policy-pod-name() {
    echo "$(namespace=kube-system wait_t=5 vm-wait-pod-regexp nri-resource-policy-)"
}

port_forward_log_file=/tmp/nri-resource-policy-port-forward

vm-port-forward-enable() {
    local pod_name=$(vm-nri-resource-policy-pod-name)

    vm-port-forward-disable

    vm-command "kubectl port-forward $pod_name 8891:8891 -n kube-system > $port_forward_log_file 2>&1 &"
}

vm-port-forward-disable() {
    vm-command "fuser --kill $port_forward_log_file 2>/dev/null || :"
}

vm-start-log-collection() {
    local log_file="${log_file:-nri-resource-policy.output.txt}"
    local log_args="$*"

    log_file="$log_file" vm-stop-log-collection
    vm-command "kubectl logs -f $log_args >$log_file 2>&1 &"
}

vm-stop-log-collection() {
    local log_file="${log_file:-nri-resource-policy.output.txt}"
    vm-command "fuser --kill $log_file 2>/dev/null || :"
}
