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
