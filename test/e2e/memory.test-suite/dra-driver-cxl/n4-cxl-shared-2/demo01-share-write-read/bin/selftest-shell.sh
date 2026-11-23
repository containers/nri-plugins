#!/bin/bash
# selftest-shell.sh: checks of pool-tmpfs.sh, latency.sh and the Vagrantfile
# patch of the e2e library without root, a cluster or a VM (selftest.py runs
# it). sudo, findmnt, mount, umount, taskset and migratepages are stubs in a
# temporary directory, /sys is a fake tree, and the background load is a
# stand-in. Prints the failed checks and exits 1 if there are any.

BIN="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
FAILED="$TMP/failed"
STUBS="$TMP/stubs"
export STUB_MOUNTS="$TMP/mounts" STUB_LOG="$TMP/stub.log"

cleanup() {
    # Stop what the checks started, and remove the temporary directory.
    [ ! -s "$TMP/state/cxl-load.pid" ] || kill "$(cut -d: -f1 "$TMP/state/cxl-load.pid")" 2>/dev/null
    pkill -f -- "$TMP/" 2>/dev/null
    rm -rf "$TMP"
}
trap cleanup EXIT

expect() {
    # Usage: expect NAME CONDITION...
    #
    # Run the CONDITION command; record NAME as failed if it fails.
    local name="$1"
    shift
    "$@" || echo "$name" >> "$FAILED"
}

contains() {
    # Usage: contains TEXT WORD
    #
    # Succeed if TEXT has the string WORD.
    [[ "$1" == *"$2"* ]]
}

gone() {
    # Usage: gone PID
    #
    # Succeed if PID is a number and no process has it.
    [ -n "$1" ] && ! kill -0 "$1" 2>/dev/null
}

make-stubs() {
    # Usage: make-stubs
    #
    # Write the commands that stand for the privileged ones into STUBS, which
    # goes first in PATH: they log to STUB_LOG and keep the mounts in
    # STUB_MOUNTS (lines of TARGET, SOURCE and OPTIONS, separated by tabs).
    mkdir -p "$STUBS"
    : > "$STUB_MOUNTS"
    : > "$STUB_LOG"
    # sudo runs the command with the PATH STUB_SUDO_PATH, if set, like the
    # secure_path of a real sudo.
    cat > "$STUBS/sudo" <<'EOF'
#!/bin/bash
[ "$1" != -n ] || shift
exec env PATH="${STUB_SUDO_PATH:-$PATH}" "$@"
EOF
    cat > "$STUBS/findmnt" <<'EOF'
#!/bin/bash
cols="" dir=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) cols="$2"; shift;;
        --mountpoint|-T) dir="$2"; shift;;
    esac
    shift
done
line=$(awk -F'\t' -v d="$dir" '$1 == d' "$STUB_MOUNTS" | head -n 1)
[ -n "$line" ] || exit 1
IFS=$'\t' read -r target source options <<< "$line"
out=()
for col in ${cols//,/ }; do
    case "$col" in
        TARGET) out+=("$target");;
        SOURCE) out+=("$source");;
        OPTIONS) out+=("$options");;
    esac
done
echo "${out[*]}"
EOF
    cat > "$STUBS/mount" <<'EOF'
#!/bin/bash
echo "mount LIBMOUNT_FORCE_MOUNT2=${LIBMOUNT_FORCE_MOUNT2:-unset} $*" >> "$STUB_LOG"
if [ -n "${STUB_MOUNT_FAIL:-}" ]; then
    echo "mount: $STUB_MOUNT_FAIL" >&2
    exit 32
