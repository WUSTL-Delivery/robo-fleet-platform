# fleet-server image. Built by .github/workflows/ci.yml and published to
# ghcr.io/<owner>/fleet-server (see docs/RELEASING.md); consumers pin an exact tag.
#
# The build context is the REPO ROOT (the console and the TS SDK live outside
# server/), so build it with:
#   docker build -f server/Dockerfile -t fleet-server:dev .
# The web console (console/) is built first and embedded into the binary, so the
# one image serves the console at /, clients at /ws, and the admin API.
#
# Config is read from env vars so no config file needs to be mounted:
#   FLEET_LISTEN                 (default ":8080")
#   FLEET_DB                     (default "/var/lib/fleet/fleet.db", set below)
#   FLEET_HEARTBEAT_INTERVAL_MS  FLEET_LEASE_TTL_MS  FLEET_SWEEP_MS
#   FLEET_CLIENT_MSGS_PER_SEC    FLEET_CLIENT_MSGS_BURST (per-connection rate limits)
#   FLEET_MAP_CENTER "lat,lon"   FLEET_MAP_RADIUS_M  FLEET_MAP_LOCK (console map home view)
# A -config file still works if you mount one.

# ---- console ------------------------------------------------------------------
# Static assets are platform-independent, so build them once on the build host's
# platform even for the multi-platform image.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS console

WORKDIR /src

# The console depends on the SDK via "file:../sdk/typescript"; build the SDK first.
COPY sdk/typescript/package.json sdk/typescript/package-lock.json sdk/typescript/
RUN cd sdk/typescript && npm ci
COPY console/package.json console/package-lock.json console/
RUN cd console && npm ci

COPY sdk/typescript/ sdk/typescript/
RUN cd sdk/typescript && npm run build
COPY console/ console/
RUN cd console && npm run build

# ---- build --------------------------------------------------------------------
FROM golang:1.26 AS build

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Dependency layer first so source edits don't re-download modules.
COPY server/go.mod server/go.sum ./
RUN go mod download

COPY server/ .
# go:embed picks this up (internal/web/console.go).
COPY --from=console /src/console/dist/ ./internal/web/dist/

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
