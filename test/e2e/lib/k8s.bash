# Kubernetes clusters of e2e VMs: commands in VMs other than the VM of the
# test, and reshaping the clusters of existing VMs during a test run.
#
# Every e2e VM is provisioned as a single-node cluster. vm-k8s-reinit makes
# other shapes of them, for instance one cluster of two VMs, and back:
#
#   vm-k8s-reinit "$VM2 $VM1"     # one cluster: VM2 control plane, VM1 worker
#   vm-k8s-reinit "$VM2" "$VM1"   # two single-node clusters again
#
# The VMs of a multi-node cluster talk over a VM network: the topologies of
# the VMs set vm_network (vm_network.var.sh), which gives each VM a second
# NIC (lib/vm.bash, vm-vmnet-*). The nodes use the addresses of that
# network; pods of different nodes reach each other through static routes
# between the per-node subnets of the bridge CNI.
#
# This file is sourced by lib/test.bash. The helpers take VMDIR, the output
# directory of a VM ($OUTPUT_DIR for the VM of the test), or a VM name,
# which is the name of its directory next to $OUTPUT_DIR.
#
# The helpers that reshape networks and clusters work on VMs only. They
# refuse to run against a host which is not a VM (vm-hw-mode).

K8S_KUBEADM_CONFIG=/root/kubeadm-config.yaml
K8S_REINIT_CONFIG=/root/kubeadm-reinit.yaml
K8S_REINIT_TIMEOUT="${K8S_REINIT_TIMEOUT:-600}"
K8S_CNI_FILE=/etc/cni/net.d/10-bridge.conf
K8S_CNI_TEMPLATE="$nri_resource_policy_src/test/e2e/files/10-bridge.conf.in"
K8S_NM_PROFILE=e2e-vmnet
# The short names of VMs in prompts and command output file names, by
# VMDIR: a suite fills it (vmdir-label).
declare -A VMDIR_LABELS=()

###
### Commands in any VM
###

vmdir-of() { # script API
    # Usage: vmdir-of VMNAME|VMDIR
    #
    # Print the output directory of a VM: VMDIR as it is, or the directory
    # of VMNAME next to $OUTPUT_DIR.
    case "$1" in
        */*) echo "${1%/}";;
        *) echo "$(dirname "$OUTPUT_DIR")/$1";;
    esac
}

vmdir-label() {
    # Usage: vmdir-label VMDIR
    #
    # Print the short name of a VM in prompts and command output file names:
    # its VMDIR_LABELS entry, else vm for the VM of the test and the VM name
    # for the others.
    if [ -n "${VMDIR_LABELS[$1]:-}" ]; then
        echo "${VMDIR_LABELS[$1]}"
    elif [ "$1" == "$OUTPUT_DIR" ]; then
        echo "vm"
    else
        vmdir-name "$1"
    fi
}

vmdir-name() {
    # Usage: vmdir-name VMDIR
    #
    # Print the name of the VM of VMDIR, the name of the directory: its host
    # name and Kubernetes node name.
    basename "$1"
}

vmdir-command() { # script API
    # Usage: vmdir-command VMDIR COMMAND
    #
    # Execute COMMAND as root in the VM of VMDIR, the way vm-command does in
    # the VM of the test: print the command and its output, set
    # COMMAND_OUTPUT and COMMAND_STATUS and return the exit status.
    local vmdir="$1" label
    if [ "$vmdir" == "$OUTPUT_DIR" ]; then
        vm-command "$2"
        return
    fi
    label="$(vmdir-label "$vmdir")"
    command-start "$label" "\e[38;5;13mroot@$label>\e[0m " "$2"
    ssh -F "$vmdir/.ssh-config" node sudo bash -l <<<"$COMMAND" 2>&1 | command-handle-output
    command-end "${PIPESTATUS[0]}"
    return "$COMMAND_STATUS"
}

vmdir-command-q() {
    # Usage: vmdir-command-q VMDIR COMMAND
    #
    # Execute COMMAND as root in the VM of VMDIR quietly, like vm-command-q.
    if [ "$1" == "$OUTPUT_DIR" ]; then
        vm-command-q "$2"
    else
        ssh -F "$1/.ssh-config" node sudo bash -l <<<"$2"
    fi
}

vmdir-put-file() { # script API
    # Usage: vmdir-put-file VMDIR SRC-HOST-FILE DST-VM-FILE
    #
    # Copy a file from the host to the VM of VMDIR, like vm-put-file.
    local vmdir="$1" src="$2" dst="$3" tmp
    if [ "$vmdir" == "$OUTPUT_DIR" ]; then
        vm-put-file "$src" "$dst"
        return
    fi
    tmp="vm-put-file.${src##*/}"
    host-command "scp -F \"$vmdir/.ssh-config\" \"$src\" node:\"$tmp\"" ||
        command-error "failed to copy $src to $(vmdir-name "$vmdir")"
    vmdir-command "$vmdir" "mkdir -p \"$(dirname "$dst")\" && mv \"$tmp\" \"$dst\"" ||
        command-error "failed to move $src to $dst in $(vmdir-name "$vmdir")"
}

