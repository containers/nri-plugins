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

container_image=ghcr.io/containers/nri-plugins/nri-plugins-operator:$version

if ! [[ $version =~ ^v[0-9]+\.[0-9]+\..+$ ]]; then
    echo -e "ERROR: invalid VERSION '$version'"
    exit 1
fi

# Patch Helm charts
echo "Patching Helm charts"
find deployment/helm -name Chart.yaml | xargs -I '{}' \
    sed -e s"/appVersion:.*/appVersion: $version/" -i '{}'
find deployment/helm -name values.yaml | xargs -I '{}' \
    sed -e s"/pullPolicy:.*/pullPolicy: IfNotPresent/" -i '{}'

# Patch deployment templates
echo Patching kustomize templates to use $container_image
find deployment/operator/config/manifests/bases -name nri-plugins-operator.clusterserviceversion.yaml | xargs -I '{}' \
    sed -E -e s",^([[:space:]]+)containerImage:.+$,\1containerImage: $container_image," -i '{}'
find deployment/operator/config/manager -name kustomization.yaml | xargs -I '{}' \
    sed -E -e s",^([[:space:]]+)newTag:.+$,\1newTag: $version," -i '{}'
