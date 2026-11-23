#!/bin/bash
# Test 2: dax slices of a sliced local memdev, shared by containers.
#
#   claim dax-shared (2Gi, mode dax), referenced by pods writer, reader-a
#                    and reader-b: one slice, one /dev/daxX.Y in all of
#                    them (single writer, many readers by convention: the
#                    device node is read-write in every pod of the claim)
#   claim dax-ro     (2Gi, mode dax, readOnly) of pod ro: a slice of its own
#                    whose device node allows reads only
#   claim numa-c     (2Gi, mode numa) of pod numa-c: a numa slice of the
#                    same memdev at the same time
#   pod plain        no claim
#
# Checks the environment and the device nodes, that what writer writes
# reader-a and reader-b read (also live, while they keep it mapped), the
# whole slice end to end with a pattern, that the ro slice cannot be
# written and is zeroed, that dax slices add nothing to the NUMA node of
# numa slices, that numa-c is the only container of the node with the
# NUMA node of its slice in its effective mems (the dax pods, plain and the
# kube-system containers do not have it), that plain has no device node,
# that a new slice on the memory of a released one is zeroed, and that the
# region goes away with the last claim.
#
# Needs the driver deployed with "deploy.sh local".
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
TEST_NAME=test2-local-dax
GiB=$((1 << 30))

log "$TEST_NAME: memdev $(serial) on $TARGET, memory block size $(block_bytes)"
reset_ns
check "the memdev is published as a sliced local device" \
    test "$(jget "$(slice_json)" 'j["attributes"]["shared"]["bool"]')" == False

log "creating claims dax-shared, dax-ro, numa-c and pods writer, reader-a, reader-b, ro, numa-c, plain"
{
    claim_yaml dax-shared cxl-local memory=2Gi mode=dax; echo ---
    claim_yaml dax-ro cxl-local memory=2Gi mode=dax readonly; echo ---
    claim_yaml numa-c cxl-local memory=2Gi; echo ---
    for p in writer reader-a reader-b; do pod_yaml "$p" claim=dax-shared; echo ---; done
    pod_yaml ro claim=dax-ro; echo ---
    pod_yaml numa-c claim=numa-c; echo ---
    pod_yaml plain
} | kapply
for p in writer reader-a reader-b ro numa-c plain; do
    wait_pod "$p" Running 300 || fail "pod $p is not Running"
done
log "all pods Running"

for p in writer reader-a ro; do
    echo "  env of $p:"
    pod_env "$p" | sed 's/^/    /'
done
DAX=$(pod_env_value writer DAX)
RO_DAX=$(pod_env_value ro DAX)
check "writer: CXL_<KEY>_MODE=dax" test "$(pod_env_value writer MODE)" == dax
check "writer: CXL_<KEY>_SIZE=2Gi" test "$(pod_env_value writer SIZE)" == $((2 * GiB))
check "writer: CXL_<KEY>_READ_ONLY=0" test "$(pod_env_value writer READ_ONLY)" == 0
check "reader-a and reader-b have the slice of writer ($DAX)" \
    test "$(pod_env_value reader-a DAX)" == "$DAX" -a "$(pod_env_value reader-b DAX)" == "$DAX"
check "ro: a slice of its own ($RO_DAX), CXL_<KEY>_READ_ONLY=1" \
    test -n "$RO_DAX" -a "$RO_DAX" != "$DAX" -a "$(pod_env_value ro READ_ONLY)" == 1
for p in writer reader-a reader-b ro; do
    d=$(pod_env_value "$p" DAX)
    echo "  $p: $(pexec "$p" ls -l "$d")"
done
check "writer: $DAX is a character device" pexec writer test -c "$DAX"
check "plain: has no $DAX" eval "! pexec plain test -e $DAX"
check "numa-c: has no $DAX" eval "! pexec numa-c test -e $DAX"
t "daxctl list" | sed 's/^/    /'

N=$(pod_env_value numa-c NUMA_NODE)
check "numa-c: CXL_<KEY>_MODE=numa on node $N" test "$(pod_env_value numa-c MODE)" == numa -a -n "$N"
mib=$(numa_node_mib "$N")
check "node $N has only the 2Gi numa slice, dax slices add nothing (MemTotal $mib MiB)" \
    test "$mib" -le 2048 -a "$mib" -ge $((2048 - 128))