vmdir-ssh-wait() {
    # Usage: vmdir-ssh-wait VMDIR [TIMEOUT]
    #
    # Wait until ssh to the VM of VMDIR works, TIMEOUT seconds at most
    # (default 180).
    local vmdir="$1"
    retry-until --timeout "${2:-180}" --interval 2 --message "ssh to $(vmdir-name "$vmdir")" \
        '[ -f "$vmdir/.ssh-config" ] && command timeout 15 ssh -F "$vmdir/.ssh-config" -o ConnectTimeout=5 node true'
}

vmdir-up() { # script API
    # Usage: vmdir-up VMDIR [HINT]
    #
    # Make sure the VM of VMDIR runs: start it with vagrant up
    # --no-provision, the way vm-setup does, if ssh does not reach it, and
    # check that it is the VM of VMDIR (its host name is the VM name). Fail
    # with HINT, how to create the VM, if there is no VM in VMDIR.
    local vmdir="$1" hint="${2:-create it first with test00-up of its topology}" name
    name="$(vmdir-name "$vmdir")"
    [ -f "$vmdir/Vagrantfile" ] && [ -d "$vmdir/.vagrant" ] ||
        error "no VM $name in $vmdir: $hint"
    if ! { [ -f "$vmdir/.ssh-config" ] && command timeout 30 ssh -F "$vmdir/.ssh-config" -o ConnectTimeout=5 node true; }; then
        echo "vmdir-up: $name is not reachable, starting it"
        ( cd "$vmdir" || exit 1
          # qemu creates the backing files of file-backed memory, not their
          # directories, and /tmp may have been emptied since VM creation.
          for mem_path in $(grep -o 'mem-path=[^,"]*' Vagrantfile | sed 's/^mem-path=//'); do
              mkdir -p "$(dirname "$mem_path")" || exit 1
          done
          vagrant up --no-provision --provider qemu || exit 1
          if [ ! -f .ssh-config ]; then
              vagrant ssh-config > .ssh-config || exit 1
              printf '  ControlMaster auto\n  ControlPersist 60\n  ControlPath /tmp/ssh-%%C\n' >> .ssh-config
              sed -i 's/^Host /Host node /' .ssh-config
          fi
        ) || error "cannot start the VM in $vmdir with vagrant up --no-provision"
        vmdir-ssh-wait "$vmdir" || error "ssh to the VM in $vmdir does not work after vagrant up"
    fi
    vmdir-command "$vmdir" "hostname" || command-error "cannot run commands in $name"
    [ "$COMMAND_OUTPUT" == "$name" ] ||
        error "the VM in $vmdir calls itself $COMMAND_OUTPUT, expected $name"
}

vmdir-restart() { # script API
    # Usage: vmdir-restart VMDIR
    #
    # Restart qemu of the VM of VMDIR, so that it starts with the qemu
    # arguments of its Vagrantfile, and wait for ssh.
    local vmdir="$1" name i
    name="$(vmdir-name "$vmdir")"
    if [ "$vmdir" == "$OUTPUT_DIR" ]; then
        timeout=300 vm-reboot || error "cannot restart $name"
        return 0
    fi
    vmdir-command "$vmdir" "sync; shutdown -h 0" || :
    for i in $(seq 60); do
        (cd "$vmdir" && vagrant status 2>/dev/null | grep -q running) || break
        sleep 2
    done
    if (cd "$vmdir" && vagrant status 2>/dev/null | grep -q running); then
        echo "vmdir-restart: $name did not shut down, vagrant halt"
        (cd "$vmdir" && vagrant halt) || :
        for i in $(seq 30); do
            (cd "$vmdir" && vagrant status 2>/dev/null | grep -q running) || break
            sleep 2
        done
        (cd "$vmdir" && vagrant status 2>/dev/null | grep -q running) &&
            error "cannot stop $name, stop it by hand"
    fi
    echo "vmdir-restart: $name stopped, starting it"
    vmdir-up "$vmdir"
}

vm-k8s-refuse-hw() {
    # Usage: vm-k8s-refuse-hw WHAT
    #
    # Fail the test, naming WHAT, if the run targets a host which is not a
    # VM.
    vm-hw-mode || return 0
    error "$1 reshapes the network and the Kubernetes cluster of VMs.
$(vm-hw-address) is not a VM. Run this on VMs of its own."
}

###
### VM network in the VM
###

