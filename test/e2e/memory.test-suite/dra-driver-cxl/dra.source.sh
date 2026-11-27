# Helpers for testing CXL memory pooling and sharing with Dynamic Resource
# Allocation (DRA): nri-dra-driver-cxl (the CXL DRA driver, drivers
# cxl.nri.io and cxl-pool.nri.io) runs on every node of the cluster of the
# VM of the test as a DaemonSet that helm installs from
# deployment/helm/dra-driver-cxl, fake-cxl-pool-controller (the pool
# controller of fake-cxl-pool, see scripts/testing/fake-cxl-pool) runs in
# the VM of the test as a transient systemd unit, one per pool server
# (dra-controller-start; a test may start them otherwise),
# fake-cxl-pool-server runs on the host (pool.source.sh), once per pool.
#
# The driver comes from its image, built like the images of the other
# plugins:
#   make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images
# The e2e framework imports the newest image into the VM of the test
# (playbook/nri-dra-driver-cxl-plugin-deploy.yaml), dra-image-put into the
# other nodes of its cluster.
#
# The contract between the controller and the driver is
# docs/memory/cxl/pool.md, the lease protocol and cxl-request are
# docs/memory/cxl/leases.md.
#
# This file is sourced for every topology of dra-driver-cxl, so it only
# defines functions and variables. The helpers take VMDIR, the output
# directory of a VM, first, like those of pool.source.sh: $OUTPUT_DIR for
# the VM of the test. Other VMs work where pool.source.sh is sourced too
# (n4-cxl-shared-2: $POOL_VM1_DIR and $POOL_VM0_DIR, the workers of the
# cluster).
#
# Typical use:
#   pool-server-start "DEVICES"   # first: dra-install chains its EXIT trap
#   dra-build
#   dra-install
#   dra-trace-start "$VM"
#   dra-apply "$VM" <<EOF ... EOF
#   dra-wait "$VM" LABEL "resourceclaim -n $DRA_NS NAME" "condition('cxl-pool.nri.io/Attached') == 'True'"
#   dra-trace-stop; dra-trace-summary

# The driver in the cluster: its helm chart and release, the values that
# dra-install gives on top of helm-set-args, the directory of the image
# tarballs of "make images", the image of helm-set-args, and the
# DaemonSet, container and pod label of the chart.
DRA_CHART=dra-driver-cxl
DRA_HELM_RELEASE="test"
DRA_HELPER_DIR="$(dirname "${BASH_SOURCE[0]}")"
DRA_HELM_VALUES="$DRA_HELPER_DIR/dra-driver-cxl.helm-values.yaml"
DRA_IMAGE_DIR="$nri_resource_policy_src/build/images"
DRA_TEST_IMAGE=localhost/$DRA_CHART:testing
DRA_DAEMONSET=nri-dra-driver-cxl
DRA_CONTAINER=nri-dra-driver-cxl
DRA_POD_LABEL=app.kubernetes.io/name=nri-dra-driver-cxl
# The DeviceClasses that the chart creates.
DRA_CLASSES=(cxl-local cxl-local-shared cxl-local-persistent cxl-pool cxl-pool-shared cxl-pool-persistent cxl-region cxl-dram cxl-lease)
# The "shares" capacity of every shared device, local or pool
# (device.SharesUnlimited, printed 1M). A claim that requests no shares
# consumes all of them, the device exclusively; shares: "1" is one share.
DRA_SHARES=1000000
# The configuration file of the driver (YAML, .Values.config of the chart,
# docs/memory/cxl/usage.md), "" for none: dra-install gives it to the chart.
# Example: DRA_DRIVER_CONFIG="sharedMemdevs: [\"$DRA_LOCAL_SERIAL\"]"
DRA_DRIVER_CONFIG=""
# The EXIT trap that dra-install sets, "" for "dra-cleanup; pool-cleanup".
DRA_EXIT_TRAP=""
# The pool servers of pool-server-start (by --name) that dra-install runs
# no pool controller for: their devices are never published as pool
# devices (test21: a persistent device that the node takes as local).
DRA_UNCONTROLLED_POOLS=()
# Lending memory to running containers (docs/memory/cxl/leases.md): the CLI
# that the driver puts on the node and mounts into containers that claim
# the lease device "lease" (DeviceClass cxl-lease), there and in the
# containers, and the directory of the per-claim lease sockets on the node.
DRA_REQUEST_NODE_BIN=/var/lib/kubelet/plugins/cxl.nri.io/cxl-request
DRA_REQUEST_CTR_BIN=/usr/local/bin/cxl-request
# shellcheck disable=SC2034 # for the tests: the filesystems of fs claims without "path" in a container
DRA_FS_CTR_DIR=/run/cxl-fs
# shellcheck disable=SC2034 # for the tests: leases.companionImage of the chart, the companion pods of leases
DRA_COMPANION_IMAGE=registry.k8s.io/pause:3.10
DRA_LEASE_DIR=/var/lib/kubelet/plugins/cxl.nri.io/leases
# The filesystems of fs leases: the host directory of each lease claim
# (<claim uid>) under DRA_LEASE_FS_DIR, a read-only slave bind at
# DRA_LEASE_FS_CTR_DIR in the containers of the claim; the driver binds the
# filesystem of a lease at <claim uid>/<name>, /run/cxl/fs/<name> in them.
DRA_LEASE_FS_DIR=/var/lib/kubelet/plugins/cxl.nri.io/lease-fs
DRA_LEASE_FS_CTR_DIR=/run/cxl/fs
# The directory of the CDI specs (JSON) that the drivers write on the node.
DRA_CDI_DIR=/var/run/cdi
# The labels of the companion pods and claims of leases.
DRA_LEASE_LABEL=cxl.nri.io/lease
DRA_REQUESTER_LABEL=cxl.nri.io/requester-uid
DRA_NODE_LABEL=cxl.nri.io/node
DRA_REQUESTER_ANNOTATION=cxl.nri.io/requester
# The state document of the driver: the records of its claims, memdevs,
# regions and slices (of the node-local CXL memory devices that it publishes
# whole and partitions on demand, cxl.nri.io devices memdev-<serial>,
# DeviceClass cxl-local, docs/memory/cxl/local.md) and leases.
DRA_STATE=/var/lib/kubelet/plugins/cxl.nri.io/state.json
DRA_CONTROLLER_BIN="$nri_resource_policy_src/scripts/testing/fake-cxl-pool/bin/fake-cxl-pool-controller"
# The systemd units of the pool controllers in the VM of the test:
# fake-cxl-pool-controller for the first pool server, and
# fake-cxl-pool-controller-<name> for every other server of
# pool.source.sh; the units that dra-install started.
DRA_CONTROLLER_UNIT=fake-cxl-pool-controller
DRA_CONTROLLER_UNITS_GLOB="$DRA_CONTROLLER_UNIT*"
DRA_CONTROLLER_UNITS=()
declare -A DRA_CONTROLLER_URLS=()  # dra-install: unit -> the URL of its pool server, seen from the VMs
# The namespace of pool controllers that run as Deployments (the demo's
# setup, test98-demo-setup): dra-install removes them too.
DRA_CONTROLLER_NS=fake-cxl-pool
DRA_POOL_DRIVER=cxl-pool.nri.io    # pool devices, published by the controller
# The node mounts of the file systems of persistent pool devices (mode fs),
# one per device and access: <serial hex without 0x>-<direct|cached>.
DRA_POOL_MOUNT_DIR=/var/lib/kubelet/plugins/$DRA_POOL_DRIVER/mounts
DRA_LOCAL_DRIVER=cxl.nri.io        # node-local CXL and DRAM, published by the driver
# The node mounts of persistent local memdevs, named like those of pool
# devices.
DRA_LOCAL_MOUNT_DIR=/var/lib/kubelet/plugins/$DRA_LOCAL_DRIVER/mounts
# The opaque configuration of the claims and DeviceClasses of both drivers
# (docs/memory/cxl/usage.md): one kind with the sections device (how the
# kubelet plugin prepares a device), container (how the NRI plugin manages
# the containers that use it: steering is opt-in, policy none by default)
# and pool (pool.opaque.<controller key> for the pool controllers). The
# fake pool controller's key is fake-cxl-pool (slot, numaNode).
DRA_CONFIG_API=cxl.nri.io/v1alpha1
DRA_CONFIG_KIND=MemoryConfig
DRA_POOL_CONFIG_KEY=fake-cxl-pool
DRA_NS=cxl-e2e               # namespace of the test objects
DRA_INSTALLED_VMS=()               # dra-install: the VMDIRs of the nodes, the VM of the test first,
declare -A DRA_WORKERS=()          # and VMDIR -> 1 of the other nodes, the workers
DRA_DAX_TOOL="$DRA_HELPER_DIR/dax-rw.py"
DRA_NUMA_TOOL="$DRA_HELPER_DIR/numa-touch.py"
DRA_JSON=""                        # JSON of the last dra-json/dra-wait
DRA_LOCAL_NAME=""                  # dra-local-device: the device in fake-cxl-pool-server,
DRA_LOCAL_SERIAL=""                # its serial (0x<lower-case hex>),
DRA_LOCAL_HEX=""                   # the serial in environment variable names (upper-case hex),
DRA_LOCAL_SIZE=""                  # its size in bytes,
DRA_LOCAL_DEVICE=""                # its device in the cxl.nri.io ResourceSlice (memdev-<hex>),
DRA_LOCAL_BLOCK_SIZE=""            # the memory block size of the VM in bytes,
DRA_LOCAL_LD=""                    # dra-py expression of its ResourceSlice device,
DRA_LOCAL_REGIONS=""               # cxl-py expression of its regions in cxl-dump,
DRA_LOCAL_PUBLISHED=""             # its ResourceSlice device as JSON,
DRA_LOCAL_ATTACHMENT=""            # and the state of its attachment to the VM
DRA_STATE_JSON=""                  # dra-local-state-shared: the state document of the driver
DRA_LOCAL_SHARED_REGION=""         # dra-local-shared-wait: the region of the shared local device
DRA_LOCAL_SHARED_DAX=""            # its dax device (daxX.Y)
DRA_LOCAL_SHARED_MEMDEV=""         # and the memdev (memN)
DRA_DRIVER_POD=""                  # dra-driver-restart: the new pod of the driver
DRA_CLAIM_JSON=""                  # dra-local-claim-check: the claim
DRA_CLAIM_UID=""                   # and its uid
DRA_POD_UID=""                     # dra-lease-pod-start: the pod
DRA_REQ_STATUS=""                  # dra-request: the exit status of cxl-request,
DRA_REQ_OUT=""                     # what it printed (stdout, then stderr),
DRA_REQ_JSON=""                    # and its last JSON line ("{}" if none)
DRA_LEASE_IDS=()                   # dra-lease-new: the lease ids of the test
DRA_LEASE_CLAIM_UID=""             # and the uid of its lease claim
DRA_NUMA_NODE_KB=""                     # dra-numa-node-wait: the last MemTotal of the node in kB
DRA_TOOLS_IMAGE=docker.io/library/python:3-alpine   # the image of pods that run the tools of dra-tools-install
DRA_MEMDEV=""                      # JSON of the last dra-memdev-sysfs/dra-memdev-wait
DRA_TOUCH_VM=""                    # dra-touch-start: the VM and POD/CONTAINER of numa-touch.py,
DRA_TOUCH_TARGET=""
DRA_TOUCH_EXEC=""                  # kubectl exec into that container,
DRA_TOUCH_PID=""                   # its pid in the container,
DRA_TOUCH_LOG=""                   # and its output when it had touched its memory
DRA_TOUCH_PAGES=""                 # dra-touch-pages: its pages on the node
DRA_MEM_HIGH=""                    # dra-memory-wait: memory.high of the container
DRA_MEMPOLICY=""                   # and the memory policy of its main process
# The main process of the containers of dra-pod-yaml: it runs /tmp/run
# (written by dra-run) when it appears, so that the workloads are its
# descendants and inherit the memory policy that the driver sets on the
# processes of a steered container. Processes of kubectl exec do not.
DRA_RUNNER='trap "exit 0" TERM
while :; do
    if [ -f /tmp/run ]; then
        mv /tmp/run /tmp/run.sh
        sh /tmp/run.sh > /tmp/run.out 2>&1
        echo $? > /tmp/run.rc.new
        mv /tmp/run.rc.new /tmp/run.rc
    fi
    sleep 0.2 & wait
done'
DRA_TRACE_DIR=""                   # $TEST_OUTPUT_DIR/trace of a running trace
DRA_TRACE_STAMP="$nri_resource_policy_src/test/e2e/memory.test-suite/dra-driver-cxl/dra-trace-stamp.py"
DRA_TRACE_RUNNING=""
DRA_TRACE_VMS=()
# Kill the trace collectors in a VM: each runs in a process group of its own.
DRA_TRACE_KILL='for f in /run/dra-trace/*.pid; do [ -f "$f" ] || continue; kill -TERM -- "-$(cat "$f")" 2>/dev/null; rm -f "$f"; done'

