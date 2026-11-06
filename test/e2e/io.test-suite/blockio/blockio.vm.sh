# Copyright The NRI Plugins Authors. All Rights Reserved.
# SPDX-License-Identifier: Apache-2.0

# Sourced in the test VM by blockio.source.sh. Each operation runs in a new
# vm-command shell, so state lives in the test directory. Private CRI-O/NRI
# sockets and storage keep the provisioned Kubernetes runtime untouched.
blockio_work=/var/tmp/nri-blockio-e2e
blockio_run=/run/nbio-e2e
blockio_slice=nri-blockio-e2e.slice
blockio_image=docker.io/library/busybox:1.37

blockio-cri() {
    crictl --runtime-endpoint "unix://$blockio_run/crio.sock" \
        --image-endpoint "unix://$blockio_run/crio.sock" --timeout 30s "$@"
}

blockio-setup() {
    local coverage=$1 tool index device scheduler original
    for tool in crio pinns runc crun conmon crictl jq losetup nohup nri-blockio; do
        command -v "$tool"
    done
    test -f /sys/fs/cgroup/cgroup.controllers
    # Clean a previous interrupted test before replacing its device records.
    blockio-cleanup
    rm -rf "$blockio_work"
    mkdir -p "$blockio_work" "$blockio_run" "$coverage"
    printf '%s\n' "$coverage" > "$blockio_work/coverage-dir"
    { crio --version; runc --version; crun --version; uname -r; } > "$blockio_work/versions.txt"
    # BFQ is built in on some kernels, a module on others.
    modprobe bfq || true
    for index in 0 1; do
        truncate -s 16M "$blockio_work/disk$index.img"
        device=$(losetup --find --show --read-only "$blockio_work/disk$index.img")
        echo "$device" >> "$blockio_work/devices"
        scheduler=/sys/block/${device##*/}/queue/scheduler
        original=$(sed -n 's/.*\[\([^]]*\)\].*/\1/p' "$scheduler")
        printf '%s %s\n' "$scheduler" "$original" >> "$blockio_work/schedulers"
        grep -qw bfq "$scheduler"
        echo bfq > "$scheduler"
    done
    echo '{"default":[{"type":"insecureAcceptAnything"}]}' > "$blockio_work/policy.json"
}

blockio-class-config() {
    local weight=$1 mode
    mode=$(cat "$blockio_work/mode")
    case "$weight" in
        malformed) echo 'Classes: [' > "$blockio_work/$mode/classes.yaml"; return;;
        missing) rm "$blockio_work/$mode/classes.yaml"; return;;
    esac
    jq -n --argjson weight "$weight" --rawfile devices "$blockio_work/devices" '
        ($devices | split("\n") | map(select(length > 0))) as $devs |
        {Classes: {
            gold: [{Weight: $weight}],
            bronze: [
                {Weight: 100},
                {Devices: $devs, Weight: 50, ThrottleReadBps: "50M",
                 ThrottleWriteBps: "10Mi", ThrottleReadIOPS: "10k", ThrottleWriteIOPS: "5k"},
                {Devices: [$devs[1]], Weight: 200, ThrottleReadBps: "25M",
                 ThrottleWriteBps: "20M", ThrottleReadIOPS: "2k"}
            ],
            empty: [],
            zero: [{Devices: [$devs[0]], ThrottleWriteBps: "0"}]
        }}' > "$blockio_work/$mode/classes.yaml"
}

