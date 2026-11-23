#!/bin/bash
# Test 4: the local memdev as a shared device (sharedMemdevs), single
# writer and many readers.
#
# Reconfigures the driver with "deploy.sh local-shared": the memdev is one devdax
# device of the whole memdev that every claim of DeviceClass
# cxl-local-shared maps, the same bytes.
#
#   claim writer          shares: 1, read-write; pod writer
#   template reader       shares: 1, readOnly; pods reader-a, reader-b, reader-c
#   pod lease-r           a lease claim: a read-only shared lease (fd)
#   claim excl            no shares request: the whole device exclusively
#
# Checks the environment, that the first claim gets a zeroed device, that
# readers read what the writer writes (also live), the whole device end to
# end with a pattern, that readers cannot write, that an exclusive claim
# waits while sharers hold the device and runs alone when they are gone,
# that the data survives idle times (the region stays), and that switching
# back to "deploy.sh local" releases the region.
#
# Needs an idle memdev: no claims of the sliced configuration.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
TEST_NAME=test4-local-shared-writer-readers
CXL_REQUEST=/usr/local/bin/cxl-request

log "$TEST_NAME: memdev $(serial) on $TARGET"
reset_ns
"$LIB_DIR/deploy.sh" local-shared 2>&1 | grep -E '^\[' | sed 's/^/  deploy: /'
D=$(slice_json)
echo "  published: $D"
check "the memdev is published shared" test "$(jget "$D" 'j["attributes"]["shared"]["bool"]')" == True
check "the shared device has the capacity shares" test "$(jget "$D" 'j["capacity"]["shares"]["value"]')" == 1M
SIZE=$(jget "$D" 'j["attributes"]["size"]["int"]')

log "the first claim (writer, one read-write share) creates the region and zeroes all $((SIZE >> 30)) GiB"
{ claim_yaml writer cxl-local-shared shares=1; echo ---; pod_yaml writer claim=writer; } | kapply
wait_pod writer Running 600 || fail "pod writer is not Running"
driver_log 10m | grep -E "zeroed new shared" | sed 's/^[^]]*] //; s/^/    /'
echo "  env of writer:"
pod_env writer | sed 's/^/    /'
DAX=$(pod_env_value writer DAX)
check "writer: CXL_<KEY>_SHARED=1, MODE=dax, READ_ONLY=0" \
    test "$(pod_env_value writer SHARED)$(pod_env_value writer MODE)$(pod_env_value writer READ_ONLY)" == 1dax0
check "writer: CXL_<KEY>_SIZE is the whole memdev ($SIZE)" test "$(pod_env_value writer SIZE)" == "$SIZE"
out=$(pexec writer python3 /tools/dax-pattern.py zero "$DAX" 2>&1)
echo "    writer: $out"
check "writer: the new shared device is all zeroes" grep -q "non-zero MiB 0 " <<< "$out"

log "readers: three pods with one read-only share each"
{ claim_yaml reader cxl-local-shared shares=1 readonly template; echo ---
  for p in reader-a reader-b reader-c; do pod_yaml "$p" template=reader; echo ---; done
  lease_template_yaml; echo ---; pod_yaml lease-r lease; } | kapply
for p in reader-a reader-b reader-c lease-r; do
    wait_pod "$p" Running 300 || fail "pod $p is not Running"
done
for p in reader-a reader-b reader-c; do
    check "$p: the same device $DAX, read-only" \
        test "$(pod_env_value "$p" DAX)$(pod_env_value "$p" READ_ONLY)$(pod_env_value "$p" SHARED)" == "${DAX}11"
done
echo "  $(pexec writer ls -l "$DAX")  (writer)"
echo "  $(pexec reader-a ls -l "$DAX")  (reader-a)"
kn get resourceclaims | sed 's/^/    /'

log "single writer, many readers"
pexec reader-a sh -c "setsid python3 /tools/dax-rw.py watch $DAX 0 /tmp/stop > /tmp/watch.log 2>&1 < /dev/null &"
watching() { pexec reader-a grep -q holding /tmp/watch.log; }
wait_for 30 "reader-a watches" watching
TEXT1="shared hello from writer at $(date +%T)"
pexec writer python3 /tools/dax-rw.py write "$DAX" 0 "$TEXT1" >/dev/null
for p in reader-b reader-c; do
    got=$(pexec "$p" python3 /tools/dax-rw.py read "$DAX" 0)
    echo "    $p: $got"
    check "$p reads what writer wrote" grep -qF "$TEXT1" <<< "$got"
done
TEXT2="shared second write at $(date +%T.%N)"
pexec writer python3 /tools/dax-rw.py write "$DAX" 0 "$TEXT2" >/dev/null
changed() { pexec reader-a grep -qF "changed: $TEXT2" /tmp/watch.log; }
check "reader-a sees the second write live in its mapping" wait_for 30 "reader-a sees the change" changed
pexec reader-a touch /tmp/stop
for p in reader-a reader-b reader-c; do
    out=$(pexec "$p" python3 /tools/dax-rw.py check-ro "$DAX" 0 2>&1)
    rc=$?
    echo "$out" | sed "s/^/    $p: /"
    check "$p: writing $DAX is refused" test "$rc" == 0
