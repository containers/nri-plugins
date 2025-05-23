#!/bin/bash

TITLE="NRI Resource Policy End-to-End Testing"
DEFAULT_DISTRO=${DEFAULT_DISTRO:-"fedora/40-cloud-base"}

# Other tested distros
#    generic/ubuntu2204
#    fedora/37-cloud-base

SCRIPT_DIR="$(dirname "${BASH_SOURCE[0]}")"
SRC_DIR=$(realpath "$SCRIPT_DIR/../..")
LIB_DIR=$(realpath "$SCRIPT_DIR/lib")
qemu_dir="${qemu_dir:-/usr/share/qemu}"

export OUTPUT_DIR=${outdir:-"$SCRIPT_DIR"/output}

# Place output of each test under a separate test case directory
export TEST_OUTPUT_DIR=${test_outdir:-"$OUTPUT_DIR"}
export COMMAND_OUTPUT_DIR="$TEST_OUTPUT_DIR"/commands

distro=${distro:-$DEFAULT_DISTRO}
export k8scri=${k8scri:-"containerd"}
export cni_plugin=${cni_plugin:-bridge}
export cni_release=${cni_release:-latest}
TOPOLOGY_DIR=${TOPOLOGY_DIR:=e2e}

GH_K8S_REPO="kubernetes/kubernetes"
export k8s_release=${k8s_release:-"latest"}
export k8s_version=""

source "$LIB_DIR"/vm.bash

list-github-releases () {
    local repo="$1" max="${max_releases:-20}" page=1 tags="" more="" sep=""
    local total=0 cnt=-1

    while [ "$cnt" != 0 -a "$total" -lt "$max" ]; do
        echo -n "fetching releases for $repo (page $page)..." 1>&2
        if more=$(wget -q "https://github.com/$repo/releases?page=$page" -O- | \
                      grep -E '.*/releases/tag/v[0-9]+.[0-9]+.[0-9]+"' | \
                      sed -E 's|.*/releases/tag/(v[0-9]+.[0-9]+.[0-9]+).*|\1|g')
        then
            tags="$tags$sep$more"
            sep=" "
            cnt="$(echo $more | wc -w)"
            total=$((total+cnt))
            echo " got $cnt more, $total in total..." 1>&2
            page=$((page+1))
        else
            echo "failed to determine latest github release for $repo"
            return 1
        fi
    done

    echo "$tags" | tr -s ' ' '\n' | sort -Vr
}

latest-github-release () {
    list-github-releases "$1" | head -1
    return $?
}

if [ "$k8s_release" = "latest" ]; then
    if latest_k8s_release=$(vm-load-cached-var "$OUTPUT_DIR" latest_k8s_release); then
        echo "Loaded cached latest_k8s_release=$latest_k8s_release..."
        k8s_release="$latest_k8s_release"
    else
        if ! k8s_release=$(latest-github-release $GH_K8S_REPO); then
            error "$k8s_release"
        fi
        vm-save-cached-var "$OUTPUT_DIR" latest_k8s_release $k8s_release
    fi
    k8s_release="${k8s_release#v}"
    echo "Latest Kubernetes release: $k8s_release"
fi

export k8s_version=$(echo $k8s_release | sed -E 's/([0-9]*\.[0-9]*)(\.[0-9]*)/\1/g')
if [ -z "$k8s_version" ]; then
    error "failed to determine latest Kubernetes version from \"$k8s_release"\"
fi

export vm_name=${vm_name:=$(vm-create-name "$k8scri" "$(basename "$TOPOLOGY_DIR")" ${distro})}
ESCAPED_VM=$(printf '%s\n' "$vm_name" | sed -e 's/[\/]/-/g')
export VM_HOSTNAME="$ESCAPED_VM"
export POLICY=${policy:-"topology-aware"}

# These exports force ssh-* to fail instead of prompting for a passphrase.
export DISPLAY=bogus-none
export SSH_ASKPASS=/bin/false
SSH_OPTS="-F $OUTPUT_DIR/.ssh-config"
SSH_PERSIST_OPTS="-o ControlMaster=auto -o ControlPersist=60 -o ControlPath=/tmp/ssh-%C"
export SSH="ssh $SSH_OPTS $SSH_PERSIST_OPTS"
export SCP="scp $SSH_OPTS $SSH_PERSIST_OPTS"
export VM_SSH_USER=vagrant

export nri_resource_policy_src=${nri_resource_policy_src:-"$SRC_DIR"}

nri_resource_policy_cache="/var/lib/nri-resource-policy/cache"

if [ "$k8scri" == "containerd" ]; then
    k8scri_sock="/var/run/containerd/containerd.sock"
else
    k8scri_sock="/var/run/crio/crio.sock"
