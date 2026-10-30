source "$(dirname "${BASH_SOURCE[0]}")/command.bash"
source "$(dirname "${BASH_SOURCE[0]}")/e2eish.bash"

export VM_CONTEXT=""
export VM_PROMPT=${VM_PROMPT-"\e[38;5;11mroot@vm>\e[0m "}
export VM_SAVED_PROMPT=""
CACHE_DIR="${CACHE_DIR:-$HOME/.cache/nri-plugins/e2e}"
CACHE_DECAY="${CACHE_DECAY:-$((3 * 24 * 3600))}" # global cached variables valid for 3 days

# Cache of "vagrant package"d images of fully provisioned VMs. Reusing one
# skips installing Kubernetes, the container runtime and the CNI plugin.
#
# Expect a box to be a couple of gigabytes, and note that vagrant unpacks it
# into ~/.vagrant.d/boxes on first use, so the disk cost of a box is roughly
# twice its file size.
#
# The boxes contain a running single-node cluster whose certificates the
# kubeadm defaults give a year to live, so they must not be kept for too long.
BOX_CACHE_DIR="${BOX_CACHE_DIR:-$CACHE_DIR/boxes}"
BOX_CACHE_DECAY="${BOX_CACHE_DECAY:-$((30 * 24 * 3600))}" # cached boxes valid for 30 days

# Name prefix of the vagrant boxes of packaged VMs. It has to differ from the
# name of the distro box, or adding a packaged box would overwrite the distro
# image which was downloaded for it.
BOX_NAME_PREFIX="${BOX_NAME_PREFIX:-nri-e2e}"

# Name of the file which records, in an output directory, the box key of the VM
# in it, that is, the versions it was provisioned with.
#
# vagrant provisions a machine once. An output directory which is kept boots
# the VM it already has, so without this the versions a run resolves say
# nothing about the versions it tests.
VM_KEY_FILE="${VM_KEY_FILE:-.e2e-vm-key}"

# Files whose content shapes a provisioned VM. The name of a cached box has a
# hash of them, so that editing any of them invalidates the cached boxes
# instead of a later run reusing an image which predates the change.
#
# The paths are relative to test/e2e. Only what provisioning itself uses
# belongs here: the playbooks which deploy a plugin and the ones which install
# a custom kernel run per test run, after a box has been packaged, so they do
# not affect what is in it.
#
# Add a file here when provisioning starts using one.
BOX_RECIPE_FILES=(
    playbook/provision.yaml
    files/Vagrantfile.in
    files/env.in
    files/containerd-nri-enable
    files/crio-nri-enable
    files/10-bridge.conf.in
)

# How long to wait for the cluster in a VM which has just booted to become
# usable. Booting a large topology and starting the control plane, the CNI
# plugin and the cluster DNS all happen within this.
CLUSTER_READY_TIMEOUT="${CLUSTER_READY_TIMEOUT:-300}"

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

vm-set-context() {
    VM_CONTEXT="$1"
    # we can't stack contexts, only original one gets saved/restored
    if [ -z "$VM_SAVED_PROMPT" ]; then
        VM_SAVED_PROMPT="$VM_PROMPT"
    fi
    VM_PROMPT="\e[38;5;11mroot@vm${VM_CONTEXT+ }${VM_CONTEXT:-}>\e[0m "
}

