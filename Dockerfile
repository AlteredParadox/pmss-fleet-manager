# syntax=docker/dockerfile:1
# Container image for pmss-fleet-manager. Two stages: compile the static
# binary with the Go toolchain go.mod names, then copy it alone into
# distroless/static, which carries CA roots and tzdata but no shell, package
# manager or libc, and runs as the unprivileged `nonroot` user (uid 65532).
#
# The build stage runs on the BUILD platform and cross-compiles with GOARCH
# (pure Go: no cgo), so a multi-arch build needs no QEMU emulation.
#
# go.mod decides the compiler, not the builder image: GOTOOLCHAIN is set to
# the version go.mod names, so the image is built with exactly the Go the
# release binaries are built with (setup-go reads the same file) and the
# govulncheck job's stdlib findings are fixed in one place. The golang tag
# below is only the bootstrap; when it is newer than go.mod the matching
# toolchain is downloaded once per build (cached by buildx).
#
# Base images are pinned by digest like the workflow actions are pinned by
# SHA; Dependabot (docker ecosystem) refreshes them. Keep the tag in the
# comment so the pin stays readable.
#
#   docker build --build-arg VERSION=$(git describe --tags) -t pmss-fleet-manager .
#   docker run -d -p 127.0.0.1:8080:8080 -v pmss-data:/data pmss-fleet-manager

FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
WORKDIR /src
# Module download is its own layer so source edits do not refetch it.
COPY go.mod go.sum ./
# The `toolchain` line if present, else the `go` line: the same rule the go
# command applies. Written to a file so the build step reads the same value.
RUN tc=$(sed -n 's/^toolchain //p' go.mod); [ -n "$tc" ] || tc="go$(sed -n 's/^go //p' go.mod)"; \
    echo "$tc" > /toolchain && GOTOOLCHAIN="$tc" go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
# Same flags as `make build`. THIRD_PARTY_LICENSES.md is taken as committed;
# the release workflow verifies it against the module graph before any image
# is pushed (make notices-check).
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOTOOLCHAIN="$(cat /toolchain)" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/pmss-fleet-manager ./cmd/pmss-fleet-manager \
 && mkdir -p /out/data \
 && go version -m /out/pmss-fleet-manager | head -1

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/pmss-fleet-manager /pmss-fleet-manager
# An empty, nonroot-owned /data so an anonymous or fresh named volume is
# writable by the process (the store creates the database there, 0700).
COPY --from=build --chown=nonroot:nonroot /out/data /data
# Inside a container the dashboard must listen on all interfaces to be
# reachable through the published port; the Host-header check is therefore
# off unless PMSS_ALLOWED_HOSTS is set (see README, "Security and hardening").
ENV PMSS_LISTEN=0.0.0.0:8080 \
    PMSS_DATA=/data
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/pmss-fleet-manager"]
