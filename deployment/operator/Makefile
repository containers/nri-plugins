SHELL := /bin/bash

# Default image version (tag) and image save path.
IMAGE_VERSION ?= $(shell git describe --tags --dirty 2> /dev/null)
IMAGE_PATH ?= .

# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "candidate,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=candidate,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="candidate,fast,stable")
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
#
# For example, running 'make bundle-build bundle-push catalog-build catalog-push' will build and push both
# ghcr.io/containers/nri-plugins-operator-bundle:$IMAGE_VERSION and ghcr.io/containers/nri-plugins-operator-catalog:$IMAGE_VERSION.
IMAGE_TAG_BASE ?= ghcr.io/containers/nri-plugins/nri-plugins-operator

# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:$(IMAGE_VERSION)

# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= $(shell echo $(IMAGE_VERSION) | sed 's/^v//')
# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)

# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

# Set the Operator SDK version to use. By default, what is installed on the system is used.
# This is useful for CI or a project to utilize a specific version of the operator-sdk toolkit.
OPERATOR_SDK_VERSION ?= v1.32.0

# Image URL to use all building/pushing image targets
IMAGE ?= $(IMAGE_TAG_BASE):$(IMAGE_VERSION)

# (Default) operator image dockerfile and bundle image dockerfile.
DOCKERFILE ?= Dockerfile
BUNDLE_DOCKERFILE = bundle.Dockerfile

CRD_SOURCE_DIR := ../../config/crd/bases
CRD_DEST_DIR := config/crd/bases
SAMPLE_SOURCE_DIR := ../../sample-configs
SAMPLE_DEST_DIR := config/samples

.PHONY: all
all: docker-build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: ansible-operator
ANSIBLE_OPERATOR = $(shell pwd)/bin/ansible-operator
ansible-operator: ##Download ansible-operator locally if necessary, preferring the $(pwd)/bin path over global if both exist.
ifeq (,$(wildcard $(ANSIBLE_OPERATOR)))
ifeq (,$(shell which ansible-operator 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(ANSIBLE_OPERATOR)) ;\
	curl -sSLo $(ANSIBLE_OPERATOR) https://github.com/operator-framework/ansible-operator-plugins/releases/download/v1.32.0/ansible-operator_$(OS)_$(ARCH) ;\
	chmod +x $(ANSIBLE_OPERATOR) ;\
	}
else
ANSIBLE_OPERATOR = $(shell which ansible-operator)
endif
endif

.PHONY: bundle
bundle: copy-crds kustomize operator-sdk kustomizations ## Generate bundle manifests and metadata, then validate generated files.
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMAGE)
	find config/manifests/bases -name nri-plugins-operator.clusterserviceversion.yaml | xargs -I '{}' \
		sed -e "s|containerImage:.*|containerImage: $(IMAGE)|" -i '{}';
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	$(MAKE) bundle-validate
	$(MAKE) cleanup-crds
	
.PHONY: bundle-validate
bundle-validate:	
	$(OPERATOR_SDK) bundle validate ./bundle \
	--select-optional name=operatorhub \
	--select-optional suite=operatorframework \
	--select-optional name=good-practices \
	--select-optional suite=operatorframework

.PHONY: 	
docker-build:
	docker build -f $(DOCKERFILE) -t ${IMAGE} .

.PHONY: docker-push
docker-push:
	docker push ${IMAGE}

.PHONY: docker-save
docker-save: docker-build
	@img_name="${IMAGE}"; \
	img_name="$${img_name##*/}"; \
	img_name="$${img_name%:*}"; \
	img_info=`docker images --filter=reference=${IMAGE} --format '{{.ID}} {{.Repository}}:{{.Tag}} (created {{.CreatedSince}}, {{.CreatedAt}})' | head -n 1`; \
	img_id=`awk '{print $$1}' <<< "$${img_info}"`; \
	img_tag=`awk '{print $$2}' <<< "$${img_info}"`; \
	img_tar=`realpath "$(IMAGE_PATH)/$${img_name}-image-$${img_id}.tar"`; \
	docker image save "$${img_tag}" > "$${img_tar}";

.PHONY: bundle-build
bundle-build: bundle ## Build the bundle image.
	$(MAKE) DOCKERFILE=$(BUNDLE_DOCKERFILE) IMAGE=$(BUNDLE_IMG) docker-build

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) docker-push IMAGE=$(BUNDLE_IMG)

