# Multi-stage build. The result is a small static image that runs the
# clow daemon as an unprivileged user. Nothing host-level is touched:
# tailcat is userspace-only (no TUN, no routes, no root), so the mesh
# needs no exposed ports — only outbound access to the DERP relays and
# to peers' public endpoints.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/clow .

FROM alpine:3.21
# A shell (alpine, not distroless) so `kubectl exec` can run the clow
# CLI against the daemon's IPC socket inside the container.
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S clow \
    && adduser -S -G clow clow
COPY --from=build /out/clow /usr/local/bin/clow

# The cat's identity, roster, spool, outbox and IPC socket live here;
# mount a volume or the pod restarts as a brand-new cat. The cat's name
# defaults to the hostname (the pod name under k8s) and can be set with
# CLOWDER_NAME.
ENV CLOWDER_DIR=/config
RUN mkdir -p /config && chown clow:clow /config
WORKDIR /config
VOLUME ["/config"]
USER clow

ENTRYPOINT ["clow"]
CMD ["daemon"]
