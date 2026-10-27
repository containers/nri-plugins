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

IMAGE_VERSION  ?= $(shell git describe --tags --dirty 2> /dev/null || echo v0.0.0-unknown)
ifdef IMAGE_REPO
    override IMAGE_REPO := $(IMAGE_REPO)/
endif

export IMAGE_VERSION
export IMAGE_PATH

# Determine binary version and buildid.
BUILD_VERSION ?= $(shell scripts/build/get-buildid --version --shell=no)
BUILD_BUILDID ?= $(shell scripts/build/get-buildid --buildid --shell=no)
BUILD_FLAGS   ?=

GO_CMD     := go
GO_BUILD    = $(GO_CMD) build $(BUILD_FLAGS)
GO_INSTALL := $(GO_CMD) install
GO_TEST    := $(GO_CMD) test
GO_FMT     := gofmt
GO_VET     := $(GO_CMD) vet
GO_DEPS    := $(GO_CMD) list -f '{{ join .Deps "\n" }}'
GO_VERSION ?= 1.26.0

GO_MODULES := $(shell $(GO_CMD) list ./...)

GO_TEST_PKGS ?= $(shell find . -name \*_test.go -exec dirname {} \; | \
                   sort -u | while read dir; do \
                       [ ! -e $$dir/go.mod ] && echo $$dir; \
                   done)

GO_TEST_MODS ?= $(shell find . -name \*_test.go -exec dirname {} \; | \
                   sort -u | while read dir; do \
                       [ -e $$dir/go.mod ] && echo $$dir; \
                   done)

GOLANG_CILINT := golangci-lint
TEST_SETUP    := test-setup.sh
TEST_CLEANUP  := test-cleanup.sh
TEST_PKGS     := .

TOP_DIR       := $(dir $(realpath $(word 1,$(MAKEFILE_LIST))))
BUILD_PATH    := $(TOP_DIR)/build
BIN_PATH      := $(BUILD_PATH)/bin
COVERAGE_PATH := $(BUILD_PATH)/coverage
IMAGE_PATH    := $(BUILD_PATH)/images
LICENSE_PATH  := $(BUILD_PATH)/licenses

# Pin license extraction tool version to :main for now.
GO_LICENSES_IMAGE   ?= ghcr.io/klihub/go-and-maybe-cache-licenses:main

# The one definition of which licenses are acceptable. Both the local check
# and CI read it from here, so the two cannot come to disagree.
LICENSE_ALLOWED     ?= Apache-2.0 BSD-2-Clause BSD-3-Clause ISC MIT
export LICENSE_ALLOWED
GO_LICENSES_CACHE   ?= $(BUILD_PATH)/cache/licenses
SKIP_LICENSES       ?= 0

export GO_LICENSES_IMAGE
export GO_LICENSES_CACHE

E2E_DIR       := $(TOP_DIR)/test/e2e
E2E_RUN       := k8scri=$${k8scri:-containerd} $(E2E_DIR)/run_tests.sh
E2E_TESTS     ?= $(E2E_DIR)/policies.test-suite
E2E_WORKDIR   ?= $(TOP_DIR)/e2e-results

E2E_REPORT_SRC := ./test/e2e/cmd/e2e-report
E2E_REPORT     := $(BIN_PATH)/e2e-report

DOCKER       := docker
DOCKER_BUILD := $(DOCKER) buildx build --load

MAKEFLAGS += -j$(shell nproc || printf 1)

# Plugins and other binaries we build.
#
# Notes:
#   All plugins have names matching nri-resource-policy-% or nri-%.
#   All plugins are in the directory cmd/plugin/$dir, where $dir is
#   the name of the plugin with the above mentioned prefixes removed.
#   All other binaries are in the directory cmd/$dir, where $dir
#   MUST NOT have an nri-% prefix.
#
PLUGINS ?= \
	nri-resource-policy-topology-aware \
	nri-resource-policy-balloons \
	nri-resource-policy-template \
	nri-memory-policy \
	nri-memory-qos \
	nri-memtierd \
        nri-sgx-epc \
	nri-resctrl-mon

BINARIES ?= \
	config-manager \
	mpolset \
	resource-annotator

OTHER_IMAGE_TARGETS ?= \
	nri-plugins-operator-image \
	nri-plugins-operator-bundle-image

