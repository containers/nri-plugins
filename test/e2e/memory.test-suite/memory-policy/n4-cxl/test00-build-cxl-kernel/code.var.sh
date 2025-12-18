# Test following features in the test framework.
# - Create a virtual machine with hotpluggable CXL memories (none attached at boot time).
# - Building and installing a custom kernel with custom configuration.
# - Using udev-monitor for detecting hardware configuration changes.
# - Hotplugging, onlining, offlining and hotremoving CXL memories.

if [[ "$distro" != *"fedora"* ]]; then
    echo "SKIP: this test runs only on fedora"
    exit 0
fi

vm-kernel-pkgs-install

vm-command "command -v cxl || dnf install -y /usr/bin/cxl numactl"

vm-command "command -v udev-monitor" || {
    HOST_UDEV_MONITOR=$OUTPUT_DIR/udev-monitor

    [ -f "$HOST_UDEV_MONITOR" ] || \
        GOARCH=amd64 go build -o "$HOST_UDEV_MONITOR" "${TEST_DIR%%/test/e2e/*}/scripts/udev-monitor/udev-monitor.go" || \
        error "failed to build $HOST_UDEV_MONITOR"

    vm-put-file "$HOST_UDEV_MONITOR" "/usr/local/bin/udev-monitor"
}

echo "launching udev-monitor in the background"
vm-command-q "udev-monitor 2>&1 | tee udev-monitor.output" &

sleep 2
echo "next: hotplug CXL"
vm-cxl-hotplug cxl_memdev0

sleep 2
vm-command "set -x
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
sleep 2
echo "hotplugging more memories"
sleep 2
vm-cxl-hotplug cxl_memdev3
vm-cxl-hotplug cxl_memdev1
vm-cxl-hotplug cxl_memdev2
sleep 5
vm-command "cxl list"

echo "hotremoving single memory"
vm-cxl-hotremove cxl_memdev2
sleep 5
vm-command "cxl list"

vm-command "pkill udev-monitor"
