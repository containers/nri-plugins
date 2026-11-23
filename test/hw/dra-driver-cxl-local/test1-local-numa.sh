#!/bin/bash
# Test 1: a sliced local memdev as extra NUMA memory of several containers.
#
# Three containers claim slices of the local CXL memdev in mode numa (the
# default of DeviceClass cxl-local), two of them opt in to steering:
#
#   numa-a  claim numa-a, 4Gi, container.steering.policy auto
#   numa-b  claim numa-b, 2Gi, container.steering.policy auto
#   numa-u  claim numa-u, 2Gi, not steered (the default policy none)
#   plain   no claim
#   plain2  no claim, created after a restart of the driver
#
# Checks that the slices are onlined on the same new NUMA node N, that N
# has the size of the slices, that N is in Mems_allowed of numa-a, numa-b
# and numa-u but not of plain, that they are the only containers of the
# node with N in their effective mems (plain and the kube-system
# containers were created before N existed), that the memory of numa-a
# and numa-b lands on N (default policy set by the driver's steering, and
# an explicit MPOL_PREFERRED), that numa-u is not steered (memory.high
# max, its own nodes and N in cpuset.mems, default allocations on DRAM,
# MPOL_PREFERRED N lands on N), that plain cannot use N, that a steered
# container past its claim keeps allocating on N with memory.high max,
# that N shrinks when numa-u and numa-b are released, that after a
# restart of the driver
# while numa-a holds N only numa-a has N and plain2 gets a cpuset.mems
# without N at its creation, and that N goes away when the claims are
# released, leaving no region on the memdev.
#
# The workloads run as descendants of the main process of the containers
# (prun), which inherit the memory policy that the driver sets when it
# steers a container; a process of kubectl exec gets it only at the next
# step, which is shown separately.
#
# Needs the driver deployed with "deploy.sh local". CONTAINER_FENCING
# (deploy.sh) none: the fencing checks fail, and numa-u is managed "none".
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
TEST_NAME=test1-local-numa
GiB=$((1 << 30))

log "$TEST_NAME: memdev $(serial) on $TARGET, memory block size $(block_bytes)"
reset_ns
check "the memdev is published as a sliced local device" \
    test "$(jget "$(slice_json)" 'j["attributes"]["shared"]["bool"]')" == False

log "creating claims numa-a (4Gi, steered), numa-b (2Gi, steered), numa-u (2Gi) and pods numa-a, numa-b, numa-u, plain"
{ claim_yaml numa-a cxl-local memory=4Gi steering=auto; echo ---; pod_yaml numa-a claim=numa-a; } | kapply
wait_pod numa-a Running 300
{
    claim_yaml numa-b cxl-local memory=2Gi steering=auto; echo ---; pod_yaml numa-b claim=numa-b; echo ---
    claim_yaml numa-u cxl-local memory=2Gi; echo ---; pod_yaml numa-u claim=numa-u; echo ---
    pod_yaml plain
} | kapply
wait_pod numa-b Running 300
wait_pod numa-u Running 300
wait_pod plain Running 300

for p in numa-a numa-b numa-u; do
    echo "  env of $p:"
    pod_env "$p" | sed 's/^/    /'
done
check "numa-a: CXL_<KEY>_MODE=numa" test "$(pod_env_value numa-a MODE)" == numa
check "numa-b: CXL_<KEY>_MODE=numa" test "$(pod_env_value numa-b MODE)" == numa
check "numa-a: CXL_<KEY>_SIZE=4Gi" test "$(pod_env_value numa-a SIZE)" == $((4 * GiB))
check "numa-b: CXL_<KEY>_SIZE=2Gi" test "$(pod_env_value numa-b SIZE)" == $((2 * GiB))
check "numa-u: CXL_<KEY>_SIZE=2Gi" test "$(pod_env_value numa-u SIZE)" == $((2 * GiB))
N=$(pod_env_value numa-a NUMA_NODE)
NB=$(pod_env_value numa-b NUMA_NODE)
NU=$(pod_env_value numa-u NUMA_NODE)
check "numa-a, numa-b and numa-u are on the same node ($N, $NB, $NU)" test -n "$N" -a "$N" == "$NB" -a "$N" == "$NU"
log "CXL node N=$N"
t "numactl -H | grep -E '^available|^node $N '" | sed 's/^/    /'
mib=$(numa_node_mib "$N")
check "node $N has the memory of the three slices, 8192 MiB (MemTotal $mib MiB)" \
    test "$mib" -le 8192 -a "$mib" -ge $((8192 - 256))

