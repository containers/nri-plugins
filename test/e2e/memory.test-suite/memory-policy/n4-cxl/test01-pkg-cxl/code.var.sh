# Test that package kernel (pkg/cxl/kernel) reads the CXL hardware of a VM
# correctly. Drive the CXL memory devices of the VM through the states
# below, and check after each of them what the cxl-dump tool, that is, the
# topology of package kernel, makes of the result.
# - No CXL memory devices at all.
# - A memory device plugged in, enabled and disabled.
# - A region of a single memory device, enabled but without online memory,
#   with a part of its memory online, and fully online.
# - A region interleaved over two memory devices.
# - Two regions, one on each host bridge, on separate NUMA nodes.
# - Memory devices hotremoved.
#
# The CXL hardware of the n4-cxl topology: two host bridges, each with a switch
# with two hotpluggable 256M memory devices, none of them present at boot.
# cxl_memdev0 and cxl_memdev1 are behind the host bridge of NUMA node 0,
# cxl_memdev2 and cxl_memdev3 behind the one of node 1. Each host bridge has a
# memory window of its own, so memory devices can be interleaved only within a
# host bridge, and a single HDM decoder, so each host bridge can host only one
# region at a time.

cxl-reset() {
    # Usage: cxl-reset
    #
    # Make sure that the VM has no CXL memory devices plugged in, and that
    # hotplugged memory is not onlined without the test asking for it.
    #
    # Restart qemu if there are devices: hot-unplugging a CXL memory device
    # leaves its pci slot occupied and its memory backend in use in qemu
    # (11.1), so the same device cannot be hotplugged again before qemu
    # restarts. vm-reboot restarts qemu, which returns every CXL memory device
    # to its boot time state.
    if vm-cxl-hw | grep -q plugged; then
        echo "cxl-reset: CXL memory devices are plugged in, restarting qemu..."
        timeout=300 vm-reboot
    fi
    vm-cxl-hw | grep -q plugged &&
        error "cxl-reset: failed to unplug all CXL memory devices"
    vm-command "echo offline > /sys/devices/system/memory/auto_online_blocks"
    return 0
}

cxl-qemu-serial() {
    # Usage: cxl-qemu-serial QEMU_MEMDEV
    #
    # Print the serial number of a hotpluggable qemu CXL memory device, for
    # instance 0xc100e2e0 of cxl_memdev0. The serial number is what identifies
    # the device in the VM: the name it gets there depends on the order in
    # which the devices were plugged in.
    local serial
    serial=$(show_sn=1 vm-cxl-hw | awk -v dev="$1" '$1 == dev {print $2}')
    serial=${serial#sn=}
    [ -n "$serial" ] || error "cxl-qemu-serial: no qemu CXL memory device $1"
    echo "$serial"
}

cxl-hotplug() {
    # Usage: cxl-hotplug QEMU_MEMDEV
    #
    # Hotplug a CXL memory device and wait until cxl-dump sees it in the VM
    # with its endpoint port.
    local qemu_memdev="$1" serial
    serial=$(cxl-qemu-serial "$qemu_memdev")
    vm-cxl-hotplug "$qemu_memdev"
    cxl-wait "$qemu_memdev-plugged" "by_serial($serial) is not None and by_serial($serial)['state'] == 'READY'"
}

cxl-hotremove() {
    # Usage: cxl-hotremove QEMU_MEMDEV
    #
    # Hotremove a CXL memory device and wait until it is gone from the VM.
    # Disable the device and destroy the regions using it first, otherwise the
    # VM will not release it.
    local qemu_memdev="$1" serial
    serial=$(cxl-qemu-serial "$qemu_memdev")
    vm-cxl-hotremove "$qemu_memdev"
    cxl-wait "$qemu_memdev-removed" "by_serial($serial) is None"
}

cxl-region-blocks() {
    # Usage: cxl-region-blocks REGION [all|first|last]
    #
    # Print the indexes of the memory blocks of a region in the last cxl-dump
    # output.
    local rg="by_name(regions, '$1')['range']"
    local blocks="range(${rg}['start'] // block_size, (${rg}['end'] + 1) // block_size)"
    case "${2:-all}" in
        all) cxl-value "' '.join(str(b) for b in $blocks)" ;;
        first) cxl-value "${blocks}[0]" ;;
        last) cxl-value "${blocks}[-1]" ;;
        *) error "cxl-region-blocks: invalid selection $2" ;;
    esac
}

