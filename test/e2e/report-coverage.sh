#!/bin/bash
#
# Report the coverage of the e2e tests. The tests collect the coverage data of
# the plugins they exercise into a coverage directory per test case. This
# script merges the data of all of them into a single report.
#
# Usage: report-coverage.sh [--output OUTDIR] [DIR...]
#
# Look for the data under each DIR, the current working directory by default,
# and write the report to OUTDIR, the coverage-report directory of the first
# DIR by default. Print the coverage of each plugin and the total.
#
# Give several DIRs to report on the tests of one plugin across the topologies
# it was tested on, for instance.
#
# The report consists of coverprofile in the text format go tool cover reads,
# coverage.html for browsing, the data merged over the tests in merged/, and
# summary.json with the numbers in it for whoever reports on them further.
#
# The plugins must have been built with coverage instrumentation for there to
# be any data to report on. make e2e-tests does that, but a manual "make
# images" does not, unless given COVER=1.

set -o pipefail

GO_CMD="${GO_CMD:-go}"

usage() {
    echo "Usage: report-coverage.sh [--output OUTDIR] [DIR...]"
    echo ""
    echo "Merge and report the coverage data the e2e tests collected under the"
    echo "given directories, the current working directory by default."
    echo ""
    echo "  -o, --output OUTDIR  write the report here, instead of to the"
    echo "                       coverage-report directory of the first DIR"
    echo "  -h, --help           show this help"
}

# Usage: summarize PROFILE TESTS SUMMARY
#
# Print the coverage of the logic of each plugin in PROFILE, in other words of
# the code under cmd/plugins/PLUGIN, and the total over everything
# instrumented, and write the same numbers as json to SUMMARY. TESTS is the
# number of test cases the profile covers, reported as is in the json.
#
# The arithmetic is in e2e-report, which reports on a whole run with the same
# numbers, from a directory of its own so that it runs from any directory.
#
# Note that go tool covdata percent cannot report any of this: it only ever
# reports per package, and it prints a package which has no statements at all,
# such as one declaring nothing but types, without a percentage and without a
# line break, running the line of the next package into it.
summarize() {
    local profile="$1" tests="$2" summary="$3"

    (cd "$(dirname "$0")" &&
         "$GO_CMD" run ./cmd/e2e-report coverage \
                   --tests "$tests" --summary "$summary" "$profile")
}

outdir=""
dirs=()

while [ "$#" -gt 0 ]; do
    case "$1" in
        -h|--help|help)
            usage
            exit 0
            ;;
        -o|--output)
            if [ -z "$2" ]; then
                echo "report-coverage.sh: $1 needs a directory" >&2
                exit 1
            fi
            outdir="$2"
            shift 2
            ;;
        -*)
            echo "report-coverage.sh: unknown option: $1" >&2
            usage >&2
            exit 1
            ;;
        *)
            dirs+=("$1")
            shift
            ;;
    esac
done

if [ "${#dirs[@]}" = "0" ]; then
    dirs=("$(pwd)")
fi

for i in "${!dirs[@]}"; do
    if [ ! -d "${dirs[$i]}" ]; then
        echo "report-coverage.sh: no such directory: ${dirs[$i]}" >&2
        exit 1
    fi
    dirs[$i]=$(realpath "${dirs[$i]}")
done

outdir=$(realpath -m "${outdir:-${dirs[0]}/coverage-report}")
merged="$outdir/merged"
profile="$outdir/coverprofile"
html="$outdir/coverage.html"
summary="$outdir/summary.json"

# Collect the directories which hold the data of a test case, skipping our own
# output. Both the name of a file and the coverage directory holding it have to
# match, the same way as when the data is discarded: either on its own is too
# little, as a directory to report on can be anywhere, the source tree included,
# where a directory named coverage is just as likely to be a package of ours,
# and a covmeta file outside one is not from a test of ours.
data_dirs=$(find "${dirs[@]}" -type f -path '*/coverage/covmeta.*' \
                -not -path "$outdir/*" -printf '%h\n' | sort -u | paste -sd, -)
tests=$(echo "$data_dirs" | tr ',' '\n' | grep -c .)

if [ -z "$data_dirs" ]; then
    echo "No coverage data found under ${dirs[*]}."
    echo "Were the plugins built with coverage instrumentation (make COVER=1)?"
    exit 0
fi

if ! command -v "$GO_CMD" >/dev/null; then
    echo "No $GO_CMD available, cannot report on the collected coverage data." >&2
    exit 1
fi

rm -rf "$outdir"
mkdir -p "$merged" || exit 1

echo ""
echo "Merging the coverage data of $tests test cases..."
if ! "$GO_CMD" tool covdata merge -i="$data_dirs" -o="$merged"; then
    echo "Failed to merge the collected coverage data." >&2
    exit 1
fi

if ! "$GO_CMD" tool covdata textfmt -i="$merged" -o="$profile"; then
    echo "Failed to convert the merged coverage data." >&2
    exit 1
fi

"$GO_CMD" tool cover -html="$profile" -o="$html" ||
    echo "WARNING: failed to generate $html"

echo ""
echo "Coverage of the e2e tests:"
summarize "$profile" "$tests" "$summary" ||
    echo "WARNING: failed to summarize $profile"

echo ""
echo "  per package and function: $GO_CMD tool cover -func=$profile"
echo "  browsable report:         $html"
echo "  numbers in json:          $summary"
