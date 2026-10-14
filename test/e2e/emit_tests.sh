#!/bin/bash
#
# Generate e2e tests from the e2eish tests of a policy made with
# NewE2EScript, those which pass. See "Generating e2e tests from unit tests"
# in test/e2e/README.md.
#
# Usage: [E2EISH_EMIT_FORCE=1] emit_tests.sh POLICY [-- GO-TEST-ARG...]
#
# E2EISH_EMIT_FORCE=1 replaces generated tests which differ. Arguments after
# -- go to go test, like -run TestE2eishExample to pick tests.

set -e -o pipefail

usage() {
    sed -n '3,10s/^# \?//p' "$0"
}

case "$1" in
    -h|--help) usage; exit 0;;
    ""|--) usage >&2; exit 1;;
esac
policy="$1"
shift
[ "$1" == "--" ] && shift

cd "$(dirname "$0")/../.."
suite="$PWD/test/e2e/policies.test-suite/$policy"
if [ ! -d "$suite" ]; then
    echo "emit_tests.sh: no test suite $suite" >&2
    exit 1
fi
E2EISH_EMIT="$suite" "${GO_CMD:-go}" test "./cmd/plugins/$policy/policy/" \
    -run 'TestE2eish' -count=1 "$@"
git status --short -- "$suite"