cxl-memory-state() {
    # Usage: cxl-memory-state online_movable|offline BLOCK...
    #
    # Online or offline memory blocks. Skip the blocks that are already in the
    # requested state: writing the state a block already has fails.
    local state="$1" block state_file
    shift
    for block in "$@"; do
        state_file="/sys/devices/system/memory/memory$block/state"
        vm-command "grep -qx ${state%_movable} $state_file || echo $state > $state_file" ||
            command-error "cannot set memory block $block $state"
    done
}

if [[ "$distro" != *"fedora"* ]]; then
    echo "SKIP: this test runs only on fedora"
    exit 0
fi

# CXL needs the kernel built for this topology, and the tools to drive it.
vm-kernel-pkgs-install
cxl-tools-install

MEM_SIZE=$(( 256 * 1024 * 1024 ))

echo "### no CXL memory devices"
cxl-reset
cxl-dump no-devices
cxl-assert 'memdevs == [] and regions == [] and daxdevs == [] and blocks == []'
cxl-assert 'len(nodes) == 2'
cxl-assert 'all(n["memTotal"] > 0 for n in nodes)'
cxl-assert "block_size == $CXL_BLOCK_SIZE"

echo "### cxl_memdev0 hotplugged: the kernel binds it and adds its endpoint port"
cxl-hotplug cxl_memdev0
cxl-assert 'len(memdevs) == 1 and len(endpoints) == 1 and regions == []'
cxl-assert 'memdevs[0]["name"] == "mem0"'
cxl-assert 'memdevs[0]["serial"] == 0xc100e2e0'
cxl-assert "memdevs[0][\"ramSize\"] == $MEM_SIZE"
cxl-assert 'memdevs[0]["pmemSize"] == 0'
cxl-assert 'memdevs[0]["major"] > 0'
# The memory device is behind the host bridge of NUMA node 0.
cxl-assert 'memdevs[0]["numaAffinity"] == 0'
# Its decoder path from the CXL root: the port of the host bridge, the port
# of the switch, its endpoint port. No region has a decoder of it yet.
cxl-assert 'len(memdevs[0]["ports"]) == 3 and memdevs[0]["ports"][-1] == memdevs[0]["endpoint"]'
cxl-assert 'memdevs[0]["regions"] == []'

echo "### mem0 disabled: no driver, no endpoint"
vm-command "cxl disable-memdev mem0" || command-error "cannot disable mem0"
cxl-wait mem0-disabled 'not enabled(memdevs[0])'
cxl-assert 'len(memdevs) == 1 and endpoints == []'
cxl-assert 'memdevs[0]["state"] == "UNBOUND" and memdevs[0]["ports"] == []'
cxl-assert "memdevs[0][\"ramSize\"] == $MEM_SIZE"

echo "### region of mem0, enabled, memory offline"
vm-command "cxl enable-memdev mem0" || command-error "cannot enable mem0"
vm-command "cxl create-region -t ram -d decoder0.0 -m mem0" ||
    command-error "cannot create a region of mem0"
