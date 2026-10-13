# e2eish replay records, see "Replaying test logs as unit tests" in
# test/e2e/README.md.

E2EISH_SRC_DIR="$(realpath "$(dirname "${BASH_SOURCE[0]}")/../../..")"

e2eish-build-yaml2json() {
    # Usage: e2eish-build-yaml2json
    #
    # Build the YAML converter, once per process, into the output directory
    # of the run.
    [ -n "$E2EISH_YAML2JSON" ] && return 0
    local bin="${OUTPUT_DIR:-${TMPDIR:-/tmp}}/e2eish/yaml2json"
    "${GO_CMD:-go}" build -C "$E2EISH_SRC_DIR" -o "$bin" ./test/e2e/cmd/yaml2json ||
        return 1
    E2EISH_YAML2JSON="$bin"
}

e2eish-mark() {
    # Usage: e2eish-mark OP [KEY=VALUE...]
    #
    # Print a record. A KEY:json=VALUE is a JSON value, a KEY:yaml=VALUE a YAML
    # one and a KEY:list=VALUE a list of its words. Any other VALUE is a
    # string. YAML which fails to convert becomes null.
    [ "${e2eish:-1}" == 1 ] || return 0
    local args=() arg json
    for arg in "$@"; do
        case "${arg%%=*}" in
            *:yaml)
                json=null
                if e2eish-build-yaml2json; then
                    json=$("$E2EISH_YAML2JSON" <<< "${arg#*=}") || json=null
                fi
                arg="${arg%%:yaml=*}:json=$json"
                ;;
        esac
        args+=("$arg")
    done
    python3 -c '
import json, sys
record = {"op": sys.argv[1]}
for arg in sys.argv[2:]:
    key, _, value = arg.partition("=")
    if key.endswith(":json"):
        record[key[:-5]] = json.loads(value)
    elif key.endswith(":list"):
        record[key[:-5]] = value.split()
    else:
        record[key] = value
print("E2EISH " + json.dumps(record, separators=(",", ":")))
' "${args[@]}"
}

e2eish-mark-sync() {
    # Usage: e2eish-mark-sync
    #
    # Record the pods on the node with their running containers, those the
    # plugin synchronizes with when it starts.
    e2eish-mark-pods sync
}

e2eish-mark-pods() {
    # Usage: e2eish-mark-pods OP [KEY=VALUE...]
    #
    # Record the pods on the node with their running containers and the IDs
    # of those, as the pods of an OP record with the given string fields.
    [ "${e2eish:-1}" == 1 ] || return 0
    vm-command-q "kubectl get pods -A -o json" | python3 -c '
import json, sys
record = {"op": sys.argv[1]}
for arg in sys.argv[2:]:
    key, _, value = arg.partition("=")
    record[key] = value
pods = []
for item in json.load(sys.stdin)["items"]:
    ids = {s["name"]: s.get("containerID", "") for s in item.get("status", {}).get("containerStatuses", [])
           if "running" in s.get("state", {})}
    running = set(ids)
    if not running:
        continue
    meta, spec = item["metadata"], item["spec"]
    strip = lambda ctrs: [{"name": c["name"], "resources": c.get("resources", {})} for c in ctrs]
    pods.append({
        "metadata": {k: meta[k] for k in ("name", "namespace", "annotations") if k in meta},
        "spec": {
            "initContainers": strip(spec.get("initContainers", [])),
            "containers": strip(spec["containers"]),
            "resourceClaims": spec.get("resourceClaims", []),
        },
        "status": {"qosClass": item["status"].get("qosClass", "")},
        "running": sorted(running),
        "ids": ids,
    })
record["pods"] = pods
print("E2EISH " + json.dumps(record, separators=(",", ":")))
' "$@"
}

e2eish-mark-consts() {
    # Usage: e2eish-mark-consts
    #
    # Record py_consts, if it changed since it was last recorded.
    [ "${e2eish:-1}" == 1 ] || return 0
    [ "${E2EISH_CONSTS-unset}" != "${py_consts-}" ] || return 0
    E2EISH_CONSTS="${py_consts-}"
    e2eish-mark consts code="$E2EISH_CONSTS"
}

e2eish-mark-command() {
    # Usage: e2eish-mark-command COMMAND
    #
    # Record the pod deletions of a VM command, and flag the commands which
    # kill or restart containers behind the policy's back, or tamper with its
    # cache.
    [ "${e2eish:-1}" == 1 ] || return 0
    local line
    while IFS= read -r line; do
        line="${line#"${line%%[![:space:]]*}"}"
        case "$line" in
            *"kubectl delete "*)
                e2eish-mark-delete ${line#*kubectl delete }
                ;;
            kill\ *|pkill\ *|killall\ *|"crictl stop"*|"crictl rm"*)
                e2eish-mark unsupported command="$line"
                ;;
            mv\ *nri-resource-policy/cache*|ln\ *nri-resource-policy/cache*|cp\ *nri-resource-policy/cache*)
                e2eish-mark unsupported command="$line"
                ;;
        esac
    done <<< "$(sed -e 's/&&/\n/g' -e 's/;/\n/g' <<< "$1")"
}

e2eish-mark-delete() {
    # Usage: e2eish-mark-delete KUBECTL-DELETE-ARGS...
    local ns=default what="" pods=() all=false arg
    while [ $# -gt 0 ]; do
        arg=$1
        shift
        case "$arg" in
            -n|--namespace) ns=$1; shift;;
            --namespace=*) ns=${arg#*=};;
            --all) all=true;;
            -*) ;;
            pod|pods) what=pods;;
            pod/*|pods/*) what=pods; pods+=("${arg#*/}");;
            *)
                [ -z "$what" ] && return 0
                pods+=("$arg")
                ;;
        esac
    done
    [ "$what" == pods ] || return 0
    if [ "$all" == true ]; then
        e2eish-mark delete namespace="$ns" all:json=true
    elif [ ${#pods[@]} -gt 0 ]; then
        e2eish-mark delete namespace="$ns" pods:list="${pods[*]}"
    fi
    return 0
}
