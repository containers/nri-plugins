#!/bin/bash
#
# Generate the e2eish-gen-TOPOLOGY_test.go unit tests of the policies from the
# e2e test logs in the given directories, then run them. Every run.sh.output
# found under a directory is the log of a test of its policy and topology.
#
# Usage: generate_tests.sh DIR... [-- GO-TEST-ARG...]
#
# The file of each topology with logs is written anew, so give the output of
# a full test run. git diff shows what changed. Arguments after -- go to the
# go test of the generated tests.

set -e -o pipefail

usage() {
    sed -n '3,11s/^# \?//p' "$0"
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
    echo "generate_tests.sh: no run.sh.output in ${dirs[*]}" >&2
    exit 1
fi

cd "$(dirname "$0")/../.."
pkgs=(./cmd/plugins/topology-aware/policy/ ./cmd/plugins/balloons/policy/)
# Print what the generator logs, each line after the test of its log.
E2EISH_GENERATE="$logs" "${GO_CMD:-go}" test "${pkgs[@]}" \
    -tags e2eish_generate -run 'TestE2eishGenerate$' -count=1 -v | awk '
        /^=== RUN +TestE2eishGenerate\// { test = substr($3, 20); next }
        /^ +[^ ]+\.go:[0-9]+: wrote / { sub(/^ +[^ ]+\.go:[0-9]+: /, ""); print; next }
        /^ +[^ ]+\.go:[0-9]+: / { sub(/^ +[^ ]+\.go:[0-9]+: /, ""); print test ": " $0 }
        /^(FAIL|panic)/ { print }'
exec "${GO_CMD:-go}" test "${pkgs[@]}" -run 'TestE2eishGen[A-Z]' -count=1 "$@"
