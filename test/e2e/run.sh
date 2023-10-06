#!/bin/bash

TITLE="NRI Resource Policy End-to-End Testing"
DEFAULT_DISTRO=${DEFAULT_DISTRO:-"generic/fedora37"}

# Other tested distros
#    generic/ubuntu2204
#    fedora/37-cloud-base

SCRIPT_DIR="$(dirname "${BASH_SOURCE[0]}")"
SRC_DIR=$(realpath "$SCRIPT_DIR/../..")
LIB_DIR=$(realpath "$SCRIPT_DIR/lib")

export OUTPUT_DIR=${outdir:-"$SCRIPT_DIR"/output}

# Place output of each test under a separate test case directory
export TEST_OUTPUT_DIR=${test_outdir:-"$OUTPUT_DIR"}
export COMMAND_OUTPUT_DIR="$TEST_OUTPUT_DIR"/commands

distro=${distro:-$DEFAULT_DISTRO}
export k8scri=${k8scri:-"containerd"}
TOPOLOGY_DIR=${TOPOLOGY_DIR:=e2e}

source "$LIB_DIR"/vm.bash

export vm_name=${vm_name:=$(vm-create-name "$k8scri" "$(basename "$TOPOLOGY_DIR")" ${distro})}
ESCAPED_VM=$(printf '%s\n' "$vm_name" | sed -e 's/[\/]/-/g')
export VM_HOSTNAME="$ESCAPED_VM"
export POLICY=${policy:-"topology-aware"}

# These exports force ssh-* to fail instead of prompting for a passphrase.
export DISPLAY=bogus-none
export SSH_ASKPASS=/bin/false
SSH_OPTS="-F $OUTPUT_DIR/.ssh-config"
export SSH="ssh $SSH_OPTS"
export SCP="scp $SSH_OPTS"
export VM_SSH_USER=vagrant

export nri_resource_policy_src=${nri_resource_policy_src:-"$SRC_DIR"}
export nri_resource_policy_cfg=${nri_resource_policy_cfg:-"${SRC_DIR}/test/e2e/files/nri-resource-policy-topology-aware.cfg"}

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
export containerd_release=1.7.6
export containerd_src=${containerd_src:-}


# If we run tests with CRI-O as the runtime, we install it from
# a release tarball with the version given below... unless a
# source directory is given, which is then expected to contain
# a compiled version of CRI-O which we should install.
export crio_release=1.28.1
export crio_src=${crio_src:-}

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

