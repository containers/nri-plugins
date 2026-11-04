# Copyright The NRI Plugins Authors. All Rights Reserved.
# SPDX-License-Identifier: Apache-2.0

# Host-side helpers. Keep test orchestration in code.var.sh, and execute the
# privileged CRI/cgroup operations through the framework's vm-command helper.
blockio-vm() {
    local args
    printf -v args '%q ' "$@"
    vm-command "set -euo pipefail; source /var/tmp/nri-blockio-e2e.sh; $args"
}

blockio-finish() {
    local status=$?
    trap - EXIT
    blockio-vm blockio-cleanup || status=1
    # Preserve diagnostics even when setup, creation, or comparison failed.
    host-command "$SCP -r $VM_HOSTNAME:/var/tmp/nri-blockio-e2e \"$TEST_OUTPUT_DIR/blockio\"" || status=1
    exit "$status"
}

blockio-setup() {
    [ "$k8scri" = crio ] || error 'blockio comparison requires k8scri=crio'
    rm -rf "$TEST_OUTPUT_DIR/blockio"
    vm-put-file "$POLICY_DIR/blockio.vm.sh" /var/tmp/nri-blockio-e2e.sh
    trap blockio-finish EXIT
    blockio-vm blockio-setup "$(vm-coverage-gocoverdir)" || command-error 'blockio setup failed'
}

blockio-start() {
    blockio-vm blockio-start "$@" || command-error 'starting comparison runtime failed'
}

blockio-stop() {
    blockio-vm blockio-stop || command-error 'stopping comparison runtime failed'
}

blockio-class-config() {
    blockio-vm blockio-class-config "$@" || command-error 'changing class configuration failed'
}

blockio-case() {
    blockio-vm blockio-case "$@" || command-error "blockio case $1 failed"
}

blockio-compare() {
    blockio-vm blockio-compare || command-error 'CRI-O and plugin cgroup settings differ'
}
