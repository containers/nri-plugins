ARG GO_VERSION=1.19

FROM golang:${GO_VERSION}-buster as builder

WORKDIR /go/builder

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
COPY pkg/topology/ pkg/topology/
RUN go mod download

# Build nri-resmgr
COPY . .

RUN make clean
RUN make PLUGINS=nri-resource-policy-template build-plugins-static

FROM gcr.io/distroless/static

LABEL org.opencontainers.image.source=https://github.com/containers/nri-plugins
LABEL org.opencontainers.image.description="NRI template resource policy plugin"
LABEL org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /go/builder/build/bin/nri-resource-policy-template /bin/nri-resource-policy-template

ENTRYPOINT ["/bin/nri-resource-policy-template"]