ma=$(mems_allowed numa-a)
mb=$(mems_allowed numa-b)
mu=$(mems_allowed numa-u)
mp=$(mems_allowed plain)
log "Mems_allowed_list: numa-a=$ma numa-b=$mb numa-u=$mu plain=$mp"
check "node $N is in Mems_allowed of numa-a ($ma)" in_list "$N" "$ma"
check "node $N is in Mems_allowed of numa-b ($mb)" in_list "$N" "$mb"
check "node $N is in Mems_allowed of numa-u ($mu)" in_list "$N" "$mu"
check "node $N is not in Mems_allowed of plain ($mp)" eval "! in_list $N $mp"
for p in numa-a numa-b numa-u plain; do
    echo "  cpuset.mems.effective of $p: $(pexec "$p" cat /sys/fs/cgroup/cpuset.mems.effective)"
done
check_fenced "$N" numa-a numa-b numa-u

pages_on() { # Usage: pages_on OUTPUT NODE -- 4K pages on NODE in the last "pages" line
    grep 'pages ' <<< "$1" | tail -n 1 | tr ' ' '\n' | sed -n "s/^N$2=//p" | grep . || echo 0
}

log "numa-a: 1024 MiB with the default memory policy (the driver steers it)"
out=$(prun numa-a "python3 /tools/alloc-touch.py 1024" 2>&1)
echo "$out" | sed 's/^/    /'
p=$(pages_on "$out" "$N")
check "numa-a: >= 90% of 1024 MiB of default allocations on node $N ($p pages)" test "$p" -ge $((1024 * 256 * 9 / 10))
check "numa-a: the memory holds what was written" grep -q "verify ok" <<< "$out"
log "numa-a: 1024 MiB with the default policy in a process of kubectl exec (not a descendant of the steered process)"
for i in 1 2 3; do
    out=$(pexec numa-a python3 /tools/alloc-touch.py 1024 2>&1)
    echo "    try $i: $(grep pages <<< "$out") (informational: the policy of the steering comes at the next step)"
done

log "numa-b: 1024 MiB preferring node $N (numa-touch.py)"
out=$(prun numa-b "python3 /tools/numa-touch.py 1024 $N" 2>&1)
echo "$out" | sed 's/^/    /'
p=$(pages_on "$out" "$N")
check "numa-b: >= 90% of 1024 MiB preferring node $N on it ($p pages)" test "$p" -ge $((1024 * 256 * 9 / 10))

log "numa-u: not steered"
hu=$(pexec numa-u cat /sys/fs/cgroup/memory.high)
echo "  numa-u: memory.high $hu, cpuset.mems $(pexec numa-u cat /sys/fs/cgroup/cpuset.mems), Mems_allowed $mu"
check "numa-u: memory.high is max ($hu)" test "$hu" == max
all_in() { # Usage: all_in LIST1 LIST2 -- is every node of LIST1 in LIST2
    local n
    for n in $(python3 -c 'import sys
for p in sys.argv[1].split(","):
    lo, _, hi = p.partition("-")
    print(*range(int(lo), int(hi or lo) + 1))' "$1"); do
        in_list "$n" "$2" || return 1
    done
}
check "numa-u: the nodes of plain ($mp) are in its Mems_allowed ($mu)" all_in "$mp" "$mu"
out=$(prun numa-u "python3 /tools/alloc-touch.py 1024" 2>&1)
echo "$out" | sed 's/^/    /'
p=$(pages_on "$out" "$N")
check "numa-u: <= 1% of 1024 MiB of default allocations on node $N ($p pages)" test "$p" -le $((1024 * 256 / 100))
check "numa-u: the memory holds what was written" grep -q "verify ok" <<< "$out"
out=$(prun numa-u "python3 /tools/numa-touch.py 1024 $N" 2>&1)
echo "$out" | sed 's/^/    /'
p=$(pages_on "$out" "$N")
check "numa-u: >= 90% of 1024 MiB preferring node $N on it ($p pages)" test "$p" -ge $((1024 * 256 * 9 / 10))
case "${CONTAINER_FENCING:-allowed-mems}" in
    none) want_u=none ;;
    *) want_u=fenced ;;