fi

# If we run tests with containerd as the runtime, we install it
# from a release tarball with the version given below... unless
# a source directory is given, which is then expected to contain
# a compiled version of containerd which we should install.
GH_CONTAINERD_REPO="containerd/containerd"
export containerd_release=${containerd_release:-latest}

if [ "$k8scri" = "containerd" -a "$containerd_release" = "latest" ]; then
    if latest_containerd_release=$(vm-load-cached-var "$OUTPUT_DIR" latest_containerd_release); then
        echo "Loaded cached latest_containerd_release=$latest_containerd_release..."
        containerd_release="$latest_containerd_release"
    else
        if ! containerd_release=$(latest-github-release $GH_CONTAINERD_REPO); then
            error "$containerd_release"
        fi
        vm-save-cached-var "$OUTPUT_DIR" latest_containerd_release $containerd_release
    fi
    containerd_release="${containerd_release#v}"
    echo "Latest containerd release: $containerd_release"
fi


export containerd_src=${containerd_src:-}


# If we run tests with CRI-O as the runtime, we install it from
# a release tarball with the version given below... unless a
# source directory is given, which is then expected to contain
# a compiled version of CRI-O which we should install.
GH_CRIO_REPO="cri-o/cri-o"
export crio_release=${crio_release:-latest}
export crio_src=${crio_src:-}

if [ "$k8scri" = "crio" -a "$crio_release" = "latest" ]; then
    if latest_crio_release=$(vm-load-cached-var "$OUTPUT_DIR" latest_crio_release); then
        echo "Loaded cached latest_crio_release=$latest_crio_release..."
        crio_release="$latest_crio_release"
    else
        if ! crio_release=$(latest-github-release $GH_CRIO_REPO); then
            error "$crio_release"
        fi
        vm-save-cached-var "$OUTPUT_DIR" latest_crio_release $crio_release
    fi
    crio_release="${crio_release#v}"
    echo "Latest CRI-O release: $crio_release"
fi

# Default topology if not given. The run_tests.sh script will figure out
# the topology from the test directory structure and contents.
if [ -z "$topology" ]; then
    topology='[
            {"mem": "1G", "cores": 1, "nodes": 2, "packages": 2, "node-dist": {"4": 28, "5": 28}},
            {"nvmem": "8G", "node-dist": {"5": 28, "0": 17}},
            {"nvmem": "8G", "node-dist": {"2": 17}}
        ]'
fi

source "$LIB_DIR"/command.bash
source "$LIB_DIR"/vm.bash
source "$LIB_DIR"/host.bash

# Special handling for printing out runtime logs. This is called
# by run_tests.sh script and done here as run_tests.sh does not
# know all the configuration details that this script knows.
# So in order not to duplicate code, let run_tests.sh call this
# to just print runtime logs.
if [ "$1" == "runtime-logs" ]; then
    vm-command "journalctl /usr/bin/${k8scri}" > "${OUTPUT_DIR}/runtime.log"
    exit
fi

script_source="$(< "$0") $(< "$LIB_DIR/vm.bash")"

help() { # script API
    # Usage: help [FUNCTION|all]
    #
    # Print help on all functions or on the FUNCTION available in script.
    awk -v f="$1" \
        '/^[a-z].*script API/{split($1,a,"(");if(f==""||f==a[1]||f=="all"){print "";print a[1]":";l=2}}
         !/^    #/{l=l-1}
         /^    #/{if(l>=1){split($0,a,"#"); print "   "a[2]; if (f=="") l=0}}' <<<"$script_source"
}

if [ "$1" == "help" ]; then
    help
    exit 0
fi

echo
echo "    VM              = $vm_name"
echo "    Distro          = $distro"
echo "    Kubernetes"
echo "      - release     = $k8s_release"
echo "      - version     = $k8s_version"
echo "    Runtime         = $k8scri"
echo "    Output dir      = $OUTPUT_DIR"
echo "    Test output dir = $TEST_OUTPUT_DIR"
echo "    NRI dir         = $nri_resource_policy_src"
if [ "$k8scri" == "containerd" ]; then
    echo "    Containerd"
    echo "      release       = $containerd_release"
    echo "      sources       = $containerd_src"
else
    echo "    CRI-O"
    echo "      release       = $crio_release"
    echo "      sources       = $crio_src"
fi
echo "    Policy          = $POLICY"
echo "    Topology        = $topology"
echo

vm-setup "$OUTPUT_DIR" "$ESCAPED_VM" "$distro" "$TOPOLOGY_DIR" "$topology"

vm-nri-plugin-deploy "$OUTPUT_DIR" "$ESCAPED_VM" "$POLICY"

