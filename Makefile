#   Copyright 2022 Intel Corporation. All Rights Reserved.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

# Kubernetes version we pull in as modules and our external API versions.
KUBERNETES_VERSION := $(shell grep 'k8s.io/kubernetes ' go.mod | sed 's/^.* //')
RESMGR_API_VERSION := $(shell ls pkg/apis/resmgr | grep '^v[0-9]*')

# Directories (in cmd) with go code we'll want to create Docker images from.
IMAGE_DIRS  = $(shell find cmd -name Dockerfile | sed 's:cmd/::g;s:/.*::g' | uniq)
IMAGE_VERSION  := $(shell git describe --dirty 2> /dev/null || echo unknown)
ifdef IMAGE_REPO
    override IMAGE_REPO := $(IMAGE_REPO)/
endif

# Determine binary version and buildid, and versions for rpm, deb, and tar packages.
BUILD_VERSION := $(shell scripts/build/get-buildid --version --shell=no)
BUILD_BUILDID := $(shell scripts/build/get-buildid --buildid --shell=no)
RPM_VERSION   := $(shell scripts/build/get-buildid --rpm --shell=no)
DEB_VERSION   := $(shell scripts/build/get-buildid --deb --shell=no)
TAR_VERSION   := $(shell scripts/build/get-buildid --tar --shell=no)

CONTAINER_RUN_CMD ?= docker run
IMAGE_BUILD_CMD ?= docker build
IMAGE_BUILD_EXTRA_OPTS ?=
BUILDER_IMAGE ?= golang:1.19-bullseye

# Protoc compiler and protobuf definitions we might need to recompile.
PROTO_SOURCES = $(shell find . -name '*.proto' | grep -v /vendor/)
PROTO_GOFILES = $(patsubst %.proto,%.pb.go,$(PROTO_SOURCES))
PROTO_INCLUDE = -I$(PWD):/usr/local/include:/usr/include
PROTO_OPTIONS = --proto_path=. $(PROTO_INCLUDE) \
    --go_opt=paths=source_relative --go_out=. \
    --go-ttrpc_opt=paths=source_relative --go-ttrpc_out=.
PROTO_COMPILE = PATH=$(PATH):$(shell go env GOPATH)/bin; protoc $(PROTO_OPTIONS)
PROTOCODE := $(patsubst %.proto,%.pb.go,$(PROTO_SOURCES))

