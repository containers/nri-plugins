ARG GO_VERSION=1.24

FROM golang:${GO_VERSION}-bullseye AS builder

ARG IMAGE_VERSION
ARG BUILD_VERSION
ARG BUILD_BUILDID
ARG DEBUG=0
ARG NORACE=0

WORKDIR /go/builder

RUN mkdir -p /debug-extras; \
    if [ "$DEBUG" = 1 ]; then \
        mkdir -p /debug-extras/bin; \
          GOBIN=/debug-extras/bin go install -tags osusergo,netgo -ldflags "-extldflags=-static" github.com/go-delve/delve/cmd/dlv@latest; \
    fi

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
COPY pkg/topology/ pkg/topology/
RUN go mod download

# Build nri-resource-policy
COPY . .

RUN make clean
RUN make IMAGE_VERSION=${IMAGE_VERSION} BUILD_VERSION=${BUILD_VERSION} BUILD_BUILDID=${BUILD_BUILDID} PLUGINS=nri-resource-policy-topology-aware DEBUG=$DEBUG NORACE=$NORACE V=$DEBUG build-plugins-static

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
COPY --from=builder /debug-extras /

ENTRYPOINT ["/bin/nri-resource-policy-topology-aware"]