vm-reset-context() {
    VM_PROMPT="$VM_SAVED_PROMPT"
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

vm-save-cached-var() {
    local output_dir="$1"
    local var="$2"
    local val="${3:-}"
    local cache_dir="$output_dir/cache"

    if [ "$cache" = "global" ]; then
        cache_dir="$CACHE_DIR"
    fi

    if [ $# = 3 ]; then
        val="$3"
    else
        val="${!var}"
    fi

    if [ -z "$val" ]; then
        echo "WARNING: not saving cached empty value for variable $var..." 1>&2
        return 0
    fi

    if [ ! -d "$cache_dir" ]; then
        mkdir -p "$cache_dir" || \
            error "failed to create cache dir $cache_dir"
    fi

    echo "$val" > "$cache_dir/$var"
    if [ $? = 0 ]; then
        echo "saved cached variable $var=$val..." 1>&2
        return 0
    fi

    return 1
}

vm-load-cached-var() {
    local output_dir="$1"
    local var="$2"
    local cache_dir="$output_dir/cache"
    local val

    if [ ! -f "$cache_dir/$var" ]; then
        if [ "$cache" != "local" -a -f "$CACHE_DIR/$var" ]; then
            if [ $(( $(stat -c %Y "$CACHE_DIR/$var") + $CACHE_DECAY )) -gt $(date +%s) ]; then
                cache_dir="$CACHE_DIR"
            else
                return 1
            fi
        else
            return 1
        fi
    fi

    val="$(cat $cache_dir/$var)"
    if [ $? = 0 ]; then
        echo "loaded cached variable $var=$val..." 1>&2
        echo $val
        return 0
    fi

    error "failed to load cached variable $var" 1>&2
    return 1
}

vm-box-cache-supported() {
    # Usage: vm-box-cache-supported
    #
    # Return success if the installed vagrant-qemu implements "vagrant
    # package". Older versions declare the action but do not ship the
    # middlewares it uses, so calling it fails.
    local version dir

    version=$(vagrant plugin list 2>/dev/null |
                  sed -n 's/^vagrant-qemu (\([^,)]*\).*/\1/p' | head -n 1)
    if [ -z "$version" ]; then
        return 1
    fi
    for dir in "$HOME"/.vagrant.d/gems/*/gems/"vagrant-qemu-$version"; do
        if [ -f "$dir/lib/vagrant-qemu/action/export.rb" ]; then
            return 0
        fi
    done
    return 1
}

vm-box-cache-enabled() {
    # Usage: vm-box-cache-enabled
    #
    # Return success if the box cache is in use. Set e2e_vm_cache to
    #   yes:     use a cached box if there is one, create one if there is not
    #   refresh: ignore any cached box, provision from scratch, then replace it
    #   cleanup: remove all cached boxes and exit, see vm-cleanup-boxes
    #            (nuke and drop are synonyms)
    #   no:      do not use or create cached boxes (the default)
    case "${e2e_vm_cache:-no}" in
        yes|1|refresh)
            ;;
        *)
            return 1
            ;;
    esac

    if ! vm-box-cache-supported; then
        echo "WARNING: e2e_vm_cache=$e2e_vm_cache, but the installed" \
             "vagrant-qemu does not support \"vagrant package\"." \
             "Provisioning the VM from scratch..." >&2
        return 1
    fi
    if [ -z "$k8s_release" ] || [ -z "$distro" ]; then
        echo "WARNING: e2e_vm_cache=$e2e_vm_cache, but the versions to" \
             "install are not resolved yet. Provisioning the VM from scratch..." >&2
        return 1
    fi

    # Check the recipe here, where failing aborts the run. The hash of it ends
    # up in the name of a box through command substitutions, which would
    # swallow the failure.
    vm-check-provisioning-recipe

    return 0
}

vm-check-provisioning-recipe() {
    # Usage: vm-check-provisioning-recipe
    #
    # Fail unless every file in BOX_RECIPE_FILES can be read.
    #
    # Hashing an incomplete recipe would name boxes which do not correspond to
    # it, so a file which has been renamed, moved or removed without updating
    # BOX_RECIPE_FILES has to be reported rather than quietly skipped.
    #
    # Call this outside a command substitution. error only exits the subshell
    # of one, which is why vm-provisioning-recipe-hash cannot be the only place
    # which checks.
    local e2e_dir="$nri_resource_policy_src/test/e2e"
    local file unreadable=""

    for file in "${BOX_RECIPE_FILES[@]}"; do
        if [ ! -f "$e2e_dir/$file" ] || [ ! -r "$e2e_dir/$file" ]; then
            unreadable="$unreadable $file"
        fi
    done
    if [ -n "$unreadable" ]; then
        error "cannot read provisioning recipe file(s):$unreadable"
    fi
}

vm-provisioning-recipe-hash() {
    # Usage: vm-provisioning-recipe-hash
    #
    # Print a hash of the files in BOX_RECIPE_FILES, that is, of everything
    # which shapes a provisioned VM.
    local e2e_dir="$nri_resource_policy_src/test/e2e"

    vm-check-provisioning-recipe

    ( cd "$e2e_dir" && cat "${BOX_RECIPE_FILES[@]}" ) | sha256sum | cut -c1-12
}

vm-box-key() {
    # Usage: vm-box-key VMNAME
    #
    # Print the cache key of the box of a fully provisioned VM.
    #
    # VMNAME already covers the topology, the distro and the container runtime,
    # and the topology has to be part of the key: the hostname of the VM is
    # derived from it, and kubeadm bakes the hostname into the name of the node,
    # into the certificates and into etcd.
    local vmname="$1" cri_release gates key

    case "$k8scri" in
        crio) cri_release="$crio_release";;
        *)    cri_release="$containerd_release";;
    esac

    # Sorted, so that the same gates given in another order share a box.
    gates="$(tr ',' '\n' <<< "${k8s_feature_gates//[[:space:]]/}" | sed '/^$/d' | sort | paste -sd, -)"

    key="$vmname-k8s$k8s_release-$k8scri$cri_release"
    key="$key-cni$cni_plugin$cni_release-helm$helm_release"
    if [ -n "$gates" ]; then
        key="$key-gates$gates"
    fi
    key="$key-$(vm-provisioning-recipe-hash)"

    echo "${key//[^A-Za-z0-9._-]/-}"
}

vm-box-name() {
    # Usage: vm-box-name VMNAME
    #
    # Print the name of the vagrant box of the packaged VM of VMNAME.
    echo "$BOX_NAME_PREFIX/$(vm-box-key "$1")"
}

# Testing on a host which already exists, real hardware or a VM somebody else
# made, instead of creating one with vagrant. Set
#   test_hw_addr=user@address   the host to log in to
#   test_hw_key=path            the private key to log in with
# The host has to let that user in by key and run sudo without a password.

vm-hw-mode() {
    # Usage: vm-hw-mode
    #
    # Return success if this run targets a host we did not create.
    #
    # Silent and status-only. Every branch which has to tell the two kinds of
    # host apart asks this, so it must neither print nor exit.
    [ -n "${test_hw_addr:-}" ]
}

vm-hw-valid-target() {
    # Usage: vm-hw-valid-target ADDR
    #
    # Return success if ADDR names both a user and a host. Without a user ssh
    # would quietly log in as whoever started the run, which is not a default
    # worth having.
    local addr="$1"

    case "$addr" in
        ?*@?*) return 0;;
    esac
    echo "test_hw_addr=\"$addr\" names no user: it has to read user@host," \
         "since that user is what the run logs in as and runs sudo as" >&2
    return 1
}

vm-hw-valid-key() {
    # Usage: vm-hw-valid-key PATH
    #
    # Return success if PATH is a private key ssh will agree to use. ssh
    # refuses one which group or others can read, so refuse it here, where
    # the message can name the file instead of coming out of ssh.
    local key="$1" mode

    if [ -z "$key" ]; then
        echo "test_hw_key is not set: a run against a host which is not a VM" \
             "logs in with a key" >&2
        return 1
    fi
    if [ -d "$key" ]; then
        echo "test_hw_key=\"$key\" is a directory, not a private key" >&2
        return 1
    fi
    if [ ! -f "$key" ] || [ ! -r "$key" ]; then
        echo "test_hw_key=\"$key\" is not a readable file" >&2
        return 1
    fi
    mode=$(stat -c %a "$key")
    if [ "${mode: -2}" != "00" ]; then
        echo "test_hw_key=\"$key\" is mode $mode: ssh refuses a key which" \
             "group or others can read, so chmod 600 it" >&2
        return 1
    fi
    return 0
}

vm-hw-valid-hostname() {
    # Usage: vm-hw-valid-hostname NAME
    #
    # Return success if NAME can serve as a Kubernetes node name.
    #
    # kubeadm names the node after the host, and the framework looks the node
    # up by that name, so a name which will not survive the lookup has to be
    # refused here rather than turn into test failures nobody can read back.
    local name="$1"

    if [ -z "$name" ] || [ "$name" = "localhost" ]; then
        echo "the host calls itself \"$name\", which cannot identify a node:" \
             "give it a name of its own" >&2
        return 1
    fi
    if [[ ! "$name" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]]; then
        echo "the host calls itself \"$name\": a node name takes lower case" \
             "letters, digits and dashes, and starts and ends with a letter" \
             "or a digit" >&2
        return 1
    fi
    return 0
}

vm-hw-check-options() {
    # Usage: vm-hw-check-options
    #
    # Fail the run unless the hardware options are usable. The validators
    # print what is wrong; this turns it into an exit.
    #
    # Call this outside a command substitution: error only exits the subshell
    # of one, which is why it cannot live in the validators themselves.
    vm-hw-valid-target "${test_hw_addr:-}" ||
        error "test_hw_addr is unusable, see above"
    vm-hw-valid-key "${test_hw_key:-}" ||
        error "test_hw_key is unusable, see above"
}

vm-hw-user() {
    # Usage: vm-hw-user
    local addr="${test_hw_addr:-}"
    printf '%s\n' "${addr%%@*}"
}

vm-hw-address() {
    # Usage: vm-hw-address
    local addr="${test_hw_addr:-}"
    printf '%s\n' "${addr#*@}"
}

vm-hw-key() {
    # Usage: vm-hw-key
    #
    # Print the private key to log in with, as an absolute path.
    #
    # Absolute because this path is written into the generated ssh config and
    # handed to ansible, and both are used from the output directory rather
    # than from wherever the run was started. A relative path authenticates
    # for the probes here and then stops working the moment something changes
    # directory, which reads as the key being rejected.
    local key="${test_hw_key:-}"

    if [ -z "$key" ]; then
        return 0
    fi

    realpath -m "$key" 2>/dev/null || printf '%s\n' "$key"
}

vm-hw-ssh() {
    # Usage: vm-hw-ssh COMMAND...
    #
    # Run COMMAND on the host over a connection of our own.
    #
    # Everything the tests do goes through $SSH and the generated ssh config.
    # These few probes run before that config exists, so they spell the same
    # options out instead of using -F.
    ssh -i "$(vm-hw-key)" \
        -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o IdentitiesOnly=yes \
        -o PasswordAuthentication=no \
        -o LogLevel=FATAL \
        -o ConnectTimeout=10 \
        "$(vm-hw-user)@$(vm-hw-address)" "$@"
}

vm-hw-hostname() {
    # Usage: vm-hw-hostname
    #
    # Print the short hostname of the host, read from it. Fail, and say what
    # ssh said, when the host cannot be reached.
    #
    # Failing to log in is the likeliest mistake of a first run, so it has to
    # be told apart from a host which genuinely has no usable name. Reporting
    # both as an empty name sends the reader after the wrong thing.
    local out status

    out=$(vm-hw-ssh hostname -s 2>&1)
    status=$?
    if [ "$status" != 0 ]; then
        echo "cannot reach $(vm-hw-user)@$(vm-hw-address) with the key" \
             "$(vm-hw-key):" >&2
        printf '%s\n' "$out" >&2
        return 1
    fi

    # The last line only: a login shell which echoes from an rc file would
    # otherwise become a hostname nobody can account for.
    printf '%s\n' "$(tail -n 1 <<< "$out")"
}

vm-hw-check-sudo() {
    # Usage: vm-hw-check-sudo
    #
    # Return success if the login user can run sudo without being asked for a
    # password, which is what provisioning and every vm-command need.
    vm-hw-ssh sudo -n true > /dev/null 2>&1
}

vm-hw-has-cluster() {
    # Usage: vm-hw-has-cluster
    #
    # Return success if the host already runs a Kubernetes cluster.
    #
    # Asked of the host rather than of the output directory: a directory can
    # be removed and a cluster cannot, so the host is the only honest answer.
    vm-hw-ssh test -f /etc/kubernetes/admin.conf > /dev/null 2>&1
}

vm-hw-check-provisionable() {
    # Usage: vm-hw-check-provisionable VAGRANTDIR
    #
    # Fail the run rather than provision over a cluster nobody asked to lose.
    #
    # A second kubeadm init refuses to run, and the way through that is
    # provision=all, which takes the cluster down first. Doing that on our own
    # would destroy a cluster on a machine the user may share, so it is said
    # rather than done.
    local vagrantdir="$1"

    vm-hw-check-sudo ||
        error "$(vm-hw-user)@$(vm-hw-address) cannot run sudo without a
password, which provisioning and the tests both need. Check the host's
sudoers, and that test_hw_key is the key that user accepts."

    if [ "${provision:-}" = "all" ]; then
        return 0
    fi

    if vm-hw-has-cluster && [ -z "$(vm-recorded-box-key "$vagrantdir")" ]; then
        error "$(vm-hw-address) already runs a Kubernetes cluster and this run
has no record of what went into it. Set provision=all to take that cluster
down and build it again, or point the run at a host without one."
    fi

    return 0
}

vm-provision-hw() {
    # Usage: vm-provision-hw VAGRANTDIR
    #
    # Run the provisioning playbook against the host.
    #
    # The same playbook and the same variables the vagrant provisioner uses,
    # from the same file, which is the point of writing that file.
    local vagrantdir="$1"
    local playbook="$nri_resource_policy_src/test/e2e/playbook/provision.yaml"
    local login_args=()

    mapfile -t login_args < <(vm-ansible-login-args "$VM_HOSTNAME" "$vagrantdir")

    echo "provisioning $VM_HOSTNAME..."
    (cd "$vagrantdir";
     export ANSIBLE_PIPELINING=True;
     ANSIBLE_SSH_ARGS="-F $vagrantdir/.ssh-config" ansible-playbook "$playbook" \
          -i "$VM_HOSTNAME," "${login_args[@]}" \
          --ssh-common-args "-F $vagrantdir/.ssh-config" \
          --extra-vars "@$vagrantdir/ansible-vars.json"
    ) || error "failed to provision $VM_HOSTNAME"
}

vm-hw-resolve-names() {
    # Usage: vm-hw-resolve-names
    #
    # Set vm_name and VM_HOSTNAME for a run against a host we did not create.
    #
    # The name of the VM doubles as the ssh alias and, through
    # get-hostname-for-vm, as the Kubernetes node name. On a host of its own
    # those come apart: the node is named after the host, while the output
    # directory has to tell a containerd run from a crio one against the same
    # box. So VM_HOSTNAME is the host and vm_name carries the runtime too.
    #
    # Reading the name costs an ssh round trip, and both run_tests.sh and
    # run.sh default vm_name independently, so an already exported VM_HOSTNAME
    # is taken as the answer rather than asked for again.
    local hostname="${VM_HOSTNAME:-}"

    if [ -z "$hostname" ]; then
        vm-hw-check-options
        hostname="$(vm-hw-hostname)" ||
            error "could not read the name of the host, see above"
        vm-hw-valid-hostname "$hostname" ||
            error "the host does not have a usable name, see above"
    fi

    export VM_HOSTNAME="$hostname"
    export vm_name="${vm_name:-$hostname-${k8scri:-containerd}}"
}

vm-write-hw-ssh-config() {
    # Usage: vm-write-hw-ssh-config VAGRANTDIR HOSTNAME
    #
    # Write the ssh config of a host we did not create.
    #
    # Same shape vagrant ssh-config produces, alias included, so that
    # vm-command, vm-play and the host helpers need to know nothing about
    # which kind of host they are talking to.
    #
    # Host key checking is off and known hosts discarded, as on the vagrant
    # path: a test framework has no business writing to anyone's known_hosts,
    # and should not start failing because a test box was reinstalled.
    local vagrantdir="$1" hostname="$2" aliases="node $2"

    # vagrant answers to "node" and to the name of the VM, and the framework
    # uses both: vm-command goes through VM_HOSTNAME, which is the host, while
    # vm-play hands ansible vm_name as the inventory host. So answer to all of
    # them.
    if [ -n "${vm_name:-}" ] && [ "${vm_name:-}" != "$hostname" ]; then
        aliases="$aliases $vm_name"
    fi

    cat > "$vagrantdir/.ssh-config" <<EOF
Host $aliases
  HostName $(vm-hw-address)
  User $(vm-hw-user)
  UserKnownHostsFile /dev/null
  StrictHostKeyChecking no
  PasswordAuthentication no
  IdentityFile $(vm-hw-key)
  IdentitiesOnly yes
  LogLevel FATAL

  ControlMaster auto
  ControlPersist 60
  ControlPath /tmp/ssh-%C
EOF
}

vm-write-env-file() {
    # Usage: vm-write-env-file VAGRANTDIR [NO_PROVISION]
    #
    # Write the dotenv file of a run, unless it is already there, and keep its
    # provisioning flag in step.
    #
    # The Vagrantfile loads this file, and vm-write-ansible-vars reads the
    # proxy settings back out of it, so a run against a host which is not a VM
    # needs it too, even though the ssh port and the disk size in it mean
    # nothing there.
    local vagrantdir="$1" no_provision="${2:-}"
    local files="$nri_resource_policy_src/test/e2e/files"
    local SSH_PORT ESCAPED_PROXY

    if [ ! -f "$vagrantdir/env" ]; then
	# Get a random port between 50023 - 52071 to be used to access the VM
	SSH_PORT="$[ $RANDOM % 2048 + 50023 ]"

	if [ ! -z "${proxy:-}" ]; then
	    ESCAPED_PROXY=$(printf '%s\n' "$proxy" | sed -e 's/[\/&]/\\&/g')

	    sed -e "s/\#PROXY=\"\"/PROXY=\"$ESCAPED_PROXY\"/g" \
		-e "s/\#HTTP/HTTP/g" \
		-e "s/DNS_NAMESERVER=\"\"/DNS_NAMESERVER=\"${dns_nameserver:-}\"/g" \
		-e "s/DNS_SEARCH_DOMAIN=\"\"/DNS_SEARCH_DOMAIN=\"${dns_search_domain:-}\"/g" \
		-e "s/SSH_PORT=/SSH_PORT=$SSH_PORT/g" \
                -e "s:CACHE_DIR=:CACHE_DIR=\"${CACHE_DIR:-}\":g" \
                -e "s:E2E_NO_PROVISION=:E2E_NO_PROVISION=$no_provision:g" \
		"$files/env.in" > "$vagrantdir/env"
	else
	    sed -e "s/DNS_NAMESERVER=\"\"/DNS_NAMESERVER=\"${dns_nameserver:-}\"/g" \
		-e "s/DNS_SEARCH_DOMAIN=\"\"/DNS_SEARCH_DOMAIN=\"${dns_search_domain:-}\"/g" \
		-e "s/SSH_PORT=/SSH_PORT=$SSH_PORT/g" \
                -e "s:CACHE_DIR=:CACHE_DIR=\"${CACHE_DIR:-}\":g" \
                -e "s:E2E_NO_PROVISION=:E2E_NO_PROVISION=$no_provision:g" \
		"$files/env.in" > "$vagrantdir/env"
	fi
    fi

    # An env file written before this VM was known to come from a box has no
    # flag, or a stale one. Keep it in sync, so that a vagrant up which is not
    # ours, from make up or make ssh, does not provision the VM either.
    if [ -n "$no_provision" ] && [ -f "$vagrantdir/env" ] &&
           ! grep -q '^E2E_NO_PROVISION=1$' "$vagrantdir/env"; then
        sed -i 's/^E2E_NO_PROVISION=.*$/E2E_NO_PROVISION=1/' "$vagrantdir/env"
        grep -q '^E2E_NO_PROVISION=1$' "$vagrantdir/env" ||
            echo "E2E_NO_PROVISION=1" >> "$vagrantdir/env"
    fi
}

vm-hw-check-cache-conflict() {
    # Usage: vm-hw-check-cache-conflict
    #
    # Fail the run if it asks for the box cache against a host we did not
    # create. There is nothing to package and nothing a cached box could be
    # used for, so the two options cannot both be meant.
    case "${e2e_vm_cache:-no}" in
        yes|1|refresh)
            error "e2e_vm_cache=$e2e_vm_cache and test_hw_addr do not go
together: a host we were given is never created from a box and cannot be
packaged into one. Unset one of them."
            ;;
    esac
    return 0
}

vm-setup-hw() {
    # Usage: vm-setup-hw OUTPUT_DIR VMNAME KEY
    #
    # Prepare a host we were given, instead of creating a VM.
    #
    # Almost nothing vagrant does applies here: no image, no topology, no
    # Vagrantfile, no Makefile and nothing to package. What is left is the ssh
    # config the rest of the framework talks through, the playbook, and the
    # record of what went into the host.
    local output_dir="$1" vmname="$2" vm_key="$3"
    local vagrantdir="$output_dir" first=""

    mkdir -p "$vagrantdir"
    vm-write-hw-ssh-config "$vagrantdir" "$VM_HOSTNAME"
    vm-write-env-file "$vagrantdir"
    vm-write-ansible-vars "$vagrantdir"

    vm-first-run "$vagrantdir" && first=1

    # provision at all: a first run, or one which was asked to.
    if [ -n "$first" ] || [ -n "${provision:-}" ]; then
        vm-hw-check-provisionable "$vagrantdir"
        vm-provision-hw "$vagrantdir"
    fi

    # Record what went in only when it really did. A plain provision= leaves
    # the cluster alone, so it cannot claim the versions above it changed.
    if [ -n "$first" ] || [ "${provision:-}" = "all" ]; then
        vm-record-box-key "$vagrantdir" "$vm_key"
    fi

    mkdir -p "$COMMAND_OUTPUT_DIR"
    rm -f "$COMMAND_OUTPUT_DIR"/0*

    # The cluster was either built a moment ago or days ago on a host which
    # has been rebooted since. Either way it is worth waiting for.
    wait-for-node-ready
    wait-for-dns-ready
}

vm-json-quote() {
    # Usage: vm-json-quote VALUE
    #
    # Print VALUE as a JSON string.
    local s="${1//\\/\\\\}"

    s="${s//\"/\\\"}"
    printf '"%s"' "$s"
}

vm-write-ansible-vars() {
    # Usage: vm-write-ansible-vars VAGRANTDIR
    #
    # Write the variables the provisioning playbook takes into
    # $VAGRANTDIR/ansible-vars.json.
    #
    # One file serves both ways of provisioning: the vagrant ansible
    # provisioner takes it as "@<path>" and a run against a host which is not
    # a VM hands the same path to ansible-playbook as --extra-vars. Assembling
    # these in the Vagrantfile as well would be a second copy, in another
    # language, for the two to drift apart.
    local vagrantdir="$1"
    local cri_runtime="${k8scri:-containerd}"
    local containerd_rel="" containerd_source=""
    local crio_rel="1.28.1" crio_source=""
    local http_p="" https_p="" no_p=""
    local reset=false hardware=false

    # Only the selected runtime's release is passed on, which is what the
    # Vagrantfile did. crio keeps a fallback release so that the tarball URL
    # the playbook builds from it stays well formed when it goes unused.
    if [ "$cri_runtime" = "containerd" ]; then
        containerd_rel="${containerd_release:-}"
        containerd_source="${containerd_src:-}"
    else
        crio_rel="${crio_release:-}"
        crio_source="${crio_src:-}"
    fi

    # NO_PROXY is spelled out in files/env.in and the two proxy variables are
    # written into the env file from $proxy, so the env file is where all
    # three are read from rather than being repeated here.
    if [ -f "$vagrantdir/env" ]; then
        { read -r http_p; read -r https_p; read -r no_p; } < <(
            set +u
            . "$vagrantdir/env" > /dev/null 2>&1
            printf '%s\n%s\n%s\n' "${http_proxy:-}" "${https_proxy:-}" \
                   "${no_proxy:-}"
        )
    fi

    if [ "${provision:-}" = "all" ]; then
        reset=true
    fi

    # Tells the playbook it is on a host nobody gave it permission to reshape.
    # Anything which is fine to do to a throwaway VM and rude to do to a
    # machine somebody owns is gated on this.
    if vm-hw-mode; then
        hardware=true
    fi

    cat > "$vagrantdir/ansible-vars.json" <<EOF
{
  "network": $(vm-json-quote "${pod_cidr:-10.217.0.0/16}"),
  "hostname": $(vm-json-quote "${VM_HOSTNAME:-}"),
  "dns_nameserver": $(vm-json-quote "${dns_nameserver:-}"),
  "dns_search_domain": $(vm-json-quote "${dns_search_domain:-}"),
  "https_proxy": $(vm-json-quote "$https_p"),
  "http_proxy": $(vm-json-quote "$http_p"),
  "no_proxy": $(vm-json-quote "$no_p"),
  "kernel_getsource": $(vm-json-quote "${kernel_getsource:-}"),
  "kernel_config": $(vm-json-quote "${kernel_config:-}"),
  "k8s_release": $(vm-json-quote "${k8s_release:-}"),
  "k8s_version": $(vm-json-quote "${k8s_version:-}"),
  "k8s_feature_gates": $(vm-json-quote "${k8s_feature_gates:-}"),
  "kubeadm_reset": $reset,
  "is_hardware": $hardware,
  "helm_release": $(vm-json-quote "${helm_release:-}"),
  "cri_runtime": $(vm-json-quote "$cri_runtime"),
  "containerd_release": $(vm-json-quote "$containerd_rel"),
  "containerd_src": $(vm-json-quote "$containerd_source"),
  "crio_release": $(vm-json-quote "$crio_rel"),
  "crio_src": $(vm-json-quote "$crio_source"),
  "cache_dir": $(vm-json-quote "${CACHE_DIR:-}"),
  "cni_plugin": $(vm-json-quote "${cni_plugin:-}"),
  "cni_release": $(vm-json-quote "${cni_release:-}"),
  "nri_resource_policy_src": $(vm-json-quote "${nri_resource_policy_src:-}"),
  "outdir": $(vm-json-quote "${OUTPUT_DIR:-}")
}
EOF
}

vm-first-run() {
    # Usage: vm-first-run VAGRANTDIR
    #
    # Return success if this is the run which brings the host into being:
    # creating the VM, or provisioning a host we were given for the first
    # time.
    #
    # A VM is new when its output directory has no Vagrantfile. A host of its
    # own never has one, so what tells its first run from a later one is
    # whether anything was recorded about it.
    local vagrantdir="$1"

    if vm-hw-mode; then
        [ -z "$(vm-recorded-box-key "$vagrantdir")" ]
        return
    fi

    [ ! -f "$vagrantdir/Vagrantfile" ]
}

vm-recorded-box-key() {
    # Usage: vm-recorded-box-key VAGRANTDIR
    #
    # Print the box key recorded for the VM of an output directory, or nothing
    # if there is none to read.
    #
    # vm-record-box-key writes the file when a VM is created. A VM which
    # predates the file and came from a cached box still names that box in its
    # Vagrantfile, and the name holds the key, so read it as a fallback. A VM
    # which predates the file and was provisioned from a distro image has
    # nothing to read, which is why an empty answer means "cannot tell" and
    # never "does not match".
    local vagrantdir="$1" key

    if [ -s "$vagrantdir/$VM_KEY_FILE" ]; then
        head -n 1 "$vagrantdir/$VM_KEY_FILE"
        return 0
    fi

    key="$(sed -n "s|^IMAGE_NAME = \"$BOX_NAME_PREFIX/\(.*\)\"$|\1|p" \
               "$vagrantdir/Vagrantfile" 2>/dev/null | head -n 1)"
    if [ -n "$key" ]; then
        echo "$key"
    fi
}

vm-record-box-key() {
    # Usage: vm-record-box-key VAGRANTDIR KEY
    #
    # Record KEY as the versions the VM of an output directory has just been
    # created with. An empty KEY records nothing.
    #
    # Only for a VM which this run created. Rewriting the file for a VM which
    # was already there would bless a mismatch instead of reporting it.
    local vagrantdir="$1" key="$2"

    if [ -z "$key" ]; then
        return 0
    fi

    if ! echo "$key" > "$vagrantdir/$VM_KEY_FILE"; then
        echo "WARNING: failed to record the versions of the VM in" \
             "$vagrantdir. A later run cannot tell whether they are still" \
             "the ones it resolves." >&2
    fi
}

vm-report-box-key-mismatch() {
    # Usage: vm-report-box-key-mismatch RECORDED CURRENT
    #
    # Print how the versions of an existing VM differ from the ones this run
    # resolved.
    #
    # A key is a "-" separated list whose components hold "-" themselves, so
    # the fields of two keys only line up when there are as many of them.
    # Which is the usual case, both keys starting with the same VM name and
    # carrying the same components, and the exception is a run which added or
    # dropped the feature gates. Print the keys whole when they do not line up.
    local recorded="$1" current="$2"
    local -a was now
    local i last label

    IFS=- read -r -a was <<< "$recorded"
    IFS=- read -r -a now <<< "$current"

    if [ "${#was[@]}" = "${#now[@]}" ]; then
        # The last field is the hash of the provisioning recipe, which says
        # nothing to a reader on its own.
        last=$(( ${#was[@]} - 1 ))
        for i in "${!was[@]}"; do
            if [ "${was[$i]}" = "${now[$i]}" ]; then
                continue
            fi
            label=""
            if [ "$i" = "$last" ]; then
                label="provisioning recipe "
            fi
            echo "  $label${was[$i]} -> ${now[$i]}" >&2
        done
    else
        echo "  VM:  $recorded" >&2
        echo "  run: $current" >&2
    fi
}

vm-check-recorded-box-key() {
    # Usage: vm-check-recorded-box-key VAGRANTDIR KEY
    #
    # Fail unless the VM of an output directory was provisioned with KEY, the
    # versions this run resolved. An empty KEY checks nothing.
    #
    # vagrant provisions a machine once, so an output directory which is kept
    # boots the VM it has however old the Kubernetes, the container runtime,
    # the CNI plugin or Helm in it have become, while the run goes on
    # reporting the versions it resolved. Set e2e_vm_versions to
    #   check:  refuse to run against a VM which does not match (the default)
    #   ignore: warn and run against the VM as it is
    #
    # A run which was asked to provision again only warns: provisioning is
    # what changes the versions in a VM, so refusing to start would block the
    # one thing which resolves the difference. Only provision=all rebuilds
    # the cluster, so only it ends up matching.
    local vagrantdir="$1" key="$2" recorded

    case "${e2e_vm_versions:-check}" in
        check|ignore)
            ;;
        *)
            error "invalid e2e_vm_versions value: \"$e2e_vm_versions\"," \
                  "expected check or ignore"
            ;;
    esac

    if [ -z "$key" ]; then
        return 0
    fi

    recorded="$(vm-recorded-box-key "$vagrantdir")"
    if [ -z "$recorded" ]; then
        echo "WARNING: the versions of the VM in $vagrantdir are not" \
             "recorded, so what is installed in it cannot be told apart" \
             "from what this run resolved." >&2
        return 0
    fi

    if [ "$recorded" = "$key" ]; then
        return 0
    fi

    echo "" >&2
    if vm-hw-mode; then
        echo "$(vm-hw-address) is not provisioned with what this run" \
             "resolved:" >&2
    else
        echo "The VM in $vagrantdir is not provisioned with what this run" \
             "resolved:" >&2
    fi
    vm-report-box-key-mismatch "$recorded" "$key"

    if [ "${provision:-}" = "all" ]; then
        echo "" >&2
        echo "WARNING: provision=all, so the VM is provisioned again," \
             "cluster included, and is expected to end up with what this" \
             "run resolved." >&2
        echo "" >&2
        return 0
    fi

    if [ -n "${provision:-}" ]; then
        echo "" >&2
        echo "WARNING: provision=$provision, so provisioning runs again but" \
             "the cluster is left as it is. Use provision=all to rebuild it" \
             "with the versions above." >&2
        echo "" >&2
        return 0
    fi

    if [ "${e2e_vm_versions:-check}" = "ignore" ]; then
        echo "" >&2
        echo "WARNING: e2e_vm_versions=ignore, so the tests run against the" \
             "VM as it is. Every version this run reports is one it" \
             "resolved, not one it installed." >&2
        echo "" >&2
        return 0
    fi

    # The way out is not the same for the two: a host we were given keeps
    # whatever is installed on it however many directories are deleted.
    if vm-hw-mode; then
        error "$(vm-hw-address) is provisioned with other versions.
Set provision=all to build it again with the ones above, or set
e2e_vm_versions=ignore to run the tests against the host as it is. Deleting
the output directory will not help: the host keeps what is on it."
    fi

    error "the VM in $vagrantdir was provisioned with other versions.
Remove the output directory to provision a new VM with the ones above, set
provision=all to reprovision this one in place, or set e2e_vm_versions=ignore
to run the tests against the VM as it is."
}

vm-box-cache-cleanup-requested() {
    # Usage: vm-box-cache-cleanup-requested
    #
    # Return success if e2e_vm_cache asks for removing the cached boxes.
    case "${e2e_vm_cache:-no}" in
        cleanup|nuke|drop)
            return 0
            ;;
    esac
    return 1
}

vm-cleanup-boxes() {
    # Usage: vm-cleanup-boxes
    #
    # Remove every cached box, printing each of them as it goes.
    #
    # This removes both halves of a cached box: the file in the box cache, and
    # the copy which vagrant unpacked into its own box directory when a VM was
    # created from it. Together they take a couple of gigabytes per box.
    local box name files=0 boxes=0 bytes=0

    for box in "$BOX_CACHE_DIR"/*.box "$BOX_CACHE_DIR"/*.box.tmp.*; do
        if [ ! -f "$box" ]; then
            continue
        fi
        echo "removing $box ($(du -h "$box" | cut -f1))"
        bytes=$(( bytes + $(stat -c %s "$box") ))
        if rm -f "$box"; then
            files=$(( files + 1 ))
        else
            echo "WARNING: failed to remove $box" >&2
        fi
    done

    while read -r name _; do
        case "$name" in
            "$BOX_NAME_PREFIX"/*)
                echo "removing vagrant box $name"
                if vagrant box remove --force "$name" > /dev/null 2>&1; then
                    boxes=$(( boxes + 1 ))
                else
                    echo "WARNING: failed to remove vagrant box $name" >&2
                fi
                ;;
        esac
    done < <(vagrant box list 2>/dev/null)

    if [ "$files" = 0 ] && [ "$boxes" = 0 ]; then
        echo "no cached boxes to remove"
    else
        echo "removed $files cached box file(s), $(( bytes / (1024 * 1024) )) MB" \
             "from $BOX_CACHE_DIR, and $boxes vagrant box(es)"
    fi
}

vm-cached-box-usable() {
    # Usage: vm-cached-box-usable BOXFILE
    #
    # Return success if BOXFILE exists and is not too old to be reused.
    local box="$1"

    if [ ! -f "$box" ]; then
        return 1
    fi
    if [ $(( $(stat -c %Y "$box") + BOX_CACHE_DECAY )) -lt $(date +%s) ]; then
        echo "cached box $box is more than $(( BOX_CACHE_DECAY / 86400 )) days" \
             "old, provisioning the VM from scratch..." >&2
        return 1
    fi
    return 0
}

vm-package-box() {
    # Usage: vm-package-box VAGRANTDIR BOXFILE
    #
    # Export the provisioned VM in VAGRANTDIR into BOXFILE.
    #
    # Note that packaging shuts the VM down, so the caller has to bring it back
    # up. Write to a temporary file first, so that a run which fails or is
    # interrupted halfway does not leave a truncated box behind for the next
    # one to use.
    local vagrantdir="$1" box="$2" tmp="$2.tmp.$$"

    if ! mkdir -p "$(dirname "$box")"; then
        echo "WARNING: cannot create box cache dir $(dirname "$box")" >&2
        return 1
    fi

    echo "packaging the provisioned VM into $box..."
    if ! ( cd "$vagrantdir" && vagrant package --output "$tmp" ); then
        rm -f "$tmp"
        echo "WARNING: failed to package the VM into a box" >&2
        return 1
    fi
    if ! mv "$tmp" "$box"; then
        rm -f "$tmp"
        echo "WARNING: failed to move the packaged box to $box" >&2
        return 1
    fi

    echo "packaged the provisioned VM into $box ($(du -h "$box" | cut -f1))"
    return 0
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
    local qemu_dir="${qemu_dir:-/usr/share/qemu}"
    local efi_code efi_vars kind
    local box_name="$distro" box_file="" package_box="" use_cached_box=""
    local no_provision="" e2e_no_provision="" new_vm="" vm_key=""

    # The key names the versions a VM has, and both the check below and the
    # record at the end of this function need it. Compute it here, before
    # anything expensive happens: it hashes the provisioning recipe, and a
    # recipe file which cannot be read has to be reported rather than
    # discovered once a VM has been built. It stays empty when the versions
    # are not resolved, which leaves the check and the record out.
    if [ -n "$k8s_release" ] && [ -n "$distro" ]; then
        # Outside the command substitution, which would swallow the failure.
        vm-check-provisioning-recipe
        vm_key="$(vm-box-key "$vmname")"
    fi

    if vm-first-run "$vagrantdir"; then
        new_vm=1
    else
        # The host of this output directory is the one the tests will run on,
        # whatever this run resolved, so check the two agree before going on.
        vm-check-recorded-box-key "$vagrantdir" "$vm_key"
    fi

    # A host we were given shares the version check above and nothing below
    # it: the rest of this function is about making a VM.
    if vm-hw-mode; then
        vm-hw-check-cache-conflict
        vm-hw-check-options
        vm-setup-hw "$output_dir" "$vmname" "$vm_key"
        return
    fi

    # Reuse an already provisioned VM if we have one for this topology and for
    # the versions we are about to install. Otherwise provision as usual and
    # keep the result for the next run.
    #
    # This only concerns a VM which does not exist yet. An output directory
    # which already has a Vagrantfile keeps the VM and the box it was created
    # from, so start from a clean output directory to benefit from the cache.
    if [ -n "$new_vm" ] && vm-box-cache-enabled; then
        box_file="$BOX_CACHE_DIR/$(vm-box-key "$vmname").box"
        if [ "$e2e_vm_cache" != "refresh" ] && vm-cached-box-usable "$box_file"; then
            echo "using cached provisioned VM $box_file..."
            use_cached_box=1
            box_name="$(vm-box-name "$vmname")"
            distro_img="file://$box_file"
            # The box already has everything the playbook installs, and
            # kubeadm init cannot run a second time. Keep the provisioner out
            # of the Vagrantfile as well: --no-provision leaves the machine
            # flagged as not provisioned, so the next vagrant up, whether it
            # comes from the next test case or from make ssh, would run it.
            no_provision="--no-provision"
            e2e_no_provision=1
        else
            package_box=1
        fi
    elif grep -q "^IMAGE_NAME = \"$BOX_NAME_PREFIX/" "$vagrantdir/Vagrantfile" 2>/dev/null; then
        # The VM of this output directory was created from a packaged box, so it
        # is provisioned whatever this run was asked to do. Skip provisioning
        # here too: vagrant would otherwise run it either on a VM which is
        # already up, or on one which it is re-importing from that box.
        echo "the VM of this output directory comes from a packaged box," \
             "skipping provisioning..."
        use_cached_box=1
        no_provision="--no-provision"
        e2e_no_provision=1
    fi

    local distro_name=$(printf '%s\n' "$box_name" | sed -e 's/[\/&]/\\&/g')

    mkdir -p "$inventory"
    if [ ! -f "$inventory/vagrant.ini" ]; then
	sed "s/SERVER_NAME/$vmname/g" "$files/vagrant.ini.in" > "$inventory/vagrant.ini"
    fi

    VM_QEMU_CPUMEM=$(echo "$topology" | SEPARATED_OUTPUT_VARS=1 "$LIB_DIR/topology2qemuopts.py")
    if [ "$?" -ne  "0" ]; then
        error "error in topology"
    fi

    local MACHINE=$(echo $VM_QEMU_CPUMEM | sed 's/MACHINE:-machine \([^|]*\).*/\1/g')
    local CPU=$(echo $VM_QEMU_CPUMEM | sed 's/MACHINE:.*CPU:-cpu \([^|]*\).*/\1/g')
    local SMP=$(echo $VM_QEMU_CPUMEM | sed 's/MACHINE:.*CPU:.*SMP:-smp \([^|]*\).*/\1/g')
    local MEM=$(echo $VM_QEMU_CPUMEM | sed 's/MACHINE:.*CPU:.*SMP:.*MEM:-m \([^|]*\).*/\1/g')
    local EXTRA_ARGS=$(echo $VM_QEMU_CPUMEM | sed 's/MACHINE:.*CPU:.*SMP:.*MEM:.*EXTRA:\([^|]*\).*/\1/g')
    local EXTRA_ARGS+="${EXTRA_ARGS:+,} \"-monitor\", \"unix:monitor.sock,server,nowait\""

    case $efi in
        "") ;;
        1) efi=/usr/share/OVMF;;
        /*) ;;
        *) error "invalid efi value: $efi, should be 1 or absolute path to OVMF";;
    esac

    if [ -n "$efi" ]; then
        if [ ! -f "$vagrantdir/OVMF_CODE.fd" -o ! -f "$vagrantdir/OVMF_VARS.fd" ]; then
            for kind in "" _4M; do
                if [ -e "$efi/OVMF_CODE${kind}.fd" -a -e "$efi/OVMF_VARS${kind}.fd" ]; then
                    efi_code="OVMF_CODE${kind}.fd"
                    efi_vars="OVMF_VARS${kind}.fd"
                    break
                fi
            done
            if [ -z "$efi_code" -o -z "$efi_vars" ]; then
                error "EFI requested but OVMF files not found in $efi"
            fi
            echo "copying OVMF files to $vagrantdir..."
            rm -f "$vagrantdir/OVMF_*.fd"
            cp "$efi/$efi_code" "$vagrantdir/OVMF_CODE.fd" || \
                error "cannot copy $efi/$efi_code"
            cp "$efi/$efi_vars" "$vagrantdir/OVMF_VARS.fd" || \
                error "cannot copy $efi/$efi_vars"
        fi

        EXTRA_ARGS+="${EXTRA_ARGS:+,} \"-drive\", \"file=$vagrantdir/OVMF_CODE.fd,format=raw,if=pflash\", \"-drive\", \"file=$vagrantdir/OVMF_VARS.fd,format=raw,if=pflash\""
    fi

    VM_MONITOR="(cd \"$output_dir\" && socat STDIO unix-connect:monitor.sock)"

    if [ "$vagrant_debug" == "1" ]; then
	echo "MACHINE: $MACHINE"
	echo "CPU: $CPU"
	echo "SMP: $SMP"
	echo "MEM: $MEM"
	echo "EXTRA: $EXTRA_ARGS"
        echo "image: ${distro_img:-vagrant default}"
    fi

    if [ -n "$distro_img" ]; then
        CUSTOM_IMAGE="config.vm.box_url = \"$distro_img\""
    else
        CUSTOM_IMAGE="# config.vm.box_url = vagrant default image"
    fi

    if [ ! -f "$vagrantdir/Vagrantfile" ]; then
	sed -e "s/SERVER_NAME/$vmname/g" \
	    -e "s/DISTRO/$distro_name/g" \
	    -e "s/QEMU_MACHINE/$MACHINE/" \
	    -e "s/QEMU_CPU/$CPU/" \
	    -e "s/QEMU_SMP/$SMP/" \
	    -e "s/QEMU_MEM/$MEM/" \
	    -e "s|QEMU_EXTRA_ARGS|$EXTRA_ARGS|" \
            -e "s:QEMU_DIR:$qemu_dir:" \
            -e "s|^.*config.vm.box_url.*$|$CUSTOM_IMAGE|g" \
	    "$files/Vagrantfile.in" > "$vagrantdir/Vagrantfile.erb"
    fi

    if [ ! -f "$vagrantdir/Makefile" ]; then
	sed -e "s/SERVER_NAME/$vmname/g" -e "s/DISTRO/$distro_name/g" "$files/Makefile.in" > "$vagrantdir/Makefile"
    fi

    vm-write-env-file "$vagrantdir" "$e2e_no_provision"

    # The playbook's variables, for whoever ends up running it. This reads the
    # env file, so it has to come after that file is written.
    vm-write-ansible-vars "$vagrantdir"

    (cd "$vagrantdir";
     export ANSIBLE_PIPELINING=True;
     # Make sure the vagrant plugins are installed
     make install || error "failed to check/install vagrant plugins"

     if [ ! -d .vagrant ]; then
	 vagrant init ${vagrant_debug:+--debug} --template Vagrantfile $box_name || \
             error "failed to vagrant init $box_name"
     fi

     # If you want to force provisioning of already provisioned vm,
     # then you can set provision=1 when calling e2e test script.
     # That leaves the cluster alone, so it stops at kubeadm init
     # unless provisioning failed before kubernetes was set up.
     # provision=all takes the cluster down first and rebuilds it.
     #
     # provision=all reaches the playbook through the Vagrantfile,
     # which is generated once, so a VM created before it existed
     # cannot be reprovisioned this way: kubeadm init fails as it
     # used to, and the way out is a clean output directory.
     if [ ! -z "$provision" ]; then
         if ! (export ANSIBLE_SSH_ARGS="$SSH_PERSIST_OPTS"
	  vagrant provision ${vagrant_debug:+--debug} || error "failed to provision VM"); then
             exit 1
         fi
     fi

     if ! (export ANSIBLE_SSH_ARGS="$SSH_PERSIST_OPTS"
           vagrant up $no_provision --provider qemu || error "failed to bring up VM"); then
         exit 1
     fi

     # Keep the freshly provisioned VM for the next run. Packaging shuts the
     # VM down, so bring it back up afterwards. Failing to package is not
     # fatal: the VM we have is fine, we just do not get to reuse it.
     if [ -n "$package_box" ]; then
         vm-package-box "$vagrantdir" "$box_file" || :
         if ! (export ANSIBLE_SSH_ARGS="$SSH_PERSIST_OPTS"
               vagrant up --no-provision --provider qemu || \
                   error "failed to bring the VM back up after packaging it"); then
             exit 1
         fi
     fi

     vagrant ssh-config > .ssh-config
     cat >> .ssh-config <<EOF
  ControlMaster auto
  ControlPersist 60
  ControlPath /tmp/ssh-%C
EOF

     # Add hostname alias to the ssh config so that we can ssh
     # with shorter hostname "node"
     sed -i 's/^Host /Host node /' .ssh-config
    ) || exit $?

    # The VM is up and provisioned, so record what went into it for the next
    # run to check. A VM which was already there keeps the record it has,
    # since overwriting it would hide a mismatch rather than report it, and
    # the exception is provision=all: that run rebuilt the cluster, so the
    # VM really does have what this one resolved.
    if [ -n "$new_vm" ] || [ "${provision:-}" = "all" ]; then
        vm-record-box-key "$vagrantdir" "$vm_key"
    fi

    mkdir -p "$COMMAND_OUTPUT_DIR"
    rm -f "$COMMAND_OUTPUT_DIR"/0*

    # A VM which has just booted from a box, or which was brought back up after
    # being packaged, has a cluster which is still starting up. Wait for it
    # before letting the tests run. Both waits need vm-command, so they cannot
    # happen before the ssh config above is in place.
    if [ -n "$use_cached_box" ] || [ -n "$package_box" ]; then
        wait-for-node-ready
        wait-for-dns-ready
    fi
}

vm-ansible-login-args() {
    # Usage: vm-ansible-login-args VM VAGRANTDIR
    #
    # Print how ansible-playbook is to log in, one argument per line: the
    # user, then the private key. Where those come from is the only thing
    # which differs between a VM we made and a host we were given.
    local vm="$1" vagrantdir="$2"

    if vm-hw-mode; then
        printf '%s\n' "-u" "$(vm-hw-user)" "--private-key=$(vm-hw-key)"
        return 0
    fi

    # private_key may be under qemu/ or libvirt/ directory
    printf '%s\n' "-u" "vagrant" \
           "--private-key=$(echo "$vagrantdir/.vagrant/machines/${vm}"/*/private_key)"
}

