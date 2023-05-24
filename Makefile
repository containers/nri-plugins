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

SHELL := /bin/bash
# Kubernetes version we pull in as modules and our external API versions.
KUBERNETES_VERSION := $(shell grep 'k8s.io/kubernetes ' go.mod | sed 's/^.* //')

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
GOLICENSES_VERSION  ?= v1.6.0

CONTAINER_RUN_CMD ?= docker run
IMAGE_BUILD_CMD ?= docker build
IMAGE_BUILD_EXTRA_OPTS ?=
BUILDER_IMAGE ?= golang:1.19-bullseye

GO_CMD     := go
GO_BUILD   := $(GO_CMD) build
GO_GEN     := $(GO_CMD) generate -x
GO_INSTALL := $(GO_CMD) install
GO_TEST    := $(GO_CMD) test
GO_LINT    := golint -set_exit_status
GO_FMT     := gofmt
GO_VET     := $(GO_CMD) vet
GO_DEPS    := $(GO_CMD) list -f '{{ join .Deps "\n" }}'

GO_MODULES := $(shell $(GO_CMD) list ./...)
GO_SUBPKGS := $(shell find ./pkg -name go.mod | sed 's:/go.mod::g' | grep -v testdata)

GOLANG_CILINT := golangci-lint
GINKGO        := ginkgo
TEST_SETUP    := test-setup.sh
TEST_CLEANUP  := test-cleanup.sh

BUILD_PATH    := $(shell pwd)/build
BIN_PATH      := $(BUILD_PATH)/bin
COVERAGE_PATH := $(BUILD_PATH)/coverage
IMAGE_PATH    := $(BUILD_PATH)/images
LICENSE_PATH  := $(BUILD_PATH)/licenses

DOCKER := docker

# Extra options to pass to docker (for instance --network host).
DOCKER_OPTIONS =

# Set this to empty to prevent 'docker build' from trying to pull all image refs.
DOCKER_PULL := --pull

PLUGINS := \
	nri-resource-policy-topology-aware \
	nri-resource-policy-balloons \
	nri-resource-policy-template


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
    -ldflags "$(STATIC_LDFLAGS) -X=github.com/containers/nri-plugins/pkg/version.Version=$(BUILD_VERSION) \
             -X=github.com/containers/nri-plugins/pkg/version.Build=$(BUILD_BUILDID) \
             -B 0x$(RANDOM_ID)"

#
# top-level targets
#

all: build

build: build-plugins build-check

build-static:
	$(MAKE) STATIC=1 build

clean: clean-plugins

allclean: clean clean-cache

test: test-gopkgs

verify: verify-godeps verify-fmt

#
# build targets
#

build-plugins: $(foreach bin,$(PLUGINS),$(BIN_PATH)/$(bin))

build-plugins-static:
	$(MAKE) STATIC=1 build-plugins

build-check:
	$(Q)$(GO_BUILD) -v $(GO_MODULES)

#
# clean targets
#

clean-plugins:
	$(Q)echo "Cleaning $(PLUGINS)"; \
	for i in $(PLUGINS); do \
		rm -f $(BIN_PATH)/$$i; \
	done

clean-cache:
	$(Q)$(GO_CMD) clean -cache -testcache

#
# plugins build targets
#

$(BIN_PATH)/%: .static.%.$(STATIC)
	$(Q)src=./cmd/$(patsubst nri-resource-policy-%,%,$(notdir $@)); bin=$(notdir $@); \
	echo "Building $$([ -n "$(STATIC)" ] && echo 'static ')$@ (version $(BUILD_VERSION), build $(BUILD_BUILDID))..."; \
	mkdir -p $(BIN_PATH) && \
	$(GO_BUILD) $(BUILD_TAGS) $(LDFLAGS) $(GCFLAGS) -o $(BIN_PATH)/$$bin $$src

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
# plugin build dependencies
#