SUMMARY_FILE="$TEST_OUTPUT_DIR/summary.txt"
echo -n "" > "$SUMMARY_FILE" || error "cannot write summary to \"$SUMMARY_FILE\""

# The breakpoint function can be used in debugging the test script. You can call
# this function which causes the script to enter interactive mode where you can
# give additional commands.
breakpoint() { # script API
    # Usage: breakpoint
    #
    # Enter the interactive/debug mode: read next script commands from
    # the standard input until "exit".
    echo "Entering the interactive mode until \"exit\"."
    INTERACTIVE_MODE=$(( INTERACTIVE_MODE + 1 ))
    # shellcheck disable=SC2162
    while read -e -p "run.sh> " -a commands; do
        if [ "${commands[0]}" == "exit" ]; then
            break
        fi
        eval "${commands[@]}"
    done
    INTERACTIVE_MODE=$(( INTERACTIVE_MODE - 1 ))
}

test-user-code() {
    vm-command-q "kubectl get pods 2>&1 | grep -q NAME" && vm-command "kubectl delete pods --all --now"

    # The $code is set by run_tests.sh script and it is the contents of
    # the code.var.sh file of the test case.
    ( eval "$code" ) || {
        TEST_FAILURES="${TEST_FAILURES} test script failed"
    }
}

is-hooked() {
    local hook_code_var hook_code
    hook_code_var=$1
    hook_code="${!hook_code_var}"
    if [ -n "${hook_code}" ]; then
        return 0 # logic: if is-hooked xyz; then run-hook xyz; fi
    fi
    return 1
}

run-hook() {
    local hook_code_var hook_code
    hook_code_var=$1
    hook_code="${!hook_code_var}"
    echo "Running hook: $hook_code_var"
    eval "${hook_code}"
}

resolve-template() {
    local name="$1" r="" d t
    shift
    for d in "$@"; do
        if [ -z "$d" ] || ! [ -d "$d" ]; then
            continue
        fi
        t="$d/$name.in"
        if ! [ -e "$t" ]; then
            continue
        fi
        if [ -z "$r" ]; then
            r="$t"
            echo 1>&2 "template $name resolved to file $r"
        else
            echo 1>&2 "WARNING: template file $r shadows $t"
        fi
    done
    if [ -n "$r" ]; then
        echo "$r"
        return 0
    fi
    return 1
}

get-ctr-id() { # script API
    # Usage: get-ctr-id pod ctr
    #
    # Runs kubectl get pod $pod -ojson | jq '.status.containerStatuses | \
    #   .[] | select ( .name == "$ctr" ) | .containerID' | sed 's:.*//::g;s:"::g'
    local pod="$1" ctr="$2"
    vm-command-q "kubectl get pod $pod -ojson | \
                 jq '.status.containerStatuses | .[] | select ( .name == \"$ctr\" ) | .containerID'" |\
                 tr -d '"' | sed 's:.*//::g'
}

delete() { # script API
    # Usage: delete PARAMETERS
    #
    # Run "kubectl delete PARAMETERS".
    vm-command "kubectl delete $*" || {
        command-error "kubectl delete failed"
    }
}

instantiate() { # script API
    # Usage: instantiate FILENAME
    #
    # Produces $TEST_OUTPUT_DIR/instance/FILENAME. Prints the filename on success.
    # Uses FILENAME.in as source (resolved from $TEST_DIR, $TOPOLOGY_DIR, ...)
    local FILENAME="$1"
    local RESULT="$TEST_OUTPUT_DIR/instance/$FILENAME"

    template_file=$(resolve-template "$FILENAME" "$TEST_DIR" "$TOPOLOGY_DIR" "$POLICY_DIR" "$SCRIPT_DIR")
    if [ ! -f "$template_file" ]; then
        error "error instantiating \"$FILENAME\": missing template ${template_file}"
    fi
    mkdir -p "$(dirname "$RESULT")" 2>/dev/null
    eval "echo -e \"$(<"${template_file}")\"" | grep -v '^ *$' > "$RESULT" ||
        error "instantiating \"$FILENAME\" failed"
    echo "$RESULT"
}