# The kernel binds the dax device of the region over all of its memory to
# kmem, which adds the memory blocks offline.
cxl-wait region-offline 'len(regions) == 1 and enabled(regions[0]) and [x["driver"] for x in daxdevs] == ["kmem"] and blocks and all(b["state"] == "OFFLINE" for b in blocks)'
cxl-assert 'regions[0]["name"] == "region0"'
cxl-assert 'regions[0]["mode"] == "ram" and regions[0]["committed"]'
cxl-assert 'regions[0]["rootDecoder"] == "decoder0.0"'
cxl-assert "size(regions[0]) == $MEM_SIZE"
cxl-assert 'regions[0]["range"]["start"] > 0'
cxl-assert 'regions[0]["memdevs"] == ["mem0"] and regions[0]["serials"] == [0xc100e2e0]'
# The one target decoder of the region is a decoder of the endpoint port of
# mem0 (decoderN.M of endpointN), and the region is the one region of mem0.
cxl-assert 'len(regions[0]["targets"]) == 1'
cxl-assert 'regions[0]["targets"][0].startswith("decoder%s." % memdevs[0]["endpoint"][len("endpoint"):])'
cxl-assert 'memdevs[0]["regions"] == ["region0"]'
cxl-assert 'regions[0]["daxRegion"] == "dax_region0"'
cxl-assert '[(x["name"], x["size"]) for x in daxdevs] == [("dax0.0", size(regions[0]))]'
cxl-assert "len(blocks) == $MEM_SIZE // block_size"
# The node of the region comes from its dax device, as no memory is online.
# It is a new node, not one of the CPUs.
cxl-assert 'regions[0]["numaNode"] not in cpu_nodes()'
cxl-assert 'online_size(regions[0]) == 0'
cxl-assert 'node(regions[0]["numaNode"])["memTotal"] == 0'
REGION_NODE=$(cxl-value 'regions[0]["numaNode"]')
vm-command "grep . /sys/bus/cxl/devices/region0/dax_region*/dax*/target_node" ||
    command-error "cannot read the target node of region0"
grep -q "^$REGION_NODE\$" <<< "$COMMAND_OUTPUT" ||
    error "cxl-dump reported node $REGION_NODE for region0, dax target_node says $COMMAND_OUTPUT"
cxl-assert "daxdevs[0]['targetNode'] == $REGION_NODE"
vm-command "cxl list -R -u | grep -A1 region0" # region as the cxl tool sees it

echo "### last memory block of the region online"
cxl-memory-state online_movable $(cxl-region-blocks region0 last)
cxl-wait region-partially-online 'online_size(regions[0]) > 0'
# Nothing but the amount of online memory changes when only a part of the
# region is online.
cxl-assert "regions[0][\"numaNode\"] == $REGION_NODE"
cxl-assert "online_size(regions[0]) == $CXL_BLOCK_SIZE"
cxl-assert "online_size(regions[0]) < size(regions[0])"
cxl-assert "node($REGION_NODE)[\"memTotal\"] == online_size(regions[0])"
cxl-assert "[b['state'] for b in blocks][-1] == 'ONLINE' and blocks[-1]['numaNode'] == $REGION_NODE"

echo "### all memory blocks of the region online"
cxl-memory-state online_movable $(cxl-region-blocks region0 all)
cxl-wait region-online 'online_size(regions[0]) == size(regions[0])'
cxl-assert "regions[0][\"numaNode\"] == $REGION_NODE"
cxl-assert "online_size(regions[0]) == $MEM_SIZE"
cxl-assert "node($REGION_NODE)[\"memTotal\"] == online_size(regions[0])"

echo "### region interleaved over mem0 and mem1 of the same host bridge"
cxl-memory-state offline $(cxl-region-blocks region0 all)
vm-command "cxl disable-region region0 && cxl destroy-region region0" ||
    command-error "cannot destroy region0"
cxl-hotplug cxl_memdev1
cxl-assert 'len(memdevs) == 2 and regions == []'
cxl-assert 'names(memdevs) == ["mem0", "mem1"]'
cxl-assert 'all(m["numaAffinity"] == 0 for m in memdevs)'
vm-command "cxl create-region -t ram -d decoder0.0 -m mem0 mem1" ||
    command-error "cannot create an interleaved region of mem0 and mem1"
cxl-wait interleaved-region 'len(regions) == 1 and enabled(regions[0]) and [x["driver"] for x in daxdevs] == ["kmem"] and blocks and all(b["state"] == "OFFLINE" for b in blocks)'
cxl-assert "size(regions[0]) == 2 * $MEM_SIZE"
cxl-assert 'len(regions[0]["targets"]) == 2 and len(set(regions[0]["targets"])) == 2'
cxl-assert 'sorted(regions[0]["memdevs"]) == ["mem0", "mem1"]'
cxl-assert 'all(m["regions"] == ["region0"] for m in memdevs)'
cxl-memory-state online_movable $(cxl-region-blocks region0 all)
cxl-wait interleaved-region-online 'online_size(regions[0]) == size(regions[0])'
cxl-assert "online_size(regions[0]) == 2 * $MEM_SIZE"
cxl-assert 'node(regions[0]["numaNode"])["memTotal"] == online_size(regions[0])'

