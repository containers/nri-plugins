ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS builder

ARG IMAGE_VERSION
ARG BUILD_VERSION
ARG BUILD_BUILDID
ARG DEBUG=0
ARG NORACE=0
ARG SKIP_LICENSES=0

WORKDIR /go/builder

# Fetch go dependencies in a separate layer for caching
COPY go.mod go.sum .
COPY pkg/topology/ pkg/topology/
RUN --mount=type=cache,target=/go/pkg/mod/ go mod download

# Build nri-dra-driver-cxl and cxl-request
COPY . .

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    make IMAGE_VERSION=${IMAGE_VERSION} \
         BUILD_VERSION=${BUILD_VERSION} \
         BUILD_BUILDID=${BUILD_BUILDID} \
         V=$DEBUG \
         DEBUG=$DEBUG \
         NORACE=$NORACE \
         OTHER_IMAGE_TARGETS="" \
         BINARIES=cxl-request \
         PLUGINS=nri-dra-driver-cxl \
         clean build-plugins-static build-binaries-static

# Build the cxl and ndctl tools of ndctl. They are dynamically linked
# (Debian has no static libudev), so collect them with their whole shared
# library closure, including the dynamic loader, into /rootfs. Paths in
# /rootfs are canonical (/usr/lib, not /lib) as /lib, /lib64 and /bin are
# symbolic links in the final image.
FROM debian:bookworm AS ndctl

ARG NDCTL_VERSION=85
# the commit of the tag v85
ARG NDCTL_COMMIT=15e932c4e1318a9608ad9b799ad83a32a8b5970d

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates git gcc libc6-dev meson ninja-build pkg-config \
        libkmod-dev libudev-dev uuid-dev libjson-c-dev libiniparser-dev && \
    rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 --branch v${NDCTL_VERSION} \
        https://github.com/pmem/ndctl.git /src/ndctl && \
    test "$(git -C /src/ndctl rev-parse HEAD)" = "${NDCTL_COMMIT}"

WORKDIR /src/ndctl

RUN meson setup build \
        -Dversion-tag=${NDCTL_VERSION} \
        -Ddocs=disabled \
        -Dasciidoctor=disabled \
        -Dsystemd=disabled \
        -Dlibtracefs=disabled \
        -Dkeyutils=disabled \
        -Dfwctl=disabled \
        -Dtest=disabled \
        -Ddestructive=disabled \
        -Dpoison=disabled \
        -Dbashcompletiondir=no && \
    meson compile -C build && \
    DESTDIR=/ndctl meson install -C build

# Copy cxl, ndctl and their libraries to /rootfs, with the license
# files of ndctl and the copyright file of the Debian package of every
# library.
RUN set -e; \
    mkdir -p /rootfs/usr/bin /rootfs/licenses/ndctl/debian; \
    for bin in cxl ndctl; do \
        cp /ndctl/usr/bin/$bin /rootfs/usr/bin/$bin; \
        LD_LIBRARY_PATH=/ndctl/usr/lib ldd /ndctl/usr/bin/$bin > /ldd.$bin; \
        if grep -q "not found" /ldd.$bin; then cat /ldd.$bin; exit 1; fi; \
    done; \
    sed -n -e 's:.* => \(/[^ ]*\) .*:\1:p' -e 's:^[[:space:]]*\(/[^ ]*\) .*:\1:p' \
        /ldd.cxl /ldd.ndctl | sort -u > /libs.txt; \
    grep -q "/ld-linux" /libs.txt; \
    while read -r lib; do \
        dir=$(realpath "$(dirname "$lib")"); \
        dir=${dir#/ndctl}; \
        mkdir -p "/rootfs$dir"; \
        cp -L "$lib" "/rootfs$dir/$(basename "$lib")"; \
        case "$lib" in \
            /ndctl/*) continue;; \
        esac; \
        pkg=$(dpkg -S "*/$(basename "$lib")" | head -n 1 | cut -d: -f1); \
        mkdir -p "/rootfs/licenses/ndctl/debian/$pkg"; \
        cp "/usr/share/doc/$pkg/copyright" "/rootfs/licenses/ndctl/debian/$pkg/"; \
    done < /libs.txt; \
    cp -r COPYING LICENSES /rootfs/licenses/ndctl/; \
    find /rootfs -type f | sort

FROM gcr.io/distroless/static

COPY --from=builder /go/builder/build/bin/nri-dra-driver-cxl /bin/nri-dra-driver-cxl
COPY --from=builder /go/builder/build/bin/cxl-request /bin/cxl-request
# The licenses of cxl-request are those of the driver: the Go standard
# library and this repository.
COPY build/licenses/nri-dra-driver-cxl/ /licenses/nri-dra-driver-cxl/
COPY --from=ndctl /rootfs/ /

ENTRYPOINT ["/bin/nri-dra-driver-cxl"]
