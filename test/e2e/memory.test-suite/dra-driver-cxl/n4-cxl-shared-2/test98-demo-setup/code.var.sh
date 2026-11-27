# Demo setup (demo01-share-write-read, docs/memory/cxl/demo.md):
# "demo setup local|pool|pool-shared-fs|pool-fs [TYPE]|wizard|llm-share"
# runs this through the e2e framework; a run of the suite skips it.
#
# Leaves running, until "demo teardown" (test99-demo-teardown):
# - fake-cxl-pool-server on the host: local demo with the local device of
#   VM2 attached, pool demo with shared0 (256Mi, shared, 0xc1ae0001),
#   pool-shared-fs demo with shared0 persistent (its data and LSA files in
#   $POOL_DIR removed first: a new device without a file system), pool-fs
#   demo with fs0 (512Mi, shared, persistent, 0xc1ae0001; its files
#   removed first too); the wizard demo with pooled0 (512Mi, exclusive,
#   0xc1ee0001), shared0 and fs0 (512Mi, shared, persistent, 0xc1ae0002)
#   on host bridge 0 and three ports of host bridge 1, the local devices
#   of VM2 and VM1 attached, and a second server local-pmem (port 9912,
#   no pool controller) with lp0 (512Mi, persistent, 0xc100f5e1) attached
#   to VM2 as a local memdev; fs0 and lp0 formatted with an ext4 holding
#   hello.txt (manifests/wizard/formatter.yaml); the llm-share demo with
#   llm0 (shared, persistent, 0xc1ae0001, of the size for the model of
#   DEMO_LLM_MODEL) in $DEMO_POOL_DIR (a tmpfs of its own with 2 MiB
#   pages spread over the memory nodes of the host, bin/pool-tmpfs.sh;
#   /dev/shm without sudo; its files removed first),
#   cabled to all three VMs, and with DEMO_LLM_MODEL_DIR the model mirror
#   (python3 -m http.server of that directory on 127.0.0.1:$LLM_MIRROR_PORT,
#   192.168.76.2 for the VMs); with DEMO_LLM_LATENCY=far or local
#   bin/latency.sh on (pinned VMs, the pool directory on the far socket
#   or interleaved over the near socket's nodes);
# - nri-dra-driver-cxl on every node of the three-node cluster (helm,
#   DaemonSet; VM0 is the second worker, pool A reaches VM1 and VM2
#   only), the local demo with
#   sharedMemdevs: [<the serial of the local device of VM2>], the pool-fs
#   demo with fsTypes: [erofs, $DEMO_FSTYPE], the wizard demo with
#   fsTypes: [erofs, ext4, xfs, btrfs];
# - fake-cxl-pool-controller in a pod, as a cluster runs the controller of
#   a pool appliance: a Deployment in namespace fake-cxl-pool (the pod mode
#   of dra-install), its image imported into every node;
# - the trace collectors of dra-trace-start (host side, setsid), in
#   pool-shared-fs and pool-fs also of the udev events of block devices of
#   both VMs;
# - namespace $DRA_NS with ConfigMap demo-tools, the images pulled on both
#   nodes (pool-shared-fs, pool-fs: the demo image
#   localhost/cxl-demo-tools:demo, built on the host with docker from
#   $DEMO_DIR/image/Dockerfile and imported instead of python:3-alpine),
#   node labels cxl-demo/node=vm1|vm2, /root/demo in VM2 (the manifests of
#   the case and a kubeconfig with the demo namespace); llm-share: VM0 in
#   all of it (the demo image and the llama.cpp image in all three VMs,
#   label vm0 too) and ConfigMap llm-model (the model, its URL and sha256,
#   the proxies of the VMs for a URL outside them, LLM_REPEAT).
# Writes $DEMO_STATE_DIR/state.env for the views, steps, zooms and truth
# dumps.
#
# Environment: DEMO_CASE (local, pool, pool-shared-fs, pool-fs, wizard or
# llm-share), DEMO_FSTYPE (pool-fs: ext4, xfs or btrfs, default ext4),
# DEMO_DIR (the demo directory), DEMO_STATE_DIR (default $DEMO_DIR/run);
# llm-share: DEMO_LLM_MODEL (a model of LLM_MODELS, default qwen3.5-2b),
# DEMO_LLM_MODEL_URL (where the writer fetches it, default its Hugging Face
# URL), DEMO_LLM_MODEL_DIR (a host directory with the model file to serve
# instead), DEMO_LLM_ACCESS (direct or cached: the first access of the
# readers, for the steps), DEMO_LLM_LATENCY (off, local or far),
# DEMO_LLM_LATENCY_LOAD (off, or a level of bin/latency.sh:
# background memory traffic on the far socket, with DEMO_LLM_LATENCY=far),
# DEMO_LLM_REPEAT (bench repetitions, default 3), DEMO_POOL_DIR (the pool
# directory, default /tmp/fake-cxl-pool-llm).

[ -n "${DEMO_DIR:-}" ] || skip-test "run by bin/demo setup"

DEMO_CASE="${DEMO_CASE:-local}"
DEMO_SETUP_START=$(date +%s)   # zoom kernel shows the kernel lines since then
DEMO_STATE_DIR="${DEMO_STATE_DIR:-$DEMO_DIR/run}"
[ -n "$DEMO_DIR" ] && [ -d "$DEMO_DIR/manifests/$DEMO_CASE" ] ||
    error "DEMO_DIR ($DEMO_DIR) has no manifests/$DEMO_CASE: run this through \"demo setup local|pool|pool-shared-fs|pool-fs|wizard|llm-share\""
DEMO_FSTYPE="${DEMO_FSTYPE:-ext4}"
[ "$DEMO_CASE" != pool-fs ] || [[ "$DEMO_FSTYPE" =~ ^(ext4|xfs|btrfs)$ ]] ||
    error "DEMO_FSTYPE is $DEMO_FSTYPE: pool-fs takes ext4, xfs or btrfs"
