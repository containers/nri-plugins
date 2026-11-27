#!/bin/bash
# wizard-rules.sh: the scripted set of the case "wizard"
# (docs/memory/cxl/demo.md, The wizard): a "demo wizard new" for every rule row of the wizard, in an
# order in which each one shows what it is about (a holder first, then who
# waits for it, then the holder goes), with the deletes and waits between
# them, and every choice that the wizard refuses (its copy of the
# driver's rules, which wizard_check.py checks; the e2e suite tests the
# driver's own refusals).
#
# Usage: wizard-rules.sh [--dry-run] [--from LINE] [--capture LINE:LABEL]...
#   (live)     on the demo set up with "demo setup wizard": every row in
#              turn, checks what it says, prints a summary; exit 1 if a row
#              did not do what it expects. Starts with demo wizard delete
#              --all and ends with it.
#   --dry-run  at the desk: every new row with --dry-run against the
#              sample wizard-W0 of make-wizard-samples.py
#              (DEMO_WIZARD_SAMPLE), refused or accepted as it expects; no
#              deletes, no waits.
#   --from N   start at the row of line N of this file.
#   --capture N:LABEL
#              after the row of line N: demo capture LABEL --truth (the
#              views and the ground truth of that moment); live only.
#
# A row is a line between ROWS and ROWS below: EXPECT OPTIONS.
#   Running, Ready   demo wizard new OPTIONS ends with "NAME: Running" (a
#                    claim's pod) or "NAME: Ready" (a lease), and with
#                    "; CMD exit 0" of a command other than idle (new waits
#                    until the command has ended, so a delete after it does
#                    not stop a mkfs or a write half way)
#   wait             it ends with a state that waits (waiting, Pending)
#   wait!            the same; the devices themselves cannot satisfy it
#                    (the wizard warns at once)
#   refused:WORDS    demo wizard new OPTIONS --dry-run is refused (exit 2)
#                    with WORDS ("_" for spaces) in the reason
#   delete NAMES     demo wizard delete NAMES
#   await NAME REGEX SECONDS
#                    wait until the line of NAME in demo wizard list
#                    matches REGEX
#   list             demo wizard list
# $VM2_LOCAL_SERIAL, $VM1_LOCAL_SERIAL, $LP_SERIAL, $FS0_SERIAL,
# $SHARED0_SERIAL and $POOLED0_SERIAL are the serials of the devices of
# demo setup wizard.

BIN="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(dirname "$BIN")"
WIZARD=(python3 "$BIN/wizard.py")   # demo wizard
STATE="${DEMO_STATE_DIR:-$DEMO_DIR/run}/state.env"