vm-vmnet-ensure() { # script API
    # Usage: vm-vmnet-ensure VMDIR [NETWORK]
    #
    # Make sure the VM of VMDIR is on VM network NETWORK (default
    # $vm_network, the network of the topology of the test): record the
    # settings in its env file and its NIC in its Vagrantfile, restart the
    # VM once if its qemu has no such NIC yet, and configure the NIC in
    # the VM: a NetworkManager profile $K8S_NM_PROFILE with the address of
    # the VM, and the network in NO_PROXY of the login shell and of
    # containerd (kubectl would send requests for those addresses to the
    # proxy otherwise, and kubeadm copies NO_PROXY into the control plane).
    local vmdir="$1" network="${2:-$vm_network}" name mac ip cidr
    vm-k8s-refuse-hw vm-vmnet-ensure
    name="$(vmdir-name "$vmdir")"
    [ -n "$network" ] ||
        error "vm-vmnet-ensure: no VM network for $name: set vm_network (vm_network.var.sh of the topology)"
    vm-vmnet-record "$vmdir" "$network" || error "vm-vmnet-ensure: cannot record VM network $network for $name"
    vm-vmnet-patch-vagrantfile "$vmdir/Vagrantfile" "$vmdir" || error "vm-vmnet-ensure: cannot patch $vmdir/Vagrantfile"
    vm-vmnet-patch-vagrantfile "$vmdir/Vagrantfile.erb" "$vmdir" || error "vm-vmnet-ensure: cannot patch $vmdir/Vagrantfile.erb"
    mac=$(vm-vmnet-get "$vmdir" VMNET_MAC)
    ip=$(vm-vmnet-get "$vmdir" VMNET_IP)
    cidr="$VMNET_CIDR_PREFIX.0.0/$VMNET_PREFIX_LEN"
    if ! vmdir-command-q "$vmdir" "ip -br link | grep -qi ' $mac '"; then
        echo "vm-vmnet-ensure: $name has no NIC $mac of VM network $network yet: restarting it with the NIC"
        vmdir-restart "$vmdir"
        vmdir-command "$vmdir" "ip -br link | grep -i ' $mac '" ||
            error "vm-vmnet-ensure: $name has no NIC $mac after the restart, see the qemu arguments in $vmdir/Vagrantfile"
    fi
    vmdir-command "$vmdir" "set -e
dev=\$(ip -br link | awk 'tolower(\$3) == \"$mac\" {print \$1}')
[ -n \"\$dev\" ]
if ! nmcli -t -f NAME con show | grep -qx $K8S_NM_PROFILE; then
    nmcli con add type ethernet con-name $K8S_NM_PROFILE ifname \"\$dev\" 802-3-ethernet.mac-address $mac \\
        ipv4.method manual ipv4.addresses $ip/$VMNET_PREFIX_LEN ipv6.method disabled \\
        connection.autoconnect yes connection.autoconnect-priority 10
fi
if ! ip -br addr show dev \"\$dev\" | grep -q ' $ip/$VMNET_PREFIX_LEN'; then
    nmcli con modify $K8S_NM_PROFILE ipv4.addresses $ip/$VMNET_PREFIX_LEN
    nmcli con up $K8S_NM_PROFILE
fi
ip -br addr show dev \"\$dev\"
for f in /etc/environment /etc/profile.d/proxy.sh /etc/systemd/system/containerd.service.d/proxy.conf; do
    [ -f \"\$f\" ] || continue
    grep -q '$cidr' \"\$f\" || sed -i -E 's#((NO_PROXY|no_proxy)=\"[^\"]*)\"#\1,$cidr\"#' \"\$f\"
done" || command-error "vm-vmnet-ensure: cannot configure the NIC of VM network $network in $name"
    echo "vm-vmnet-ensure: $name is on VM network $network: $ip/$VMNET_PREFIX_LEN, $mac"
}

###
### Cluster shapes
###

vm-k8s-node-ip() { # script API
    # Usage: vm-k8s-node-ip VMDIR
    #
    # Print the address of the VM of VMDIR in its VM network (VMNET_IP in
    # its env file), the node address of the VM in a multi-node cluster.
    vm-vmnet-get "$1" VMNET_IP
}

vm-k8s-shape() { # script API
    # Usage: vm-k8s-shape VMDIR
    #
    # Print the role of the VM of VMDIR in its cluster, read in the VM:
    #   control-plane NODE;NODE;...  (NODE = name|InternalIP|Ready|taint keys)
    #   control-plane unreachable    (the API server does not answer)
    #   worker SERVER-URL            (the API server of its kubelet)
    #   none
    vmdir-command-q "$1" "if [ -f /etc/kubernetes/manifests/kube-apiserver.yaml ] && [ -f /etc/kubernetes/admin.conf ]; then
    nodes=\$(KUBECONFIG=/etc/kubernetes/admin.conf kubectl get nodes --request-timeout=10s -o jsonpath='{range .items[*]}{.metadata.name}{\"|\"}{.status.addresses[?(@.type==\"InternalIP\")].address}{\"|\"}{.status.conditions[?(@.type==\"Ready\")].status}{\"|\"}{.spec.taints[*].key}{\";\"}{end}' 2>/dev/null) || nodes=unreachable
    echo \"control-plane \$nodes\"
elif [ -f /etc/kubernetes/kubelet.conf ]; then
    echo \"worker \$(sed -n 's/^ *server: *//p' /etc/kubernetes/kubelet.conf | head -n 1)\"
else
    echo none
fi" 2>/dev/null
}

vm-k8s-cluster-dirs() {
    # Usage: vm-k8s-cluster-dirs CLUSTER
    #
    # Print the VM directories of CLUSTER, VM names or directories
    # separated by spaces or commas, one per line, control plane first.
    local vm
    for vm in ${1//,/ }; do
        vmdir-of "$vm"
    done
}

vm-k8s-cluster-in-shape() {
    # Usage: vm-k8s-cluster-in-shape CLUSTER
    #
    # Return 0 if CLUSTER has its shape: the control plane runs the API
    # server and lists exactly the nodes of CLUSTER, all Ready, none with
    # the control-plane taint; in a multi-node cluster every node has its
    # VM network address and every worker's kubelet talks to the control
    # plane at its VM network address. Print why not otherwise.
    local dirs cp shape vmdir names="" ips="" server
    mapfile -t dirs < <(vm-k8s-cluster-dirs "$1")
    cp="${dirs[0]}"
    for vmdir in "${dirs[@]}"; do
        names+="$(vmdir-name "$vmdir") "
        ips+="$(vm-k8s-node-ip "$vmdir") "
    done
    shape=$(vm-k8s-shape "$cp")
    if [ "${shape%% *}" != "control-plane" ] || [ "${shape#control-plane }" == "unreachable" ]; then
        echo "$(vmdir-name "$cp"): not a working control plane (${shape:0:60})"
        return 1
    fi
    python3 - "${shape#control-plane }" "$names" "$ips" <<'EOF' || return 1
import sys
nodes, names, ips = sys.argv[1], sys.argv[2].split(), sys.argv[3].split(" ")
items = {}
for node in filter(None, nodes.split(";")):
    name, addr, ready, taints = (node.split("|") + ["", "", ""])[:4]
    items[name] = (addr, ready, taints.split())
problems = []
if set(items) != set(names):
    problems.append("nodes %s, expected %s" % (sorted(items), sorted(names)))
for k, name in enumerate(names):
    if name not in items:
        continue
    addr, ready, taints = items[name]
    if ready != "True":
        problems.append("%s is not Ready" % name)
    if "node-role.kubernetes.io/control-plane" in taints:
        problems.append("%s has the control-plane taint" % name)
    want = ips[k] if k < len(ips) else ""
    if len(names) > 1 and addr != want:
        problems.append("%s has InternalIP %s, expected its VM network address %s" % (name, addr, want or "(none)"))
if problems:
    print("; ".join(problems))
    sys.exit(1)
EOF
    for vmdir in "${dirs[@]:1}"; do
        shape=$(vm-k8s-shape "$vmdir")
        server="https://$(vm-k8s-node-ip "$cp"):6443"
        if [ "$shape" != "worker $server" ]; then
            echo "$(vmdir-name "$vmdir"): ${shape:0:60}, expected a worker of $server"
            return 1
        fi
    done
    return 0
}

vm-k8s-reinit() { # script API
    # Usage: vm-k8s-reinit [--force] [--timeout SECS] CLUSTER...
    #
    # Make Kubernetes clusters of existing VMs. Each CLUSTER is a string of
    # VM names or VM output directories separated by spaces or commas: the
    # first VM is the control plane, the others are workers. Pods run on
    # every node: the control plane has no taint. A cluster that has its
    # shape already is left alone, unless --force. The others are rebuilt
    # as a whole: kubeadm reset in all their VMs, kubeadm init on the
    # control plane, kubeadm join on the workers. Rebuilding loses all
    # cluster state: tests deploy what they need after it.
    #
    # The VMs of a multi-node cluster need a VM network (vm_network, see
    # vm-vmnet-ensure): the nodes use their VM network addresses, and every
    # node has a bridge CNI subnet of its own, its pod CIDR, with routes to
    # the subnets of the other nodes. A single-node cluster is what
    # provisioning makes: the slirp address and the whole pod subnet. Every
    # cluster gets the kubeadm configuration that provisioning wrote in the
    # control plane VM ($K8S_KUBEADM_CONFIG): its feature gates and kubelet
    # settings. Every node gets the admin kubeconfig as /root/.kube/config
    # and as .kube/config of the login user. Takes a few minutes per rebuilt
    # cluster, seconds if every cluster has its shape.
    #
    # Examples:
    #   vm-k8s-reinit "$OUTPUT_DIR"                    # the VM of the test alone
    #   vm-k8s-reinit "$OUTPUT_DIR $VM1"               # the VM of the test control plane, VM1 worker
    #   vm-k8s-reinit "$OUTPUT_DIR" "$VM1"             # two single-node clusters
    local force="" timeout="$K8S_REINIT_TIMEOUT" cluster vmdir seen="" dirs why
    while [ $# -gt 0 ]; do
        case "$1" in
            --force) force=1; shift;;
            --timeout) timeout="$2"; shift 2;;
            *) break;;
        esac
    done
    [ $# -gt 0 ] || error "vm-k8s-reinit: no clusters"
    vm-k8s-refuse-hw vm-k8s-reinit
    for cluster in "$@"; do
        mapfile -t dirs < <(vm-k8s-cluster-dirs "$cluster")
        [ "${#dirs[@]}" -gt 0 ] || error "vm-k8s-reinit: empty cluster \"$cluster\""
        for vmdir in "${dirs[@]}"; do
            [[ " $seen " != *" $vmdir "* ]] || error "vm-k8s-reinit: $(vmdir-name "$vmdir") is in two clusters"
            seen+=" $vmdir"
            vmdir-up "$vmdir"
        done
    done
    for cluster in "$@"; do
        if [ -z "$force" ] && why=$(vm-k8s-cluster-in-shape "$cluster"); then
            echo "vm-k8s-reinit: cluster \"$(vm-k8s-cluster-names "$cluster")\" is in shape already"
            continue
        fi
        echo "vm-k8s-reinit: rebuilding cluster \"$(vm-k8s-cluster-names "$cluster")\"${why:+: $why}"
        vm-k8s-rebuild "$cluster" "$timeout"
        echo "vm-k8s-reinit: cluster \"$(vm-k8s-cluster-names "$cluster")\" ready"
    done
}

vm-k8s-cluster-names() {
    # Usage: vm-k8s-cluster-names CLUSTER
    #
    # Print the VM names of CLUSTER separated by spaces.
    local vmdir out=""
    for vmdir in $(vm-k8s-cluster-dirs "$1"); do
        out+="$(vmdir-name "$vmdir") "
    done
    echo "${out% }"
}

vm-k8s-rebuild() {
    # Usage: vm-k8s-rebuild CLUSTER TIMEOUT
    #
    # Rebuild CLUSTER (vm-k8s-reinit) from scratch.
    local cluster="$1" timeout="$2" dirs cp vmdir ip multi="" join cpip ips=() i why
    mapfile -t dirs < <(vm-k8s-cluster-dirs "$cluster")
    cp="${dirs[0]}"
    [ "${#dirs[@]}" -gt 1 ] && multi=1
    for vmdir in "${dirs[@]}"; do
        vmdir-command-q "$vmdir" "test -f $K8S_CNI_FILE || test -f $K8S_CNI_FILE.off" ||
            error "vm-k8s-reinit: $(vmdir-name "$vmdir") has no $K8S_CNI_FILE: only the bridge CNI is supported"
    done
    if [ -n "$multi" ]; then
        for vmdir in "${dirs[@]}"; do
            vm-vmnet-ensure "$vmdir"
            ips+=("$(vm-k8s-node-ip "$vmdir")")
        done
        [ "$(printf '%s\n' "${ips[@]}" | sort -u | wc -l)" == "${#ips[@]}" ] ||
            error "vm-k8s-reinit: VMs of cluster \"$cluster\" share a VM network address: ${ips[*]}"
        for ip in "${ips[@]:1}"; do
            vmdir-command "$cp" "ping -c 2 -W 2 $ip" ||
                command-error "vm-k8s-reinit: no inter-VM network: $(vmdir-name "$cp") cannot ping $ip"
        done
    fi
    for vmdir in "${dirs[@]}"; do
        vm-k8s-reset-node "$vmdir"
    done
    cpip="${ips[0]:-}"
    vm-k8s-init-cp "$cp" "$cpip"
    join="$K8S_JOIN_COMMAND"
    for ((i = 1; i < ${#dirs[@]}; i++)); do
        vm-k8s-join-worker "${dirs[$i]}" "$cp" "$join" "${ips[$i]}"
    done
    if [ -n "$multi" ]; then
        vm-k8s-routes "${dirs[@]}"
    fi
    vm-k8s-wait-cluster "$cp" "$timeout" "${dirs[@]}"
    why=$(vm-k8s-cluster-in-shape "$cluster") ||
        error "vm-k8s-reinit: cluster \"$cluster\" is not in shape after the rebuild: $why"
}

vm-k8s-kubeadm-value() {
    # Usage: vm-k8s-kubeadm-value VMDIR KEY
    #
    # Print the value of KEY, for instance criSocket or podSubnet, in
    # $K8S_KUBEADM_CONFIG of the VM of VMDIR. Return 1 if the VM has no such
    # file or the file has no KEY.
    local value
    value=$(vmdir-command-q "$1" "cat $K8S_KUBEADM_CONFIG" 2>/dev/null |
                sed -n "s/^ *$2: *\"\{0,1\}\([^\"]*\)\"\{0,1\} *\$/\1/p" | head -n 1)
    [ -n "$value" ] && echo "$value"
}

vm-k8s-kubeadm-error() {
    # Usage: vm-k8s-kubeadm-error VMDIR KEY
    #
    # Fail the test: the VM of VMDIR has no KEY in $K8S_KUBEADM_CONFIG.
    error "vm-k8s-reinit: $(vmdir-name "$1") has no $2 in $K8S_KUBEADM_CONFIG.
Provisioning writes that file: provision the VM again."
}

vm-k8s-reset-node() {
    # Usage: vm-k8s-reset-node VMDIR
    #
    # Remove the VM of VMDIR from its cluster and remove the cluster state
    # that kubeadm reset leaves: the kubelet state (kubeadm 1.37 only
    # unmounts in /var/lib/kubelet), iptables rules, the CNI bridge and IP
    # leases, kubeconfigs. A suite stops first what it runs outside pods
    # against the cluster.
    local vmdir="$1" sock subnet
    sock=$(vm-k8s-kubeadm-value "$vmdir" criSocket) || vm-k8s-kubeadm-error "$vmdir" criSocket
    subnet=$(vm-k8s-kubeadm-value "$vmdir" podSubnet) || vm-k8s-kubeadm-error "$vmdir" podSubnet
    vmdir-command "$vmdir" "kubeadm reset -f --cleanup-tmp-dir --cri-socket $sock 2>&1 | grep -v '^W[0-9]' | tail -n 5
rc=\${PIPESTATUS[0]}
systemctl stop kubelet
for m in \$(findmnt -rn -o TARGET | grep '^/var/lib/kubelet/' | sort -r); do umount -l \"\$m\"; done
find /var/lib/kubelet -mindepth 1 -maxdepth 1 -exec rm -rf {} +
for t in filter nat mangle raw; do iptables -t \$t -F; iptables -t \$t -X; ip6tables -t \$t -F 2>/dev/null; ip6tables -t \$t -X 2>/dev/null; done
ip link del cni0 2>/dev/null
rm -rf /var/lib/cni/networks/*
# no pod network until the subnet of the node in the new cluster is known
[ ! -f $K8S_CNI_FILE ] || mv -f $K8S_CNI_FILE $K8S_CNI_FILE.off
rm -f /root/.kube/config \"\$(getent passwd \"\$SUDO_USER\" | cut -d: -f6)/.kube/config\"
nmcli -t -f NAME con show | grep -qx $K8S_NM_PROFILE && nmcli con modify $K8S_NM_PROFILE ipv4.routes '' && nmcli con up $K8S_NM_PROFILE >/dev/null
ip route | grep '^${subnet%.*.*}\\.' | grep -v 'dev cni0' | while read -r r; do ip route del \$r; done
exit \$rc" || command-error "vm-k8s-reinit: kubeadm reset failed in $(vmdir-name "$vmdir")"
}

vm-k8s-kubelet-node-ip() {
    # Usage: vm-k8s-kubelet-node-ip VMDIR IP
    #
    # Make kubelet in the VM of VMDIR register with node address IP, or with
    # its default address if IP is empty.
    vmdir-command "$1" "printf 'KUBELET_EXTRA_ARGS=%s\n' '${2:+--node-ip=$2}' > /etc/sysconfig/kubelet && cat /etc/sysconfig/kubelet" ||
        command-error "vm-k8s-reinit: cannot set the node address of kubelet in $(vmdir-name "$1")"
}

vm-k8s-cni-write() {
    # Usage: vm-k8s-cni-write VMDIR SUBNET
    #
    # Write the bridge CNI configuration of the VM of VMDIR with SUBNET.
    local vmdir="$1" subnet="$2" tmp
    tmp=$(mktemp) || error "mktemp failed"
    sed "s|CNI_NETWORK|$subnet|" "$K8S_CNI_TEMPLATE" > "$tmp" || error "cannot instantiate $K8S_CNI_TEMPLATE"
    vmdir-put-file "$vmdir" "$tmp" "$K8S_CNI_FILE"
    rm -f "$tmp"
    vmdir-command "$vmdir" "chmod 644 $K8S_CNI_FILE && rm -f $K8S_CNI_FILE.off && grep subnet $K8S_CNI_FILE" ||
        command-error "vm-k8s-reinit: cannot write $K8S_CNI_FILE in $(vmdir-name "$vmdir")"
}

vm-k8s-node-cidr() {
    # Usage: vm-k8s-node-cidr CPDIR NODE
    #
    # Wait until the controller manager gave NODE a pod CIDR and print it.
    local cp="$1" node="$2" cidr=""
    # stdout is the result: the messages of retry-until go to stderr
    retry-until --timeout 60 --interval 2 --message "pod CIDR of node $node" \
        'cidr=$(vmdir-command-q "$cp" "kubectl get node $node -o jsonpath={.spec.podCIDR}") && [ -n "$cidr" ]' >&2 ||
        error "vm-k8s-reinit: node $node got no pod CIDR"
    echo "$cidr"
}

vm-k8s-kubeconfig() {
    # Usage: vm-k8s-kubeconfig VMDIR FILE
    #
    # Install the admin kubeconfig FILE of the VM of VMDIR as the
    # kubeconfig of root and of the login user.
    vmdir-command "$1" "set -e
home=\$(getent passwd \"\$SUDO_USER\" | cut -d: -f6)
[ -n \"\$home\" ]
mkdir -p /root/.kube \"\$home/.kube\"
cp $2 /root/.kube/config
cp $2 \"\$home/.kube/config\"
chown -R \"\$SUDO_USER\": \"\$home/.kube\"
chmod 600 /root/.kube/config \"\$home/.kube/config\"" ||
        command-error "vm-k8s-reinit: cannot install the kubeconfig in $(vmdir-name "$1")"
}

vm-k8s-init-config() {
    # Usage: vm-k8s-init-config CPDIR [IP]
    #
    # Write $K8S_REINIT_CONFIG in the VM of CPDIR. It is $K8S_KUBEADM_CONFIG
    # of the VM with a new InitConfiguration. The node in it has the name of
    # the VM. The CRI socket stays. The API server address is IP if given.
    local cp="$1" ip="$2" name config sock tmp
    name="$(vmdir-name "$cp")"
    sock=$(vm-k8s-kubeadm-value "$cp" criSocket) || vm-k8s-kubeadm-error "$cp" criSocket
    config=$(vmdir-command-q "$cp" "cat $K8S_KUBEADM_CONFIG") ||
        error "vm-k8s-reinit: cannot read $K8S_KUBEADM_CONFIG in $name"
    tmp=$(mktemp) || error "mktemp failed"
    {
        printf 'apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n'
        printf 'nodeRegistration:\n  name: %s\n  criSocket: "%s"\n' "$name" "$sock"
        if [ -n "$ip" ]; then
            printf 'localAPIEndpoint:\n  advertiseAddress: %s\n' "$ip"
        fi
        # The other documents: ClusterConfiguration and KubeletConfiguration.
        awk 'function flush() {
                 if (doc !~ /(^|\n)kind: *InitConfiguration *\n/ && doc ~ /[^ \n]/) printf "---\n%s", doc
                 doc = ""
             }
             /^---/ { flush(); next }
             { doc = doc $0 "\n" }
             END { flush() }' <<< "$config"
    } > "$tmp"
    vmdir-put-file "$cp" "$tmp" "$K8S_REINIT_CONFIG"
    rm -f "$tmp"
    vmdir-command "$cp" "cat $K8S_REINIT_CONFIG" ||
        command-error "vm-k8s-reinit: cannot read $K8S_REINIT_CONFIG in $name"
}

vm-k8s-init-cp() {
    # Usage: vm-k8s-init-cp CPDIR [IP]
    #
    # kubeadm init in the VM of CPDIR from its kubeadm configuration, with
    # node address and API server address IP if given (a multi-node
    # cluster). Set K8S_JOIN_COMMAND.
    local cp="$1" ip="$2" name subnet
    name="$(vmdir-name "$cp")"
    vm-k8s-init-config "$cp" "$ip"
    vm-k8s-kubelet-node-ip "$cp" "$ip"
    if [ -z "$ip" ]; then
        subnet=$(vm-k8s-kubeadm-value "$cp" podSubnet) || vm-k8s-kubeadm-error "$cp" podSubnet
        vm-k8s-cni-write "$cp" "$subnet"
    fi
    vmdir-command "$cp" "kubeadm init --config $K8S_REINIT_CONFIG 2>&1 | grep -v '^W[0-9]' | tail -n 25; exit \${PIPESTATUS[0]}" || {
        vmdir-command "$cp" "journalctl -u kubelet --no-pager -n 50"
        command-error "vm-k8s-reinit: kubeadm init failed in $name"
    }
    vm-k8s-kubeconfig "$cp" /etc/kubernetes/admin.conf
    vmdir-command "$cp" "kubectl taint nodes --all node-role.kubernetes.io/control-plane- 2>&1 || true"
    if [ -n "$ip" ]; then
        # The node gets a /24 of the pod network: the bridge CNI of this
        # node must hand out addresses of that subnet only.
        subnet=$(vm-k8s-node-cidr "$cp" "$name")
        vm-k8s-cni-write "$cp" "$subnet"
    fi
    vmdir-command "$cp" "kubeadm token create --print-join-command" ||
        command-error "vm-k8s-reinit: cannot create a join token in $name"
    K8S_JOIN_COMMAND="$(tail -n 1 <<< "$COMMAND_OUTPUT")"
    echo "$K8S_JOIN_COMMAND" > "$cp/join-command"
}

vm-k8s-join-worker() {
    # Usage: vm-k8s-join-worker VMDIR CPDIR JOIN-COMMAND IP
    #
    # kubeadm join the VM of VMDIR to the cluster of CPDIR as a worker
    # with node address IP, give it the admin kubeconfig of the cluster and
    # the bridge CNI subnet of its pod CIDR.
    local vmdir="$1" cp="$2" join="$3" ip="$4" name tmp subnet sock
    name="$(vmdir-name "$vmdir")"
    sock=$(vm-k8s-kubeadm-value "$vmdir" criSocket) || vm-k8s-kubeadm-error "$vmdir" criSocket
    vm-k8s-kubelet-node-ip "$vmdir" "$ip"
    vmdir-command "$vmdir" "$join --node-name=$name --cri-socket $sock 2>&1 | grep -v '^W[0-9]' | tail -n 15; exit \${PIPESTATUS[0]}" || {
        vmdir-command "$vmdir" "journalctl -u kubelet --no-pager -n 50"
        command-error "vm-k8s-reinit: kubeadm join failed in $name"
    }
    tmp=$(mktemp) || error "mktemp failed"
    vmdir-command-q "$cp" "cat /etc/kubernetes/admin.conf" > "$tmp" ||
        error "vm-k8s-reinit: cannot read the admin kubeconfig of $(vmdir-name "$cp")"
    vmdir-put-file "$vmdir" "$tmp" /root/admin.conf
    rm -f "$tmp"
    vm-k8s-kubeconfig "$vmdir" /root/admin.conf
    subnet=$(vm-k8s-node-cidr "$cp" "$name")
    vm-k8s-cni-write "$vmdir" "$subnet"
}

vm-k8s-routes() {
    # Usage: vm-k8s-routes VMDIR...
    #
    # Route the pod CIDR of every node of a multi-node cluster (the VMs of
    # VMDIR..., control plane first) through its VM network address on
    # every other node, now and in the NetworkManager profile of the VM
    # network, so that the routes survive a restart.
    local cp="$1" vmdir other cidr ip routes
    for vmdir in "$@"; do
        routes=""
        for other in "$@"; do
            [ "$other" != "$vmdir" ] || continue
            cidr=$(vm-k8s-node-cidr "$cp" "$(vmdir-name "$other")")
            ip=$(vm-k8s-node-ip "$other")
            routes+="ip route replace $cidr via $ip
nmcli con modify $K8S_NM_PROFILE +ipv4.routes '$cidr $ip'
"
        done
        vmdir-command "$vmdir" "set -e
nmcli con modify $K8S_NM_PROFILE ipv4.routes ''
$routes
ip route | grep ' via $VMNET_CIDR_PREFIX\.'" ||
            command-error "vm-k8s-reinit: cannot route the pod networks of the other nodes in $(vmdir-name "$vmdir")"
    done
}

vm-k8s-wait-cluster() {
    # Usage: vm-k8s-wait-cluster CPDIR TIMEOUT VMDIR...
    #
    # Wait until the nodes of the VMs of VMDIR... are Ready, kube-proxy runs
    # on all of them and the cluster DNS is available, and save the state
    # of the cluster in $TEST_OUTPUT_DIR/k8s-cluster.<label of CPDIR>.txt.
    local cp="$1" timeout="$2" vmdir
    shift 2
    for vmdir in "$@"; do
        vmdir-command "$cp" "kubectl wait --for=condition=Ready node/$(vmdir-name "$vmdir") --timeout=${timeout}s" || {
            vmdir-command "$vmdir" "journalctl -u kubelet --no-pager -n 50"
            command-error "vm-k8s-reinit: node $(vmdir-name "$vmdir") is not Ready"
        }
    done
    vmdir-command "$cp" "kubectl -n kube-system rollout status ds/kube-proxy --timeout=${timeout}s && kubectl -n kube-system wait deploy/coredns --for=condition=Available --timeout=${timeout}s" || {
        vmdir-command "$cp" "kubectl get pods -A -o wide"
        command-error "vm-k8s-reinit: kube-proxy or the cluster DNS of $(vmdir-name "$cp") did not come up"
    }
    vmdir-command "$cp" "kubectl get nodes -o wide; kubectl get pods -A -o wide"
    if [ -n "$TEST_OUTPUT_DIR" ]; then
        echo "$COMMAND_OUTPUT" > "$TEST_OUTPUT_DIR/k8s-cluster.$(vmdir-label "$cp").txt"
    fi
}