helm-launch() { # script API
    # Usage: helm-launch TARGET
    #
    # Launch the given target plugin using helm. Start port-forwarding and log
    # collection for the plugin.
    #
    # Supported TARGETs:
    #     topology-aware, balloons: launch the given NRI resource policy plugin on VM.
    #
    # Environment variables:
    #     helm_config: configuration helm override values for the plugin
    #         default: $TEST_DIR/helm-config.yaml
    #     daemonset_name: name of the DaemonSet to wait for
    #     container_name: name of the container to collect logs for
    #         default: nri-resource-policy-$TARGET
    #     helm_name: helm installation name to use
    #         default: test
    #     launch_timeout: timeout to wait for DaemonSet to become available
    #         default: 20s
    #     cfgresource: config custom resource to wait for node status to change in
    #         default: balloonspolicies/default or topologyawarepolicies/default
    #     expect_error: don't exit, expect availability error
    #
    # Example:
    #     helm_config=$(instantiate helm-config.yaml) helm-launch balloons
    #

    local helm_config="${helm_config:-$TEST_DIR/helm-config.yaml}"
    local ds_name="${daemonset_name:-}" ctr_name="${container_name:-nri-resource-policy-$1}"
    local helm_name="${helm_name:-test}" timeout="${launch_timeout:-20s}"
    local expect_error="${expect_error:-0}"
    local plugin="$1" cfgresource=${cfgresource:-} cfgstatus
    local deadline
    shift

    host-command "$SCP \"$helm_config\" $VM_HOSTNAME:" ||
        command-error "copying \"$helm_config\" to VM failed"

    vm-command "helm install --atomic -n kube-system $helm_name ./helm/$plugin \
             --values=`basename ${helm_config}` \
             --set image.name=localhost/$plugin \
             --set image.tag=testing \
             --set image.pullPolicy=Never \
             --set resources.cpu=50m \
             --set resources.memory=256Mi \
             --set plugin-test.enableAPIs=true" ||
        error "failed to helm install/start plugin $plugin"

    case "$timeout" in
        ""|"0"|"none")
            timeout="0"
            ;;
    esac

    if [ -z "$ds_name" ]; then
        case "$plugin" in
            *topology*aware*)
                ds_name=nri-resource-policy-topology-aware
                [ -z "$cfgresource" ] && cfgresource=topologyawarepolicies/default
                ;;
            *balloons*)
                ds_name=nri-resource-policy-balloons
                [ -z "$cfgresource" ] && cfgresource=balloonspolicies/default
                ;;
            *memory-policy*)
                ds_name=nri-memory-policy
                ctr_name=nri-memory-policy
                ;;
            *)
                error "Can't wait for plugin $plugin to start, daemonset_name not set"
                return 0
                ;;
        esac
    fi

    deadline=$(deadline-for-timeout $timeout)
    vm-command "kubectl wait -n kube-system ds/${ds_name} --timeout=$timeout \
                    --for=jsonpath='{.status.numberAvailable}'=1"

    if [ "$COMMAND_STATUS" != "0" ]; then
        if [ "$expect_error" != "1" ]; then
            error "Timeout while waiting daemonset/${ds_name} to be available"
        else
            return 1
        fi
    fi

    if [[ -n "$cfgresource" ]]; then
        timeout=$(timeout-for-deadline $deadline)
        timeout=$timeout wait-config-node-status $cfgresource

        result=$(get-config-node-status-result $cfgresource)
        if [ "$result" != "Success" ]; then
            reason=$(get-config-node-status-error $cfgresource)
            error "Plugin $plugin configuration failed: $reason"
        fi
    fi

    vm-start-log-collection -n kube-system ds/$ds_name -c $ctr_name
    vm-port-forward-enable
}

helm-terminate() { # script API
    # Usage: helm-terminate
    #
    # Stop a helm-launched plugin.
    #
    # Environment variables:
    #     helm_name: helm installation name to stop,
    #         default: test
    #
    # Example:
    #     helm_name=custom-name helm-terminate
    #

    local helm_name="${helm_name:-test}"

    vm-command "helm list -n kube-system | grep -q ^$helm_name"
    if [ "$?" != "0" ]; then
        return 0
    fi
    vm-command "helm uninstall -n kube-system test --wait --timeout 20s"
    vm-port-forward-disable
}

deadline-for-timeout() {
    local timeout="$1" now=$(date +%s) diff

    case $timeout in
        [0-9]*m*[0-9]*s)
            diff="${timeout#*m}"; diff="${diff%s}"
            diff=$(($diff + 60*${timeout%m*}))
            ;;
        [0-9]*m)
            diff=$((${timeout%m} * 60))
            ;;
        [0-9]*s|[0-9]*)
            diff="${timeout%s}"
            ;;
        *)
            echo "can't handle timeout \"$timeout\""
            exit 1
            ;;
    esac

    echo $((now + diff))
}

timeout-for-deadline() {
    local deadline="$1" now=$(date +%s)
    local diff=$((deadline - now))

    if [ "$diff" -gt 0 ]; then
        echo "${diff}s"
    else
        echo 0s
    fi
}

get-config-generation() {
    local resource="$1"
    vm-command-q "kubectl get -n kube-system $resource -ojsonpath={.metadata.generation}"
}

get-hostname-for-vm() {
    local node="${node:-$VM_HOSTNAME}"
    echo ${node%.*}
}

