#!/bin/bash
# Test 3: leases of local CXL memory for running containers.
#
#   pods lease-a, lease-b  a claim of DeviceClass cxl-lease each: they
#                          lease memory with cxl-request while they run
#   pod plain              no claim
#
# lease-a and lease-b lease 2Gi of local memory in mode numa each: both
# leases are slices of the memdev on the same new node N, which comes to
# lease-a's and lease-b's cpuset.mems (Mems_allowed) while they hold the
# lease, not to plain's: no other container of the node, the companion
# pods of the leases included, has N in its effective mems. Their memory
# lands on N. Releasing a lease takes N away from the container (its
# pages move back) and shrinks N. A lease
# in mode dax passes a devdax fd to the container, which writes and reads
# it.
#
# Needs the driver deployed with "deploy.sh local".
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
TEST_NAME=test3-local-leases
CXL_REQUEST=/usr/local/bin/cxl-request

log "$TEST_NAME: memdev $(serial) on $TARGET"
reset_ns
{ lease_template_yaml; echo ---; pod_yaml lease-a lease; echo ---; pod_yaml lease-b lease; echo ---; pod_yaml plain; } | kapply
for p in lease-a lease-b plain; do
    wait_pod "$p" Running 300 || fail "pod $p is not Running"
done
echo "  env of lease-a: $(pexec lease-a sh -c 'env | grep ^CXL_ | sort | tr "\n" " "')"
check "lease-a has cxl-request" pexec lease-a test -x "$CXL_REQUEST"
base_a=$(mems_allowed lease-a)
log "Mems_allowed before leases: lease-a=$base_a lease-b=$(mems_allowed lease-b) plain=$(mems_allowed plain)"

last_json() { grep '^{' <<< "$1" | tail -n 1; }

log "lease-a: cxl-request request --source local --size 2Gi (mode numa)"
out=$(pexec lease-a "$CXL_REQUEST" request --source local --size 2Gi --wait 120s 2>&1)
rc=$?
echo "    $out" | cut -c1-400
log "lease-a: request returned $rc"
J=$(last_json "$out")
check "lease-a: the numa lease is Ready" test "$(jget "$J" 'j.get("state")' 2>/dev/null)" == Ready
LEASE_A=$(jget "$J" 'j.get("lease")' 2>/dev/null)
N=$(jget "$J" 'j.get("numaNode")' 2>/dev/null)
check "lease-a: lease $LEASE_A has node N=$N, mode numa, size 2Gi" \
    test -n "$N" -a "$(jget "$J" 'j.get("mode")' 2>/dev/null)" == numa -a "$(jget "$J" 'j.get("size")' 2>/dev/null)" == $((2 << 30))
ma=$(mems_allowed lease-a)
check "lease-a: node $N is in Mems_allowed ($ma)" in_list "$N" "$ma"
mp=$(mems_allowed plain)
check "plain: node $N is not in Mems_allowed ($mp)" eval "! in_list $N $mp"

log "lease-b: cxl-request request --source local --size 2Gi (mode numa)"
out=$(pexec lease-b "$CXL_REQUEST" request --source local --size 2Gi --wait 120s 2>&1)
J=$(last_json "$out")
LEASE_B=$(jget "$J" 'j.get("lease")' 2>/dev/null)
check "lease-b: the numa lease $LEASE_B is Ready on node $N" \
    test "$(jget "$J" 'j.get("state")' 2>/dev/null)" == Ready -a "$(jget "$J" 'j.get("numaNode")' 2>/dev/null)" == "$N"
mib=$(numa_node_mib "$N")
check "node $N has both 2Gi leases (MemTotal $mib MiB)" test "$mib" -le 4096 -a "$mib" -ge $((4096 - 256))
check "lease-b: node $N is in Mems_allowed ($(mems_allowed lease-b))" in_list "$N" "$(mems_allowed lease-b)"
check_fenced "$N" lease-a lease-b
k get pods,resourceclaims -n "$NS" | sed 's/^/    /'

pages_on() {
    grep 'pages ' <<< "$1" | tail -n 1 | tr ' ' '\n' | sed -n "s/^N$2=//p" | grep . || echo 0
}
log "lease-a and lease-b hold 1536 MiB each preferring node $N"
for p in lease-a lease-b; do
    pexec "$p" sh -c "setsid python3 /tools/numa-touch.py --hold 1536 $N > /tmp/hold.log 2>&1 < /dev/null &"
