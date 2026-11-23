#!/bin/bash
# latency.sh: realistic CXL memory latency for the demo ("demo latency
# on|off|status", docs/memory/cxl/demo.md). "on" has two setups. On a
# host of two packages (sockets) the far setup makes the VMs of the demo
# cross the socket boundary when, and only when, they access their CXL
# memory devices:
# - automatic NUMA balancing, KSM and memory demotion off, so that no page
#   moves by itself;
# - every thread of each demo VM's qemu pinned to the CPUs of package 0,
#   the near package, and its memory migrated to the near nodes;
# - the pool directory, where the pool server creates the device backing
#   files, a tmpfs mounted with mpol=bind:<the far nodes> and
#   huge=within_size: the pages of every device are memory of package 1,
#   the far package, 2 MiB pages where possible;
# - with DEMO_LLM_LATENCY_LOAD=LEVEL, a background load on the far package
#   (cxl-load: writes to the far nodes' memory from the far package's
#   CPUs), which queues up the far memory controllers and so raises the
#   latency that the near CPUs see to it, like a busy CXL link or device.
# The local setup is the same without the far package: the same tunables,
# pinning and memory of the VMs, but the pool directory is a tmpfs mounted
# with mpol=interleave:<the near nodes> and huge=within_size, so that the
# device memory is on the nodes of the VMs' own package. It starts no
# load: the best placement that the host has for a model in memory, the
# baseline for the far setup.
# Every value that "on" changes is saved in the state file first, and
# "off" restores them all (it stops the load); an "on" that fails or is
# interrupted (SIGINT, SIGTERM, SIGHUP) restores what it had changed.
# Other VMs on the host are never touched: "status" lists them.

LATENCY_BIN="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source-path=SCRIPTDIR source=pool-tmpfs.sh # the pool tmpfs, also of the demo setup
source "$LATENCY_BIN/pool-tmpfs.sh"
DEMO_STATE_DIR="${DEMO_STATE_DIR:-$(dirname "$LATENCY_BIN")/run}"
STATE="$DEMO_STATE_DIR/latency.state"
POOL_DIR="${DEMO_POOL_DIR:-}"
VM_DIRS="${DEMO_VM_DIRS:-}"
NODE_SYS=/sys/devices/system/node
CPU_SYS=/sys/devices/system/cpu
NEAR_PKG=0
FAR_PKG=1
LATENCY_LOAD="${DEMO_LLM_LATENCY_LOAD:-}"
LOAD_LOG="$DEMO_STATE_DIR/cxl-load.log"
LOAD_PIDFILE="$DEMO_STATE_DIR/cxl-load.pid"
LOAD_READY_SECONDS=180       # how long the load may take to have its buffers
LOAD_STOP_SECONDS=30         # how long the load may take to stop
LOAD_LAUNCHED=""             # set once load-start has launched the load
LOAD_SAVED=""                # the level and arguments of the load, for its state line
SETUP=far                    # the setup of "on": far or local
TMPFS_SOURCE=cxl-demo-far    # the source name of the pool tmpfs in the mount table
TMPFS_SOURCES="cxl-demo-far cxl-demo-near"   # the source names of the pool tmpfs of both setups

usage() {
    cat <<EOF
usage: latency.sh on [far|local] | off | status | levels
  on       pin the demo VMs and their memory to package 0, turn off
           automatic NUMA balancing, KSM and memory demotion, and mount the
           pool directory as a tmpfs; every changed value is saved first,
           and an "on" that fails or gets SIGINT, SIGTERM or SIGHUP
           restores them. The setup (far if you give none) sets what
           else happens:
             far    the tmpfs is memory of package 1 (mpol=bind) and the
                    background load of DEMO_LLM_LATENCY_LOAD starts if it
                    is set
             local  the tmpfs is memory of package 0, spread over its nodes
                    (mpol=interleave); no load
  off      stop the load, restore every saved value (nothing to do
           without a state)
  status   show the topology, the settings, the demo VMs and their memory,
           the placement of the pool files, the background load, the other
           VMs on the host
  levels   print the levels of DEMO_LLM_LATENCY_LOAD
Environment:
  DEMO_POOL_DIR       the pool directory of the demo: empty or missing, and
                      not a mount point, at "on"
  DEMO_VM_DIRS        the e2e output directories of the demo VMs, separated
                      by spaces
  DEMO_STATE_DIR      the directory of the state file latency.state
                      (now $DEMO_STATE_DIR)
  DEMO_LLM_LATENCY_LOAD
                      off (default) or the level of the background load
                      on package 1's memory (the far setup only):
                      $(load-levels). The levels were calibrated on a
                      two-socket host with sub-NUMA clustering (SNC2);
                      the higher the level, the busier the far
                      memory. See docs/memory/cxl/demo.md
  LATENCY_LOAD_ARGS   more arguments for cxl-load after those of the
                      level, which they override (to calibrate a host)
  LATENCY_TMPFS_SIZE  the size of the pool tmpfs (default: half of the
                      memory of the nodes that the setup puts it on)
"on" and "off" need sudo without a password; "status" uses it if it can.
EOF
}

say() {
    echo "latency: $*"
}

warn() {
    echo "latency: WARNING: $*" >&2
}

die() {
    echo "latency: $*" >&2
    exit 1
}

nri-src() {
    # Usage: nri-src
    #
    # Print the nri-plugins tree: NRI_PLUGINS_SRC, else the nearest
    # directory above this script with the go.mod of module
    # github.com/containers/nri-plugins.
    local d="$LATENCY_BIN"
    if [ -n "${NRI_PLUGINS_SRC:-}" ]; then
        echo "$NRI_PLUGINS_SRC"
        return 0
    fi
    while [ "$d" != / ] && [ -n "$d" ]; do
        d="$(dirname "$d")"
        if [ -f "$d/go.mod" ] && grep -qx 'module github.com/containers/nri-plugins' "$d/go.mod"; then
            echo "$d"
            return 0
        fi
    done
    return 1
}

# The binaries built from the nri-plugins tree: empty if there is no tree.
NRI_SRC="$(nri-src)" || NRI_SRC=""
POOL_SRC_DIR="${NRI_SRC:+$NRI_SRC/scripts/testing/fake-cxl-pool}"
NUMA_PLACE="${POOL_SRC_DIR:+$POOL_SRC_DIR/bin/cxl-numa-place}"
LOAD_BIN="${POOL_SRC_DIR:+$POOL_SRC_DIR/bin/cxl-load}"

