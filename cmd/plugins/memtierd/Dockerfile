ARG GO_VERSION=1.20

FROM golang:${GO_VERSION}-bullseye as builder

WORKDIR /go/builder

RUN GOBIN=/bin go install -tags osusergo,netgo -ldflags "-extldflags=-static" github.com/intel/memtierd/cmd/memtierd@c67204d6af3e5f64cd396f1c29aafa729e4363ba

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
COPY pkg/topology/ pkg/topology/
RUN go mod download

# Build nri-memtierd
COPY . .

RUN make clean
RUN make PLUGINS=nri-memtierd build-plugins-static

FROM gcr.io/distroless/static
ENV PATH=/bin

COPY --from=builder /go/builder/build/bin/nri-memtierd /bin/nri-memtierd
COPY --from=builder /bin/memtierd /bin/memtierd

ENTRYPOINT ["/bin/nri-memtierd", "--idx", "45"]
