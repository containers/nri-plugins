ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-bookworm AS builder

ARG IMAGE_VERSION
ARG BUILD_VERSION
ARG BUILD_BUILDID
WORKDIR /go/builder

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
COPY pkg/topology/ pkg/topology/
RUN go mod download

# Build nri-resmgr
COPY . .

RUN make clean install-go-licenses
RUN make IMAGE_VERSION=${IMAGE_VERSION} BUILD_VERSION=${BUILD_VERSION} BUILD_BUILDID=${BUILD_BUILDID} PLUGINS=nri-memory-qos build-plugins-static licenses

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-memory-qos /bin/nri-memory-qos
COPY --from=builder /go/builder/build/licenses/nri-memory-qos/ /licenses/nri-memory-qos/
COPY --from=builder /go/builder/sample-configs/nri-memory-qos.yaml /etc/nri/memory-qos/config.yaml

ENTRYPOINT ["/bin/nri-memory-qos", "-idx", "40", "-config", "/etc/nri/memory-qos/config.yaml"]
