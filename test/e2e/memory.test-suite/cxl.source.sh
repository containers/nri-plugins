# Helpers that read the CXL topology of the VM with the cxl-dump tool.
#
# cxl-dump prints what package kernel (pkg/cxl/kernel) reads from the sysfs
# of the VM: memdevs, regions, dax devices, memory blocks and NUMA nodes
# (kernel.ObserveTopology as JSON). The tests drive the CXL hardware of the
# VM and check the topology that results. Every test of memory.test-suite
# has these helpers.

CXL_DUMP_FILE=""   # cxl-dump output of the last cxl-dump/cxl-wait
CXL_BLOCK_SIZE=""  # memory block size in bytes in the VM
# The evaluator of cxl-py, and of dra-py and pool-py of dra-driver-cxl.
E2E_JSON="$nri_resource_policy_src/test/e2e/memory.test-suite/e2e_json.py"

cxl-tools-install() {
    # Usage: cxl-tools-install
    #
    # Install the cxl and numactl tools in the VM if they are missing, and
    # always rebuild and install cxl-dump from the source tree.
    local host_cxl_dump="$OUTPUT_DIR/cxl-dump"
    local src_dir="$nri_resource_policy_src"

    vm-command-q "command -v cxl >/dev/null && command -v numactl >/dev/null" ||
        vm-command "dnf install -y /usr/bin/cxl numactl" ||
            error "cannot install cxl and numactl in the VM"

    GOARCH=amd64 go build -o "$host_cxl_dump" "$src_dir/scripts/cxl-dump/cxl-dump.go" ||
        error "cannot build $src_dir/scripts/cxl-dump/cxl-dump.go"
    vm-put-file "$host_cxl_dump" "/usr/local/bin/cxl-dump"

    CXL_BLOCK_SIZE=$(cxl-block-size-parse "$(vm-command-q "cat /sys/devices/system/memory/block_size_bytes")") ||
        error "cannot read memory block size from the VM"
    echo "cxl-tools-install: memory block size $CXL_BLOCK_SIZE bytes"
}

cxl-block-size-parse() {
    # Usage: cxl-block-size-parse TEXT
    #
    # Print the memory block size in bytes that TEXT, the contents of
    # /sys/devices/system/memory/block_size_bytes, gives: hexadecimal with
    # or without 0x ("8000000" is 128 MiB). Return 1 unless TEXT is a
    # positive hexadecimal number.
    local hex="${1//[[:space:]]/}"
    hex="${hex#0x}"
    [[ "$hex" =~ ^[0-9a-fA-F]+$ ]] || return 1
    (( 16#$hex > 0 )) || return 1
    echo $(( 16#$hex ))
}

cxl-dump() {
    # Usage: cxl-dump LABEL
    #
    # Run cxl-dump in the VM and store its output as
    # $TEST_OUTPUT_DIR/cxl-dump.LABEL.json. Store the zone and memory block
    # files of the VM next to the dump, too. They help to debug a failed
    # assertion.
    local label="$1"
    [ -n "$label" ] || error "cxl-dump: missing LABEL"

    cxl-dump-json "$label" || error "cxl-dump: running cxl-dump in the VM failed"

    # Read no file below /sys/bus/cxl/devices here. A recursive read there
    # reaches the mappingN files of dax devices. A read of such a file can
    # deadlock with a dax size write of the driver.
    vm-command-q "cat /proc/zoneinfo" > "$TEST_OUTPUT_DIR/zoneinfo.$label.txt"
    vm-command-q "grep -s '' /sys/devices/system/memory/block_size_bytes /sys/devices/system/memory/memory*/state; grep -H MemTotal /sys/devices/system/node/node*/meminfo" \
        > "$TEST_OUTPUT_DIR/memory.$label.txt"

    echo "cxl-dump $label: $(cxl-value 'summary()')"
}

cxl-dump-json() {
    # Usage: cxl-dump-json LABEL
    #
    # Store only the cxl-dump json output. This is the fast part of cxl-dump,
    # the one that cxl-wait repeats.
    local label="$1" json
    json=$(vm-command-q "cxl-dump") || return 1
    CXL_DUMP_FILE="$TEST_OUTPUT_DIR/cxl-dump.$label.json"
    echo "$json" > "$CXL_DUMP_FILE"
}

cxl-wait() {
    # Usage: cxl-wait LABEL EXPRESSION [TIMEOUT]
    #
    # Run cxl-dump in the VM until EXPRESSION, a Python expression on its
    # output, becomes true, TIMEOUT seconds at most (30 by default). Fail the
    # test on timeout. Store the dump as LABEL, both when the wait succeeds and
    # when it times out, and leave it loaded for cxl-assert and cxl-value.
    #
    # Hotplugging, enabling and onlining are all asynchronous, so wait for the
    # state that a test step is expected to reach instead of sleeping.
    local label="$1" expression="$2" tmo="${3:-30}"
    [ -n "$label" ] || error "cxl-wait: missing LABEL"
    [ -n "$expression" ] || error "cxl-wait: missing EXPRESSION"

    retry-until --timeout "$tmo" --message "cxl-dump: $expression" \
        'cxl-dump-json "$label" && cxl-py assert "$expression" >/dev/null' || {
        cxl-dump "$label"
        error "cxl-dump did not show the expected state in ${tmo}s: $expression"
    }
    cxl-dump "$label"
}

cxl-assert() {
    # Usage: cxl-assert EXPRESSION
    #
    # Fail the test unless EXPRESSION, a Python expression on the output of the
    # last cxl-dump or cxl-wait, is true.
    #
    # The dump is available as d, and its lists as memdevs, regions,
    # daxdevs, blocks and nodes, the endpoint ports of the memdevs as
    # endpoints. See kind cxl of e2e_json.py for the helper functions.
    local expression="$1"
    cxl-py assert "$expression" ||
        error "cxl-dump assertion failed: $expression (see $CXL_DUMP_FILE)"
    echo "cxl-dump ok: $expression"
}

cxl-value() {
    # Usage: cxl-value EXPRESSION
    #
    # Print the value of EXPRESSION, a Python expression on the output of the
    # last cxl-dump or cxl-wait.
    cxl-py eval "$1" || error "cxl-dump expression failed: $1"
}

cxl-py() {
    # Usage: cxl-py assert|eval EXPRESSION
    #
    # Evaluate EXPRESSION on the last cxl-dump output. In the assert mode exit
    # with a non-zero status unless the value is true, in the eval mode print
    # the value. The keys that the dump leaves out when their value is empty
    # or zero have that value here (kind cxl of e2e_json.py).
    [ -f "$CXL_DUMP_FILE" ] || error "cxl-py: no cxl-dump output, call cxl-dump first"
    python3 "$E2E_JSON" "$1" cxl "$2" "$CXL_DUMP_FILE"
}
