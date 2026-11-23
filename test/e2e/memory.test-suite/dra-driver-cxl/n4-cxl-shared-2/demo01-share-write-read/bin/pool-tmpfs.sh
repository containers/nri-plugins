# pool-tmpfs.sh: the tmpfs of the pool directory of the llm-share demo,
# sourced by latency.sh and by the setup and teardown of the demo
# (test98-demo-setup and test99-demo-teardown of n4-cxl-shared-2). The pool server
# creates the backing files of its devices there, so the pages of this
# tmpfs are the memory of the pool devices: 2 MiB pages where possible
# (huge=within_size) with and without the latency setup. Without it the
# pages are spread evenly over the memory nodes of the host that also have
# CPUs (mpol=interleave:NODES, pool-tmpfs-spread-policy), so that the
# readers do not all wait on the one node that the first writer happened
# to run on; the latency setup binds them to the far nodes instead
# (mpol=bind:NODES).
# Mounting, unmounting and listing the users of the directory need sudo
# without a password; a memory policy needs it to pass the environment
# variable LIBMOUNT_FORCE_MOUNT2 to mount (sudoers ALL, or SETENV).
# Functions only: they return their status and never end the shell that
# sources them, also under set -e and set -o pipefail.

# The source names of the pool tmpfs in the mount table: that of the
# demo without the latency setup (latency.sh names its own).
# shellcheck disable=SC2034 # the setup and teardown of the demo use them
POOL_TMPFS_SOURCE=cxl-demo-pool
# The size of the pool tmpfs without the latency setup: the capacity of
# the pool of the demo's server (pool.source.sh).
# shellcheck disable=SC2034 # the setup of the demo uses it
POOL_TMPFS_SIZE=8g

