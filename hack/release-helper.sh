#!/bin/bash -e
set -o pipefail

this=`basename $0`

usage () {
cat << EOF
Usage: $this VERSION

Example:

  $this v0.2.0
EOF
}

# Check args
if [ $# -ne 1 ]; then
    usage
    exit 1
fi

version=$1
shift 1

if ! [[ $version =~ ^v[0-9]+\.[0-9]+\..+$ ]]; then
    echo -e "ERROR: invalid VERSION '$version'"
    exit 1
fi

# Patch Kustomize
echo "Patching kustomize files"
find deployment/base deployment/overlays -name '*.yaml' | xargs -I '{}' \
    sed -E -e s",newTag:.+$,newTag: $version," \
           -e s",imagePullPolicy:.+$,imagePullPolicy: IfNotPresent," \
           -i '{}'

# Patch Helm charts
echo "Patching Helm charts"
find deployment/helm -name Chart.yaml | xargs -I '{}' \
    sed -e s"/appVersion:.*/appVersion: $version/" -i '{}'
find deployment/helm -name values.yaml | xargs -I '{}' \
    sed -e s"/pullPolicy:.*/pullPolicy: IfNotPresent/" -i '{}'

