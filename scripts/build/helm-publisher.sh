#!/bin/bash
set -e

index_file="index.yaml"
browser_download_url="$@"

git checkout gh-pages

# Verify if the release assets include Helm chart packages. If they do,
# we can proceed with updating the index.yaml file, otherwise throw an error.
charts_urls=$(echo "$browser_download_url" | grep '.*helm-chart-.*.tgz')

# Check if Helm release assets were found
if [ -n "$charts_urls" ]; then
    # Loop through the URLs
    for chart in $charts_urls; do
        # Check if the URL path exists in index.yaml
        # and if not, update the index.yaml accordingly
        if ! grep -q "$chart" "$index_file"; then
            wget "$chart"
            base_url=$(dirname "$chart")
            if ! helm repo index . --url "$base_url" --merge "$index_file"; then
                echo "Failed to update "$index_file" for: $base_url"
            fi
            rm *chart*.tgz
        fi
    done
else
    echo "No Helm packages were found on this release"
    exit 1
fi

# Create a new commit
release=$(basename "$base_url")
commit_msg="Update Helm index for release $release"

echo "Committing changes..."

git config user.name "Github Actions"
git config user.email "no-reply@github.com"
git add index.yaml
git commit -m "$commit_msg"

echo "gh-pages branch successfully updated"