dra-vm-check() {
    # Usage: dra-vm-check
    #
    # Install the CXL kernel and tools in the VM of the test and check that
    # it can run the tests of the suite (test00-up of n4-cxl-shared-0, -1
    # and -2): the kernel with CONFIG_FS_DAX, CONFIG_EROFS_FS and DMI;
    # qemu answering on QMP, two host bridges with hdm_for_passthrough=on,
    # cxl-downstream ports that can hot-remove devices; the local device
    # cxl_memdev0 unplugged and eight free pool slots; no CXL memory device
    # in the VM.
    local kernel_tarball kernel_error vm_uuid product_uuid qemu_pid qmp_version response cxl_hw qtree
    vm-kernel-pkgs-install
    cxl-tools-install
    vm-command-q "command -v daxctl >/dev/null" ||
        vm-command "dnf install -y daxctl" ||
            command-error "cannot install daxctl in the VM"

    echo "### kernel of the VM"
    # The kernel comes from the cached kernel tarball of the kernel config, if
    # there is one. Point at it if the kernel is not the expected one.
    # shellcheck disable=SC2154 # kernel_getsource and kernel_config: the var files of the suite
    kernel_tarball=$(python3 -c '
import hashlib, re, sys
getsource, config_file, cache_dir = sys.argv[1:4]
# The name that test/e2e/playbook/custom-kernel.yaml gives the tarball.
sha = hashlib.sha256(open(config_file).read().rstrip().encode()).hexdigest()
print("%s/kernel.getsource_%s.config_%s.rpms.tar" % (cache_dir, re.sub("[^a-zA-Z0-9-]+", "_", getsource), sha))
' "$kernel_getsource" "$nri_resource_policy_src/test/e2e/playbook/files/$kernel_config" "$CACHE_DIR")
    kernel_error="the VM does not run the kernel of $kernel_tarball, the kernel of $kernel_config that provisioning installs (test/e2e/playbook/custom-kernel.yaml builds it when it is not cached). If the VM had installed older RPMs of the same name, recreate the VM, or remove ~vagrant/.vm-kernel-pkgs.installed_packages and the installed kernel RPM of the same version (sudo rpm -e kernel-<version>; reinstalling over it removes the kernel from /boot) in it and run this test again"
    vm-command "uname -r; grep -E '^CONFIG_(FS_DAX|EROFS_FS|DMIID)=' /boot/config-\$(uname -r)"
    # shellcheck disable=SC2016 # expanded in the VM
    vm-command-q 'grep -q ^CONFIG_FS_DAX=y /boot/config-$(uname -r)' ||
        error "kernel lacks CONFIG_FS_DAX=y: $kernel_error"
    # shellcheck disable=SC2016 # expanded in the VM
    vm-command-q 'grep -q ^CONFIG_EROFS_FS=y /boot/config-$(uname -r)' ||
        error "kernel lacks CONFIG_EROFS_FS=y: $kernel_error"
    vm_uuid=$(sed -n 's/^VM_UUID=//p' "$OUTPUT_DIR/env")
    product_uuid=$(vm-command-q "cat /sys/class/dmi/id/product_uuid")
    echo "VM_UUID: $vm_uuid, product_uuid in the VM: $product_uuid"
    [ -n "$vm_uuid" ] || error "no VM_UUID in $OUTPUT_DIR/env, VM created without -uuid?"
    [ "${product_uuid,,}" == "${vm_uuid,,}" ] ||
        error "product_uuid '$product_uuid' of the VM differs from VM_UUID '$vm_uuid' (kernel without CONFIG_DMIID?): $kernel_error"

    echo "### qemu of the VM"
    qemu_pid=$(vm-qemu-pid)
    [[ "$qemu_pid" =~ ^[0-9]+$ ]] || error "expected one qemu process of $OUTPUT_DIR, found: '$qemu_pid'"
    tr '\0' ' ' < "/proc/$qemu_pid/cmdline" > "$TEST_OUTPUT_DIR/qemu-cmdline.txt"
    echo "qemu pid:     $qemu_pid"
    echo "qemu binary:  $(readlink "/proc/$qemu_pid/exe")"
    echo "qemu cmdline: $TEST_OUTPUT_DIR/qemu-cmdline.txt"
    [ "$(grep -o 'pxb-cxl,[^ ]*,hdm_for_passthrough=on' "$TEST_OUTPUT_DIR/qemu-cmdline.txt" | wc -l)" = "2" ] ||
        error "expected two pxb-cxl host bridges with hdm_for_passthrough=on: recreate the VM from topology.var.json"
    qmp_version=$(vm-qmp query-version) || error "QMP query-version failed: $qmp_version"
    echo "qemu version: $qmp_version"
    python3 -c 'import json, sys; v = json.loads(sys.argv[1])["return"]; print("%(major)d.%(minor)d.%(micro)d" % v["qemu"], v["package"])' "$qmp_version" ||
        error "unexpected QMP query-version response: $qmp_version"
    # A pool detaches devices again: qemu must complete their hot-removal.
    response=$(vm-qmp qom-get '{"path": "/machine/peripheral/cxlsw_ds0_usrp0hb0", "property": "power_controller_present"}')
    [ "$response" == '{"return": true}' ] ||
        error "qemu cannot hot-remove CXL memory devices: its cxl-downstream ports have no power_controller_present ($response). Recreate the VM with qemu_bin=/path/to/qemu-system-x86_64 of a qemu that can (docs/memory/cxl/e2e.md, Prerequisites)"

    echo "### qemu CXL memory devices and slots"
    cxl_hw=$(show_sn=1 show_be=1 vm-cxl-hw)
    echo "$cxl_hw"
    grep -q "^cxl_memdev0 sn=0xc100e2e1 volatile-memdev=beram_cxl_memdev0__" <<< "$cxl_hw" ||
        error "local CXL memory device cxl_memdev0 not found"
    if grep -q plugged <<< "$cxl_hw"; then
        error "expected no CXL memory devices plugged in at boot"
    fi
    # Every switch port, 4 + 4, is free: neither pool slots nor declared but not
    # present devices have a cxl-type3 device at boot.
    qtree=$(vm-monitor "info qtree -b")
    [ "$(grep -c 'dev: cxl-downstream' <<< "$qtree")" = "8" ] ||
        error "expected 8 cxl-downstream ports in qemu"
    if grep -q 'dev: cxl-type3' <<< "$qtree"; then
        error "expected no cxl-type3 devices in qemu"
    fi

    echo "### CXL memory devices in the VM"
    cxl-dump no-devices
    cxl-assert 'memdevs == [] and regions == [] and endpoints == []'
    cxl-assert 'len(nodes) == 2'
}

###
### Commands in the VMs
###

dra-kubectl() {
    # Usage: dra-kubectl VMDIR ARGS...
    #
    # Run "kubectl ARGS" in the VM of VMDIR, in its cluster. COMMAND_OUTPUT
    # has the output. ARGS are joined with spaces, quote them for the shell
    # of the VM.
    local vmdir="$1"
    shift
    vmdir-command "$vmdir" "kubectl $*"
}

dra-apply() {
    # Usage: dra-apply VMDIR <<EOF
    #        YAML
    #        EOF
    #
    # Show the YAML from stdin and "kubectl apply" it in the cluster of the
    # VM of VMDIR, in namespace $DRA_NS unless the YAML names another one.
    # Fail the test if kubectl fails.
    local vmdir="$1" yaml
    yaml=$(cat)
    vmdir-command "$vmdir" "kubectl apply -n $DRA_NS -f - <<'DRA_APPLY_EOF'
$yaml
DRA_APPLY_EOF" || command-error "kubectl apply failed in $(vmdir-label "$vmdir")"
}

dra-delete() {
    # Usage: dra-delete VMDIR KIND NAME [KUBECTL-ARGS...]
    #
    # Delete object NAME of KIND in namespace $DRA_NS of the cluster of the
    # VM of VMDIR. Not found is fine. Fail the test if kubectl fails.
    # Example: dra-delete "$VM" pod pooled-pod --grace-period=2
    local vmdir="$1" kind="$2" name="$3"
    shift 3
    vmdir-command "$vmdir" "kubectl delete -n $DRA_NS $kind $name --ignore-not-found $*" ||
        command-error "cannot delete $kind $name in $(vmdir-label "$vmdir")"
}

dra-pull-images() {
    # Usage: dra-pull-images VMDIR IMAGE...
    #
    # Pull IMAGEs in the VM of VMDIR with crictl, so that pull times do not
    # dominate pod start times in the tests.
    local vmdir="$1" image
    shift
    for image in "$@"; do
        vmdir-command "$vmdir" "crictl pull $image 2>&1 | grep -v 'level=warning'; exit \${PIPESTATUS[0]}" ||
            command-error "cannot pull $image in $(vmdir-label "$vmdir")"
    done
}

dra-tools-install() {
    # Usage: dra-tools-install VMDIR [NAME=HOST-FILE...]
    #
    # Create ConfigMap tools in $DRA_NS of the cluster of the VM of VMDIR,
    # with the tools that pods mount at /tools and run with python3 (image
    # $DRA_TOOLS_IMAGE):
    #   dax-rw.py      write and read strings in a dax device, or
    #                  through an inherited fd (--fd N)
    #   numa-touch.py  allocate and touch memory that prefers one NUMA
    #                  node, or with the memory policy of the process,
    #                  report where the pages are
    # and the HOST-FILEs of a test as /tools/NAME (text files).
    local vmdir="$1" spec name file from=""
    shift
    vmdir-put-file "$vmdir" "$DRA_DAX_TOOL" /root/dax-rw.py
    vmdir-put-file "$vmdir" "$DRA_NUMA_TOOL" /root/numa-touch.py
    for spec in "$@"; do
        name=${spec%%=*} file=${spec#*=}
        [[ "$name" =~ ^[A-Za-z0-9._-]+$ ]] && [ -f "$file" ] ||
            error "dra-tools-install: '$spec' is not NAME=HOST-FILE of an existing file"
        vmdir-put-file "$vmdir" "$file" "/root/tools.d.$name"
        from+=" --from-file=$name=/root/tools.d.$name"
    done
    vmdir-command "$vmdir" "kubectl create configmap tools -n $DRA_NS --from-file=dax-rw.py=/root/dax-rw.py --from-file=numa-touch.py=/root/numa-touch.py$from --dry-run=client -o yaml | kubectl apply -f -" ||
        command-error "cannot create ConfigMap tools in $(vmdir-label "$vmdir")"
}

###
### Installing the DRA components
###

dra-build() {
    # Usage: dra-build
    #
    # Build fake-cxl-pool (server, client and controller) on the host, static
    # amd64 binaries. dra-install copies the controller to the VM of the
    # test. The driver is not built here: it
    # comes from its image, see the top of this file.
    host-command "make -C \"$nri_resource_policy_src/scripts/testing/fake-cxl-pool\"" ||
        command-error "cannot build fake-cxl-pool"
    [ -x "$DRA_CONTROLLER_BIN" ] ||
        error "dra-build: make did not build $DRA_CONTROLLER_BIN"
}

dra-image-put() {
    # Usage: dra-image-put VMDIR
    #
    # Import the newest image of the driver ($DRA_IMAGE_DIR) into the
    # container runtime of the VM of VMDIR as $DRA_TEST_IMAGE, the way the
    # e2e framework imports it into the VM of the test
    # (playbook/nri-dra-driver-cxl-plugin-deploy.yaml). Skip the copy if
    # $DRA_TEST_IMAGE is that image already.
    local vmdir="$1" label tar file id
    label=$(vmdir-label "$vmdir")
    # shellcheck disable=SC2012 # the newest by mtime, like the playbook
    tar=$(ls -1t "$DRA_IMAGE_DIR"/nri-dra-driver-cxl-image-*.tar 2>/dev/null | head -n 1)
    [ -n "$tar" ] ||
        error "dra-image-put: no image of the driver in $DRA_IMAGE_DIR, build it: make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images"
    file=${tar##*/}
    id=${file##*-image-}
    id=${id%.tar}
    if [ "${k8scri:-containerd}" == containerd ] &&
           vmdir-command-q "$vmdir" "ctr -n k8s.io images ls | awk -v ref=$DRA_TEST_IMAGE -v id=sha256:$id '\$1 == ref {t = \$3} index(\$1, id) == 1 {s = \$3} END {exit !(t != \"\" && t == s)}'"; then
        echo "dra-image-put $label: $DRA_TEST_IMAGE is $file already"
        return 0
    fi
    host-command "scp -F \"$vmdir/.ssh-config\" \"$tar\" node:\"$file\"" ||
        command-error "cannot copy $tar to $label"
    if [ "${k8scri:-containerd}" == containerd ]; then
        vmdir-command "$vmdir" "ctr -n k8s.io images import $file && ref=\$(ctr -n k8s.io images ls | grep sha256:$id | tr -s '\t' ' ' | cut -d ' ' -f1 | head -n 1) && [ -n \"\$ref\" ] && ctr -n k8s.io images tag --force \$ref $DRA_TEST_IMAGE; rc=\$?; rm -f $file; exit \$rc"
    else
        vmdir-command "$vmdir" "ref=\$(podman image load -i $file | sed -n 's/^Loaded image[^:]*: *//p' | tr ',' '\n' | head -n 1) && [ -n \"\$ref\" ] && podman image tag \$ref $DRA_TEST_IMAGE; rc=\$?; rm -f $file; exit \$rc"
    fi || command-error "cannot import $file as $DRA_TEST_IMAGE in $label"
}

dra-install() {
    # Usage: dra-install
    #
    # Install the driver with helm in the cluster of the VM of the test,
    # on every node of it (release $DRA_HELM_RELEASE in kube-system, with
    # the values of helm-set-args and $DRA_HELM_VALUES, and with the driver
    # configuration $DRA_DRIVER_CONFIG unless it is empty, see
    # dra-helm-values), and run fake-cxl-pool-controller in the VM of the
    # test as a transient systemd unit of the same name, with the admin
    # kubeconfig of the VM: one per pool server that pool-server-start
    # started, unit fake-cxl-pool-controller for the first one and
    # fake-cxl-pool-controller-<name> for the others (but those of
    # DRA_UNCONTROLLED_POOLS), each with the URL of its server; the
    # controller names its DRA pool after its server. The chart creates
    # the DeviceClasses of the driver (DRA_CLASSES), and the driver puts
    # cxl-request on its nodes ($DRA_REQUEST_NODE_BIN) for the
    # containers of lease claims. The other nodes get the image of the
    # driver first (dra-image-put). Remove what an earlier run left first:
    # the release, the controller, DeviceClasses, and driver state that
    # refers to CXL regions which are gone (dra-state-cleanup). Create
    # namespace $DRA_NS empty (delete objects that an earlier run left).
    # Wait until the cluster has the pool ResourceSlices of the controllers
    # and the driver is up on every node (dra-driver-wait).
    #
    # dra-controller-start starts each controller; a test that runs them
    # otherwise redefines it (and dra-controllers-status-cmd and
    # dra-controllers-log-cmd). The controllers of an earlier run are
    # removed whatever ran them (dra-controllers-remove).
    #
    # Call pool-server-start first: the controller needs the server, and
    # dra-install chains its EXIT trap: dra-cleanup, then pool-cleanup, or
    # $DRA_EXIT_TRAP if it is set.
    local vmdir="$OUTPUT_DIR" label url names name v nodes values i units=() urls=()
    label=$(vmdir-label "$vmdir")
    [ -x "$DRA_CONTROLLER_BIN" ] || error "dra-install: no $DRA_CONTROLLER_BIN, run dra-build first"
    values=$(dra-helm-values) || error "dra-install: no helm values file, see above"
    url="${POOL_GUEST_URL:-http://192.168.76.2:${FAKE_CXL_POOL_PORT:-9909}}"
    # One controller per pool server, the first one with the URL above.
    units=("$DRA_CONTROLLER_UNIT")
    urls=("$url")
    for name in "${POOL_SERVERS[@]:1}"; do
        ! dra-pool-uncontrolled "$name" || continue
        v=$(pool-guest-url "$name") || error "dra-install: no URL of the pool server $name"
        units+=("$DRA_CONTROLLER_UNIT-$name")
        urls+=("$v")
    done
    # The nodes of the cluster are VMs next to the VM of the test, named
    # after their output directories, the VM of the test first.
    names=$(vmdir-command-q "$vmdir" "kubectl get nodes -o jsonpath='{.items[*].metadata.name}'") ||
        error "dra-install: cannot list the nodes of the cluster of $label"
    nodes=("$vmdir")
    for name in $names; do
        [ "$name" != "$(vmdir-name "$vmdir")" ] || continue
        v=$(vmdir-of "$name")
        [ -f "$v/.ssh-config" ] || error "dra-install: node $name of the cluster of $label is not a VM in $v"
        nodes+=("$v")
    done
    echo "dra-install: the cluster of $label has ${#nodes[@]} nodes: $names"
    for v in "${nodes[@]:1}"; do
        dra-image-put "$v"
    done

    echo "dra-install: removing components of an earlier run, if any"
    dra-helm-uninstall ||
        error "dra-install: pods of $DRA_DAEMONSET of an earlier run are left in the cluster of $label"
    dra-controllers-remove ||
        command-error "cannot remove the pool controllers of an earlier run in $label"
    vmdir-command "$vmdir" "kubectl delete deviceclass --ignore-not-found ${DRA_CLASSES[*]}" ||
        command-error "cannot remove the components of an earlier run in $label"
    for v in "${nodes[@]}"; do
        dra-state-cleanup "$v"
    done
    vmdir-put-file "$vmdir" "$DRA_CONTROLLER_BIN" /usr/local/bin/fake-cxl-pool-controller
    vmdir-command "$vmdir" "chmod 755 /usr/local/bin/fake-cxl-pool-controller" ||
        command-error "cannot install fake-cxl-pool-controller in $label"

    echo "dra-install: installing $DRA_CHART with helm on ${#nodes[@]} nodes, starting ${units[*]}"
    DRA_INSTALLED_VMS=("${nodes[@]}")
    DRA_WORKERS=()
    for v in "${nodes[@]:1}"; do
        DRA_WORKERS["$v"]=1
    done
    if [ -n "$DRA_EXIT_TRAP" ]; then
        # shellcheck disable=SC2064 # the value is the trap command itself
        trap "$DRA_EXIT_TRAP" EXIT
    elif type -t pool-cleanup >/dev/null; then
        trap 'dra-cleanup; pool-cleanup' EXIT
    else
        trap dra-cleanup EXIT
    fi
    # helm-launch also collects the log of the driver into the test output
    # (nri-resource-policy.output.txt).
    helm_config="$values" helm_name="$DRA_HELM_RELEASE" launch_available="${#nodes[@]}" launch_timeout=120s \
        helm-launch "$DRA_CHART"
    DRA_CONTROLLER_UNITS=()
    DRA_CONTROLLER_URLS=()
    for i in "${!units[@]}"; do
        DRA_CONTROLLER_URLS["${units[$i]}"]="${urls[$i]}"
        dra-controller-start "${units[$i]}"
        DRA_CONTROLLER_UNITS+=("${units[$i]}")
    done
    vmdir-command "$vmdir" "kubectl get deviceclass ${DRA_CLASSES[*]}" ||
        command-error "the chart $DRA_CHART does not create the DeviceClasses ${DRA_CLASSES[*]} in $label"

    for v in "${nodes[@]}"; do
        dra-driver-wait "$v"
    done

    dra-ns-reset "$vmdir" create || command-error "cannot create namespace $DRA_NS in $label"
}

dra-controller-run() {
    # Usage: dra-controller-run UNIT
    #
    # Start fake-cxl-pool-controller as the transient systemd unit UNIT in
    # the VM of the test, with the URL of its pool server
    # (DRA_CONTROLLER_URLS), Restart=no and the admin kubeconfig of the VM:
    # the command line of dra-install. Return the status of systemd-run.
    local unit="$1" url="${DRA_CONTROLLER_URLS[$1]:-}"
    if [ -z "$url" ]; then
        echo "dra-controller-run: no pool server URL of $unit: dra-install starts the controllers" >&2
        return 2
    fi
    vmdir-command "$OUTPUT_DIR" "systemd-run --unit $unit --property=Restart=no -E KUBECONFIG=/root/.kube/config /usr/local/bin/fake-cxl-pool-controller -server $url -v"
}

dra-controller-start() {
    # Usage: dra-controller-start NAME
    #
    # Start the pool controller NAME of dra-install for its pool server
    # (DRA_CONTROLLER_URLS): the transient systemd unit NAME
    # (dra-controller-run). Fail the test if it does not start. A test that
    # runs the controllers otherwise redefines this function.
    dra-controller-run "$1" || command-error "cannot start $1 in $(vmdir-label "$OUTPUT_DIR")"
}

dra-controllers-status-cmd() {
    # Usage: dra-controllers-status-cmd
    #
    # Print a command for the VM of the test that shows the state and the
    # last log lines of the pool controllers (dra-driver-wait).
    echo "systemctl status --no-pager '$DRA_CONTROLLER_UNITS_GLOB'; journalctl --no-pager -n 40 -u '$DRA_CONTROLLER_UNITS_GLOB'"
}

dra-controllers-log-cmd() {
    # Usage: dra-controllers-log-cmd
    #
    # Print a command for the VM of the test that follows the log of every
    # pool controller from now on (dra-trace-start). journalctl -f resolves
    # a unit pattern only once, to the units that have logged already: it
    # names the units that dra-install started.
    local name args=""
    for name in "${DRA_CONTROLLER_UNITS[@]}"; do
        args+=" -u $name"
    done
    [ -n "$args" ] || args=" -u '$DRA_CONTROLLER_UNITS_GLOB'"
    echo "journalctl -f -n0 -o cat$args"
}

dra-controllers-remove() {
    # Usage: dra-controllers-remove
    #
    # Stop and remove every pool controller in the cluster of the VM of the
    # test, however it ran: the systemd units $DRA_CONTROLLER_UNITS_GLOB
    # (stop, reset-failed), namespace $DRA_CONTROLLER_NS with the
    # Deployments of the demo, and the ClusterRole and ClusterRoleBinding
    # of their manifest. A controller deletes the ResourceSlices of its
    # pool when it stops. Return the status of the commands in the VM.
    vmdir-command "$OUTPUT_DIR" "systemctl stop '$DRA_CONTROLLER_UNITS_GLOB' 2>/dev/null; systemctl reset-failed '$DRA_CONTROLLER_UNITS_GLOB' 2>/dev/null
if kubectl get namespace $DRA_CONTROLLER_NS >/dev/null 2>&1; then kubectl delete namespace $DRA_CONTROLLER_NS --wait=true --timeout=90s || exit 1; fi
kubectl delete clusterrolebinding,clusterrole $DRA_CONTROLLER_UNIT --ignore-not-found"
}

dra-helm-values() {
    # Usage: dra-helm-values
    #
    # Print the path of the helm values file of dra-install: $DRA_HELM_VALUES
    # when DRA_DRIVER_CONFIG is empty, else
    # $TEST_OUTPUT_DIR/dra-driver-cxl.helm-values.yaml, written now: the
    # values of $DRA_HELM_VALUES and "config:" with DRA_DRIVER_CONFIG, the
    # configuration file of the driver. Show the file. Fail the test if
    # $DRA_HELM_VALUES is missing or has a config already.
    local file="$TEST_OUTPUT_DIR/dra-driver-cxl.helm-values.yaml"
    [ -f "$DRA_HELM_VALUES" ] || error "dra-install: no helm values $DRA_HELM_VALUES"
    if [ -z "$DRA_DRIVER_CONFIG" ]; then
        echo "$DRA_HELM_VALUES"
        return 0
    fi
    ! grep -q '^config:' "$DRA_HELM_VALUES" ||
        error "dra-install: $DRA_HELM_VALUES has a config, DRA_DRIVER_CONFIG cannot be added to it"
    # shellcheck disable=SC2001 # sed indents every line of the configuration
    { cat "$DRA_HELM_VALUES"
      echo ""
      echo "# DRA_DRIVER_CONFIG of $(basename "${TEST_DIR:-test}"), written by dra-install"
      echo "config:"
      sed 's/^/  /' <<< "$DRA_DRIVER_CONFIG"
    } > "$file" || error "dra-install: cannot write $file"
    echo "dra-install: helm values with the driver configuration, $file:" >&2
    cat "$file" >&2
    echo "$file"
}

dra-helm-uninstall() {
    # Usage: dra-helm-uninstall
    #
    # Uninstall release $DRA_HELM_RELEASE from the cluster of the VM of the
    # test, if it has one, and wait until no pod of the driver is left in
    # the cluster. Return non-zero if some are left.
    helm_name="$DRA_HELM_RELEASE" helm-terminate
    retry-until --timeout 60 --interval 1 --message "no pods of $DRA_DAEMONSET in the cluster" \
        '[ -z "$(vm-command-q "kubectl get pods -n kube-system -l $DRA_POD_LABEL -o name" 2>/dev/null)" ]'
}

dra-state-cleanup() {
    # Usage: dra-state-cleanup VMDIR
    #
    # Remove the state of the driver in the VM of VMDIR that refers to CXL
    # regions, if the VM has no regions: then it is a leftover of an earlier
    # VM boot. That is the state document of the driver (state.json,
    # state.json.prev, which also hold the serials of the pool devices that
    # the driver saw, so that every test starts with the pools of its own
    # servers only) and the lease sockets of lease claims (leases/).
    vmdir-command "$1" "if ls /sys/bus/cxl/devices/ | grep -q '^region[0-9]'; then echo 'CXL regions exist, keeping the CXL state of the driver'; else rm -fv /var/lib/kubelet/plugins/$DRA_LOCAL_DRIVER/state.json /var/lib/kubelet/plugins/$DRA_LOCAL_DRIVER/state.json.prev; rm -rfv /var/lib/kubelet/plugins/$DRA_LOCAL_DRIVER/leases; fi"
}

dra-driver-wait() {
    # Usage: dra-driver-wait VMDIR
    #
    # Wait until the cluster of the VM of VMDIR has a pool ResourceSlice of
    # every started pool server (dra-pool-names) and the cxl.nri.io
    # ResourceSlice of the node, and the node has the kubelet plugin socket
    # of cxl-pool.nri.io and the cxl-request of the driver.
    local vmdir="$1" name label pools controllers
    name=$(vmdir-name "$vmdir")
    label=$(vmdir-label "$vmdir")
    pools=$(dra-pool-names | tr '\n' ' ')
    controllers=$(dra-controllers-status-cmd)
    retry-until --timeout 60 --interval 2 --message "$label: ResourceSlices of $DRA_POOL_DRIVER${pools:+ (pools ${pools% })} and $DRA_LOCAL_DRIVER on $name, $DRA_POOL_DRIVER kubelet plugin socket, $DRA_REQUEST_NODE_BIN" \
        'dra-install-ready "$vmdir" "$name"' || {
        vmdir-command "$vmdir" "kubectl get pods -n kube-system -l $DRA_POD_LABEL -o wide; kubectl logs -n kube-system --tail=40 -c $DRA_CONTAINER \$(kubectl get pods -n kube-system -l $DRA_POD_LABEL --field-selector spec.nodeName=$name -o name | head -n 1); $controllers; kubectl get resourceslices; ls -l /var/lib/kubelet/plugins/*"
        error "dra-install: the DRA components did not come up in $label"
    }
    vmdir-command "$vmdir" "kubectl get resourceslices; ls -l /var/lib/kubelet/plugins/$DRA_POOL_DRIVER/ $DRA_REQUEST_NODE_BIN"
}

dra-install-ready() {
    # Usage: dra-install-ready VMDIR NODENAME
    local vmdir="$1" name="$2" json pools="" pool
    json=$(vmdir-command-q "$vmdir" "kubectl get resourceslices -o json") || return 1
    for pool in $(dra-pool-names); do
        pools+="'$pool', "
    done
    dra-py assert "$json" "slices(\"$DRA_POOL_DRIVER\") and set([$pools]) <= set(s['spec']['pool']['name'] for s in slices(\"$DRA_POOL_DRIVER\")) and slices(\"$DRA_LOCAL_DRIVER\", \"$name\")" || return 1
    vmdir-command-q "$vmdir" "test -S /var/lib/kubelet/plugins/$DRA_POOL_DRIVER/dra.sock && test -x $DRA_REQUEST_NODE_BIN"
}

dra-driver-pod() {
    # Usage: dra-driver-pod VMDIR
    #
    # Print the name of the running pod of the driver on the node of the VM
    # of VMDIR, quietly. Return 1 if there is none.
    local pod
    pod=$(vmdir-command-q "$1" "kubectl get pods -n kube-system -l $DRA_POD_LABEL --field-selector spec.nodeName=$(vmdir-name "$1"),status.phase=Running -o name" 2>/dev/null | head -n 1)
    [ -n "$pod" ] || return 1
    echo "${pod#pod/}"
}

dra-driver-log-save() {
    # Usage: dra-driver-log-save VMDIR
    #
    # Store the log of the driver on the node of the VM of VMDIR as
    # $TEST_OUTPUT_DIR/nri-dra-driver-cxl.<vm>.log, if the driver runs there.
    local pod file
    pod=$(dra-driver-pod "$1") || return 0
    file="$TEST_OUTPUT_DIR/$DRA_CONTAINER.$(vmdir-label "$1").log"
    vmdir-command-q "$1" "kubectl logs -n kube-system $pod -c $DRA_CONTAINER" > "$file" 2>&1
    echo "log of $DRA_CONTAINER in $(vmdir-label "$1"): $file"
}

dra-pool-names() {
    # Usage: dra-pool-names
    #
    # Print the DRA pool names of the pool servers that pool-server-start
    # started and that have a pool controller (not in
    # DRA_UNCONTROLLED_POOLS), one per line: their names. Print nothing if
    # there are none.
    local name
    for name in "${POOL_SERVERS[@]}"; do
        dra-pool-uncontrolled "$name" || echo "$name"
    done
}

dra-pool-uncontrolled() {
    # Usage: dra-pool-uncontrolled NAME
    #
    # Return 0 if the pool server NAME is in DRA_UNCONTROLLED_POOLS.
    local n
    for n in "${DRA_UNCONTROLLED_POOLS[@]}"; do
        [ "$n" != "$1" ] || return 0
    done
    return 1
}

dra-pool-urls() {
    # Usage: dra-pool-urls
    #
    # Print the URLs on the host of the pool servers that pool-server-start
    # started, one per line, or $POOL_URL if it started none. Print nothing
    # if there is no server.
    local name
    if [ "${#POOL_SERVERS[@]}" -gt 0 ]; then
        for name in "${POOL_SERVERS[@]}"; do
            pool-url "$name"
        done
    elif [ -n "$POOL_URL" ]; then
        echo "$POOL_URL"
    fi
}

dra-pool-attachments() {
    # Usage: dra-pool-attachments HOST [URL]
    #
    # Print the attachments on HOST of the server at URL (default:
    # $POOL_URL) that a pool controller owns (owner k8s:...), one
    # "device@host owner state" per line. Print nothing if there is no
    # server.
    local url="${2:-$POOL_URL}"
    [ -n "$url" ] || return 0
    curl -s --max-time 10 "$url/api/v1/attachments" 2>/dev/null | python3 -c '
import json, sys
try:
    atts = json.load(sys.stdin) or []
except ValueError:
    sys.exit(0)
for a in atts:
    if a.get("host") == sys.argv[1] and (a.get("owner") or "").startswith("k8s:"):
        print(a["device"] + "@" + a["host"], a.get("owner"), a.get("state"))' "$1"
}

dra-ns-reset() {
    # Usage: dra-ns-reset VMDIR [create]
    #
    # Delete the pods of namespace $DRA_NS, then the namespace, in the
    # cluster of the VM of VMDIR; with create, create the namespace again.
    # Return the status of the commands.
    vmdir-command "$1" "kubectl delete pods --all -n $DRA_NS --grace-period=2 --ignore-not-found --wait=true --timeout=60s 2>&1 | grep -v 'No resources found'; kubectl delete namespace $DRA_NS --ignore-not-found --wait=true --timeout=120s${2:+ && kubectl create namespace $DRA_NS}"
}

dra-uninstall() {
    # Usage: dra-uninstall
    #
    # Undo dra-install: delete namespace $DRA_NS (its pods first,
    # gracefully: the driver unprepares their claims, and the controllers
    # detach the devices when the claims are gone), wait until the
    # controllers have detached the devices of every node on every pool
    # server, store the log of the driver of every node
    # (dra-driver-log-save), uninstall the driver and its DeviceClasses
    # (dra-helm-uninstall), stop the controllers of either mode
    # (dra-controllers-remove), delete the pool ResourceSlices they left
    # and remove the driver state of every node (dra-state-cleanup). Never
    # fails the test: it runs in the EXIT trap.
    local vmdir="$OUTPUT_DIR" label left v host url urls
    [ "${#DRA_INSTALLED_VMS[@]}" -gt 0 ] || return 0
    label=$(vmdir-label "$vmdir")
    echo "dra-uninstall: the cluster of $label"
    dra-ns-reset "$vmdir"
    retry-until --timeout 60 --interval 2 --message "no ResourceClaims in $DRA_NS" \
        '[ -z "$(vmdir-command-q "$vmdir" "kubectl get resourceclaims -n $DRA_NS -o name 2>/dev/null")" ]' ||
        echo "dra-uninstall: ResourceClaims remain in $DRA_NS"
    urls=$(dra-pool-urls)
    for v in "${DRA_INSTALLED_VMS[@]}"; do
        host=$(vmdir-name "$v")
        for url in $urls; do
            if [ -n "$(dra-pool-attachments "$host" "$url")" ]; then
                retry-until --timeout 60 --interval 2 --message "the controller of $url detaches the pool devices of $host" \
                    '[ -z "$(dra-pool-attachments "$host" "$url")" ]' || {
                    echo "dra-uninstall: attachments left on $url, pool-cleanup releases and detaches them:"
                    dra-pool-attachments "$host" "$url"
                }
            fi
        done
    done
    for v in "${DRA_INSTALLED_VMS[@]}"; do
        dra-driver-log-save "$v"
    done
    dra-helm-uninstall ||
        echo "dra-uninstall: pods of $DRA_DAEMONSET are left"
    dra-controllers-remove ||
        echo "dra-uninstall: cannot remove the pool controllers"
    DRA_CONTROLLER_UNITS=()
    left=$(vmdir-command-q "$vmdir" "kubectl get resourceslices -o jsonpath='{range .items[?(@.spec.driver==\"$DRA_POOL_DRIVER\")]}{.metadata.name}{\"\\n\"}{end}'")
    if [ -n "$left" ]; then
        vmdir-command "$vmdir" "kubectl delete resourceslices $(tr "\n" " " <<< "$left")"
    fi
    for v in "${DRA_INSTALLED_VMS[@]}"; do
        dra-state-cleanup "$v"
    done
    DRA_INSTALLED_VMS=()
    DRA_WORKERS=()
    return 0
}

dra-assert-clean() {
    # Usage: dra-assert-clean VMDIR...
    #
    # Wait until a test has left nothing behind, 60 s at most, and fail
    # the test if it has: no pods, ResourceClaims or ResourceClaimTemplates
    # in $DRA_NS, and in the VM of every VMDIR no attachment of a pool
    # server that a claim owns, no CDI device of the driver, no mount under
    # the mount and lease directories of the driver, no loop device over
    # pmem and no CXL region. A test calls it at its end, before the
    # cleanup of the EXIT trap.
    local vmdir label left
    # shellcheck disable=SC2016 # evaluated by retry-until
    retry-until --timeout 60 --interval 2 --message "nothing left in $DRA_NS" \
        'left=$(vmdir-command-q "$OUTPUT_DIR" "kubectl get pods,resourceclaims,resourceclaimtemplates -n $DRA_NS -o name") && [ -z "$left" ]' ||
        error "objects left in $DRA_NS: $(tr '\n' ' ' <<< "$left")"
    for vmdir in "$@"; do
        label=$(vmdir-label "$vmdir")
        # shellcheck disable=SC2016 # evaluated by retry-until
        retry-until --timeout 60 --interval 2 --message "$label: nothing of a claim left" \
            'left=$(dra-leftovers "$vmdir") && [ -z "$left" ]' ||
            error "$label: left behind: $left"
    done
    echo "dra-assert-clean: nothing left in $DRA_NS and in $(for vmdir in "$@"; do vmdir-label "$vmdir"; done | tr '\n' ' ')"
}

dra-leftovers() {
    # Usage: dra-leftovers VMDIR
    #
    # Print what claims left in the VM of VMDIR (see dra-assert-clean), one
    # item per line, nothing if it is clean. Return non-zero if something
    # cannot be read.
    local vmdir="$1" host url atts cdis dir mounts loops regions
    host=$(vmdir-name "$vmdir")
    for url in $(dra-pool-urls); do
        atts=$(dra-pool-attachments "$host" "$url")
        [ -z "$atts" ] || echo "attachments on $url: $(tr '\n' ' ' <<< "$atts")"
    done
    cdis=$(dra-cdi-devices-with "$vmdir" "CXL_") || return 1
    [ -z "$cdis" ] || echo "CDI devices: $(tr '\n' ' ' <<< "$cdis")"
    for dir in "$DRA_LOCAL_MOUNT_DIR" "$DRA_POOL_MOUNT_DIR" "$DRA_LEASE_FS_DIR"; do
        dra-fs-mounts "$vmdir" "$dir" >/dev/null || return 1
        mounts=$COMMAND_OUTPUT
        [ "$mounts" == "[]" ] || echo "mounts under $dir: $mounts"
    done
    loops=$(dra-pmem-loops "$vmdir") || return 1
    [ -z "$loops" ] || echo "loop devices over pmem: $(tr '\n' ' ' <<< "$loops")"
    regions=$(vmdir-command-q "$vmdir" "ls /sys/bus/cxl/devices/ | grep -E '^region[0-9]' || true") || return 1
    [ -z "$regions" ] || echo "CXL regions: $(tr '\n' ' ' <<< "$regions")"
}

dra-cleanup() {
    # Usage: dra-cleanup
    #
    # The EXIT trap of dra-install (before pool-cleanup): dra-uninstall,
    # then stop the trace if it runs. Report CXL memory devices that are
    # still in the nodes: pool-cleanup, which runs next, releases and
    # detaches them.
    local vmdir vms memdevs
    vms=("${DRA_INSTALLED_VMS[@]}")
    dra-uninstall
    dra-trace-stop
    for vmdir in "${vms[@]}"; do
        memdevs=$(vmdir-command-q "$vmdir" "ls /sys/bus/cxl/devices/ | grep -E '^mem[0-9]'")
        if [ -n "$memdevs" ]; then
            echo "dra-cleanup: $(vmdir-label "$vmdir") still has CXL memory devices: $(tr "\n" " " <<< "$memdevs")"
        else
            echo "dra-cleanup: $(vmdir-label "$vmdir") has no CXL memory devices"
        fi
    done
    return 0
}

###
### Node-local CXL memory devices
###

dra-local-find() {
    # Usage: dra-local-find [--pool NAME] VMDIR
    #
    # Set DRA_LOCAL_NAME, DRA_LOCAL_SERIAL, DRA_LOCAL_HEX, DRA_LOCAL_SIZE,
    # DRA_LOCAL_DEVICE and DRA_LOCAL_ATTACHMENT (attached, detached, or
    # another attachment state) of the local CXL memory device of the VM of
    # VMDIR in fake-cxl-pool-server NAME (default: the first started
    # server; a server knows the local devices of the VMs it is cabled to):
    # the device of scope local that is bound to this VM only and is not
    # shared (a memory-backend-ram device of the topology). Fail the test
    # unless there is exactly one.
    local server=() host found
    if [ "$1" == "--pool" ]; then
        server=(--pool "$2")
        shift 2
    fi
    type -t pool-host-client >/dev/null ||
        error "${FUNCNAME[1]}: needs pool.source.sh and fake-cxl-pool-server (pool-server-start)"
    host=$(vmdir-name "$1")
    pool-host-client "${server[@]}" -o json devices || command-error "cannot list the devices of fake-cxl-pool-server${server[1]:+ ${server[1]}}"
    found=$(pool-value "$COMMAND_OUTPUT" "chr(10).join(' '.join([d['name'], d['serial'], str(d['size']), next((a['state'] for a in d.get('attachments') or [] if a['host'] == '$host'), 'detached')]) for d in j if d.get('scope') == 'local' and not d.get('shared') and d.get('localHost') == '$host')")
    [ -n "$found" ] && [ "$(wc -l <<< "$found")" == 1 ] ||
        error "${FUNCNAME[1]}: expected one local, not shared device of $host in fake-cxl-pool-server, found: '$found'"
    read -r DRA_LOCAL_NAME DRA_LOCAL_SERIAL DRA_LOCAL_SIZE DRA_LOCAL_ATTACHMENT <<< "$found"
    DRA_LOCAL_HEX=${DRA_LOCAL_SERIAL#0x}
    DRA_LOCAL_DEVICE="memdev-${DRA_LOCAL_HEX,,}"
    DRA_LOCAL_HEX=${DRA_LOCAL_HEX^^}
}

dra-local-device() {
    # Usage: dra-local-device [--pool NAME] VMDIR
    #
    # Attach the local CXL memory device of the VM of VMDIR (see
    # dra-local-find) through fake-cxl-pool-server NAME (default: the first
    # started server), unless it is attached, and wait until cxl-dump sees
    # it in the VM. If dra-install runs in the
    # VM, wait also until the node's cxl.nri.io ResourceSlice has its
    # device. Print and set DRA_LOCAL_NAME (the device in the server),
    # DRA_LOCAL_SERIAL (0x<lower-case hex>), DRA_LOCAL_HEX (the serial in
    # environment variable names, upper-case hex), DRA_LOCAL_SIZE (bytes),
    # DRA_LOCAL_DEVICE (memdev-<lower-case hex>, its device in the
    # ResourceSlice), DRA_LOCAL_BLOCK_SIZE (the memory block size of the
    # VM in bytes, see dra-block-size), DRA_LOCAL_LD (its device in the
    # ResourceSlices of dra-py), DRA_LOCAL_REGIONS (its regions in cxl-dump
    # of cxl-py) and, if dra-install runs in the VM, DRA_LOCAL_PUBLISHED
    # (its device in the ResourceSlice as JSON, see dra-local-unchanged).
    # The attachment has the owner e2e:dra-local-device, not a k8s: owner of
    # the pool controller.
    local server=() vmdir host label v installed=""
    if [ "$1" == "--pool" ]; then
        server=(--pool "$2")
        shift 2
    fi
    vmdir="$1"
    host=$(vmdir-name "$vmdir")
    label=$(vmdir-label "$vmdir")
    dra-local-find "${server[@]}" "$vmdir"
    case "$DRA_LOCAL_ATTACHMENT" in
        attached)
            echo "dra-local-device $label: $DRA_LOCAL_NAME is attached already";;
        detached)
            pool-host-client "${server[@]}" -o json attach "$DRA_LOCAL_NAME" --host "$host" --owner e2e:dra-local-device ||
                command-error "cannot attach the local device $DRA_LOCAL_NAME to $label";;
        *)
            error "dra-local-device: the attachment of $DRA_LOCAL_NAME to $host is $DRA_LOCAL_ATTACHMENT";;
    esac
    pool-cxl-wait "$vmdir" local-device "by_serial($DRA_LOCAL_SERIAL) is not None" 60
    for v in "${DRA_INSTALLED_VMS[@]}"; do
        [ "$v" != "$vmdir" ] || installed=1
    done
    DRA_LOCAL_LD="device('$DRA_LOCAL_DEVICE', '$DRA_LOCAL_DRIVER', '$host')"
    DRA_LOCAL_PUBLISHED=""
    if [ -n "$installed" ]; then
        dra-wait "$vmdir" local-device resourceslices "$DRA_LOCAL_LD is not None and attr($DRA_LOCAL_LD, 'type') == 'memdev' and attr($DRA_LOCAL_LD, 'serial') == '$DRA_LOCAL_SERIAL'" 60
        DRA_LOCAL_PUBLISHED=$(dra-value "$COMMAND_OUTPUT" "json.dumps($DRA_LOCAL_LD, sort_keys=True)")
    fi
    DRA_LOCAL_BLOCK_SIZE=$(dra-block-size "$vmdir") ||
        error "dra-local-device: cannot read the memory block size of $label"
    DRA_LOCAL_REGIONS="[r for r in regions if by_serial($DRA_LOCAL_SERIAL) is not None and sorted(r['memdevs']) == [by_serial($DRA_LOCAL_SERIAL)['name']]]"
    echo "dra-local-device $label: $DRA_LOCAL_NAME, serial $DRA_LOCAL_SERIAL ($DRA_LOCAL_HEX), $DRA_LOCAL_SIZE bytes, ResourceSlice device $DRA_LOCAL_DEVICE, memory block size $DRA_LOCAL_BLOCK_SIZE bytes"
}

dra-local-detach() {
    # Usage: dra-local-detach [--pool NAME] VMDIR
    #
    # Detach the local CXL memory device of the VM of VMDIR (see
    # dra-local-device) if it is attached, and wait until cxl-dump does not
    # see it any more. While the driver runs on the node, wait first until
    # it has released the regions of the device. Then release what is left of it
    # in the VM (fake-cxl-pool-client guest disable: offline, destroy the
    # regions, disable the memdev) and detach it through
    # fake-cxl-pool-server NAME (default: the first started server).
    local server=() vmdir host label expr
    if [ "$1" == "--pool" ]; then
        server=(--pool "$2")
        shift 2
    fi
    vmdir="$1"
    host=$(vmdir-name "$vmdir")
    label=$(vmdir-label "$vmdir")
    dra-local-find "${server[@]}" "$vmdir"
    if [ "$DRA_LOCAL_ATTACHMENT" == detached ]; then
        echo "dra-local-detach $label: $DRA_LOCAL_NAME is not attached"
        return 0
    fi
    expr="by_serial($DRA_LOCAL_SERIAL) is None or not any(by_serial($DRA_LOCAL_SERIAL)['name'] in sorted(r['memdevs']) for r in regions)"
    if dra-driver-pod "$vmdir" >/dev/null; then
        pool-cxl-wait "$vmdir" local-released "$expr" 60
    fi
    pool-client "${server[@]}" "$vmdir" guest disable "$DRA_LOCAL_SERIAL" ||
        command-error "cannot release $DRA_LOCAL_NAME in $label"
    pool-host-client "${server[@]}" detach "$DRA_LOCAL_NAME" --host "$host" ||
        command-error "cannot detach $DRA_LOCAL_NAME from $label"
    pool-cxl-wait "$vmdir" local-detached "by_serial($DRA_LOCAL_SERIAL) is None" 60
    echo "dra-local-detach $label: $DRA_LOCAL_NAME detached"
}

dra-memdev-sysfs() {
    # Usage: dra-memdev-sysfs VMDIR SERIAL
    #
    # Read in the sysfs of the VM of VMDIR what the CXL memory device with
    # SERIAL (0x<lower-case hex>) has, as one JSON object:
    #   memdev    its name (memN), null if it is not there
    #   enabled   whether its driver is bound
    #   regions   its regions: name, size, and of its dax region available
    #             (available_size), align and dax: the dax devices (name,
    #             size, node = target_node, dev = "major:minor", driver)
    # DRA_MEMDEV and COMMAND_OUTPUT have the JSON. Return non-zero if the VM
    # cannot be read.
    local vmdir="$1" serial="$2" script text
    script="s=$serial
$(cat <<'EOF'
for m in /sys/bus/cxl/devices/mem[0-9]*; do
    [ "$(cat "$m/serial" 2>/dev/null)" == "$s" ] || continue
    mem=${m##*/} ep="" drv=$(readlink "$m/driver" 2>/dev/null)
    drv=${drv##*/}
    for e in /sys/bus/cxl/devices/endpoint[0-9]*; do
        [ "$(basename "$(readlink "$e/uport" 2>/dev/null)")" == "$mem" ] && ep=${e##*/}
    done
    echo "memdev $mem ${ep:--} ${drv:--}"
    [ -n "$ep" ] || break
    for r in /sys/bus/cxl/devices/region[0-9]*; do
        [ -e "$r" ] || continue
        cat "$r"/target* 2>/dev/null | grep -q "^decoder${ep#endpoint}\." || continue
        dr=$(ls -d "$r"/dax_region[0-9]* 2>/dev/null | head -n 1)
        echo "region ${r##*/} $(cat "$r/size") $(cat "$dr/dax_region/available_size" 2>/dev/null || echo -1) $(cat "$dr/dax_region/align" 2>/dev/null || echo -1)"
        for x in "$dr"/dax[0-9]*.[0-9]*; do
            [ -n "$dr" ] && [ -e "$x" ] || continue
            d=$(readlink "$x/driver" 2>/dev/null)
            d=${d##*/}
            echo "dax ${x##*/} $(cat "$x/size") $(cat "$x/target_node") $(cat "$x/dev") ${d:--}"
        done
    done
    break
done
EOF
)"
    text=$(vmdir-command-q "$vmdir" "$script") || return 1
    DRA_MEMDEV=$(python3 -c '
import json, sys
out = {"serial": sys.argv[1], "memdev": None, "enabled": False, "regions": []}
for line in sys.stdin.read().splitlines():
    w = line.split()
    if len(w) == 4 and w[0] == "memdev":
        out.update(memdev=w[1], endpoint=None if w[2] == "-" else w[2], enabled=w[3] != "-")
    elif len(w) == 5 and w[0] == "region":
        out["regions"].append({"name": w[1], "size": int(w[2], 0), "available": int(w[3], 0), "align": int(w[4], 0), "dax": []})
    elif len(w) == 6 and w[0] == "dax" and out["regions"]:
        out["regions"][-1]["dax"].append({"name": w[1], "size": int(w[2], 0), "node": int(w[3]), "dev": w[4], "driver": None if w[5] == "-" else w[5]})
print(json.dumps(out))' "$serial" <<< "$text") || return 1
    COMMAND_OUTPUT="$DRA_MEMDEV"
}

dra-memdev-wait() {
    # Usage: dra-memdev-wait VMDIR LABEL SERIAL EXPRESSION [TIMEOUT]
    #
    # Repeat dra-memdev-sysfs VMDIR SERIAL until the Python EXPRESSION (see
    # dra-py, j is the document, dax_slices() its dax devices with a size)
    # is true on it, TIMEOUT seconds at most (default 60). Store the last
    # JSON in $TEST_OUTPUT_DIR/memdev.LABEL.<vm>.json and in DRA_MEMDEV and
    # COMMAND_OUTPUT. Fail the test on timeout, showing the JSON.
    local vmdir="$1" label="$2" serial="$3" expression="$4" tmo="${5:-60}" file
    file="$TEST_OUTPUT_DIR/memdev.$label.$(vmdir-label "$vmdir").json"
    retry-until --timeout "$tmo" --message "$(vmdir-label "$vmdir"): sysfs of memdev $serial: $expression" \
        'dra-memdev-sysfs "$vmdir" "$serial" && dra-py assert "$DRA_MEMDEV" "$expression" 2>/dev/null' || {
        echo "$DRA_MEMDEV" > "$file"
        error "$(vmdir-label "$vmdir"): CXL memory device $serial did not reach the expected state in ${tmo}s: $expression
sysfs: $DRA_MEMDEV"
    }
    echo "$DRA_MEMDEV" > "$file"
    COMMAND_OUTPUT="$DRA_MEMDEV"
    echo "memdev ok: $label: $expression"
    echo "memdev $serial: $DRA_MEMDEV"
}

dra-block-size() {
    # Usage: dra-block-size VMDIR
    #
    # Print the memory block size of the VM of VMDIR in bytes, the unit in
    # which memory goes online and offline: block_size_bytes in
    # /sys/devices/system/memory, parsed by cxl-block-size-parse
    # (cxl.source.sh). Return non-zero if it cannot be read.
    local text
    type -t cxl-block-size-parse >/dev/null ||
        error "${FUNCNAME[1]}: needs cxl.source.sh"
    text=$(vmdir-command-q "$1" "cat /sys/devices/system/memory/block_size_bytes") || return 1
    cxl-block-size-parse "$text"
}

dra-numa-node-memtotal() {
    # Usage: dra-numa-node-memtotal VMDIR NODE
    #
    # Print MemTotal of NUMA node NODE of the VM of VMDIR in kB
    # (/sys/devices/system/node/nodeNODE/meminfo), 0 if the node is not
    # there, quietly. Return non-zero if it cannot be read or NODE is not a
    # number.
    local kb
    [[ "$2" =~ ^[0-9]+$ ]] || return 1
    kb=$(vmdir-command-q "$1" "d=/sys/devices/system/node/node$2; if [ -d \$d ]; then awk '\$3 == \"MemTotal:\" {print \$4}' \$d/meminfo; else echo 0; fi") || return 1
    [[ "$kb" =~ ^[0-9]+$ ]] || return 1
    echo "$kb"
}

dra-numa-node-wait() {
    # Usage: dra-numa-node-wait VMDIR NODE KB [TIMEOUT]
    #
    # Wait until MemTotal of NUMA node NODE of the VM of VMDIR is KB kB (0:
    # the node has no memory or is not there), TIMEOUT seconds at most
    # (default 30), and print the line of the node in "numactl -H".
    # DRA_NUMA_NODE_KB is the last MemTotal read. Fail the test on timeout.
    local vmdir="$1" node="$2" kb="$3" tmo="${4:-30}" label
    label=$(vmdir-label "$vmdir")
    [[ "$node" =~ ^[0-9]+$ ]] || error "dra-numa-node-wait: NUMA node '$node' is not a number"
    DRA_NUMA_NODE_KB=""
    retry-until --timeout "$tmo" --interval 1 --message "$label: MemTotal of NUMA node $node is $kb kB" \
        'DRA_NUMA_NODE_KB=$(dra-numa-node-memtotal "$vmdir" "$node") && [ "$DRA_NUMA_NODE_KB" == "$kb" ]' ||
        error "$label: MemTotal of NUMA node $node is ${DRA_NUMA_NODE_KB:-unreadable} kB after ${tmo}s, expected $kb kB"
    echo "node ok: $label: MemTotal of NUMA node $node is $kb kB, numactl -H: $(vmdir-command-q "$vmdir" "numactl -H | grep '^node $node size:'" || echo "no node $node")"
}

dra-memmap-on-memory-off() {
    # Usage: dra-memmap-on-memory-off VMDIR
    #
    # Fail the test unless MemTotal of a NUMA node of the VM of VMDIR grows
    # and shrinks by exactly the memory blocks that go online and offline
    # on it: memory_hotplug.memmap_on_memory is off (the parameter is
    # missing or N).
    local value
    value=$(vmdir-command-q "$1" "cat /sys/module/memory_hotplug/parameters/memmap_on_memory 2>/dev/null || echo missing")
    echo "$(vmdir-label "$1"): memory_hotplug.memmap_on_memory: $value"
    [ "$value" == missing ] || [ "$value" == N ] ||
        error "memory_hotplug.memmap_on_memory is '$value' in $(vmdir-label "$1"), expected missing or N: with it, the vmemmap of hotplugged memory blocks comes from the blocks themselves, and MemTotal of a node is not the size of its online blocks. Boot the VM without memory_hotplug.memmap_on_memory"
}

dra-mi() {
    # Usage: dra-mi BYTES
    #
    # Print BYTES as a quantity in Mi.
    echo "$(( $1 >> 20 ))Mi"
}

dra-local-round() {
    # Usage: dra-local-round BYTES
    #
    # Print BYTES rounded up to whole memory blocks of DRA_LOCAL_BLOCK_SIZE
    # bytes (see dra-local-device): what a claim of BYTES consumes of a
    # local device, the step of its capacity.
    local bs="$DRA_LOCAL_BLOCK_SIZE"
    echo $(( ($1 + bs - 1) / bs * bs ))
}

dra-local-blocks-require() {
    # Usage: dra-local-blocks-require VMDIR TOPOLOGY BLOCKS
    #
    # Fail the test unless the memory block size of the VM of VMDIR
    # (DRA_LOCAL_BLOCK_SIZE) is a multiple of 16 MiB and its local device
    # (see dra-local-device) is whole blocks, at least BLOCKS of them. The
    # VM was created from TOPOLOGY.
    local vmdir="$1" topology="$2" blocks="$3" bs="$DRA_LOCAL_BLOCK_SIZE" mib=$(( 1 << 20 ))
    if ! [[ "$bs" =~ ^[0-9]+$ ]] || (( bs < 16 * mib || bs % (16 * mib) != 0 )); then
        error "unexpected memory block size '$bs' bytes in $(vmdir-label "$vmdir")"
    fi
    (( DRA_LOCAL_SIZE % bs == 0 && DRA_LOCAL_SIZE >= blocks * bs )) ||
        error "the local device $DRA_LOCAL_NAME has $DRA_LOCAL_SIZE bytes, the test needs $blocks memory blocks of $bs bytes, as in topology.var.json of $topology: recreate the VM: vagrant destroy in $vmdir, remove the directory, and run
  qemu_bin=/path/to/qemu-system-x86_64 ./run_tests.sh memory.test-suite/dra-driver-cxl/$topology/test00-up"
    echo "memory block size $bs bytes ($(dra-mi "$bs")), the local device has $(( DRA_LOCAL_SIZE / bs )) blocks"
}

dra-local-slices-wait() {
    # Usage: dra-local-slices-wait VMDIR LABEL NODE numa|dax:BYTES...
    #
    # Wait until the local device of the VM of VMDIR (see dra-local-device)
    # has one region with exactly these slices: numa slices bound to kmem,
    # dax slices bound to device_dax, all of them dax devices of NUMA node
    # NODE ("": any one node), and the rest of the region available.
    # DRA_MEMDEV is the sysfs view of the device.
    local vmdir="$1" label="$2" node="$3" want="" total=0 s kind size
    shift 3
    for s in "$@"; do
        kind=${s%%:*} size=${s#*:}
        case "$kind" in
            numa) want+="('kmem', $size), ";;
            dax) want+="('device_dax', $size), ";;
            *) error "dra-local-slices-wait: unknown slice $s";;
        esac
        total=$(( total + size ))
    done
    dra-memdev-wait "$vmdir" "$label" "$DRA_LOCAL_SERIAL" \
        "len(j['regions']) == 1 and sorted((d['driver'], d['size']) for d in dax_slices()) == sorted([$want]) and len(set(d['node'] for d in dax_slices())) == 1${node:+ and dax_slices()[0]['node'] == $node} and j['regions'][0]['available'] == $DRA_LOCAL_SIZE - $total"
}

dra-local-numa-node-size() {
    # Usage: dra-local-numa-node-size VMDIR LABEL NODE BYTES [LOCAL]
    #
    # Wait until NUMA node NODE of the VM of VMDIR has BYTES of memory,
    # whole memory blocks (MemTotal; 0: no memory or no node), and until
    # the region of its local device (see dra-local-device), if there is
    # one, has LOCAL bytes online in cxl-dump (default BYTES).
    local vmdir="$1" label="$2" node="$3" bytes="$4" local_bytes="${5:-$4}" bs="$DRA_LOCAL_BLOCK_SIZE" blocks
    (( bytes % bs == 0 )) || error "dra-local-numa-node-size $label: $bytes bytes are not whole memory blocks of $bs bytes"
    blocks=$(( bytes / bs ))
    dra-numa-node-wait "$vmdir" "$node" "$(( blocks * (bs / 1024) ))" 30
    pool-cxl-wait "$vmdir" "node-$label" "len($DRA_LOCAL_REGIONS) <= 1 and all(online_size(r) == $local_bytes for r in $DRA_LOCAL_REGIONS)" 10
}

dra-local-slices-none() {
    # Usage: dra-local-slices-none VMDIR LABEL
    #
    # Wait until the local device of the VM of VMDIR (see dra-local-device)
    # has no slice and no region: its memdev enabled and no region of it,
    # neither in cxl-dump nor in sysfs. DRA_MEMDEV is the sysfs
    # view of the device.
    local vmdir="$1" label="$2"
    pool-cxl-wait "$vmdir" "$label" "by_serial($DRA_LOCAL_SERIAL) is not None and enabled(by_serial($DRA_LOCAL_SERIAL)) and $DRA_LOCAL_REGIONS == []" 60
    dra-memdev-wait "$vmdir" "$label" "$DRA_LOCAL_SERIAL" "j['memdev'] and j['enabled'] and j['regions'] == []" 10
}

dra-local-unchanged() {
    # Usage: dra-local-unchanged VMDIR
    #
    # Fail the test unless the node's cxl.nri.io ResourceSlice publishes
    # the local device of the VM of VMDIR exactly as dra-local-device saw
    # it (DRA_LOCAL_PUBLISHED): the slices, regions and modes of its claims
    # and leases are not published. COMMAND_OUTPUT has the ResourceSlices.
    [ -n "$DRA_LOCAL_PUBLISHED" ] ||
        error "dra-local-unchanged: no published device, call dra-local-device after dra-install"
    dra-json "$1" resourceslices
    dra-assert "$COMMAND_OUTPUT" "json.dumps($DRA_LOCAL_LD, sort_keys=True) == $(python3 -c 'import sys; print(repr(sys.argv[1]))' "$DRA_LOCAL_PUBLISHED")"
}

dra-local-claim-check() {
    # Usage: dra-local-claim-check VMDIR POD BYTES
    #
    # Check that the claim of pod POD in $DRA_NS is allocated to the local
    # device of the VM of VMDIR (see dra-local-device) on its node and
    # consumes BYTES of its memory. DRA_CLAIM_JSON and DRA_CLAIM_UID are
    # the claim.
    local vmdir="$1" pod="$2" bytes="$3" claim
    claim=$(dra-pod-claim "$vmdir" "$pod") || error "pod $pod has no ResourceClaim"
    dra-json "$vmdir" resourceclaim -n "$DRA_NS" "$claim"
    DRA_CLAIM_JSON="$COMMAND_OUTPUT"
    dra-assert "$DRA_CLAIM_JSON" "[(r['driver'], r['device']) for r in results()] == [('$DRA_LOCAL_DRIVER', '$DRA_LOCAL_DEVICE')] and allocated_node() == '$(vmdir-name "$vmdir")'"
    dra-assert "$DRA_CLAIM_JSON" "consumed() == $bytes"
    DRA_CLAIM_UID=$(dra-value "$DRA_CLAIM_JSON" "j['metadata']['uid']")
}

dra-delete-pod() {
    # Usage: dra-delete-pod VMDIR POD
    #
    # Delete pod POD in $DRA_NS and wait until it is gone.
    dra-delete "$1" pod "$2" --grace-period=2 --wait=true --timeout=60s
}

dra-cdi-specs() {
    # Usage: dra-cdi-specs VMDIR
    #
    # Print the CDI spec files (JSON) in $DRA_CDI_DIR of the VM of VMDIR,
    # quietly, one per line: the path, a tab and the content without
    # newlines. Return non-zero if the directory cannot be read.
    local script
    script="dir=$(printf %q "$DRA_CDI_DIR")
$(cat <<'EOF2'
[ -r "$dir" ] || exit 1
for f in "$dir"/*.json; do
    [ -f "$f" ] || continue
    printf '%s\t' "$f"
    tr -d '\n' < "$f"
    echo
done
EOF2
)"
    vmdir-command-q "$1" "$script"
}

dra-cdi-py() {
    # Usage: dra-cdi-py device|devices-with ARG
    #
    # Read the output of dra-cdi-specs and print CDI device ARG (device) or
    # the devices that contain ARG (devices-with), see the callers.
    python3 -c '
import json, sys

mode, arg = sys.argv[1], sys.argv[2]
found = set()
for line in sys.stdin.read().splitlines():
    path, _, text = line.partition("\t")
    if not text.strip():
        continue
    spec = json.loads(text)
    for dev in spec.get("devices") or []:
        if mode == "devices-with" and arg in json.dumps(dev):
            found.add(dev["name"])
        if mode == "device" and dev.get("name") == arg:
            edits = dev.get("containerEdits") or {}
            print(json.dumps({"file": path, "kind": spec.get("kind"), "name": arg,
                              "env": edits.get("env") or [],
                              "deviceNodes": edits.get("deviceNodes") or [],
                              "mounts": edits.get("mounts") or []}))
            sys.exit(0)
for name in sorted(found):
    print(name)' "$1" "$2"
}

dra-cdi-device() {
    # Usage: dra-cdi-device VMDIR NAME
    #
    # Print CDI device NAME of the CDI specs of the VM of VMDIR (see
    # dra-cdi-specs) as a JSON object, quietly: file and kind of its spec,
    # name, and env, deviceNodes and mounts of its containerEdits ([] if
    # none). Print nothing if there is no such device.
    # Example: dra-cdi-device "$VM" "claim-$CLAIM_UID-0"
    dra-cdi-specs "$1" | dra-cdi-py device "$2"
}

dra-cdi-devices-with() {
    # Usage: dra-cdi-devices-with VMDIR TEXT
    #
    # Print the names of the CDI devices of the CDI specs of the VM of
    # VMDIR (see dra-cdi-specs) whose JSON has TEXT (a fixed string, for
    # instance an environment variable name), sorted, one per line,
    # quietly. Return non-zero if the specs cannot be read.
    local specs
    specs=$(dra-cdi-specs "$1") || return 1
    dra-cdi-py devices-with "$2" <<< "$specs"
}

dra-lease-cdi-assert() {
    # Usage: dra-lease-cdi-assert VMDIR CLAIM-UID
    #
    # Fail the test unless the VM of VMDIR has the CDI device lease-CLAIM-UID
    # of kind $DRA_LOCAL_DRIVER/device, the device of a lease claim that may
    # lease filesystems: env CXL_LEASE_SOCKET=/run/cxl/cxl.sock and exactly
    # three bind mounts, $DRA_LEASE_DIR/CLAIM-UID at /run/cxl (rbind, ro),
    # then $DRA_LEASE_FS_DIR/CLAIM-UID at $DRA_LEASE_FS_CTR_DIR (rbind, ro,
    # rslave), and $DRA_REQUEST_NODE_BIN at $DRA_REQUEST_CTR_BIN (bind, ro).
    local vmdir="$1" uid="$2" cdi
    cdi=$(dra-cdi-device "$vmdir" "lease-$uid")
    echo "CDI device lease-$uid: $cdi"
    [ -n "$cdi" ] || error "no CDI device lease-$uid in $(vmdir-label "$vmdir")"
    dra-assert "$cdi" "j['kind'] == '$DRA_LOCAL_DRIVER/device' and 'CXL_LEASE_SOCKET=/run/cxl/cxl.sock' in j['env']"
    dra-assert "$cdi" "sorted((m.get('hostPath'), m.get('containerPath'), m.get('type'), m.get('options')) for m in j['mounts']) == sorted([('$DRA_LEASE_DIR/$uid', '/run/cxl', 'bind', ['rbind', 'ro']), ('$DRA_LEASE_FS_DIR/$uid', '$DRA_LEASE_FS_CTR_DIR', 'bind', ['rbind', 'ro', 'rslave']), ('$DRA_REQUEST_NODE_BIN', '$DRA_REQUEST_CTR_BIN', 'bind', ['bind', 'ro'])])"
    # The socket directory first: the filesystem directory is mounted inside it.
    dra-assert "$cdi" "[m['containerPath'] for m in j['mounts']].index('/run/cxl') < [m['containerPath'] for m in j['mounts']].index('$DRA_LEASE_FS_CTR_DIR')"
}

dra-fs-mounts() {
    # Usage: dra-fs-mounts VMDIR [DIR]
    #
    # Print the mounts under DIR (default $DRA_POOL_MOUNT_DIR) in the mount
    # namespace of the VM of VMDIR as a JSON list, quietly, one object per
    # mount: target, source, fstype, options (the superblock options, "ro"
    # or "rw" first), vfs (the options of the mount itself), and for a
    # loop device source its backing file (backing), direct I/O (dio) and
    # read-only (ro) flags ("" for other sources). COMMAND_OUTPUT has the
    # list. Return non-zero if the mounts cannot be read or a line of
    # findmnt cannot be parsed.
    local lines dir="${2:-$DRA_POOL_MOUNT_DIR}"
    lines=$(vmdir-command-q "$1" "findmnt -rn -o TARGET,SOURCE,FSTYPE,FS-OPTIONS,VFS-OPTIONS | while read -r t s f o v; do
    case \"\$t\" in $dir/*) ;; *) continue;; esac
    o=\${o:--} v=\${v:--} b=- d=- r=-
    case \"\$s\" in /dev/loop[0-9]*) l=/sys/block/\${s#/dev/}; b=\$(cat \$l/loop/backing_file); d=\$(cat \$l/loop/dio); r=\$(cat \$l/ro);; esac
    echo \"\$t \$s \$f \$o \$v \$b \$d \$r\"
done") || return 1
    COMMAND_OUTPUT=$(python3 -c '
import json, sys
keys = ["target", "source", "fstype", "options", "vfs", "backing", "dio", "ro"]
out = []
for line in sys.stdin.read().splitlines():
    fields = line.split()
    if not fields:
        continue
    if len(fields) != len(keys):
        sys.exit("dra-fs-mounts: cannot parse: %r" % line)
    out.append({k: ("" if v == "-" else v) for k, v in zip(keys, fields)})
print(json.dumps(out, sort_keys=True))' <<< "$lines") || return 1
    echo "$COMMAND_OUTPUT"
}

dra-pmem-loops() {
    # Usage: dra-pmem-loops VMDIR
    #
    # Print the loop devices of the VM of VMDIR whose backing file is a
    # pmem block device, one "loopN /dev/pmemK" per line, quietly. Return
    # non-zero if the loop devices cannot be listed.
    vmdir-command-q "$1" "test -d /sys/block || exit 1; for f in /sys/block/loop*/loop/backing_file; do [ -f \"\$f\" ] || continue; b=\$(cat \"\$f\") || exit 1; case \"\$b\" in /dev/pmem*) n=\${f#/sys/block/}; echo \"\${n%%/*} \$b\";; esac; done; exit 0"
}

dra-lease-fs-mounts() {
    # Usage: dra-lease-fs-mounts VMDIR
    #
    # Print the mounts under $DRA_LEASE_FS_DIR in the mount namespace of the
    # VM of VMDIR as a JSON list sorted by target, quietly, one object per
    # mount: target, source, fstype, options (the superblock options) and
    # vfs (the options of the mount itself, for instance
    # ro,nosuid,nodev,relatime). COMMAND_OUTPUT has the list. Return
    # non-zero if the mounts cannot be read.
    local json
    json=$(vmdir-command-q "$1" "findmnt -J -l -o TARGET,SOURCE,FSTYPE,FS-OPTIONS,VFS-OPTIONS") || return 1
    COMMAND_OUTPUT=$(python3 -c '
import json, sys
base = sys.argv[1].rstrip("/") + "/"
out = [{"target": m["target"], "source": m.get("source") or "", "fstype": m.get("fstype") or "",
        "options": m.get("fs-options") or "", "vfs": m.get("vfs-options") or ""}
       for m in json.loads(sys.stdin.read())["filesystems"] if m["target"].startswith(base)]
print(json.dumps(sorted(out, key=lambda m: m["target"]), sort_keys=True))' "$DRA_LEASE_FS_DIR" <<< "$json") || return 1
    echo "$COMMAND_OUTPUT"
}

dra-local-state-claims() {
    # Usage: dra-local-state-claims VMDIR
    #
    # Print how many claims the driver in the VM of VMDIR has on its
    # local memdevs in its state document (0 without one): the
    # local_claims() of dra-py of every local memdev.
    local json
    json=$(dra-state-json "$1" 2>/dev/null) || json='{}'
    echo "$DRA_STATE: claims of local memdevs: $(dra-value "$json" "json.dumps({m['serial']: local_claims(m) for m in local_memdevs()})")" >&2
    dra-value "$json" "sum(len(local_claims(m)) for m in local_memdevs())"
}

dra-local-initial-state() {
    # Usage: dra-local-initial-state VMDIR LABEL [NODE]
    #
    # Wait until the local device of the VM of VMDIR (see dra-local-device)
    # is in its initial state again: the memdev enabled, no region (see
    # dra-local-slices-none), the device published as it was
    # (dra-local-unchanged), and no memory on its NUMA node NODE (if
    # given). Check that nothing of its claims is left: no CDI device with
    # an environment variable of the device (CXL_<KEY>_), no claims in the
    # state of the driver, which must have its state file.
    local vmdir="$1" label="$2" node="$3"
    dra-local-slices-none "$vmdir" "$label"
    dra-local-unchanged "$vmdir"
    [ -z "$(dra-cdi-devices-with "$vmdir" "CXL_${DRA_LOCAL_HEX}_")" ] ||
        error "$label: CDI devices of claims of the local device are left: $(dra-cdi-devices-with "$vmdir" "CXL_${DRA_LOCAL_HEX}_")"
    vmdir-command-q "$vmdir" "test -s $DRA_STATE" ||
        error "$label: the driver has no $DRA_STATE"
    [ "$(dra-local-state-claims "$vmdir")" == 0 ] ||
        error "$label: the driver still has claims of local memdevs in $DRA_STATE"
    [ -z "$node" ] || dra-numa-node-wait "$vmdir" "$node" 0 10
    echo "$label: the local device is in its initial state"
}

dra-local-state-shared() {
    # Usage: dra-local-state-shared VMDIR CLAIM-UID...
    #
    # Return 0 if the state document of the driver in the VM of VMDIR
    # ($DRA_STATE) has the local memdev DRA_LOCAL_SERIAL with the fact
    # "shared": true and exactly the claims CLAIM-UID (none: no claims, see
    # local_claims() of dra-py). DRA_STATE_JSON is the document as it was
    # read.
    local vmdir="$1" want=""
    shift
    DRA_STATE_JSON=$(dra-state-json "$vmdir" 2>/dev/null) || DRA_STATE_JSON='{}'
    [ "$#" == 0 ] || want=$(printf "'%s'," "$@")
    dra-py assert "$DRA_STATE_JSON" "(lambda m: m is not None and m.get('source') == 'local' and (m.get('facts') or {}).get('shared') is True and sorted(local_claims(m)) == sorted([$want]))(memdev('$DRA_LOCAL_SERIAL'))"
}

dra-local-shared-wait() {
    # Usage: dra-local-shared-wait VMDIR LABEL [CLAIM-UID...]
    #
    # Wait until the local device of the VM of VMDIR (see dra-local-device)
    # is a shared device that exactly the claims CLAIM-UID use (none: an
    # idle shared device, which keeps its region): the state file of the
    # driver has the device with "shared": true and these claims;
    # cxl-dump sees one region of the device, enabled, nothing of
    # it online; in sysfs the memdev is enabled and its one region is the
    # whole device, with one dax device of that size bound to device_dax.
    # DRA_LOCAL_SHARED_REGION, DRA_LOCAL_SHARED_DAX and
    # DRA_LOCAL_SHARED_MEMDEV are the region (regionN), its dax device
    # (daxX.Y) and the memdev (memN), DRA_MEMDEV the sysfs view.
    local vmdir="$1" label="$2" vmlabel claims
    shift 2
    claims=("$@")
    vmlabel=$(vmdir-label "$vmdir")
    retry-until --timeout 60 --interval 1 --message "$vmlabel: $DRA_STATE has $DRA_LOCAL_SERIAL shared, with claims [$*]" \
        'dra-local-state-shared "$vmdir" "${claims[@]}"' ||
        error "$vmlabel: $DRA_STATE does not have $DRA_LOCAL_SERIAL shared with claims [$*]: $(dra-py eval "$DRA_STATE_JSON" "json.dumps((lambda m: m and dict(m, claims=local_claims(m)))(memdev('$DRA_LOCAL_SERIAL')))")"
    echo "$vmlabel: $DRA_STATE: $DRA_LOCAL_SERIAL shared, claims [$*]"
    pool-cxl-wait "$vmdir" "$label" "len($DRA_LOCAL_REGIONS) == 1 and enabled(${DRA_LOCAL_REGIONS}[0]) and online_size(${DRA_LOCAL_REGIONS}[0]) == 0" 30
    dra-memdev-wait "$vmdir" "$label" "$DRA_LOCAL_SERIAL" \
        "j['enabled'] and len(j['regions']) == 1 and j['regions'][0]['size'] == $DRA_LOCAL_SIZE and [(d['size'], d['driver']) for d in dax_slices()] == [($DRA_LOCAL_SIZE, 'device_dax')]" 30
    DRA_LOCAL_SHARED_REGION=$(dra-value "$DRA_MEMDEV" "j['regions'][0]['name']")
    DRA_LOCAL_SHARED_DAX=$(dra-value "$DRA_MEMDEV" "dax_slices()[0]['name']")
    DRA_LOCAL_SHARED_MEMDEV=$(dra-value "$DRA_MEMDEV" "j['memdev']")
    echo "$vmlabel: the shared local device is $DRA_LOCAL_SHARED_REGION, /dev/$DRA_LOCAL_SHARED_DAX"
}

dra-local-seed() {
    # Usage: dra-local-seed VMDIR TEXT OFFSET...
    #
    # Write TEXT at every OFFSET of the local CXL memory device of the VM
    # of VMDIR (see dra-local-device) and read it back, through a devdax
    # region that fake-cxl-pool-client creates in the VM, then destroy the
    # region, leaving the memdev enabled and without regions. Fail the
    # test if any step fails.
    local vmdir="$1" text="$2" label region dax off
    shift 2
    label=$(vmdir-label "$vmdir")
    pool-client "$vmdir" guest region create "$DRA_LOCAL_SERIAL" --mode dax ||
        command-error "cannot create a devdax region of the local device in $label"
    dra-memdev-wait "$vmdir" seed-region "$DRA_LOCAL_SERIAL" "len(j['regions']) == 1 and len(dax_slices()) == 1" 30
    region=$(dra-value "$DRA_MEMDEV" "j['regions'][0]['name']")
    dax=/dev/$(dra-value "$DRA_MEMDEV" "dax_slices()[0]['name']")
    for off in "$@"; do
        vmdir-command "$vmdir" "dax-rw.py write $dax $off $(printf %q "$text") && dax-rw.py read $dax $off" ||
            command-error "cannot write to $dax at $off in $label"
        [ "$(tail -n 1 <<< "$COMMAND_OUTPUT")" == "$text" ] ||
            error "read '$(tail -n 1 <<< "$COMMAND_OUTPUT")' at $off of $dax in $label, expected '$text'"
    done
    vmdir-command "$vmdir" "cxl destroy-region -f $region" ||
        command-error "cannot destroy $region in $label"
    dra-memdev-wait "$vmdir" seed-gone "$DRA_LOCAL_SERIAL" "j['memdev'] and j['enabled'] and j['regions'] == []" 30
    echo "dra-local-seed $label: '$text' at $* of the local device, $region destroyed"
}

dra-driver-log-wait() {
    # Usage: dra-driver-log-wait VMDIR TEXT [TIMEOUT]
    #
    # Wait until the log of the driver on the node of the VM of VMDIR (see
    # dra-driver-log) has a line with the fixed string TEXT, TIMEOUT
    # seconds at most (default 30), and print the line. Fail the test on
    # timeout.
    local vmdir="$1" text="$2" tmo="${3:-30}"
    retry-until --timeout "$tmo" --interval 1 --message "$(vmdir-label "$vmdir"): the log of $DRA_CONTAINER has '$text'" \
        'dra-driver-log "$vmdir" | grep -qF -- "$text"' ||
        error "the log of $DRA_CONTAINER in $(vmdir-label "$vmdir") has no line with '$text'"
    dra-driver-log "$vmdir" | grep -F -- "$text"
}

dra-local-shared-cleanup() {
    # Usage: dra-local-shared-cleanup VMDIR
    #
    # The EXIT trap of a test that configures the local device of the VM of
    # VMDIR shared (DRA_DRIVER_CONFIG sharedMemdevs): such a device keeps
    # its region while the driver runs. dra-cleanup, then, with the driver
    # gone, release the regions of the device in the VM (fake-cxl-pool-client
    # guest disable) and wait until none is left, then pool-cleanup, which
    # detaches the device. If a region is left, or the device cannot be
    # read, stop the server only and leave the device attached: a detach
    # cannot hot-remove it, and pool-vm-reset of the next test restarts the
    # VM. Never fails the test.
    local vmdir="$1" label
    label=$(vmdir-label "$vmdir")
    dra-cleanup
    if [ -n "$DRA_LOCAL_SERIAL" ] && dra-memdev-sysfs "$vmdir" "$DRA_LOCAL_SERIAL" &&
           dra-py assert "$DRA_MEMDEV" "j['memdev'] and j['regions']"; then
        echo "dra-local-shared-cleanup: $label: the local device $DRA_LOCAL_SERIAL has regions, releasing them"
        pool-client "$vmdir" guest disable "$DRA_LOCAL_SERIAL"
    fi
    if [ -n "$DRA_LOCAL_SERIAL" ] && ! retry-until --timeout 60 --interval 1 --message "$label: no region of the local device $DRA_LOCAL_SERIAL" \
           'dra-memdev-sysfs "$vmdir" "$DRA_LOCAL_SERIAL" && dra-py assert "$DRA_MEMDEV" "j[\"regions\"] == []"'; then
        echo "dra-local-shared-cleanup: $label: the local device $DRA_LOCAL_SERIAL still has regions or cannot be read, not detaching it: $DRA_MEMDEV
  The next test restarts the VM (pool-vm-reset), or by hand in the VM: fake-cxl-pool-client guest disable $DRA_LOCAL_SERIAL"
        pool-server-stop
        return 0
    fi
    pool-cleanup
    return 0
}

dra-driver-restart() {
    # Usage: dra-driver-restart VMDIR
    #
    # Restart the driver on the node of the VM of VMDIR: store the log of
    # its pod as $TEST_OUTPUT_DIR/nri-dra-driver-cxl.<vm>.<pod>.log, delete
    # the pod, and wait until the DaemonSet runs a new pod there that is
    # Ready, and the node has what dra-driver-wait waits for. A running trace
    # follows the log of the new pod (dra-trace-driver-follow).
    # DRA_DRIVER_POD is the new pod.
    local vmdir="$1" label old
    label=$(vmdir-label "$vmdir")
    old=$(dra-driver-pod "$vmdir") ||
        error "dra-driver-restart: $DRA_DAEMONSET does not run on $(vmdir-name "$vmdir")"
    vmdir-command-q "$vmdir" "kubectl logs -n kube-system $old -c $DRA_CONTAINER" > "$TEST_OUTPUT_DIR/$DRA_CONTAINER.$label.$old.log" 2>&1
    echo "dra-driver-restart $label: log of $old in $TEST_OUTPUT_DIR/$DRA_CONTAINER.$label.$old.log"
    vmdir-command "$vmdir" "kubectl delete pod -n kube-system $old --wait=true --timeout=60s" ||
        command-error "cannot delete the pod $old of $DRA_DAEMONSET in $label"
    DRA_DRIVER_POD=""
    retry-until --timeout 120 --interval 1 --message "$label: a new Ready pod of $DRA_DAEMONSET" \
        'DRA_DRIVER_POD=$(dra-driver-pod "$vmdir") && [ "$DRA_DRIVER_POD" != "$old" ] && vmdir-command-q "$vmdir" "kubectl wait -n kube-system --for=condition=Ready pod/$DRA_DRIVER_POD --timeout=1s" >/dev/null 2>&1' || {
        vmdir-command "$vmdir" "kubectl get pods -n kube-system -l $DRA_POD_LABEL -o wide"
        error "dra-driver-restart: no new Ready pod of $DRA_DAEMONSET in $label"
    }
    echo "dra-driver-restart $label: $old replaced by $DRA_DRIVER_POD"
    dra-driver-wait "$vmdir"
    dra-trace-driver-follow "$vmdir"
}

dra-driver-log() {
    # Usage: dra-driver-log VMDIR
    #
    # Print the log of the driver on the node of the VM of VMDIR since its
    # container started (kubectl logs of its pod), quietly. Fail the test if
    # the driver does not run there.
    local pod
    pod=$(dra-driver-pod "$1") ||
        error "dra-driver-log: $DRA_DAEMONSET does not run on $(vmdir-name "$1")"
    vmdir-command-q "$1" "kubectl logs -n kube-system $pod -c $DRA_CONTAINER"
}

dra-local-slices-aligned() {
    # Usage: dra-local-slices-aligned VMDIR
    #
    # Fail the test if the driver in the VM of VMDIR, since it started,
    # created a region of a local device that does not start at a memory
    # block boundary, placed a slice explicitly (the range that the kernel
    # chose was not whole memory blocks) or rounded a slice up (a claim
    # consumed less than whole memory blocks).
    local log
    log=$(dra-driver-log "$1")
    if grep -E 'engine: local memdev .*(does not start at a memory block boundary|placing it explicitly|rounded up to)' <<< "$log"; then
        error "the driver aligned, placed or rounded regions or slices of local devices itself, see above"
    fi
    echo "the driver's regions and slices were block-aligned: no slice placed explicitly or rounded up"
}

dra-memory-config() {
    # Usage: dra-memory-config SECTIONS
    #
    # Print the parameters of an opaque config entry of a claim or a
    # DeviceClass, either driver, as a YAML flow mapping: apiVersion
    # $DRA_CONFIG_API, kind $DRA_CONFIG_KIND and SECTIONS, the sections of
    # the configuration as flow YAML ("": none).
    # Example: dra-memory-config "device: {mode: dax}, container: {steering: {policy: auto}}"
    echo "{apiVersion: $DRA_CONFIG_API, kind: $DRA_CONFIG_KIND${1:+, $1}}"
}

dra-config-sections() {
    # Usage: dra-config-sections DEVICE CONTAINER POOL
    #
    # Print the sections device, container and pool of a configuration as
    # flow YAML, each with its fields (flow YAML, "": no such section).
    local out="" name fields
    for name in device container pool; do
        fields="$1"
        shift
        [ -n "$fields" ] || continue
        out+="${out:+, }$name: {$fields}"
    done
    echo "$out"
}

dra-claim-yaml() {
    # Usage: dra-claim-yaml claim|template NAME CLASS SIZE [MODE [read-only]] [SECTION=FIELDS...]
    #
    # Print a ResourceClaim (claim) or a ResourceClaimTemplate (template)
    # NAME with one request "mem" of DeviceClass CLASS for SIZE of memory
    # ("": no amount). The claim has an opaque config of kind
    # $DRA_CONFIG_KIND for the driver of CLASS ($DRA_POOL_DRIVER for
    # cxl-pool*, else $DRA_LOCAL_DRIVER) if any of these is given:
    #   MODE            device.mode (numa, dax, ...; "": none)
    #   read-only       device.readOnly: true
    #   device=FIELDS   more fields of the device section (flow YAML)
    #   container=FIELDS  the container section, for instance
    #                   "steering: {policy: auto}"
    #   pool=FIELDS     the pool section, for instance
    #                   "opaque: {fake-cxl-pool: {numaNode: 1}}"
    # Examples:
    #   dra-claim-yaml template dax-ro cxl-local 64Mi dax read-only
    #   dra-claim-yaml claim steered cxl-local 128Mi "container=steering: {policy: auto}"
    local kind="$1" name="$2" class="$3" size="$4" driver="$DRA_LOCAL_DRIVER" mode="" ro="" device="" container="" pool="" a n=0 sections devices
    shift 4
    for a in "$@"; do
        case "$a" in
            device=*) device="${a#device=}";;
            container=*) container="${a#container=}";;
            pool=*) pool="${a#pool=}";;
            *)
                case "$n" in
                    0) mode="$a";;
                    1) ro="$a";;
                    *) error "dra-claim-yaml: unexpected argument '$a'";;
                esac
                n=$(( n + 1 ));;
        esac
    done
    [ -z "$ro" ] || [ "$ro" == read-only ] || error "dra-claim-yaml: '$ro' is not read-only"
    [[ "$class" != cxl-pool* ]] || driver="$DRA_POOL_DRIVER"
    [ -z "$ro" ] || device="readOnly: true${device:+, $device}"
    [ -z "$mode" ] || device="mode: $mode${device:+, $device}"
    sections=$(dra-config-sections "$device" "$container" "$pool")
    devices="devices:
  requests:
  - name: mem
    exactly:
      deviceClassName: $class"
    if [ -n "$size" ]; then
        devices+="
      capacity:
        requests:
          memory: $size"
    fi
    if [ -n "$sections" ]; then
        devices+="
  config:
  - opaque:
      driver: $driver
      parameters: $(dra-memory-config "$sections")"
    fi
    # shellcheck disable=SC2001 # sed indents every line
    if [ "$kind" == claim ]; then
        cat <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: $name
spec:
$(sed 's/^/  /' <<< "$devices")
EOF
    else
        cat <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: $name
spec:
  spec:
$(sed 's/^/    /' <<< "$devices")
EOF
    fi
}

dra-pod-yaml() {
    # Usage: dra-pod-yaml NAME NODE [CLAIM...] [-- CONTAINER...]
    #
    # Print pod NAME on Kubernetes node NODE (nodeSelector
    # kubernetes.io/hostname), restartPolicy Never, no memory limits, with
    # containers of $DRA_TOOLS_IMAGE that have the tools of
    # dra-tools-install in /tools and run $DRA_RUNNER as their main
    # process (see dra-run). Every CLAIM is a claim of the pod:
    #   CNAME=claim:CLAIM        the ResourceClaim CLAIM
    #   CNAME=template:TEMPLATE  a claim made from the ResourceClaimTemplate
    # Every CONTAINER is a container of the pod and the claims it uses:
    #   CTR=REF[,REF...]         REF: a claim CNAME (every result of it) or
    #                            CNAME/REQUEST (only the result of REQUEST)
    #   CTR                      no claims
    # Without CONTAINERs the pod has one container "main" with every claim.
    # Example: dra-pod-yaml split "$VM2_NAME" mem=claim:split -- c-numa=mem/numa c-dax=mem/dax
    local name="$1" node="$2" claims=() ctrs=() a cname src ctr refs ref yaml
    shift 2
    [ -n "$node" ] || error "dra-pod-yaml: no node for pod $name"
    while [ $# -gt 0 ] && [ "$1" != -- ]; do
        claims+=("$1")
        shift
    done
    [ "$1" != -- ] || shift
    ctrs=("$@")
    if [ "${#ctrs[@]}" == 0 ]; then
        refs=""
        for a in "${claims[@]}"; do
            refs+="${refs:+,}${a%%=*}"
        done
        ctrs=("main${refs:+=$refs}")
    fi
    yaml="apiVersion: v1
kind: Pod
metadata:
  name: $name
spec:
  nodeSelector:
    kubernetes.io/hostname: $node
  terminationGracePeriodSeconds: 2
  restartPolicy: Never"
    if [ "${#claims[@]}" -gt 0 ]; then
        yaml+="
  resourceClaims:"
        for a in "${claims[@]}"; do
            cname=${a%%=*} src=${a#*=}
            case "$src" in
                claim:*) yaml+="
  - name: $cname
    resourceClaimName: ${src#claim:}";;
                template:*) yaml+="
  - name: $cname
    resourceClaimTemplateName: ${src#template:}";;
                *) error "dra-pod-yaml: claim '$a' is not CNAME=claim:CLAIM or CNAME=template:TEMPLATE";;
            esac
        done
    fi
    yaml+="
  volumes:
  - name: tools
    configMap:
      name: tools
  containers:"
    for a in "${ctrs[@]}"; do
        ctr=${a%%=*} refs=""
        [[ "$a" != *=* ]] || refs=${a#*=}
        # shellcheck disable=SC2001 # sed indents every line of the runner
        yaml+="
  - name: $ctr
    image: $DRA_TOOLS_IMAGE
    imagePullPolicy: IfNotPresent
    command:
    - sh
    - -c
    - |
$(sed 's/^/      /' <<< "$DRA_RUNNER")
    volumeMounts:
    - name: tools
      mountPath: /tools"
        [ -n "$refs" ] || continue
        yaml+="
    resources:
      claims:"
        for ref in ${refs//,/ }; do
            yaml+="
      - name: ${ref%%/*}"
            [[ "$ref" != */* ]] || yaml+="
        request: ${ref#*/}"
        done
    done
    echo "$yaml"
}

dra-claim-pod-yaml() {
    # Usage: dra-claim-pod-yaml NAME claim|template SOURCE NODE
    #
    # Print pod NAME on Kubernetes node NODE (see dra-pod-yaml) whose claim
    # "mem" is the ResourceClaim SOURCE (claim) or is made from the
    # ResourceClaimTemplate SOURCE (template): one container "main" that
    # uses the claim, the tools of dra-tools-install in /tools, its main
    # process $DRA_RUNNER (see dra-run), no memory limit.
    [ -n "$4" ] || error "dra-claim-pod-yaml: no node for pod $1"
    dra-pod-yaml "$1" "$4" "mem=$2:$3"
}

dra-claim-pod() {
    # Usage: dra-claim-pod VMDIR NAME CLASS SIZE [MODE [read-only]] [SECTION=FIELDS...]
    #
    # Create pod NAME on the node of the VM of VMDIR with a claim of
    # DeviceClass CLASS for SIZE, with the configuration of MODE, read-only
    # and SECTION=FIELDS (see dra-claim-yaml), made from a
    # ResourceClaimTemplate NAME of its own: the claim lives and dies with
    # the pod.
    # Example: dra-claim-pod "$VM" steered cxl-local 128Mi "container=steering: {policy: auto}"
    local vmdir="$1" name="$2"
    shift 2
    dra-apply "$vmdir" <<< "$(dra-claim-yaml template "$name" "$@")
---
$(dra-claim-pod-yaml "$name" template "$name" "$(vmdir-name "$vmdir")")"
}

dra-run() {
    # Usage: dra-run VMDIR POD[/CONTAINER] SCRIPT [TIMEOUT]
    #
    # Run the shell SCRIPT as a child of the main process of a container of
    # dra-pod-yaml (the default container of POD when CONTAINER is not
    # given), so that it inherits the memory policy of that process, and
    # wait until it exits, TIMEOUT seconds at most (default 120). One
    # kubectl exec writes the script for $DRA_RUNNER and waits in the
    # container. COMMAND_OUTPUT has what SCRIPT printed (stdout and
    # stderr), COMMAND_STATUS its exit status (124 on timeout), which is
    # also returned.
    # Example: dra-run "$VM" steered "python3 /tools/numa-touch.py 64 -"
    local vmdir="$1" target="$2" script="$3" tmo="${4:-120}"
    # shellcheck disable=SC2016 # expanded by sh in the container
    dra-exec "$vmdir" "$target" sh -c '
rm -f /tmp/run.rc /tmp/run.out
printf "%s\n" "$1" > /tmp/run.new && mv /tmp/run.new /tmp/run || exit 125
i=0
while [ ! -f /tmp/run.rc ]; do
    if [ "$i" -ge "$2" ]; then
        echo "dra-run: the script did not finish in $(( $2 / 10 )) s" >&2
        exit 124
    fi
    sleep 0.1
    i=$(( i + 1 ))
done
cat /tmp/run.out
exit "$(cat /tmp/run.rc)"' sh "$script" "$(( tmo * 10 ))"
}

dra-shared-claim-yaml() {
    # Usage: dra-shared-claim-yaml NAME SERIAL [SHARES [PARAMETERS [CLASS]]]
    #
    # Print ResourceClaim NAME with one request "mem" of DeviceClass CLASS
    # (default cxl-pool-shared) for the shared device SERIAL. SHARES is the
    # "shares" capacity to request: "1" for one share of the device, ""
    # for none, which is the device exclusively (the request consumes all
    # of its shares). PARAMETERS are the fields of the device section of
    # its configuration, for instance "mode: dax, readOnly: true" ("": no
    # configuration), or, when they start with "device:", "container:" or
    # "pool:", its sections (see dra-memory-config). The selector and the
    # configuration are of the driver of CLASS: $DRA_LOCAL_DRIVER for
    # cxl-local*, else $DRA_POOL_DRIVER.
    # Examples:
    #   dra-shared-claim-yaml writer 0xc1ae0001 "" "mode: block" cxl-pool-persistent
    #   dra-shared-claim-yaml placed 0xc1ee0002 "" "pool: {opaque: {fake-cxl-pool: {numaNode: 1}}}" cxl-pool
    local name="$1" serial="$2" shares="$3" params="$4" class="${5:-cxl-pool-shared}" driver="$DRA_POOL_DRIVER"
    [[ "$class" != cxl-local* ]] || driver="$DRA_LOCAL_DRIVER"
    [[ -z "$params" || "$params" =~ ^(device|container|pool): ]] || params="device: {$params}"
    cat <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: $name
spec:
  devices:
    requests:
    - name: mem
      exactly:
        deviceClassName: $class
        selectors:
        - cel:
            expression: 'device.attributes["$driver"].serial == "$serial"'
EOF
    if [ -n "$shares" ]; then
        cat <<EOF
        capacity:
          requests:
            shares: "$shares"
EOF
    fi
    if [ -n "$params" ]; then
        cat <<EOF
    config:
    - opaque:
        driver: $driver
        parameters: $(dra-memory-config "$params")
EOF
    fi
}

dra-serial-claim-pod() {
    # Usage: dra-serial-claim-pod VMDIR NAME SERIAL SHARES PARAMETERS CLASS [SCRIPT]
    #
    # Create ResourceClaim NAME of DeviceClass CLASS for the device SERIAL
    # with SHARES ("1" a share, "" the whole device) and the fields
    # PARAMETERS of the device section of its MemoryConfig (see
    # dra-shared-claim-yaml), and pod NAME on the node of VMDIR whose
    # container "main" uses the claim: it runs the shell SCRIPT
    # (dra-script-pod-yaml), or the runner of dra-pod-yaml without SCRIPT.
    # Example: dra-serial-claim-pod "$VM2" rw-a 0xc1ae0001 1 "mode: fs, readOnly: false" cxl-pool-persistent
    local pod
    if [ $# -ge 7 ]; then
        pod=$(dra-script-pod-yaml "$2" "$1" "$2" "$7")
    else
        pod=$(dra-pod-yaml "$2" "$(vmdir-name "$1")" "mem=claim:$2")
    fi
    dra-apply "$OUTPUT_DIR" <<< "$(dra-shared-claim-yaml "$2" "$3" "$4" "$5" "$6")
---
$pod"
}

dra-claim-pod-delete() {
    # Usage: dra-claim-pod-delete NAME...
    #
    # Delete the pods NAME, then their claims NAME, and wait until they are
    # gone.
    local n
    for n in "$@"; do
        dra-delete-pod "$OUTPUT_DIR" "$n"
    done
    for n in "$@"; do
        dra-delete "$OUTPUT_DIR" resourceclaim "$n" --wait=true --timeout=60s
    done
}

dra-pending-assert() {
    # Usage: dra-pending-assert POD...
    #
    # Fail the test unless every pod POD is Pending without a node: not
    # bound, its claim not usable anywhere yet.
    local p
    for p in "$@"; do
        dra-json "$OUTPUT_DIR" pod -n "$DRA_NS" "$p"
        dra-py assert "$COMMAND_OUTPUT" "j['status']['phase'] == 'Pending' and not j['spec'].get('nodeName')" ||
            error "$p is not Pending without a node: phase $(dra-value "$COMMAND_OUTPUT" "j['status']['phase']"), node $(dra-value "$COMMAND_OUTPUT" "j['spec'].get('nodeName')")"
        echo "$p: Pending, not bound to a node"
    done
}

dra-pod-event-wait() {
    # Usage: dra-pod-event-wait POD TEXT... [-- TIMEOUT]
    #
    # Wait until an Event of pod POD has every TEXT (fixed strings) in its
    # message, TIMEOUT seconds at most (default 120): kubelet reports so
    # that the driver refused to prepare its claim. Fail the test on
    # timeout.
    local pod="$1" texts=() tmo=120 expr
    shift
    while [ $# -gt 0 ]; do
        if [ "$1" == -- ]; then
            tmo="$2"
            break
        fi
        texts+=("$1")
        shift
    done
    expr=$(python3 -c 'import sys; print("any(all(t in (e.get(\"message\") or \"\") for t in %r) for e in items)" % sys.argv[1:])' "${texts[@]}")
    dra-wait "$OUTPUT_DIR" "$pod-event" "events -n $DRA_NS --field-selector involvedObject.name=$pod" "$expr" "$tmo"
    dra-kubectl "$OUTPUT_DIR" "get events -n $DRA_NS --field-selector involvedObject.name=$pod"
}

dra-unschedulable-wait() {
    # Usage: dra-unschedulable-wait VMDIR POD
    #
    # Wait until pod POD in $DRA_NS is not scheduled because its claims
    # cannot be allocated (PodScheduled False, reason Unschedulable), 60 s
    # at most, and print the message of the scheduler. Fail the test on
    # timeout.
    dra-wait "$1" "$2-unschedulable" "pod -n $DRA_NS $2" \
        "any(c['type'] == 'PodScheduled' and c['status'] == 'False' and c.get('reason') == 'Unschedulable' and 'cannot allocate all claims' in c.get('message', '') for c in (j.get('status') or {}).get('conditions') or [])" 60
    vmdir-command "$1" "kubectl get pod -n $DRA_NS $2 -o jsonpath='{.status.conditions[?(@.type==\"PodScheduled\")].message}'"
}

dra-unmatched() {
    # Usage: dra-unmatched NAME SERIAL CLASS
    #
    # Fail the test unless a claim of DeviceClass CLASS for the device
    # SERIAL cannot be allocated: its pod NAME (on the VM of the test)
    # stays unschedulable, the claim without an allocation. Delete both.
    dra-apply "$OUTPUT_DIR" <<< "$(dra-shared-claim-yaml "$1" "$2" "" "" "$3")
---
$(dra-pod-yaml "$1" "$(vmdir-name "$OUTPUT_DIR")" "mem=claim:$1")"
    dra-unschedulable-wait "$OUTPUT_DIR" "$1"
    dra-json "$OUTPUT_DIR" resourceclaim -n "$DRA_NS" "$1"
    dra-assert "$COMMAND_OUTPUT" "not (j.get('status') or {}).get('allocation')"
    dra-claim-pod-delete "$1"
    echo "class $3 does not select $2"
}

# shellcheck disable=SC2001 # sed indents every line of SCRIPT
dra-script-pod-yaml() {
    # Usage: dra-script-pod-yaml NAME VMDIR CLAIM SCRIPT [TYPE:PATH...]
    #
    # Print pod NAME on the node of VMDIR, restartPolicy Never, with
    # ResourceClaim CLAIM as its claim "mem" and the tools of
    # dra-tools-install in /tools, running the shell SCRIPT in its
    # container "main" (image $DRA_TOOLS_IMAGE), then idling until it is
    # deleted. Every TYPE:PATH is a hostPath volume of the node, mounted
    # read-only at the same path: TYPE is its hostPath type (File,
    # Directory).
    # Example: dra-script-pod-yaml reader "$VM" shared-ro 'ls /opt/x' Directory:/opt/x
    local name="$1" node claim="$3" script="$4" volumes="" mounts="" n=0 spec
    node=$(basename "$2")
    shift 4
    for spec in "$@"; do
        volumes+="
  - name: host$n
    hostPath:
      path: ${spec#*:}
      type: ${spec%%:*}"
        mounts+="
    - name: host$n
      mountPath: ${spec#*:}
      readOnly: true"
        n=$(( n + 1 ))
    done
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
spec:
  nodeSelector:
    kubernetes.io/hostname: $node
  terminationGracePeriodSeconds: 2
  restartPolicy: Never
  resourceClaims:
  - name: mem
    resourceClaimName: $claim
  volumes:
  - name: tools
    configMap:
      name: tools$volumes
  containers:
  - name: main
    image: $DRA_TOOLS_IMAGE
    imagePullPolicy: IfNotPresent
    command:
    - sh
    - -c
    - |
$(sed 's/^/      /' <<< "$script")
      trap "exit 0" TERM; sleep infinity & wait
    volumeMounts:
    - name: tools
      mountPath: /tools$mounts
    resources:
      claims:
      - name: mem
EOF
}

###
### Containers of pods, and leases of pool memory
###

dra-pod-container() {
    # Usage: dra-pod-container POD[/CONTAINER]
    #
    # Print "POD CONTAINER" (CONTAINER empty if not given).
    local pod="${1%%/*}" container=""
    [[ "$1" == */* ]] && container="${1#*/}"
    echo "$pod $container"
}

dra-exec() {
    # Usage: dra-exec VMDIR POD[/CONTAINER] COMMAND [ARGS...]
    #
    # Run COMMAND with ARGS in a container of pod POD in $DRA_NS (kubectl
    # exec; the default container when CONTAINER is not given). The
    # arguments reach the container as they are, one argv word each.
    # COMMAND_OUTPUT has what COMMAND printed to stdout, DRA_EXEC_STDERR what
    # it printed to stderr (both are shown), COMMAND_STATUS its exit status,
    # which is also returned.
    # Example: dra-exec "$VM" requester/main cxl-request request --size 512Mi
    local vmdir="$1" pod container cmd errfile
    read -r pod container <<< "$(dra-pod-container "$2")"
    shift 2
    cmd="kubectl exec -n $DRA_NS $pod${container:+ -c $container} --$(printf ' %q' "$@")"
    echo -e "\e[38;5;13mroot@$(vmdir-label "$vmdir")>\e[0m $cmd"
    errfile=$(mktemp)
    COMMAND_OUTPUT=$(vmdir-command-q "$vmdir" "$cmd" 2>"$errfile")
    COMMAND_STATUS=$?
    DRA_EXEC_STDERR=$(cat "$errfile")
    rm -f "$errfile"
    [ -z "$COMMAND_OUTPUT" ] || echo "$COMMAND_OUTPUT"
    [ -z "$DRA_EXEC_STDERR" ] || echo "(stderr) $DRA_EXEC_STDERR"
    [ "$COMMAND_STATUS" == 0 ] || echo "(exit status $COMMAND_STATUS)"
    return "$COMMAND_STATUS"
}

dra-last-json() {
    # Usage: dra-last-json TEXT
    #
    # Print the last line of TEXT that is a JSON object, for instance the
    # response of "cxl-request request --size 512Mi" among other output,
    # also after the "cxl-request: " prefix of its lines on stderr. Return 1
    # if there is none.
    python3 -c '
import json, sys
last = None
for line in sys.stdin.read().splitlines():
    line = line.strip()
    if line.startswith("cxl-request: "):
        line = line[len("cxl-request: "):]
    if not line.startswith("{"):
        continue
    try:
        last = json.loads(line)
    except ValueError:
        continue
if last is None:
    sys.exit(1)
print(json.dumps(last))' <<< "$1"
}

dra-env-assert() {
    # Usage: dra-env-assert LABEL TEXT KEY FIELD=VALUE...
    #
    # Fail the test unless the lines CXL_KEY_FIELD=VALUE of TEXT (what env
    # or "cxl-request env" printed) are exactly the FIELD=VALUE pairs given,
    # no field missing and none more, the environment of one memory device
    # (docs/memory/cxl/usage.md). KEY is the upper-case hex serial of a memdev,
    # REGION<n> or DRAM. VALUE is a shell pattern, for instance
    # DAX=/dev/dax*.
    # Example: dra-env-assert pooled-pod "$LOG" C1EE0001 SERIAL=0xc1ee0001 SHARED=0 MODE=numa SIZE=536870912 NUMA_NODE=2
    local label="$1" text="$2" key="$3"
    shift 3
    python3 -c '
import fnmatch, sys
text, key, want = sys.argv[1], sys.argv[2], sys.argv[3:]
prefix = "CXL_%s_" % key
got = {}
for line in text.splitlines():
    name, sep, value = line.strip().partition("=")
    # A field starts with a letter: CXL_C100E2E1_2_SIZE is another key.
    if sep and name.startswith(prefix) and name[len(prefix):][:1].isalpha():
        got[name[len(prefix):]] = value
want = dict(w.partition("=")[::2] for w in want)
bad = sorted(set(got) ^ set(want)) + sorted(f for f in set(got) & set(want) if not fnmatch.fnmatchcase(got[f], want[f]))
if bad:
    print("got:  %s\nwant: %s\nfields that differ: %s" % (
        " ".join("%s%s=%s" % (prefix, f, got[f]) for f in sorted(got)),
        " ".join("%s%s=%s" % (prefix, f, want[f]) for f in sorted(want)), " ".join(bad)))
    sys.exit(1)' "$text" "$key" "$@" ||
        error "$label: the environment of CXL_${key}_ is not $*"
    echo "env ok: $label: CXL_${key}_: $*"
}

dra-env-value() {
    # Usage: dra-env-value TEXT NAME
    #
    # Print the value of the line NAME=VALUE of TEXT (what env or
    # "cxl-request env" printed). Return 1 if there is none.
    local value
    value=$(sed -n "s/^$2=//p" <<< "$1" | head -n 1)
    [ -n "$value" ] || grep -q "^$2=" <<< "$1" || return 1
    echo "$value"
}

dra-companion-meta-check() {
    # Usage: dra-companion-meta-check JSON LEASE REQUESTER_UID NODE NS/POD/CONTAINER
    #
    # Fail the test unless the companion pod or claim of lease LEASE in
    # JSON has exactly the labels of the lease ($DRA_LEASE_LABEL=LEASE,
    # $DRA_REQUESTER_LABEL=REQUESTER_UID, $DRA_NODE_LABEL=NODE) and the
    # annotation $DRA_REQUESTER_ANNOTATION=NS/POD/CONTAINER, the requesting
    # container.
    dra-assert "$1" "j['metadata']['labels'] == {'$DRA_LEASE_LABEL': '$2', '$DRA_REQUESTER_LABEL': '$3', '$DRA_NODE_LABEL': '$4'}"
    dra-assert "$1" "j['metadata']['annotations']['$DRA_REQUESTER_ANNOTATION'] == '$5'"
}

dra-lease-template-yaml() {
    # Usage: dra-lease-template-yaml [NAME]
    #
    # Print the ResourceClaimTemplate NAME (default cxl-lease) with one
    # request "lease" of DeviceClass cxl-lease: a pod with a claim from it
    # may lease memory and filesystems for its containers that use the
    # claim, and the claim lives and dies with the pod.
    cat <<EOF2
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: ${1:-cxl-lease}
spec:
  spec:
    devices:
      requests:
      - name: lease
        exactly:
          deviceClassName: cxl-lease
EOF2
}

dra-lease-pod-start() {
    # Usage: dra-lease-pod-start VMDIR NAME NODE [TEMPLATE]
    #
    # Create pod NAME of dra-pod-yaml on Kubernetes node NODE with a claim
    # "lease" from the ResourceClaimTemplate TEMPLATE (default cxl-lease,
    # see dra-lease-template-yaml) in its container main, and wait until
    # it runs. DRA_POD_UID is the uid of the pod, DRA_LEASE_CLAIM_UID the
    # uid of its lease claim.
    local vmdir="$1" name="$2"
    dra-apply "$vmdir" <<< "$(dra-pod-yaml "$name" "$3" "lease=template:${4:-cxl-lease}")"
    dra-pod-wait "$vmdir" "$name" Running 120
    dra-json "$vmdir" pod -n "$DRA_NS" "$name"
    DRA_POD_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
    dra-json "$vmdir" resourceclaim -n "$DRA_NS" "$(dra-value "$COMMAND_OUTPUT" "j['status']['resourceClaimStatuses'][0]['resourceClaimName']")"
    DRA_LEASE_CLAIM_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
}

dra-request() {
    # Usage: dra-request VMDIR POD[/CONTAINER] ARGS...
    #
    # Run "cxl-request ARGS..." in the container (dra-exec). DRA_REQ_STATUS
    # is its exit status, which is also returned, DRA_REQ_OUT what it
    # printed (stdout, then stderr), DRA_REQ_JSON the last JSON line of
    # them ("{}" if none): with --exec the lease's is on stderr.
    local vmdir="$1" target="$2"
    shift 2
    dra-exec "$vmdir" "$target" "$DRA_REQUEST_CTR_BIN" "$@"
    DRA_REQ_STATUS=$COMMAND_STATUS
    DRA_REQ_OUT="$COMMAND_OUTPUT
$DRA_EXEC_STDERR"
    # shellcheck disable=SC2034 # for the tests
    DRA_REQ_JSON=$(dra-last-json "$DRA_REQ_OUT") || DRA_REQ_JSON="{}"
    return "$DRA_REQ_STATUS"
}

dra-request-refused() {
    # Usage: dra-request-refused VMDIR POD[/CONTAINER] STATUS TEXT ARGS...
    #
    # Fail the test unless "cxl-request ARGS..." in the container exits
    # with STATUS and prints TEXT, a fixed string.
    local vmdir="$1" target="$2" status="$3" text="$4"
    shift 4
    dra-request "$vmdir" "$target" "$@"
    [ "$DRA_REQ_STATUS" == "$status" ] || error "cxl-request $* exited $DRA_REQ_STATUS, expected $status"
    grep -qF -- "$text" <<< "$DRA_REQ_OUT" || error "cxl-request $* did not print '$text'"
    echo "refused as expected: cxl-request $*: $text"
}

dra-lease-new() {
    # Usage: dra-lease-new LEASE
    #
    # Record lease id LEASE in DRA_LEASE_IDS. Fail the test if LEASE is
    # not a lease id or the test has seen it before.
    local l
    [[ "$1" =~ ^[0-9a-f]{8}$ ]] || error "lease id '$1' is not 8 hex digits"
    for l in "${DRA_LEASE_IDS[@]}"; do
        [ "$l" != "$1" ] || error "lease id $1 was used before"
    done
    DRA_LEASE_IDS+=("$1")
}

dra-leases() {
    # Usage: dra-leases VMDIR POD[/CONTAINER]
    #
    # Run "cxl-request list" in the container (the default container of
    # POD when not given) and print its leases as one JSON list, whatever
    # form cxl-request printed them in (a list, {"leases": [...]}, or one
    # object per line). COMMAND_OUTPUT and DRA_LEASES have the list. Fail
    # the test if cxl-request fails.
    local vmdir="$1" target="$2"
    dra-exec "$vmdir" "$target" "$DRA_REQUEST_CTR_BIN" list ||
        command-error "cxl-request list failed in $target"
    DRA_LEASES=$(python3 -c '
import json, sys
text = sys.stdin.read().strip()
leases = []
try:
    j = json.loads(text) if text else []
    if isinstance(j, dict):
        j = j.get("leases", [j] if "lease" in j else [])
    leases = j or []
except ValueError:
    for line in text.splitlines():
        line = line.strip()
        if line.startswith("{"):
            leases.append(json.loads(line))
print(json.dumps(leases))' <<< "$COMMAND_OUTPUT") ||
        error "dra-leases: cannot parse the output of cxl-request list: $COMMAND_OUTPUT"
    COMMAND_OUTPUT="$DRA_LEASES"
    echo "leases of $target: $DRA_LEASES"
}

dra-container-pid-sh() {
    # Usage: dra-container-pid-sh POD CONTAINER
    #
    # Print a shell command for a VM that sets id to the id of container
    # CONTAINER of pod POD in $DRA_NS and pid to the pid of its init
    # process (crictl inspect .info.pid), and fails if either is missing.
    local pod="$1" container="$2"
    echo "id=\$(kubectl get pod -n $DRA_NS $pod -o jsonpath='{.status.containerStatuses[?(@.name==\"$container\")].containerID}') && id=\${id#*://} && [ -n \"\$id\" ] &&
        pid=\$(crictl inspect -o go-template --template '{{.info.pid}}' \$id 2>/dev/null) && [ -n \"\$pid\" ]"
}

dra-container-cgroup() {
    # Usage: dra-container-cgroup VMDIR POD CONTAINER
    #
    # Print the cgroup v2 directory of container CONTAINER of pod POD in
    # $DRA_NS, in the VM of VMDIR: the cgroup of its init process (crictl
    # inspect .info.pid, /proc/PID/cgroup). Processes of "kubectl exec" run
    # in the same cgroup.
    local vmdir="$1" pod="$2" container="$3"
    vmdir-command-q "$vmdir" "$(dra-container-pid-sh "$pod" "$container") &&
        cg=\$(sed -n 's/^0:://p' /proc/\$pid/cgroup) && [ -d \"/sys/fs/cgroup\$cg\" ] && echo \"/sys/fs/cgroup\$cg\""
}

dra-container-read() {
    # Usage: dra-container-read VMDIR POD CONTAINER FILE
    #
    # Print cgroup FILE of container CONTAINER of pod POD in $DRA_NS, read
    # in the VM of VMDIR, quietly. Return non-zero if the container or the
    # file cannot be found.
    local cg
    cg=$(dra-container-cgroup "$1" "$2" "$3") || return 1
    vmdir-command-q "$1" "cat $cg/$4"
}

dra-mems-wait() {
    # Usage: dra-mems-wait VMDIR POD CONTAINER NODE
    #
    # Wait until cpuset.mems of container CONTAINER of pod POD in $DRA_NS
    # has NUMA node NODE, 30 s at most, and print its cpuset.mems,
    # cpuset.mems.effective and the Mems_allowed_list of a process in it.
    # Fail the test on timeout.
    local vmdir="$1" pod="$2" container="$3" node="$4" mems
    retry-until --timeout 30 --interval 1 --message "cpuset.mems of container $container of $pod has node $node" \
        'mems=$(dra-container-read "$vmdir" "$pod" "$container" cpuset.mems) && dra-numa-nodes-has "$mems" "$node"' ||
        error "cpuset.mems of container $container of pod $pod is '$(dra-container-read "$vmdir" "$pod" "$container" cpuset.mems)', expected that it has node $node"
    echo "$pod/$container: cpuset.mems $mems (has node $node), effective $(dra-container-read "$vmdir" "$pod" "$container" cpuset.mems.effective)"
    dra-exec "$vmdir" "$pod/$container" grep Mems_allowed_list /proc/self/status ||
        command-error "cannot read Mems_allowed_list in container $container of pod $pod"
}

dra-mems-allowed() {
    # Usage: dra-mems-allowed VMDIR POD/CONTAINER
    #
    # Print the Mems_allowed_list of a process in the container, as the
    # container sees it. Fail the test if it cannot be read.
    dra-exec "$1" "$2" grep Mems_allowed_list /proc/self/status >&2 ||
        command-error "cannot read Mems_allowed_list in $2"
    awk '{print $2}' <<< "$COMMAND_OUTPUT"
}

dra-mems-once() {
    # Usage: dra-mems-once LABEL VMDIR POD CONTAINER DRAM VAR
    #
    # Check that cpuset.mems of container CONTAINER of pod POD is the value
    # of variable VAR, or, when that is empty, the DRAM nodes DRAM (a node
    # list), and set VAR to it: an empty cpuset.mems changes once, when the
    # driver fences its first CXL node, and then stays.
    local label="$1" vmdir="$2" pod="$3" ctr="$4" dram="$5" var="$6" mems
    mems=$(dra-container-read "$vmdir" "$pod" "$ctr" cpuset.mems) ||
        error "$label: cannot read cpuset.mems of container $ctr of $pod"
    [ "$mems" == "${!var}" ] && return 0
    [ -z "${!var}" ] ||
        error "$label: cpuset.mems of container $ctr of $pod changed from '${!var}' to '$mems'"
    dra-numa-nodes-eq "$mems" "$dram" ||
        error "$label: cpuset.mems of container $ctr of $pod is '$mems', expected '' or its DRAM nodes $dram"
    echo "$label: cpuset.mems of container $ctr of $pod changed from '' to its DRAM nodes '$mems'"
    printf -v "$var" '%s' "$mems"
}

dra-mems-fenced() {
    # Usage: dra-mems-fenced VMDIR NODE [NS/POD/CONTAINER...]
    #
    # Wait until NUMA node NODE is in cpuset.mems.effective of the listed
    # containers and of no other running container of the node of the VM
    # of VMDIR, 30 s at most, and print the cpusets of every running
    # container (see dra-mems-table). Containers are those of crictl ps:
    # pod sandboxes are not containers. Fail the test on timeout, naming
    # the offenders: a container that has the node without being listed,
    # one that is listed and lacks it, a listed one that does not run.
    # Example: dra-mems-fenced "$VM" "$CXL_NODE" "$DRA_NS/pooled-pod/main"
    local vmdir="$1" node="$2" label wanted table="" status=""
    shift 2
    local ctrs=("$@")
    label=$(vmdir-label "$vmdir")
    if [ "${#ctrs[@]}" == 0 ]; then
        wanted="of no container"
    else
        wanted="of ${ctrs[*]} and of no other container"
    fi
    retry-until --timeout 30 --interval 1 --message "$label: node $node in cpuset.mems.effective $wanted" \
        'table=$(dra-mems-table "$vmdir" "$node" "${ctrs[@]}"); status=$?; [ "$status" == 0 ]'
    echo "$table"
    case "$status" in
        0) return 0;;
        3) error "$label: node $node is allowed only in ${ctrs[*]:-no container}, offenders:
$(grep -F '  <- ' <<< "$table")";;
        *) error "dra-mems-fenced: cannot list the containers of $label and their cpusets (status $status)";;
    esac
}

dra-mems-table() {
    # Usage: dra-mems-table VMDIR NODE [NS/POD/CONTAINER...]
    #
    # Print one line per running container of the node of the VM of VMDIR,
    # sorted, read in one go:
    #   NS/POD/CONTAINER cpuset.mems MEMS effective EFFECTIVE[  <- REASON]
    # MEMS and EFFECTIVE are its cpuset.mems and cpuset.mems.effective
    # ('""' when empty). REASON marks an offender: "has node NODE" (not
    # listed), "lacks node NODE" (listed), "cannot read its cpuset: ...",
    # and "not running" for a listed container that does not run (MEMS and
    # EFFECTIVE are "-").
    # Return 0 if there is no offender, 3 if there is one, other values if
    # the containers cannot be listed.
    local vmdir="$1" prog
    shift
    # The cgroup of a container is that of its init process (crictl
    # inspect .info.pid, /proc/PID/cgroup). A container that exits during
    # the listing is left out.
    prog='
import json, os, subprocess, sys
def nodes(text):
    out = set()
    for part in text.strip().split(","):
        if part.strip():
            lo, _, hi = part.strip().partition("-")
            out.update(range(int(lo), int(hi or lo) + 1))
    return out
def crictl(*args):
    return subprocess.run(["crictl"] + list(args), capture_output=True, text=True)
def inspected(text):
    j = json.loads(text)
    return j if isinstance(j, list) else [j]
node, allowed = int(sys.argv[1]), set(sys.argv[2:])
ps = crictl("ps", "-q")
if ps.returncode != 0:
    sys.exit("crictl ps failed: " + ps.stderr.strip())
ids = ps.stdout.split()
infos = []
if ids:
    r = crictl("inspect", "-o", "json", *ids)
    if r.returncode == 0:
        infos = inspected(r.stdout)
    else:
        # A container was removed after the listing: the others one by one.
        for i in ids:
            r = crictl("inspect", "-o", "json", i)
            if r.returncode == 0:
                infos += inspected(r.stdout)
rows, seen = [], set()
for c in infos:
    st = c.get("status") or {}
    if st.get("state") != "CONTAINER_RUNNING":
        continue
    lb = st.get("labels") or {}
    name = "/".join([lb.get("io.kubernetes.pod.namespace", "-"), lb.get("io.kubernetes.pod.name", "-"),
                     lb.get("io.kubernetes.container.name") or (st.get("metadata") or {}).get("name", "-")])
    pid = (c.get("info") or {}).get("pid") or 0
    try:
        with open("/proc/%d/cgroup" % pid) as f:
            cg = "/sys/fs/cgroup" + [l[3:].strip() for l in f if l.startswith("0::")][0]
        with open(cg + "/cpuset.mems") as f:
            mems = f.read().strip()
        with open(cg + "/cpuset.mems.effective") as f:
            eff = f.read().strip()
    except (OSError, IndexError) as e:
        if pid == 0 or not os.path.exists("/proc/%d" % pid):
            continue
        seen.add(name)
        rows.append((name, "?", "?", "cannot read its cpuset: %s" % e))
        continue
    seen.add(name)
    has = node in nodes(eff)
    why = ""
    if has and name not in allowed:
        why = "has node %d" % node
    elif not has and name in allowed:
        why = "lacks node %d" % node
    rows.append((name, mems or "\"\"", eff or "\"\"", why))
rows += [(name, "-", "-", "not running") for name in allowed - seen]
w = max([len(r[0]) for r in rows] + [1])
for name, mems, eff, why in sorted(rows):
    print(("%-*s cpuset.mems %-5s effective %-5s%s" % (w, name, mems, eff, "  <- " + why if why else "")).rstrip())
sys.exit(3 if any(r[3] for r in rows) else 0)'
    vmdir-command-q "$vmdir" "python3 -c $(printf %q "$prog")$(printf ' %q' "$@")"
}

dra-container-anon() {
    # Usage: dra-container-anon VMDIR POD CONTAINER NODE
    #
    # Print the anonymous memory of container CONTAINER of pod POD on NUMA
    # node NODE in bytes ("anon N<NODE>=" of memory.numa_stat, 0 if the
    # node is not listed), quietly.
    local vmdir="$1" pod="$2" container="$3" node="$4" cg
    cg=$(dra-container-cgroup "$vmdir" "$pod" "$container") || return 1
    vmdir-command-q "$vmdir" "cat $cg/memory.numa_stat" |
        awk -v n="N$node" '$1 == "anon" { for (i = 2; i <= NF; i++) { split($i, kv, "="); if (kv[1] == n) v = kv[2] } } END { print v + 0 }'
}

dra-container-mempolicy() {
    # Usage: dra-container-mempolicy VMDIR POD CONTAINER
    #
    # Print the memory policy of the main process of container CONTAINER
    # of pod POD in $DRA_NS (its init process, crictl inspect .info.pid),
    # read in the VM of VMDIR, quietly: the policies of the mappings in its
    # numa_maps, which show the policy of the process where a mapping has
    # none of its own, distinct, space-separated, for instance "default",
    # "prefer:2" or "interleave:0-2". Return non-zero if it cannot be read.
    local vmdir="$1" pod="$2" container="$3"
    vmdir-command-q "$vmdir" "$(dra-container-pid-sh "$pod" "$container") &&
        p=\$(awk '{print \$2}' /proc/\$pid/numa_maps | sort -u | tr '\n' ' ') && [ -n \"\$p\" ] && echo \"\${p% }\""
}

dra-memory-state() {
    # Usage: dra-memory-state VMDIR POD CONTAINER
    #
    # Set DRA_MEM_HIGH to memory.high of container CONTAINER of pod POD and
    # DRA_MEMPOLICY to the memory policy of its main process (see
    # dra-container-mempolicy). Return non-zero if either cannot be read.
    DRA_MEM_HIGH=$(dra-container-read "$1" "$2" "$3" memory.high) || return 1
    DRA_MEMPOLICY=$(dra-container-mempolicy "$1" "$2" "$3") || return 1
}

dra-memory-wait() {
    # Usage: dra-memory-wait VMDIR POD CONTAINER steered|fenced|none [STEERING]
    #
    # Wait until the driver on the node of the VM of VMDIR has logged how
    # it manages the memory of container CONTAINER of pod POD in $DRA_NS
    # ("NRI CreateContainer: ctr=NS/POD/CONTAINER: memory MANAGEMENT
    # (steering STEERING, fencing ..."; STEERING is the effective policy
    # and its origin, for instance "none (default)" or "auto from claim
    # NS/NAME request mem"; any if not given), and check the container:
    #   steered        memory.high is not max and the main process has a
    #                  memory policy (waits until the steering has started)
    #   fenced, none   memory.high is max and the main process has the
    #                  default memory policy
    # DRA_MEM_HIGH and DRA_MEMPOLICY are what was read last. Fail the test
    # on timeout or a mismatch.
    # Example: dra-memory-wait "$VM" numa-a main fenced "none (default)"
    local vmdir="$1" pod="$2" container="$3" mgmt="$4" steering="$5" ctr label
    ctr="$DRA_NS/$pod/$container"
    label=$(vmdir-label "$vmdir")
    case "$mgmt" in
        steered|fenced|none) ;;
        *) error "dra-memory-wait: unknown management '$mgmt'";;
    esac
    dra-driver-log-wait "$vmdir" "NRI CreateContainer: ctr=$ctr: memory $mgmt (${steering:+steering $steering}" 60
    DRA_MEM_HIGH="" DRA_MEMPOLICY=""
    if [ "$mgmt" == steered ]; then
        retry-until --timeout 30 --interval 1 --message "$label: $ctr is steered: memory.high is not max, its main process has a memory policy" \
            'dra-memory-state "$vmdir" "$pod" "$container" && [ "$DRA_MEM_HIGH" != max ] && [ "$DRA_MEMPOLICY" != default ]' ||
            error "$label: $ctr is not steered: memory.high '$DRA_MEM_HIGH', memory policy of its main process '$DRA_MEMPOLICY'"
    else
        dra-memory-state "$vmdir" "$pod" "$container" ||
            error "$label: cannot read memory.high or the memory policy of the main process of $ctr"
        [ "$DRA_MEM_HIGH" == max ] && [ "$DRA_MEMPOLICY" == default ] ||
            error "$label: $ctr is $mgmt, not steered, but memory.high is '$DRA_MEM_HIGH' and the memory policy of its main process '$DRA_MEMPOLICY', expected max and default"
    fi
    echo "memory ok: $label: $ctr $mgmt${steering:+, steering $steering}: memory.high $DRA_MEM_HIGH, memory policy of the main process $DRA_MEMPOLICY"
}

dra-container-devices() {
    # Usage: dra-container-devices VMDIR POD CONTAINER
    #
    # Print what the device cgroup of container CONTAINER of pod POD allows,
    # in a form to compare before and after: the device rules of its runtime
    # spec (crictl inspect, line "spec: ...") and the distinct eBPF device
    # programs attached to its cgroup (line "bpf: ...", hashes of their code,
    # bpftool). Measured in VM2 (containerd, systemd cgroup driver): the
    # first runtime update of a container (crictl update, NRI
    # UpdateContainers) attaches one more, different program; later updates
    # add none. Compare the spec line to see whether the rules changed.
    # Quietly.
    local vmdir="$1" pod="$2" container="$3" cg
    cg=$(dra-container-cgroup "$vmdir" "$pod" "$container") || return 1
    vmdir-command-q "$vmdir" "$(dra-container-pid-sh "$pod" "$container")
        echo \"spec: \$(crictl inspect \$id 2>/dev/null | jq -c .info.runtimeSpec.linux.resources.devices)\"
        echo \"bpf: \$(for p in \$(bpftool cgroup show $cg 2>/dev/null | awk '\$2 == \"cgroup_device\" {print \$1}'); do bpftool prog dump xlated id \$p | sha256sum | cut -c1-16; done | sort -u | tr '\\n' ' ')\""
}

dra-pod-claim() {
    # Usage: dra-pod-claim VMDIR POD [NAME]
    #
    # Print the name of the ResourceClaim of pod POD in $DRA_NS, the claim
    # that the pod calls NAME (default: its first claim): the claim made
    # from its template (status.resourceClaimStatuses), or the claim it
    # references. Return 1 if there is none yet.
    local vmdir="$1" pod="$2" name="$3" json claim
    json=$(vmdir-command-q "$vmdir" "kubectl get pod -n $DRA_NS $pod -o json") || return 1
    claim=$(dra-py eval "$json" "next((s.get('resourceClaimName') for s in (j.get('status') or {}).get('resourceClaimStatuses') or [] if '$name' in ('', s.get('name'))), None) or next((c.get('resourceClaimName') for c in j['spec'].get('resourceClaims') or [] if '$name' in ('', c.get('name'))), None) or ''") || return 1
    [ -n "$claim" ] || return 1
    echo "$claim"
}

dra-numa-nodes-py() {
    # Usage: dra-numa-nodes-py has|eq|sub LIST ARG
    #
    # has: return 0 if node ARG is in the node list LIST ("0-1,3").
    # eq:  return 0 if the node lists LIST and ARG have the same nodes
    #      ("0-2" and "0-1,2": the kernel joins ranges).
    # sub: return 0 if LIST has nodes and all of them are in the node list
    #      ARG.
    python3 -c '
import sys
def nodes(text):
    out = set()
    for part in text.strip().split(","):
        if part:
            lo, _, hi = part.partition("-")
            out.update(range(int(lo), int(hi or lo) + 1))
    return out
op, a, b = sys.argv[1:4]
if op == "has":
    ok = int(b) in nodes(a)
elif op == "sub":
    ok = bool(nodes(a)) and nodes(a) <= nodes(b)
else:
    ok = nodes(a) == nodes(b)
sys.exit(0 if ok else 1)' "$1" "$2" "$3"
}

dra-numa-nodes-has() {
    # Usage: dra-numa-nodes-has LIST NODE
    #
    # Return 0 if NUMA node NODE is in the node list LIST ("0-1,3").
    dra-numa-nodes-py has "$1" "$2"
}

dra-numa-nodes-eq() {
    # Usage: dra-numa-nodes-eq LIST1 LIST2
    #
    # Return 0 if the node lists LIST1 and LIST2 have the same nodes.
    dra-numa-nodes-py eq "$1" "$2"
}

dra-numa-nodes-subset() {
    # Usage: dra-numa-nodes-subset LIST1 LIST2
    #
    # Return 0 if the node list LIST1 has nodes and every one of them is in
    # the node list LIST2.
    dra-numa-nodes-py sub "$1" "$2"
}

dra-touch-start() {
    # Usage: dra-touch-start VMDIR POD/CONTAINER MIB NODE
    #
    # Start numa-touch.py (dra-tools-install) in the background in
    # container CONTAINER of pod POD in $DRA_NS (kubectl exec: not a child
    # of the main process, see dra-run): MIB MiB that prefer NUMA node NODE
    # ("-": the policy of the process), held until dra-touch-stop. Wait
    # until it has touched every
    # page. DRA_TOUCH_PID is its pid in the container, DRA_TOUCH_LOG its
    # output then, with the pages per node. Fail the test if it fails.
    local vmdir="$1" target="$2" mib="$3" node="$4" pod container
    read -r pod container <<< "$(dra-pod-container "$target")"
    DRA_TOUCH_VM="$vmdir"
    DRA_TOUCH_TARGET="$target"
    DRA_TOUCH_EXEC="kubectl exec -n $DRA_NS $pod${container:+ -c $container} --"
    DRA_TOUCH_PID=""
    # shellcheck disable=SC2016 # $1 and $2 are expanded by sh in the container
    dra-exec "$vmdir" "$target" sh -c 'setsid python3 /tools/numa-touch.py --hold "$1" "$2" > /tmp/numa-touch.log 2>&1 < /dev/null &' sh "$mib" "$node" ||
        command-error "cannot start numa-touch.py in $target"
    retry-until --timeout 60 --interval 1 --message "numa-touch.py in $target has touched $mib MiB" \
        'vmdir-command-q "$vmdir" "$DRA_TOUCH_EXEC cat /tmp/numa-touch.log" | grep -qE "^numa-touch: (ready|.*failed)"' ||
        error "numa-touch.py in $target did not finish touching its memory"
    dra-exec "$vmdir" "$target" cat /tmp/numa-touch.log
    grep -q "^numa-touch: ready" <<< "$COMMAND_OUTPUT" || error "numa-touch.py in $target failed: $COMMAND_OUTPUT"
    DRA_TOUCH_PID=$(sed -n 's/^numa-touch: pid \([0-9]*\) .*/\1/p' <<< "$COMMAND_OUTPUT")
    DRA_TOUCH_LOG="$COMMAND_OUTPUT"
}

dra-touch-log-pages() {
    # Usage: dra-touch-log-pages TEXT NODES
    #
    # Print the pages on the NUMA nodes NODES (a node list, "2" or "0-1")
    # in the last "numa-touch: pages" line of TEXT, the output of
    # numa-touch.py (0 for a node that is not listed).
    python3 -c '
import sys
text, spec = sys.argv[1], sys.argv[2]
want = set()
for part in spec.strip().split(","):
    if part:
        lo, _, hi = part.partition("-")
        want.update(range(int(lo), int(hi or lo) + 1))
lines = [l for l in text.splitlines() if l.startswith("numa-touch: pages")]
pages = 0
for w in (lines[-1].split()[2:] if lines else []):
    k, sep, v = w.partition("=")
    if sep and k[:1] == "N" and k[1:].isdigit() and int(k[1:]) in want:
        pages += int(v)
print(pages)' "$1" "$2"
}

dra-touch-pages() {
    # Usage: dra-touch-pages NODE
    #
    # Ask the numa-touch.py of dra-touch-start where its pages are now
    # (SIGUSR1) and wait for its answer. DRA_TOUCH_PAGES is how many of
    # them are on NUMA node NODE. Fail the test if it does not run any more.
    local node="$1" before
    [ -n "$DRA_TOUCH_PID" ] || error "dra-touch-pages: numa-touch.py does not run, call dra-touch-start first"
    before=$(dra-touch-reports)
    dra-exec "$DRA_TOUCH_VM" "$DRA_TOUCH_TARGET" kill -USR1 "$DRA_TOUCH_PID" ||
        error "numa-touch.py (pid $DRA_TOUCH_PID) in $DRA_TOUCH_TARGET is not running any more"
    retry-until --timeout 10 --interval 1 --message "numa-touch.py reports its pages" \
        '[ "$(dra-touch-reports)" -gt "${before:-0}" ]' ||
        error "numa-touch.py (pid $DRA_TOUCH_PID) in $DRA_TOUCH_TARGET did not report its pages"
    dra-exec "$DRA_TOUCH_VM" "$DRA_TOUCH_TARGET" cat /tmp/numa-touch.log
    DRA_TOUCH_PAGES=$(dra-touch-log-pages "$COMMAND_OUTPUT" "$node")
}

dra-touch-reports() {
    # Usage: dra-touch-reports
    #
    # Print how many "pages" lines the numa-touch.py of dra-touch-start has
    # written (0 if its log cannot be read).
    local n
    n=$(vmdir-command-q "$DRA_TOUCH_VM" "$DRA_TOUCH_EXEC grep -c '^numa-touch: pages' /tmp/numa-touch.log")
    echo "${n:-0}"
}

dra-touch-forget() {
    # Usage: dra-touch-forget
    #
    # Forget the numa-touch.py of dra-touch-start without stopping it, for
    # instance when its pod is deleted.
    DRA_TOUCH_PID=""
}

dra-touch-stop() {
    # Usage: dra-touch-stop
    #
    # Stop the numa-touch.py of dra-touch-start, if it runs.
    [ -n "$DRA_TOUCH_PID" ] || return 0
    dra-exec "$DRA_TOUCH_VM" "$DRA_TOUCH_TARGET" kill "$DRA_TOUCH_PID"
    DRA_TOUCH_PID=""
}

dra-stays() {
    # Usage: dra-stays SECONDS MESSAGE SNIPPET
    #
    # Evaluate SNIPPET on the host at once and then once a second for
    # SECONDS seconds, and fail the test with MESSAGE as soon as it is not
    # true. SNIPPET must not refer to variables named _stays_*.
    local _stays_secs="$1" _stays_msg="$2" _stays_snippet="$3" _stays_end
    echo "checking for ${_stays_secs}s: $_stays_msg"
    _stays_end=$(( SECONDS + _stays_secs ))
    while true; do
        eval "$_stays_snippet" ||
            error "not true any more after $(( _stays_secs - _stays_end + SECONDS ))s: $_stays_msg"
        [ "$SECONDS" -lt "$_stays_end" ] || break
        sleep 1
    done
    echo "held for ${_stays_secs}s: $_stays_msg"
}

###
### Querying and waiting
###

dra-py() {
    # Usage: dra-py assert|eval JSON EXPRESSION
    #
    # Evaluate the Python EXPRESSION on the JSON document (output of
    # "kubectl get ... -o json"), kind k8s of e2e_json.py. Names in
    # EXPRESSION:
    #   j                      the document
    #   items                  j["items"] of a list, [j] of a single object
    #   names()                sorted metadata.name of items
    #   by_name(name)          the item called name, None if not found
    #   slices(driver, node)   ResourceSlices of driver (and node, if given)
    #   pool_slices(pool, driver)  ResourceSlices of driver (default
    #                          cxl-pool.nri.io) of DRA pool pool
    #   pool_nodes(pool, driver)   sorted kubernetes.io/hostname values of
    #                          the nodeSelector of the pool_slices if they
    #                          all have the same selector of exactly one
    #                          term with exactly that one In expression,
    #                          and no allNodes; None otherwise
    #   devices(driver, node)  devices of those slices
    #   device(name, driver, node)  the device called name (of driver, in
    #                          the slice of node, if given), None if not found
    #   attr(dev, name)        value of an attribute ({"string": v} -> v)
    #   capacity(dev, name)    value of a capacity ("512Mi")
    #   request_policy(dev, name)  requestPolicy of a capacity, None if none
    #   quantity(q)            bytes of a quantity ("64Mi", "268435456")
    #   MiB                    1 << 20
    #   results(claim)         allocation results of a ResourceClaim (j)
    #   consumed(claim, name)  bytes of capacity name ("memory") that the
    #                          allocation results of a claim (j) consume
    #   allocated_node(claim)  node of the allocation's nodeSelector
    #   condition(type, claim) status of a condition in status.devices
    #   device_data(dev, claim) data of the status.devices entry of dev
    #   subrequests(claim)     what the device requests of a ResourceClaim
    #                          (j) ask for: the exactly part of a request,
    #                          named like the request, and the subrequests
    #                          of firstAvailable, named request/subrequest
    #   configs(kind, claim)   the opaque configs of a ResourceClaim (j) of
    #                          kind (all if None): {driver, parameters,
    #                          requests}
    #   config_section(c, name)  section name (device, container, pool) of
    #                          the parameters of an entry c of configs(),
    #                          {} if it has none
    #   events(reason, name)   Events with reason, of object name
    #   dax_slices(driver)     dax devices with a size of a dra-memdev-sysfs
    #                          document (j), bound to driver if given
    # On the state document of the driver (j, dra-state-json):
    #   grant(ref)             the grant record of ref ({claim, result}),
    #                          None if none
    #   region(ref)            the region record of ref (its name, or its
    #                          noted name), None if none
    #   memdev(serial)         the memdev record of serial (a number, or
    #                          hex text), None if none
    #   local_memdevs()        the memdev records of source local
    #   claim_uses(m, uid)     the grants of claim uid that use memdev m
    #                          (REALISED, REALISED_UNRESTORED, RELEASING),
    #                          in the order of m's grants
    #   local_claims(m)        the claims of memdev m: the UIDs of its grants
    #                          that use it or are being realised (JOURNALED,
    #                          REALISING), and "-" if a region of the driver
    #                          of m has a dax device that no grant uses (not
    #                          the whole device, not the seed)
    #   lease(id)              the lease record of id, None if none
    python3 "$E2E_JSON" "$1" k8s "$3" <<< "$2"
}

dra-json() {
    # Usage: dra-json VMDIR KUBECTL-GET-ARGS...
    #
    # Run "kubectl get KUBECTL-GET-ARGS -o json" in the cluster of the VM
    # of VMDIR. COMMAND_OUTPUT and DRA_JSON have the JSON. Print the command
    # only, the JSON is long. Return the exit status of kubectl.
    local vmdir="$1" args status
    shift
    args="$*"
    echo -e "\e[38;5;13mroot@$(vmdir-label "$vmdir")>\e[0m kubectl get $args -o json"
    DRA_JSON=$(vmdir-command-q "$vmdir" "kubectl get $args -o json")
    status=$?
    COMMAND_OUTPUT="$DRA_JSON"
    return $status
}

dra-state-json() {
    # Usage: dra-state-json VMDIR
    #
    # Print the state document of the driver ($DRA_STATE) in the VM of
    # VMDIR, quietly, for the names of dra-py on it. Return 1 if it cannot
    # be read.
    vmdir-command-q "$1" "cat $DRA_STATE"
}

dra-assert() {
    # Usage: dra-assert JSON EXPRESSION
    #
    # Fail the test unless the Python EXPRESSION is true on the JSON
    # document, see dra-py for the names in EXPRESSION.
    dra-py assert "$1" "$2" ||
        error "DRA assertion failed: $2
on: $(head -c 3000 <<< "$1")"
    echo "dra ok: $2"
}

dra-value() {
    # Usage: dra-value JSON EXPRESSION
    #
    # Print the value of the Python EXPRESSION on the JSON document.
    dra-py eval "$1" "$2" || error "DRA expression failed: $2"
}

dra-wait() {
    # Usage: dra-wait VMDIR LABEL "KUBECTL-GET-ARGS" EXPRESSION [TIMEOUT]
    #
    # Repeat "kubectl get KUBECTL-GET-ARGS -o json" in the cluster of the
    # VM of VMDIR until the Python EXPRESSION (see dra-py) is true on it,
    # TIMEOUT seconds at most (default 60). Store the last JSON in
    # $TEST_OUTPUT_DIR/dra.LABEL.<vm>.json and in COMMAND_OUTPUT. Fail the
    # test on timeout, showing the JSON.
    local vmdir="$1" label="$2" args="$3" expression="$4" tmo="${5:-60}" file
    file="$TEST_OUTPUT_DIR/dra.$label.$(vmdir-label "$vmdir").json"
    retry-until --timeout "$tmo" --message "$(vmdir-label "$vmdir"): kubectl get $args: $expression" \
        'DRA_JSON=$(vmdir-command-q "$vmdir" "kubectl get $args -o json" 2>&1) && dra-py assert "$DRA_JSON" "$expression" 2>/dev/null' || {
        echo "$DRA_JSON" > "$file"
        error "DRA: $(vmdir-label "$vmdir") did not reach the expected state in ${tmo}s: $expression
kubectl get $args: $(head -c 3000 <<< "$DRA_JSON")
(all in $file)"
    }
    echo "$DRA_JSON" > "$file"
    COMMAND_OUTPUT="$DRA_JSON"
    echo "dra ok: $label: $expression"
}

dra-claim-conditions() {
    # Usage: dra-claim-conditions VMDIR CLAIM
    #
    # Print the conditions of the devices of ResourceClaim CLAIM in $DRA_NS,
    # one "device: type=status reason" line each, and the data that the pool
    # controller wrote for the driver.
    local vmdir="$1" claim="$2" json
    json=$(vmdir-command-q "$vmdir" "kubectl get resourceclaim -n $DRA_NS $claim -o json") ||
        error "dra-claim-conditions: no ResourceClaim $claim in $(vmdir-label "$vmdir")"
    echo "conditions of ResourceClaim $claim in $(vmdir-label "$vmdir"):"
    python3 -c '
import json, sys
j = json.loads(sys.stdin.read())
for d in (j.get("status") or {}).get("devices") or []:
    for c in d.get("conditions") or []:
        print("  %s: %s=%s %s" % (d.get("device"), c.get("type"), c.get("status"), c.get("reason", "")))
    if d.get("data"):
        print("  %s: data %s" % (d.get("device"), json.dumps(d["data"], sort_keys=True)))' <<< "$json"
}

dra-claim-condition-watch() {
    # Usage: dra-claim-condition-watch VMDIR CLAIM TYPE REASON [TIMEOUT]
    #
    # Watch ResourceClaim CLAIM in $DRA_NS (kubectl get -w) until the status
    # of one of its devices has condition TYPE with status True and reason
    # REASON, TIMEOUT seconds at most (default 60), and print the condition.
    # The watch sees conditions that last only until the scheduler
    # deallocates the claim, for instance those of bindingFailureConditions.
    # Fail the test on timeout.
    # Example: dra-claim-condition-watch "$VM" bad-pool cxl-pool.nri.io/AttachFailed InvalidConfig
    local vmdir="$1" claim="$2" ctype="$3" reason="$4" tmo="${5:-60}" prog
    prog='
import json, os, select, subprocess, sys, time
ns, claim, ctype, reason, tmo = sys.argv[1:6]
p = subprocess.Popen(["kubectl", "get", "resourceclaim", "-n", ns, claim, "-w", "-o", "json"], stdout=subprocess.PIPE)
deadline = time.monotonic() + int(tmo)
dec, buf = json.JSONDecoder(), ""
while True:
    left = deadline - time.monotonic()
    if left <= 0:
        p.kill()
        sys.exit(1)
    if not select.select([p.stdout], [], [], left)[0]:
        continue
    chunk = os.read(p.stdout.fileno(), 65536)
    if not chunk:
        sys.exit(1)
    buf += chunk.decode(errors="replace")
    while True:
        buf = buf.lstrip()
        try:
            obj, end = dec.raw_decode(buf)
        except ValueError:
            break
        buf = buf[end:]
        for d in (obj.get("status") or {}).get("devices") or []:
            for c in d.get("conditions") or []:
                if c.get("type") == ctype and c.get("status") == "True" and c.get("reason") == reason:
                    print("%s: %s=True reason %s: %s" % (d.get("device"), ctype, reason, c.get("message", "")))
                    p.kill()
                    sys.exit(0)'
    echo "watching ResourceClaim $claim for $ctype=True reason $reason, ${tmo}s at most"
    vmdir-command "$vmdir" "python3 -c $(printf %q "$prog")$(printf ' %q' "$DRA_NS" "$claim" "$ctype" "$reason" "$tmo")" ||
        error "$(vmdir-label "$vmdir"): ResourceClaim $claim had no device with condition $ctype=True reason $reason in ${tmo}s"
}

dra-pod-wait() {
    # Usage: dra-pod-wait VMDIR POD PHASE [TIMEOUT]
    #
    # Wait until pod POD in $DRA_NS is in PHASE (Pending, Running,
    # Succeeded, Failed), TIMEOUT seconds at most (default 120). Fail the
    # test on timeout, and show the pod and its events.
    local vmdir="$1" pod="$2" phase="$3" tmo="${4:-120}"
    retry-until --timeout "$tmo" --interval 1 --message "$(vmdir-label "$vmdir"): pod $pod $phase" \
        '[ "$(vmdir-command-q "$vmdir" "kubectl get pod -n $DRA_NS $pod -o jsonpath={.status.phase}" 2>/dev/null)" == "$phase" ]' || {
        vmdir-command "$vmdir" "kubectl describe pod -n $DRA_NS $pod"
        error "pod $pod in $(vmdir-label "$vmdir") is not $phase after ${tmo}s"
    }
    vmdir-command "$vmdir" "kubectl get pod -n $DRA_NS $pod -o wide"
}

dra-pod-log() {
    # Usage: dra-pod-log VMDIR POD
    #
    # Print the log of pod POD in $DRA_NS of the cluster of the VM of
    # VMDIR, quietly.
    vmdir-command-q "$1" "kubectl logs -n $DRA_NS $2 2>/dev/null"
}

dra-wait-log() {
    # Usage: dra-wait-log VMDIR POD LINE [TIMEOUT]
    #
    # Wait until the log of pod POD in $DRA_NS of the cluster of the VM of
    # VMDIR has the line LINE (a fixed string that matches the whole line),
    # TIMEOUT seconds at most (default 120). Fail the test on timeout, and
    # show the log and the end of the description of the pod.
    local vmdir="$1" pod="$2" line="$3" tmo="${4:-120}"
    retry-until --timeout "$tmo" --interval 1 --message "$pod: \"$line\"" \
        'dra-pod-log "$vmdir" "$pod" | grep -qxF -- "$line"' || {
        vmdir-command "$vmdir" "kubectl logs -n $DRA_NS $pod; kubectl describe pod -n $DRA_NS $pod | tail -n 20"
        error "pod $pod did not log \"$line\" in ${tmo}s"
    }
}

###
### Trace: every component's log in one timeline
###

dra-trace-start() {
    # Usage: dra-trace-start [VMDIR...]
    #
    # Start collecting what every component does, in the background on the
    # host, until dra-trace-stop: the logs of the fake-cxl-pool-servers, and
    # in each VM (default: the VM of the test) the logs of the
    # fake-cxl-pool-controllers, the driver (its pod on the node),
    # kube-scheduler and kubelet (DRA lines), Events, ResourceClaim and Pod
    # watches and udev
    # events of cxl, dax, memory and node devices. Every line is stamped
    # with the host clock when it arrives on the host, so the streams of
    # all VMs merge into one timeline without VM clock skew; ssh adds a few
    # ms. The streams go to $TEST_OUTPUT_DIR/trace/<tag>.log, tag
    # <vm>.<component>, for instance vm.controller or vm1.udev; the first
    # pool server is "server", the others <name>.server. The controllers
    # share the tag <vm>.controller: their lines name their pools.
    local vmdir label name n pod controller_log
    [ -z "$DRA_TRACE_RUNNING" ] || error "dra-trace-start: a trace is running already"
    [ $# -gt 0 ] || set -- "$OUTPUT_DIR"
    DRA_TRACE_DIR="$TEST_OUTPUT_DIR/trace"
    rm -rf "$DRA_TRACE_DIR"
    mkdir -p "$DRA_TRACE_DIR" || error "dra-trace-start: cannot create $DRA_TRACE_DIR"
    : > "$DRA_TRACE_DIR/pids"
    DRA_TRACE_VMS=("$@")
    DRA_TRACE_RUNNING=1
    if [ -n "$POOL_SERVER_LOG" ] && [ -f "$POOL_SERVER_LOG" ]; then
        dra-trace-collect server lines "tail -n0 -F $(printf %q "$POOL_SERVER_LOG")"
    fi
    for name in "${POOL_SERVERS[@]:1}"; do
        # shellcheck disable=SC2153 # POOL_SERVER_LOGS is of pool.source.sh
        dra-trace-collect "$name.server" lines "tail -n0 -F $(printf %q "${POOL_SERVER_LOGS[$name]}")"
    done
    controller_log=$(dra-controllers-log-cmd)
    for vmdir in "$@"; do
        label=$(vmdir-label "$vmdir")
        name=$(vmdir-name "$vmdir")
        # Collectors of an aborted trace may still run.
        vmdir-command-q "$vmdir" "$DRA_TRACE_KILL; rm -rf /run/dra-trace; mkdir -p /run/dra-trace"
        pod=$(dra-driver-pod "$vmdir") ||
            error "dra-trace-start: $DRA_DAEMONSET does not run on $name"
        dra-trace-collect-vm "$vmdir" "$label.driver" lines "kubectl logs -n kube-system -f --tail=0 -c $DRA_CONTAINER $pod"
        dra-trace-collect-vm "$vmdir" "$label.udev" lines "stdbuf -oL udevadm monitor -k -u -s cxl -s dax -s memory -s node"
        dra-trace-collect-vm "$vmdir" "$label.kubelet" lines "journalctl -f -n0 -o cat -u kubelet | grep --line-buffered -iE 'dra|resourceclaim|cxl'"
        # The rest is of the cluster: once per cluster, on its control plane.
        [ -z "${DRA_WORKERS[$vmdir]}" ] || continue
        dra-trace-collect-vm "$vmdir" "$label.controller" lines "$controller_log"
        dra-trace-collect-vm "$vmdir" "$label.scheduler" lines "kubectl -n kube-system logs -f --tail=0 kube-scheduler-$name"
        dra-trace-collect-vm "$vmdir" "$label.events" lines "kubectl get events -A --watch-only -o custom-columns=NS:.metadata.namespace,KIND:.involvedObject.kind,NAME:.involvedObject.name,REASON:.reason,MSG:.message --no-headers"
        dra-trace-collect-vm "$vmdir" "$label.claims" claims "kubectl get resourceclaims -A -w --output-watch-events -o json"
        dra-trace-collect-vm "$vmdir" "$label.pods" lines "kubectl get pods -A -w --output-watch-events --no-headers"
    done
    # Every remote collector writes its pid file just before it starts.
    for vmdir in "$@"; do
        n=8
        [ -z "${DRA_WORKERS[$vmdir]}" ] || n=3
        retry-until --timeout 30 --message "trace collectors in $(vmdir-label "$vmdir")" \
            '[ "$(vmdir-command-q "$vmdir" "ls /run/dra-trace/ | grep -c .pid\$")" == "$n" ]' ||
            error "dra-trace-start: trace collectors did not start in $(vmdir-label "$vmdir"), see $DRA_TRACE_DIR"
    done
    # The events watch (--watch-only) prints nothing when it is up, so
    # there is nothing to wait for: a second for it, else the trace may
    # miss the first events of the test. Nothing checks the trace.
    sleep 1
    n=$(wc -l < "$DRA_TRACE_DIR/pids")
    echo "dra-trace-start: $n collectors, streams in $DRA_TRACE_DIR"
}

dra-trace-collect() {
    # Usage: dra-trace-collect TAG MODE HOST-COMMAND
    #
    # Run HOST-COMMAND in the background in a process group of its own,
    # stamp its output lines (MODE lines) or the ResourceClaims of its
    # JSON watch stream (MODE claims) to $DRA_TRACE_DIR/TAG.log.
    local tag="$1" mode="$2" cmd="$3" log err
    log="$DRA_TRACE_DIR/$tag.log"
    err="$DRA_TRACE_DIR/$tag.stderr"
    if [ "$mode" == "lines" ]; then
        err="&1"
    else
        err="$(printf %q "$err")"
    fi
    setsid bash -c "$cmd 2>$err | python3 -u $(printf %q "$DRA_TRACE_STAMP") $(printf %q "$tag") $mode >> $(printf %q "$log")" </dev/null >/dev/null 2>&1 &
    echo "$! $tag" >> "$DRA_TRACE_DIR/pids"
}

dra-trace-driver-follow() {
    # Usage: dra-trace-driver-follow VMDIR
    #
    # Add the log of the pod of the driver that runs now on the node of the
    # VM of VMDIR to the running trace, from the start of the pod, with the
    # tag <vm>.<pod name suffix>.driver. Nothing if no trace runs.
    local vmdir="$1" pod
    [ -n "$DRA_TRACE_RUNNING" ] || return 0
    pod=$(dra-driver-pod "$vmdir") ||
        error "dra-trace-driver-follow: $DRA_DAEMONSET does not run on $(vmdir-name "$vmdir")"
    dra-trace-collect-vm "$vmdir" "$(vmdir-label "$vmdir").${pod##*-}.driver" lines "kubectl logs -n kube-system -f --tail=-1 -c $DRA_CONTAINER $pod"
    echo "dra-trace-driver-follow: the trace follows the log of $pod"
}

dra-trace-collect-vm() {
    # Usage: dra-trace-collect-vm VMDIR TAG MODE COMMAND
    #
    # dra-trace-collect of COMMAND run as root in the VM of VMDIR over a
    # dedicated ssh connection. The remote command runs in a session of
    # its own whose id is in /run/dra-trace/TAG.pid, so that
    # dra-trace-stop can kill it in the VM, not only the ssh client.
    local vmdir="$1" tag="$2" mode="$3" cmd="$4" inner remote
    inner="echo \$\$ > /run/dra-trace/$tag.pid; export KUBECONFIG=/root/.kube/config; $cmd"
    remote="mkdir -p /run/dra-trace && exec setsid -w bash -c $(printf %q "$inner")"
    # Quoted twice: once for "bash -c" on the host, once for the shell
    # that sshd runs in the VM.
    dra-trace-collect "$tag" "$mode" "ssh -n -F $(printf %q "$vmdir/.ssh-config") -o ControlMaster=no -o ControlPath=none node sudo bash -lc $(printf %q "$(printf %q "$remote")")"
}

dra-trace-stop() {
    # Usage: dra-trace-stop
    #
    # Stop the collectors of dra-trace-start, in the VMs and on the host,
    # and merge their streams into $TEST_OUTPUT_DIR/trace.txt in the order
    # of the host clock, lines "HH:MM:SS.micro <tag> <line>". Never fails
    # the test: it runs in the EXIT trap, too.
    local vmdir pid tag i alive
    [ -n "$DRA_TRACE_RUNNING" ] || return 0
    DRA_TRACE_RUNNING=""
    for vmdir in "${DRA_TRACE_VMS[@]}"; do
        vmdir-command-q "$vmdir" "$DRA_TRACE_KILL; rmdir /run/dra-trace 2>/dev/null"
    done
    # tail of the server log: the stamper reads to the end and exits.
    while read -r pid tag; do
        [[ "$tag" == server || "$tag" == *.server ]] && pkill -TERM -g "$pid" -x tail
    done < "$DRA_TRACE_DIR/pids"
    # With the remote commands gone, ssh exits and the stampers flush.
    for i in $(seq 10); do
        alive=0
        while read -r pid tag; do
            kill -0 "$pid" 2>/dev/null && alive=1
        done < "$DRA_TRACE_DIR/pids"
        [ "$alive" == 0 ] && break
        sleep 0.5
    done
    while read -r pid tag; do
        kill -TERM -- "-$pid" 2>/dev/null
    done < "$DRA_TRACE_DIR/pids"
    LC_ALL=C sort -s -k1,1n "$DRA_TRACE_DIR"/*.log > "$DRA_TRACE_DIR/merged.txt"
    python3 -c '
import sys, time
width = 0
rows = []
for line in open(sys.argv[1], errors="replace"):
    parts = line.rstrip("\n").split(" ", 2)
    if len(parts) < 2:
        continue
    t, tag, text = float(parts[0]), parts[1], parts[2] if len(parts) > 2 else ""
    rows.append((t, tag, text))
    width = max(width, len(tag))
with open(sys.argv[2], "w") as out:
    for t, tag, text in rows:
        out.write("%s.%06d %-*s  %s\n" % (time.strftime("%H:%M:%S", time.localtime(t)), int(t % 1 * 1e6), width, tag, text))
' "$DRA_TRACE_DIR/merged.txt" "$TEST_OUTPUT_DIR/trace.txt"
    echo "dra-trace-stop: $(wc -l < "$TEST_OUTPUT_DIR/trace.txt") lines in $TEST_OUTPUT_DIR/trace.txt"
    return 0
}

dra-trace-summary() {
    # Usage: dra-trace-summary
    #
    # Pick the lines that tell the story from the trace of dra-trace-stop:
    # claims created, allocated and deleted, binding conditions, attach and
    # detach in the server and the controller, prepare and release in the
    # driver (pool devices, and regions and slices of local devices),
    # udev events of the devices, pods starting and stopping.
    # Write them to $TEST_OUTPUT_DIR/trace-summary.txt and print it.
    [ -f "$DRA_TRACE_DIR/merged.txt" ] || error "dra-trace-summary: no trace, run dra-trace-start and dra-trace-stop first"
    python3 -c '
import re, sys, time

merged, out_path, ns, test, vms = sys.argv[1:6]
patterns = {
    # fake-cxl-pool-server on the host: attachments and qemu hotplug events.
    "server": r"attachment \S+: (attaching|attached|detaching|detached|failed)|event DEVICE_DELETED|device \S+: (created|deleted)",
    # fake-cxl-pool-controller: what it attached, detached and wrote.
    "controller": r"(?i)attach|detach|condition|status|publish|slice|error|fail",
    # nri-dra-driver-cxl: the pool driver (prepared, released, unprepared),
    # pool devices skipped by the cxl.nri.io publisher, the lease broker
    # (leases, companions, lends and revokes, fds sent), the local
    # allocator (regions and slices of local memdevs) and the memdev
    # devices it publishes, errors.
    "driver": r"(?i)cxl-pool\.nri\.io|pool device|publisher: |lease|companion|\blend|revok|local memdev|memdev-|no room for the slice|error|fail",
    "scheduler": r"(?i)binding|resourceclaim|claim|bound|error",
    "events": r"",
    "claims": r"",
    "pods": r"",
    "udev": r"^KERNEL\[",
    "kubelet": r"(?i)prepar|resourceclaim|error|fail",
}
# Lines that match but tell nothing: the controller logs every server
# event, the udev watcher logs every rescan trigger.
noise = {
    "driver": r"udev event triggered rescan|^cxl region: (decoder_region_action|region_action|cmd_disable_region)",
    "controller": r"server event ",
}
rows = []
for line in open(merged, errors="replace"):
    parts = line.rstrip("\n").split(" ", 2)
    if len(parts) < 3:
        continue
    t, tag, text = float(parts[0]), parts[1], parts[2]
    kind = tag.split(".")[-1]
    pattern = patterns.get(kind)
    if pattern is None or not re.search(pattern, text):
        continue
    if kind in noise and re.search(noise[kind], text):
        continue
    words = text.split()
    if kind == "events" and words and words[0] == "kube-system":
        continue
    if kind == "pods" and (len(words) < 2 or words[1] != ns):
        continue
    if kind == "udev" and not re.search(r"\((cxl|dax|node|memory)\)$", text):
        continue
    if kind == "server" and " GET " in text:
        continue
    rows.append((t, tag, text))
width = max([len(r[1]) for r in rows] + [3])
with open(out_path, "w") as out:
    out.write("# What every component did in %s, in order.\n" % test)
    out.write("# Times are host clock receipt times: the host stamped each line when it\n")
    out.write("# arrived. Lines from the VMs come over ssh, which adds a small delay; there is\n")
    out.write("# no VM clock skew. +s is seconds since the first line below.\n")
    out.write("# Tags: server = fake-cxl-pool-server on the host (<name>.server: the pool\n")
    out.write("# server <name> of several); <vm>.controller = the fake-cxl-pool-controllers,\n")
    out.write("# <vm>.driver = nri-dra-driver-cxl, <vm>.scheduler,\n")
    out.write("# <vm>.kubelet, <vm>.events, <vm>.claims (ResourceClaim changes), <vm>.pods,\n")
    out.write("# <vm>.udev (kernel uevents) of the VMs: %s.\n" % vms)
    out.write("# cxl-lease-<id> are the companion pods and claims of leases.\n")
    out.write("# The full trace is trace.txt.\n")
    t0 = rows[0][0] if rows else 0
    for t, tag, text in rows:
        out.write("%s.%06d %+9.3fs %-*s  %s\n" % (time.strftime("%H:%M:%S", time.localtime(t)), int(t % 1 * 1e6), t - t0, width, tag, text))
' "$DRA_TRACE_DIR/merged.txt" "$TEST_OUTPUT_DIR/trace-summary.txt" "$DRA_NS" "$(basename "${TEST_DIR:-test}")" \
        "$(for vmdir in "${DRA_TRACE_VMS[@]}"; do printf '%s = %s ' "$(vmdir-label "$vmdir")" "$(vmdir-name "$vmdir")"; done)" ||
        error "dra-trace-summary: cannot summarize $DRA_TRACE_DIR/merged.txt"
    echo "### trace summary ($TEST_OUTPUT_DIR/trace-summary.txt)"
    cat "$TEST_OUTPUT_DIR/trace-summary.txt"
}