blockio-start() {
    local mode=$1 reload=$2 directory classfile plugin_reload='' builtin_config builtin_reload=false
    local attempt daemon
    echo "$mode" > "$blockio_work/mode"
    directory=$blockio_work/$mode
    mkdir -p "$directory"/{conf.d,plugins,nri-config,cni,logs}
    mkdir -p "$blockio_run"
    blockio-class-config 400
    classfile=$directory/classes.yaml
    builtin_config=$classfile
    if [ "$mode" = plugin ]; then
        builtin_config=''
        [ "$reload" != true ] || plugin_reload=--blockio-reload
        cat > "$directory/plugins/50-blockio" <<EOF
#!/bin/sh
export GOCOVERDIR=$(cat "$blockio_work/coverage-dir")
exec $(command -v nri-blockio) --config $classfile --v $plugin_reload >$directory/plugin.log 2>&1
EOF
        chmod +x "$directory/plugins/50-blockio"
    else
        builtin_reload=$reload
    fi
    cat > "$directory/crio.conf" <<EOF
[crio]
root = "$blockio_work/storage"
runroot = "$blockio_run/storage"
storage_driver = "vfs"
log_dir = "$directory/logs"
version_file = "$directory/version"
version_file_persist = "$directory/version-persist"
clean_shutdown_file = "$directory/clean.shutdown"
internal_wipe = false
internal_repair = false
[crio.api]
listen = "$blockio_run/crio.sock"
stream_port = "0"
[crio.runtime]
default_runtime = "runc"
cgroup_manager = "systemd"
selinux = false
pinns_path = "$(command -v pinns)"
container_exits_dir = "$blockio_run/exits"
container_attach_socket_dir = "$blockio_run/attach"
namespaces_dir = "$blockio_run/ns"
blockio_config_file = "$builtin_config"
blockio_reload = $builtin_reload
[crio.runtime.runtimes.runc]
runtime_path = "$(command -v runc)"
runtime_type = "oci"
runtime_root = "$blockio_run/runc"
monitor_path = "$(command -v conmon)"
[crio.runtime.runtimes.crun]
runtime_path = "$(command -v crun)"
runtime_type = "oci"
runtime_root = "$blockio_run/crun"
monitor_path = "$(command -v conmon)"
[crio.image]
signature_policy = "$blockio_work/policy.json"
pause_image = "registry.k8s.io/pause:3.10"
[crio.network]
network_dir = "$directory/cni"
[crio.nri]
enable_nri = true
nri_listen = "$blockio_run/nri.sock"
nri_plugin_dir = "$directory/plugins"
nri_plugin_config_dir = "$directory/nri-config"
EOF
    nohup crio --config "$directory/crio.conf" --config-dir "$directory/conf.d" \
        --log-level debug > "$directory/crio.log" 2>&1 </dev/null &
    daemon=$!
    echo "$daemon" > "$blockio_work/crio.pid"
    for attempt in {1..120}; do
        kill -0 "$daemon" || { cat "$directory/crio.log"; return 1; }
        if [ -S "$blockio_run/crio.sock" ] && blockio-cri version; then
            break
        fi
        sleep 0.25
    done
    [ "$attempt" -lt 120 ]
    if [ "$mode" = plugin ]; then
        grep -q 'synchronization success' "$directory/crio.log"
    fi
    if [ -z "$(blockio-cri images --quiet "$blockio_image")" ]; then
        blockio-cri pull "$blockio_image"
    fi
}

blockio-case() {
    # Optional positional arguments distinguish absent annotations from empty
    # overrides, which is not possible with a single ${arg:-default} expansion.
    local name=$1 runtime=$2 mode directory uid pod container pid cgroup file
    local pod_annotations='{}' ctr_annotations='{}'
    if [ "$#" -ge 3 ]; then
        pod_annotations=$(jq -n --arg v "$3" '{"blockio.resources.beta.kubernetes.io/pod": $v}')
    fi
    if [ "$#" -ge 4 ]; then
        pod_annotations=$(jq --arg v "$4" '. + {"blockio.resources.beta.kubernetes.io/container.app": $v}' <<< "$pod_annotations")
    fi
    if [ "$#" -ge 5 ]; then
        ctr_annotations=$(jq -n --arg v "$5" '{"io.kubernetes.cri.blockio-class": $v}')
    fi
    mode=$(cat "$blockio_work/mode")
    directory=$blockio_work/$mode/$runtime-$name
    mkdir -p "$directory"
    uid=$(cat /proc/sys/kernel/random/uuid)
    jq -n --arg uid "$uid" --arg logs "$directory" --arg slice "$blockio_slice" \
        --argjson annotations "$pod_annotations" '{
            metadata: {name: ("blockio-" + $uid), uid: $uid, namespace: "nri-blockio-test", attempt: 0},
            log_directory: $logs, annotations: $annotations,
            linux: {cgroup_parent: $slice, security_context: {namespace_options: {network: 2, pid: 1, ipc: 2}}}
        }' > "$directory/pod.json"
    jq -n --arg image "$blockio_image" --argjson annotations "$ctr_annotations" '{
            metadata: {name: "app", attempt: 0}, image: {image: $image},
            command: ["/bin/sleep", "300"], log_path: "app.log", annotations: $annotations,
            linux: {security_context: {namespace_options: {network: 2, pid: 1, ipc: 2}}}
        }' > "$directory/container.json"
    pod=$(blockio-cri runp --runtime "$runtime" "$directory/pod.json")
    if container=$(blockio-cri create "$pod" "$directory/container.json" "$directory/pod.json" 2> "$directory/create.log"); then
        # The baseline must reject zero on kernels which reject io.max=0.
        test "$name:$mode" != zero:baseline
    else
        cat "$directory/create.log"
        test "$name:$mode" = zero:baseline
        grep -Eq 'io.max|wbps' "$directory/create.log"
        grep -qi 'out of range' "$directory/create.log"
        echo '{"creation_failed":true}' > "$directory/cgroups.json"
        blockio-cri stopp "$pod"
        blockio-cri rmp "$pod"
        return
    fi
    blockio-cri start "$container"
    blockio-cri inspect "$container" > "$directory/inspect.json"
    pid=$(jq -er '.info.pid' "$directory/inspect.json")
    cgroup=$(sed -n 's/^0:://p' "/proc/$pid/cgroup")
    test -n "$cgroup"
    echo '{}' > "$directory/cgroups.json"
    for file in io.max io.bfq.weight io.weight; do
        if [ -f "/sys/fs/cgroup$cgroup/$file" ]; then
            jq --arg file "$file" --rawfile value "/sys/fs/cgroup$cgroup/$file" \
                '. + {($file): ($value | split("\n") | map(select(length > 0)) | sort)}' \
                "$directory/cgroups.json" > "$directory/cgroups.tmp"
        else
            jq --arg file "$file" '. + {($file): null}' "$directory/cgroups.json" > "$directory/cgroups.tmp"
        fi
        mv "$directory/cgroups.tmp" "$directory/cgroups.json"
    done
    if [ "$mode" = plugin ]; then
        jq -e '.info.runtimeSpec.linux.resources.blockIO == null' "$directory/inspect.json"
    fi
    if [ "$name" = zero ]; then
        jq -e '.info.runtimeSpec.linux.resources.unified["io.max"] | endswith("wbps=max")' "$directory/inspect.json"
        jq -e 'all(.["io.max"][]; (contains("wbps=") | not) or contains("wbps=max"))' "$directory/cgroups.json"
    fi
    blockio-cri stop --timeout 0 "$container"
    blockio-cri rm "$container"
    blockio-cri stopp "$pod"
    blockio-cri rmp "$pod"
}

