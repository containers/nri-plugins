source "$(dirname "${BASH_SOURCE[0]}")/command.bash"

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

# Where it is recorded that a VM has been provisioned.
#
# Provisioning has to leave a mark, because nothing around it says whether it
# ever ran to the end. A Vagrantfile appears before the VM is even created, and
# the provisioned flag of vagrant is cleared by the --no-provision of a VM which
# comes from a box, so a run which was interrupted or which failed while
# provisioning would otherwise look, to the next one, exactly like a run which
# got all the way through. That next run then skips provisioning and every test
# case fails on a VM which has no cluster.
#
# There are two marks. The one in the output directory is what a run reads when
# it decides whether to provision. The one in the VM travels inside the disk
# image, so a VM created from a packaged box carries it, and a box can be checked
# rather than taken at its word.
PROVISIONED_STAMP=".provisioned"
VM_PROVISIONED_STAMP="/etc/nri-e2e-provisioned"

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
    local vmname="$1" cri_release key

    case "$k8scri" in
        crio) cri_release="$crio_release";;
        *)    cri_release="$containerd_release";;
    esac

    key="$vmname-k8s$k8s_release-$k8scri$cri_release"
    key="$key-cni$cni_plugin$cni_release-helm$helm_release"
    key="$key-$(vm-provisioning-recipe-hash)"

    echo "${key//[^A-Za-z0-9._-]/-}"
}

