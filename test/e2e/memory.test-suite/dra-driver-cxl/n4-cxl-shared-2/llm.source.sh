# Helpers for running a small language model (LLM) from shared persistent
# CXL memory in the VMs of n4-cxl-shared-1 and n4-cxl-shared-2
# (test16-cxl-llm-share).
#
# The model is Qwen3-0.6B Q4_K_M, a GGUF file pinned by its Hugging Face
# commit and sha256, cached on the host in $CACHE_DIR/llm. The VM of the
# writer gets the model in $LLM_VM_MODEL_DIR and builds an erofs image of
# that directory, $LLM_VM_IMAGE, that a writer pod flashes into a
# persistent pool device. The readers are stock llama.cpp, the server image
# $LLM_SERVER_IMAGE, on the erofs that the driver mounts from the device.
# cxl-where.py shows whether the page frames of the model mapping of a
# reader are CXL memory or DRAM.
#
# The helpers that work on either VM take VMDIR first, like those of
# pool.source.sh and dra.source.sh.

LLM_HELPER_DIR="$(dirname "${BASH_SOURCE[0]}")"
LLM_MODEL_REPO=unsloth/Qwen3-0.6B-GGUF
LLM_MODEL_COMMIT=50968a4468ef4233ed78cd7c3de230dd1d61a56b
LLM_MODEL_FILE=Qwen3-0.6B-Q4_K_M.gguf
LLM_MODEL_URL="https://huggingface.co/$LLM_MODEL_REPO/resolve/$LLM_MODEL_COMMIT/$LLM_MODEL_FILE"
LLM_MODEL_SHA256=ac2d97712095a558e31573f62f466a3f9d93990898b0ec79d7c974c1780d524a
LLM_MODEL_SIZE=396705472           # bytes
LLM_CACHE="${CACHE_DIR:-$HOME/.cache/nri-plugins/e2e}/llm"
LLM_VM_DIR=/opt/cxl-llm            # in the VMs
LLM_VM_MODEL_DIR="$LLM_VM_DIR/model"            # the model only: the root of the erofs image
LLM_VM_MODEL="$LLM_VM_MODEL_DIR/$LLM_MODEL_FILE"
LLM_VM_IMAGE="$LLM_VM_DIR/llm.erofs"            # the erofs image of $LLM_VM_MODEL_DIR
LLM_VM_STAMP="$LLM_VM_IMAGE.stamp"              # what the image was built from
# The file data of the model in one plain extent at a 2 MiB-aligned offset
# of the image: without -Enoinline_data the tail of the file is inline and
# the file silently gets no DAX.
LLM_MKFS="mkfs.erofs -b4096 --dsunit=512 -Enoinline_data"
LLM_VM_WHERE=/usr/local/bin/cxl-where.py
# Stock llama.cpp: entrypoint /app/llama-server, Ubuntu 24.04 with sh and
# curl, listens on 0.0.0.0:8080 (LLAMA_ARG_HOST, LLAMA_ARG_PORT).
LLM_SERVER_IMAGE=ghcr.io/ggml-org/llama.cpp:server-b11681
LLM_SERVER_PORT=8080
# Qwen3 thinks unless the think block is empty: the raw chat prompt of
# "hello" with an empty think block, answered greedily.
LLM_PROMPT='<|im_start|>user\nhello<|im_end|>\n<|im_start|>assistant\n<think>\n\n</think>\n\n'
LLM_MODEL=""                       # llm-model-fetch: the model on the host
LLM_IMAGE_SIZE=""                  # llm-image-build: the size of $LLM_VM_IMAGE in bytes
LLM_IMAGE_SHA256=""                # and its sha256
LLM_WHERE=""                       # llm-where: the mappings of the model, a JSON list
LLM_LOG=""                         # llm-server-wait: the log of the pod
LLM_ANSWER=""                      # llm-ask, llm-serve-sampled: the response of the server, JSON,
LLM_SAMPLES=""                     # llm-serve-sampled: where the model was while it served,
LLM_LOOP_READ=""                   # and the bytes read through the loop device meanwhile
LLM_CONTENT=""                     # and the completion in it