done
out=$(pexec reader-b python3 /tools/dax-rw.py write "$DAX" 64K "reader-b was here" 2>&1)
rc=$?
echo "    reader-b write: $out"
check "reader-b: a write attempt fails" test "$rc" != 0

log "a read-only shared lease of a running container (cxl-request --serial --share --read-only)"
out=$(pexec lease-r "$CXL_REQUEST" request --serial "$(serial)" --share --read-only --wait 120s --exec python3 /tools/dax-rw.py --fd 3 read 0 2>&1)
echo "$out" | sed 's/^/    lease-r: /' | cut -c1-300
check "lease-r: reads what writer wrote through a leased read-only fd" grep -qF "$TEXT2" <<< "$out"
LEASE_R=$(pexec lease-r "$CXL_REQUEST" list 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["leases"][0]["lease"])' 2>/dev/null)
echo "    lease-r: lease $LEASE_R"
out=$(pexec lease-r "$CXL_REQUEST" env --lease "$LEASE_R" --exec python3 /tools/dax-rw.py --fd 3 check-ro 0 2>&1)
rc=$?
echo "$out" | sed 's/^/    lease-r: /'
check "lease-r: the leased fd cannot be written" test "$rc" == 0

log "the whole device end to end: writer writes a pattern, reader-c verifies it"
out=$(pexec writer python3 /tools/dax-pattern.py write "$DAX" 0x5ea1 2>&1)
echo "    writer: $out"
out=$(pexec reader-c python3 /tools/dax-pattern.py verify "$DAX" 0x5ea1 2>&1)
echo "    reader-c: $out"
check "reader-c verifies every page of the $((SIZE >> 30)) GiB device" grep -q " bad 0 " <<< "$out"
pexec writer python3 /tools/dax-rw.py write "$DAX" 0 "$TEXT2" >/dev/null

log "an exclusive claim waits while sharers hold the device"
{ claim_yaml excl cxl-local-shared; echo ---; pod_yaml excl claim=excl; } | kapply
check "the scheduler cannot place excl while sharers hold the device" wait_for 60 "a FailedScheduling event of excl" unschedulable excl
check "excl is Pending while sharers hold the device" pod_is excl Pending
pod_events excl | tail -n 2 | sed 's/^/    event: /'
pexec lease-r "$CXL_REQUEST" release --all --wait 60s >/dev/null 2>&1
kn delete pod writer reader-a reader-b reader-c lease-r --grace-period=1 --wait=true --timeout=300s >/dev/null
kn delete resourceclaim writer --wait=true --timeout=120s >/dev/null
check "excl runs when the sharers are gone" wait_pod excl Running 300
check "excl: CXL_<KEY>_READ_ONLY=0 on $DAX" test "$(pod_env_value excl READ_ONLY)$(pod_env_value excl DAX)" == "0$DAX"
got=$(pexec excl python3 /tools/dax-rw.py read "$DAX" 0)
echo "    excl: $got"
check "excl reads what the writer left (not zeroed between claims)" grep -qF "$TEXT2" <<< "$got"
{ pod_yaml late template=reader; } | kapply
check "the scheduler cannot place late while excl holds the device" wait_for 60 "a FailedScheduling event of late" unschedulable late
check "a new reader is Pending while excl holds the device" pod_is late Pending
kn delete pod excl --grace-period=1 --wait=true --timeout=300s >/dev/null
kn delete resourceclaim excl --wait=true --timeout=120s >/dev/null
check "the new reader runs when excl is gone" wait_pod late Running 300
got=$(pexec late python3 /tools/dax-rw.py read "$DAX" 0)
check "late reads the same data" grep -qF "$TEXT2" <<< "$got"

log "all claims gone: the region and the data stay"
delete_all
check "the region of the idle shared device stays" test "$(region_count)" == 1
{ claim_yaml again cxl-local-shared shares=1 readonly; echo ---; pod_yaml again claim=again; } | kapply
wait_pod again Running 300
got=$(pexec again python3 /tools/dax-rw.py read "$DAX" 0)
echo "    again: $got"
check "a later claim reads the data of the idle device" grep -qF "$TEXT2" <<< "$got"
delete_all

log "driver log of the shared device (its prepared claims)"
driver_log 30m | grep -E "cxl.nri.io: claim [^ ]+: prepared" | sed 's/^[^]]*] //; s/^/    /' | tail -n 20

log "switching back to the sliced configuration releases the region"
"$LIB_DIR/deploy.sh" local 2>&1 | grep -E '^\[' | sed 's/^/  deploy: /'
check "no CXL region is left on the memdev" wait_for 120 "no regions" no_regions
check "the memdev is published sliced again" \
    test "$(jget "$(slice_json)" 'j["attributes"]["shared"]["bool"]')" == False
driver_log 5m | grep -E "cxl.nri.io: claim [^ ]+: prepared" | sed 's/^[^]]*] //; s/^/    /'

summary