get-config-node-status-generation() {
    local resource="$1" node="$(get-hostname-for-vm)"
    vm-command-q "kubectl get -n kube-system $resource \
                      -ojsonpath=\"{.status.nodes['$node'].generation}\""
}

get-config-node-status-result() {
    local resource="$1" node="$(get-hostname-for-vm)"
    vm-command-q "kubectl get -n kube-system $resource \
                     -ojsonpath=\"{.status.nodes['$node'].status}\""
}

get-config-node-status-error() {
    local resource="$1" node="$(get-hostname-for-vm)"
    vm-command-q "kubectl get -n kube-system $resource \
                      -ojsonpath=\"{.status.nodes['$node'].errors}\""
}

wait-config-node-status() {
    local resource="$1" node="$(get-hostname-for-vm)"
    local timeout="${timeout:-5s}"
    local deadline=$(deadline-for-timeout $timeout)
    local generation jsonpath result errors

    generation=$(get-config-generation $resource)
    jsonpath="{.status.nodes['$node'].generation}"

    vm-command-q "kubectl wait -n kube-system --timeout=$timeout \
                      $resource --for=jsonpath=\"$jsonpath\"=$generation > /dev/null" ||
        error "waiting for node $node update in $resource failed"
}

declare -a pulled_images_on_vm
create() { # script API
    # Usage: [VAR=VALUE][n=COUNT] create TEMPLATE_NAME
    #
    # Create n instances from TEMPLATE_NAME.yaml.in, copy each of them
    # from host to vm, kubectl create -f them, and wait for them
    # becoming Ready. Templates are searched in $TEST_DIR, $TOPOLOGY_DIR,
    # $POLICY_DIR, and $SCRIPT_DIR/files in this order of preference. The first
    # template found is used.
    #
    # Parameters:
    #   TEMPLATE_NAME: the name of the template without extension (.yaml.in)
    #
    # Optional parameters (VAR=VALUE):
    #   namespace: namespace to which instances are created
    #   wait: condition to be waited for (see kubectl wait --for=condition=).
    #         If empty (""), skip waiting. The default is wait="Ready".
    #   wait_t: wait timeout. The default is wait_t=240s.
    local template_file
    template_file=$(resolve-template "$1.yaml" "$TEST_DIR" "$TOPOLOGY_DIR" "$POLICY_DIR" "$SCRIPT_DIR/files")
    local namespace_args
    local template_kind
    template_kind=$(awk '/kind/{print tolower($2)}' < "$template_file")
    local wait=${wait-Ready}
    local wait_t=${wait_t-240s}
    local images
    local image
    local tag
    local errormsg
    local default_name=${NAME:-""}
    if [ -z "$n" ]; then
        local n=1
    fi
    if [ -n "${namespace:-}" ]; then
        namespace_args="-n $namespace"
    else
        namespace_args=""
    fi
    if [ ! -f "$template_file" ]; then
        error "error creating from template \"$template_file.yaml.in\": template file not found"
    fi
    for _ in $(seq 1 $n); do
        kind_count[$template_kind]=$(( ${kind_count[$template_kind]} + 1 ))
        if [ -n "$default_name" ]; then
            local NAME="$default_name"
        else
            local NAME="${template_kind}$(( ${kind_count[$template_kind]} - 1 ))" # the first pod is pod0
        fi
        eval "echo -e \"$(<"${template_file}")\"" | grep -v '^ *$' > "$TEST_OUTPUT_DIR/$NAME.yaml"
        host-command "$SCP \"$TEST_OUTPUT_DIR/$NAME.yaml\" $VM_HOSTNAME:" || {
            command-error "copying \"$TEST_OUTPUT_DIR/$NAME.yaml\" to VM failed"
        }
        vm-command "cat $NAME.yaml"
        images="$(grep -E '^ *image: .*$' "$TEST_OUTPUT_DIR/$NAME.yaml" | sed -E 's/^ *image: *([^ ]*)$/\1/g' | sort -u)"
        if [ "${#pulled_images_on_vm[@]}" = "0" ]; then
            # Initialize pulled images available on VM
            vm-command "crictl -i unix://${k8scri_sock} images" >/dev/null &&
            while read -r image tag _; do
                if [ "$image" = "IMAGE" ]; then
                    continue
                fi
                local notopdir_image="${image#*/}"
                local norepo_image="${image##*/}"
                if [ "$tag" = "latest" ]; then
                    pulled_images_on_vm+=("$image")
                    pulled_images_on_vm+=("$notopdir_image")
                    pulled_images_on_vm+=("$norepo_image")
                fi
                pulled_images_on_vm+=("$image:$tag")
                pulled_images_on_vm+=("$notopdir_image:$tag")
                pulled_images_on_vm+=("$norepo_image:$tag")
            done <<< "$COMMAND_OUTPUT"
        fi
        for image in $images; do
            if ! [[ " ${pulled_images_on_vm[*]} " == *" ${image} "* ]]; then
                if [ "$use_host_images" == "1" ] && vm-put-docker-image "$image"; then
                    : # no need to pull the image to vm, it is now imported.
                else
                    vm-command "crictl -i unix://${k8scri_sock} pull \"$image\"" || {
                        errormsg="pulling image \"$image\" for \"$TEST_OUTPUT_DIR/$NAME.yaml\" failed."
                        if is-hooked on_create_fail; then
                            echo "$errormsg"
                            run-hook on_create_fail
                        else
                            command-error "$errormsg"
                        fi
                    }
                fi
                pulled_images_on_vm+=("$image")
            fi
        done
        vm-command "kubectl create -f $NAME.yaml $namespace_args" || {
            if is-hooked on_create_fail; then
                echo "kubectl create error"
                run-hook on_create_fail
            else
                command-error "kubectl create error"
            fi
        }
        if [ "x$wait" != "x" ]; then
            vm-command "kubectl wait --timeout=${wait_t} --for=condition=${wait} $namespace_args ${template_kind}/$NAME" >/dev/null 2>&1 || {
                errormsg="waiting for ${template_kind} \"$NAME\" to become ready timed out"
                if is-hooked on_create_fail; then
                    echo "$errormsg"
                    run-hook on_create_fail
                else
                    command-error "$errormsg"
                fi
            }
        fi
    done
    is-hooked on_create && run-hook on_create
    return 0
}

