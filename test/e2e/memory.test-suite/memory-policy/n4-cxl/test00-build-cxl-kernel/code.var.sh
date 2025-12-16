vm-kernel-pkgs-install

vm-cxl-hotplug memdev1

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
           "
