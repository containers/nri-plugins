# Shared helpers for test cases (code.var.sh files).
#
# This library is not sourced by run.sh. Instead, run_tests.sh seeds the
# *.source.sh chain with it, so that
#
#   - the helpers live in the same subshell as the test code and nowhere else,
#     and cannot clash with the script API of run.sh, and
#   - any *.source.sh file (test suite, policy, topology or test case level) can
#     override a helper defined here, because those are sourced after this file.
#
# Helpers here are documented in the same format as the script API of run.sh,
# that is, a "# script API" marked function followed by indented comment lines.
# Run "./run.sh help" to print the documentation of all available functions.
#
# Add a helper here if more than one test case needs it. Keep policy-specific
# helpers in POLICY/*.source.sh instead.

# Fail if this was sourced without the primitives it builds on.
if ! type -t vm-command >/dev/null; then
    echo "error: lib/test.bash: lib/vm.bash must be sourced first" >&2
    return 1
fi

###
### Waiting for things to happen
###

retry-until() { # script API
    # Usage: retry-until [--timeout SECS] [--interval SECS] [--message MSG] SNIPPET
    #
    # Evaluate SNIPPET on the host repeatedly until it exits with status 0.
    # Give up after SECS seconds (default 30), waiting SECS seconds between
    # the attempts (default 1). Both must be integers. Print MSG before the
    # first attempt and when giving up.
    #
    # Return 0 if SNIPPET succeeded, non-zero if it did not. Never fails the
    # test, so that the caller can choose between error, command-error and
    # ignoring the timeout.
    #
    # SNIPPET is evaluated, so single-quote it to have the values it refers to
    # re-read on every attempt:
    #   retry-until --timeout 10 'vm-command "kubectl get pod $pod"'
    #
    # This is the host side counterpart of vm-run-until.
    local timeout=30 interval=1 message="" elapsed=0
    while [ "${1#--}" != "$1" ]; do
        case "$1" in
            --timeout)  timeout="$2"; shift 2;;
            --interval) interval="$2"; shift 2;;
            --message)  message="$2"; shift 2;;
            --)         shift; break;;
            *)          error "retry-until: unknown option \"$1\"";;
        esac
    done
    if [ -n "$message" ]; then
        echo "waiting for: $message"
    fi
    while true; do
        if eval "$*"; then
            return 0
        fi
        if [ "$elapsed" -ge "$timeout" ]; then
            break
        fi
        sleep "$interval"
        elapsed=$(( elapsed + interval ))
    done
    echo "timeout after ${timeout}s${message:+ waiting for: $message}" >&2
    return 1
}

wait-pod-gone() { # script API
    # Usage: wait-pod-gone POD [TIMEOUT]
    #
    # Wait until POD no longer exists, TIMEOUT seconds at most, 30 by default.
    # Fail the test if the pod is still there after that.
    local pod=$1 timeout=${2:-30}
    vm-run-until --timeout "$timeout" "! kubectl get pod $pod -o name 2>/dev/null | grep -q ." || {
        command-error "pod $pod did not disappear within ${timeout}s"
    }
}

###
### Launching the plugin
###

expect-launch-failure() { # script API
    # Usage: expect-launch-failure POLICY [TIMEOUT]
    #
    # Expect launching POLICY to fail within TIMEOUT, 5s by default. Fail the
    # test if the launch succeeds instead.
    #
    # Read the configuration from the helm_config variable, just like
    # helm-launch does. Use this to test that the plugin refuses an invalid
    # configuration:
    #     helm_config=$(instantiate broken-config.yaml) expect-launch-failure balloons
    local policy=$1 timeout=${2:-5s}

    # helm-launch is run in a subshell, because on some failures it calls
    # error, which would otherwise fail the test we expect to fail.
    if ( expect_error=1 launch_timeout=$timeout helm-launch "$policy" ); then
        error "launching $policy succeeded, but was expected to fail"
    fi
    echo "launching $policy failed as expected"
}

###
### Cleaning up
###

delete-pods() { # script API
    # Usage: delete-pods [-n NAMESPACE] {--all | POD...}
    #
    # Delete pods immediately, ignoring pods which do not exist. Delete the
    # pods in NAMESPACE, or in the default namespace if -n is not given.
    #
    # Never fails, so this is safe to call both before and after a test.
    local ns=""
    if [ "$1" == "-n" ]; then
        ns="-n $2"
        shift 2
    fi
    if [ $# -eq 0 ]; then
        error "delete-pods: expected --all or a list of pods"
    fi
    vm-command "kubectl delete pods $ns $* --now --ignore-not-found=true" || :
}

create-namespaces() { # script API
    # Usage: create-namespaces NAMESPACE...
    #
    # Create namespaces unless they already exist. Never fails.
    local ns
    for ns in "$@"; do
        vm-command "kubectl get namespace $ns > /dev/null 2>&1 || kubectl create namespace $ns" || :
    done
}

delete-namespaces() { # script API
    # Usage: delete-namespaces NAMESPACE...
    #
    # Delete all pods in the namespaces, then the namespaces themselves.
    # Namespaces which do not exist are ignored. Never fails.
    local ns
    for ns in "$@"; do
        vm-command "kubectl delete pods -n $ns --all --now --ignore-not-found=true
                    kubectl delete namespace $ns --now --ignore-not-found=true" || :
    done
}

remove-policy-cache() { # script API
    # Usage: remove-policy-cache
    #
    # Remove the cache file of the resource policy from the node. Never fails.
    #
    # Use this to prevent the cache of a previously running policy from
    # affecting the policy which the test launches.
    vm-command "rm -f /var/lib/nri-resource-policy/cache" || :
}

kill-test-processes() { # script API
    # Usage: kill-test-processes
    #
    # Kill leftover processes of test containers in the VM. Never fails.
    #
    # The patterns match the commands which the pod templates in files/ run.
    # They use a bracket expression so that they do not match the command
    # line of the shell which runs pkill.
    vm-command 'pkill -9 -f "sleep[ ]inf"
                pkill -9 -f "echo[ ]pod"' || :
}