reset() { # script API
    # Usage: reset counters
    #
    # Resets counters
    if [ "$1" == "counters" ]; then
        kind_count[pod]=0
    else
        error "invalid reset \"$1\""
    fi
}

get-py-allowed() {
    topology_dump_file="$TEST_OUTPUT_DIR/topology_dump.$VM_HOSTNAME"
    res_allowed_file="$TEST_OUTPUT_DIR/res_allowed.$VM_HOSTNAME"
    if ! [ -f "$topology_dump_file" ]; then
        vm-command "$("$LIB_DIR/topology.py" bash_topology_dump)" >/dev/null || {
            command-error "error fetching topology_dump from $VM_HOSTNAME"
        }
        echo -e "$COMMAND_OUTPUT" > "$topology_dump_file"
    fi
    # Fetch data and update allowed* variables from the virtual machine
    vm-command "$("$LIB_DIR/topology.py" bash_res_allowed 'pod[0-9]*c[0-9]*')" >/dev/null || {
        command-error "error fetching res_allowed from $VM_HOSTNAME"
    }
    echo -e "$COMMAND_OUTPUT" > "$res_allowed_file"
    # Validate res_allowed_file. Error out if there is same container
    # name with two different sets of allowed CPUs or memories.
    awk -F "[ /]" '{if (pod[$1]!=0 && pod[$1]!=""$3""$4){print "error: ambiguous allowed resources for name "$1; exit(1)};pod[$1]=""$3""$4}' "$res_allowed_file" || {
        error "container/process name collision: test environment needs cleanup."
    }
    py_allowed="
import re
allowed=$("$LIB_DIR/topology.py" -t "$topology_dump_file" -r "$res_allowed_file" res_allowed -o json)
_branch_pod=[(p, d, n, c, t, cpu, pod.rsplit('/', 1)[0])
             for p in allowed
             for d in allowed[p]
             for n in allowed[p][d]
             for c in allowed[p][d][n]
             for t in allowed[p][d][n][c]
             for cpu in allowed[p][d][n][c][t]
             for pod in allowed[p][d][n][c][t][cpu]]
# cpu resources allowed for a pod:
packages, dies, nodes, cores, threads, cpus = {}, {}, {}, {}, {}, {}
# mem resources allowed for a pod:
mems = {}
for p, d, n, c, t, cpu, pod in _branch_pod:
    if c == 'mem': # this _branch_pod entry is about memory
        if not pod in mems:
            mems[pod] = set()
        # topology.py can print memory nodes as children of cpu-ful nodes
        # if distance looks like they are behind the same memory controller.
        # The thread field, however, is the true node who contains the memory.
        mems[pod].add(t)
        continue
    # this _branch_pod entry is about cpu
    if not pod in packages:
        packages[pod] = set()
        dies[pod] = set()
        nodes[pod] = set()
        cores[pod] = set()
        threads[pod] = set()
        cpus[pod] = set()
    packages[pod].add(p)
    dies[pod].add('%s/%s' % (p, d))
    nodes[pod].add(n)
    cores[pod].add('%s/%s' % (n, c))
    threads[pod].add('%s/%s/%s' % (n, c, t))
    cpus[pod].add(cpu)

def disjoint_sets(*sets):
    'set.isdisjoint() for n > 1 sets'
    s = sets[0]
    for next in sets[1:]:
        if not s.isdisjoint(next):
            return False
        s = s.union(next)
    return True

def set_ids(str_ids, chars='[a-z]'):
    num_ids = set()
    for str_id in str_ids:
        if '/' in str_id:
            num_ids.add(tuple(int(re.sub(chars, '', s)) for s in str_id.split('/')))
        else:
            num_ids.add(int(re.sub(chars, '', str_id)))
    return num_ids
package_ids = lambda i: set_ids(i, '[package]')
die_ids = lambda i: set_ids(i, '[packagedie]')
node_ids = lambda i: set_ids(i, '[node]')
core_ids = lambda i: set_ids(i, '[nodecore]')
thread_ids = lambda i: set_ids(i, '[nodecorethread]')
cpu_ids = lambda i: set_ids(i, '[cpu]')
"
}