### Lists of numbers ###

list-expand() {
    # Usage: list-expand LIST
    #
    # Print the numbers of a list like "0-3,8", separated by spaces.
    local item items out=()
    IFS=, read -ra items <<< "$1"
    for item in "${items[@]}"; do
        item="${item//[[:space:]]/}"
        [ -n "$item" ] || continue
        if [[ "$item" == *-* ]]; then
            # shellcheck disable=SC2207 # numbers only
            out+=($(seq "${item%-*}" "${item#*-}"))
        else
            out+=("$item")
        fi
    done
    echo "${out[*]}"
}

list-compact() {
    # Usage: list-compact "NUMBER..."
    #
    # Print the numbers, separated by spaces, as a list with ranges, like
    # "0-3,8".
    [ -n "${1// /}" ] || { echo; return 0; }
    # shellcheck disable=SC2086 # split the numbers
    printf '%s\n' $1 | sort -nu | awk '
        function range() { return a == b ? a : a "-" b }
        NR == 1 { a = b = $1; next }
        $1 == b + 1 { b = $1; next }
        { out = out sep range(); sep = ","; a = b = $1 }
        END { print out sep range() }'
}

list-minus() {
    # Usage: list-minus "NUMBER..." "NUMBER..."
    #
    # Print the numbers of the first set that are not in the second one.
    local n out=()
    for n in $1; do
        [[ " $2 " == *" $n "* ]] || out+=("$n")
    done
    echo "${out[*]}"
}

same-list() {
    # Usage: same-list LIST LIST
    #
    # Succeed if the lists like "0-3,8" have the same numbers.
    [ "$1" == "$2" ] || [ "$(list-compact "$(list-expand "$1")")" == "$(list-compact "$(list-expand "$2")")" ]
}

### The host ###