launch() { # script API
    # Usage: launch TARGET
    #
    # Supported TARGETs:
    #   nri-resource-policy:  launch nri-resource-policy on VM. Environment variables:
    #                nri_resource_policy_cfg: configuration filepath (on host)
    #                nri_resource_policy_extra_args: extra arguments on command line
    #                nri_resource_policy_config: "force" (default) or "fallback"
    #                k8scri: if the CRI pipe starts with nri-resource-policy
    #                        this launches nri-resource-policy as a proxy,
    #                        otherwise as a dynamic NRI plugin.
    #
    #   nri-resource-policy-daemonset:
    #                launch nri-resource-policy on VM using Kubernetes DaemonSet
    #
    #   nri-resource-policy-systemd:
    #                launch nri-resource-policy on VM using "systemctl start".
    #                Works when installed with binsrc=packages/<distro>.
    #                Environment variables:
    #                nri_resource_policy_cfg: configuration filepath (on host)
    #
    # Example:
    #   nri_resource_policy_cfg=/tmp/topology-aware.cfg launch nri-resource-policy

    local target="$1"
    local launch_cmd
    local node_resource_topology_schema="$SRC_DIR/deployment/base/crds/noderesourcetopology_crd.yaml"
    local nri_resource_policy_config_option="-${nri_resource_policy_config:-force}-config"
    local nri_resource_policy_mode=""

    case $target in
        "nri-resource-policy-systemd")
            host-command "$SCP \"$nri_resource_policy_cfg\" $VM_HOSTNAME:" ||
                command-error "copying \"$nri_resource_policy_cfg\" to VM failed"
            vm-command "cp \"$(basename "$nri_resource_policy_cfg")\" /etc/nri-resource-policy/fallback.cfg"
            vm-command "systemctl daemon-reload ; systemctl start nri-resource-policy" ||
                command-error "systemd failed to start nri-resource-policy"
            vm-wait-process --timeout 30 nri-resource-policy
            vm-command "systemctl is-active nri-resource-policy" || {
                vm-command "systemctl status nri-resource-policy"
                command-error "nri-resource-policy did not become active after systemctl start"
            }
            ;;

        "nri-resource-policy")
	    if [ "$nri_resource_policy_config" == "fallback" ]; then
		nri_resource_policy_deployment_file="/etc/nri-resource-policy/nri-resource-policy-deployment-fallback.yaml"
	    else
		nri_resource_policy_deployment_file="/etc/nri-resource-policy/nri-resource-policy-deployment.yaml"
	    fi
	    vm-command "chown $VM_SSH_USER:$VM_SSH_USER /etc/nri-resource-policy/"
	    vm-command "rm -f /etc/nri-resource-policy/nri-resource-policy.cfg"
	    host-command "$SCP \"$nri_resource_policy_cfg\" $VM_HOSTNAME:/etc/nri-resource-policy/nri-resource-policy.cfg" || {
                command-error "copying \"$nri_resource_policy_cfg\" to VM failed"
	    }
            host-command "$SCP \"$node_resource_topology_schema\" $VM_HOSTNAME:" ||
		command-error "copying \"$node_resource_topology_schema\" to VM failed"
            vm-command "kubectl delete -f $(basename "$node_resource_topology_schema"); kubectl create -f $(basename "$node_resource_topology_schema")"
	    vm-command "kubectl apply -f $nri_resource_policy_deployment_file" ||
		error "Cannot apply deployment"

            if [ "${wait_t}" = "none" ]; then
                return 0
            fi

	    # Direct logs to output file
	    local POD="$(namespace=kube-system wait_t=${wait_t:-120} vm-wait-pod-regexp nri-resource-policy-)"
	    if [ ! -z "$POD" ]; then
		# If the POD contains \n, then the old pod is still there. Wait a sec in this
		# case and retry.
		local POD_CHECK=$(echo "$POD" | awk 'BEGIN { RS=""; FS="\n"} { print $2 }')
		if [ ! -z "$POD_CHECK" ]; then
		    sleep 1
		    POD="$(namespace=kube-system wait_t=${wait_t:-60} vm-wait-pod-regexp nri-resource-policy-)"
		    if [ -z "$POD" ]; then
			error "Cannot figure out pod name"
		    fi
		fi

		if [ "$ds_wait_t" != "none" ]; then
		    # Wait a while so that the status check can get somewhat meaningful status
		    vm-command "kubectl -n kube-system rollout status daemonset/nri-resource-policy --timeout=${ds_wait_t:-20s}"
		    if [ $? -ne 0 ]; then
			error "Timeout while waiting daemonset/nri-resource-policy to be ready"
		    fi
		fi

		# Check if we have anything else than Running status for the pod
		status="$(vm-command-q "kubectl get pod "$POD" -n kube-system | tail -1 | awk '{ print \$3 }'")"
		if [ "$status" != "Running" ]; then
		    # Check if nri-resource-policy failed
		    if vm-command "kubectl logs $POD -n kube-system | tail -1 | grep -q ^F" 2>&1; then
			error "Cannot start nri-resource-policy"
		    fi
		fi

		vm-command "fuser --kill nri-resource-policy.output.txt 2>/dev/null"
		vm-command "kubectl -n kube-system logs "$POD" -f >nri-resource-policy.output.txt 2>&1 &"

		vm-port-forward-enable
	    else
		error "nri-resource-policy pod not found"
	    fi
	    ;;

        *)
            error "launch: invalid target \"$1\""
            ;;
    esac
    return 0
}

terminate() { # script API
    # Usage: terminate TARGET
    #
    # Supported TARGETs:
    #   nri-resource-policy: stop (kill) nri-resource-policy.
    local target="$1"
    case $target in
        "nri-resource-policy")
	    vm-port-forward-disable
	    vm-command "kubectl delete -f /etc/nri-resource-policy/nri-resource-policy-deployment.yaml"
            ;;
        *)
            error "terminate: invalid target \"$target\""
            ;;
    esac
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
test-user-code

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