$(BIN_PATH)/nri-resource-policy-topology-aware: \
    $(shell for f in cmd/topology-aware/*.go; do echo $$f; done; \
            for dir in $(shell $(GO_DEPS) ./cmd/topology-aware/... | \
                          grep -E '(/nri-plugins/)|(cmd/topology-aware/)' | \
                          sed 's#github.com/containers/nri-plugins/##g'); do \
                find $$dir -name \*.go; \
            done | sort | uniq)

$(BIN_PATH)/nri-resource-policy-balloons: \
    $(shell for f in cmd/balloons/*.go; do echo $$f; done; \
                for dir in $(shell $(GO_DEPS) ./cmd/balloons/... | \
                          grep -E '(/nri-plugins/)|(cmd/balloons/)' | \
                          sed 's#github.com/containers/nri-plugins/##g'); do \
                find $$dir -name \*.go; \
            done | sort | uniq)

$(BIN_PATH)/nri-resource-policy-template: \
    $(shell for f in cmd/template/*.go; do echo $$f; done; \
                for dir in $(shell $(GO_DEPS) ./cmd/template/... | \
                          grep -E '(/nri-plugins/)|(cmd/template/)' | \
                          sed 's#github.com/containers/nri-plugins/##g'); do \
                find $$dir -name \*.go; \
            done | sort | uniq)

#
# test targets
#

test-gopkgs: ginkgo-test-setup ginkgo-tests ginkgo-subpkgs-tests ginkgo-test-cleanup

ginkgo-test-setup:
	$(Q)for i in $$(find . -name $(TEST_SETUP)); do \
	    echo "+ Running test setup $$i..."; \
	    (cd $${i%/*}; \
	        if [ -x "$(TEST_SETUP)" ]; then \
	            ./$(TEST_SETUP); \
	        fi); \
	done

ginkgo-test-cleanup:
	$(Q)for i in $$(find . -name $(TEST_CLEANUP)); do \
	    echo "- Running test cleanup $$i..."; \
	    (cd $${i%/*}; \
	        if [ -x "$(TEST_CLEANUP)" ]; then \
	            ./$(TEST_CLEANUP); \
	        fi); \
	done

ginkgo-tests:
	$(Q)$(GINKGO) run \
	    --race \
	    --trace \
	    --cover \
	    --covermode atomic \
	    --output-dir $(COVERAGE_PATH) \
	    --junit-report junit.xml \
	    --coverprofile $(COVERAGE_PATH)/coverprofile \
	    --keep-separate-coverprofiles \
	    --succinct \
            --skip-package $$(echo $(GO_SUBPKGS) | tr -s '\t ' ',') \
	    -r .; \
	$(GO_CMD) tool cover -html=$(COVERAGE_PATH)/coverprofile -o $(COVERAGE_PATH)/coverage.html

ginkgo-subpkgs-tests: # TODO(klihub): coverage ?
	$(Q)for i in $(GO_SUBPKGS); do \
	    (cd $$i; \
	        $(GINKGO) run \
	            --race \
	            --trace \
	            --succinct \
	            -r . || exit 1); \
	done

codecov: SHELL := $(shell which bash)
codecov:
	bash <(curl -s https://codecov.io/bash) -f $(COVERAGE_PATH)/coverprofile

#
# other validation targets
#

fmt format:
	$(Q)$(GO_FMT) -s -d -e .

reformat:
	$(Q)$(GO_FMT) -s -d -w $$(git ls-files '*.go')

lint:
	$(Q)$(GO_LINT) -set_exit_status ./...

vet:
	$(Q)$(GO_VET) ./...

golangci-lint:
	$(Q)$(GOLANG_CILINT) run

verify-godeps:
	$(Q) $(GO_CMD) mod tidy && git diff --quiet; ec="$$?"; \
	if [ "$$ec" != "0" ]; then \
	    echo "ERROR: go mod dependencies are not up-to-date."; \
	    echo "ERROR:"; \
	    git --no-pager diff go.mod go.sum | sed 's/^/ERROR: /g'; \
	    echo "ERROR:"; \
	    echo "ERROR: please run 'go mod tidy' and commit these changes."; \
	    exit "$$ec"; \
	fi; \
	$(GO_CMD) mod verify

verify-fmt:
	$(Q)report=`$(GO_FMT) -s -d -e $$(git ls-files '*.go')`; \
	if [ -n "$$report" ]; then \
	    echo "ERROR: go formatting errors"; \
	    echo "$$report" | sed 's/^/ERROR: /g'; \
	    echo "ERROR: please run make reformat or go fmt by hand and commit any changes."; \
	    exit 1; \
	fi

#
# targets for installing dependencies
#

install-ginkgo:
	$(Q)$(GO_INSTALL) -mod=mod github.com/onsi/ginkgo/v2/ginkgo

images: $(foreach dir,$(IMAGE_DIRS),image-$(dir)) \
	$(foreach dir,$(IMAGE_DIRS),image-deployment-$(dir))

image-deployment-%:
	$(Q)mkdir -p $(IMAGE_PATH); \
	img=$(patsubst image-deployment-%,%,$@); tag=nri-resource-policy-$$img; \
	NRI_IMAGE_INFO=`$(DOCKER) images --filter=reference=$${tag} --format '{{.ID}} {{.Repository}}:{{.Tag}} (created {{.CreatedSince}}, {{.CreatedAt}})' | head -n 1`; \
	NRI_IMAGE_ID=`awk '{print $$1}' <<< "$${NRI_IMAGE_INFO}"`; \
	NRI_IMAGE_REPOTAG=`awk '{print $$2}' <<< "$${NRI_IMAGE_INFO}"`; \
	NRI_IMAGE_TAR=`realpath "$(IMAGE_PATH)/$${tag}-image-$${NRI_IMAGE_ID}.tar"`; \
	$(DOCKER) image save "$${NRI_IMAGE_REPOTAG}" > "$${NRI_IMAGE_TAR}"; \
	sed -e "s|IMAGE_PLACEHOLDER|$${NRI_IMAGE_REPOTAG}|g" \
            -e 's|^\(\s*\)tolerations:$$|\1tolerations:\n\1  - {"key": "cmk", "operator": "Equal", "value": "true", "effect": "NoSchedule"}|g' \
            -e 's/imagePullPolicy: Always/imagePullPolicy: Never/g' \
            < "`pwd`/cmd/$${img}/$${tag}-deployment.yaml.in" \
            > "$(IMAGE_PATH)/$${tag}-deployment.yaml"; \
	sed -e "s|IMAGE_PLACEHOLDER|$${NRI_IMAGE_REPOTAG}|g" \
            -e 's|^\(\s*\)tolerations:$$|\1tolerations:\n\1  - {"key": "cmk", "operator": "Equal", "value": "true", "effect": "NoSchedule"}|g' \
            -e 's/imagePullPolicy: Always/imagePullPolicy: Never/g' \
            < "`pwd`/test/e2e/files/$${tag}-deployment.yaml.in" \
            > "$(IMAGE_PATH)/$${tag}-deployment-e2e.yaml"

image-%:
	$(Q)mkdir -p $(IMAGE_PATH); \
	bin=$(patsubst image-%,%,$@); tag=nri-resource-policy-$$bin; \
	    go_version=`$(GO_CMD) list -m -f '{{.GoVersion}}'`; \
	    $(DOCKER) build . -f "cmd/$$bin/Dockerfile" \
	    --build-arg GO_VERSION=$${go_version} \
	    -t $(IMAGE_REPO)$$tag:$(IMAGE_VERSION)

pkg/sysfs/sst_types%.go: pkg/sysfs/_sst_types%.go pkg/sysfs/gen_sst_types.sh
	$(Q)cd $(@D) && \
	    KERNEL_SRC_DIR=$(KERNEL_SRC_DIR) $(GO_GEN)

report-licenses:
	$(Q)mkdir -p $(LICENSE_PATH) && \
	for cmd in $(IMAGE_DIRS); do \
	    LICENSE_PKGS="$$LICENSE_PKGS ./cmd/$$cmd"; \
	done && \
	go-licenses report $$LICENSE_PKGS \
	        --ignore github.com/containers/nri-plugins \
	        > $(LICENSE_PATH)/licenses.csv && \
	echo See $(LICENSE_PATH)/licenses.csv for license information
