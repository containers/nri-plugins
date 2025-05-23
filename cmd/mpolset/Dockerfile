ARG GO_VERSION=1.23

FROM golang:${GO_VERSION}-bullseye AS builder

ARG IMAGE_VERSION
ARG BUILD_VERSION
ARG BUILD_BUILDID
ARG DEBUG=0
ARG NORACE=0

WORKDIR /go/builder

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
COPY pkg/topology/ pkg/topology/
RUN go mod download

# Build mpolset
COPY . .

RUN make BINARIES=mpolset IMAGE_VERSION=${IMAGE_VERSION} BUILD_VERSION=${BUILD_VERSION} BUILD_BUILDID=${BUILD_BUILDID} build-binaries-static

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/mpolset /bin/mpolset

ENTRYPOINT ["/bin/mpolset"]