llm-model-fetch() {
    # Usage: llm-model-fetch
    #
    # Set LLM_MODEL to the model on the host: $llm_model if it is set (a
    # local copy), else $LLM_CACHE/$LLM_MODEL_FILE, downloaded from
    # $LLM_MODEL_URL if it is missing (the download goes into the cache
    # only with that sha256). Fail the test unless the file has sha256
    # $LLM_MODEL_SHA256.
    local model="${llm_model:-$LLM_CACHE/$LLM_MODEL_FILE}" part sum
    if [ -z "$llm_model" ] && [ ! -f "$model" ]; then
        mkdir -p "$LLM_CACHE" || error "llm-model-fetch: cannot create $LLM_CACHE"
        part="$model.part"
        # At most 30 min, and given up when slower than 1 MB/s for a minute.
        host-command "curl -fsSL --retry 3 --connect-timeout 30 --max-time 1800 --speed-limit 1048576 --speed-time 60 -o \"$part\" \"$LLM_MODEL_URL\"" || {
            rm -f "$part"
            error "llm-model-fetch: cannot download the model $LLM_MODEL_URL (proxy, network?). Download it by hand to $model, or set llm_model=PATH of a copy with sha256 $LLM_MODEL_SHA256"
        }
        sum=$(sha256sum "$part" | cut -d' ' -f1)
        if [ "$sum" != "$LLM_MODEL_SHA256" ]; then
            rm -f "$part"
            error "llm-model-fetch: the download of $LLM_MODEL_URL has sha256 $sum, expected $LLM_MODEL_SHA256"
        fi
        mv "$part" "$model" || error "llm-model-fetch: cannot move $part to $model"
    fi
    [ -f "$model" ] || error "llm-model-fetch: no model file llm_model=$model"
    if ! echo "$LLM_MODEL_SHA256  $model" | sha256sum --quiet -c; then
        [ -n "$llm_model" ] ||
            error "llm-model-fetch: $model does not have sha256 $LLM_MODEL_SHA256 of $LLM_MODEL_REPO $LLM_MODEL_FILE at $LLM_MODEL_COMMIT. Remove it to download it again"
        error "llm-model-fetch: llm_model=$model does not have sha256 $LLM_MODEL_SHA256 of $LLM_MODEL_REPO $LLM_MODEL_FILE at $LLM_MODEL_COMMIT"
    fi
    LLM_MODEL="$model"
    echo "llm-model-fetch: $LLM_MODEL, sha256 $LLM_MODEL_SHA256"
}

llm-vm-put() {
    # Usage: llm-vm-put VMDIR SRC DST MODE
    #
    # Copy the host file SRC to DST in the VM of VMDIR, owned by root with
    # file mode MODE, unless DST has the same sha256 already.
    local vmdir="$1" src="$2" dst="$3" mode="$4" sum
    sum=$(sha256sum "$src" | cut -d' ' -f1)
    if [ "$(vmdir-command-q "$vmdir" "sha256sum $dst 2>/dev/null" | cut -d' ' -f1)" == "$sum" ]; then
        echo "llm-vm-put: $(vmdir-label "$vmdir"):$dst is up to date"
        return 0
    fi
    vmdir-command "$vmdir" "mkdir -p $(dirname "$dst")" || command-error "cannot create $(dirname "$dst") in $(vmdir-label "$vmdir")"
    vmdir-put-file "$vmdir" "$src" "$dst"
    vmdir-command "$vmdir" "chown root:root $dst && chmod $mode $dst && sha256sum $dst" ||
        command-error "cannot install $dst in $(vmdir-label "$vmdir")"
    [ "${COMMAND_OUTPUT%% *}" == "$sum" ] || error "llm-vm-put: $dst in $(vmdir-label "$vmdir") is not a copy of $src"
}

llm-install() {
    # Usage: llm-install VMDIR [model]
    #
    # Install cxl-where.py as $LLM_VM_WHERE in the VM of VMDIR. With
    # "model", make $LLM_VM_MODEL_DIR hold the model (llm-model-fetch) and
    # nothing else, and $LLM_VM_DIR nothing but it and the erofs image;
    # without it, empty $LLM_VM_DIR.
    local vmdir="$1" with_model="$2"
    llm-vm-put "$vmdir" "$LLM_HELPER_DIR/cxl-where.py" "$LLM_VM_WHERE" 0555
    if [ "$with_model" == model ]; then
        [ -n "$LLM_MODEL" ] || error "llm-install: run llm-model-fetch first"
        llm-vm-put "$vmdir" "$LLM_MODEL" "$LLM_VM_MODEL" 0444
        vmdir-command "$vmdir" "find $LLM_VM_MODEL_DIR -mindepth 1 ! -path $LLM_VM_MODEL -delete; find $LLM_VM_DIR -mindepth 1 -maxdepth 1 ! -path $LLM_VM_MODEL_DIR ! -path $LLM_VM_IMAGE ! -path $LLM_VM_STAMP -exec rm -rf {} +; ls -la $LLM_VM_DIR $LLM_VM_MODEL_DIR" ||
            command-error "cannot clean up $LLM_VM_DIR in $(vmdir-label "$vmdir")"
    else
        vmdir-command "$vmdir" "rm -rf $LLM_VM_DIR && mkdir -p $LLM_VM_DIR && ls -la $LLM_VM_DIR" ||
            command-error "cannot remove the model from $(vmdir-label "$vmdir")"
    fi
}