rows() {
    cat <<'ROWS'
# 1. Volatile local memdevs: slices (numa, dax, read-only dax), the whole
#    device, a lease, steering, firstAvailable; a shared local device that
#    the setup has not (the driver accepts it, nothing satisfies it) and a
#    device of another node (lp0 is vm2's; the local devices of vm1 and
#    vm2 have the same serial: by its serial a claim gets the one of its
#    pod's node).
Running   --name w1 --kind claim --source local --memory volatile --share=false --mode numa --size 256Mi --serial $VM2_LOCAL_SERIAL --node vm2 --command env
Running   --name w2 --kind claim --source local --memory volatile --share=false --mode dax --size 128Mi --serial any --node vm1 --read-only=false --command write "Hello from w2"
Running   --name w3 --kind claim --source local --memory volatile --share=false --mode dax --size 128Mi --node vm1 --read-only --command read
Ready     --name w4 --kind lease --source local --memory volatile --share=false --mode numa --size 128Mi --node vm2 --command env
Running   --name w5 --kind claim --source local --memory volatile --share=false --mode numa --size 128Mi --node vm2 --steering auto --command idle
wait!     --name w6 --kind claim --source local --memory volatile --share --mode dax --node vm2 --read-only --command read
Running   --name w7 --kind claim --source any --memory volatile --share=false --mode dax --size 256Mi --node vm2 --command write "Hello from w7"
wait!     --name w8 --kind claim --source local --memory persistent --share --mode fs --serial $LP_SERIAL --node vm1
list
delete w1 w2 w3 w4 w5 w6 w7 w8
Running   --name w9 --kind claim --source local --memory volatile --share=false --mode dax --size whole --serial $VM1_LOCAL_SERIAL --node vm1 --command read
delete w9
# 2. Volatile pool devices: the unshared pooled0 by its least size, too
#    large a size, shared0 exclusively and a lease that waits for it, a
#    share of it, a lease of the whole pooled0.
Running   --name w10 --kind claim --source pool --memory volatile --share=false --mode numa --size 512Mi --node any --command env
wait!     --name w11 --kind claim --source pool --memory volatile --share=false --mode dax --size 1Gi --node any
Running   --name w12 --kind claim --source pool --memory volatile --share=false --mode dax --size whole --serial $SHARED0_SERIAL --node vm1 --command write "Hello from w12"
wait      --name w13 --kind lease --source pool --memory volatile --share --mode dax --serial $SHARED0_SERIAL --read-only --node vm2 --command read
delete w12
await w13 Ready 90
Running   --name w14 --kind claim --source pool --memory volatile --share --mode dax --read-only --node vm1 --command read
delete w10 w11
Ready     --name w15 --kind lease --source pool --memory volatile --share=false --mode dax --size whole --serial $POOLED0_SERIAL --node vm2 --command write "Hello from w15"
delete w13 w14 w15
# 3. fs0 of the pool (persistent, shared, the ext4 of the setup):
#    read-only on both nodes, a read-write claim that waits until the other
#    node is gone (ReadWriteOnce), a reader of the other node that waits
#    for the writer; access cached at a path; the block device: mkfs
#    exclusively, a read-only share.
Running   --name w16 --kind claim --source pool --memory persistent --share --mode fs --serial $FS0_SERIAL --node vm1 --command read
Ready     --name w17 --kind lease --source pool --memory persistent --share --mode fs --serial $FS0_SERIAL --node vm2 --command read
wait      --name w18 --kind claim --source pool --memory persistent --share --mode fs --read-only=false --fs-type ext4 --serial $FS0_SERIAL --node vm1 --command write "Hello from w18"
delete w17
await w18 Running 120
wait      --name w19 --kind claim --source pool --memory persistent --share --mode fs --serial $FS0_SERIAL --node vm2 --command read
delete w16 w18 w19
Running   --name w20 --kind claim --source pool --memory persistent --share --mode fs --access cached --path /data --serial $FS0_SERIAL --node vm1 --command read
delete w20
Running   --name w21 --kind claim --source pool --memory persistent --share=false --mode block --read-only=false --serial $FS0_SERIAL --node vm2 --command mkfs ext4
delete w21
Ready     --name w22 --kind lease --source pool --memory persistent --share --mode block --read-only --serial $FS0_SERIAL --node vm1 --command read
delete w22
# 4. lp0, the persistent local memdev of vm2: a read-write claim and a
#    read-write lease of one superblock; mkfs xfs (no files: mkfs.xfs of
#    the image crashes with -p), a writer of hello.txt and a reader next to
#    it, both declaring xfs; mkfs ext4 through a lease (the fd); a lease
#    and a claim of any source (local first, then pool).
Running   --name w23 --kind claim --source local --memory persistent --share --mode fs --read-only=false --serial $LP_SERIAL --node vm2 --command write "Hello from w23"
Ready     --name w24 --kind lease --source local --memory persistent --share --mode fs --read-only=false --serial $LP_SERIAL --node vm2 --command write "Hello from w24"
delete w23 w24
Running   --name w25 --kind claim --source local --memory persistent --share=false --mode block --read-only=false --serial $LP_SERIAL --node vm2 --command mkfs xfs
delete w25
Running   --name w26 --kind claim --source local --memory persistent --share --mode fs --read-only=false --fs-type xfs --serial $LP_SERIAL --node vm2 --command write "Hello from w26"
Running   --name w30 --kind claim --source local --memory persistent --share --mode fs --fs-type xfs --serial $LP_SERIAL --node vm2 --command read
delete w26 w30
Ready     --name w27 --kind lease --source local --memory persistent --share=false --mode block --read-only=false --serial $LP_SERIAL --node vm2 --command mkfs ext4
delete w27
Ready     --name w28 --kind lease --source any --memory persistent --share --mode fs --node vm2 --command read
delete w28
Running   --name w29 --kind claim --source any --memory persistent --share --mode fs --node vm1 --command read
list
delete w29
# 5. Choices that the wizard refuses: each with the reason.
refused:a_persistent_device_needs_mode_block_or_fs        --memory persistent --mode dax
refused:mode_fs_needs_a_persistent_device                 --memory volatile --mode fs
refused:used_as_dax_only                                  --share --mode numa
refused:readOnly_needs_mode_dax                           --mode numa --read-only
refused:writable_in_mode_block_only_exclusively           --source local --memory persistent --share --mode block --read-only=false
refused:writable_in_mode_block_only_exclusively           --source pool --memory persistent --share --mode block --read-only=false
refused:erofs_is_a_read-only_filesystem                   --memory persistent --mode fs --read-only=false --fs-type erofs
refused:btrfs_has_no_DAX                                  --memory persistent --mode fs --fs-type btrfs --access direct
refused:not_allowed_on_this_node                          --memory persistent --mode fs --fs-type squashfs
refused:invalid_path                                      --memory persistent --mode fs --path /run/cxl/data
refused:a_lease's_filesystem                              --kind lease --memory persistent --mode fs --share --path /data
refused:mode_numa_only                                    --mode dax --steering auto
refused:cxl-request_has_no_steering                       --kind lease --mode numa --size 128Mi --steering auto
refused:waypoints                                         --mode numa --steering waypoints
refused:a_shared_device_is_used_whole                     --share --size 256Mi
refused:a_persistent_device_is_used_whole                 --memory persistent --size 256Mi
refused:cxl-request_needs                                 --kind lease --source local --mode dax --size whole --serial any
refused:a_serial_names_a_device_of_one_source             --source any --serial $SHARED0_SERIAL
refused:not_shareable                                     --source pool --share --serial $POOLED0_SERIAL
refused:in_a_pod_of_its_node_only                         --kind lease --source local --memory persistent --share --serial $LP_SERIAL --node vm1
refused:numa_memory                                       --mode numa --command read
refused:mkfs_writes_a_block_device                        --mode dax --command mkfs ext4
ROWS
}