[ "$DEMO_CASE" == pool-fs ] || DEMO_FSTYPE=""
[ ! -f "$DEMO_STATE_DIR/state.env" ] ||
    error "a demo is set up already ($DEMO_STATE_DIR/state.env): demo teardown first"

VM2="$OUTPUT_DIR"      # control plane, the VM of this test
VM1="$POOL_VM1_DIR"    # worker
VM0="$POOL_VM0_DIR"    # worker, in the demo of llm-share only
DRA_NS=cxl-demo
SHARED_SERIAL=0xc1ae0001
DEMO_TOOLS=(cxl-demo.py write read hold env mkfs fswrite)
DEMO_IMAGE=localhost/cxl-demo-tools:demo   # the pods of pool-shared-fs and pool-fs
POOL_DEVICE=shared0
POOL_DEVICE_FILES=""
PMEM_CASE=""   # the cases of a persistent pool device: the demo image, block udev events
case "$DEMO_CASE" in pool-shared-fs|pool-fs|wizard|llm-share) PMEM_CASE=1;; esac
DEMO_VMS=("$VM2" "$VM1")   # the VMs of the demo: images, labels, trace
[ "$DEMO_CASE" != llm-share ] || DEMO_VMS+=("$VM0")
declare -A DEMO_VM_LABEL=(["$VM2"]=vm2 ["$VM1"]=vm1 ["$VM0"]=vm0)   # cxl-demo/node of their nodes
# The wizard: pooled0, shared0 and fs0 of pool A, lp0 of the second server
# local-pmem (no pool controller: the node's driver publishes it as local).
POOLED_SERIAL=0xc1ee0001
FS0_SERIAL=0xc1ae0002
LP_SERVER=local-pmem
LP_PORT=9912
LP_DEVICE=lp0
LP_SERIAL=0xc100f5e1
WIZARD_FS_TYPES="erofs ext4 xfs btrfs"
VM2_LOCAL_SERIAL=""
VM1_LOCAL_SERIAL=""
[ "$DEMO_CASE" != wizard ] || DEMO_TOOLS+=(cxl-wizard.py)
# llm-share: llm0 of the model's size; the model of the writer from
# LLM_URL; the readers' stock llama.cpp image.
DEMO_LLM_MODEL="${DEMO_LLM_MODEL:-qwen3.5-2b}"
DEMO_LLM_ACCESS="${DEMO_LLM_ACCESS:-direct}"
DEMO_LLM_LATENCY="${DEMO_LLM_LATENCY:-off}"
DEMO_LLM_LATENCY_LOAD="${DEMO_LLM_LATENCY_LOAD:-off}"
DEMO_LLM_REPEAT="${DEMO_LLM_REPEAT:-3}"
DEMO_POOL_DIR="${DEMO_POOL_DIR:-/tmp/fake-cxl-pool-llm}"
# The pool tmpfs that setup mounts without the latency setup ("" for
# none: latency.sh mounts its own, or no sudo), and the pool directory
# without sudo, on the host's shared tmpfs.
POOL_TMPFS=""
# Set once llm-pool-dir starts latency.sh on.
LLM_LATENCY_ON=""
POOL_DIR_NO_SUDO=/dev/shm/fake-cxl-pool-llm
# shellcheck source=/dev/null # the pool tmpfs of llm-share, also of latency.sh
source "$DEMO_DIR/bin/pool-tmpfs.sh"
# The models of llm-share: NAME, the Hugging Face repository, the commit of
# the file, the GGUF file, its sha256 and size in bytes, and the size of
# llm0 for its erofs (the file plus the erofs and the pfn info block of the
# fsdax namespace, 256Mi steps).
LLM_MODELS="
qwen3.5-2b   unsloth/Qwen3.5-2B-GGUF   f6d5376be1edb4d416d56da11e5397a961aca8ae Qwen3.5-2B-Q4_K_M.gguf   aaf42c8b7c3cab2bf3d69c355048d4a0ee9973d48f16c731c0520ee914699223 1280835840 1536Mi
qwen3.5-0.8b unsloth/Qwen3.5-0.8B-GGUF 6ab461498e2023f6e3c1baea90a8f0fe38ab64d0 Qwen3.5-0.8B-Q4_K_M.gguf bd258782e35f7f458f8aced1adc053e6e92e89bc735ba3be89d38a06121dc517 532517120  768Mi
qwen3-0.6b   unsloth/Qwen3-0.6B-GGUF   50968a4468ef4233ed78cd7c3de230dd1d61a56b Qwen3-0.6B-Q4_K_M.gguf   ac2d97712095a558e31573f62f466a3f9d93990898b0ec79d7c974c1780d524a 396705472  512Mi"
LLM_SERVER_IMAGE=ghcr.io/ggml-org/llama.cpp:server-b11681
LLM_MIRROR_PORT=9920
LLM_MIRROR_PID=""
LLM_URL=""

llm-latency-name() {
    # Usage: llm-latency-name NAME
    #
    # Print the latency setup NAME: off, local or far. Fail for another
    # name.
    case "$1" in
        off) echo off;;
        local) echo local;;
        far) echo far;;
        *) return 1;;
    esac
}