topology() {
    # Usage: topology
    #
    # Set the NUMA topology of the host: PACKAGES (the package ids of the
    # nodes with CPUs), MEM_NODES (the nodes with memory), NEAR_NODES and
    # FAR_NODES (the nodes with CPUs and memory of packages 0 and 1),
    # NEAR_CPUS and FAR_CPUS (the CPUs of packages 0 and 1, lists like
    # "0-63,128-191"),
    # MEMLESS_NODES (the nodes with CPUs and no memory) and CPULESS_NODES
    # (the nodes with memory and no CPUs, like CXL memory: neither near
    # nor far).
    # Lists of nodes are separated by spaces.
    local n cpus pkg near_cpus="" far_cpus="" pkgs=()
    MEM_NODES=$(list-expand "$(cat "$NODE_SYS/has_memory")")
    NEAR_NODES="" FAR_NODES="" MEMLESS_NODES="" CPULESS_NODES=""
    for n in $(find "$NODE_SYS" -maxdepth 1 -name 'node[0-9]*' -printf '%f\n' | sed 's/^node//' | sort -n); do
        cpus=$(cat "$NODE_SYS/node$n/cpulist")
        if [ -z "$cpus" ]; then
            [[ " $MEM_NODES " != *" $n "* ]] || CPULESS_NODES+="$n "
            continue
        fi
        pkg=$(cat "$CPU_SYS/cpu${cpus%%[-,]*}/topology/physical_package_id")
        pkgs+=("$pkg")
        if [[ " $MEM_NODES " != *" $n "* ]]; then
            MEMLESS_NODES+="$n "
        elif [ "$pkg" == "$NEAR_PKG" ]; then
            NEAR_NODES+="$n "
        elif [ "$pkg" == "$FAR_PKG" ]; then
            FAR_NODES+="$n "
        fi
        [ "$pkg" != "$NEAR_PKG" ] || near_cpus+=",$cpus"
        [ "$pkg" != "$FAR_PKG" ] || far_cpus+=",$cpus"
    done
    PACKAGES=$(printf '%s\n' "${pkgs[@]}" | sort -nu | xargs)
    NEAR_NODES=${NEAR_NODES% } FAR_NODES=${FAR_NODES% }
    MEMLESS_NODES=${MEMLESS_NODES% } CPULESS_NODES=${CPULESS_NODES% }
    NEAR_CPUS=$(list-compact "$(list-expand "${near_cpus#,}")")
    FAR_CPUS=$(list-compact "$(list-expand "${far_cpus#,}")")
}

topology-problem() {
    # Usage: topology-problem SETUP
    #
    # Print why this host cannot have the setup SETUP, far or local, after
    # topology; print nothing if it can. The far setup needs two packages,
    # the local one the near package only.
    if [ "$1" == far ] && [ "$(wc -w <<< "$PACKAGES")" -lt 2 ]; then
        echo "this host has one package (socket): the far memory of a CXL device needs a second one"
    elif [ -z "$NEAR_NODES" ]; then
        echo "package $NEAR_PKG has no NUMA node with CPUs and memory"
    elif [ "$1" == far ] && [ -z "$FAR_NODES" ]; then
        echo "package $FAR_PKG has no NUMA node with CPUs and memory"
    fi
}

topology-line() {
    # Usage: topology-line
    #
    # Print the topology of the host in two lines, after topology.
    local others memless cpuless near far
    others=$(list-minus "$PACKAGES" "$NEAR_PKG $FAR_PKG")
    memless=$(list-compact "$MEMLESS_NODES")
    cpuless=$(list-compact "$CPULESS_NODES")
    near=$(list-compact "$NEAR_NODES")
    far=$(list-compact "$FAR_NODES")
    echo "packages $PACKAGES; near package $NEAR_PKG: nodes ${near:-none}, CPUs ${NEAR_CPUS:-none}; far package $FAR_PKG: nodes ${far:-none}, CPUs ${FAR_CPUS:-none}${others:+; packages $others not used}"
    echo "  nodes with CPUs and no memory: ${memless:-none}; with memory and no CPUs (neither near nor far): ${cpuless:-none}"
}

need-root() {
    # Usage: need-root
    #
    # Exit unless sudo works without a password.
    sudo -n true 2>/dev/null || die "this needs sudo without a password (sudo -n true fails)"
}

read-file() {
    # Usage: read-file FILE
    #
    # Print the content of FILE, read as root if it is not readable
    # otherwise.
    cat "$1" 2>/dev/null || sudo -n cat "$1" 2>/dev/null
}

write-file() {
    # Usage: write-file FILE VALUE
    #
    # Write VALUE to FILE as root, and fail unless FILE reads VALUE after
    # it.
    printf '%s\n' "$2" | sudo -n tee "$1" >/dev/null || return 1
    [ "$(read-file "$1")" == "$2" ]
}

### The state file: lines of KIND <tab> KEY <tab> VALUE ###
# KIND file: FILE had VALUE; affinity: the threads of a qemu PID:START had
# the CPU affinity VALUE; mount: the pool tmpfs of source VALUE is mounted
# at KEY; load: the process PID:START is the background load of the level
# and arguments VALUE (the level first); info: facts for status (never
# restored).

state-put() {
    # Usage: state-put KIND KEY VALUE
    #
    # Append the line KIND KEY VALUE to the state file.
    mkdir -p "$DEMO_STATE_DIR" && printf '%s\t%s\t%s\n' "$1" "$2" "$3" >> "$STATE"
}

state-values() {
    # Usage: state-values KIND KEY
    #
    # Print the values of the lines of KIND and KEY in the state file.
    [ -f "$STATE" ] && awk -F'\t' -v kind="$1" -v key="$2" '$1 == kind && $2 == key { print $3 }' "$STATE"
}

state-lines() {
    # Usage: state-lines KIND
    #
    # Print KEY <tab> VALUE of the lines of KIND in the state file.
    [ -f "$STATE" ] && awk -F'\t' -v kind="$1" '$1 == kind { print $2 "\t" $3 }' "$STATE"
}

state-drop() {
    # Usage: state-drop KIND KEY
    #
    # Remove the lines of KIND and KEY from the state file.
    awk -F'\t' -v kind="$1" -v key="$2" '!($1 == kind && $2 == key)' "$STATE" > "$STATE.tmp" &&
        mv "$STATE.tmp" "$STATE"
}

state-restorable() {
    # Usage: state-restorable
    #
    # Print the lines of the state file that "off" restores, the last
    # saved first.
    [ -f "$STATE" ] && grep -E $'^(file|affinity|mount|load)\t' "$STATE" | tac
}

set-saved() {
    # Usage: set-saved LABEL FILE VALUE
    #
    # Set FILE to VALUE, after saving its value in the state file, unless
    # it has VALUE already. Print the change under LABEL.
    local label="$1" file="$2" want="$3" now
    now=$(read-file "$file") || { warn "$label: cannot read $file"; return 1; }
    if [ "$now" == "$want" ]; then
        say "$label: $now (unchanged)"
        return 0
    fi
    state-put file "$file" "$now" || { warn "cannot write $STATE"; return 1; }
    write-file "$file" "$want" || {
        warn "$label: cannot set $file to $want: it is $(read-file "$file")"
        return 1
    }
    say "$label: $now -> $want"
}

restore-file() {
    # Usage: restore-file FILE VALUE
    #
    # Write the saved VALUE back to FILE and print it.
    write-file "$1" "$2" || { warn "cannot restore $1 to $2: it is $(read-file "$1")"; return 1; }
    say "restored $1: $2"
}

### Tunables ###

tunables-on() {
    # Usage: tunables-on
    #
    # Turn off automatic NUMA balancing, KSM (unmerging what it merged)
    # and memory demotion, saving the values that change.
    local ksm=/sys/kernel/mm/ksm
    if [ -f /proc/sys/kernel/numa_balancing ]; then
        set-saved "automatic NUMA balancing (kernel.numa_balancing)" /proc/sys/kernel/numa_balancing 0 || return 1
    fi
    if [ -f "$ksm/run" ]; then
        if [ "$(read-file "$ksm/run")" != 0 ] || [ "$(read-file "$ksm/pages_sharing")" != 0 ]; then
            set-saved "KSM (run; 2 unmerges and stops it)" "$ksm/run" 2 || return 1
        else
            say "KSM: run 0 and nothing merged (unchanged)"
        fi
    fi
    if [ -f /sys/kernel/mm/numa/demotion_enabled ]; then
        set-saved "memory demotion (demotion_enabled)" /sys/kernel/mm/numa/demotion_enabled false || return 1
    fi
}

tunables-status() {
    # Usage: tunables-status
    #
    # Print the tunables of "on" and the saved value of each it changed,
    # and the other host settings that affect where pages are.
    local f label saved
    for f in /proc/sys/kernel/numa_balancing /sys/kernel/mm/ksm/run /sys/kernel/mm/ksm/pages_sharing \
             /sys/kernel/mm/numa/demotion_enabled /proc/sys/vm/zone_reclaim_mode \
             /sys/kernel/mm/transparent_hugepage/shmem_enabled; do
        [ -f "$f" ] || continue
        label=${f#/proc/sys/}
        label=${label#/sys/kernel/mm/}
        saved=$(state-values file "$f" | head -n 1)
        echo "  $label: $(read-file "$f")${saved:+ (saved: $saved)}"
    done
    for f in numad memtierd tuned; do
        ! pgrep -x "$f" >/dev/null || echo "  $f runs: it may move pages or change settings"
    done
}

### The demo VMs ###

proc-start() {
    # Usage: proc-start PID
    #
    # Print the start time of the process PID (/proc/PID/stat starttime).
    sed 's/.*) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f20
}

qemu-pids() {
    # Usage: qemu-pids
    #
    # Print the pids of the qemu processes on the host.
    pgrep '^qemu-system' || true
}

qemu-name() {
    # Usage: qemu-name PID
    #
    # Print the name of the VM of qemu PID: the vagrant machine of its
    # disk, else its -name, else "qemu".
    local args name
    args=$(tr '\0' '\n' < "/proc/$1/cmdline" 2>/dev/null)
    name=$(sed -n 's|.*file=.*/\.vagrant/machines/\([^/]*\)/.*|\1|p' <<< "$args" | head -n 1)
    [ -n "$name" ] || name=$(grep -A1 -x -- -name <<< "$args" | sed -n '2{s/^guest=//; s/,.*//; p}')
    echo "${name:-qemu}"
}

vm-pid() {
    # Usage: vm-pid VMDIR
    #
    # Print the pid of the qemu of the VM of the e2e output directory
    # VMDIR: the qemu with its disk in VMDIR/.vagrant/machines/. Fail
    # unless exactly one qemu has.
    local dir real pid pids=()
    dir="${1%/}"
    real=$(realpath -e "$dir" 2>/dev/null) || return 1
    for pid in $(qemu-pids); do
        if tr '\0' '\n' < "/proc/$pid/cmdline" 2>/dev/null | grep -qF -e "file=$dir/.vagrant/machines/" -e "file=$real/.vagrant/machines/"; then
            pids+=("$pid")
        fi
    done
    [ "${#pids[@]}" -eq 1 ] || return 1
    echo "${pids[0]}"
}

cpus-allowed() {
    # Usage: cpus-allowed PID TID
    #
    # Print the CPU affinity of the thread TID of the process PID, a list.
    sed -n 's/^Cpus_allowed_list:[[:space:]]*//p' "/proc/$1/task/$2/status" 2>/dev/null | grep .
}

threads-pinned() {
    # Usage: threads-pinned PID
    #
    # Print "PINNED THREADS": the number of threads of the process PID
    # whose CPU affinity is the near CPUs, and the number of threads.
    local task mask pinned=0 threads=0
    for task in "/proc/$1/task"/*; do
        mask=$(cpus-allowed "$1" "${task##*/}") || continue
        threads=$((threads + 1))
        ! same-list "$mask" "$NEAR_CPUS" || pinned=$((pinned + 1))
    done
    echo "$pinned $threads"
}

proc-memory() {
    # Usage: proc-memory PID [DIR]
    #
    # Print the memory of the process PID per NUMA node in MiB and its
    # share on the near nodes, like "N1 1200.0 N2 3.0 (near 100%)"; with
    # DIR, the pages of the files in DIR after it, with their share on the
    # far nodes.
    local maps
    maps=$(read-file "/proc/$1/numa_maps") || { echo "(numa_maps not readable)"; return 0; }
    awk -v dir="${2:+$2/}" -v near=" $NEAR_NODES " -v far=" $FAR_NODES " '
        function show(t, set, label,    n, s, tot, sel) {
            for (n = 0; n <= maxn; n++) {
                if (!(n in t)) continue
                s = s sprintf("N%d %.1f ", n, t[n] / 1024)
                tot += t[n]
                if (index(set, " " n " ")) sel += t[n]
            }
            if (tot == 0) return "none"
            return s sprintf("(%s %.0f%%)", label, 100 * sel / tot)
        }
        {
            kb = 4
            if (match($0, /kernelpagesize_kB=[0-9]+/)) kb = substr($0, RSTART + 18, RLENGTH - 18) + 0
            pool = dir != "" && index($0, " file=" dir) > 0
            for (i = 3; i <= NF; i++) {
                if ($i !~ /^N[0-9]+=[0-9]+$/) continue
                split(substr($i, 2), a, "=")
                if (a[1] + 0 > maxn) maxn = a[1] + 0
                if (pool) p[a[1]] += a[2] * kb; else m[a[1]] += a[2] * kb
                if (pool) haspool = 1
            }
        }
        END {
            printf "%s", show(m, near, "near")
            if (dir != "") printf "; pool files: %s", haspool ? show(p, far, "far") : "none"
            print ""
        }' <<< "$maps"
}

vms-find() {
    # Usage: vms-find
    #
    # Set VM_PIDS, VM_STARTS (their start times), VM_NAMES and VM_PATHS
    # (the real paths of the directories) of the VMs of DEMO_VM_DIRS, and
    # exit unless each has one running qemu.
    local dir pid
    VM_PIDS=() VM_STARTS=() VM_NAMES=() VM_PATHS=()
    for dir in $VM_DIRS; do
        pid=$(vm-pid "$dir") || die "no qemu, or more than one, runs the VM of $dir"
        [[ " ${VM_PIDS[*]} " != *" $pid "* ]] || die "the VM of $dir is in DEMO_VM_DIRS twice"
        VM_PIDS+=("$pid")
        VM_STARTS+=("$(proc-start "$pid")")
        VM_NAMES+=("$(basename "$dir")")
        VM_PATHS+=("$(realpath "$dir")")
    done
}

vm-pin() {
    # Usage: vm-pin PID START NAME
    #
    # Pin every thread of the qemu PID that started at START (proc-start)
    # of the VM NAME to the near CPUs, after saving the CPU affinity of its
    # threads in the state file, and migrate its memory from the other nodes
    # to the near nodes. Print the memory of the VM per node after it. Fail,
    # and pin nothing, if PID is not that process any more.
    local pid="$1" start="$2" name="$3" main task mask saved pinned threads from to mem near
    if [ "$(proc-start "$pid")" != "$start" ] || ! main=$(cpus-allowed "$pid" "$pid"); then
        warn "$name: qemu $pid has exited"
        return 1
    fi
    saved="$main"
    for task in "/proc/$pid/task"/*; do
        mask=$(cpus-allowed "$pid" "${task##*/}") || continue
        [ "$mask" == "$main" ] || saved+=";${task##*/}=$mask"
    done
    state-put affinity "$pid:$start" "$saved" || { warn "cannot write $STATE"; return 1; }
    # A second pass pins a thread that an unpinned one started during the
    # first.
    for _ in 1 2; do
        sudo -n taskset -a -p -c "$NEAR_CPUS" "$pid" >/dev/null ||
            { warn "$name: cannot pin the threads of qemu $pid to the CPUs $NEAR_CPUS"; return 1; }
        read -r pinned threads <<< "$(threads-pinned "$pid")"
        [ "$pinned" != "$threads" ] || break
    done
    [ "$pinned" == "$threads" ] ||
        { warn "$name: only $pinned of the $threads threads of qemu $pid run on the CPUs $NEAR_CPUS"; return 1; }
    say "$name: the $threads threads of qemu $pid pinned to the CPUs $NEAR_CPUS (were $main)"
    from=$(list-compact "$(list-minus "$MEM_NODES" "$NEAR_NODES")")
    to=$(list-compact "$NEAR_NODES")
    if [ -n "$from" ]; then
        sudo -n migratepages "$pid" "$from" "$to" ||
            { warn "$name: migratepages $pid $from $to failed"; return 1; }
    fi
    mem=$(proc-memory "$pid")
    say "$name: memory MiB $mem"
    near=$(sed -n 's/.*(near \([0-9]*\)%).*/\1/p' <<< "$mem")
    [ "${near:-0}" -ge 99 ] ||
        warn "$name: ${near:-?}% of the memory of qemu $pid is on the near nodes, are they short of free memory?"
}

vm-unpin() {
    # Usage: vm-unpin PID:START SAVED
    #
    # Restore the CPU affinity of the threads of the qemu PID that SAVED
    # holds, "MAIN;TID=MASK...": MAIN for every thread but those listed.
    # Succeed without a change if PID is not the process that started at
    # START any more.
    local pid="${1%%:*}" start="${1#*:}" saved="$2" main item items
    main="${saved%%;*}"
    if [ "$(proc-start "$pid")" != "$start" ]; then
        say "qemu $pid has exited: its CPU affinity needs no restore"
        return 0
    fi
    sudo -n taskset -a -p -c "$main" "$pid" >/dev/null ||
        { warn "cannot restore the CPU affinity $main of qemu $pid"; return 1; }
    IFS=';' read -ra items <<< "${saved#"$main"}"
    for item in "${items[@]}"; do
        [ -n "$item" ] && [ -d "/proc/$pid/task/${item%%=*}" ] || continue
        sudo -n taskset -p -c "${item#*=}" "${item%%=*}" >/dev/null ||
            { warn "cannot restore the CPU affinity ${item#*=} of thread ${item%%=*} of qemu $pid"; return 1; }
    done
    say "restored the CPU affinity of qemu $pid ($(qemu-name "$pid")): $main${items[*]:+, threads ${items[*]}}"
}

vms-status() {
    # Usage: vms-status POOLDIR
    #
    # Print the demo VMs, their pinning and their memory per node: those
    # that "on" saved, else those of DEMO_VM_DIRS.
    local line pid rest start dir pinned threads now
    local -a vms=()
    mapfile -t vms < <(state-values info vm)
    if [ "${#vms[@]}" -eq 0 ]; then
        for dir in $VM_DIRS; do
            if pid=$(vm-pid "$dir"); then
                vms+=("$pid:$(proc-start "$pid") $dir")
            else
                echo "  $(basename "$dir"): no qemu runs it"
            fi
        done
    fi
    [ "${#vms[@]}" -gt 0 ] || { echo "  none known: set DEMO_VM_DIRS"; return 0; }
    for line in "${vms[@]}"; do
        pid="${line%%:*}"
        rest="${line#*:}"
        start="${rest%% *}"
        dir="${rest#* }"
        DEMO_PIDS+=" $pid"
        if [ "$(proc-start "$pid")" != "$start" ]; then
            now=$(vm-pid "$dir")
            echo "  $(basename "$dir"): qemu $pid has exited${now:+; qemu $now runs it now, not pinned by latency on (RESTARTED)}"
            [ -z "$now" ] || DEMO_PIDS+=" $now"
            continue
        fi
        read -r pinned threads <<< "$(threads-pinned "$pid")"
        echo "  $(basename "$dir"): qemu $pid, $pinned of $threads threads on the near CPUs, affinity $(cpus-allowed "$pid" "$pid")"
        echo "    memory MiB $(proc-memory "$pid" "$1")"
    done
}

other-vms-status() {
    # Usage: other-vms-status
    #
    # Print the qemu processes that are not in DEMO_PIDS, with their CPU
    # affinity and memory per node, and the other processes of over 1 GiB
    # of memory.
    local pid any=""
    for pid in $(qemu-pids); do
        [[ " $DEMO_PIDS " != *" $pid "* ]] || continue
        any=1
        echo "  $(qemu-name "$pid"): qemu $pid, affinity $(cpus-allowed "$pid" "$pid"), memory MiB $(proc-memory "$pid")"
    done
    [ -n "$any" ] || echo "  none"
    echo "other processes of over 1 GiB (RSS):"
    ps -eo pid=,rss=,comm= --sort=-rss | awk '$2 > 1048576 && $3 !~ /^qemu-system/ { printf "  %s pid %s: %.1f GiB\n", $3, $1, $2 / 1048576; if (++n == 5) exit } END { if (!n) print "  none" }'
}

### The pool tmpfs ###

pool-check() {
    # Usage: pool-check
    #
    # Exit unless the pool directory can become the pool tmpfs: an
    # absolute path of at least two levels, not a mount point, empty or
    # missing.
    local files
    [ -n "$POOL_DIR" ] || die "DEMO_POOL_DIR is not set: the pool directory of the demo"
    [[ "$POOL_DIR" == /*/* ]] || die "DEMO_POOL_DIR $POOL_DIR is not an absolute path below a top-level directory"
    [ ! -e "$POOL_DIR" ] || [ -d "$POOL_DIR" ] || die "DEMO_POOL_DIR $POOL_DIR is not a directory"
    ! mountpoint -q "$POOL_DIR" 2>/dev/null ||
        die "the pool directory $POOL_DIR is a mount point already ($(findmnt -n -o SOURCE,FSTYPE --mountpoint "$POOL_DIR")): unmount it first"
    files=$(find "$POOL_DIR" -mindepth 1 -maxdepth 1 -printf '%f ' 2>/dev/null)
    [ -z "$files" ] ||
        die "the pool directory $POOL_DIR is not empty (${files% }): latency on goes before the pool server makes its files (demo teardown first)"
}

memory-half() {
    # Usage: memory-half "NODE..."
    #
    # Print half of the memory of the nodes, a tmpfs size like "1024k".
    # Fail, and print nothing, if their meminfo files give no memory: a
    # tmpfs of size 0 has no limit.
    local n files=()
    for n in $1; do
        files+=("$NODE_SYS/node$n/meminfo")
    done
    awk '/MemTotal:/ { kb += $4 } END { if (kb < 2) exit 1; printf "%dk\n", kb / 2 }' "${files[@]}" 2>/dev/null
}

pool-mount() {
    # Usage: pool-mount
    #
    # Mount the pool tmpfs of the setup at the pool directory
    # (pool-tmpfs-mount), after saving it in the state file: of the far
    # nodes' memory in the far setup, spread over the near nodes in the
    # local one. Fail unless the mount has the memory policy and the huge
    # page option.
    local mpol size nodes
    if [ "$SETUP" == local ]; then
        nodes="$NEAR_NODES"
        mpol="interleave:$(list-compact "$NEAR_NODES")"
    else
        nodes="$FAR_NODES"
        mpol="bind:$(list-compact "$FAR_NODES")"
    fi
    size="${LATENCY_TMPFS_SIZE:-$(memory-half "$nodes")}" || size=""
    [ -n "$size" ] ||
        { warn "cannot read the memory of the nodes $nodes of the $SETUP setup from $NODE_SYS: set LATENCY_TMPFS_SIZE"; return 1; }
    state-put mount "$POOL_DIR" "$TMPFS_SOURCE" || { warn "cannot write $STATE"; return 1; }
    if ! pool-tmpfs-mount "$POOL_DIR" "$TMPFS_SOURCE" "$size" "$mpol"; then
        state-drop mount "$POOL_DIR"
        return 1
    fi
    pool-tmpfs-check "$POOL_DIR" "$mpol"
}

pool-status() {
    # Usage: pool-status DIR
    #
    # Print the mount of the pool directory DIR and the placement of its
    # files per node.
    local dir="$1" mnt files=()
    [ -n "$dir" ] || { echo "  none known: set DEMO_POOL_DIR"; return 0; }
    [ -d "$dir" ] || { echo "  $dir: missing"; return 0; }
    mnt=$(findmnt -n -o SOURCE,FSTYPE,OPTIONS -T "$dir")
    if [[ " $TMPFS_SOURCES " == *" $(findmnt -n -o SOURCE --mountpoint "$dir") "* ]]; then
        echo "  $dir: the pool tmpfs of latency on: ${mnt##* }"
    else
        echo "  $dir: on $(findmnt -n -o TARGET -T "$dir") ($mnt), not the pool tmpfs of latency on"
    fi
    mapfile -t files < <(find "$dir" -maxdepth 1 -type f | sort)
    if [ "${#files[@]}" -eq 0 ]; then
        echo "  no files"
    elif [ -x "$NUMA_PLACE" ]; then
        "$NUMA_PLACE" where "${files[@]}" 2>&1 | sed 's/^/  /'
    elif [ -n "$POOL_SRC_DIR" ]; then
        echo "  ${#files[@]} files; cxl-numa-place is not built (make -C $POOL_SRC_DIR bin/cxl-numa-place)"
    else
        echo "  ${#files[@]} files; cxl-numa-place is not built, and there is no nri-plugins source tree (NRI_PLUGINS_SRC) to build it from"
    fi
}

### The background load ###

load-levels() {
    # Usage: load-levels
    #
    # Print the levels of DEMO_LLM_LATENCY_LOAD, separated by spaces.
    echo "700 1000"
}

load-level-args() {
    # Usage: load-level-args LEVEL
    #
    # Print the arguments of cxl-load for the level LEVEL besides -numa-nodes
    # and -cpus, which are the memory and the CPUs of the far package.
    # Fail for another level.
    # Every far CPU writes to its own buffer, with a pause of 120 us after
    # each block of 256 KiB at 700, and as fast as it can at 1000, which is
    # as much as the server of the calibration takes (two sockets with
    # sub-NUMA clustering, SNC2): its memory channels are full.
    case "$1" in
        700) echo "-buffer 64M -writes 100 -pause 120us";;
        1000) echo "-buffer 64M -writes 100 -pause 0";;
        *) return 1;;
    esac
}

load-check() {
    # Usage: load-check
    #
    # Exit unless DEMO_LLM_LATENCY_LOAD is off, unset or a level, and
    # cxl-load is built, building it if it is not. Set LATENCY_LOAD to
    # empty for off.
    case "$LATENCY_LOAD" in
        ""|off) LATENCY_LOAD=""; return 0;;
    esac
    load-level-args "$LATENCY_LOAD" >/dev/null ||
        die "DEMO_LLM_LATENCY_LOAD is $LATENCY_LOAD: it is off or one of the levels $(load-levels)"
    [ -n "$POOL_SRC_DIR" ] ||
        die "no nri-plugins source tree (set NRI_PLUGINS_SRC): the load of level $LATENCY_LOAD needs cxl-load from it"
    [ -x "$LOAD_BIN" ] || make -C "$POOL_SRC_DIR" bin/cxl-load >/dev/null ||
        die "cannot build cxl-load (make -C $POOL_SRC_DIR bin/cxl-load), which the load of level $LATENCY_LOAD needs"
}

proc-alive() {
    # Usage: proc-alive PID START
    #
    # Succeed if the process PID started at START (proc-start) and is not
    # a zombie.
    local state
    [ "$(proc-start "$1")" == "$2" ] || return 1
    state=$(sed 's/.*) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f1)
    [ -n "$state" ] && [ "$state" != Z ]
}

proc-gone() {
    # Usage: proc-gone PID START
    #
    # Succeed unless the process PID that started at START is alive.
    ! proc-alive "$@"
}

wait-for() {
    # Usage: wait-for SECONDS COMMAND...
    #
    # Run COMMAND at once, and every second until it succeeds or SECONDS
    # have passed. Succeed if it did.
    local deadline=$((SECONDS + $1))
    shift
    while ! "$@"; do
        [ "$SECONDS" -lt "$deadline" ] || return 1
        sleep 1
    done
}

signal-proc() {
    # Usage: signal-proc SIGNAL PID START
    #
    # Send SIGNAL to the process PID that started at START, as root if
    # this user cannot. Succeed if the process is gone or got the signal.
    proc-alive "$2" "$3" || return 0
    kill "-$1" "$2" 2>/dev/null || sudo -n kill "-$1" "$2" 2>/dev/null || proc-gone "$2" "$3"
}

load-running() {
    # Usage: load-running
    #
    # Succeed if the log of the load says that it streams.
    grep -q '^cxl-load: running' "$LOAD_LOG" 2>/dev/null
}

load-reported() {
    # Usage: load-reported
    #
    # Succeed if the log of the load has a line of achieved bandwidth.
    grep -q -E '^cxl-load: [0-9.]+ s: ' "$LOAD_LOG" 2>/dev/null
}

load-settled() {
    # Usage: load-settled PID START
    #
    # Succeed if the load PID:START streams or has ended.
    load-running || proc-gone "$1" "$2"
}

load-save-launched() {
    # Usage: load-save-launched
    #
    # Save the background load that load-start launched, PID:START and
    # LOAD_SAVED, in the state file unless it holds a load already: wait up
    # to 10 s for the load's pid file. Succeed if no load was launched;
    # fail if the load has no pid file or the state file cannot be written.
    local i pid
    [ -n "$LOAD_LAUNCHED" ] || return 0
    [ -z "$(state-lines load)" ] || return 0
    for ((i = 0; i < 100; i++)); do
        [ ! -s "$LOAD_PIDFILE" ] || break
        sleep 0.1
    done
    [ -s "$LOAD_PIDFILE" ] || { warn "cxl-load did not start"; return 1; }
    pid=$(cat "$LOAD_PIDFILE")
    state-put load "$pid:$(proc-start "$pid")" "$LOAD_SAVED" || { warn "cannot write $STATE"; return 1; }
}

load-start() {
    # Usage: load-start
    #
    # Start cxl-load of the level LATENCY_LOAD on the CPUs and the memory
    # of the far package in the background, save it in the state file, and
    # return when its buffers are in place and it streams. Succeed without
    # a level; fail if the load ends before.
    local args pid start
    [ -n "$LATENCY_LOAD" ] || return 0
    args="-numa-nodes $(list-compact "$FAR_NODES") -cpus $FAR_CPUS $(load-level-args "$LATENCY_LOAD") ${LATENCY_LOAD_ARGS:-}"
    LOAD_SAVED="$LATENCY_LOAD $args"
    LOAD_LAUNCHED=1
    rm -f "$LOAD_PIDFILE"
    # A session of its own, so that the load outlives this script, at the
    # lowest priority, so that other work on the far CPUs goes first, and
    # with none of this script's descriptors: not its output, which a
    # caller waits to end, and not the lock (9).
    # shellcheck disable=SC2086,SC2016 # the arguments are split on purpose; $$ is the shell's own
    setsid bash -c 'echo $$ > "$1"; shift; exec nice -n 19 "$@"' _ "$LOAD_PIDFILE" "$LOAD_BIN" $args \
        > "$LOAD_LOG" 2>&1 < /dev/null 9>&- &
    load-save-launched || return 1
    pid=$(cat "$LOAD_PIDFILE")
    start=$(proc-start "$pid")
    wait-for "$LOAD_READY_SECONDS" load-settled "$pid" "$start"
    if ! load-running; then
        warn "cxl-load (level $LATENCY_LOAD) did not start streaming: $(tail -n 3 "$LOAD_LOG" | tr '\n' ' ')"
        return 1
    fi
    say "background load of level $LATENCY_LOAD: cxl-load pid $pid: $args"
    sed -n 's/^cxl-load: \(placement.*\)/latency:   cxl-load \1/p' "$LOAD_LOG"
    wait-for 15 load-reported || warn "the load has not reported its bandwidth yet: $LOAD_LOG"
    sed -n -E 's/^cxl-load: ([0-9.]+ s: .*)/latency:   cxl-load \1/p' "$LOAD_LOG" | head -n 1
}

load-forget() {
    # Usage: load-forget
    #
    # Keep the log of the background load as cxl-load.log.last, and remove
    # its pid file.
    mv -f "$LOAD_LOG" "$LOAD_LOG.last" 2>/dev/null || true
    rm -f "$LOAD_PIDFILE"
}

load-stop() {
    # Usage: load-stop PID:START LEVEL-AND-ARGUMENTS
    #
    # Stop the background load PID:START with SIGTERM, and with SIGKILL
    # if it does not end in LOAD_STOP_SECONDS. Succeed without a change if
    # PID is not the process that started at START any more.
    local pid="${1%%:*}" start="${1#*:}"
    if proc-gone "$pid" "$start"; then
        say "the background load (cxl-load $pid) has exited: nothing to stop"
        load-forget
        return 0
    fi
    signal-proc TERM "$pid" "$start" || { warn "cannot stop cxl-load $pid"; return 1; }
    if ! wait-for "$LOAD_STOP_SECONDS" proc-gone "$pid" "$start"; then
        warn "cxl-load $pid is still there after $LOAD_STOP_SECONDS s: SIGKILL"
        if ! { signal-proc KILL "$pid" "$start" && wait-for 5 proc-gone "$pid" "$start"; }; then
            warn "cannot stop cxl-load $pid"
            return 1
        fi
    fi
    say "stopped the background load of level ${2%% *} (cxl-load $pid): $(tail -n 1 "$LOAD_LOG" 2>/dev/null)"
    load-forget
}

load-status() {
    # Usage: load-status
    #
    # Print the background load that the state file holds and the last
    # line of its report.
    local line key value pid start
    local -a lines
    mapfile -t lines < <(state-lines load)
    if [ "${#lines[@]}" -eq 0 ]; then
        echo "  none"
        return 0
    fi
    for line in "${lines[@]}"; do
        IFS=$'\t' read -r key value <<< "$line"
        pid="${key%%:*}" start="${key#*:}"
        if proc-alive "$pid" "$start"; then
            echo "  level ${value%% *}: cxl-load pid $pid runs: ${value#* }"
            echo "  last report: $(grep -E '^cxl-load: [0-9.]+ s: ' "$LOAD_LOG" 2>/dev/null | tail -n 1 | sed 's/^cxl-load: //')"
        else
            echo "  level ${value%% *}: cxl-load pid $pid has exited (the last of its log: $(tail -n 1 "$LOAD_LOG" 2>/dev/null))"
        fi
    done
}

### Commands ###

restore-all() {
    # Usage: restore-all
    #
    # Restore every saved value of the state file, the last saved first,
    # dropping each from the state file when it is back. Fail if something
    # stays.
    local line kind key value failed=0
    local -a lines
    mapfile -t lines < <(state-restorable)
    for line in "${lines[@]}"; do
        IFS=$'\t' read -r kind key value <<< "$line"
        case "$kind" in
            file) restore-file "$key" "$value";;
            affinity) vm-unpin "$key" "$value";;
            mount) pool-tmpfs-umount "$key" "$value";;
            load) load-stop "$key" "$value";;
        esac && state-drop "$kind" "$key" || failed=1
    done
    return "$failed"
}

lock() {
    # Usage: lock
    #
    # Exit if another "on" or "off" runs, else hold the lock until exit.
    mkdir -p "$DEMO_STATE_DIR" || die "cannot create $DEMO_STATE_DIR"
    exec 9>>"$DEMO_STATE_DIR/latency.lock"
    flock -n 9 || die "another latency.sh on or off runs"
}

setup-name() {
    # Usage: setup-name NAME
    #
    # Print the setup NAME, far or local. Fail for another name.
    case "$1" in
        far) echo far;;
        local) echo local;;
        *) return 1;;
    esac
}

load-refuse() {
    # Usage: load-refuse
    #
    # Exit if DEMO_LLM_LATENCY_LOAD asks for a background load: the local
    # setup starts none.
    case "$LATENCY_LOAD" in
        ""|off) LATENCY_LOAD=""; return 0;;
    esac
    die "DEMO_LLM_LATENCY_LOAD is $LATENCY_LOAD: the local setup starts no background load, only the far setup does"
}

setup-apply() {
    # Usage: setup-apply
    #
    # Make the changes of the setup SETUP, each after saving its old value;
    # fail at the first change that fails. Both setups turn off the tunables,
    # pin the VMs and mount the pool tmpfs; the far one also starts the
    # background load.
    tunables-on && vms-on && pool-mount || return 1
    [ "$SETUP" == far ] || return 0
    load-start
}

cmd-on() {
    # Usage: cmd-on [SETUP]
    #
    # Turn the latency setup SETUP, far (the default) or local, on.
    local i problem tool
    SETUP=$(setup-name "${1:-far}") || die "the setup is far or local, not $1"
    [ "$SETUP" == far ] || TMPFS_SOURCE=cxl-demo-near
    need-root
    lock
    [ -z "$(state-restorable)" ] ||
        die "latency is on, or partly ($(state-restorable | wc -l) saved values in $STATE): latency.sh off first"
    topology
    problem=$(topology-problem "$SETUP")
    [ -z "$problem" ] || die "$problem"
    pool-check
    if [ "$SETUP" == far ]; then
        load-check
    else
        load-refuse
    fi
    [ -n "$VM_DIRS" ] || die "DEMO_VM_DIRS is not set: the e2e output directories of the demo VMs"
    vms-find
    for tool in taskset migratepages findmnt mountpoint flock; do
        command -v "$tool" >/dev/null || die "$tool is missing (migratepages is in numactl, the others in util-linux)"
    done
    if [ ! -x "$NUMA_PLACE" ]; then
        if [ -z "$POOL_SRC_DIR" ]; then
            warn "no nri-plugins source tree (NRI_PLUGINS_SRC) to build cxl-numa-place from: status cannot show the pool files"
        else
            make -C "$POOL_SRC_DIR" bin/cxl-numa-place >/dev/null || warn "cannot build cxl-numa-place: status cannot show the pool files"
        fi
    fi
    # From here on a signal restores what has been changed.
    trap 'on-failed "latency on was interrupted"' INT TERM HUP
    [ ! -f "$STATE" ] || mv "$STATE" "$STATE.last"
    state-put info since "$(date '+%Y-%m-%d %H:%M:%S')"
    state-put info setup "$SETUP"
    state-put info pool "$POOL_DIR"
    for i in "${!VM_PIDS[@]}"; do
        state-put info vm "${VM_PIDS[$i]}:${VM_STARTS[$i]} ${VM_PATHS[$i]}"
    done
    say "on, $SETUP setup: $(topology-line | head -n 1)"
    setup-apply || on-failed "latency on failed"
    echo "other VMs on this host (latency on does not touch them):"
    DEMO_PIDS="${VM_PIDS[*]}"
    other-vms-status
    trap - INT TERM HUP
    say "on, $SETUP setup: done; latency.sh status shows it all, latency.sh off restores it"
}

on-failed() {
    # Usage: on-failed REASON
    #
    # Print REASON, restore every value that "on" has saved so far, stop a
    # background load that it has launched, and exit 1. If something does
    # not get restored, keep the state file for "off".
    trap '' INT TERM HUP
    warn "$1: restoring what it changed"
    load-save-launched || true
    if restore-all; then
        [ ! -f "$STATE" ] || mv "$STATE" "$STATE.last"
    else
        warn "not everything is restored: the rest is in $STATE, latency.sh off again"
    fi
    exit 1
}

vms-on() {
    # Usage: vms-on
    #
    # Pin the demo VMs and migrate their memory to the near package.
    local i
    for i in "${!VM_PIDS[@]}"; do
        vm-pin "${VM_PIDS[$i]}" "${VM_STARTS[$i]}" "${VM_NAMES[$i]}" || return 1
    done
}

cmd-off() {
    if [ ! -f "$STATE" ]; then
        say "off: nothing to restore (no $STATE)"
        return 0
    fi
    need-root
    lock
    if restore-all; then
        mv "$STATE" "$STATE.last"
        say "off: every saved value is restored (the record: $STATE.last); the memory of the VMs stays where it is"
        return 0
    fi
    warn "off: not everything is restored, the rest is in $STATE: latency.sh off again"
    return 1
}

cmd-status() {
    local since setup name pool problem saved
    DEMO_PIDS=""
    topology
    since=$(state-values info since)
    setup=$(state-values info setup | tail -n 1)
    saved=$(state-restorable | wc -l)
    if state-restorable | grep -qv $'^mount\t'; then
        echo "latency: on, ${setup:-?} setup, since ${since:-?} ($saved saved values in $STATE)"
    elif [ "$saved" -gt 0 ]; then
        echo "latency: off, but the pool tmpfs of the ${setup:-?} setup stays mounted: its files were in use at latency.sh off ($STATE)"
    elif [ -n "$since" ]; then
        echo "latency: off; $STATE has no saved values left"
    else
        echo "latency: off (no saved values)"
    fi
    echo "topology: $(topology-line)"
    for name in far local; do
        problem=$(topology-problem "$name")
        [ -z "$problem" ] || echo "  latency on $name is not possible: $problem"
    done
    echo "tunables:"
    tunables-status
    pool=$(state-values info pool)
    pool="${pool:-$POOL_DIR}"
    echo "demo VMs:"
    vms-status "$pool"
    echo "pool directory:"
    pool-status "$pool"
    echo "background load:"
    load-status
    echo "other VMs on this host (latency on does not touch them):"
    other-vms-status
}

main() {
    local cmd="$1"
    case "$cmd" in
        on) cmd-on "${2:-}";;
        off) cmd-off;;
        status) cmd-status;;
        levels) load-levels;;
        ""|-h|--help|help) usage;;
        *) usage >&2; exit 2;;
    esac
}

# Run unless sourced (the checks of selftest-shell.sh source it).
if [ "${BASH_SOURCE[0]}" == "$0" ]; then
    main "$@"
fi
