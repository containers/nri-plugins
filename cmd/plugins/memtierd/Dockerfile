ARG GO_VERSION=1.24

FROM golang:${GO_VERSION}-bullseye AS builder

ARG IMAGE_VERSION
ARG BUILD_VERSION
ARG BUILD_BUILDID
WORKDIR /go/builder

RUN GOBIN=/bin go install -tags osusergo,netgo -ldflags "-extldflags=-static" github.com/intel/memtierd/cmd/memtierd@v0.1.1

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
COPY pkg/topology/ pkg/topology/
RUN go mod download

# Build nri-memtierd
COPY . .

RUN make clean
RUN make IMAGE_VERSION=${IMAGE_VERSION} BUILD_VERSION=${BUILD_VERSION} BUILD_BUILDID=${BUILD_BUILDID} PLUGINS=nri-memtierd build-plugins-static

FROM gcr.io/distroless/static
ENV PATH=/bin

COPY --from=builder /go/builder/build/bin/nri-memtierd /bin/nri-memtierd
COPY --from=builder /bin/memtierd /bin/memtierd

ENTRYPOINT ["/bin/nri-memtierd", "--idx", "45"]
