vm-command "uname -a"

vm-command "[ -d linux ]" || {
    # git clone fedora42: 74 seconds
    # git clone fedora42 with virtio-pci-blk-drive: 86 seconds
    getkernel="git clone --depth 1 -b next git://git.kernel.org/pub/scm/linux/kernel/git/cxl/cxl.git linux" vm-kernel-install-devel-env

    vm-put-file $TEST_DIR/kernel.config linux/.config

    # ubuntu2204: 156 seconds
    # fedora 42: 195 seconds
    # fedora 42 with virtio-pci-blk-drive: 167 seconds
    vm-command 'cd linux; make -j$(nproc) && make modules -j$(nproc) && make modules_install && make install' ||
        error "building and installing CXL kernel failed"

    # fedora 42 with virtio-pci-blk-drive: rougly a minute
    vm-reboot
}

vm-command "uname -a"

# Install utilities
vm-command "command -v cxl || dnf install -y /usr/bin/cxl numactl golang"

vm-command "mkdir udev-monitor 2>/dev/null" && {
    udev_monitor_tool="${TEST_DIR%%/test/e2e/*}/scripts/udev-monitor/udev-monitor.go"
    vm-put-file "$udev_monitor_tool" "./udev-monitor/udev-monitor.go"
    vm-command "cd udev-monitor && go mod init udev-monitor && go mod tidy && go build . && cp -v udev-monitor /usr/local/bin"
}

vm-command "(udev-monitor 2>&1 | tee udev-monitor.output) &
           sleep 1
           set -x
           sh -c 'grep . /sys/devices/system/node/{online,possible} /sys/devices/system/memory/auto_online_blocks'
           echo online_movable > /sys/devices/system/memory/auto_online_blocks
           sh -c 'grep 0 /sys/devices/system/memory/memory*/online'
           sleep 1
           cxl enable-memdev mem0
           sleep 1
           cxl create-region -t ram -d decoder0.0 -m mem0
           sleep 1
           cxl enable-region region0
           sleep 1
           sh -c 'grep . /sys/devices/system/node/{online,possible}'
           sh -c 'grep 0 /sys/devices/system/memory/memory*/online'
           sh -c 'for f in /sys/devices/system/memory/memory*/online; do echo 1 > \$f; done'
           sleep 1
           sh -c 'grep 0 /sys/devices/system/memory/memory*/online'
           kill %1
           "