ifneq ($(V),1)
  Q := @
endif

# Git (tagged) version and revisions we'll use to linker-tag our binaries with.
RANDOM_ID := "$(shell head -c20 /dev/urandom | od -An -tx1 | tr -d ' \n')"

ifdef STATIC
    STATIC_LDFLAGS := -extldflags=-static
    BUILD_TAGS     := -tags osusergo,netgo
    STATIC_TYPE    := "static "
endif

LDFLAGS    = \
    -ldflags "$(STATIC_LDFLAGS) -X=github.com/containers/nri-plugins/pkg/version.Version=$(BUILD_VERSION) \
             -X=github.com/containers/nri-plugins/pkg/version.Build=$(BUILD_BUILDID) \
             -B 0x$(RANDOM_ID)"

ifeq ($(DEBUG),1)
    GCFLAGS ?= -gcflags "all=-N -l"
    DOCKER_BUILD_DEBUG := --build-arg DEBUG=1
  ifneq ($(NORACE),1)
    BUILD_FLAGS += -race
  else
    DOCKER_BUILD_DEBUG += --build-arg NORACE=1
  endif
endif

# Packages to instrument for coverage with COVER=1. We instrument only our
# own code. pkg/topology needs to be listed extra as it is a module of its
# own.
COVER_PKGS ?= github.com/containers/nri-plugins/...,github.com/containers/nri-plugins/pkg/topology/...

# COVER=1 builds resource-management plugins with coverage instrumentation,
# to collect runtime coverage data. See test/e2e/README.md for how the e2e
# tests use this. Atomic counters are a must (for coverage.ClearCounters()).
ifeq ($(COVER),1)
    COVER_FLAGS := -cover -covermode=atomic -coverpkg=$(COVER_PKGS)
    COVER_TYPE  := "instrumented "
    DOCKER_BUILD_COVER := --build-arg COVER=1
endif

# Documentation-related variables
SPHINXOPTS    ?= -W
SPHINXBUILD   = sphinx-build
SITE_BUILDDIR ?= build/docs

# Docker base command for working with html documentation.
DOCKER_SITE_BUILDER_IMAGE := nri-plugins-site-builder
DOCKER_SITE_CMD := $(DOCKER) run --rm -v "`pwd`:/docs" --user=`id -u`:`id -g` \
	-p 8081:8081 \
	-e SITE_BUILDDIR=$(SITE_BUILDDIR) -e SPHINXOPTS=$(SPHINXOPTS)

#
# top-level targets
#

all: build

build: build-plugins build-binaries build-check

build-static:
	$(MAKE) STATIC=1 build

clean: clean-plugins clean-binaries clean-e2e-report

allclean: clean clean-cache

test: test-setup test-run-pkgs test-run-mods test-cleanup

verify: verify-godeps verify-fmt verify-generate verify-build verify-docs

#
# build targets
#

build-plugins: $(foreach bin,$(PLUGINS),$(BIN_PATH)/$(bin))

build-plugins-static:
	$(MAKE) STATIC=1 DEBUG=$(DEBUG) NORACE=$(NORACE) COVER=$(COVER) build-plugins

build-binaries: $(foreach bin,$(BINARIES),$(BIN_PATH)/$(bin))

build-binaries-static:
	$(MAKE) STATIC=1 DEBUG=$(DEBUG) NORACE=$(NORACE) build-binaries

build-images: images

e2e-report: $(E2E_REPORT)

