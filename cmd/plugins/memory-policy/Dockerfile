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
RUN make IMAGE_VERSION=${IMAGE_VERSION} BUILD_VERSION=${BUILD_VERSION} BUILD_BUILDID=${BUILD_BUILDID} PLUGINS=nri-memory-policy BINARIES=mpolset build-plugins-static build-binaries-static licenses

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-memory-policy /bin/nri-memory-policy
COPY --from=builder /go/builder/build/bin/mpolset /bin/mpolset
COPY --from=builder /go/builder/build/licenses/nri-memory-policy/ /licenses/nri-memory-policy/
COPY --from=builder /go/builder/build/licenses/mpolset/ /licenses/mpolset/
COPY --from=builder /go/builder/sample-configs/nri-memory-policy.yaml /etc/nri/memory-policy/config.yaml

ENTRYPOINT ["/bin/nri-memory-policy", "-idx", "92", "-config", "/etc/nri/memory-policy/config.yaml"]