llm-image-build() {
    # Usage: llm-image-build VMDIR
    #
    # Make $LLM_VM_IMAGE in the VM of VMDIR an erofs image of
    # $LLM_VM_MODEL_DIR (llm-install VMDIR model), built with
    # "$LLM_MKFS IMAGE DIR". Reuse the image if its stamp names the sha256
    # of the model and that command. Install erofs-utils if mkfs.erofs is
    # missing. Fail the test unless dump.erofs shows the model as one plain
    # extent (layout 0) at a 2 MiB-aligned offset of the image. Set
    # LLM_IMAGE_SIZE and LLM_IMAGE_SHA256.
    local vmdir="$1" label stamp out
    label=$(vmdir-label "$vmdir")
    stamp="model sha256 $LLM_MODEL_SHA256, $LLM_MKFS"
    vmdir-command-q "$vmdir" "command -v mkfs.erofs >/dev/null && command -v dump.erofs >/dev/null" ||
        vmdir-command "$vmdir" "dnf install -y erofs-utils" ||
            command-error "cannot install erofs-utils in $label"
    vmdir-command "$vmdir" "mkfs.erofs --version | head -n 1" || command-error "no mkfs.erofs in $label"
    if [ "$(vmdir-command-q "$vmdir" "cat $LLM_VM_STAMP 2>/dev/null && test -f $LLM_VM_IMAGE")" == "$stamp" ]; then
        echo "llm-image-build: $label:$LLM_VM_IMAGE is up to date ($stamp)"
    else
        vmdir-command "$vmdir" "rm -f $LLM_VM_STAMP $LLM_VM_IMAGE && $LLM_MKFS $LLM_VM_IMAGE $LLM_VM_MODEL_DIR && echo '$stamp' > $LLM_VM_STAMP" ||
            command-error "cannot build the erofs image $LLM_VM_IMAGE in $label"
    fi
    vmdir-command "$vmdir" "dump.erofs -e --path=/$LLM_MODEL_FILE $LLM_VM_IMAGE" ||
        command-error "dump.erofs cannot show /$LLM_MODEL_FILE in $LLM_VM_IMAGE"
    out="$COMMAND_OUTPUT"
    # The extent line: "   0:        0..396705792 | 396705792 :    2097152.. 398802944 | 396705792".
    python3 -c '
import re, sys
text, size = sys.argv[1], int(sys.argv[2])
layout = re.search(r"\bLayout: (\d+)\b", text)
exts = re.findall(r"^\s*\d+:\s*(\d+)\.\.\s*(\d+)\s*\|\s*(\d+)\s*:\s*(\d+)\.\.\s*(\d+)\s*\|\s*(\d+)\s*$", text, re.M)
if not layout or layout.group(1) != "0":
    sys.exit("the model has layout %s, not 0 (plain): its tail is inline, the file gets no DAX" % (layout.group(1) if layout else "?"))
if len(exts) != 1 or int(exts[0][0]) != 0 or int(exts[0][1]) < size:
    sys.exit("the model is not one extent of %d bytes: %s" % (size, exts))
if int(exts[0][3]) % (2 << 20):
    sys.exit("the model starts at %s of the image, not at a 2 MiB-aligned offset" % exts[0][3])
print("the model: one plain extent at offset %s of the image" % exts[0][3])' "$out" "$LLM_MODEL_SIZE" ||
        error "llm-image-build: $LLM_VM_IMAGE does not have the model in one plain 2 MiB-aligned extent"
    vmdir-command "$vmdir" "stat -c %s $LLM_VM_IMAGE && sha256sum $LLM_VM_IMAGE" ||
        command-error "cannot read $LLM_VM_IMAGE in $label"
    LLM_IMAGE_SIZE=$(head -n 1 <<< "$COMMAND_OUTPUT")
    LLM_IMAGE_SHA256=$(sed -n '2s/ .*//p' <<< "$COMMAND_OUTPUT")
    [ -n "$LLM_IMAGE_SIZE" ] && [ -n "$LLM_IMAGE_SHA256" ] || error "llm-image-build: no size and sha256 of $LLM_VM_IMAGE"
    echo "llm-image-build: $label:$LLM_VM_IMAGE, $LLM_IMAGE_SIZE bytes, sha256 $LLM_IMAGE_SHA256"
}

