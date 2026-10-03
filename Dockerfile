# Multi-stage build. The result is a small static image that runs the
# clow daemon as an unprivileged user. Nothing host-level is touched:
# tailcat is userspace-only (no TUN, no routes, no root), so the mesh
# needs no exposed ports — only outbound access to the DERP relays and
# to peers' public endpoints.

# Build on the builder's native platform (BUILDPLATFORM) and let Go's
# cross-compiler target the image platform (TARGETOS/TARGETARCH). An
# emulated `go build` is slow on Apple Silicon and crashes outright
# under Rosetta ("found pointer to free object" — Go's GC breaks there).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/clow .

FROM alpine:3.21
# A shell (alpine, not distroless) so `kubectl exec` can run the clow
# CLI against the daemon's IPC socket inside the container.
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S clow \
    && adduser -S -G clow clow
COPY --from=build /out/clow /usr/local/bin/clow

# The clowder config base lives here; each local clowder is a directory
# under it (CLOWDER=<name>, default "default" — one clowder per
# container unless you mount a shared base). Mount a volume or the pod
# restarts as a brand-new cat. The cat's name defaults to the hostname
# (the pod name under k8s) and can be set with CLOWDER_NAME.
# CLOWDER_HEALTH_ADDR (e.g. ":8080") optionally serves GET /healthz for
# container probes — the only inbound port the daemon ever opens.
# CLOWDER_DERPMAP_URL points the daemon at a self-hosted DERP map for
# air-gapped clusters.
ENV CLOWDER_DIR=/config
RUN mkdir -p /config && chown clow:clow /config
WORKDIR /config
VOLUME ["/config"]
USER clow

ENTRYPOINT ["clow"]
CMD ["daemon"]
