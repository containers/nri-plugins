ARG GO_VERSION=1.22

FROM golang:${GO_VERSION}-bullseye as builder

ARG IMAGE_VERSION
ARG BUILD_VERSION
ARG BUILD_BUILDID
WORKDIR /go/builder

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
RUN go mod download

# Build the nri-cdi-device-injector plugin.
COPY . .

RUN make clean
RUN make IMAGE_VERSION=${IMAGE_VERSION} BUILD_VERSION=${BUILD_VERSION} BUILD_BUILDID=${BUILD_BUILDID} PLUGINS=nri-cdi-device-injector build-plugins-static

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-cdi-device-injector /bin/nri-cdi-device-injector

ENTRYPOINT ["/bin/nri-cdi-device-injector", "-idx", "40"]
