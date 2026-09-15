#!/bin/bash
#
# Report the total coverage of the e2e tests. The tests collect the coverage
# data of the plugins they exercise into a coverage directory per test case.
# This script merges the data of all of them into a single report.
#
# Usage: report-coverage.sh [DIR]
#
# Look for the data under DIR, the current working directory by default, and
# write the report to DIR/coverage-report. Print a per-package summary and the
# total.
#
# The plugins must have been built with coverage instrumentation for there to
# be any data to report on. make e2e-tests does that, but a manual "make
# images" does not, unless given COVER=1.

set -o pipefail

GO_CMD="${GO_CMD:-go}"

usage() {
    echo "Usage: report-coverage.sh [DIR]"
    echo "Merge and report the coverage data the e2e tests collected under DIR."
}

# Usage: summarize plugins|total PROFILE
#
# Print the coverage of the logic of each plugin in PROFILE, in other words of
# the code under cmd/plugins/PLUGIN, or the total over everything instrumented.
# Weight both by statements, the way go tool cover calculates its percentages.
#
# Note that go tool covdata percent cannot report either of these: it only ever
# reports per package, and it prints a package which has no statements at all,
# such as one declaring nothing but types, without a percentage and without a
# line break, running the line of the next package into it.
summarize() {
    local what="$1" profile="$2"

    awk -v what="$what" '
        function report(label, hits, stmts) {
            printf "  %-28s %5.1f%% (%d/%d statements)\n",
                label, 100 * hits / stmts, hits, stmts
        }

        NR > 1 {
            split($1, path, ":")
            hit = ($3 > 0) ? $2 : 0

            total_stmts += $2
            total_hits += hit

            # Attribute cmd/plugins/PLUGIN/... to the logic of PLUGIN.
            cnt = split(path[1], part, "/")
            for (i = 1; i + 2 <= cnt; i++) {
                if (part[i] == "cmd" && part[i + 1] == "plugins") {
                    plugin = part[i + 2]
                    stmts[plugin] += $2
                    hits[plugin] += hit
                    break
                }
            }
        }

        END {
            if (what == "total") {
                if (total_stmts > 0)
                    report("all instrumented packages", total_hits, total_stmts)
                else
                    print "  nothing instrumented to report on"
                exit
            }

            for (plugin in stmts)
                if (stmts[plugin] > 0)
                    report("cmd/plugins/" plugin, hits[plugin], stmts[plugin])
        }
    ' "$profile"
}

case "$1" in
    -h|--help|help)
        usage
        exit 0
        ;;
esac

dir="${1:-$(pwd)}"
if [ ! -d "$dir" ]; then
    echo "report-coverage.sh: no such directory: $dir" >&2
    exit 1
fi
dir=$(realpath "$dir")

outdir="$dir/coverage-report"
merged="$outdir/merged"
profile="$outdir/coverprofile"
html="$outdir/coverage.html"

# Collect the directories which hold the data of a test case, skipping our own
# output. Both the name of a file and the coverage directory holding it have to
# match, the same way as when the data is discarded: either on its own is too
# little, as the directory to report on can be anywhere, the source tree
# included, where a directory named coverage is just as likely to be a package
# of ours, and a covmeta file outside one is not from a test of ours.
data_dirs=$(find "$dir" -type f -path '*/coverage/covmeta.*' \
                -not -path "$outdir/*" -printf '%h\n' | sort -u | paste -sd, -)

if [ -z "$data_dirs" ]; then
    echo "No coverage data found under $dir."
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
echo "Merging the coverage data of the tests..."
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
summarize plugins "$profile" | LC_ALL=C sort
summarize total "$profile"

echo ""
echo "  per package and function: $GO_CMD tool cover -func=$profile"
echo "  browsable report:         $html"
