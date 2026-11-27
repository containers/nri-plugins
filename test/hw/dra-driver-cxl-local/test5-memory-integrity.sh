#!/bin/bash
# Test 5: does every byte that the driver hands out hold what was written,
# and are slices isolated from each other?
#
# 0. A small dax claim makes the driver create (and probe) the region of
#    the memdev. With verifyAddressing (default) a memdev whose memory
#    aliases is used only up to its distinct bytes: the log says so, and
#    the published capacity shrinks. Prints the probe result.
# 1. One dax claim of the whole published capacity: dax-scan.py writes
#    every 4 KiB page and reads it back, and reports runs of pages that hold
#    the data of another offset (aliasing).
# 2. Isolation of slices: claim a (2Gi dax, the first slice of the region),
#    claim filler (all but 4Gi) and claim c (2Gi dax, the last slice). a
#    writes a pattern, c writes another, a verifies its own slice.
#
# Needs the driver deployed with "deploy.sh local" and an idle memdev.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
TEST_NAME=test5-memory-integrity
GiB=$((1 << 30))

log "$TEST_NAME: memdev $(serial) on $TARGET"
reset_ns

capacity() { # the published memory capacity of the memdev in bytes
    python3 -c 'import sys
from decimal import Decimal
u = {"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40}
v = sys.argv[1]
for k, m in u.items():
    if v.endswith(k):
        print(int(Decimal(v[:-2]) * m)); break
else:
    print(int(v))' "$(jget "$(slice_json)" 'j["capacity"]["memory"]["value"]')"
}

log "0. the first region of the memdev and its address line probe"
before=$(capacity)
{ claim_yaml probe cxl-local memory=2Gi mode=dax; echo ---; pod_yaml probe claim=probe; } | kapply
wait_pod probe Running 300 || fail "pod probe is not Running"
driver_log 10m | grep -E "address line probe|using only|creating a region of|destroyed aliased" | sed 's/^[^]]*] //; s/^/    /'
delete_all
settled() { [ "$(capacity)" != "$before" ] || ! driver_log 10m | grep -q "using only"; }
wait_for 60 "the capacity is republished" settled
after=$(capacity)
echo "  published capacity: before the probe $((before >> 20)) MiB, after $((after >> 20)) MiB"
t "cat /var/lib/kubelet/plugins/cxl.nri.io/localProbes.json 2>/dev/null" | sed 's/^/    localProbes.json: /'

log "1. the whole published capacity as one dax slice"
{ claim_yaml whole cxl-local mode=dax; echo ---; pod_yaml whole claim=whole; } | kapply
wait_pod whole Running 600 || fail "pod whole is not Running"
DAX=$(pod_env_value whole DAX)
SIZE=$(pod_env_value whole SIZE)
echo "  whole: $DAX, $((SIZE >> 30)) GiB"
region=$(t "cat /sys/bus/cxl/devices/region*/resource /sys/bus/cxl/devices/region*/size" | tr '\n' ' ')
echo "  region start, size, slice start, end: $region$(dax_span "$DAX")"
out=$(pexec whole python3 /tools/dax-scan.py 0x77 2>&1)
echo "$out" | sed 's/^/    /'
check "every page of the slice holds what was written (no aliasing)" grep -q "^bad pages 0 " <<< "$out"
delete_all

log "2. slices a (first), filler, c (last)"
{ claim_yaml a cxl-local memory=2Gi mode=dax; echo ---; pod_yaml a claim=a; } | kapply
wait_pod a Running 300
FILL=$(((SIZE - 4 * GiB) >> 30))
{ claim_yaml filler cxl-local memory=${FILL}Gi mode=dax; echo ---; pod_yaml filler claim=filler; } | kapply
wait_pod filler Running 600
{ claim_yaml c cxl-local memory=2Gi mode=dax; echo ---; pod_yaml c claim=c; } | kapply
wait_pod c Running 300
for p in a filler c; do
    echo "  $p: $(pod_env_value "$p" DAX)"
done
# The driver logs the ranges of every slice it creates. A slice may have
# several ranges, so its resource and size do not give its placement.
driver_log 15m | grep -E "created slice" | sed 's/^[^]]*] //; s/^/    /' | tail -n 3
DA=$(pod_env_value a DAX)
DC=$(pod_env_value c DAX)
pexec a python3 /tools/dax-pattern.py write "$DA" 0xaaaa | sed 's/^/    a: /'
out=$(pexec a python3 /tools/dax-pattern.py verify "$DA" 0xaaaa 2>&1)
echo "    a: $out"
check "a: its slice holds its pattern before c writes" grep -q " bad 0 " <<< "$out"
pexec c python3 /tools/dax-pattern.py write "$DC" 0xcccc | sed 's/^/    c: /'
out=$(pexec a python3 /tools/dax-pattern.py verify "$DA" 0xaaaa 2>&1)
echo "    a: $out"
check "a: c's writes do not change a's slice" grep -q " bad 0 " <<< "$out"
delete_all
check "no CXL region is left on the memdev" wait_for 120 "no regions" no_regions

summary