esac
managed() { # Usage: managed POD MANAGEMENT -- the driver logged the management of POD
    driver_log | grep -q "NRI CreateContainer: ctr=$NS/$1/main: memory $2 "
}
check "driver: numa-u is managed $want_u" managed numa-u "$want_u"
check "driver: numa-a is managed steered" managed numa-a steered
driver_log | grep -E "NRI CreateContainer: ctr=$NS/numa-[abu]/main: (memory|cpuset)" | sed 's/^[^]]*] //; s/^/    /' | cut -c1-300

log "releasing numa-u: node $N shrinks to the 6Gi of numa-a and numa-b"
kn delete pod numa-u --wait=true --timeout=300s >/dev/null
kn delete resourceclaim numa-u --wait=true --timeout=300s >/dev/null
shrunk_u() { [ "$(numa_node_mib "$N")" -le 6144 ]; }
check "node $N shrinks to <= 6144 MiB in 120 s" wait_for 120 "node $N shrinks" shrunk_u

log "plain: numa-touch.py preferring node $N must fail (not in Mems_allowed)"
out=$(prun plain "python3 /tools/numa-touch.py 64 $N" 2>&1)
rc=$?
echo "$out" | sed 's/^/    /'
check "plain: set_mempolicy(MPOL_PREFERRED, $N) is refused (exit $rc)" test "$rc" == 1
out=$(prun plain "python3 /tools/alloc-touch.py 512" 2>&1)
echo "$out" | sed 's/^/    /'
check "plain: no page of 512 MiB of default allocations on node $N" test "$(pages_on "$out" "$N")" == 0

log "numa-a and numa-b hold memory on node $N at the same time"
prun numa-a "setsid python3 /tools/alloc-touch.py --hold 2048 > /tmp/hold.log 2>&1 < /dev/null &"
prun numa-b "setsid python3 /tools/numa-touch.py --hold 1536 $N > /tmp/hold.log 2>&1 < /dev/null &"
holders_ready() { pexec numa-a grep -q ready /tmp/hold.log && pexec numa-b grep -q ready /tmp/hold.log; }
wait_for 120 "holders ready" holders_ready
ha=$(pexec numa-a cat /tmp/hold.log)
hb=$(pexec numa-b cat /tmp/hold.log)
echo "$ha" | sed 's/^/    numa-a: /'
echo "$hb" | sed 's/^/    numa-b: /'
t "grep -E 'MemTotal|MemUsed|MemFree' /sys/devices/system/node/node$N/meminfo" | sed 's/^/    /'
check "numa-a holds >= 90% of 2048 MiB on node $N" test "$(pages_on "$ha" "$N")" -ge $((2048 * 256 * 9 / 10))
check "numa-a: the held memory holds what was written" grep -q "verify ok" <<< "$ha"
check "numa-b holds >= 90% of 1536 MiB on node $N" test "$(pages_on "$hb" "$N")" -ge $((1536 * 256 * 9 / 10))

log "past the claim: numa-b (2Gi claim, holding 1.5 GiB) holds 2048 MiB more with the default policy"
prun numa-b "setsid python3 /tools/alloc-touch.py --hold 2048 > /tmp/more.log 2>&1 < /dev/null &"
more_ready() { pexec numa-b grep -q ready /tmp/more.log; }
wait_for 120 "numa-b holds 2048 MiB more" more_ready
pexec numa-b cat /tmp/more.log | sed 's/^/    /'
n3=$(pexec numa-b sh -c "grep '^anon ' /sys/fs/cgroup/memory.numa_stat" | tr ' ' '\n' | sed -n "s/^N$N=//p")
echo "  numa-b: anon memory on node $N $((n3 >> 20)) MiB, memory.high $(pexec numa-b cat /sys/fs/cgroup/memory.high), memory.max $(pexec numa-b cat /sys/fs/cgroup/memory.max), Mems_allowed $(mems_allowed numa-b)"
check "numa-b: past its 2Gi claim, >= 90% of 3584 MiB on node $N ($((n3 >> 20)) MiB)" test "$n3" -ge $(((3584 * 9 / 10) << 20))
check "numa-b: past its last waypoint, memory.high max" test "$(pexec numa-b cat /sys/fs/cgroup/memory.high)" == max
pexec numa-b pkill -f alloc-touch.py
pexec numa-a pkill -f alloc-touch.py
pexec numa-b pkill -f numa-touch.py