die() {
    echo "wizard-rules: $*" >&2
    exit 1
}

dry=""
from=0
declare -A capture=()
while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) dry=1; shift;;
        --from) from="$2"; shift 2;;
        --capture)
            [[ "$2" =~ ^[0-9]+:[A-Za-z0-9._-]+$ ]] || die "--capture LINE:LABEL, not '$2'"
            capture["${2%%:*}"]="${2#*:}"
            shift 2;;
        *) die "usage: wizard-rules.sh [--dry-run] [--from LINE] [--capture LINE:LABEL]...";;
    esac
done

if [ -n "$dry" ]; then
    samples=$(mktemp -d)
    trap 'rm -rf "$samples"' EXIT
    python3 -B "$DEMO_DIR/testdata/make-wizard-samples.py" "$samples" > /dev/null ||
        die "cannot write the wizard samples"
    export DEMO_WIZARD_SAMPLE="$samples/wizard-W0"
    STATE="$DEMO_WIZARD_SAMPLE/state.env"
fi
[ -f "$STATE" ] || die "no $STATE: demo setup wizard"
VM2_LOCAL_SERIAL=$(sed -n 's/^VM2_LOCAL_SERIAL=//p' "$STATE")
VM1_LOCAL_SERIAL=$(sed -n 's/^VM1_LOCAL_SERIAL=//p' "$STATE")
LP_SERIAL=$(sed -n 's/^LP_SERIAL=//p' "$STATE")
FS0_SERIAL=0xc1ae0002
SHARED0_SERIAL=0xc1ae0001
POOLED0_SERIAL=0xc1ee0001
export VM2_LOCAL_SERIAL VM1_LOCAL_SERIAL LP_SERIAL FS0_SERIAL SHARED0_SERIAL POOLED0_SERIAL

ok=0
bad=()
note() {
    # Usage: note LINE OK|BAD TEXT
    if [ "$2" == OK ]; then
        ok=$((ok + 1))
    else
        bad+=("line $1: $3")
    fi
    echo "wizard-rules: line $1: $2: $3"
}

