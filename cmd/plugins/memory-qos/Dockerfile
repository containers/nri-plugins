ARG GO_VERSION=1.25

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

# Build nri-resmgr
COPY . .

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    make IMAGE_VERSION=${IMAGE_VERSION} \
         BUILD_VERSION=${BUILD_VERSION} \
         BUILD_BUILDID=${BUILD_BUILDID} \
         DEBUG=$DEBUG \
         NORACE=$NORACE \
         OTHER_IMAGE_TARGETS="" \
         BINARIES="" \
         PLUGINS=nri-memory-qos \
         clean install-go-licenses build-plugins-static licenses

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-memory-qos /bin/nri-memory-qos
COPY --from=builder /go/builder/build/licenses/nri-memory-qos/ /licenses/nri-memory-qos/
COPY --from=builder /go/builder/sample-configs/nri-memory-qos.yaml /etc/nri/memory-qos/config.yaml

ENTRYPOINT ["/bin/nri-memory-qos", "-idx", "40", "-config", "/etc/nri/memory-qos/config.yaml"]