pool-tmpfs-spread-policy() {
    # Usage: pool-tmpfs-spread-policy
    #
    # Print the memory policy that spreads the pages of a tmpfs evenly over
    # the NUMA nodes of the host that have both memory and CPUs, like
    # interleave:1-2,5-6. A node with memory and no CPUs, like the memory
    # of a CXL device or HBM, is left out. Print nothing if the host has
    # fewer than two such nodes or the lists are unreadable: there is
    # nothing to spread over.
    local sys="${NODE_SYS:-/sys/devices/system/node}" nodes
    # The first file is the nodes with CPUs, the second the nodes with
    # memory; print the nodes in both, as a list with ranges.
    nodes=$(awk '
        function mark(list, set,    n, parts, i, bounds, k) {
            n = split(list, parts, ",")
            for (i = 1; i <= n; i++) {
                if (split(parts[i], bounds, "-") == 2) {
                    for (k = bounds[1]; k <= bounds[2]; k++) {
                        set[k] = 1
                        if (k > max) max = k
                    }
                } else if (parts[i] != "") {
                    set[parts[i] + 0] = 1
                    if (parts[i] + 0 > max) max = parts[i] + 0
                }
            }
        }
        function flush() {
            if (first == "") return
            out = out sep (first == last ? first : first "-" last)
            sep = ","
        }
        FNR == 1 { file++ }
        file == 1 { mark($0, cpus) }
        file == 2 { mark($0, mem) }
        END {
            for (k = 0; k <= max; k++) {
                if (!(k in cpus) || !(k in mem)) continue
                if (first != "" && k == last + 1) { last = k; continue }
                flush()
                first = last = k
            }
            flush()
            print out
        }' "$sys/has_cpu" "$sys/has_memory" 2>/dev/null) || nodes=""
    # A list with a range or a comma has more than one node.
    [[ "$nodes" != *[-,]* ]] || echo "interleave:$nodes"
    return 0
}

pool-tmpfs-mount() {
    # Usage: pool-tmpfs-mount DIR SOURCE SIZE [MPOL]
    #
    # Mount a tmpfs named SOURCE at the directory DIR, created if missing,
    # of at most SIZE (like 8g or 1024k), owned by the user, with 2 MiB
    # pages where possible (huge=within_size) and the memory policy MPOL
    # (mpol=MPOL, like bind:5-6 or interleave:1-2,5-6) if given. Return
    # non-zero, and print why, if it cannot be mounted.
    local dir="$1" src="$2" size="$3" mpol="${4:-}" opts env=() out
    opts="huge=within_size,size=$size,mode=0755,uid=$(id -u),gid=$(id -g)${mpol:+,mpol=$mpol}"
    mkdir -p "$dir" || { echo "pool-tmpfs: cannot create $dir" >&2; return 1; }
    # A node list can have a comma (1-2,5-6). mount(8) splits the options at
    # their commas and gives each to the kernel on its own, which refuses
    # "5-6" ("tmpfs: Unknown parameter"). The mount(2) call of libmount
    # gives the kernel the options as one string, and it reads a comma
    # followed by a digit as part of the node list.
    [ -z "$mpol" ] || env=(LIBMOUNT_FORCE_MOUNT2=always)
    out=$(sudo -n "${env[@]}" mount -t tmpfs -o "$opts" "$src" "$dir" 2>&1) ||
        { echo "pool-tmpfs: cannot mount a tmpfs at $dir with $opts: ${out:-no message}" >&2; return 1; }
}

pool-tmpfs-check() {
    # Usage: pool-tmpfs-check DIR [MPOL]
    #
    # Print the options of the tmpfs mounted at DIR. Return non-zero
    # unless it has huge=within_size and, if MPOL is given, mpol=MPOL.
    local dir="$1" mpol="${2:-}" opts
    opts=$(findmnt -n -o OPTIONS --mountpoint "$dir") || opts=""
    if [[ ",$opts," != *",huge=within_size,"* ]] || { [ -n "$mpol" ] && [[ ",$opts," != *",mpol=$mpol,"* ]]; }; then
        echo "pool-tmpfs: the tmpfs at $dir has the options ${opts:-none}, not huge=within_size${mpol:+ and mpol=$mpol}" >&2
        return 1
    fi
    echo "pool-tmpfs: $dir is a tmpfs $(findmnt -n -o SOURCE --mountpoint "$dir"): $opts"
}

pool-tmpfs-users() {
    # Usage: pool-tmpfs-users DIR
    #
    # Print "PID PATH" for every file in DIR that a process maps or has
    # open; PATH ends with " (deleted)" for a removed file. Print nothing
    # if there is none. Always succeed.
    # Each of the two searches may find nothing, or lose a process that
    # exits while it is read: neither is a failure.
    {
        sudo -n grep -sF -- " $1/" /proc/[0-9]*/maps |
            sed -nE 's|^/proc/([0-9]+)/maps:([^ ]+ +){5}(.*)$|\1 \3|p' || true
        sudo -n find /proc/[0-9]*/fd -maxdepth 1 -lname "$1/*" -printf '%h %l\n' 2>/dev/null |
            sed -E 's|^/proc/([0-9]+)/fd |\1 |' || true
    } | sort -u
}

pool-tmpfs-umount() {
    # Usage: pool-tmpfs-umount DIR SOURCE
    #
    # Unmount the pool tmpfs SOURCE at DIR, and print the files that go
    # with it. Unmount it lazily if processes map or hold only removed
    # files of it (their memory goes when they let go); fail if a process
    # uses a file of it that is still there. Nothing to do if SOURCE is not
    # mounted at DIR.
    local dir="$1" src="$2" users live files line
    if [ "$(findmnt -n -o SOURCE --mountpoint "$dir")" != "$src" ]; then
        echo "pool-tmpfs: $dir: the tmpfs $src is not mounted any more"
        return 0
    fi
    users=$(pool-tmpfs-users "$dir") || users=""
    live=$(grep -v ' (deleted)$' <<< "$users" | grep . || true)
    if [ -n "$live" ]; then
        echo "pool-tmpfs: $dir: the tmpfs stays mounted, processes use its files (PID FILE):" >&2
        while IFS= read -r line; do
            echo "    $line" >&2
        done <<< "$live"
        return 1
    fi
    files=$(find "$dir" -mindepth 1 -maxdepth 1 -printf '%f ' 2>/dev/null) || files=""
    [ -z "$files" ] || echo "pool-tmpfs: $dir: the files of the tmpfs go with it: ${files% }"
    if [ -n "$users" ]; then
        sudo -n umount -l "$dir" || { echo "pool-tmpfs: cannot unmount $dir lazily" >&2; return 1; }
        echo "pool-tmpfs: $dir: tmpfs unmounted lazily, processes $(cut -d' ' -f1 <<< "$users" | sort -u | xargs) still map removed files of it"
    else
        sudo -n umount "$dir" || { echo "pool-tmpfs: cannot unmount $dir" >&2; return 1; }
        echo "pool-tmpfs: $dir: tmpfs unmounted"
    fi
}