fi
while [ $# -gt 2 ]; do
    [ "$1" != -o ] || opts="$2"
    shift
done
printf '%s\t%s\trw,%s\n' "$2" "$1" "$opts" >> "$STUB_MOUNTS"
EOF
    cat > "$STUBS/umount" <<'EOF'
#!/bin/bash
echo "umount $*" >> "$STUB_LOG"
awk -F'\t' -v d="${*: -1}" '$1 != d' "$STUB_MOUNTS" > "$STUB_MOUNTS.new"
mv "$STUB_MOUNTS.new" "$STUB_MOUNTS"
EOF
    for cmd in taskset migratepages; do
        # shellcheck disable=SC2016 # expanded when the stub runs
        printf '#!/bin/bash\necho "%s $*" >> "$STUB_LOG"\n' "$cmd" > "$STUBS/$cmd"
    done
    # A stand-in for cxl-load: logs, then waits to be stopped.
    printf '#!/bin/bash\necho "cxl-load: placement stub"\nexec sleep 600\n' > "$STUBS/cxl-load"
    chmod +x "$STUBS"/*
}

mount-add() {
    # Usage: mount-add DIR SOURCE OPTIONS
    #
    # Put a mount into the table of the stubs.
    printf '%s\t%s\t%s\n' "$1" "$2" "$3" >> "$STUB_MOUNTS"
}

spread-policy-test() {
    # Check which nodes pool-tmpfs-spread-policy spreads over.
    local sys="$TMP/spread" cpus mem want got
    mkdir -p "$sys"
    # CPUs, memory, policy: nodes with memory and CPUs only; none with one.
    while read -r cpus mem want; do
        echo "$cpus" > "$sys/has_cpu"
        echo "$mem" > "$sys/has_memory"
        got=$(NODE_SYS="$sys" bash -c 'set -eo pipefail; source "$1"; pool-tmpfs-spread-policy' _ "$BIN/pool-tmpfs.sh")
        expect "spread policy for CPUs $cpus and memory $mem: want '${want#-}', got '$got'" test "$got" == "${want#-}"
    done <<'EOF'
0-7 1-2,5-6 interleave:1-2,5-6
0-1,3 0-2 interleave:0-1
0-3 0-3 interleave:0-3
0,2,4 0-5 interleave:0,2,4
0 0-1 -
0 0 -
EOF
    got=$(NODE_SYS="$TMP/no-such-dir" bash -c 'set -eo pipefail; source "$1"; pool-tmpfs-spread-policy; echo "rc $?"' _ "$BIN/pool-tmpfs.sh")
    expect "spread policy without the node lists: want nothing and rc 0, got '$got'" test "$got" == "rc 0"
}

mount-test() {
    # Check the mount command that pool-tmpfs-mount runs, and what it says
    # when the mount fails.
    local dir="$TMP/mount-dir" out
    : > "$STUB_LOG"
    PATH="$STUBS:$PATH" bash -c 'source "$1"; pool-tmpfs-mount "$2" cxl-demo-pool 8g interleave:1-2,5-6' _ "$BIN/pool-tmpfs.sh" "$dir"
    expect "mount with a policy forces mount(2) and gives the policy as one option" \
        grep -q "^mount LIBMOUNT_FORCE_MOUNT2=always -t tmpfs -o huge=within_size,size=8g,.*,mpol=interleave:1-2,5-6 cxl-demo-pool $dir\$" "$STUB_LOG"
    : > "$STUB_LOG"
    PATH="$STUBS:$PATH" bash -c 'source "$1"; pool-tmpfs-mount "$2" cxl-demo-pool 8g' _ "$BIN/pool-tmpfs.sh" "$dir"
    expect "mount without a policy does not force mount(2)" grep -q "^mount LIBMOUNT_FORCE_MOUNT2=unset " "$STUB_LOG"
    out=$(STUB_MOUNT_FAIL="you may not set LIBMOUNT_FORCE_MOUNT2" PATH="$STUBS:$PATH" bash -c 'source "$1"; pool-tmpfs-mount "$2" x 8g bind:1' _ "$BIN/pool-tmpfs.sh" "$dir" 2>&1)
    expect "a failed mount says why" contains "$out" "you may not set LIBMOUNT_FORCE_MOUNT2"
    out=$(PATH="$STUBS:$PATH" bash -c 'set -eo pipefail; source "$1"; pool-tmpfs-check "$2" bind:1 || echo "rc $?"; echo REACHED' _ "$BIN/pool-tmpfs.sh" "$TMP/not-mounted" 2>&1)
    expect "pool-tmpfs-check of a directory that is no mount fails and goes on under set -e: $out" contains "$out" "rc 1"$'\n'REACHED
}

umount-test() {
    # Check pool-tmpfs-umount and pool-tmpfs-users under set -e and
    # pipefail: no user, a user of a file that is there, a user of a removed
    # file.
    local dir="$TMP/umount-dir" out rc pid
    mkdir -p "$dir"
    : > "$STUB_LOG"
    mount-add "$dir" cxl-demo-pool "rw,huge=within_size"
    out=$(PATH="$STUBS:$PATH" bash -c 'set -eo pipefail; source "$1"; pool-tmpfs-umount "$2" cxl-demo-pool; echo REACHED' _ "$BIN/pool-tmpfs.sh" "$dir" 2>&1)
    expect "umount of a directory that nobody uses ends in the caller's next line under set -e: $out" contains "$out" "tmpfs unmounted"$'\n'REACHED
    expect "umount of a directory that nobody uses is not lazy" grep -qx "umount $dir" "$STUB_LOG"
    out=$(PATH="$STUBS:$PATH" bash -c 'set -eo pipefail; source "$1"; pool-tmpfs-users "$2"; echo "rc $?"' _ "$BIN/pool-tmpfs.sh" "$dir" 2>&1)
    expect "pool-tmpfs-users with no user succeeds under pipefail: $out" test "$out" == "rc 0"

    mount-add "$dir" cxl-demo-pool "rw,huge=within_size"
    : > "$STUB_LOG"
    sleep 600 > "$dir/open.txt" &
    pid=$!
    rc=0
    out=$(PATH="$STUBS:$PATH" bash -c 'set -eo pipefail; source "$1"; pool-tmpfs-umount "$2" cxl-demo-pool' _ "$BIN/pool-tmpfs.sh" "$dir" 2>&1) || rc=$?
    expect "umount refuses a directory with a file in use (rc $rc): $out" test "$rc" == 1
    expect "umount names the process and the file in use" contains "$out" "$pid $dir/open.txt"
    expect "umount does not unmount a directory with a file in use" test ! -s "$STUB_LOG"
    kill "$pid"

    : > "$STUB_LOG"
    sleep 600 > "$dir/removed.txt" &
    pid=$!
    rm -f "$dir/open.txt" "$dir/removed.txt"
    rc=0
    out=$(PATH="$STUBS:$PATH" bash -c 'set -eo pipefail; source "$1"; pool-tmpfs-umount "$2" cxl-demo-pool' _ "$BIN/pool-tmpfs.sh" "$dir" 2>&1) || rc=$?
    expect "umount of a directory with only removed files in use succeeds (rc $rc): $out" test "$rc" == 0
    expect "umount of a directory with only removed files in use is lazy" grep -qx "umount -l $dir" "$STUB_LOG"
    expect "umount names the process that maps a removed file" contains "$out" "processes $pid still map removed files"
    kill "$pid"
}

fake-host() {
    # Usage: fake-host
    #
    # Write a fake /sys under TMP/sys: two packages of four CPUs, with
    # FAKE_NODES_PER_PACKAGE nodes each (1 or 2, default 1; CPUs 0-3 and
    # 4-7 split evenly, 4 GiB per node).
    local sys="$TMP/sys" cpu n per="${FAKE_NODES_PER_PACKAGE:-1}" nodes
    nodes=$((2 * per))
    rm -rf "$sys"
    mkdir -p "$sys/node"
    echo "0-$((nodes - 1))" > "$sys/node/has_memory"
    echo "0-$((nodes - 1))" > "$sys/node/has_cpu"
    for ((n = 0; n < nodes; n++)); do
        mkdir -p "$sys/node/node$n"
        echo "$((n * 4 / per))-$(((n + 1) * 4 / per - 1))" > "$sys/node/node$n/cpulist"
        echo "Node $n MemTotal:     4194304 kB" > "$sys/node/node$n/meminfo"
    done
    for cpu in 0 1 2 3 4 5 6 7; do
        mkdir -p "$sys/cpu/cpu$cpu/topology"
        echo $((cpu / 4)) > "$sys/cpu/cpu$cpu/topology/physical_package_id"
    done
}

# shellcheck disable=SC2329 # latency.sh calls the functions that this replaces
latency-source() {
    # Usage: latency-source
    #
    # Source latency.sh into this shell with the stubs and the fake /sys
    # (fake-host), its state and pool directories in TMP, and the commands
    # that find or pin the VMs replaced.
    export DEMO_STATE_DIR="$TMP/state" DEMO_POOL_DIR="$TMP/pool-on"
    rm -rf "$DEMO_STATE_DIR" "$DEMO_POOL_DIR"
    mkdir -p "$DEMO_STATE_DIR"
    PATH="$STUBS:$PATH"
    # shellcheck source-path=SCRIPTDIR source=latency.sh
    source "$BIN/latency.sh"
    NODE_SYS="$TMP/sys/node" CPU_SYS="$TMP/sys/cpu"
    POOL_DIR="$DEMO_POOL_DIR"
    VM_DIRS=none
    POOL_SRC_DIR="$TMP" NUMA_PLACE=/bin/true LOAD_BIN="$STUBS/cxl-load"
    vms-find() { VM_PIDS=() VM_STARTS=() VM_NAMES=() VM_PATHS=(); }
    vms-on() { :; }
}

memory-test() {
    # Check memory-half: half of the memory of some nodes, and a failure for
    # none.
    local got rc=0
    fake-host
    got=$( (latency-source; topology; memory-half "$FAR_NODES") 2>&1)
    expect "half of 4 GiB: want 2097152k, got '$got'" test "$got" == 2097152k
    got=$( (latency-source; topology; memory-half "0 1") 2>&1)
    expect "half of 8 GiB: want 4194304k, got '$got'" test "$got" == 4194304k
    echo "Node 1 HugePages_Total: 0" > "$TMP/sys/node/node1/meminfo"
    got=$( (latency-source; topology; memory-half "$FAR_NODES") 2>&1) || rc=$?
    expect "memory-half fails and prints nothing without MemTotal (rc $rc, '$got')" test "$rc/$got" == "1/"
}

pin-test() {
    # Check that vm-pin pins nothing if the process is not the one that
    # was found.
    local rc=0
    fake-host
    : > "$STUB_LOG"
    (latency-source; vm-pin $$ 1 vm-test) >/dev/null 2>&1 || rc=$?
    expect "vm-pin refuses a process with another start time (rc $rc)" test "$rc" == 1
    expect "vm-pin runs no taskset on it" test ! -s "$STUB_LOG"
}

# shellcheck disable=SC2329 # latency.sh calls the functions that this replaces
mount-order-test() {
    # Check that pool-mount saves the mount before it mounts, and takes the
    # line back if the mount fails.
    local seen
    fake-host
    seen=$( (latency-source; topology
             pool-tmpfs-mount() { grep -q $'^mount\t' "$STATE" && echo "saved before the mount"; return 0; }
             pool-tmpfs-check() { :; }
             pool-mount) 2>&1)
    expect "pool-mount saves the mount line before it mounts: $seen" contains "$seen" "saved before the mount"
    seen=$( (latency-source; topology
             pool-tmpfs-mount() { return 1; }
             pool-mount || grep -c $'^mount\t' "$STATE") 2>&1)
    expect "pool-mount drops the mount line if the mount fails: $seen" contains "$seen" "0"
}

load-test() {
    # Check that a load is saved with its start time and stopped by its
    # state line, and that a load that was launched and not saved is.
    local out pid
    fake-host
    out=$( (latency-source; topology
            LATENCY_LOAD=700
            LOAD_READY_SECONDS=5
            # shellcheck disable=SC2329 # called by load-start
            load-running() { true; }
            # shellcheck disable=SC2329 # called by load-start
            load-reported() { true; }
            load-start
            echo "STATE: $(grep $'^load\t' "$STATE" | cut -f2)"
            restore-all
            echo "LEFT: $(grep -c $'^load\t' "$STATE")") 2>&1)
    pid=$(sed -n 's/^STATE: \([0-9]*\):[0-9]*$/\1/p' <<< "$out")
    expect "load-start saves PID:START of the load, restore-all stops it: $out" gone "$pid"
    expect "restore-all drops the load line: $out" contains "$out" "LEFT: 0"

    # Launched, with the pid file written, and not saved.
    sleep 600 &
    pid=$!
    out=$( (latency-source
            echo "$pid" > "$LOAD_PIDFILE"
            LOAD_LAUNCHED=1 LOAD_SAVED="700 -numa-nodes 1"
            load-save-launched
            load-save-launched
            echo "LINES: $(grep -c $'^load\t' "$STATE")"
            restore-all) 2>&1)
    expect "load-save-launched saves a launched load once: $out" contains "$out" "LINES: 1"
    expect "restore-all stops a load that load-save-launched saved" gone "$pid"
    kill "$pid" 2>/dev/null
}

# shellcheck disable=SC2329 # latency.sh calls the functions that this replaces
on-test() {
    # Check that a signal, and a failure, halfway through cmd-on restore
    # what it changed: the tunable, the tmpfs mount and the load.
    local out rc pid i loadpid
    for how in signal failure; do
        fake-host
        echo 1 > "$TMP/tunable"
        : > "$STUB_MOUNTS"
        : > "$STUB_LOG"
        rm -f "$TMP/on.out"
        (
            latency-source
            tunables-on() { set-saved "a tunable" "$TMP/tunable" 0; }
            if [ "$how" == signal ]; then
                # Stay in load-start until the signal comes.
                load-running() { false; }
            else
                load-start() { warn "no load"; return 1; }
            fi
            LATENCY_LOAD=700
            cmd-on
        ) > "$TMP/on.out" 2>&1 &
        pid=$!
        if [ "$how" == signal ]; then
            for ((i = 0; i < 100; i++)); do
                [ ! -s "$TMP/state/cxl-load.pid" ] || break
                sleep 0.1
            done
            loadpid=$(cat "$TMP/state/cxl-load.pid" 2>/dev/null)
            kill -TERM "$pid"
        fi
        rc=0
        wait "$pid" || rc=$?
        out=$(cat "$TMP/on.out")
        expect "$how: latency.sh on exits 1 (rc $rc): $out" test "$rc" == 1
        expect "$how: the tunable is back: $out" test "$(cat "$TMP/tunable")" == 1
        expect "$how: the pool tmpfs is unmounted: $out" test ! -s "$STUB_MOUNTS"
        expect "$how: the state file is kept as .last" test -f "$TMP/state/latency.state.last"
        expect "$how: no state file is left" test ! -f "$TMP/state/latency.state"
        if [ "$how" == signal ]; then
            expect "signal: the message says it was interrupted: $out" contains "$out" "interrupted: restoring what it changed"
            expect "signal: the load is stopped" gone "$loadpid"
        else
            expect "failure: the message says on failed: $out" contains "$out" "latency on failed: restoring what it changed"
            expect "failure: the load was never started" test ! -e "$TMP/state/cxl-load.pid"
        fi
    done
}

# shellcheck disable=SC2329 # latency.sh calls the functions that this replaces
setup-run() {
    # Usage: setup-run SETUP [LOAD]
    #
    # Print what cmd-on SETUP, cmd-status and cmd-off print on the fake host
    # (as the caller made it with fake-host) with the stubs, a tunable in
    # TMP/tunable, the load LOAD if given, and the pinning and the lists
    # of other processes left out; MARK lines say what the host looked like
    # after cmd-on and after cmd-off. The state, the mounts and the stub log
    # stay for the caller.
    echo 1 > "$TMP/tunable"
    : > "$STUB_MOUNTS"
    : > "$STUB_LOG"
    (
        latency-source
        other-vms-status() { :; }
        tunables-status() { :; }
        vms-status() { :; }
        tunables-on() { set-saved "a tunable" "$TMP/tunable" 0; }
        LATENCY_LOAD="${2:-}"
        cmd-on "$1"
        echo "MARK on rc $?"
        echo "MARK mounts: $(cut -f2,3 "$STUB_MOUNTS" | tr '\t' ' ')"
        echo "MARK state: $(tr '\t' ' ' < "$STATE" | grep -E '^(info setup|mount) ' | tr '\n' ';')"
        echo "MARK load: $(compgen -G "$TMP/state/cxl-load.pid" || echo none)"
        cmd-status
        cmd-off
        echo "MARK off rc $?"
        echo "MARK after off: tunable $(cat "$TMP/tunable"), mounts $(grep -c . "$STUB_MOUNTS")"
    ) 2>&1
}

far-test() {
    # Check the far setup: the tmpfs of the far nodes' memory (bind), "far"
    # in the state and the status, and all of it restored by cmd-off.
    local out
    FAKE_NODES_PER_PACKAGE=2 fake-host
    out=$(setup-run far)
    expect "far: on succeeds: $out" contains "$out" "MARK on rc 0"
    expect "far: the pool tmpfs is the far nodes' memory, bound: $out" contains "$out" "MARK mounts: cxl-demo-far rw,huge=within_size,size=4194304k,"
    expect "far: the pool tmpfs has the policy bind:2-3: $out" contains "$out" "mpol=bind:2-3"
    expect "far: the state says far: $out" contains "$out" "MARK state: info setup far;mount "
    expect "far: the status names the setup: $out" contains "$out" "latency: on, far setup, since "
    expect "far: off succeeds: $out" contains "$out" "MARK off rc 0"
    expect "far: off restores the tunable and the mount: $out" \
        contains "$out" "MARK after off: tunable 1, mounts 0"
}

local-test() {
    # Check the local setup: the tunables off as in the far one, the pool
    # tmpfs interleaved over the near nodes, no load;
    # "local" in the state and the status; restored by cmd-off. Check what
    # it needs of a host.
    local out
    FAKE_NODES_PER_PACKAGE=2 fake-host
    out=$(setup-run local)
    expect "local: on succeeds: $out" contains "$out" "MARK on rc 0"
    expect "local: the pool tmpfs is the near nodes' memory: $out" contains "$out" "MARK mounts: cxl-demo-near rw,huge=within_size,size=4194304k,"
    expect "local: the pool tmpfs is interleaved over the near nodes 0-1: $out" contains "$out" "mpol=interleave:0-1"
    expect "local: the mount forces mount(2) for the comma: $(cat "$STUB_LOG")" grep -q "^mount LIBMOUNT_FORCE_MOUNT2=always .*mpol=interleave:0-1 cxl-demo-near " "$STUB_LOG"
    expect "local: the state says local: $out" contains "$out" "MARK state: info setup local;mount "
    expect "local: no load runs: $out" contains "$out" "MARK load: none"
    expect "local: the tunable is turned off by on: $out" contains "$out" "a tunable: 1 -> 0"
    expect "local: the status names the setup: $out" contains "$out" "latency: on, local setup, since "
    expect "local: the status shows the pool tmpfs of latency on: $out" contains "$out" "the pool tmpfs of latency on: "
    expect "local: off succeeds: $out" contains "$out" "MARK off rc 0"
    expect "local: off restores the tunable and the mount: $out" \
        contains "$out" "MARK after off: tunable 1, mounts 0"
    expect "local: the state is kept as .last" test -f "$TMP/state/latency.state.last"

    # A background load is refused, before anything changes; "off" is no load.
    out=$(setup-run local 700)
    expect "local with a load is refused: $out" contains "$out" "DEMO_LLM_LATENCY_LOAD is 700: the local setup starts no background load"
    expect "local with a load mounts nothing: $out" test ! -s "$STUB_MOUNTS"
    expect "local with a load leaves the tunable: $out" test "$(cat "$TMP/tunable")" == 1
    out=$(setup-run local off)
    expect "local with the load off is fine: $out" contains "$out" "MARK on rc 0"

    # One socket: local works (the near package is all there is), far does not.
    FAKE_NODES_PER_PACKAGE=2 fake-host
    rm -rf "$TMP/sys/node/node2" "$TMP/sys/node/node3"
    echo 0-1 > "$TMP/sys/node/has_memory"
    echo 0-1 > "$TMP/sys/node/has_cpu"
    out=$( (latency-source; topology; echo "far: $(topology-problem far)"; echo "local: [$(topology-problem local)]") 2>&1)
    expect "one package: far is refused: $out" contains "$out" "far: this host has one package (socket)"
    expect "one package: local is possible: $out" contains "$out" "local: []"
    out=$(setup-run local)
    expect "one package: local on succeeds: $out" contains "$out" "MARK on rc 0"
    expect "one package: the pool tmpfs is interleaved over the nodes 0-1: $out" contains "$out" "mpol=interleave:0-1"
    expect "one package: status says that far is not possible: $out" contains "$out" "latency on far is not possible: this host has one package"
    expect "one package: status does not say that local is not possible: $out" test "${out#*latency on local is not possible}" == "$out"
    out=$(setup-run far)
    expect "one package: far on is refused: $out" contains "$out" "this host has one package (socket)"

    # Without a node with CPUs and memory in package 0 there is no local setup.
    fake-host
    echo "" > "$TMP/sys/node/has_memory"
    out=$( (latency-source; topology; echo "local: $(topology-problem local)") 2>&1)
    expect "package 0 without memory: local is refused: $out" contains "$out" "local: package 0 has no NUMA node with CPUs and memory"
}

setup-name-test() {
    # Check the names of the latency setups: latency.sh takes far and local;
    # the demo command and the setup test also take off. Check
    # that demo setup refuses a bad name, and a load that is not with far,
    # before it changes anything.
    local demo="$BIN/demo" setup="$BIN/../../test98-demo-setup/code.var.sh"
    local name sh_want demo_want got out rc
    # NAME, then what latency.sh and what the demo and the setup test print.
    while read -r name sh_want demo_want; do
        got=$(bash -c 'source "$1"; setup-name "$2"' _ "$BIN/latency.sh" "$name" 2>&1) || got="refused"
        expect "latency.sh setup-name $name: want $sh_want, got $got" test "$got" == "$sh_want"
        got=$(eval "$(sed -n '/^latency-setup() {/,/^}/p' "$demo")"; latency-setup "$name" 2>&1) || got="refused"
        expect "bin/demo latency-setup $name: want $demo_want, got $got" test "$got" == "$demo_want"
        got=$(eval "$(sed -n '/^llm-latency-name() {/,/^}/p' "$setup")"; llm-latency-name "$name" 2>&1) || got="refused"
        expect "test90 llm-latency-name $name: want $demo_want, got $got" test "$got" == "$demo_want"
    done <<'EOF'
far far far
on refused refused
local local local
off refused off
bogus refused refused
Far refused refused
EOF
    got=$(eval "$(sed -n '/^latency-setup() {/,/^}/p' "$demo")"; unset DEMO_LLM_LATENCY; latency-setup 2>&1)
    expect "bin/demo latency-setup without a name and DEMO_LLM_LATENCY: want off, got $got" test "$got" == off
    got=$(eval "$(sed -n '/^latency-setup() {/,/^}/p' "$demo")"; DEMO_LLM_LATENCY=local latency-setup 2>&1)
    expect "bin/demo latency-setup of DEMO_LLM_LATENCY=local: want local, got $got" test "$got" == local

    # latency.sh refuses another name before it asks for sudo.
    rc=0
    out=$(bash "$BIN/latency.sh" on bogus 2>&1) || rc=$?
    expect "latency.sh on bogus is refused (rc $rc): $out" test "$rc" == 1
    expect "latency.sh on bogus says what the setups are: $out" contains "$out" "the setup is far or local, not bogus"

    rc=0
    out=$(DEMO_STATE_DIR="$TMP/demo-state" DEMO_LLM_LATENCY=bogus "$demo" setup llm-share 2>&1) || rc=$?
    expect "demo setup refuses DEMO_LLM_LATENCY=bogus (rc $rc): $out" test "$rc" == 1
    expect "demo setup names the setups: $out" contains "$out" "DEMO_LLM_LATENCY is bogus: off, local or far"
    rc=0
    out=$(DEMO_STATE_DIR="$TMP/demo-state" DEMO_LLM_LATENCY=local DEMO_LLM_LATENCY_LOAD=700 "$demo" setup llm-share 2>&1) || rc=$?
    expect "demo setup refuses a load with the local setup (rc $rc): $out" test "$rc" == 1
    expect "demo setup says that local starts no load: $out" contains "$out" "DEMO_LLM_LATENCY_LOAD=700 needs DEMO_LLM_LATENCY=far (the local setup starts no load)"
    rc=0
    out=$(DEMO_STATE_DIR="$TMP/demo-state" DEMO_LLM_LATENCY_LOAD=700 "$demo" setup llm-share 2>&1) || rc=$?
    expect "demo setup refuses a load with latency off (rc $rc): $out" test "$rc" == 1
    expect "demo setup says that a load needs far: $out" contains "$out" "DEMO_LLM_LATENCY_LOAD=700 needs DEMO_LLM_LATENCY=far"
    expect "the refused setups left no state behind" test ! -e "$TMP/demo-state"
}

cleanup-test() {
    # Check that llm-cleanup of the setup test runs latency.sh off when the
    # setup started latency.sh on, and only then.
    local demo="$TMP/demo" setup="$BIN/../../test98-demo-setup/code.var.sh"
    mkdir -p "$demo/bin"
    # shellcheck disable=SC2016 # expanded when the stub runs
    printf '#!/bin/bash\necho "latency.sh $* DEMO_STATE_DIR=$DEMO_STATE_DIR" >> "$STUB_LOG"\n' > "$demo/bin/latency.sh"
    chmod +x "$demo/bin/latency.sh"
    : > "$STUB_LOG"
    (
        # shellcheck disable=SC2034 # llm-cleanup reads them
        DEMO_DIR="$demo" DEMO_STATE_DIR="$TMP/cleanup-state" LLM_MIRROR_PID="" POOL_TMPFS="" LLM_LATENCY_ON=""
        eval "$(sed -n '/^llm-cleanup() {/,/^}/p' "$setup")"
        llm-cleanup
        expect "llm-cleanup does not run latency.sh off when the setup did not start on" test ! -s "$STUB_LOG"
        LLM_LATENCY_ON=1
        llm-cleanup
        expect "llm-cleanup runs latency.sh off with the state directory of the setup" \
            grep -qx "latency.sh off DEMO_STATE_DIR=$TMP/cleanup-state" "$STUB_LOG"
        expect "llm-cleanup forgets that latency was started" test -z "$LLM_LATENCY_ON"
    )
}

thp-test() {
    # Check vm-thp-patch-vagrantfile of the e2e library on a copy of
    # files/Vagrantfile.in: it patches once, keeps the file as it was when it
    # cannot patch or the Ruby is not valid, and does nothing for a missing
    # file.
    local e2e="$BIN/../../../../.." dir="$TMP/thp" once rc
    mkdir -p "$dir/bin"
    printf '#!/bin/sh\nexit 1\n' > "$dir/bin/ruby-rejects"
    chmod +x "$dir/bin/ruby-rejects"
    cp "$e2e/files/Vagrantfile.in" "$dir/Vagrantfile"
    cp "$e2e/files/Vagrantfile.in" "$dir/Vagrantfile.orig"
    printf 'nothing to patch\n' > "$dir/no-anchor"
    (
        eval "$(sed -n '/^vm-thp-patch-vagrantfile() {/,/^}/p' "$e2e/lib/vm.bash")"
        vm-thp-patch-vagrantfile "$dir/Vagrantfile" > /dev/null
        expect "vm-thp-patch-vagrantfile adds the prctl after the provider line" grep -q "prctl.call(41, 0, 0, 0, 0)" "$dir/Vagrantfile"
        expect "vm-thp-patch-vagrantfile puts it right after the anchor line" \
            test "$(grep -A2 -x "ENV\['VAGRANT_DEFAULT_PROVIDER'\] = 'qemu'" "$dir/Vagrantfile" | sed -n 3p)" == "# Ruby disables transparent huge pages (THP) for its own process when the"
        if command -v ruby > /dev/null; then
            expect "the patched Vagrantfile is valid Ruby" ruby -c "$dir/Vagrantfile" > /dev/null
        fi
        once=$(cat "$dir/Vagrantfile")
        vm-thp-patch-vagrantfile "$dir/Vagrantfile" > /dev/null
        expect "a second vm-thp-patch-vagrantfile changes nothing" test "$(cat "$dir/Vagrantfile")" == "$once"
        rc=0
        vm-thp-patch-vagrantfile "$dir/no-anchor" > /dev/null 2>&1 || rc=$?
        expect "vm-thp-patch-vagrantfile fails on a file without the anchor line" test "$rc" == 1
        expect "vm-thp-patch-vagrantfile leaves it as it was" test "$(cat "$dir/no-anchor")" == "nothing to patch"
        rc=0
        vm-thp-patch-vagrantfile "$dir/missing" > /dev/null 2>&1 || rc=$?
        expect "vm-thp-patch-vagrantfile succeeds for a missing file" test "$rc" == 0
        # ruby that finds the patched file invalid: the file stays as it was.
        cp "$dir/Vagrantfile.orig" "$dir/Vagrantfile"
        ln -sf ruby-rejects "$dir/bin/ruby"
        rc=0
        PATH="$dir/bin:$PATH" vm-thp-patch-vagrantfile "$dir/Vagrantfile" > /dev/null 2>&1 || rc=$?
        expect "vm-thp-patch-vagrantfile fails if ruby rejects the patched file" test "$rc" == 1
        expect "vm-thp-patch-vagrantfile then leaves the Vagrantfile as it was" cmp -s "$dir/Vagrantfile" "$dir/Vagrantfile.orig"
        expect "vm-thp-patch-vagrantfile leaves no temporary file" test "$(find "$dir" -name '*.thp-patch*' | wc -l)" == 0
    )
}

make-stubs
spread-policy-test
mount-test
umount-test
thp-test
cleanup-test
memory-test
far-test
local-test
setup-name-test
pin-test
mount-order-test
load-test
on-test

if [ -s "$FAILED" ]; then
    echo "selftest-shell: FAIL"
    sed 's/^/  /' "$FAILED"
    exit 1
fi
echo "selftest-shell: ok"
