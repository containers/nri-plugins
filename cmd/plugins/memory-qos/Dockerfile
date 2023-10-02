ARG GO_VERSION=1.20

FROM golang:${GO_VERSION}-bullseye as builder

WORKDIR /go/builder

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
COPY pkg/topology/ pkg/topology/
RUN go mod download

# Build nri-resmgr
COPY . .

RUN make clean
RUN make PLUGINS=nri-memory-qos build-plugins-static

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-memory-qos /bin/nri-memory-qos
COPY --from=builder /go/builder/sample-configs/nri-memory-qos.yaml /etc/nri/memory-qos/config.yaml

ENTRYPOINT ["/bin/nri-memory-qos", "-idx", "40", "-config", "/etc/nri/memory-qos/config.yaml"]
