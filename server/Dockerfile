# fleet-server image. Built by .github/workflows/ci.yml and published to
# ghcr.io/<owner>/fleet-server (see docs/RELEASING.md); consumers pin an exact tag.
#
# Config is read from env vars so no config file needs to be mounted:
#   FLEET_LISTEN                 (default ":8080")
#   FLEET_DB                     (default "/var/lib/fleet/fleet.db", set below)
#   FLEET_HEARTBEAT_INTERVAL_MS  FLEET_LEASE_TTL_MS  FLEET_SWEEP_MS
# A -config file still works if you mount one.

# ---- build --------------------------------------------------------------------
FROM golang:1.26 AS build

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Dependency layer first so source edits don't re-download modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# modernc.org/sqlite is pure Go, so CGO can stay off: static binary, cross-compiles
# cleanly for the multi-platform build.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/fleet-server ./cmd/fleet-server

# ---- runtime ------------------------------------------------------------------
# distroless static: no shell, no package manager, runs as nonroot (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/fleet-server /usr/local/bin/fleet-server

ENV FLEET_DB=/var/lib/fleet/fleet.db
WORKDIR /var/lib/fleet
VOLUME /var/lib/fleet

EXPOSE 8080

# -healthcheck GETs /healthz on the configured listen port and exits 0/1.
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s \
    CMD ["fleet-server", "-healthcheck"]

ENTRYPOINT ["fleet-server"]
