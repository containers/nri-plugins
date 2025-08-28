ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /go/builder

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
COPY pkg/topology/ pkg/topology/
RUN go mod download

# Build config-manager binary
COPY . .

RUN make clean install-go-licenses
RUN make IMAGE_VERSION=${IMAGE_VERSION} BUILD_VERSION=${BUILD_VERSION} BUILD_BUILDID=${BUILD_BUILDID} PLUGINS= BINARIES=config-manager build-binaries-static licenses

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/config-manager /bin/config-manager
COPY --from=builder /go/builder/build/licenses/config-manager/ /licenses/config-manager/

ENTRYPOINT ["/bin/config-manager"]
