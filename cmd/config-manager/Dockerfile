# Build Stage
ARG GO_VERSION=1.24

FROM golang:${GO_VERSION}-bullseye AS build
WORKDIR /go/builder

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum ./
COPY pkg/topology/ pkg/topology/
RUN go mod download

# Build the config-manager
COPY cmd/config-manager/*.go ./
RUN go build -tags osusergo,netgo -ldflags "-extldflags=-static" -o config-manager

# Final Image
FROM gcr.io/distroless/static
COPY --from=build /go/builder/config-manager /bin/config-manager
ENTRYPOINT ["/bin/config-manager"]