vm-play() {
    local vm="$1"
    local playbook="$2"
    local vagrantdir="$3"
    local login_args=()

    mapfile -t login_args < <(vm-ansible-login-args "$vm" "$vagrantdir")

    (cd "$vagrantdir";
     export ANSIBLE_PIPELINING=True;
     # ansible synchronize does not respect --ssh-common-args,
     # therefore pass the same in the environment variable, too
     ANSIBLE_SSH_ARGS="-F $vagrantdir/.ssh-config" ansible-playbook "$playbook" \
	  -i "${vm}," "${login_args[@]}" \
	  --ssh-common-args "-F $vagrantdir/.ssh-config" \
	  --extra-vars "cri_runtime=${k8scri} nri_resource_policy_src=${nri_resource_policy_src} cache_dir=$CACHE_DIR kernel_getsource=$kernel_getsource kernel_config=$kernel_config"
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
    e2eish-mark-command "$1"
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

vm-reboot() { # script API
    # Usage: vm-reboot
    #
    # Reboots the virtual machine and waits that the ssh server starts
    # responding again.

    local _deadline=${deadline:-}
    local _vagrantdir=${1:-$OUTPUT_DIR}
    local _shutdown=0
    local _pid

    if [ -z "$deadline" ]; then
        _deadline=$(( $(date +%s) + ${timeout:-60} ))
    fi

    vm-command "sync; echo 3 > /proc/sys/vm/drop_caches; shutdown -h 0"
    (
        cd "$_vagrantdir"
        while (( $(date +%s) < $_deadline )); do
            vagrant status 2>/dev/null | grep running || {
                echo "vm-reboot: qemu shutdown gracefully"
                _shutdown=1
                break
            }
            sleep 1
        done

        if [ $_shutdown = 0 ]; then
            vagrant halt
            sleep 5
            vagrant status 2>/dev/null | grep running || {
                echo "vm-reboot: qemu shutdown with vagrant halt"
                _shutdown=1
            }
        fi

        if [ $_shutdown = 0 ]; then
            vm-monitor "quit"
            sleep 3
            vagrant status 2>/dev/null | grep running || {
                echo "vm-reboot: qemu shutdown with vm-monitor quit"
                _shutdown=1
            }
        fi

        if [ $_shutdown = 0 ]; then
            for _pid in $(lsof -Fp monitor.sock 2>/dev/null); do
                kill ${_pid#p} || :
            done
            sleep 3
            vagrant status 2>/dev/null | grep running || {
                echo "vm-reboot: qemu shutdown with SIGTERM"
                _shutdown=1
            }
        fi

        if [ $_shutdown = 0 ]; then
            for _pid in $(lsof -Fp monitor.sock 2>/dev/null); do
                kill -9 ${_pid#p} || :
            done
            sleep 3
            vagrant status 2>/dev/null | grep running || {
                echo "vm-reboot: qemu shutdown with SIGKILL"
                _shutdown=1
            }
        fi

        if [ $_shutdown = 0 ]; then
            vagrant status 2>/dev/null | grep running && {
                error "vm-reboot: immortal qemu error, cannot reboot"
            }
        fi

        vagrant up --no-provision
    )
    deadline=$_deadline host-wait-vm-ssh-server $_vagrantdir
}

vm-cpu-hotplug() { # script API
    # Usage: vm-cpu-hotplug SOCKETID COREID THREADID
    #
    # Hotplug currently unplugged CPU to VM.
    #
    # Examples:
    #   vm-cpu-hotplug 0 255 0
    local socketid=$1
    local coreid=$2
    local threadid=$3
    local deviceid="cpu-s$socketid-c$coreid-t$threadid"
    if [[ -z "$threadid" ]]; then
        error "missing one or more IDs: socket core thread"
        return 1
    fi
    vm-monitor "device_add driver=host-x86_64-cpu,id=${deviceid},socket-id=${socketid},core-id=${coreid},thread-id=${threadid}"
}

vm-cpu-hotremove() { # script API
    # Usage: vm-cpu-hotremove SOCKETID COREID THREADID
    #
    # Hotremove currently plugged CPU from VM.
    #
    # Examples:
    #   vm-cpu-hotremove 0 255 0
    local socketid=$1
    local coreid=$2
    local threadid=$3
    local deviceid="cpu-s$socketid-c$coreid-t$threadid"
    if [[ -z "$threadid" ]]; then
        error "missing one or more IDs: socket core thread"
        return 1
    fi
    vm-monitor "device_del ${deviceid}"
}

_vm_cxl_hotplug_count=""

vm-cxl-hw() { # script API
    # Usage: vm-cxl-hw
    #
    # List hotpluggable and removable cxl memory devices
    # See also: vm-cxl-hotplug, vm-cxl-hotremove
    local plugged plugged_id
    declare -A plugged
    for plugged_id in $(vm-monitor "info qtree -b" | awk -F\" '/dev: cxl-type3/{print $2}' | sed 's/\.hp.*//g'); do
        plugged[$plugged_id]=1
    done
    vm-monitor "info memdev" | awk '/ beram_cxl_memdev/{print $3}' | while read beram_id; do
        read dev bus sn <<< "$(sed -e 's/^beram_\(cxl_memdev[0-9]\+\)__bus_\(.*\)__sn_\(.*\)$/\1 \2 \3/g' <<< "$beram_id")"
        echo -n "$dev"
        [ "$show_bus" = 1 ] && echo -n " bus=$bus"
        [ "$show_sn" = 1 ] && echo -n " sn=$sn"
        [ "$show_be" = 1 ] && echo -n " volatile-memdev=$beram_id"
        [ "${plugged[$dev]}" = 1 ] && echo -n " plugged"
        echo
    done
}

vm-cxl-hotplug() { # script API
    # Usage: vm-cxl-hotplug
    #
    # Hotplug CXL memory device.
    #
    # Example: vm-cxl-hotplug cxl_memdev1
    local memmatch memline devadd
    memmatch=$1
    if [ -z "$memmatch" ]; then
        error "missing CXL_MEMDEV"
        return 1
    fi
    memline="$(show_bus=1 show_sn=1 show_be=1 vm-cxl-hw | grep "${memmatch}__bus")"
    if [ -z "$memline" ]; then
        error "no cxl memory devices matching '$memmatch'"
        return 1
    fi
    while read dev bus sn be dontcare; do
        # Qemu does not allow hotplugging a device with same ID twice,
        # even if it would be deleted. Workaround by adding a hotplug
        # counter as device ID suffix
        _vm_cxl_hotplug_count=$(( _vm_cxl_hotplug_count + 1 ))
        dev=${dev}.hp${_vm_cxl_hotplug_count}
        vm-monitor "device_add cxl-type3,$bus,$be,id=$dev,$sn"
    done <<< "$memline"
}

vm-cxl-hotremove() { # script API
    # Usage: vm-cxl-remove
    #
    # Hotremove CXL memory device.
    #
    # Example: vm-cxl-remove cxl_memdev1
    local memmatch memline devadd
    memmatch=$1
    if [ -z "$memmatch" ]; then
        error "missing CXL_MEMDEV"
        return 1
    fi
    memline="$(vm-monitor "info qtree -b" | awk -F\" '/dev: cxl-type3/{print $2}' | grep "$memmatch")"
    echo "$memline" | while read dev dontcare; do
        vm-monitor "device_del $dev"
    done
}

vm-mem-hotplug() { # script API
    # Usage: vm-mem-hotplug MEMORY
    #
    # Hotplug currently unplugged MEMORY to VM.
    # Find unplugged memory with "vm-mem-hw | grep unplugged".
    #
    # Examples:
    #   vm-mem-hotplug mem2
    local memmatch memline memid memdimm memnode memdriver
    memmatch=$1
    if [ -z "$memmatch" ]; then
        error "missing MEMORY"
        return 1
    fi
    memline="$(vm-mem-hw | grep unplugged | grep "$memmatch")"
    if [ -z "$memline" ]; then
        error "unplugged memory matching '$memmatch' not found"
        return 1
    fi
    memid="$(awk '{print $1}' <<<"$memline")"
    memid=${memid#mem}
    memid=${memid%[: ]*}
    memdimm="$(awk '{print $2}' <<<"$memline")"
    memnode="$(awk '{print $4}' <<<"$memline")"
    memnode=${memnode#node}
    if [ "$memdimm" == "nvdimm" ]; then
        memdriver="nvdimm"
    else
        memdriver="pc-dimm"
    fi
    vm-monitor "device_add ${memdriver},id=${memdimm}${memid},memdev=mem${memdimm}_${memid}_node_${memnode},node=${memnode}"
}

vm-mem-hotremove() { # script API
    # Usage: vm-mem-hotremove MEMORY
    #
    # Hotremove currently plugged MEMORY from VM.
    # Find plugged memory with "vm-mem-hw | grep ' plugged'".
    #
    # Examples:
    #   vm-mem-hotremove mem2
    local memmatch memline memid memdimm memnode memdriver
    memmatch=$1
    if [ -z "$memmatch" ]; then
        error "missing MEMORY"
        return 1
    fi
    memline="$(vm-mem-hw | grep \ plugged | grep "$memmatch")"
    if [ -z "$memline" ]; then
        error "plugged memory matching '$memmatch' not found"
        return 1
    fi
    memid="$(awk '{print $1}' <<<"$memline")"
    memid=${memid#mem}
    memid=${memid%[: ]*}
    memdimm="$(awk '{print $2}' <<<"$memline")"
    vm-monitor "device_del ${memdimm}${memid}"
}

vm-mem-hw() { # script API
    # Usage: vm-mem-hw
    #
    # List VM memory hardware with current status.
    # See also: vm-mem-hotplug, vm-mem-hotremove
    vm-monitor "$(
        echo info memdev
        echo info memory-devices
    )" | awk '
      /memdev: /{
          split($2,a,"_");
          state[a[2]]="plugged  ";
      }
      /memory backend: membuiltin/{
          split($3,a,"_"); backend=1;
          type[a[2]]="ram    "; state[a[2]]="builtin  "; node[a[2]]=a[4];
      }
      /memory backend: memnvbuiltin/{
          split($3,a,"_"); backend=1;
          type[a[2]]="nvram  "; state[a[2]]="builtin  "; node[a[2]]=a[4];
      }
      /memory backend: memnvdimm/{
          split($3,a,"_"); backend=1;
          type[a[2]]="nvdimm "; state[a[2]]="unplugged"; node[a[2]]=a[4];
      }
      /memory backend: memdimm/{
          split($3,a,"_"); backend=1;
          type[a[2]]="dimm   "; state[a[2]]="unplugged"; node[a[2]]=a[4];
      }
      /size: /{sz=$2/1024/1024; if (backend==1) {size[a[2]]=sz;backend=0;}}
      END{
          for (m in node) print "mem"m": "type[m]" "state[m]" node"node[m]" size="size[m]"M";
      }'
}

vm-monitor() { # script API
    # Usage: vm-monitor COMMAND
    #
    # Execute COMMAND on Qemu monitor.
    #
    # Example: VM monitor help:
    #  vm-monitor "help" | less
    #
    # Example: print memdev objects and plugged in memory devices:
    #  vm-monitor "info memdev"
    #  vm-monitor "info memory-devices"
    #
    # Example: hot plug a NVDIMM to NUMA node 1 when launched with topology
    # topology='[{"cores":2,"mem":"2G"},{"nvmem":"4G","dimm":"unplugged"}]':
    #   vm-monitor "device_add pc-dimm,id=nvdimm0,memdev=nvmem0,node=1"
    [ -n "$VM_MONITOR" ] ||
        error "VM is not running"
    eval "$VM_MONITOR" <<<"$1" | sed 's/\r//g'
    if [ "${PIPESTATUS[0]}" != "0" ]; then
        error "sending command to Qemu monitor failed"
    fi
    echo ""
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

    # Print a single pod and never a terminating one.
    local POD="$(vm-command-q "kubectl get pods $namespace_args | \
        awk '/${pod_regexp}/ && \$3 != \"Terminating\" { print \$1; exit }'")"
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

# We can collect coverage data from the instrumented plugins during e2e
# tests. The data is dumped by the go runtime during graceful exit, and
# the plugins can also serve it over HTTP. Set up here the dump directory
# for the runtime, the URL and command the tests can use to pull data
# over HTTP and the subdirectory for coverage reports. We keep reports
# in their own directory, separate from other coverage data.

vm_coverage_dir=/var/lib/nri-resource-policy/coverage
vm_coverage_url=http://localhost:8891/coverage
vm_coverage_curl="curl --silent --show-error --fail --noproxy localhost"
vm_coverage_report_dir=coverage-report

# Print GOCOVERDIR we should launch plugins with.
vm-coverage-gocoverdir() {
    echo "$vm_coverage_dir"
}

# Return success if earlier coverage data should be discarded before running
# any tests. Refuse anything but 1 and 0, so that an unrecognized value cannot
# silently leave the data of earlier runs in the report.
vm-coverage-reset-requested() {
    case "${reset_coverage:-0}" in
        1)
            return 0
            ;;
        0)
            return 1
            ;;
    esac

    error "invalid reset_coverage=\"$reset_coverage\", expected 1 or 0"
}

# Discard all coverage data and the report collected under the given
# directory if coverage reset was requested.
vm-coverage-discard-collected() {
    local dir="$1"

    vm-coverage-reset-requested || return 0

    case "$dir" in
        ""|/)
            echo "WARNING: refusing to discard coverage data under \"$dir\""
            return 0
            ;;
    esac
    [ -d "$dir" ] || return 0

    echo "Discarding all coverage data collected earlier under $dir..."

    find "$dir" -type f \( -path '*/coverage/covmeta.*' \
                           -o -path '*/coverage/covcounters.*' \) -delete
    rm -rf "$dir/$vm_coverage_report_dir"
}

# Discard the coverage data an earlier test left on the VM.
vm-coverage-reset() {
    vm-command-q "rm -rf $vm_coverage_dir && mkdir -p $vm_coverage_dir" ||
        echo "WARNING: failed to reset coverage directory $vm_coverage_dir"
}

# Prepare for collecting the coverage data of a plugin about to be launched.
vm-coverage-prepare() {
    vm-command-q "mkdir -p $vm_coverage_dir" ||
        echo "WARNING: failed to create coverage directory $vm_coverage_dir"
}

# Dump the coverage data of the running plugin on the VM.
vm-coverage-snapshot() {
    local id

    id=$(vm-command-q "$vm_coverage_curl $vm_coverage_url/id")
    if [ -z "$id" ]; then
        # uninstrumented build, test APIs off, or a terminated plugin
        echo "no coverage data to dump from the plugin"
        return 0
    fi

    vm-command-q "cd $vm_coverage_dir && \
        $vm_coverage_curl -o covmeta.$id $vm_coverage_url/meta && \
        $vm_coverage_curl -o covcounters.$id.1.\$(date +%s%N) $vm_coverage_url/counters" \
        >/dev/null ||
        echo "WARNING: failed to dump coverage data of the plugin"
}

# Copy the coverage data collected on the VM to the host.
vm-coverage-collect() {
    local dir="$1"

    if ! vm-command-q "ls $vm_coverage_dir/covmeta.* >/dev/null 2>&1"; then
        echo "no coverage data collected on the VM"
        return 0
    fi

    # Start from scratch, so that we don't mix in the data of an earlier
    # run of this test, potentially with a different build of the plugin.
    rm -rf "$dir"
    if ! mkdir -p "$dir"; then
        echo "WARNING: failed to create coverage directory $dir"
        return 0
    fi

    host-command "$SCP $VM_HOSTNAME:$vm_coverage_dir/'cov*' \"$dir/\"" ||
        echo "WARNING: copying coverage data from the VM failed"
}

vm-seconds-now() {
    vm-command-q "date +%s"
}

vm-seconds-since() {
    echo $(( $(vm-seconds-now) - $1 + 1 ))
}

vm-pull-journal() {
    local _service="${service:+-u} ${service:-} "
    local _since="${since:+--since }${since:-}"

    vm-command-q "journalctl $_service $_since" || \
        command-error "failed to pull journal logs (service: ${service:-all}, since: ${since:--}"
}

fedora-set-kernel-cmdline() {
    local e2e_defaults="$*"
    vm-command "mkdir -p /etc/default; touch /etc/default/grub; sed -i '/e2e:fedora-set-kernel-cmdline/d' /etc/default/grub"
    vm-command "echo 'GRUB_CMDLINE_LINUX_DEFAULT=\"\${GRUB_CMDLINE_LINUX_DEFAULT} ${e2e_defaults}\" # by e2e:fedora-set-kernel-cmdline' >> /etc/default/grub" || {
        command-error "writing new command line parameters failed"
    }
    vm-command "grub2-mkconfig -o /boot/grub2/grub.cfg" || {
        command-error "updating grub failed"
    }
}

ubuntu-set-kernel-cmdline() {
    local e2e_defaults="$*"
    vm-command "echo 'GRUB_CMDLINE_LINUX_DEFAULT=\"\${GRUB_CMDLINE_LINUX_DEFAULT} ${e2e_defaults}\"' > /etc/default/grub.d/60-e2e-defaults.cfg" || {
        command-error "writing new command line parameters failed"
    }
    vm-command "update-grub" || {
        command-error "updating grub failed"
    }
}

vm-set-kernel-cmdline() {
    if [[ "$distro" == *fedora* ]]; then
        fedora-set-kernel-cmdline "$*"
    else
        ubuntu-set-kernel-cmdline "$*"
    fi
}

vm-cpus-enabled() { # script API
    # Usage: vm-cpus-enabled CPU...
    #
    # Return success if all the given CPUs are enabled in the VM, that is,
    # they are either present from the start or have been hot-plugged.
    #
    # Example:
    #   vm-cpus-enabled 511 1535 || vm-cpu-hotplug ...
    local enabled cpu
    vm-command "cat /sys/devices/system/cpu/enabled"
    enabled=$(expand-cpulist "$(tr -d '[:space:]' <<< "$COMMAND_OUTPUT")")
    for cpu in "$@"; do
        [[ " $enabled " == *" $cpu "* ]] || return 1
    done
    return 0
}

vm-wait-cpus() { # script API
    # Usage: vm-wait-cpus CPU...
    #
    # Wait until the kernel of the VM has exposed all the given CPUs in sysfs.
    # Use this after hot-plugging CPUs.
    local cpu test=""
    for cpu in "$@"; do
        test="${test}${test:+ && }[ -d /sys/devices/system/cpu/cpu$cpu ]"
    done
    vm-run-until "$test"
}

vm-online-all-cpus() { # script API
    # Usage: vm-online-all-cpus
    #
    # Bring every offline CPU of the VM online. Print the resulting state of
    # each CPU. Never fails: a CPU which cannot be onlined is reported but
    # tolerated, as not all of them can be.
    vm-command 'for cpuX in /sys/devices/system/cpu/cpu[1-9]*; do
            echo onlining $cpuX
            ( echo 1 > $cpuX/online && echo Successful: write 1 to $cpuX/online ) || echo Failed: write 1 to $cpuX/online
        done
       grep . /sys/devices/system/cpu/cpu[1-9]*/online'
}

vm-restart-kubelet() { # script API
    # Usage: vm-restart-kubelet
    #
    # Restart kubelet and wait until the node is ready again.
    vm-command "systemctl restart kubelet"
    wait-for-node-ready
}

vm-set-kernel-cmdline-reboot() { # script API
    # Usage: [timeout=SECS] vm-set-kernel-cmdline-reboot [CMDLINE]
    #
    # Set the kernel command line parameters of the VM to CMDLINE, none by
    # default, reboot the VM, and wait until the node is ready again. Verify
    # that CMDLINE took effect, and fail the test if it did not.
    #
    # timeout is the time to wait for the VM to reboot, 600 seconds by default.
    local cmdline="$1"

    vm-set-kernel-cmdline "$cmdline"
    timeout=${timeout:-600} vm-reboot
    if [ -n "$cmdline" ]; then
        vm-command "grep -q -- '$cmdline' /proc/cmdline" ||
            error "failed to set kernel command line parameters \"$cmdline\""
    fi
    e2eish-mark machine cmdline="$cmdline"
    vm-restart-kubelet
}

vm-kernel-pkgs-install() { # script API
    # Usage: vm-kernel-pkgs-install
    #
    # Install custom kernel packages from a tar file.
    # Does nothing if requested packages are already installed.
    #
    # Environment variables:
    #   file       contains the name of the tarball on vm.
    #              The default is ~/kernel-pkgs.tar.
    #
    # Saves previous default to ~/.vm-kernel-pkgs.default-kernel.
    # That kernel can be restored with vm-kernel-pkgs-uninstall.
    local file="${file:-kernel-pkgs.tar}"
    local feat="vm-kernel-pkgs"

    vm-command "tar tf $file" || \
        command-error "cannot list contents of kernel packages file $file"
    local to_be_installed="$COMMAND_OUTPUT"

    vm-command "cat .$feat.installed_packages 2>/dev/null || echo ''" || \
        command-error "cannot read previously installed kernel packages list"
    local already_installed="$COMMAND_OUTPUT"

    if [ "$to_be_installed" == "$already_installed" ]; then
        echo "vm-kernel-pkgs-install: kernel packages from $file already installed, skipping installation"
        return 0
    fi

    vm-command "[ -d $feat ] && rm -rf $feat; mkdir $feat; tar xvf $file -C $feat | tee .$feat.extracted_packages" ||
        command-error "cannot extract kernel packages from $file"

    if grep -q .rpm <<< "$to_be_installed"; then
        # Store current kernel / Fedora
        vm-command "grubby --default-kernel | tee .$feat.default-kernel" || \
            command-error "cannot save current default kernel"
        # Install new kernel packages / Fedora
        vm-command "rpm -Uvh --force $feat/*.rpm" || \
            command-error "cannot install kernel rpm packages from $file"
    elif grep -q .deb <<< "$to_be_installed"; then
        # Store current kernel / Ubuntu
        vm-command "uname -r | tee .$feat.default-kernel"
        # Install new kernel packages / Ubuntu
        vm-command "dpkg -i $feat/*.deb" || \
            command-error "cannot install kernel deb packages from $file"
        # In lack of grubby, set GRUB_DEFAULT in both "original" (for current) and "override" (for new kernel) configs.
        # They both boot the system with current kernel and new kernel, respectively.
        # When wish to change back from the new to the original kernel, it suffices to remove the "override" config
        # and update grub.
        vm-command "
            oldkernelversion=\$(uname -r)
            newkernelversion=\$(dpkg --contents $feat/linux-image-*.deb | awk -Fvmlinuz- /vmlinuz-/'{print \$2}')
            submenu=\$(grep -i submenu.*advanced /boot/grub/grub.cfg | awk -F\' '{print \$2}')
            oldmenuchoice=\$(grep \"menuentry.*\$oldkernelversion\" /boot/grub/grub.cfg | grep -v recovery | awk -F\' '{print \$2}')
            newmenuchoice=\$(grep \"menuentry.*\$newkernelversion\" /boot/grub/grub.cfg | grep -v recovery | awk -F\' '{print \$2}')
            echo 'Save original grub default'
            echo \"GRUB_DEFAULT='\$submenu>\$oldmenuchoice'\" | tee /etc/default/grub.d/e2e-00-original-default-kernel.cfg
            echo 'Override original grub default with:'
            echo \"GRUB_DEFAULT='\$submenu>\$newmenuchoice'\" | tee /etc/default/grub.d/e2e-01-override-default-kernel.cfg
            update-grub
            "
    else
        command-error "no kernel packages found in $file"
    fi
    vm-command "cat .$feat.extracted_packages >> .$feat.installed_packages && rm .$feat.extracted_packages" || \
        command-error "cannot save installed kernel packages list"

    echo "Booting to new kernel..."
    vm-reboot || \
        error "vm-kernel-pkgs-install: reboot failed after installing new kernel packages"

    vm-command "uname -a"
}

vm-kernel-pkgs-uninstall() { # script API
    # Usage: vm-kernel-pkgs-uninstall
    #
    # Boot to previous kernel before vm-kernel-pkgs-install and
    # uninstall custom kernel packages installed with vm-kernel-pkgs-install.
    local feat="vm-kernel-pkgs"
    local default_kernel
    local installed_packages
    vm-command "cat .$feat.default-kernel" || {
        echo "vm-kernel-pkgs-uninstall: kernel-pkgs not installed"
        return 0
    }

    default_kernel="$COMMAND_OUTPUT"
    if [ -z "$default_kernel" ]; then
        command-error "cannot restore previous default kernel, file ~/.$feat.default-kernel is missing or empty"
    fi

    vm-command "cat .$feat.installed_packages" || \
        command-error "cannot find installed kernel packages list file ~/.$feat.installed_packages"
    installed_packages="$COMMAND_OUTPUT"
    if [ -z "$installed_packages" ]; then
        command-error "cannot uninstall kernel packages, file ~/.$feat.installed_packages is missing or empty"
    fi

    # Modify grub to select previous kernel on next boot
    if grep -q rpm <<< "$installed_packages"; then
        vm-command "grubby --set-default=\"$default_kernel\"" || \
            command-error "cannot restore previous default kernel to $default_kernel"
    else
        vm-command "rm -f /etc/default/grub.d/e2e-01-override-default-kernel.cfg; update-grub"
    fi

    echo "Booting to previous kernel: $default_kernel."
    vm-reboot

    # Uninstall previously installed custom kernel packages.
    if grep -q rpm <<< "$installed_packages"; then
        vm-command "sed 's/.rpm//g' < .$feat.installed_packages | xargs rpm -e --nodeps" || \
            command-error "failed to uninstall packages: $installed_packages"
    elif grep -q deb <<< "$installed_packages"; then
        vm-command "xargs -a .$feat.installed_packages dpkg -r" || \
            command-error "failed to uninstall packages: $installed_packages"
        vm-command "rm -f /etc/default/grub.d/e2e-00-original-default-kernel.cfg; update-grub"
    else
        command-error "no kernel packages found in ~/.$feat.installed_packages"
    fi
    vm-command "rm -f .$feat.installed_packages .$feat.default-kernel" || \
        command-error "cannot remove ~/.$feat.installed_packages and ~/.$feat.default-kernel files"

    vm-command "uname -a"
}

vm-post-reboot-runtime-check() { # script API
    # Usage: vm-post-reboot-runtime-check [image_type]
    #
    # Check whether a reboot rendered some runtime image blobs unreadable
    # and try to fix it. Optionally also reimport and tag a local image
    # from a tarball for the given (balloons or topology-aware) image type.
    #
    # The blob problem is known to be triggerable when the underlying fs
    # is BTRFS and the old or new running kernel was from Torvalds tree
    # ('vanilla' in provisioning). The exact reasons are unknown.

    local image_type=$1

    if ! $k8scri-check-unreadable-blobs; then
        echo "no post-boot runtime blob read failures detected..."
        return 0
    fi

    echo "post-boot runtime blob read failures detected... trying to fix them"
    vm-command "systemctl stop kubelet"
    vm-command "systemctl stop $k8scri"
    vm-command "killall -9 etcd"
    vm-command "killall -9 kube-apiserver"
    vm-command "killall -9 kube-scheduler"
    vm-command "killall -9 kube-controller-manager"

    $k8scri-fix-unreadable-blobs

    vm-command "systemctl start $k8scri"
    vm-command "systemctl start kubelet"

    wait-for-node-ready

    $k8scri-reimport-image $image_type
}

wait-for-dns-ready() {
    # Usage: [timeout=SECS] wait-for-dns-ready
    #
    # Wait until the cluster DNS is available, $CLUSTER_READY_TIMEOUT seconds
    # at most.
    #
    # The cluster of a VM which has just booted is still starting up. A test
    # which creates pods before the DNS is up can fail for reasons which have
    # nothing to do with what it tests, so wait for it here.
    #
    # Note that kubectl wait fails immediately if it cannot reach the API
    # server, so wait for the node to be ready before calling this.
    local timeout="${timeout:-$CLUSTER_READY_TIMEOUT}"

    vm-command "kubectl -n kube-system wait deployments/coredns \
                    --for=condition=Available --timeout=${timeout}s" ||
        command-error "cluster DNS did not become available in ${timeout}s"
}

wait-for-node-ready() {
    local now=$(date +%s)
    local deadline=$(($now + 5 * 60))

    while true; do
        vm-command "crictl ps | grep kube-apiserver | grep Running"
        if ! grep -q Running <<< $COMMAND_OUTPUT; then
            sleep 5
        else
            vm-command "kubectl wait --for=condition=Ready=True nodes/$VM_HOSTNAME --timeout=15s"
            if grep -q "condition met" <<< $COMMAND_OUTPUT; then
                break
            fi

            sleep 1
            now=$(date +%s)
            if [ $now -gt $deadline ]; then
                command-error "failed to wait for VM k8s node to get ready"
            fi
        fi
    done
}

containerd-check-unreadable-blobs() {
    vm-command "journalctl -u containerd -I | grep blob | grep 'operation not supported' | \
                   head -1"
    return $?
}

containerd-fix-unreadable-blobs() {
    vm-command "mv /var/lib/containerd /var/lib/containerd.unreadable-blobs.$(date +%s)" || \
        command-error "failed to fix unreadable blobs"
    vm-command "mkdir /var/lib/containerd" || \
        command-error "failed to fix unreadable blobs"
}

containerd-reimport-image() {
    local image_type="$1" tarball="" sha256="" ref="" tag=""

    if [ -z "$image_type" ]; then
        return 0
    fi

    # Resolve which image to reimport on the host rather than on the node. A
    # node accumulates one tarball per distinct build deployed to it, so the
    # name is ambiguous there, and insisting on exactly one of them failed
    # every reboot once a node had seen a second build. A test which reboots
    # to change the kernel was then left on that kernel, which breaks every
    # test which runs after it.
    #
    # Take the newest tarball of this image type from build/images which is
    # also present on the node: a newer build on the host might never have
    # been deployed here, and only a tarball which is on the node can be
    # reimported there. 'ls -1t' orders the host builds newest first and
    # 'grep -f' preserves that order, so the first match is the newest
    # deployed build, which is the one running on the node.
    local on_node="" host_builds=""

    on_node=$(vm-command-q "ls -1 *$image_type*-image-*.tar 2>/dev/null")
    host_builds=$(cd "$nri_resource_policy_src/build/images" && \
                      ls -1t *"$image_type"*-image-*.tar 2>/dev/null)
    tarball=$(grep -Fx -m 1 -f <(echo "$on_node") <<< "$host_builds")

    if [ -z "$tarball" ]; then
        command-error "failed to reimport image, no $image_type image tarball in build/images and on the node"
    fi
    echo "reimporting $image_type image $tarball..."

   case $tarball in
        *balloons*)
            tag=localhost/balloons:testing
            ;;
        *topology-aware*)
            tag=localhost/topology-aware:testing
            ;;
        *)
            command-error "failed to reimport, unknown image type/name for $image_type ($tarball)"
            ;;
    esac

    vm-command "ctr -n k8s.io images import $tarball" || \
        command-error "failed to reimport image tarball $tarball"

    sha256=${tarball%.tar}
    sha256=${sha256##*-image-}

    vm-command "ctr -n k8s.io images ls | grep sha256:$sha256 | head -n 1 | tr -s '\t' ' ' | cut -d ' ' -f1"
    ref="$COMMAND_OUTPUT"
    if [ -z "$ref" ]; then
        command-error "failed to resolve image ref for sha256:$sha256"
    fi

    vm-command "ctr -n k8s.io images tag --force $ref $tag" || \
        command-error "failed to tag reimported image"
}

crio-check-unreadable-blobs() {
    echo "TODO: implement unreadable blob check for CRI-O... assuming no errors for now"
}

crio-fix-unreadable-blobs() {
    echo "TODO: implement unreadable blob fix for CRI-O (if possible)... assuming no-op for now"
}

crio-reimport-image() {
    echo "TODO: implement image reimport for CRI-O... assuming no-op for now"
}
