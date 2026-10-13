#!/bin/bash
#
# Replay the e2e test logs in the given directories against the policy alone,
# as unit tests. Every run.sh.output found under a directory is replayed by
# the TestE2eishReplay of its policy.
#
# Usage: [E2EISH_VERBOSE=1] [E2EISH_LOG_DIR=DIR] replay_tests.sh DIR... [-- GO-TEST-ARG...]
#
# E2EISH_VERBOSE=1 prints the log of each replay as it is collected, and
# E2EISH_LOG_DIR is where the logs of failed replays go. Arguments after --
# go to go test, like -run 'TestE2eishReplay/test04' to pick some logs.

set -e -o pipefail

usage() {
    sed -n '3,12s/^# \?//p' "$0"
}

dirs=()
while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help) usage; exit 0;;
        --) shift; break;;
        *) dirs+=("$(realpath "$1")"); shift;;
    esac
done
if [ ${#dirs[@]} -eq 0 ]; then
    usage >&2
    exit 1
fi

logs=$(find "${dirs[@]}" -name run.sh.output | sort | paste -sd:)
if [ -z "$logs" ]; then
    echo "replay_tests.sh: no run.sh.output in ${dirs[*]}" >&2
    exit 1
fi

cd "$(dirname "$0")/../.."
E2EISH_REPLAY="$logs" exec "${GO_CMD:-go}" test ./cmd/plugins/topology-aware/policy/ \
    -run "TestE2eishReplay" -count=1 -v "$@"