await() {
    # Usage: await LINE NAME REGEX SECONDS
    local line="$1" name="$2" re="$3" secs="$4" t0=$SECONDS got
    while :; do
        got=$("${WIZARD[@]}" list | awk -v n="$name" '$1 == n')
        if [[ "$got" =~ $re ]]; then
            note "$line" OK "$name: $got"
            return 0
        fi
        if [ $((SECONDS - t0)) -ge "$secs" ]; then
            note "$line" BAD "$name did not match /$re/ in ${secs}s: $got"
            return 1
        fi
        sleep 2
    done
}

run-row() {
    # Usage: run-row LINE EXPECT OPTIONS
    local line="$1" want="$2" opts="$3" out rc last args words
    # The options are shell words with $SERIAL variables.
    eval "args=($opts)"
    case "$want" in
        refused:*)
            out=$("${WIZARD[@]}" new "${args[@]}" --dry-run 2>&1)
            rc=$?
            words="${want#refused:}"
            if [ "$rc" == 2 ] && grep -qF -- "${words//_/ }" <<< "$out"; then
                note "$line" OK "$(tail -n 1 <<< "$out")"
            else
                note "$line" BAD "expected $want, exit $rc: $(tail -n 1 <<< "$out")"
            fi
            return
            ;;
    esac
    if [ -n "$dry" ]; then
        out=$("${WIZARD[@]}" new "${args[@]}" --dry-run 2>&1)
        rc=$?
        if [ "$rc" == 0 ]; then
            note "$line" OK "accepted: $(sed -n 's/^#  *Warning: /warning: /p' <<< "$out" | head -n 1)"
        else
            note "$line" BAD "refused: $(tail -n 1 <<< "$out")"
        fi
        return
    fi
    out=$("${WIZARD[@]}" new "${args[@]}" 2>&1)
    rc=$?
    echo "$out"
    last=$(grep -E '^[a-z0-9-]+: ' <<< "$out" | tail -n 1)
    case "$want" in
        Running|Ready)
            if [[ "$last" =~ ^[a-z0-9-]+:\ $want ]] && ! [[ "$last" =~ exit\ [1-9]|still\ running ]]; then
                note "$line" OK "$last"
            else
                note "$line" BAD "expected $want and its command done: $last"
            fi;;
        wait|wait!)
            if [[ "$last" =~ ^[a-z0-9-]+:\ (waiting|Pending) ]]; then note "$line" OK "$last"; else note "$line" BAD "expected a wait: $last"; fi;;
    esac
}

[ -n "$dry" ] || "${WIZARD[@]}" delete --all || die "demo wizard delete --all failed"
n=0
start=$(grep -n "<<'ROWS'" "${BASH_SOURCE[0]}" | head -n 1 | cut -d: -f1)
while IFS= read -r row; do
    n=$((n + 1))
    line=$((start + n))
    [ "$line" -ge "$from" ] || continue
    row="${row#"${row%%[![:space:]]*}"}"
    [ -n "$row" ] && [ "${row:0:1}" != "#" ] || continue
    word="${row%% *}"
    rest="${row#"$word"}"
    rest="${rest#"${rest%%[![:space:]]*}"}"
    case "$word" in
        delete)
            # shellcheck disable=SC2086 # NAMES
            [ -n "$dry" ] || "${WIZARD[@]}" delete $rest || note "$line" BAD "delete $rest failed";;
        await)
            # shellcheck disable=SC2086 # NAME REGEX SECONDS
            [ -n "$dry" ] || await "$line" $rest;;
        list)
            [ -n "$dry" ] || "${WIZARD[@]}" list;;
        *)
            run-row "$line" "$word" "$rest";;
    esac
    if [ -z "$dry" ] && [ -n "${capture[$line]:-}" ]; then
        "$BIN/demo" capture "${capture[$line]}" --truth </dev/null || note "$line" BAD "capture ${capture[$line]} failed"
        echo "wizard-rules: line $line: captured ${capture[$line]}"
    fi
done < <(rows)
[ -n "$dry" ] || "${WIZARD[@]}" delete --all
echo "wizard-rules: ${ok} ok, ${#bad[@]} not as expected"
for b in "${bad[@]}"; do
    echo "  $b"
done
[ "${#bad[@]}" == 0 ]