llm-reader-script() {
    # Usage: llm-reader-script KEY THREADS
    #
    # Print the shell script of a reader container (image
    # $LLM_SERVER_IMAGE) with the erofs of device KEY at CXL_<KEY>_PATH:
    # show the environment of the device, its mount in the container and
    # the files in it, whether the container has pmem devices ("no pmem
    # devices" or "pmem devices: ..."), try to create a file under the
    # mount ("write under PATH refused: ERROR" or "write under PATH: done"),
    # then run llama-server on THREADS threads with the model mapped from
    # the mount, without repacking the weights, on port $LLM_SERVER_PORT.
    local key="$1" threads="$2"
    cat <<SCRIPT
env | grep ^CXL_${key}_ | sort
p="\$CXL_${key}_PATH"
echo "mount: \$(grep " \$p " /proc/self/mountinfo)"
echo "files: \$(ls "\$p" | tr '\n' ' ')"
if ls /dev/pmem* >/dev/null 2>&1; then echo "pmem devices: \$(echo /dev/pmem*)"; else echo "no pmem devices"; fi
if out=\$(touch "\$p/probe" 2>&1); then echo "write under \$p: done"; else echo "write under \$p refused: \$out"; fi
exec /app/llama-server -m "\$p/$LLM_MODEL_FILE" -nr -t $threads -c 512 -np 1 --host 0.0.0.0 --port $LLM_SERVER_PORT
SCRIPT
}

llm-lease-reader-script() {
    # Usage: llm-lease-reader-script KEY ACCESS NAME THREADS
    #
    # Print the shell script of a reader container (image
    # $LLM_SERVER_IMAGE) with a lease claim: it leases the erofs of the
    # persistent shared device KEY (serial 0x<key>) read-only with ACCESS
    # (direct or cached) at /run/cxl/fs/NAME ("": the lease id) and runs
    # the script of llm-reader-script with THREADS threads on it
    # (cxl-request request --exec). Its log has the command line of
    # cxl-request, then CXL_LEASE=<id> and the lines of llm-reader-script.
    local key="$1" access="$2" name="$3" threads="$4" request
    request="cxl-request request --serial 0x${key,,} --share --read-only --mode fs --access $access${name:+ --name $name} --wait 120s --exec sh /tmp/reader.sh"
    cat <<SCRIPT
cat > /tmp/reader.sh <<'LLM_READER_EOF'
echo "CXL_LEASE=\$CXL_LEASE"
$(llm-reader-script "$key" "$threads")
LLM_READER_EOF
echo "$request"
exec $request
SCRIPT
}

# shellcheck disable=SC2001 # sed indents every line of SCRIPT
llm-server-pod-yaml() {
    # Usage: llm-server-pod-yaml NAME VMDIR KIND CLAIM SCRIPT
    #
    # Print pod NAME on the node of VMDIR, restartPolicy Never, running the
    # shell SCRIPT in its container "main" of $LLM_SERVER_IMAGE. KIND is
    # the claim of the pod: "claim", ResourceClaim CLAIM as its claim
    # "mem"; "lease", a claim "lease" from the ResourceClaimTemplate CLAIM
    # (dra-lease-template-yaml); "none", no claim (CLAIM is "").
    local name="$1" node kind="$3" claim="$4" script="$5" pod_claims="" uses="" ref="" source=""
    node=$(basename "$2")
    case "$kind" in
        claim) ref=mem; source="resourceClaimName: $claim";;
        lease) ref=lease; source="resourceClaimTemplateName: $claim";;
        none) ;;
        *) error "llm-server-pod-yaml: unknown claim kind \"$kind\" (claim, lease or none)";;
    esac
    if [ -n "$ref" ]; then
        pod_claims="
  resourceClaims:
  - name: $ref
    $source"
        uses="
    resources:
      claims:
      - name: $ref"
    fi
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
spec:
  nodeSelector:
    kubernetes.io/hostname: $node
  terminationGracePeriodSeconds: 2
  restartPolicy: Never$pod_claims
  containers:
  - name: main
    image: $LLM_SERVER_IMAGE
    imagePullPolicy: IfNotPresent
    command:
    - sh
    - -c
    - |