blockio-stop() {
    local pod daemon attempt status=0
    if [ -S "$blockio_run/crio.sock" ]; then
        while read -r pod; do
            [ -n "$pod" ] || continue
            blockio-cri stopp "$pod" || status=1
            blockio-cri rmp --force "$pod" || status=1
        done < <(blockio-cri pods --quiet)
    fi
    if [ -f "$blockio_work/crio.pid" ]; then
        daemon=$(cat "$blockio_work/crio.pid")
        # A stale PID file must never cause an unrelated process to be killed.
        if [ -f "/proc/$daemon/cmdline" ] && grep -zqF "$blockio_work/" "/proc/$daemon/cmdline"; then
            kill "$daemon" || status=1
            for attempt in {1..120}; do
                [ -e "/proc/$daemon/exe" ] || break
                sleep 0.25
            done
            if [ -e "/proc/$daemon/exe" ]; then
                kill -KILL "$daemon" || return 1
                status=1
            fi
        fi
        rm "$blockio_work/crio.pid"
    fi
    rm -rf "$blockio_run"
    return "$status"
}

blockio-compare() {
    local baseline plugin case_name count=0
    # Check both coverage and values: an accidentally omitted case cannot pass.
    diff -u <(cd "$blockio_work/baseline" && find . -name cgroups.json | sort) \
        <(cd "$blockio_work/plugin" && find . -name cgroups.json | sort)
    for baseline in "$blockio_work/baseline"/*/cgroups.json; do
        case_name=${baseline%/cgroups.json}
        case_name=${case_name##*/}
        plugin=$blockio_work/plugin/$case_name/cgroups.json
        if [[ "$case_name" = *-zero ]]; then
            jq -e '.creation_failed == true' "$baseline"
            jq -e 'has("io.max")' "$plugin"
            echo "Expected difference: $case_name: CRI-O rejects 0; plugin writes max"
        else
            diff -u <(jq -S . "$baseline") <(jq -S . "$plugin")
        fi
        count=$((count + 1))
    done
    test "$count" -gt 0
    echo "Compared $count cases for each implementation"
}

blockio-cleanup() {
    local scheduler original device status=0
    # Continue independent cleanup steps after a failure, but report the error.
    blockio-stop || status=1
    if [ "$(systemctl show --property LoadState --value "$blockio_slice")" != not-found ]; then
        systemctl stop "$blockio_slice" || status=1
    fi
    if [ -f "$blockio_work/schedulers" ]; then
        while read -r scheduler original; do
            echo "$original" > "$scheduler" || status=1
        done < "$blockio_work/schedulers"
    fi
    if [ -f "$blockio_work/devices" ]; then
        while read -r device; do
            losetup --detach "$device" || status=1
        done < "$blockio_work/devices"
    fi
    # Make inspect/log artifacts readable to the framework's SSH user.
    if [ -d "$blockio_work" ]; then
        # Keep the large private image store out of collected artifacts.
        rm -rf "$blockio_work/storage" || status=1
        chmod -R a+rX "$blockio_work" || status=1
        # Retain the cleanup records after failures so the next setup can retry.
        if [ "$status" = 0 ]; then
            rm -f "$blockio_work/devices" "$blockio_work/schedulers"
        fi
    fi
    return "$status"
}