vm-box-name() {
    # Usage: vm-box-name VMNAME
    #
    # Print the name of the vagrant box of the packaged VM of VMNAME.
    echo "$BOX_NAME_PREFIX/$(vm-box-key "$1")"
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

vm-provisioned() {
    # Usage: vm-provisioned VAGRANTDIR
    #
    # Return success if the VM of VAGRANTDIR has been provisioned by a run which
    # got all the way through, see PROVISIONED_STAMP.
    [ -f "$1/$PROVISIONED_STAMP" ]
}

vm-provisioned-from-box() {
    # Usage: vm-provisioned-from-box VAGRANTDIR
    #
    # Return success if the VM of VAGRANTDIR was created from a packaged box
    # instead of being provisioned in place.
    grep -q '^box=.' "$1/$PROVISIONED_STAMP" 2>/dev/null
}

vm-mark-provisioned() {
    # Usage: vm-mark-provisioned VAGRANTDIR VMNAME [BOXNAME]
    #
    # Record that the VM of VAGRANTDIR is provisioned and ready to run tests.
    # BOXNAME names the packaged box it was created from, if it came from one.
    local vagrantdir="$1" vmname="$2" box="${3:-}"

    {
        echo "# Written by vm-setup once this VM was up with everything the"
        echo "# provisioning playbook installs. Remove this file to have the"
        echo "# next run provision the VM again."
        echo "vm=$vmname"
        echo "box=$box"
        echo "k8s=$k8s_release"
        echo "k8scri=$k8scri"
        echo "cni=$cni_plugin$cni_release"
        echo "helm=$helm_release"
        echo "date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    } > "$vagrantdir/$PROVISIONED_STAMP" ||
        error "cannot record the provisioning of VM $vmname"
}

vm-unmark-provisioned() {
    # Usage: vm-unmark-provisioned VAGRANTDIR
    #
    # Forget that the VM of VAGRANTDIR is provisioned, for as long as it takes to
    # provision it again.
    rm -f "$1/$PROVISIONED_STAMP"
}

vm-verify-provisioned() {
    # Usage: vm-verify-provisioned VMNAME
    #
    # Fail unless the VM which is up has been provisioned, that is, unless the
    # playbook wrote VM_PROVISIONED_STAMP into it.
    #
    # This is what catches a VM whose provisioning never finished, and a cached
    # box which was packaged from one.
    local vmname="$1"

    if vm-command-q "[ -f $VM_PROVISIONED_STAMP ]" > /dev/null; then
        return 0
    fi

    error "VM $vmname does not look provisioned: $VM_PROVISIONED_STAMP is missing.
Its provisioning either never finished, or the VM comes from a cached box or an
output directory which predates this check. Start from a clean output directory,
add e2e_vm_cache=refresh to replace the cached box, or provision=1 to provision
this VM again."
}

vm-kubeadm-reset() {
    # Usage: vm-kubeadm-reset VAGRANTDIR
    #
    # Tear down the Kubernetes cluster of the VM of VAGRANTDIR, if it has one.
    #
    # Provisioning ends in "kubeadm init", which fails on a VM which already has
    # a cluster: the ports are taken, the manifests are in place and etcd has
    # data. So provisioning a VM again has to start by resetting whatever is
    # there. A VM which is not up, or which never got as far as installing
    # kubeadm, has nothing to reset.
    local vagrantdir="$1"
    local ids=( "$vagrantdir"/.vagrant/machines/*/*/id )

    if [ ! -f "${ids[0]}" ]; then
        # The VM has not been created yet, so there is no cluster in it either.
        return 0
    fi

    echo "resetting the Kubernetes cluster of the VM before provisioning it again..."
    if ! ( cd "$vagrantdir" && vagrant ssh -c "sudo sh -xc '
               command -v kubeadm > /dev/null || exit 0
               kubeadm reset --force || true
               rm -rf /etc/cni/net.d /root/.kube /home/vagrant/.kube
               rm -f $VM_PROVISIONED_STAMP'" ); then
        echo "WARNING: could not reset the cluster of the VM." \
             "Provisioning it as it is..." >&2
    fi
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
    local no_provision="" e2e_no_provision=""

    # Decide what this run does about provisioning.
    #
    # Skip it for a VM which has already been provisioned, and only for such a
    # VM. The mark is written once the playbook has run to the end, or once a VM
    # created from a box has been checked to carry it, see PROVISIONED_STAMP.
    #
    # Reuse an already provisioned VM if we have one for this topology and for
    # the versions we are about to install. Otherwise provision as usual and
    # keep the result for the next run.
    #
    # The box cache only concerns a VM which does not exist yet. An output
    # directory which already has a Vagrantfile keeps the VM and the box it was
    # created from, so start from a clean output directory to benefit from the
    # cache.
    if [ -n "$provision" ]; then
        # Provisioning was asked for explicitly, so provision whatever is here.
        # Until that succeeds this VM does not count as provisioned.
        vm-unmark-provisioned "$vagrantdir"
    elif vm-provisioned "$vagrantdir"; then
        echo "VM $vmname is already provisioned, skipping provisioning..."
        # Keep the provisioner out of the Vagrantfile too: --no-provision leaves
        # the machine flagged as not provisioned, so the next vagrant up, whether
        # it comes from the next test case or from make ssh, would run it.
        no_provision="--no-provision"
        e2e_no_provision=1
        # The cluster of a VM which came from a box starts up when the VM boots,
        # so it may still be starting, see the wait at the end of this function.
        if vm-provisioned-from-box "$vagrantdir"; then
            use_cached_box=1
        fi
    elif [ ! -f "$vagrantdir/Vagrantfile" ] && vm-box-cache-enabled; then
        box_file="$BOX_CACHE_DIR/$(vm-box-key "$vmname").box"
        if [ "$e2e_vm_cache" != "refresh" ] && vm-cached-box-usable "$box_file"; then
            echo "using cached provisioned VM $box_file..."
            use_cached_box=1
            box_name="$(vm-box-name "$vmname")"
            distro_img="file://$box_file"
            # The box already has everything the playbook installs, and kubeadm
            # init cannot run a second time.
            no_provision="--no-provision"
            e2e_no_provision=1
        else
            package_box=1
        fi
    elif grep -q "^IMAGE_NAME = \"$BOX_NAME_PREFIX/" "$vagrantdir/Vagrantfile" 2>/dev/null; then
        # The VM of this output directory was created from a packaged box, so its
        # disk is provisioned however the run which created it ended: a box only
        # exists because provisioning succeeded before it was packaged. Skip
        # provisioning here too, vagrant would otherwise run it either on a VM
        # which is already up, or on one which it is re-importing from that box.
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
                -e "s:CACHE_DIR=:CACHE_DIR=\"$CACHE_DIR\":g" \
                -e "s:E2E_NO_PROVISION=:E2E_NO_PROVISION=$e2e_no_provision:g" \
		"$files/env.in" > "$vagrantdir/env"
	else
	    sed -e "s/DNS_NAMESERVER=\"\"/DNS_NAMESERVER=\"$dns_nameserver\"/g" \
		-e "s/DNS_SEARCH_DOMAIN=\"\"/DNS_SEARCH_DOMAIN=\"$dns_search_domain\"/g" \
		-e "s/SSH_PORT=/SSH_PORT=$SSH_PORT/g" \
                -e "s:CACHE_DIR=:CACHE_DIR=\"$CACHE_DIR\":g" \
                -e "s:E2E_NO_PROVISION=:E2E_NO_PROVISION=$e2e_no_provision:g" \
		"$files/env.in" > "$vagrantdir/env"
	fi
    fi

    # An env file written by an earlier run carries the flag of that run, which
    # may not be what this one does about provisioning. Keep it in sync, both
    # ways: a vagrant up which is not ours, from make up or make ssh, should
    # leave a provisioned VM alone, and a run which asks for provisioning needs
    # the provisioner in the Vagrantfile.
    if [ -f "$vagrantdir/env" ] &&
           ! grep -q "^E2E_NO_PROVISION=$e2e_no_provision\$" "$vagrantdir/env"; then
        sed -i "s/^E2E_NO_PROVISION=.*\$/E2E_NO_PROVISION=$e2e_no_provision/" \
            "$vagrantdir/env"
        grep -q "^E2E_NO_PROVISION=" "$vagrantdir/env" ||
            echo "E2E_NO_PROVISION=$e2e_no_provision" >> "$vagrantdir/env"
    fi

    (cd "$vagrantdir";
     export ANSIBLE_PIPELINING=True;
     # Make sure the vagrant plugins are installed
     make install || error "failed to check/install vagrant plugins"

     if [ ! -d .vagrant ]; then
	 vagrant init ${vagrant_debug:+--debug} --template Vagrantfile $box_name || \
             error "failed to vagrant init $box_name"
     fi

     # The playbook ends in kubeadm init, which cannot run on a VM which already
     # has a cluster. A VM which is about to be provisioned and which has been
     # here before may well have one: an earlier run may have been interrupted
     # after kubeadm init and before the end of the playbook, or this run may be
     # provisioning a working VM again on purpose. Either way, reset it first.
     if [ -z "$no_provision" ]; then
         vm-kubeadm-reset "$vagrantdir"
     fi

     # If you want to force provisioning of already provisioned vm,
     # then you can set provision=1 when calling e2e test script.
     # This can be used if the provisioning failed before kubernetes
     # was setup, or to reinstall the cluster of a VM from scratch.
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

    mkdir -p "$COMMAND_OUTPUT_DIR"
    rm -f "$COMMAND_OUTPUT_DIR"/0*

    # Record that this VM is provisioned, now that it is up and everything which
    # provisions it has run. A VM which came from a box was provisioned before it
    # was packaged, so look for the mark the playbook left inside it rather than
    # take the box at its word.
    #
    # This needs vm-command, so it cannot happen before the ssh config above is
    # in place. It does come before waiting for the cluster below: a VM which is
    # not provisioned has no cluster to wait for, and saying that outright beats
    # timing out on a node which is never going to be ready.
    if ! vm-provisioned "$vagrantdir"; then
        vm-verify-provisioned "$vmname"
        vm-mark-provisioned "$vagrantdir" "$vmname" \
            "$(sed -n "s/^IMAGE_NAME = \"\($BOX_NAME_PREFIX\/.*\)\"\$/\1/p" \
                   "$vagrantdir/Vagrantfile" 2>/dev/null)"
    fi

    # A VM which has just booted from a box, or which was brought back up after
    # being packaged, has a cluster which is still starting up. Wait for it
    # before letting the tests run.
    if [ -n "$use_cached_box" ] || [ -n "$package_box" ]; then
        wait-for-node-ready
        wait-for-dns-ready
    fi
}

vm-play() {
    local vm="$1"
    local playbook="$2"
    local vagrantdir="$3"

    (cd "$vagrantdir";
     export ANSIBLE_PIPELINING=True;
     # private_key may be under qemu/ or libvirt/ directory
     private_key=$(echo .vagrant/machines/${vm}/*/private_key)
     # ansible synchronize does not respect --ssh-common-args,
     # therefore pass the same in the environment variable, too
     ANSIBLE_SSH_ARGS="-F $vagrantdir/.ssh-config" ansible-playbook "$playbook" \
	  -i "${vm}," -u vagrant \
	  --private-key="$private_key" \
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

    vm-command "ls *$image_type*-image-*.tar"
    tarball="$COMMAND_OUTPUT"

    if [ "$(echo $tarball | wc -w)" != 1 ]; then
        command-error "failed to reimport image, one image expected (found: $tarball)"
    fi

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