$(sed 's/^/      /' <<< "$script")
    ports:
    - containerPort: $LLM_SERVER_PORT$uses
EOF
}

llm-warm() {
    # Usage: llm-warm VMDIR
    #
    # Read the files of /app of $LLM_SERVER_IMAGE and the libraries of
    # llama-server once on the node of VMDIR into its page cache, in pod
    # llm-warm without a claim.
    local vmdir="$1"
    dra-apply "$OUTPUT_DIR" <<< "$(llm-server-pod-yaml llm-warm "$vmdir" none "" "cat \$(find /app -type f) \$(ldd /app/llama-server /app/*.so 2>/dev/null | awk '\$3 ~ /^\\// {print \$3}' | sort -u) >/dev/null 2>&1; echo \"warm: \$(du -sh /app | cut -f1) of /app and the libraries of llama-server read\"")"
    dra-pod-wait "$OUTPUT_DIR" llm-warm Succeeded 120
    dra-pod-log "$OUTPUT_DIR" llm-warm
    dra-delete-pod "$OUTPUT_DIR" llm-warm
}

llm-pod-ip() {
    # Usage: llm-pod-ip POD
    #
    # Print the IP address of pod POD in $DRA_NS, quietly. Return 1 if it
    # has none.
    local ip
    ip=$(vmdir-command-q "$OUTPUT_DIR" "kubectl get pod -n $DRA_NS $1 -o jsonpath={.status.podIP}")
    [ -n "$ip" ] || return 1
    echo "$ip"
}

llm-server-ready() {
    # Usage: llm-server-ready VMDIR POD
    #
    # Return 0 if llama-server in pod POD answers /health with 200 on its
    # pod IP, asked from the VM of VMDIR. Return 2 if the pod has
    # terminated.
    local vmdir="$1" pod="$2" ip phase
    phase=$(vmdir-command-q "$OUTPUT_DIR" "kubectl get pod -n $DRA_NS $pod -o jsonpath={.status.phase}")
    case "$phase" in Succeeded|Failed) return 2;; esac
    ip=$(llm-pod-ip "$pod") || return 1
    [ "$(vmdir-command-q "$vmdir" "curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://$ip:$LLM_SERVER_PORT/health")" == 200 ]
}

llm-server-wait() {
    # Usage: llm-server-wait VMDIR POD [TIMEOUT]
    #
    # Wait until llama-server in pod POD has loaded the model and serves
    # (llm-server-ready from the VM of VMDIR, the node of the pod), TIMEOUT
    # seconds at most (default 180). Fail the test if the pod terminates
    # before, or on timeout. LLM_LOG has the log of the pod then.
    local vmdir="$1" pod="$2" tmo="${3:-180}" rc=0
    retry-until --timeout "$tmo" --interval 1 --message "$pod: llama-server serves" \
        'llm-server-ready "$vmdir" "$pod"; rc=$?; [ "$rc" == 0 ] || [ "$rc" == 2 ]' || rc=1
    LLM_LOG=$(dra-pod-log "$OUTPUT_DIR" "$pod")
    if [ "$rc" != 0 ]; then
        echo "$LLM_LOG"
        vmdir-command "$OUTPUT_DIR" "kubectl describe pod -n $DRA_NS $pod | tail -n 20"
        [ "$rc" == 2 ] && error "llama-server in pod $pod terminated before it served"
        error "llama-server in pod $pod did not serve in ${tmo}s"
    fi
}

llm-ask() {
    # Usage: llm-ask VMDIR POD
    #
    # Ask llama-server in pod POD, from the VM of VMDIR (the node of the
    # pod), on its pod IP, for the greedy completion (temperature 0, top_k
    # 1) of $LLM_PROMPT in 32 tokens at most. LLM_ANSWER has the response
    # of the server, LLM_CONTENT the completion without surrounding white
    # space. Fail the test if there is none.
    local vmdir="$1" pod="$2" ip
    ip=$(llm-pod-ip "$pod") || error "llm-ask: pod $pod has no IP"
    vmdir-command "$vmdir" "curl -sS --max-time 120 -H 'Content-Type: application/json' http://$ip:$LLM_SERVER_PORT/completion -d @- <<'LLM_ASK_EOF'
{\"prompt\": \"$LLM_PROMPT\", \"n_predict\": 32, \"temperature\": 0, \"top_k\": 1}
LLM_ASK_EOF" || command-error "llm-ask: no response of llama-server in pod $pod"
    LLM_ANSWER="$COMMAND_OUTPUT"
    LLM_CONTENT=$(python3 -c 'import json, sys; print(json.loads(sys.argv[1])["content"].strip())' "$LLM_ANSWER") ||
        error "llm-ask: llama-server in pod $pod gave no completion: $LLM_ANSWER"
    echo "llm-ask: $pod answered: $LLM_CONTENT"
}

llm-answer-value() {
    # Usage: llm-answer-value EXPRESSION
    #
    # Print the value of the Python EXPRESSION on the last response of
    # llm-ask, j, for instance j["tokens_predicted"].
    pool-value "$LLM_ANSWER" "$1"
}

llm-page-cache() {
    # Usage: llm-page-cache VMDIR
    #
    # Print the page cache of the VM of VMDIR, the sum of FilePages of its
    # NUMA nodes, in kB, quietly.
    vmdir-command-q "$1" "cat /sys/devices/system/node/node*/meminfo" | awk '$3 == "FilePages:" { kb += $4 } END { print kb + 0 }'
}

llm-pod-file() {
    # Usage: llm-pod-file VMDIR POD
    #
    # Print the page cache charged to the cgroup of pod POD in $DRA_NS (the
    # parent of the cgroup of its container "main"), in bytes ("file" of
    # its memory.stat), quietly. Return 1 if there is no such cgroup.
    local cg bytes
    cg=$(dra-container-cgroup "$1" "$2" main) || return 1
    bytes=$(vmdir-command-q "$1" "awk '\$1 == \"file\" {print \$2}' $(dirname "$cg")/memory.stat")
    [ -n "$bytes" ] || return 1
    echo "$bytes"
}

llm-server-pid() {
    # Usage: llm-server-pid VMDIR POD
    #
    # Print the pid of llama-server (comm llama-server) in container "main"
    # of pod POD in $DRA_NS, in the VM of VMDIR, quietly. Return 1 if it
    # does not run.
    local cg pid
    cg=$(dra-container-cgroup "$1" "$2" main) || return 1
    pid=$(vmdir-command-q "$1" "for p in \$(cat $cg/cgroup.procs); do [ \"\$(cat /proc/\$p/comm 2>/dev/null)\" == llama-server ] && echo \$p; done")
    [ -n "$pid" ] || return 1
    echo "$pid"
}

llm-rss-anon() {
    # Usage: llm-rss-anon VMDIR POD
    #
    # Print RssAnon of llama-server in pod POD, in kB, quietly. Return 1 if
    # it does not run.
    local pid
    pid=$(llm-server-pid "$1" "$2") || return 1
    vmdir-command-q "$1" "awk '\$1 == \"RssAnon:\" {print \$2}' /proc/$pid/status"
}

llm-where() {
    # Usage: llm-where VMDIR POD PATH
    #
    # Show where the page frames of the mappings of the file PATH (the
    # model as the server sees it) of llama-server in pod POD are, with
    # cxl-where.py in the VM of VMDIR. LLM_WHERE has them as a JSON list,
    # one object per mapping (see cxl-where.py).
    local vmdir="$1" pod="$2" path="$3" pid
    pid=$(llm-server-pid "$vmdir" "$pod") || error "llm-where: no llama-server runs in pod $pod"
    vmdir-command "$vmdir" "$LLM_VM_WHERE $pid $path" ||
        command-error "cxl-where.py failed on llama-server of pod $pod"
    LLM_WHERE=$(python3 -c 'import json, sys; print(json.dumps([json.loads(l) for l in sys.stdin if l.startswith("{")]))' <<< "$COMMAND_OUTPUT")
}

llm-where-check() {
    # Usage: llm-where-check WHERE MIN-BYTES CHAIN all|present
    #
    # Return 0 if WHERE, the mappings of llm-where, are MIN-BYTES bytes at
    # least in all and the frames of their present pages are all in
    # resources whose chain matches the Python regular expression CHAIN;
    # with "all" every page must be present, with "present" one at least.
    # Print the mappings and what is wrong.
    python3 -c '
import json, re, sys
maps, least, chain, mode = json.loads(sys.argv[1]), int(sys.argv[2]), sys.argv[3], sys.argv[4]
print("mappings of the model: %s" % json.dumps(maps))
size = sum(m["size"] for m in maps)
if size < least:
    sys.exit("%d bytes mapped, less than %d" % (size, least))
present = sum(m["present"] for m in maps)
if mode == "present" and not present:
    sys.exit("no page of the mappings is present")
for m in maps:
    bad = sorted(k for k in m["where"] if not re.search(chain, k))
    if bad or (mode == "all" and m["present"] != m["pages"]):
        sys.exit("mapping at %s: %d of %d pages present, frames outside %r: %s" % (m["start"], m["present"], m["pages"], chain, bad))
print("%d bytes mapped, %d of %d pages present, all in %r" % (size, present, sum(m["pages"] for m in maps), chain))' "$@"
}

llm-where-assert() {
    # Usage: llm-where-assert VMDIR POD PATH MIN-BYTES CHAIN [all|present]
    #
    # Fail the test unless llama-server in pod POD (llm-where) maps the
    # file PATH with MIN-BYTES bytes at least in all, and the page frames
    # of its mappings of PATH are in resources of /proc/iomem whose chain
    # (cxl-where.py "where", for instance "CXL Window 0 > Persistent Memory
    # > region0 > namespace0.0") matches the Python regular expression
    # CHAIN: of every page ("all", the default), or of the present pages,
    # one at least ("present").
    local vmdir="$1" pod="$2" path="$3" min="$4" chain="$5" mode="${6:-all}"
    llm-where "$vmdir" "$pod" "$path"
    llm-where-check "$LLM_WHERE" "$min" "$chain" "$mode" ||
        error "the model mappings of llama-server in pod $pod are not $min bytes at least with $mode pages in '$chain'"
    echo "where ok: $pod: the mappings of $path, $mode pages in '$chain'"
}

llm-loop-reads() {
    # Usage: llm-loop-reads VMDIR [LOOP]
    #
    # Print the bytes read through every loop device of the VM of VMDIR
    # since it was created, or through LOOP (loopN) only, "loopN BYTES"
    # per line (sectors read of /sys/block/loopN/stat, 512 bytes each),
    # quietly. Return non-zero if they cannot be read.
    vmdir-command-q "$1" "test -d /sys/block || exit 1; for d in /sys/block/${2:-loop[0-9]*}; do [ -f \$d/stat ] || continue; echo \"\${d#/sys/block/} \$(( \$(awk '{print \$3}' \$d/stat) * 512 ))\"; done"
}

llm-fincore() {
    # Usage: llm-fincore VMDIR FILE
    #
    # Print how many bytes of FILE in the VM of VMDIR are in its page
    # cache (fincore RES), quietly. Return non-zero if fincore fails.
    local res
    res=$(vmdir-command-q "$1" "fincore -b -n -o RES $2") || return 1
    [[ "$res" =~ ^[[:space:]]*[0-9]+[[:space:]]*$ ]] || return 1
    echo $(( res ))
}

llm-serve-sampled() {
    # Usage: llm-serve-sampled VMDIR POD PATH LOOP N-PREDICT
    #
    # Ask llama-server in pod POD, from the VM of VMDIR, for N-PREDICT
    # tokens of $LLM_PROMPT (greedy, ignore_eos), and while it generates
    # take up to three samples of where the page frames of its mappings of
    # the file PATH are (cxl-where.py), each sample started and finished
    # while the request runs. Wait for the response. Set LLM_ANSWER to
    # the response, LLM_SAMPLES to the samples (a JSON list of llm-where
    # lists) and LLM_LOOP_READ to the bytes read through the loop device
    # LOOP (loopN) during the request. Fail the test if the request fails.
    local vmdir="$1" pod="$2" path="$3" loop="$4" n="$5" ip pid out
    ip=$(llm-pod-ip "$pod") || error "llm-serve-sampled: pod $pod has no IP"
    pid=$(llm-server-pid "$vmdir" "$pod") || error "llm-serve-sampled: no llama-server runs in pod $pod"
    vmdir-command "$vmdir" "req=/tmp/llm-serve-$pod.json; res=/tmp/llm-serve-$pod.out
cat > \$req <<'LLM_SERVE_EOF'
{\"prompt\": \"$LLM_PROMPT\", \"n_predict\": $n, \"ignore_eos\": true, \"temperature\": 0, \"top_k\": 1}
LLM_SERVE_EOF
r0=\$(awk '{print \$3}' /sys/block/$loop/stat)
curl -sS --max-time 300 -H 'Content-Type: application/json' http://$ip:$LLM_SERVER_PORT/completion -d @\$req > \$res 2>&1 &
c=\$!
trap 'kill \$c 2>/dev/null; rm -f \$req \$res' EXIT
i=0
while [ \$i -lt 3 ] && kill -0 \$c 2>/dev/null; do
    s=\$($LLM_VM_WHERE $pid $path) || break
    kill -0 \$c 2>/dev/null || break
    i=\$(( i + 1 ))
    echo \"\$s\" | sed \"s/^/sample \$i: /\"
done
wait \$c
echo \"curl exit \$?\"
r1=\$(awk '{print \$3}' /sys/block/$loop/stat)
echo \"loop $loop read \$(( (r1 - r0) * 512 ))\"
echo \"response: \$(cat \$res)\"" >/dev/null ||
        command-error "llm-serve-sampled: the request to llama-server in pod $pod failed"
    out="$COMMAND_OUTPUT"
    grep -qx "curl exit 0" <<< "$out" || error "llm-serve-sampled: the request to $pod failed: $(grep -E '^(curl exit|response:)' <<< "$out")"
    LLM_ANSWER=$(sed -n 's/^response: //p' <<< "$out")
    LLM_LOOP_READ=$(sed -n "s/^loop $loop read \([0-9]*\)$/\1/p" <<< "$out")
    [ -n "$LLM_LOOP_READ" ] || error "llm-serve-sampled: no read counter of $loop"
    LLM_SAMPLES=$(python3 -c '
import json, sys
samples = {}
for line in sys.stdin:
    head, sep, rest = line.partition(": ")
    if sep and head.startswith("sample ") and rest.startswith("{"):
        samples.setdefault(int(head.split()[1]), []).append(json.loads(rest))
print(json.dumps([samples[k] for k in sorted(samples)]))' <<< "$out")
}


llm-file-sha256() {
    # Usage: llm-file-sha256 FILE [LENGTH]
    #
    # Print the sha256 of the host FILE, or of its first LENGTH bytes, for
    # instance of the backing file of a device.
    if [ -n "$2" ]; then
        head -c "$2" "$1" | sha256sum | cut -d' ' -f1
    else
        sha256sum "$1" | cut -d' ' -f1
    fi
}

llm-file-zero() {
    # Usage: llm-file-zero FILE OFFSET LENGTH
    #
    # Return 0 if the LENGTH bytes at OFFSET of the host FILE are all zero.
    python3 -c '
import sys
with open(sys.argv[1], "rb") as f:
    f.seek(int(sys.argv[2]))
    left = int(sys.argv[3])
    while left:
        data = f.read(min(left, 1 << 22))
        if not data or data.strip(b"\0"):
            sys.exit(1)
        left -= len(data)' "$1" "$2" "$3"
}

llm-file-find() {
    # Usage: llm-file-find FILE MODEL ALIGN
    #
    # Print the first offset of the host FILE, a multiple of ALIGN, at
    # which the bytes of the host file MODEL are, compared by sha256.
    # Return 1 if there is none.
    python3 -c '
import hashlib, os, sys
path, model, align = sys.argv[1], sys.argv[2], int(sys.argv[3])
size = os.path.getsize(model)
with open(model, "rb") as f:
    head = f.read(1 << 16)
    want = hashlib.sha256(head + f.read()).hexdigest()
with open(path, "rb") as f:
    end = os.path.getsize(path) - size
    for off in range(0, end + 1, align):
        f.seek(off)
        if f.read(len(head)) != head:
            continue
        h = hashlib.sha256(head)
        left = size - len(head)
        while left:
            data = f.read(min(left, 1 << 22))
            if not data:
                break
            h.update(data)
            left -= len(data)
        if h.hexdigest() == want:
            print(off)
            sys.exit(0)
sys.exit(1)' "$1" "$2" "$3"
}
