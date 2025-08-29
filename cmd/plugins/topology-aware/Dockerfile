ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-bookworm AS builder

ARG IMAGE_VERSION
ARG BUILD_VERSION
ARG BUILD_BUILDID
ARG DEBUG=0
ARG NORACE=0
ARG SKIP_LICENSES=0

WORKDIR /go/builder

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    mkdir -p /debug-extras; \
    if [ "$DEBUG" = 1 ]; then \
        mkdir -p /debug-extras/bin; \
          GOBIN=/debug-extras/bin go install -tags osusergo,netgo -ldflags "-extldflags=-static" github.com/go-delve/delve/cmd/dlv@latest; \
    fi

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum .
COPY pkg/topology/ pkg/topology/
RUN --mount=type=cache,target=/go/pkg/mod/ go mod download

# Build nri-resource-policy
COPY . .

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    make IMAGE_VERSION=${IMAGE_VERSION} \
         BUILD_VERSION=${BUILD_VERSION} \
         BUILD_BUILDID=${BUILD_BUILDID} \
         V=$DEBUG \
         DEBUG=$DEBUG \
         NORACE=$NORACE \
         BINARIES="" \
         OTHER_IMAGE_TARGETS="" \
         PLUGINS=nri-resource-policy-topology-aware \
         clean install-go-licenses build-plugins-static licenses

RUN cpgodir() { \
        mkdir -p $2; \
        find $1 -name '*.s' -o -name '*.go' | grep -v -E '/test/|/testdata/|_test.go' \
        | xargs -I {} cp --parents {} $2; \
    }; \
    if [ "$DEBUG" = 1 ]; then \
        cpgodir /go/pkg /debug-extras; \
        cpgodir /go/builder/pkg /debug-extras; \
        cpgodir /go/builder/cmd /debug-extras; \
        cpgodir /go/builder/vendor /debug-extras; \
        cpgodir /usr/local/go /debug-extras; \
    fi

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-resource-policy-topology-aware /bin/nri-resource-policy-topology-aware
COPY --from=builder /go/builder/build/licenses/nri-resource-policy-topology-aware/ /licenses/nri-resource-policy-topology-aware/
COPY --from=builder /debug-extras /

ENTRYPOINT ["/bin/nri-resource-policy-topology-aware"]
