# fake-cxl-pool: build the server (host), the client (host and VMs), the
# DRA pool controller (VMs or a cluster), cxl-numa-place (host: the NUMA
# placement of the device backing files) and cxl-load (host: background
# memory traffic on the far socket).
#
#   make            build bin/fake-cxl-pool-server, bin/fake-cxl-pool-client,
#                   bin/fake-cxl-pool-controller, bin/cxl-numa-place and
#                   bin/cxl-load
#   make image      build the image of the controller (Dockerfile) with docker
#                   and save it to bin/fake-cxl-pool-controller-image.tar

GO      ?= go
PKG     := github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool
BINDIR  := bin
# the client and the controller run in the VMs: build them static
CLIENT_ENV := CGO_ENABLED=0 GOARCH=amd64
DOCKER  ?= docker
# The image of the controller, tagged also with the first 12 hex digits of
# the sha256 of its binary (that tag in CONTROLLER_IMAGE_REF), saved with
# both tags.
CONTROLLER_IMAGE_NAME ?= localhost/fake-cxl-pool-controller
CONTROLLER_IMAGE      ?= $(CONTROLLER_IMAGE_NAME):testing
CONTROLLER_IMAGE_TAR  := $(BINDIR)/fake-cxl-pool-controller-image.tar
CONTROLLER_IMAGE_REF  := $(BINDIR)/fake-cxl-pool-controller-image.ref

all: $(BINDIR)/fake-cxl-pool-server $(BINDIR)/fake-cxl-pool-client $(BINDIR)/fake-cxl-pool-controller \
	$(BINDIR)/cxl-numa-place $(BINDIR)/cxl-load

$(BINDIR)/fake-cxl-pool-server: FORCE
	$(GO) build -o $@ $(PKG)/cmd/fake-cxl-pool-server

$(BINDIR)/fake-cxl-pool-client: FORCE
	$(CLIENT_ENV) $(GO) build -o $@ $(PKG)/cmd/fake-cxl-pool-client

$(BINDIR)/fake-cxl-pool-controller: FORCE
	$(CLIENT_ENV) $(GO) build -o $@ $(PKG)/cmd/fake-cxl-pool-controller

# static, without cgo, like the binaries of the VMs
$(BINDIR)/cxl-numa-place: FORCE
	CGO_ENABLED=0 $(GO) build -o $@ $(PKG)/cmd/cxl-numa-place

$(BINDIR)/cxl-load: FORCE
	CGO_ENABLED=0 $(GO) build -o $@ $(PKG)/cmd/cxl-load

image: $(BINDIR)/fake-cxl-pool-controller
	@set -e; ref=$(CONTROLLER_IMAGE_NAME):$$(sha256sum $< | cut -c1-12); \
	$(DOCKER) build -q -t $(CONTROLLER_IMAGE) -t $$ref . >/dev/null; \
	$(DOCKER) save -o $(CONTROLLER_IMAGE_TAR) $(CONTROLLER_IMAGE) $$ref; \
	echo $$ref > $(CONTROLLER_IMAGE_REF); \
	echo "$(CONTROLLER_IMAGE_TAR): $(CONTROLLER_IMAGE) $$ref"

clean:
	rm -rf $(BINDIR)

.PHONY: all image clean FORCE
FORCE:
