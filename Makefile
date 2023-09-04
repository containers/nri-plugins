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

GO_CMD     := go
GO_BUILD   := $(GO_CMD) build
GO_INSTALL := $(GO_CMD) install
GO_TEST    := $(GO_CMD) test
GO_LINT    := golint -set_exit_status
GO_FMT     := gofmt
GO_VET     := $(GO_CMD) vet
GO_DEPS    := $(GO_CMD) list -f '{{ join .Deps "\n" }}'
GO_VERSION ?= 1.20.7

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

DOCKER       := docker
DOCKER_BUILD := $(DOCKER) build

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
	nri-resource-policy-template

BINARIES ?= \
	config-manager

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

clean: clean-plugins clean-binaries

allclean: clean clean-cache

test: test-gopkgs

verify: verify-godeps verify-fmt

#
# build targets
#

build-plugins: $(foreach bin,$(PLUGINS),$(BIN_PATH)/$(bin))

build-plugins-static:
	$(MAKE) STATIC=1 build-plugins

build-binaries: $(foreach bin,$(BINARIES),$(BIN_PATH)/$(bin))

build-binaries-static:
	$(MAKE) STATIC=1 build-binaries

build-images: images

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

clean-images:
	$(Q)echo "Cleaning exported images and deployment files."; \
	rm -f $(IMAGE_PATH)/*

clean-cache:
	$(Q)$(GO_CMD) clean -cache -testcache

#
# plugins build targets
#

$(BIN_PATH)/nri-resource-policy-%: .static.%.$(STATIC)
	$(Q)echo "Building $(STATIC_TYPE)$@ (version $(BUILD_VERSION), build $(BUILD_BUILDID))..."; \
	src="./cmd/plugins/$(patsubst nri-resource-policy-%,%,$(notdir $@))"; \
	mkdir -p $(BIN_PATH); \
	cd "$$src" && $(GO_BUILD) $(BUILD_TAGS) $(LDFLAGS) $(GCFLAGS) -o $@

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

images: $(foreach p,$(PLUGINS),image.$(p)) $(foreach p,$(PLUGINS),image-deployment.$(p)) \
	$(foreach p,$(BINARIES),image.$(p)) $(foreach p,$(BINARIES),image-deployment.$(p)) \

image.nri-resource-policy-% \
image.nri-% \
image.%:
	$(Q)mkdir -p $(IMAGE_PATH); \
	case $@ in \
	    *.nri-resource-policy-*) \
		dir=$(patsubst image.nri-resource-policy-%,cmd/plugins/%,$@); \
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
	    -t $(IMAGE_REPO)$$tag:$(IMAGE_VERSION)

image-deployment.nri-resource-policy-% \
image-deployment.nri-% \
image-deployment.%:
	$(Q)mkdir -p $(IMAGE_PATH); \
	case $@ in \
	    *.nri-resource-policy-*) \
		dir=$(patsubst image-deployment.nri-resource-policy-%,cmd/plugins/%,$@); \
	        ;; \
	    *.nri-*) \
		dir=$(patsubst image-deployment.nri-%,cmd/plugins/%,$@); \
	        ;; \
	    *.*) \
		dir=$(patsubst image-deployment.%,cmd/%,$@); \
	        ;; \
	esac; \
	img=$(patsubst image-deployment.%,%,$@); \
	tag=$$img; \
	NRI_IMAGE_INFO=`$(DOCKER) images --filter=reference=$${tag} --format '{{.ID}} {{.Repository}}:{{.Tag}} (created {{.CreatedSince}}, {{.CreatedAt}})' | head -n 1`; \
	NRI_IMAGE_ID=`awk '{print $$1}' <<< "$${NRI_IMAGE_INFO}"`; \
	NRI_IMAGE_REPOTAG=`awk '{print $$2}' <<< "$${NRI_IMAGE_INFO}"`; \
	NRI_IMAGE_TAR=`realpath "$(IMAGE_PATH)/$${tag}-image-$${NRI_IMAGE_ID}.tar"`; \
	$(DOCKER) image save "$${NRI_IMAGE_REPOTAG}" > "$${NRI_IMAGE_TAR}"; \
	in="`pwd`/$$dir/$${tag}-deployment.yaml.in"; \
	out="$(IMAGE_PATH)/$${tag}-deployment.yaml"; \
	[ ! -f "$$in" ] && exit 0 || :; \
	sed -e "s|IMAGE_PLACEHOLDER|$${NRI_IMAGE_REPOTAG}|g" \
            -e 's/imagePullPolicy: Always/imagePullPolicy: Never/g' \
            < "$$in" > "$$out"; \
	in="`pwd`/test/e2e/files/$${tag}-deployment.yaml.in"; \
	out="$(IMAGE_PATH)/$${tag}-deployment-e2e.yaml"; \
	sed -e "s|IMAGE_PLACEHOLDER|$${NRI_IMAGE_REPOTAG}|g" \
            -e 's/imagePullPolicy: Always/imagePullPolicy: Never/g' \
            < "$$in" > "$$out"

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

report-licenses:
	$(Q)mkdir -p $(LICENSE_PATH) && \
	for cmd in $(IMAGE_DIRS); do \
	    LICENSE_PKGS="$$LICENSE_PKGS ./cmd/$$cmd"; \
	done && \
	go-licenses report $$LICENSE_PKGS \
	        --ignore github.com/containers/nri-plugins \
	        > $(LICENSE_PATH)/licenses.csv && \
	echo See $(LICENSE_PATH)/licenses.csv for license information

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