echo "### second region on the other host bridge"
cxl-hotplug cxl_memdev2
cxl-assert 'memdevs[-1]["serial"] == 0xc100e2e2'
# cxl_memdev2 is behind the host bridge of NUMA node 1, unlike mem0 and mem1.
cxl-assert 'by_serial(0xc100e2e2)["numaAffinity"] == 1'
vm-command "cxl create-region -t ram -d decoder0.1 -m mem2" ||
    command-error "cannot create a region of mem2"
cxl-wait two-regions 'len(regions) == 2 and all(enabled(r) for r in regions) and len(daxdevs) == 2 and all(x["driver"] == "kmem" for x in daxdevs) and all(b["state"] != "ABSENT" for b in blocks)'
cxl-assert 'names(regions) == ["region0", "region1"]'
cxl-assert 'by_name(regions, "region1")["memdevs"] == ["mem2"]'
cxl-assert 'by_name(regions, "region1")["rootDecoder"] == "decoder0.1"'
cxl-assert "size(by_name(regions, \"region1\")) == $MEM_SIZE"
# Every region has a NUMA node of its own, and its own amount of online memory.
cxl-assert 'len(set(r["numaNode"] for r in regions)) == 2'
cxl-assert 'not set(r["numaNode"] for r in regions) & set(cpu_nodes())'
cxl-assert 'online_size(by_name(regions, "region1")) == 0'
cxl-assert "online_size(by_name(regions, \"region0\")) == 2 * $MEM_SIZE"
cxl-memory-state online_movable $(cxl-region-blocks region1 all)
cxl-wait two-regions-online 'all(online_size(r) == size(r) for r in regions)'
cxl-assert "online_size(by_name(regions, \"region1\")) == $MEM_SIZE"
cxl-assert "online_size(by_name(regions, \"region0\")) == 2 * $MEM_SIZE"
cxl-assert 'all(node(r["numaNode"])["memTotal"] == online_size(r) for r in regions)'
cxl-assert 'len(nodes) == 4'
vm-command "numactl -H" # nodes as the kernel presents them

echo "### region1 disabled"
cxl-memory-state offline $(cxl-region-blocks region1 all)
vm-command "cxl disable-region region1" || command-error "cannot disable region1"
cxl-wait region1-disabled 'by_name(regions, "region1")["state"] == "DISABLED"'
# The dax region and its dax device are gone, and kmem removed the offline
# memory blocks with them: the region has no node left.
cxl-assert 'by_name(regions, "region1")["daxRegion"] == ""'
cxl-assert 'by_name(regions, "region1")["numaNode"] == -1'
cxl-assert 'online_size(by_name(regions, "region1")) == 0'
# Disabling the region does not disable the memory device it uses.
cxl-assert 'enabled(by_serial(0xc100e2e2))'
# ...and leaves the other region alone.
cxl-assert 'enabled(by_name(regions, "region0"))'
cxl-assert "online_size(by_name(regions, \"region0\")) == 2 * $MEM_SIZE"

echo "### all memory devices hotremoved"
vm-command "cxl destroy-region region1" || command-error "cannot destroy region1"
cxl-memory-state offline $(cxl-region-blocks region0 all)
vm-command "cxl disable-region region0 && cxl destroy-region region0" ||
    command-error "cannot destroy region0"
vm-command "cxl disable-memdev mem0 mem1 mem2" ||
    command-error "cannot disable the memory devices"
cxl-hotremove cxl_memdev2
cxl-hotremove cxl_memdev1
cxl-hotremove cxl_memdev0
cxl-dump devices-removed
cxl-assert 'memdevs == [] and regions == [] and endpoints == []'
cxl-assert 'len(nodes) == 2'

# The CXL memory devices of this VM cannot be hotplugged again before qemu
# restarts, so leave the VM without them. cxl-reset does the restart when the
# next test needs the devices back.