done
holding() { pexec lease-a grep -q ready /tmp/hold.log && pexec lease-b grep -q ready /tmp/hold.log; }
wait_for 120 "holders ready" holding
for p in lease-a lease-b; do
    out=$(pexec "$p" cat /tmp/hold.log)
    echo "$out" | sed "s/^/    $p: /"
    check "$p: >= 90% of 1536 MiB on node $N" test "$(pages_on "$out" "$N")" -ge $((1536 * 256 * 9 / 10))
done
out=$(pexec plain python3 /tools/numa-touch.py 64 "$N" 2>&1)
rc=$?
echo "$out" | sed 's/^/    plain: /'
check "plain: set_mempolicy(MPOL_PREFERRED, $N) is refused (exit $rc)" test "$rc" == 1

log "lease-a releases its lease while it holds memory on node $N"
out=$(pexec lease-a "$CXL_REQUEST" release --lease "$LEASE_A" --wait 120s 2>&1)
echo "    $out" | cut -c1-300
ma=$(mems_allowed lease-a)
check "lease-a: node $N is not in Mems_allowed any more ($ma)" eval "! in_list $N $ma"
check_fenced "$N" lease-b
# numa-touch.py prints a "pages" line for every SIGUSR1
reports() { pexec lease-a grep -c pages /tmp/hold.log; }
before=$(reports)
pexec lease-a pkill -USR1 -f numa-touch.py
reported() { [ "$(reports)" -gt "$before" ]; }
check "lease-a: numa-touch.py reports its pages" wait_for 30 "the pages report of lease-a" reported
out=$(pexec lease-a cat /tmp/hold.log)
echo "    lease-a: $(grep pages <<< "$out" | tail -n 1)"
check "lease-a: its pages moved off node $N" test "$(pages_on "$out" "$N")" == 0
check "lease-a: numa-touch.py lives on" pexec lease-a pgrep -f numa-touch.py
shrunk() { [ "$(numa_node_mib "$N")" -le 2048 ]; }
check "node $N shrinks to the lease of lease-b" wait_for 120 "node $N shrinks" shrunk

log "lease-b: a dax lease (cxl-request request --source local --mode dax --size 2Gi)"
out=$(pexec lease-b "$CXL_REQUEST" request --source local --mode dax --size 2Gi --wait 120s 2>&1)
echo "    $out" | cut -c1-400
J=$(last_json "$out")
LEASE_D=$(jget "$J" 'j.get("lease")' 2>/dev/null)
check "lease-b: the dax lease $LEASE_D is Ready" test "$(jget "$J" 'j.get("state")' 2>/dev/null)" == Ready
TEXT="written through a leased fd at $(date +%T)"
out=$(pexec lease-b "$CXL_REQUEST" env --lease "$LEASE_D" --exec python3 /tools/dax-rw.py --fd 3 write 0 "$TEXT" 2>&1)
echo "    $out"
out=$(pexec lease-b "$CXL_REQUEST" env --lease "$LEASE_D" --exec python3 /tools/dax-rw.py --fd 3 read 0 2>&1)
echo "    $out"
check "lease-b: reads what it wrote through the dax fd" grep -qF "$TEXT" <<< "$out"
list=$(pexec lease-b "$CXL_REQUEST" list 2>&1)
echo "    list: $list" | cut -c1-400

log "releasing every lease and pod: no region is left"
pexec lease-b "$CXL_REQUEST" release --all --wait 120s >/dev/null 2>&1
delete_all
gone() { [ "$(numa_node_mib "$N")" == 0 ]; }
check "node $N has no memory in 120 s" wait_for 120 "node $N empty" gone
check "no CXL region is left on the memdev" wait_for 120 "no regions" no_regions
check "no companion pods or claims are left" test -z "$(kn get pods,resourceclaims -o name 2>/dev/null)"

log "driver log of the leases"
driver_log 15m | grep -i -E "lease" | grep -v -E "Synchronize" | sed 's/^[^]]*] //; s/^/    /' | tail -n 20

summary
