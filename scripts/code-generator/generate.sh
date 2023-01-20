#!/bin/bash -e
set -o pipefail

# Default path for code-generator repo
K8S_CODE_GENERATOR=${K8S_CODE_GENERATOR:-../code-generator}

#go mod vendor

go generate ./cmd/... ./pkg/...

#rm -rf vendor/

controller-gen object crd output:crd:stdout paths=./pkg/apis/... > build/deployment/nri-resmgr-api-crds.yaml

rm -rf github.com

${K8S_CODE_GENERATOR}/generate-groups.sh all \
    github.com/intel/nri-resmgr/pkg/apis/resmgr/generated \
    github.com/intel/nri-resmgr/pkg/apis \
    "resmgr:v1alpha1" --output-base=. \
    --go-header-file scripts/code-generator/boilerplate.go.txt

rm -rf pkg/apis/resmgr/generated

mv github.com/intel/nri-resmgr/pkg/apis/resmgr/generated pkg/apis/resmgr

rm -rf github.com
