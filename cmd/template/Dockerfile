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
RUN make build-static

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-resmgr-template /bin/nri-resmgr-template

ENTRYPOINT ["/bin/nri-resmgr-template"]
