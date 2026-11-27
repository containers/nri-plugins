# Copyright The NRI Plugins Authors. All Rights Reserved.
# SPDX-License-Identifier: Apache-2.0

blockio_work=/var/tmp/nri-blockio-e2e

blockio-setup() {
    local index device scheduler original
    test -f /sys/fs/cgroup/cgroup.controllers
    blockio-cleanup
    rm -rf "$blockio_work"
    mkdir -p "$blockio_work"
    modprobe bfq || true
    for index in 0 1; do
        truncate -s 16M "$blockio_work/disk$index.img"
        device=$(losetup --find --show --read-only "$blockio_work/disk$index.img")
        echo "$device" >> "$blockio_work/devices"
        scheduler=/sys/block/${device##*/}/queue/scheduler
        original=$(sed -n 's/.*\[\([^]]*\)\].*/\1/p' "$scheduler")
        printf '%s %s\n' "$scheduler" "$original" >> "$blockio_work/schedulers"
        grep -qw bfq "$scheduler"
        echo bfq > "$scheduler"
    done
}

blockio-cleanup() {
    local scheduler original device status=0
    if [ -f "$blockio_work/schedulers" ]; then
        while read -r scheduler original; do
            echo "$original" > "$scheduler" || status=1
        done < "$blockio_work/schedulers"
    fi
    if [ -f "$blockio_work/devices" ]; then
        while read -r device; do
            losetup --detach "$device" || status=1
        done < "$blockio_work/devices"
    fi
    if [ -d "$blockio_work" ]; then
        chmod -R a+rX "$blockio_work" || status=1
        if [ "$status" = 0 ]; then
            rm -f "$blockio_work/devices" "$blockio_work/schedulers"
        fi
    fi
    return "$status"
}