$(E2E_REPORT): $(wildcard test/e2e/cmd/e2e-report/*.go)
	$(Q)echo "Building $@..."; \
	mkdir -p $(BIN_PATH) && \
	$(GO_BUILD) -o $@ $(E2E_REPORT_SRC)

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

clean-binaries:
	$(Q)echo "Cleaning $(BINARIES)"; \
	for i in $(BINARIES); do \
		rm -f $(BIN_PATH)/$$i; \
	done

clean-e2e-report:
	$(Q)echo "Cleaning $(notdir $(E2E_REPORT))"; \
	rm -f $(E2E_REPORT)

clean-images:
	$(Q)echo "Cleaning exported images and deployment files."; \
	rm -f $(IMAGE_PATH)/*

clean-licenses:
	$(Q)echo "Cleaning up extracted license information."; \
	rm -fr $(LICENSE_PATH)

clean-cache:
	$(Q)$(GO_CMD) clean -cache -testcache

#
# plugins build targets
#

$(BIN_PATH)/nri-resource-policy-%: .static.%.$(STATIC)
	$(Q)echo "Building $(COVER_TYPE)$(STATIC_TYPE)$@ (version $(BUILD_VERSION), build $(BUILD_BUILDID))..."; \
	src="./cmd/plugins/$(patsubst nri-resource-policy-%,%,$(notdir $@))"; \
	mkdir -p $(BIN_PATH); \
	cd "$$src" && $(GO_BUILD) $(BUILD_TAGS) $(LDFLAGS) $(GCFLAGS) $(COVER_FLAGS) -o $@

$(BIN_PATH)/nri-%: .static.%.$(STATIC)
	$(Q)echo "Building $(STATIC_TYPE)$@ (version $(BUILD_VERSION), build $(BUILD_BUILDID))..."; \
	src="./cmd/plugins/$(patsubst nri-%,%,$(notdir $@))"; \
	mkdir -p $(BIN_PATH) && \
	cd "$$src" && $(GO_BUILD) $(BUILD_TAGS) $(LDFLAGS) $(GCFLAGS) -o $@

$(BIN_PATH)/%: .static.%.$(STATIC)
	$(Q)echo "Building $(STATIC_TYPE)$@ (version $(BUILD_VERSION), build $(BUILD_BUILDID))..."; \
	src="./cmd/$(notdir $@)"; \
	mkdir -p $(BIN_PATH) && \
	cd "$$src" && $(GO_BUILD) $(BUILD_TAGS) $(LDFLAGS) $(GCFLAGS) -o $@

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
# Image building test deployment generation targets
#

images: $(foreach p,$(PLUGINS),image.$(p)) \
	$(foreach p,$(BINARIES),image.$(p)) \
	$(OTHER_IMAGE_TARGETS)

image.nri-resource-policy-% \
image.nri-% \
image.%: licenses
	$(Q)mkdir -p $(IMAGE_PATH); \
	cover=""; \
	case $@ in \
	    *.nri-resource-policy-*) \
		dir=$(patsubst image.nri-resource-policy-%,cmd/plugins/%,$@); \
		cover="$(DOCKER_BUILD_COVER)"; \
	        ;; \
	    *.nri-*) \
		dir=$(patsubst image.nri-%,cmd/plugins/%,$@); \
	        ;; \
	    *.*) \
		dir=$(patsubst image.%,cmd/%,$@); \
	        ;; \
	esac; \
	bin=$(patsubst image.%,%,$@); \
	tag=$(patsubst image.%,%,$@); \
	$(DOCKER_BUILD) . -f "$$dir/Dockerfile" \
	    --build-arg GO_VERSION=$(GO_VERSION) \
	    $(DOCKER_BUILD_DEBUG) \
	    $$cover \
	    --build-arg IMAGE_VERSION=$(IMAGE_VERSION) \
	    --build-arg BUILD_VERSION=$(BUILD_VERSION) \
	    --build-arg BUILD_BUILDID=$(BUILD_BUILDID) \
	    --build-arg SKIP_LICENSES=$(SKIP_LICENSES) \
	    -t $(IMAGE_REPO)$$tag:$(IMAGE_VERSION) || exit $$?; \
	NRI_IMAGE_INFO=`$(DOCKER) images --filter=reference=$${tag} --format '{{.ID}} {{.Repository}}:{{.Tag}} (created {{.CreatedSince}}, {{.CreatedAt}})' | head -n 1`; \
	NRI_IMAGE_ID=`awk '{print $$1}' <<< "$${NRI_IMAGE_INFO}"`; \
	NRI_IMAGE_REPOTAG=`awk '{print $$2}' <<< "$${NRI_IMAGE_INFO}"`; \
	NRI_IMAGE_TAR=`realpath "$(IMAGE_PATH)/$${tag}-image-$${NRI_IMAGE_ID}.tar"`; \
	$(DOCKER) image save "$${NRI_IMAGE_REPOTAG}" > "$${NRI_IMAGE_TAR}";

.PHONY: licenses
# Map a shipped binary name to the directory that builds it.
license-src = $(if $(filter nri-resource-policy-%,$(1)),./cmd/plugins/$(patsubst nri-resource-policy-%,%,$(1)),$(if $(filter nri-%,$(1)),./cmd/plugins/$(patsubst nri-%,%,$(1)),./cmd/$(1)))
license-targets = $(foreach b,$(1),$(b)=$(call license-src,$(b)))

# Print the accepted licenses, so CI can ask for them rather than repeating
# the list.
.PHONY: list-allowed-licenses
list-allowed-licenses:
	$(Q)echo "$(LICENSE_ALLOWED)" | tr -s ' ' ','

# Print the license check targets, so CI can ask for them rather than
# repeating the list and letting the two definitions drift apart.
.PHONY: list-license-check-targets
list-license-check-targets:
	$(Q)for t in $(call license-targets,$(PLUGINS) $(BINARIES)); do echo "$$t"; done

licenses:
	$(Q)if [ "$(SKIP_LICENSES)" = "1" ]; then \
	    for b in $(PLUGINS) $(BINARIES); do mkdir -p $(LICENSE_PATH)/$$b; done; \
	    exit 0; \
	fi; \
	./scripts/build/extract-licenses.sh $(LICENSE_PATH) \
	    $(call license-targets,$(PLUGINS) $(BINARIES))

licenses.%:
	$(Q)bin="$(patsubst licenses.%,%,$@)"; \
	if [ "$(SKIP_LICENSES)" = "1" ]; then \
	    mkdir -p "$(LICENSE_PATH)/$$bin"; \
	    exit 0; \
	fi; \
	./scripts/build/extract-licenses.sh $(LICENSE_PATH) \
	    $(call license-targets,$(patsubst licenses.%,%,$@))

nri-plugins-operator-image:
	$(Q)mkdir -p $(IMAGE_PATH); \
	if ! $(MAKE) -C deployment/operator image-save; then \
	    case $(IMAGE_VERSION) in \
	        *-[0-9]*-g[0-9a-f]*) \
		   echo "ERROR: build of $@ failed"; \
	           echo "WARNING: ignoring failure non-release build $(IMAGE_VERSION)"; \
                   ;; \
	        *) \
                   echo "ERROR: $@ build failed"; \
	           exit 1 \
                   ;; \
	    esac; \
	fi

nri-plugins-operator-bundle-image:
	$(Q)mkdir -p $(IMAGE_PATH); \
	if ! $(MAKE) -C deployment/operator bundle-save; then \
	    case $(IMAGE_VERSION) in \
	        *-[0-9]*-g[0-9a-f]*) \
		   echo "ERROR: build of $@ failed"; \
	           echo "WARNING: ignoring failure non-release build $(IMAGE_VERSION)"; \
                   ;; \
	        *) \
                   echo "ERROR: $@ build failed"; \
	           exit 1 \
                   ;; \
	    esac; \
	fi

#
# plugin build dependencies
#

$(BIN_PATH)/nri-resource-policy-topology-aware: \
    $(shell for f in cmd/plugins/topology-aware/*.go; do echo $$f; done; \
            for dir in $(shell $(GO_DEPS) ./cmd/plugins/topology-aware/... | \
                          grep -E '(/nri-plugins/)|(cmd/plugins/topology-aware/)' | \
                          sed 's#github.com/containers/nri-plugins/##g'); do \
                find $$dir -name \*.go; \
            done | sort | uniq)

$(BIN_PATH)/nri-resource-policy-balloons: \
    $(shell for f in cmd/plugins/balloons/*.go; do echo $$f; done; \
                for dir in $(shell $(GO_DEPS) ./cmd/plugins/balloons/... | \
                          grep -E '(/nri-plugins/)|(cmd/plugins/balloons/)' | \
                          sed 's#github.com/containers/nri-plugins/##g'); do \
                find $$dir -name \*.go; \
            done | sort | uniq)

$(BIN_PATH)/nri-resource-policy-template: \
    $(shell for f in cmd/plugins/template/*.go; do echo $$f; done; \
                for dir in $(shell $(GO_DEPS) ./cmd/plugins/template/... | \
                          grep -E '(/nri-plugins/)|(cmd/plugins/template/)' | \
                          sed 's#github.com/containers/nri-plugins/##g'); do \
                find $$dir -name \*.go; \
            done | sort | uniq)

$(BIN_PATH)/resource-annotator: \
    $(shell for f in cmd/resource-annotator/*.go; do echo $$f; done; \
            for dir in $(shell $(GO_DEPS) ./cmd/resource-annotator/... | \
                          grep -E '(/nri-plugins/)|(cmd/resource-annotator/)' | \
                          sed 's#github.com/containers/nri-plugins/##g'); do \
                find $$dir -name \*.go; \
            done | sort | uniq)

#
# test targets
#

.PHONY: test-setup
test-setup:
	$(Q)for i in $$(find $(GO_TEST_PKGS) $(GO_TEST_MODS) -name $(TEST_SETUP) | sort -u); do \
	    echo "+ Running test setup $$i..."; \
	    (cd $${i%/*}; \
	        if [ -x "$(TEST_SETUP)" ]; then \
	            ./$(TEST_SETUP) > /dev/null; \
	        fi); \
	done

.PHONY: test-cleanup
test-cleanup: test-run-pkgs test-run-mods
	$(Q)for i in $$(find $(GO_TEST_PKGS) $(GO_TEST_MODS) -name $(TEST_CLEANUP) | sort -u); do \
	    echo "- Running test cleanup $$i..."; \
	    (cd $${i%/*}; \
	        if [ -x "$(TEST_CLEANUP)" ]; then \
	            ./$(TEST_CLEANUP) > /dev/null; \
	        fi); \
	done

.PHONY: test-run-pkgs
test-run-pkgs: test-setup
	$(Q)$(GO_TEST) -v \
	    --race \
	    --cover \
	    --covermode atomic \
	     $(GO_TEST_PKGS)

.PHONY: test-run-mods
test-run-mods: test-setup
	$(Q)for dir in $(GO_TEST_MODS); do \
	    (cd $$dir; $(GO_TEST) -v \
	    --race \
	    --cover \
	    --covermode atomic \
	    .); \
	done

# The e2e tests collect coverage data from the plugins they exercise and report
# the total at the end of the run, so build the plugins with instrumentation.
e2e-tests:
	$(Q)$(MAKE) COVER=1 build images
	mkdir -p $(shell realpath $(E2E_WORKDIR)) && \
	cd $(shell realpath $(E2E_WORKDIR)) && \
	    $(E2E_RUN) $(realpath $(E2E_TESTS))

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

lint: golangci-lint

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
	$(Q)report=`$(GO_FMT) -s -d -e $$(git ls-files '*.go' | while read f; do grep -q 'Code generated by ' $$f || echo $$f; done)`; \
	if [ -n "$$report" ]; then \
	    echo "ERROR: go formatting errors"; \
	    echo "$$report" | sed 's/^/ERROR: /g'; \
	    echo "ERROR: please run make reformat or go fmt by hand and commit any changes."; \
	    exit 1; \
	fi

verify-generate: generate
	$(Q) git diff --quiet; ec="$$?"; \
	if [ "$$ec" != "0" ]; then \
	    echo "ERROR: generated artifacts are not up-to-date."; \
	    echo "ERROR:"; \
	    git --no-pager diff | sed 's/^/ERROR: /g'; \
	    echo "ERROR:"; \
	    echo "ERROR: please run 'make generate' and commit these changes."; \
	    exit "$$ec"; \
	fi

verify-build: build

verify-docs: site-build clean-html

verify-codespell codespell:
	$(Q) codespell

verify-licenses: licenses
	$(Q)if [ "$(SKIP_LICENSES)" = "1" ]; then \
	    echo "Skipping license verification as requested."; \
	    exit 0; \
	fi; \
	./scripts/build/verify-licenses.sh -v $(LICENSE_PATH)

#
# targets for installing dependencies
#

.PHONY: pull-license-tool
pull-license-tool:
	$(Q)if [ "$(SKIP_LICENSES)" = "1" ]; then \
	    echo "Skipping license tool image pull as requested."; \
	    exit 0; \
	fi; \
	$(DOCKER) pull -q $(GO_LICENSES_IMAGE)

#
# Rules for documentation
#

html: clean-html
	$(Q)BUILD_VERSION=$(BUILD_VERSION) \
		$(SPHINXBUILD) -c docs . "$(SITE_BUILDDIR)" $(SPHINXOPTS)
	cp docs/index.html "$(SITE_BUILDDIR)"
	for d in $$(find docs -name figures -type d); do \
	    mkdir -p $(SITE_BUILDDIR)/$$d && cp $$d/* $(SITE_BUILDDIR)/$$d; \
	done

serve-html: html
	$(Q)cd $(SITE_BUILDDIR) && python3 -m http.server 8081

clean-html:
	rm -rf $(SITE_BUILDDIR)

site-build: .$(DOCKER_SITE_BUILDER_IMAGE).image.stamp
	$(Q)$(DOCKER_SITE_CMD) $(DOCKER_SITE_BUILDER_IMAGE) make html

site-serve: .$(DOCKER_SITE_BUILDER_IMAGE).image.stamp
	$(Q)$(DOCKER_SITE_CMD) -it $(DOCKER_SITE_BUILDER_IMAGE) make serve-html

.$(DOCKER_SITE_BUILDER_IMAGE).image.stamp: docs/Dockerfile docs/requirements.txt
	$(DOCKER_BUILD) -t $(DOCKER_SITE_BUILDER_IMAGE) docs
	touch $@

docs: site-build

#
# CRD et. al code generation
#

CRD_BASE_DIR := config/crd/bases
HELM_TOP_DIR := deployment/helm

# Location to install external generation tool binaries
BUILD_AUX ?= $(shell pwd)/build-aux
LOCALBIN ?= $(BUILD_AUX)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# External generation tool versions
CLIENT_GEN_VERSION := v0.33.4
CONTROLLER_GEN_VERSION := v0.18.0
DEEPEQUAL_GEN_VERSION := latest

# External generation tool binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CLIENT_GEN ?= $(LOCALBIN)/client-gen
DEEPEQUAL_GEN ?= $(LOCALBIN)/deepequal-gen
DEEPEQUAL_GEN_SH ?= ./scripts/hack/deepequal-gen.sh

# Client generation rules
.PHONY: controller-gen
controller-gen:
	@GOBIN=$(LOCALBIN) $(GO_CMD) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: code-generator
code-generator:
	@GOBIN=$(LOCALBIN) $(GO_CMD) install k8s.io/code-generator/cmd/client-gen@$(CLIENT_GEN_VERSION)

.PHONY: deepequal-gen
deepequal-gen:
	@GOBIN=$(LOCALBIN) $(GO_CMD) install github.com/cilium/deepequal-gen@$(DEEPEQUAL_GEN_VERSION)

# Generate RBAC and CRD objects.
.PHONY: manifests
manifests: controller-gen
	$(CONTROLLER_GEN) \
		crd:generateEmbeddedObjectMeta=true output:crd:artifacts:config=$(CRD_BASE_DIR)\
		paths="./pkg/apis/..."
	$(CONTROLLER_GEN) \
		rbac:roleName=manager-role crd paths="./pkg/apis/..." output:crd:artifacts:config=$(CRD_BASE_DIR)

.PHONY: generate-clients
generate-clients: controller-gen code-generator manifests
	$(CONTROLLER_GEN) object:headerFile="scripts/hack/boilerplate.go.txt" paths="./pkg/apis/..."
	./scripts/hack/update_codegen.sh $(GO_CMD) $(BUILD_AUX)

generate-deepequals: deepequal-gen controller-gen code-generator manifests
	$(Q)DEEPEQUAL_GEN=$(DEEPEQUAL_GEN) $(DEEPEQUAL_GEN_SH) ./pkg/apis/...

# golang code generation rules
generate-go:
	$(Q)$(GO_CMD) generate ./...

# rule to update generated CRDs in our helm charts
.PHONY: update-helm-crds

update-helm-crds: generate-clients
	$(Q)for plugin in $(PLUGINS); do \
	    plugin="$${plugin#nri-}"; plugin="$${plugin#resource-policy-}"; \
            helm_dir=$(HELM_TOP_DIR)/$$plugin/crds; \
	    if [ ! -d "$$helm_dir" ]; then \
	        echo "No generated CRD found for $$plugin plugin..."; \
	    else \
	        echo "Updating Helm chart CRDs for $$plugin plugin..."; \
	        cp -v $(CRD_BASE_DIR)/*_$${plugin//-/}*.yaml $$helm_dir | sed 's/^/  /g'; \
	    fi; \
	done

# top level rule to (re)generate everything we need
generate: generate-go generate-clients generate-deepequals update-helm-crds