get-py-cache() {
    # Fetch current nri-resource-policy cache from a virtual machine.
    vm-command "cat \"$nri_resource_policy_cache\"" >/dev/null 2>&1 || {
        command-error "fetching cache file failed"
    }
    cat > "${TEST_OUTPUT_DIR}/cache" <<<"$COMMAND_OUTPUT"
    py_cache="
import json
cache=json.load(open(\"${TEST_OUTPUT_DIR}/cache\"))
try:
    allocations=json.loads(cache['PolicyJSON']['allocations'])
except KeyError:
    allocations=None
containers=cache['Containers']
pods=cache['Pods']
for _contid in list(containers.keys()):
    try:
        _cmd = ' '.join(containers[_contid]['Command'])
    except:
        continue # Command may be None
    # Recognize echo podXcY ; sleep inf -type test pods and make them
    # easily accessible: containers['pod0c0'], pods['pod0']
    if 'echo pod' in _cmd and 'sleep inf' in _cmd:
        _contname = _cmd.split()[3] # _contname is podXcY
        _podid = containers[_contid]['PodID']
        _podname = pods[_podid]['Name'] # _podname is podX
        if not allocations is None and _contid in allocations:
            allocations[_contname] = allocations[_contid]
        containers[_contname] = containers[_contid]
        pods[_podname] = pods[_podid]
"
}

pyexec() { # script API
    # Usage: pyexec [PYTHONCODE...]
    #
    # Run python3 -c PYTHONCODEs on host. Stops if execution fails.
    #
    # Variables available in PYTHONCODE:
    #   allocations: dictionary: shorthand to nri-resource-policy policy allocations
    #                (unmarshaled cache['PolicyJSON']['allocations'])
    #   allowed      tree: {package: {die: {node: {core: {thread: {pod}}}}}}
    #                resource topology and pods allowed to use the resources.
    #   packages, dies, nodes, cores, threads:
    #                dictionaries: {podname: set-of-allowed}
    #                Example: pyexec 'print(dies["pod0c0"])'
    #   cache:       dictionary, nri-resource-policy cache
    #
    # Note that variables are *not* updated when pyexec is called.
    # You can update the variables by running "verify" without expressions.
    #
    # Code in environment variable py_consts runs before PYTHONCODE.
    #
    # Example:
    #   verify ; pyexec 'import pprint; pprint.pprint(allowed)'
    PYEXEC_STATE_PY="$TEST_OUTPUT_DIR/pyexec_state.py"
    PYEXEC_PY="$TEST_OUTPUT_DIR/pyexec.py"
    PYEXEC_LOG="$TEST_OUTPUT_DIR/pyexec.output.txt"
    local last_exit_status=0
    {
        echo "import pprint; pp=pprint.pprint"
        echo "# \$py_allowed:"
        echo -e "$py_allowed"
        echo "# \$py_cache:"
        echo -e "$py_cache"
        echo "# \$py_consts:"
        echo -e "$py_consts"
    } > "$PYEXEC_STATE_PY"
    for PYTHONCODE in "$@"; do
        {
            echo "from pyexec_state import *"
            echo -e "$PYTHONCODE"
        } > "$PYEXEC_PY"
        PYTHONPATH="$TEST_OUTPUT_DIR:$PYTHONPATH:$LIB_DIR" python3 "$PYEXEC_PY" 2>&1 | tee "$PYEXEC_LOG"
        last_exit_status=${PIPESTATUS[0]}
        if [ "$last_exit_status" != "0" ]; then
            error "pyexec: non-zero exit status \"$last_exit_status\", see \"$PYEXEC_PY\" and \"$PYEXEC_LOG\""
        fi
    done
    return "$last_exit_status"
}

pp() { # script API
    # Usage: pp EXPR
    #
    # Pretty-print the value of Python expression EXPR.
    pyexec "pp($*)"
}

report() { # script API
    # Usage: report [VARIABLE...]
    #
    # Updates and reports current value of VARIABLE.
    #
    # Supported VARIABLEs:
    #     allocations
    #     allowed
    #     cache
    #
    # Example: print nri-resource-policy policy allocations. In interactive mode
    #          you may use a pager like less.
    #   report allocations | less
    local varname
    for varname in "$@"; do
        if [ "$varname" == "allocations" ]; then
            get-py-cache
            pyexec "
import pprint
pprint.pprint(allocations)
"
        elif [ "$varname" == "allowed" ]; then
            get-py-allowed
            pyexec "
import topology
print(topology.str_tree(allowed))
"
        elif [ "$varname" == "cache" ]; then
            get-py-cache
            pyexec "
import pprint
pprint.pprint(cache)
"
        else
            error "report: unknown variable \"$varname\""
        fi
    done
}

verify() { # script API
    # Usage: verify [EXPR...]
    #
    # Run python3 -c "assert(EXPR)" to test that every EXPR is True.
    # Stop evaluation on the first EXPR not True and fail the test.
    # You can allow script execution to continue after failed verification
    # by running verify in a subshell (in parenthesis):
    #   (verify 'False') || echo '...but was expected to fail.'
    #
    # Variables available in EXPRs:
    #   See variables in: help pyexec
    #
    # Note that all variables are updated every time verify is called
    # before evaluating (asserting) expressions.
    #
    # Example: require that containers pod0c0 and pod1c0 run on separate NUMA
    #          nodes and that pod0c0 is allowed to run on 4 CPUs:
    #   verify 'set.intersection(nodes["pod0c0"], nodes["pod1c0"]) == set()' \
    #          'len(cpus["pod0c0"]) == 4'
    get-py-allowed
    get-py-cache
    for py_assertion in "$@"; do
        speed=1000 out "### Verifying assertion '$py_assertion'"
        ( speed=1000 pyexec "
try:
    import time,sys
    assert(${py_assertion})
except KeyError as e:
    print('WARNING: *')
    print('WARNING: *** KeyError - %s' % str(e))
    print('WARNING: *** Your verify expression might have a typo/thinko.')
    print('WARNING: *')
    sys.stdout.flush()
    time.sleep(5)
    raise e
except IndexError as e:
    print('WARNING: *')
    print('WARNING: *** IndexError - %s' % str(e))
    print('WARNING: *** Your verify expression might have a typo/thinko.')
    print('WARNING: *')
    sys.stdout.flush()
    time.sleep(5)
    raise e
" ) || {
                out "### The assertion FAILED
### post-mortem debug help begin ###
cd $TEST_OUTPUT_DIR
python3
from pyexec_state import *
$py_assertion
### post-mortem debug help end ###"
                echo "verify: assertion '$py_assertion' failed." >> "$SUMMARY_FILE"
                if is-hooked on_verify_fail; then
                    run-hook on_verify_fail
                else
                    command-exit-if-not-interactive
                fi
        }
        out "### The assertion holds."
    done
    is-hooked on_verify && run-hook on_verify
    return 0
}

# Defaults to use in case the test case does not define these values.
yaml_in_defaults="CPU=1 MEM=100M ISO=true CPUREQ=1 CPULIM=2 MEMREQ=100M MEMLIM=200M CONTCOUNT=1"

declare -A kind_count # associative arrays for counting created objects, like kind_count[pod]=1
eval "${yaml_in_defaults}"

# Run test/demo
TEST_FAILURES=""
test_start_secs=$(vm-seconds-now)

test-user-code

test_span_secs="$(vm-seconds-since $test_start_secs)"
since="-$(( test_span_secs + 5 ))s"
service="${k8scri}" since="$since" vm-pull-journal > "${TEST_OUTPUT_DIR}"/runtime."${k8scri}".log

# If there are any nri-resource-policy logs in the DUT, copy them back to host.
host-command "$SCP $VM_HOSTNAME:nri-resource-policy.output.txt \"${TEST_OUTPUT_DIR}/\"" ||
    out "copying \"$nri-resource-policy.output.txt\" from VM failed"

# Summarize results
exit_status=0
if [ -n "$TEST_FAILURES" ]; then
    echo "Test verdict: FAIL" >> "$SUMMARY_FILE"
else
    echo "Test verdict: PASS" >> "$SUMMARY_FILE"
fi
cat "$SUMMARY_FILE"
exit $exit_status