if [ "$DEMO_CASE" == llm-share ]; then
    DEMO_TOOLS+=(cxl-llm.py llm llm-mem)
    # The EXIT traps of pool-server-start and, after it, of dra-install.
    # shellcheck disable=SC2034 # pool.source.sh and dra.source.sh read them
    POOL_EXIT_TRAP="pool-cleanup; llm-cleanup"
    # shellcheck disable=SC2034 # dra.source.sh reads it
    DRA_EXIT_TRAP="dra-cleanup; pool-cleanup; llm-cleanup"
    [[ "$DEMO_LLM_ACCESS" =~ ^(direct|cached)$ ]] || error "DEMO_LLM_ACCESS is $DEMO_LLM_ACCESS: llm-share takes direct or cached"
    latency=$(llm-latency-name "$DEMO_LLM_LATENCY") ||
        error "DEMO_LLM_LATENCY is $DEMO_LLM_LATENCY: llm-share takes off, local or far"
    DEMO_LLM_LATENCY="$latency"
    [[ "$DEMO_LLM_LATENCY_LOAD" =~ ^(off|[0-9]+)$ ]] || error "DEMO_LLM_LATENCY_LOAD is $DEMO_LLM_LATENCY_LOAD: llm-share takes off or a level of latency.sh"
    [ "$DEMO_LLM_LATENCY_LOAD" == off ] || [ "$DEMO_LLM_LATENCY" == far ] ||
        error "DEMO_LLM_LATENCY_LOAD=$DEMO_LLM_LATENCY_LOAD needs DEMO_LLM_LATENCY=far (the local setup starts no load)"
    [[ "$DEMO_LLM_REPEAT" =~ ^[1-9][0-9]*$ ]] || error "DEMO_LLM_REPEAT is $DEMO_LLM_REPEAT: a number of repetitions"
