#!/bin/bash

SUMMARY="licenses-summary.txt"
CSVFILE="licenses.csv"
ERRFILE="go-license-errors"
IGNORED="--ignore github.com/containers/nri-plugins/pkg/topology"
VERBOSE=""

if [ "$#" != 2 ]; then
    echo "Usage: $0 <input-directory> <output-directory>"
    exit 1
fi

SRC="$1"
OUT="$2"
CSV="$OUT/$CSVFILE"
ERR="$OUT.$ERRFILE"
SUM="$OUT/$SUMMARY"

echo "Extracting/checking licenses for $(basename $OUT)..."
rm -fr $OUT && mkdir -p $(dirname $OUT)
go-licenses check $IGNORED "$SRC" 2>"$ERR" && \
    go-licenses save $IGNORED "$SRC" --save_path "$OUT" 2>"$ERR" && \
        go-licenses report $IGNORED "$SRC" > "$CSV" 2>"$ERR"
status="$?"

echo "go-license warnings/errors:"
cat "$ERR" | sed 's/^/    /g'
if [ "$status" != "0" ]; then
    echo "License check FAILED, status $status"
    exit $status
fi
rm -f "$ERR"

cat "$CSV" | sed 's/^.*,//g' | sort -u > "$SUM"
