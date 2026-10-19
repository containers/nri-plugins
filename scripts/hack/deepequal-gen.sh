#!/usr/bin/env bash
set -euo pipefail

# Run from the repository root.
repo_root=$(pwd)
module_path="github.com/containers/nri-plugins"
tmp_root="$repo_root/$module_path"

# The generator binary to use is taken from the environment. All arguments
# are passed on to the generator verbatim.
generator="${DEEPEQUAL_GEN:-$repo_root/build-aux/bin/deepequal-gen}"

if [[ ! -x "$generator" ]]; then
    echo "error: generator not found or not executable: $generator" >&2
    exit 1
fi

# Generate files using the import-path-based output layout.
"$generator" --output-base="$repo_root" "$@"

# Move generated files into their corresponding source directories.
if [[ -d "$tmp_root" ]]; then
    while IFS= read -r -d '' file; do
        rel_path=${file#"$tmp_root"/}
        dest_dir="$repo_root/$(dirname "$rel_path")"

        mkdir -p "$dest_dir"
        mv -v -f -- "$file" "$dest_dir/"
    done < <(
        find "$tmp_root" -type f -name 'generated.deepequal.go' -print0
    )

    # Remove empty temporary directories, including the module hierarchy.
    find "$repo_root/github.com" -depth -type d -empty -delete 2>/dev/null || true
fi