check_fenced "$N" numa-c

log "the new slices are zeroed"
for p in writer ro; do
    d=$(pod_env_value "$p" DAX)
    out=$(pexec "$p" python3 /tools/dax-pattern.py zero "$d" 2>&1)
    echo "    $p: $out"
    check "$p: $d is all zeroes" grep -q "non-zero MiB 0 " <<< "$out"
done

log "single writer, many readers on $DAX"
pexec reader-a sh -c "setsid python3 /tools/dax-rw.py watch $DAX 0 /tmp/stop > /tmp/watch.log 2>&1 < /dev/null &"
watching() { pexec reader-a grep -q holding /tmp/watch.log; }
wait_for 30 "reader-a watches" watching
TEXT1="hello from writer at $(date +%T)"
pexec writer python3 /tools/dax-rw.py write "$DAX" 0 "$TEXT1" | sed 's/^/    writer: /'
for p in reader-a reader-b; do
    got=$(pexec "$p" python3 /tools/dax-rw.py read "$DAX" 0)
    echo "    $p: $got"
    check "$p reads what writer wrote" grep -qF "$TEXT1" <<< "$got"
done
TEXT2="second write at $(date +%T.%N)"
pexec writer python3 /tools/dax-rw.py write "$DAX" 0 "$TEXT2" >/dev/null
changed() { pexec reader-a grep -qF "changed: $TEXT2" /tmp/watch.log; }
check "reader-a sees the second write live in its mapping" wait_for 30 "reader-a sees the change" changed
pexec reader-a touch /tmp/stop
pexec reader-a cat /tmp/watch.log | sed 's/^/    reader-a: /'

log "the whole 2 GiB slice end to end: writer writes a pattern, reader-b verifies it"
out=$(pexec writer python3 /tools/dax-pattern.py write "$DAX" 0x5eed)
echo "    writer: $out"
out=$(pexec reader-b python3 /tools/dax-pattern.py verify "$DAX" 0x5eed 2>&1)
echo "    reader-b: $out"
check "reader-b verifies every page that writer wrote" grep -q " bad 0 " <<< "$out"

log "the read-only slice $RO_DAX"
out=$(pexec ro python3 /tools/dax-rw.py check-ro "$RO_DAX" 0 2>&1)
rc=$?
echo "$out" | sed 's/^/    ro: /'
check "ro: opening $RO_DAX for writing and writable mappings are refused" test "$rc" == 0

log "releasing dax-shared: a new slice on the same memory is zeroed"
kn delete pod writer reader-a reader-b --grace-period=1 --wait=true --timeout=300s >/dev/null
kn delete resourceclaim dax-shared --wait=true --timeout=300s >/dev/null
{ claim_yaml dax-new cxl-local memory=2Gi mode=dax; echo ---; pod_yaml new claim=dax-new; } | kapply
wait_pod new Running 300
NEW_DAX=$(pod_env_value new DAX)
echo "  new: $NEW_DAX"
t "daxctl list -d ${NEW_DAX#/dev/} 2>/dev/null" | sed 's/^/    /'
echo "    $NEW_DAX at $(dax_span "$NEW_DAX" 2>/dev/null)"
echo "    $DAX at $(dax_span "$DAX" 2>/dev/null)"
out=$(pexec new python3 /tools/dax-pattern.py zero "$NEW_DAX" 2>&1)
echo "    new: $out"
check "new: the new slice $NEW_DAX is all zeroes (no data of dax-shared)" grep -q "non-zero MiB 0 " <<< "$out"

log "releasing everything: no region is left"
delete_all
gone() { [ "$(numa_node_mib "$N")" == 0 ]; }
check "node $N has no memory in 120 s" wait_for 120 "node $N empty" gone
check "no CXL region is left on the memdev" wait_for 60 "no regions" no_regions

log "driver log of the slices (their prepared claims)"
driver_log 15m | grep -E "cxl.nri.io: claim [^ ]+: prepared" | sed 's/^[^]]*] //; s/^/    /' | tail -n 25

summary
