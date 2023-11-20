ARG GO_VERSION=1.20

FROM golang:${GO_VERSION}-bullseye as builder

ARG DEBUG=0

WORKDIR /go/builder

RUN if [ "$DEBUG" = 1 ]; then \
        GOBIN=/go/builder/build/bin go install -tags osusergo,netgo -ldflags "-extldflags=-static" github.com/go-delve/delve/cmd/dlv@latest; \
    fi

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
COPY pkg/topology/ pkg/topology/
RUN go mod download

# Build nri-resource-policy
COPY . .

RUN make clean
RUN make PLUGINS=nri-resource-policy-balloons DEBUG=$DEBUG V=$DEBUG build-plugins-static
RUN if [ "$DEBUG" = 0 ]; then \
        for d in /go/builder/build/bin/dlv /go/builder/pkg /go/builder/cmd /go/pkg /usr/local/go/src ; do \
            mv $d $d-dont-copy 2>/dev/null; \
            touch $d; \
        done; \
    fi

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-resource-policy-balloons /bin/nri-resource-policy-balloons
COPY --from=builder /go/builder/build/bin/dlv /bin/dlv
COPY --from=builder /go/builder/pkg /go/builder/pkg
COPY --from=builder /go/builder/cmd /go/builder/cmd
COPY --from=builder /go/pkg /go/pkg
COPY --from=builder /usr/local/go/src /usr/local/go/src

ENTRYPOINT ["/bin/nri-resource-policy-balloons"]
