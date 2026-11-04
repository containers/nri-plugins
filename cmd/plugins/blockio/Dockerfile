ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS builder

ARG IMAGE_VERSION
ARG BUILD_VERSION
ARG BUILD_BUILDID
ARG DEBUG=0
ARG NORACE=0
ARG SKIP_LICENSES=0

WORKDIR /go/builder

COPY go.mod go.sum .
COPY pkg/topology/ pkg/topology/
RUN --mount=type=cache,target=/go/pkg/mod/ go mod download

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
         PLUGINS=nri-blockio \
         build-plugins-static

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-blockio /bin/nri-blockio
COPY build/licenses/nri-blockio/ /licenses/nri-blockio/
COPY --from=builder /go/builder/sample-configs/nri-blockio.yaml /etc/nri/blockio/config.yaml

ENTRYPOINT ["/bin/nri-blockio", "-idx", "50", "-config", "/etc/nri/blockio/config.yaml"]
