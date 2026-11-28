# syntax=docker/dockerfile:1
#
# The Origin CSI driver's image. One image serves both plugins
# (--mode=controller, --mode=node). The binary is a static Go build; the
# base holds the tools the node plugin runs: blkid, mkfs.ext4, mkfs.xfs,
# fsck, resize2fs, xfs_growfs, mount and umount.
#
# Build for one platform:   docker build --build-arg VERSION=v0.1.0 .
# Build for both:           docker buildx build --platform linux/amd64,linux/arm64 .

# The Go build runs on the builder's own platform and cross-compiles, so a
# multi-arch build emulates only the package install below.
FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS build

ARG TARGETOS
ARG TARGETARCH
# The version GetPluginInfo and --version answer; the release workflow
# sets it to the tag.
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/origincloud-csi-driver ./cmd/origincloud-csi-driver

FROM debian:trixie-slim

# ca-certificates: the trust store for the TLS calls to the Origin Cloud APIs;
# util-linux: blkid (and fsck); mount: mount, umount;
# e2fsprogs: mkfs.ext4, resize2fs, e2fsck; xfsprogs: mkfs.xfs, xfs_growfs.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates e2fsprogs mount util-linux xfsprogs \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/origincloud-csi-driver /usr/local/bin/origincloud-csi-driver

LABEL org.opencontainers.image.source="https://github.com/origincloudhq/csi-driver-origincloud" \
      org.opencontainers.image.description="The Origin CSI driver, pvs.csi.origincloud.eu" \
      org.opencontainers.image.licenses="Apache-2.0"

ENTRYPOINT ["/usr/local/bin/origincloud-csi-driver"]
