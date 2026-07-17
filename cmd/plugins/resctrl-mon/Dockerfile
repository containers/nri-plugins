ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS builder

ARG IMAGE_VERSION
ARG BUILD_VERSION
ARG BUILD_BUILDID
ARG DEBUG=0
ARG NORACE=0
ARG SKIP_LICENSES=0

WORKDIR /go/builder

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum .
COPY pkg/topology/ pkg/topology/
RUN --mount=type=cache,target=/go/pkg/mod/ go mod download

# Build nri-resctrl-mon
COPY . .

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    make install-go-licenses

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    make IMAGE_VERSION=${IMAGE_VERSION} \
         BUILD_VERSION=${BUILD_VERSION} \
         BUILD_BUILDID=${BUILD_BUILDID} \
         DEBUG=$DEBUG \
         NORACE=$NORACE \
         OTHER_IMAGE_TARGETS="" \
         BINARIES="" \
         PLUGINS=nri-resctrl-mon \
         clean build-plugins-static licenses

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-resctrl-mon /bin/nri-resctrl-mon
COPY --from=builder /go/builder/build/licenses/nri-resctrl-mon/ /licenses/nri-resctrl-mon/
COPY --from=builder /go/builder/sample-configs/nri-resctrl-mon.yaml /etc/nri/resctrl-mon/config.yaml

ENTRYPOINT ["/bin/nri-resctrl-mon", "-idx", "90", "-config", "/etc/nri/resctrl-mon/config.yaml"]