log "releasing numa-b: node $N shrinks to the 4Gi of numa-a"
kn delete pod numa-b --wait=true --timeout=300s >/dev/null
kn delete resourceclaim numa-b --wait=true --timeout=300s >/dev/null
shrunk() { [ "$(numa_node_mib "$N")" -le 4096 ]; }
check "node $N shrinks to <= 4096 MiB in 120 s" wait_for 120 "node $N shrinks" shrunk
log "node $N has $(numa_node_mib "$N") MiB"
check "numa-a still runs with node $N in Mems_allowed" in_list "$N" "$(mems_allowed numa-a)"

driver_pod() { # the Running driver pod
    k get pods -n kube-system -l "$DRA_POD_LABEL" -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}'
}
log "restarting the driver while numa-a holds node $N"
old_pod=$(driver_pod)
log_before=$(driver_log)
k delete pod -n kube-system "$old_pod" --wait=true --timeout=180s >/dev/null
new_pod() { local p; p=$(driver_pod) && [ -n "$p" ] && [ "$p" != "$old_pod" ]; }
check "a new driver pod is Running" wait_for 180 "a new driver pod Running" new_pod
DRIVER_POD=$(driver_pod)
seeded() { k logs -n kube-system "$DRIVER_POD" | grep -q "NRI fence: fencing CXL NUMA nodes .* of prepared claims"; }
check "driver: fences the CXL nodes of the prepared claims at start" wait_for 60 "the fence log of $DRIVER_POD" seeded
seed=$(k logs -n kube-system "$DRIVER_POD" | sed -n 's/.*NRI fence: fencing CXL NUMA nodes \(.*\) of prepared claims.*/\1/p' | tail -n 1)
echo "  fenced nodes of the prepared claims: $seed"
check "driver: node $N is among the fenced nodes of the prepared claims ($seed)" in_list "$N" "$(tr -d '[]' <<< "$seed" | tr ' ' ',')"
check_fenced "$N" numa-a

log "creating pod plain2 (no claim) after the restart"
pod_yaml plain2 | kapply
wait_pod plain2 Running 300
m2=$(pexec plain2 cat /sys/fs/cgroup/cpuset.mems)
ma2=$(mems_allowed plain2)
echo "  plain2: cpuset.mems $m2, cpuset.mems.effective $(pexec plain2 cat /sys/fs/cgroup/cpuset.mems.effective), Mems_allowed $ma2"
check "plain2: has a cpuset.mems without node $N ($m2)" eval "test -n '$m2' && ! in_list $N '$m2'"
check "plain2: node $N is not in Mems_allowed ($ma2)" eval "! in_list $N $ma2"
adjusted() { k logs -n kube-system "$DRIVER_POD" | grep -q "NRI CreateContainer: ctr=$NS/plain2/main: cpuset.mems="; }
check "driver: sets cpuset.mems of plain2 at its creation" wait_for 30 "the adjustment of plain2 in the log" adjusted
k logs -n kube-system "$DRIVER_POD" | grep -E "NRI fence|NRI CreateContainer: ctr=$NS/plain2/main: cpuset.mems=" | sed 's/^[^]]*] //; s/^/    /' | cut -c1-300 | tail -n 20

log "releasing numa-a, plain and plain2: node $N goes away, no region is left"
delete_all
gone() { [ "$(numa_node_mib "$N")" == 0 ]; }
check "node $N has no memory in 120 s" wait_for 120 "node $N empty" gone
check "no CXL region is left on the memdev" wait_for 60 "no regions" no_regions

log "driver log of the steering of numa-a and numa-b (numa-u is not steered)"
{ echo "$log_before"; driver_log 15m; } | grep -E "numa-[abu]/main: " | sed 's/^[^]]*] //; s/^/    /' | cut -c1-300 | tail -n 40

log "driver log of the slices (local:)"
{ echo "$log_before"; driver_log 15m; } | grep -E "local: " | sed 's/^[^]]*] //; s/^/    /' | tail -n 30

summary
