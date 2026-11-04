# Copyright The NRI Plugins Authors. All Rights Reserved.
# SPDX-License-Identifier: Apache-2.0

blockio-vm() {
    local args
    printf -v args '%q ' "$@"
    vm-command "set -euo pipefail; source /var/tmp/nri-blockio-e2e.sh; $args"
}

blockio-finish() {
    local status=$?
    trap - EXIT
    vm-command 'kubectl delete pod -l blockio-test=blockio --ignore-not-found --wait=true' || status=1
    helm-terminate || status=1
    blockio-vm blockio-cleanup || status=1
    host-command "$SCP -r $VM_HOSTNAME:/var/tmp/nri-blockio-e2e \"$TEST_OUTPUT_DIR/blockio\"" || status=1
    exit "$status"
}

blockio-setup() {
    rm -rf "$TEST_OUTPUT_DIR/blockio"
    vm-put-file "$POLICY_DIR/blockio.vm.sh" /var/tmp/nri-blockio-e2e.sh
    trap blockio-finish EXIT
    vm-command 'kubectl delete pod -l blockio-test=blockio --ignore-not-found --wait=true' ||
        command-error 'deleting previous blockio test pods failed'
    helm-terminate
    blockio-vm blockio-setup || command-error 'blockio device setup failed'
    vm-command 'cat /var/tmp/nri-blockio-e2e/devices' || command-error 'reading blockio devices failed'
    readarray -t blockio_devices <<< "$COMMAND_OUTPUT"
    vm-command "lsblk -dnr -o MAJ:MIN ${blockio_devices[*]}" || command-error 'reading blockio device numbers failed'
    readarray -t blockio_devnums <<< "$COMMAND_OUTPUT"
}

verify-blockio() {
    local pod=$1 container=$2 file=$3 expected=$4 observed
    vm-command "kubectl exec $pod -c $container -- cat /sys/fs/cgroup/$file" ||
        command-error "reading $file for $pod/$container failed"
    printf '%s\n' "$COMMAND_OUTPUT" > "$TEST_OUTPUT_DIR/$pod-$container-$file"
    observed=$(sort <<< "$COMMAND_OUTPUT")
    expected=$(sort <<< "$expected")
    if [ "$observed" != "$expected" ]; then
        command-error "$pod/$container $file: expected '$expected', got '$observed'"
    fi
    echo "verify $pod/$container $file: ok"
}