# List of visualizer collateral files to go generate.
UI_ASSETS := $(shell for i in pkg/visualizer/*; do \
        if [ -d "$$i" -a -e "$$i/assets_generate.go" ]; then \
            echo $$i/assets_gendata.go; \
        fi; \
    done)

# Right now we don't depend on libexec/%.o on purpose so make sure the file
# is always up-to-date when elf/avx512.c is changed.
GEN_TARGETS := pkg/avx/programbytes_gendata.go $(PROTOCODE)

GO_CMD     := go
GO_BUILD   := $(GO_CMD) build
GO_GEN     := $(GO_CMD) generate -x
GO_INSTALL := $(GO_CMD) install
GO_TEST    := $(GO_CMD) test
GO_LINT    := golint -set_exit_status
GO_FMT     := gofmt
GO_VET     := $(GO_CMD) vet

GO_MODULES := $(shell $(GO_CMD) list ./...)

GOLANG_CILINT := golangci-lint
GINKGO        := ginkgo

BUILD_PATH    := $(shell pwd)/build
BIN_PATH      := $(BUILD_PATH)/bin
COVERAGE_PATH := $(BUILD_PATH)/coverage
IMAGE_PATH    := $(BUILD_PATH)/images

DOCKER := docker

# Extra options to pass to docker (for instance --network host).
DOCKER_OPTIONS =

# Set this to empty to prevent 'docker build' from trying to pull all image refs.
DOCKER_PULL := --pull

PLUGINS := \
	$(BIN_PATH)/nri-resmgr-topology-aware


ifneq ($(V),1)
  Q := @
endif

# Git (tagged) version and revisions we'll use to linker-tag our binaries with.
RANDOM_ID := "$(shell head -c20 /dev/urandom | od -An -tx1 | tr -d ' \n')"

ifdef STATIC
    STATIC_LDFLAGS:=-extldflags=-static
    BUILD_TAGS:=-tags osusergo,netgo
endif

LDFLAGS    = \
    -ldflags "$(STATIC_LDFLAGS) -X=github.com/intel/nri-resmgr/pkg/version.Version=$(BUILD_VERSION) \
             -X=github.com/intel/nri-resmgr/pkg/version.Build=$(BUILD_BUILDID) \
             -B 0x$(RANDOM_ID)"

#
# top-level targets
#

all: build

build: build-proto build-plugins build-check

build-static:
	$(MAKE) STATIC=1 build

clean: clean-plugins

clean-gen:
	$(Q)rm -f $(GEN_TARGETS)

allclean: clean clean-cache

test: test-gopkgs

#
# build targets
#

build-proto: $(PROTO_GOFILES)

build-plugins: $(PLUGINS)

build-check:
	$(Q)$(GO_BUILD) -v $(GO_MODULES)

#
# clean targets
#

clean-plugins:
	$(Q)rm -f $(PLUGINS)

clean-cache:
	$(Q)$(GO_CMD) clean -cache -testcache

#
# plugins build targets
#

$(BIN_PATH)/%: .static.%.$(STATIC)
	$(Q)src=./cmd/$(patsubst nri-resmgr-%,%,$(notdir $@)); bin=$(notdir $@); \
	echo "Building $$([ -n "$(STATIC)" ] && echo 'static ')$@ (version $(BUILD_VERSION), build $(BUILD_BUILDID))..."; \
	mkdir -p $(BIN_PATH) && \
	$(GO_BUILD) $(BUILD_TAGS) $(LDFLAGS) $(GCFLAGS) -o $(BIN_PATH)/$$bin $$src

$(BIN_PATH)/nri-resmgr-topology-aware: $(wildcard cmd/topology-aware/*.go) $(UI_ASSETS) $(GEN_TARGETS) \
    $(shell for dir in \
                  $(shell go list -f '{{ join .Deps  "\n"}}' ./cmd/topology-aware/... | \
                          grep nri-resmgr/pkg/ | \
                          sed 's#github.com/intel/nri-resmgr/##g'); do \
                find $$dir -name \*.go; \
            done | sort | uniq)

.static.%.$(STATIC):
	$(Q)if [ ! -f "$@" ]; then \
	    touch "$@"; \
	fi; \
	old="$@"; old="$${old%.*}"; \
        if [ -n "$(STATIC)" ]; then \
	    rm -f "$$old."; \
	else \
	    rm -f "$$old.1"; \
	fi

.PRECIOUS: $(foreach dir,$(BUILD_DIRS),.static.$(dir).1 .static.$(dir).)

#
# test targets
#

test-gopkgs: ginkgo-tests

ginkgo-tests:
	$(Q)$(GINKGO) run \
	    --race \
	    --trace \
	    --cover \
	    --covermode atomic \
	    --output-dir $(COVERAGE_PATH) \
	    --junit-report junit.xml \
	    --coverprofile coverprofile \
	    --succinct \
	    -r .; \
	$(GO_CMD) tool cover -html=$(COVERAGE_PATH)/coverprofile -o $(COVERAGE_PATH)/coverage.html

codecov: SHELL := $(shell which bash)
codecov:
	bash <(curl -s https://codecov.io/bash) -f $(COVERAGE_PATH)/coverprofile

#
# other validation targets
#

fmt format:
	$(Q)$(GO_FMT) -s -d -e .

lint:
	$(Q)$(GO_LINT) -set_exit_status ./...

vet:
	$(Q)$(GO_VET) ./...

golangci-lint:
	$(Q)$(GOLANG_CILINT) run

#
# proto generation targets
#

%.pb.go: %.proto
	$(Q)echo "Generating $@..."; \
	$(PROTO_COMPILE) $<

#
# API generation
#

.generator.image.stamp: Dockerfile_generator
	$(IMAGE_BUILD_CMD) \
	    --build-arg BUILDER_IMAGE=$(BUILDER_IMAGE) \
	    -t nri-resmgr-generator \
	    -f Dockerfile_generator .

generate: .generator.image.stamp
	mkdir -p $(BUILD_PATH)/deployment && \
	$(CONTAINER_RUN_CMD) --rm \
	    -v "`go env GOMODCACHE`:/go/pkg/mod" \
	    -v "`go env GOCACHE`:/.cache" \
	    -v "`pwd`:/go/nri-resmgr" \
	    --user=`id -u`:`id -g`\
	    nri-resmgr-generator \
	    ./scripts/code-generator/generate.sh

# unconditionally generate all apis
generate-apis: generate

# unconditionally generate (external) resmgr api
generate-resmgr-api:
	$(Q)$(call generate-api,resmgr,$(RESMGR_API_VERSION))

# automatic update of generated code for resource-manager external api
pkg/apis/resmgr/$(RESMGR_API_VERSION)/zz_generated.deepcopy.go: \
	pkg/apis/resmgr/$(RESMGR_API_VERSION)/types.go
	$(Q)$(call generate-apis)

# macro to generate code for api $(1), version $(2)
generate-api = \
	echo "Generating '$(1)' api, version $(2)..." && \
	    KUBERNETES_VERSION=$(KUBERNETES_VERSION) \
	    ./scripts/code-generator/generate-groups.sh all \
	        github.com/intel/nri-resmgr/pkg/apis/$(1)/generated \
	        github.com/intel/nri-resmgr/pkg/apis $(1):$(2) \
	        --output-base $(shell pwd)/generate && \
	    cp -r generate/github.com/intel/nri-resmgr/pkg/apis/$(1) pkg/apis && \
	        rm -fr generate/github.com/intel/nri-resmgr/pkg/apis/$(1)


#
# targets for installing dependencies
#

install-protoc install-protobuf:
	$(Q)./scripts/install-protobuf && \

install-ttrpc-plugin:
	$(Q)$(GO_INSTALL) github.com/containerd/ttrpc/cmd/protoc-gen-go-ttrpc@74421d10189e8c118870d294c9f7f62db2d33ec1

install-protoc-dependencies:
	$(Q)$(GO_INSTALL) google.golang.org/protobuf/cmd/protoc-gen-go@v1.28.0

install-ginkgo:
	$(Q)$(GO_INSTALL) -mod=mod github.com/onsi/ginkgo/v2/ginkgo

images: $(foreach dir,$(IMAGE_DIRS),image-$(dir)) \
	$(foreach dir,$(IMAGE_DIRS),image-deployment-$(dir))

image-deployment-%:
	$(Q)mkdir -p $(IMAGE_PATH); \
	img=$(patsubst image-deployment-%,%,$@); tag=nri-resmgr-$$img; \
	NRI_IMAGE_INFO=`$(DOCKER) images --filter=reference=$${tag} --format '{{.ID}} {{.Repository}}:{{.Tag}} (created {{.CreatedSince}}, {{.CreatedAt}})' | head -n 1`; \
	NRI_IMAGE_ID=`awk '{print $$1}' <<< "$${NRI_IMAGE_INFO}"`; \
	NRI_IMAGE_REPOTAG=`awk '{print $$2}' <<< "$${NRI_IMAGE_INFO}"`; \
	NRI_IMAGE_TAR=`realpath "$(IMAGE_PATH)/$${tag}-image-$${NRI_IMAGE_ID}.tar"`; \
	$(DOCKER) image save "$${NRI_IMAGE_REPOTAG}" > "$${NRI_IMAGE_TAR}"; \
	sed -e "s|IMAGE_PLACEHOLDER|$${NRI_IMAGE_REPOTAG}|g" \
            -e 's|^\(\s*\)tolerations:$$|\1tolerations:\n\1  - {"key": "cmk", "operator": "Equal", "value": "true", "effect": "NoSchedule"}|g' \
            -e 's/imagePullPolicy: Always/imagePullPolicy: Never/g' \
            < "`pwd`/cmd/$${img}/$${tag}-deployment.yaml.in" \
            > "$(IMAGE_PATH)/$${tag}-deployment.yaml"

image-%:
	$(Q)mkdir -p $(IMAGE_PATH); \
	bin=$(patsubst image-%,%,$@); tag=nri-resmgr-$$bin; \
	    go_version=`$(GO_CMD) list -m -f '{{.GoVersion}}'`; \
	    $(DOCKER) build . -f "cmd/$$bin/Dockerfile" \
	    --build-arg GO_VERSION=$${go_version} \
	    -t $(IMAGE_REPO)$$tag:$(IMAGE_VERSION)

#
# rules to run go generators
#
clean-ui-assets:
	$(Q)echo "Cleaning up generated UI assets..."; \
	for i in $(UI_ASSETS); do \
	    echo "  - $$i"; \
	    rm -f $$i; \
	done

%_gendata.go::
	$(Q)echo "Generating $@..."; \
	cd $(dir $@) && \
	    $(GO_GEN) || exit 1 && \
	cd - > /dev/null

pkg/sysfs/sst_types%.go: pkg/sysfs/_sst_types%.go pkg/sysfs/gen_sst_types.sh
	$(Q)cd $(@D) && \
	    KERNEL_SRC_DIR=$(KERNEL_SRC_DIR) $(GO_GEN)

