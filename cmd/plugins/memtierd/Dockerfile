ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-bookworm AS builder

ARG IMAGE_VERSION
ARG BUILD_VERSION
ARG BUILD_BUILDID
ARG DEBUG=0
ARG NORACE=0

WORKDIR /go/builder

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    GOBIN=/bin go install \
        -tags osusergo,netgo \
        -ldflags "-extldflags=-static" \
        github.com/intel/memtierd/cmd/memtierd@v0.1.1

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum .
COPY pkg/topology/ pkg/topology/
RUN --mount=type=cache,target=/go/pkg/mod/ go mod download

# Build nri-memtierd
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
         PLUGINS=nri-memtierd \
         clean install-go-licenses build-plugins-static licenses

FROM gcr.io/distroless/static
ENV PATH=/bin

COPY --from=builder /go/builder/build/bin/nri-memtierd /bin/nri-memtierd
COPY --from=builder /go/builder/build/licenses/nri-memtierd/ /licenses/nri-memtierd/
COPY --from=builder /bin/memtierd /bin/memtierd

ENTRYPOINT ["/bin/nri-memtierd", "--idx", "45"]