.PHONY: bundle-save ## Export the bundle image as a tarball.
bundle-save: bundle-build
	$(MAKE) DOCKERFILE=$(BUNDLE_DOCKERFILE) IMAGE=$(BUNDLE_IMG) docker-save

.PHONY: image-build
image-build: ## Build the operator image.
	$(MAKE) docker-build

.PHONY: image-push
image-push: ## Push the operator image.
	$(MAKE) docker-push

.PHONY: image-save
image-save: image-build ## Export the operator image as a tarball.
	$(MAKE) docker-save

# PLATFORMS defines the target platforms for  the manager image be build to provide support to multiple
# architectures. (i.e. make docker-buildx IMAGE=myregistry/mypoperator:0.0.1). To use this option you need to:
# - able to use docker buildx . More info: https://docs.docker.com/build/buildx/
# - have enable BuildKit, More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image for your registry (i.e. if you do not inform a valid value via IMAGE=<myregistry/image:<tag>> than the export will fail)
# To properly provided solutions that supports more than one platform you should use this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- docker buildx create --name project-v3-builder
	docker buildx use project-v3-builder
	- docker buildx build --push --platform=$(PLATFORMS) --tag ${IMAGE} -f Dockerfile.cross .
	- docker buildx rm project-v3-builder
	rm Dockerfile.cross

.PHONY: install
install: copy-crds kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -
	$(MAKE) cleanup-crds

.PHONY: uninstall
uninstall: copy-crds kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl delete -f -
	$(MAKE) cleanup-crds

.PHONY: deploy
deploy: copy-crds kustomize kustomizations ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMAGE}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/default | kubectl delete -f -
	$(MAKE) cleanup-crds

OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
ARCH := $(shell uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')

.PHONY: kustomize
KUSTOMIZE = $(shell pwd)/bin/kustomize
kustomize: kustomizations ## Download kustomize locally if necessary.
ifeq (,$(wildcard $(KUSTOMIZE)))
ifeq (,$(shell which kustomize 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(KUSTOMIZE)) ;\
	curl -sSLo - https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize/v4.5.7/kustomize_v4.5.7_$(OS)_$(ARCH).tar.gz | \
	tar xzf - -C bin/ ;\
	}
else
KUSTOMIZE = $(shell which kustomize)
endif
endif

.PHONY: operator-sdk
OPERATOR_SDK ?= ./bin/operator-sdk
operator-sdk: ## Download operator-sdk locally if necessary.
ifeq (,$(wildcard $(OPERATOR_SDK)))
ifeq (, $(shell which operator-sdk 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPERATOR_SDK)) ;\
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$(OS)_$(ARCH) ;\
	chmod +x $(OPERATOR_SDK) ;\
	}
else
OPERATOR_SDK = $(shell which operator-sdk)
endif
endif

.PHONY: copy-crds
copy-crds: ## Copy CRDs and CRs from root directory to operator directory for bundle packaging.
	mkdir -p $(CRD_DEST_DIR)
	cp $(CRD_SOURCE_DIR)/* $(CRD_DEST_DIR)
	cp $(SAMPLE_SOURCE_DIR)/balloons-config.yaml $(SAMPLE_DEST_DIR)
	cp $(SAMPLE_SOURCE_DIR)/template-config.yaml $(SAMPLE_DEST_DIR)
	cp $(SAMPLE_SOURCE_DIR)/topologyaware-config.yaml $(SAMPLE_DEST_DIR)
	cp $(SAMPLE_SOURCE_DIR)/noderesourcetopology.yaml $(SAMPLE_DEST_DIR)

.PHONY: cleanup-crds
cleanup-crds: ## Clean up temporarily copied CRDs and CRs.
	rm -f $(CRD_DEST_DIR)/*
	rm -f $(SAMPLE_DEST_DIR)/balloons-config.yaml
	rm -f $(SAMPLE_DEST_DIR)/template-config.yaml
	rm -f $(SAMPLE_DEST_DIR)/topologyaware-config.yaml
	rm -f $(SAMPLE_DEST_DIR)/noderesourcetopology.yaml

.PHONY: kustomizations
kustomizations: config/manager/kustomization.yaml config/manifests/bases/nri-plugins-operator.clusterserviceversion.yaml

config/manager/kustomization.yaml: config/manager/kustomization.yaml.in
	@cp $< $@

config/manifests/bases/nri-plugins-operator.clusterserviceversion.yaml: config/manifests/bases/nri-plugins-operator.clusterserviceversion.yaml.in
	@cp $< $@