fi
wizard-format() {
    # Usage: wizard-format NAME SERIAL CLASS DRIVER
    #
    # Make an ext4 with hello.txt on the persistent device SERIAL of
    # DeviceClass CLASS and driver DRIVER: ResourceClaim and pod
    # format-NAME on VM2 of manifests/wizard/formatter.yaml, the whole
    # device in mode block. Fail the test unless the writer exits 0. Delete
    # the pod and the claim.
    local name="format-$1" serial="$2" class="$3" driver="$4" key text
    key=${serial#0x}
    key=${key^^}
    text="Hello from the wizard setup, $(date +%T)"
    dra-apply "$VM2" <<< "$(sed -e "s|__NAME__|$name|g; s|__CLASS__|$class|g; s|__DRIVER__|$driver|g" \
        -e "s|__SERIAL__|$serial|g; s|__KEY__|$key|g; s|__TEXT__|$text|g" \
        "$DEMO_DIR/manifests/wizard/formatter.yaml")"
    fs-writer-wait "$VM2" "$name" 240
    dra-delete-pod "$VM2" "$name"
    dra-delete "$VM2" resourceclaim "$name" --wait=true --timeout=90s
    echo "wizard-format: $1 ($serial) has an ext4 with hello.txt: \"$text\""
}

demo-image-build() {
    # Usage: demo-image-build
    #
    # Build the demo image from $DEMO_DIR/image/Dockerfile with docker on
    # the host, unless it exists, and save it to a tarball. Set
    # DEMO_IMAGE_TAG to localhost/cxl-demo-tools:<the first 12 hex digits
    # of the sha256 of the Dockerfile> and DEMO_IMAGE_TAR to the tarball.
    local sha12
    command -v docker >/dev/null ||
        error "docker is needed to build the demo image localhost/cxl-demo-tools (as for the driver image)"
    sha12=$(sha256sum "$DEMO_DIR/image/Dockerfile" | cut -c1-12)
    [ -n "$sha12" ] || error "cannot read $DEMO_DIR/image/Dockerfile"
    DEMO_IMAGE_TAG="localhost/cxl-demo-tools:$sha12"
    DEMO_IMAGE_TAR="$DEMO_STATE_DIR/cxl-demo-tools-$sha12.tar"
    if docker image inspect "$DEMO_IMAGE_TAG" >/dev/null 2>&1; then
        echo "demo-image-build: $DEMO_IMAGE_TAG exists"
    else
        host-command "docker build -t $DEMO_IMAGE_TAG $(printf %q "$DEMO_DIR/image")" ||
            command-error "cannot build the demo image $DEMO_IMAGE_TAG"
    fi
    mkdir -p "$DEMO_STATE_DIR" || error "cannot create $DEMO_STATE_DIR"
    host-command "docker save $DEMO_IMAGE_TAG -o $(printf %q "$DEMO_IMAGE_TAR")" ||
        command-error "cannot save $DEMO_IMAGE_TAG"
}

llm-model-select() {
    # Usage: llm-model-select NAME
    #
    # Set LLM_REPO, LLM_COMMIT, LLM_FILE, LLM_SHA256, LLM_SIZE and
    # LLM_DEVICE_SIZE to those of the model NAME of LLM_MODELS. Fail the
    # test if there is no such model.
    local name repo commit file sha size dev
    while read -r name repo commit file sha size dev; do
        [ "$name" == "$1" ] || continue
        LLM_REPO="$repo" LLM_COMMIT="$commit" LLM_FILE="$file" LLM_SHA256="$sha" LLM_SIZE="$size" LLM_DEVICE_SIZE="$dev"
        return 0
    done <<< "$LLM_MODELS"
    error "DEMO_LLM_MODEL is $1: llm-share takes $(awk 'NF {printf "%s ", $1}' <<< "$LLM_MODELS")"
}

llm-mirror-start() {
    # Usage: llm-mirror-start DIR
    #
    # Serve the files of the host directory DIR over HTTP on
    # 127.0.0.1:$LLM_MIRROR_PORT in the background (python3 -m
    # http.server): a model registry outside the cluster, at 192.168.76.2
    # for the VMs. Set LLM_MIRROR_PID. Fail the test unless the VM of the
    # test finds $LLM_FILE there.
    pool-port-busy "$LLM_MIRROR_PORT" &&
        error "port $LLM_MIRROR_PORT of the model mirror is busy: $(ss -ltnpH "sport = :$LLM_MIRROR_PORT" 2>/dev/null)"
    python3 -m http.server --bind 127.0.0.1 --directory "$1" "$LLM_MIRROR_PORT" </dev/null >"$TEST_OUTPUT_DIR/llm-mirror.log" 2>&1 &
    LLM_MIRROR_PID=$!
    echo "llm-mirror-start: $1 on http://127.0.0.1:$LLM_MIRROR_PORT, pid $LLM_MIRROR_PID"
    # shellcheck disable=SC2016 # retry-until evaluates the condition
    retry-until --timeout 30 --message "the model mirror serves $LLM_FILE to VM2" \
        'vmdir-command-q "$VM2" "curl -sfI --noproxy \"*\" --max-time 5 http://192.168.76.2:$LLM_MIRROR_PORT/$LLM_FILE" >/dev/null' ||
        error "the model mirror on port $LLM_MIRROR_PORT does not serve $LLM_FILE to VM2, see $TEST_OUTPUT_DIR/llm-mirror.log"
}

llm-cleanup() {
    # Usage: llm-cleanup
    #
    # Stop the model mirror of llm-mirror-start, run latency.sh off if
    # llm-pool-dir started latency.sh on, and unmount the pool tmpfs that
    # llm-pool-dir mounted, if they are there. Never fails the test.
    if [ -n "$LLM_MIRROR_PID" ]; then
        kill "$LLM_MIRROR_PID" 2>/dev/null
        LLM_MIRROR_PID=""
    fi
    if [ -n "$LLM_LATENCY_ON" ]; then
        DEMO_STATE_DIR="$DEMO_STATE_DIR" "$DEMO_DIR/bin/latency.sh" off || echo "llm-cleanup: latency.sh off failed: run demo latency off"
        LLM_LATENCY_ON=""
    fi
    if [ -n "$POOL_TMPFS" ]; then
        pool-tmpfs-umount "$DEMO_POOL_DIR" "$POOL_TMPFS" || echo "llm-cleanup: the pool tmpfs at $DEMO_POOL_DIR stays mounted: demo teardown"
        POOL_TMPFS=""
    fi
    return 0
}

llm-pool-dir() {
    # Usage: llm-pool-dir
    #
    # Make $DEMO_POOL_DIR the pool directory of llm-share, empty: the
    # pool tmpfs of latency.sh on with DEMO_LLM_LATENCY=far (bound to the far
    # nodes) or local (interleaved over the near nodes), else a tmpfs of its
    # own mounted now (pool-tmpfs-mount, POOL_TMPFS), interleaved over the
    # memory nodes that have CPUs. Without
    # sudo, $POOL_DIR_NO_SUDO on the host's shared tmpfs instead, with a
    # warning. Unmount a pool tmpfs that an earlier setup left first. Set
    # the EXIT trap to llm-cleanup. Fail the test if the directory is not a
    # tmpfs or not empty then.
    local opts mpol
    if [ "$(findmnt -n -o SOURCE --mountpoint "$DEMO_POOL_DIR" 2>/dev/null)" == "$POOL_TMPFS_SOURCE" ]; then
        rm -f "$DEMO_POOL_DIR/llm0.raw" "$DEMO_POOL_DIR/llm0.lsa"
        pool-tmpfs-umount "$DEMO_POOL_DIR" "$POOL_TMPFS_SOURCE" ||
            error "the pool tmpfs of an earlier setup at $DEMO_POOL_DIR is in use: demo teardown"
    fi
    # From here on, the end of this setup unmounts and restores what it
    # mounted and changed.
    trap llm-cleanup EXIT
    if [ "$DEMO_LLM_LATENCY" != off ]; then
        # After pool-vm-reset (a restarted qemu is not pinned), before the
        # server creates the files of llm0 in the pool directory.
        LLM_LATENCY_ON=1
        host-command "DEMO_VM_DIRS=$(printf %q "$VM0 $VM1 $VM2") DEMO_POOL_DIR=$(printf %q "$DEMO_POOL_DIR") DEMO_STATE_DIR=$(printf %q "$DEMO_STATE_DIR") DEMO_LLM_LATENCY_LOAD=$(printf %q "$DEMO_LLM_LATENCY_LOAD") $(printf %q "$DEMO_DIR/bin/latency.sh") on $DEMO_LLM_LATENCY" ||
            command-error "latency.sh on failed: the end of this setup runs latency.sh off (demo latency off does it by hand)"
    elif sudo -n true 2>/dev/null; then
        if [ -n "$(ls -A "$DEMO_POOL_DIR" 2>/dev/null)" ] || mountpoint -q "$DEMO_POOL_DIR" 2>/dev/null; then
            error "DEMO_POOL_DIR=$DEMO_POOL_DIR is not empty or a mount point: the pool tmpfs is mounted there"
        fi
        # The pages of a file are on the node of the CPU that writes them
        # first; readers at once would wait on that one node.
        mpol=$(pool-tmpfs-spread-policy)
        pool-tmpfs-mount "$DEMO_POOL_DIR" "$POOL_TMPFS_SOURCE" "$POOL_TMPFS_SIZE" "$mpol" ||
            error "cannot mount the pool tmpfs at $DEMO_POOL_DIR, see above"
        POOL_TMPFS="$POOL_TMPFS_SOURCE"
        pool-tmpfs-check "$DEMO_POOL_DIR" "$mpol" ||
            error "the pool tmpfs at $DEMO_POOL_DIR lacks huge=within_size${mpol:+ or mpol=$mpol}"
    else
        opts=$(findmnt -n -o OPTIONS -T "$(dirname "$POOL_DIR_NO_SUDO")")
        echo "demo setup llm-share: WARNING: sudo asks for a password: no tmpfs of its own for the pool directory."
        echo "  The pool directory is $POOL_DIR_NO_SUDO on the host's shared tmpfs ($opts): pages of"
        echo "  4 KiB unless it has huge=, placed where the writer ran; the local and far latency setups"
        echo "  mount a tmpfs with 2 MiB pages on the nodes of one socket, so they differ from off in page"
        echo "  size and placement too."
        echo "  The results record the options."
        DEMO_POOL_DIR="$POOL_DIR_NO_SUDO"
        mkdir -p "$DEMO_POOL_DIR" || error "cannot create $DEMO_POOL_DIR"
    fi
    [ "$(findmnt -no FSTYPE -T "$DEMO_POOL_DIR")" == tmpfs ] ||
        error "the pool directory $DEMO_POOL_DIR is not on a tmpfs: its pages are the memory of llm0"
    # A new device: no file system until the writer makes one.
    rm -f "$DEMO_POOL_DIR/llm0.raw" "$DEMO_POOL_DIR/llm0.lsa" ||
        error "cannot remove $DEMO_POOL_DIR/llm0.raw and $DEMO_POOL_DIR/llm0.lsa"
    echo "demo setup llm-share: the pool directory $DEMO_POOL_DIR: $(findmnt -n -o SOURCE,OPTIONS -T "$DEMO_POOL_DIR")"
}

llm-model-configmap() {
    # Usage: llm-model-configmap
    #
    # Create ConfigMap llm-model in $DRA_NS: the model of the writer and
    # the readers (LLM_MODEL_NAME, LLM_MODEL_FILE, LLM_MODEL_URL,
    # LLM_MODEL_SHA256, LLM_MODEL_SIZE), LLM_REPEAT for the client, and,
    # unless the URL is the mirror on the host, the proxy variables of
    # /etc/environment of VM2.
    local from="" kv proxies=""
    for kv in "LLM_MODEL_NAME=$DEMO_LLM_MODEL" "LLM_MODEL_FILE=$LLM_FILE" "LLM_MODEL_URL=$LLM_URL" \
              "LLM_MODEL_SHA256=$LLM_SHA256" "LLM_MODEL_SIZE=$LLM_SIZE" "LLM_REPEAT=$DEMO_LLM_REPEAT"; do
        from+=" --from-literal=$(printf %q "$kv")"
    done
    if [[ "$LLM_URL" != http://192.168.76.2:* ]]; then
        proxies=$(vmdir-command-q "$VM2" "grep -E '^(https?|no)_proxy=' /etc/environment | tr -d '\"'")
        for kv in $proxies; do
            from+=" --from-literal=$(printf %q "$kv")"
        done
    fi
    vmdir-command "$VM2" "kubectl create configmap llm-model -n $DRA_NS$from --dry-run=client -o yaml | kubectl apply -f -" ||
        command-error "cannot create ConfigMap llm-model"
}

echo "### demo setup $DEMO_CASE: the cluster and the VMs"
pool-cluster-require
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"
pool-vm-require "$VM1" n4-cxl-shared-1
[ "$DEMO_CASE" != llm-share ] || pool-vm-require "$VM0" n4-cxl-shared-0
# test00-up installed the tools.
for vm in "${DEMO_VMS[@]}"; do
    vmdir-command-q "$vm" "command -v cxl && command -v daxctl && command -v cxl-dump" >/dev/null ||
        error "$(vmdir-label "$vm") lacks cxl, daxctl or cxl-dump: run memory.test-suite/dra-driver-cxl/n4-cxl-shared-{0,1,2}/test00-up"
done
for vm in "${DEMO_VMS[@]}"; do
    pool-vm-reset "$vm"
done
if [ "$DEMO_CASE" == wizard ] || [ "$DEMO_CASE" == llm-share ]; then
    # fs0 and lp0 (llm0) get pmem regions: the root decoders of both host
    # bridges must allow persistent memory (lp0 is on host bridge 1 of VM2,
    # llm0 on any pool slot).
    fs-kernel-require "$VM2" "$(basename "$TOPOLOGY_DIR")"
    fs-kernel-require "$VM1" n4-cxl-shared-1
    [ "$DEMO_CASE" != llm-share ] || fs-kernel-require "$VM0" n4-cxl-shared-0
    for vm in "${DEMO_VMS[@]}"; do
        # shellcheck disable=SC2016 # expanded in the VM
        vmdir-command "$vm" 'cd /sys/bus/cxl/devices && for d in decoder0.*; do echo "$d cap_pmem $(cat $d/cap_pmem)"; [ "$(cat $d/cap_pmem)" == 1 ] || exit 1; done' ||
            error "a root decoder of $(vmdir-label "$vm") has no cap_pmem: fs0, lp0 or llm0 gets no region there"
    done
fi
if [ "$DEMO_CASE" == llm-share ]; then
    echo "### demo setup llm-share: the model $DEMO_LLM_MODEL, the pool directory $DEMO_POOL_DIR"
    llm-model-select "$DEMO_LLM_MODEL"
    if [ -n "${DEMO_LLM_MODEL_URL:-}" ]; then
        LLM_URL="$DEMO_LLM_MODEL_URL"
    elif [ -n "${DEMO_LLM_MODEL_DIR:-}" ]; then
        echo "$LLM_SHA256  $DEMO_LLM_MODEL_DIR/$LLM_FILE" | sha256sum --quiet -c ||
            error "DEMO_LLM_MODEL_DIR=$DEMO_LLM_MODEL_DIR has no $LLM_FILE with sha256 $LLM_SHA256 ($LLM_REPO at $LLM_COMMIT)"
        LLM_URL="http://192.168.76.2:$LLM_MIRROR_PORT/$LLM_FILE"
    else
        LLM_URL="https://huggingface.co/$LLM_REPO/resolve/$LLM_COMMIT/$LLM_FILE"
    fi
    echo "demo setup llm-share: $LLM_FILE ($LLM_SIZE bytes, sha256 $LLM_SHA256) from $LLM_URL, llm0 $LLM_DEVICE_SIZE"
    llm-pool-dir
fi

echo "### demo setup $DEMO_CASE: fake-cxl-pool-server on the host"
if [ "$DEMO_CASE" == pool ]; then
    pool-server-start "
  - name: shared0
    size: 256Mi
    shared: true
    serial: $SHARED_SERIAL"
elif [ "$DEMO_CASE" == pool-shared-fs ]; then
    # A new device: no file system until the writer makes one.
    rm -f "$POOL_DIR/shared0.raw" "$POOL_DIR/shared0.lsa" ||
        error "cannot remove $POOL_DIR/shared0.raw and $POOL_DIR/shared0.lsa"
    pool-server-start "
  - name: shared0
    size: 256Mi
    shared: true
    persistent: true
    serial: $SHARED_SERIAL"
    pool-device-files shared0
    pool-assert "$COMMAND_OUTPUT" "j['persistent'] == True and j['shared'] == True"
    [ "$POOL_DEVICE_DATA" == "$POOL_DIR/shared0.raw" ] && [ "$POOL_DEVICE_LSA" == "$POOL_DIR/shared0.lsa" ] ||
        error "the files of shared0 are $POOL_DEVICE_DATA and $POOL_DEVICE_LSA, expected $POOL_DIR/shared0.raw and $POOL_DIR/shared0.lsa"
    POOL_DEVICE_FILES="$POOL_DEVICE_DATA $POOL_DEVICE_LSA"
elif [ "$DEMO_CASE" == pool-fs ]; then
    # A new device without a filesystem; 512Mi: mkfs.xfs needs 300 MB.
    POOL_DEVICE=fs0
    rm -f "$POOL_DIR/fs0.raw" "$POOL_DIR/fs0.lsa" ||
        error "cannot remove $POOL_DIR/fs0.raw and $POOL_DIR/fs0.lsa"
    pool-server-start "
  - name: fs0
    size: 512Mi
    shared: true
    persistent: true
    serial: $SHARED_SERIAL"
    pool-device-files fs0
    pool-assert "$COMMAND_OUTPUT" "j['persistent'] == True and j['shared'] == True"
    [ "$POOL_DEVICE_DATA" == "$POOL_DIR/fs0.raw" ] && [ "$POOL_DEVICE_LSA" == "$POOL_DIR/fs0.lsa" ] ||
        error "the files of fs0 are $POOL_DEVICE_DATA and $POOL_DEVICE_LSA, expected $POOL_DIR/fs0.raw and $POOL_DIR/fs0.lsa"
    POOL_DEVICE_FILES="$POOL_DEVICE_DATA $POOL_DEVICE_LSA"
elif [ "$DEMO_CASE" == wizard ]; then
    # Pool A: host bridge 0 (two pool ports and the port of the local
    # devices) and three ports of host bridge 1; the fourth one of VM2 is
    # local-pmem's. fs0 and lp0 start without files: setup formats them.
    POOL_DEVICE=fs0
    rm -f "$POOL_DIR/fs0.raw" "$POOL_DIR/fs0.lsa" "/tmp/fake-cxl-pool-$LP_SERVER/$LP_DEVICE.raw" "/tmp/fake-cxl-pool-$LP_SERVER/$LP_DEVICE.lsa" ||
        error "cannot remove the files of fs0 and $LP_DEVICE"
    DRA_UNCONTROLLED_POOLS=("$LP_SERVER")
    pool-server-start --slots "*hb0 cxlsw_ds[012]_usrp0hb1" "
  - name: pooled0
    size: 512Mi
    shared: false
    serial: $POOLED_SERIAL
  - name: shared0
    size: 256Mi
    shared: true
    serial: $SHARED_SERIAL
  - name: fs0
    size: 512Mi
    shared: true
    persistent: true
    serial: $FS0_SERIAL"
    pool-device-files fs0
    pool-assert "$COMMAND_OUTPUT" "j['persistent'] == True and j['shared'] == True"
    POOL_DEVICE_FILES="$POOL_DEVICE_DATA $POOL_DEVICE_LSA"
elif [ "$DEMO_CASE" == llm-share ]; then
    POOL_DEVICE=llm0
    pool-server-start --hosts "$VM0 $VM1 $VM2" --dir "$DEMO_POOL_DIR" "
  - name: llm0
    size: $LLM_DEVICE_SIZE
    shared: true
    persistent: true
    serial: $SHARED_SERIAL"
    pool-device-files llm0
    pool-assert "$COMMAND_OUTPUT" "j['persistent'] == True and j['shared'] == True"
    [ "$POOL_DEVICE_DATA" == "$DEMO_POOL_DIR/llm0.raw" ] ||
        error "the data file of llm0 is $POOL_DEVICE_DATA, expected $DEMO_POOL_DIR/llm0.raw"
    POOL_DEVICE_FILES="$POOL_DEVICE_DATA $POOL_DEVICE_LSA"
else
    pool-server-start " []"
fi
pool-client-install "${DEMO_VMS[@]}"
dra-build
# Everything but the pool server runs in pods: the controller too
# (controller.source.sh), from its image.
demo-controller-image-build

if [ "$DEMO_CASE" == local ]; then
    # The serial of VM2's local device, before the driver runs, for its
    # configuration (sharedMemdevs).
    dra-local-find "$VM2"
    SERIAL="$DRA_LOCAL_SERIAL"
    DRA_DRIVER_CONFIG="sharedMemdevs: [\"$SERIAL\"]"
    echo "demo setup: driver configuration $DRA_DRIVER_CONFIG"
else
    SERIAL="$SHARED_SERIAL"
fi
if [ "$DEMO_CASE" == pool-fs ]; then
    # The driver mounts erofs only, unless its configuration lists more.
    DRA_DRIVER_CONFIG="fsTypes: [erofs, $DEMO_FSTYPE]"
    echo "demo setup: driver configuration $DRA_DRIVER_CONFIG"
    echo "demo setup: NOTE: the nodes mount $DEMO_FSTYPE now, a type that is not hardened against crafted images:"
    echo "  this trusts every party that can write fs0 raw (a read-write block claim or lease of it, as the"
    echo "  writer's of R2, on any node of any cluster that shares the pool, and a read-write fs claim);"
    echo "  erofs alone, the default, trusts nobody (docs/memory/cxl/persistent.md, Filesystems)"
fi
if [ "$DEMO_CASE" == wizard ]; then
    SERIAL="$FS0_SERIAL"
    DRA_DRIVER_CONFIG="fsTypes: [${WIZARD_FS_TYPES// /, }]"
    echo "demo setup: driver configuration $DRA_DRIVER_CONFIG"
    echo "demo setup: NOTE: the nodes mount ext4, xfs and btrfs too, types that are not hardened against"
    echo "  crafted images: this trusts every party that can write fs0 or lp0 raw (a read-write block claim"
    echo "  or lease of it, on any node of any cluster that shares the pool, and a read-write fs claim);"
    echo "  erofs alone, the default, trusts nobody (docs/memory/cxl/persistent.md, Filesystems)"
fi

echo "### demo setup $DEMO_CASE: the driver on every node, the pool controller in a pod"
dra-install
if [ "$DEMO_CASE" == local ]; then
    dra-local-device "$VM2"
    shared=$(dra-value "$DRA_LOCAL_PUBLISHED" "j['attributes']['shared']['bool']")
    [ "$shared" == True ] ||
        error "the driver publishes $DRA_LOCAL_DEVICE with shared $shared: it does not know sharedMemdevs, build its image again"
fi
if [ "$DEMO_CASE" == wizard ]; then
    # The local devices of both nodes, sliced.
    dra-local-device "$VM2"
    VM2_LOCAL_SERIAL="$DRA_LOCAL_SERIAL"
    dra-local-device "$VM1"
    VM1_LOCAL_SERIAL="$DRA_LOCAL_SERIAL"
    echo "### demo setup wizard: $LP_SERVER, a second server without a pool controller, hot-plugs $LP_DEVICE into VM2"
    pool-server-start --name "$LP_SERVER" --port "$LP_PORT" --hosts "$VM2" --control hmp --slots cxlsw_ds3_usrp0hb1 --local-devices no "
  - name: $LP_DEVICE
    size: 512Mi
    shared: false
    persistent: true
    serial: $LP_SERIAL"
    pool-device-files --pool "$LP_SERVER" "$LP_DEVICE"
    POOL_DEVICE_FILES+=" $POOL_DEVICE_DATA $POOL_DEVICE_LSA"
    pool-host-client --pool "$LP_SERVER" -o json attach "$LP_DEVICE" --host "$POOL_VM2_NAME" --owner demo:wizard ||
        command-error "cannot attach $LP_DEVICE to VM2"
    # The driver publishes a hot-added memdev at once, or holds it back for
    # up to 5 minutes (poolSerials, docs/memory/cxl/local.md).
    lp="device('memdev-${LP_SERIAL#0x}', '$DRA_LOCAL_DRIVER', '$POOL_VM2_NAME')"
    dra-wait "$VM2" lp0-published resourceslices "$lp is not None and attr($lp, 'persistent') == True" 360
fi

echo "### demo setup $DEMO_CASE: images, tools, manifests"
if [ -n "$PMEM_CASE" ]; then
    demo-image-build
    for vm in "${DEMO_VMS[@]}"; do
        dra-image-tar-put "$vm" "$DEMO_IMAGE_TAR" "$DEMO_IMAGE_TAG" "$DEMO_IMAGE"
    done
    rm -f "$DEMO_IMAGE_TAR"
    for vm in "${DEMO_VMS[@]}"; do
        dra-pull-images "$vm" "$DRA_COMPANION_IMAGE"
    done
else
    for vm in "${DEMO_VMS[@]}"; do
        dra-pull-images "$vm" "$DRA_TOOLS_IMAGE" "$DRA_COMPANION_IMAGE"
    done
fi
if [ "$DEMO_CASE" == llm-share ]; then
    for vm in "${DEMO_VMS[@]}"; do
        dra-pull-images "$vm" "$LLM_SERVER_IMAGE"
    done
fi
vmdir-command "$VM2" "rm -rf /root/demo && mkdir -p /root/demo/tools /root/demo/$DEMO_CASE" ||
    command-error "cannot create /root/demo in VM2"
for f in "${DEMO_TOOLS[@]}"; do
    vmdir-put-file "$VM2" "$DEMO_DIR/bin/tools/$f" "/root/demo/tools/$f"
done
for f in "$DEMO_DIR/manifests/$DEMO_CASE"/*.yaml; do
    vmdir-put-file "$VM2" "$f" "/root/demo/$DEMO_CASE/${f##*/}"
done
from=""
for f in "${DEMO_TOOLS[@]}"; do
    from+=" --from-file=$f=/root/demo/tools/$f"
done
vmdir-command "$VM2" "kubectl create configmap demo-tools -n $DRA_NS$from --dry-run=client -o yaml | kubectl apply -f -" ||
    command-error "cannot create ConfigMap demo-tools"
vmdir-command "$VM2" "cp /root/.kube/config /root/demo/kubeconfig && kubectl --kubeconfig /root/demo/kubeconfig config set-context --current --namespace=$DRA_NS" ||
    command-error "cannot write /root/demo/kubeconfig"
for vm in "${DEMO_VMS[@]}"; do
    vmdir-command "$VM2" "kubectl label node $(vmdir-name "$vm") cxl-demo/node=${DEMO_VM_LABEL[$vm]} --overwrite" ||
        command-error "cannot label the node of $(vmdir-label "$vm")"
done
if [ "$DEMO_CASE" == llm-share ]; then
    echo "### demo setup llm-share: the model for the writer"
    if [ -n "${DEMO_LLM_MODEL_DIR:-}" ] && [ -z "${DEMO_LLM_MODEL_URL:-}" ]; then
        llm-mirror-start "$DEMO_LLM_MODEL_DIR"
    fi
    llm-model-configmap
fi
if [ "$DEMO_CASE" == wizard ]; then
    echo "### demo setup wizard: an ext4 with hello.txt on fs0 and $LP_DEVICE"
    wizard-format fs0 "$FS0_SERIAL" cxl-pool-persistent "$DRA_POOL_DRIVER"
    wizard-format "$LP_DEVICE" "$LP_SERIAL" cxl-local-persistent "$DRA_LOCAL_DRIVER"
    # shellcheck disable=SC2016 # retry-until evaluates the condition
    retry-until --timeout 90 --message "fs0 free and attached nowhere" \
        'pool-host-client -o json devices fs0 >/dev/null && pool-py assert "$COMMAND_OUTPUT" "j[\"state\"] == \"free\" and j[\"attachments\"] in (None, [])"' ||
        error "fs0 is not free after its formatter: $COMMAND_OUTPUT"
    pool-cxl-wait "$VM2" lp0-no-region "by_serial($LP_SERIAL) is not None and not any(by_serial($LP_SERIAL)['name'] in sorted(r['memdevs']) for r in regions)" 90
fi

echo "### demo setup $DEMO_CASE: the trace"
dra-trace-start "${DEMO_VMS[@]}"
if [ -n "$PMEM_CASE" ]; then
    # The pmem block devices: the udev stream of dra-trace-start has cxl,
    # dax, memory and node only. T5 reads <vm>.block.udev as udev events.
    for vm in "${DEMO_VMS[@]}"; do
        tag="$(vmdir-label "$vm").block.udev"
        dra-trace-collect-vm "$vm" "$tag" lines "stdbuf -oL udevadm monitor -k -s block"
        # shellcheck disable=SC2016 # retry-until evaluates the condition
        retry-until --timeout 30 --message "trace collector $tag" \
            '[ -n "$(vmdir-command-q "$vm" "ls /run/dra-trace/$tag.pid 2>/dev/null")" ]' ||
            error "the trace collector $tag did not start in $(vmdir-label "$vm")"
    done
fi

# Everything stays up: no cleanup at exit.
trap - EXIT
mkdir -p "$DEMO_STATE_DIR" || error "cannot create $DEMO_STATE_DIR"
rm -f "$DEMO_STATE_DIR/step.env"   # the last step of an earlier demo
{
    echo "# demo setup $DEMO_CASE, $(date '+%Y-%m-%d %H:%M:%S'), $TEST_OUTPUT_DIR"
    image="$DRA_TOOLS_IMAGE"
    [ -z "$PMEM_CASE" ] || image="$DEMO_IMAGE"
    for kv in "DEMO_CASE=$DEMO_CASE" "DEMO_FSTYPE=$DEMO_FSTYPE" "DEMO_SETUP_START=$DEMO_SETUP_START" "VM2_DIR=$VM2" "VM1_DIR=$VM1" \
              "VM2_NODE=$POOL_VM2_NAME" "VM1_NODE=$POOL_VM1_NAME" \
              "NS=$DRA_NS" "SERIAL=$SERIAL" "POOL_DEVICE=$POOL_DEVICE" "POOL_DEVICE_FILES=$POOL_DEVICE_FILES" \
              "DEMO_IMAGE=$image" "POOL_URL=$POOL_URL" "POOL_DEVICE_DATA=$POOL_DEVICE_DATA" \
              "POOL_SERVER_PID=$POOL_SERVER_PID" "POOL_SERVER_LOG=$POOL_SERVER_LOG" \
              "TRACE_DIR=$DRA_TRACE_DIR" "SETUP_OUTPUT_DIR=$TEST_OUTPUT_DIR" "LOCAL_DEVICE_NAME=$DRA_LOCAL_NAME"; do
        printf '%s=%q\n' "${kv%%=*}" "${kv#*=}"
    done
    if [ "$DEMO_CASE" == llm-share ]; then
        for kv in "VM0_DIR=$VM0" "VM0_NODE=$POOL_VM0_NAME" "DEMO_POOL_DIR=$DEMO_POOL_DIR" "POOL_TMPFS=$POOL_TMPFS" \
                  "DEMO_LLM_MODEL=$DEMO_LLM_MODEL" "DEMO_LLM_ACCESS=$DEMO_LLM_ACCESS" "DEMO_LLM_LATENCY=$DEMO_LLM_LATENCY" \
                  "DEMO_LLM_LATENCY_LOAD=$DEMO_LLM_LATENCY_LOAD" "DEMO_LLM_REPEAT=$DEMO_LLM_REPEAT" "LLM_MODEL_FILE=$LLM_FILE" "LLM_MODEL_SIZE=$LLM_SIZE" \
                  "LLM_MODEL_URL=$LLM_URL" "LLM_DEVICE_SIZE=$LLM_DEVICE_SIZE" "LLM_MIRROR_PID=$LLM_MIRROR_PID"; do
            printf '%s=%q\n' "${kv%%=*}" "${kv#*=}"
        done
    fi
    if [ "$DEMO_CASE" == wizard ]; then
        # Lists with commas: the views read the file without a shell.
        for kv in "VM2_LOCAL_SERIAL=$VM2_LOCAL_SERIAL" "VM1_LOCAL_SERIAL=$VM1_LOCAL_SERIAL" \
                  "LP_SERVER=$LP_SERVER" "LP_PORT=$LP_PORT" "LP_URL=http://127.0.0.1:$LP_PORT" "LP_DEVICE=$LP_DEVICE" \
                  "LP_SERIAL=$LP_SERIAL" "LP_SERVER_PID=${POOL_SERVER_PIDS[$LP_SERVER]}" \
                  "FS_TYPES=${WIZARD_FS_TYPES// /,}" "FS_SETUP=ext4" "FS_SERIALS=$FS0_SERIAL,$LP_SERIAL"; do
            printf '%s=%q\n' "${kv%%=*}" "${kv#*=}"
        done
    fi
} > "$DEMO_STATE_DIR/state.env"
cat "$DEMO_STATE_DIR/state.env"
echo "demo setup $DEMO_CASE: done"
