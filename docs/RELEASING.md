# Releasing fleet-server

The server ships as a container image. Consumers (the club's `delivery-gdg-platform`
compose stack first) reference it by exact tag and bump deliberately; nothing is built
from this repo's source inside a consumer's pipeline.

## What CI publishes

`.github/workflows/ci.yml` runs tests on every push and PR. On the default branch and on
version tags it also builds a multi-arch image (`linux/amd64` for the GCE VM,
`linux/arm64` for robots and Macs) and pushes it to GitHub Container Registry:

| Git ref | Image tags |
|---|---|
| tag `v0.3.1` | `ghcr.io/<owner>/fleet-server:0.3.1`, `:0.3`, `:0` |
| default branch | `:edge` |
| every published push | `:sha-<short sha>` |

`<owner>` is the GitHub org or user that hosts this repo, lowercased.

## Versioning rule

The image **major** version is the protocol version, the `v` field in every envelope.
While the protocol is v0, a **minor** bump may change the wire incompatibly, so consumers
pin the full `X.Y.Z` and never a floating `:0` or `:edge` in production. Patch releases
never change the wire.

## Cutting a release

```bash
git checkout main && git pull
git tag -a v0.1.0 -m "fleet-server 0.1.0"
git push origin v0.1.0
```

CI publishes the image a few minutes later. Then bump the tag in the consumer's compose
file and let its own pipeline deploy it.

## First-publish checklist

- **Make the package public.** A GHCR package is private the first time it is pushed. Go
  to the org or user page → *Packages* → `fleet-server` → *Package settings* → *Danger
  Zone* → *Change visibility* → Public. Until then, the deploy VM needs
  `docker login ghcr.io` with a token that has `read:packages`.
- **Link the package to the repo** on the same settings page so the README and
  permissions follow the repo.

## Handy commands

```bash
# Which version is in this image?
docker run --rm ghcr.io/<owner>/fleet-server:0.1.0 -version

# Mint an enrollment key for a fleet using the image (state lands in the mounted volume)
docker run --rm -v fleet-data:/var/lib/fleet ghcr.io/<owner>/fleet-server:0.1.0 \
  -bootstrap my-fleet

# Throwaway key against a temp database, nothing persisted
docker run --rm -e FLEET_DB=/tmp/x.db ghcr.io/<owner>/fleet-server:0.1.0 -bootstrap my-fleet

# Local build without CI
docker build -f server/Dockerfile -t fleet-server:dev --build-arg VERSION=dev .
```

Configuration in the container is by environment variable (`FLEET_LISTEN`, `FLEET_DB`,
`FLEET_HEARTBEAT_INTERVAL_MS`, `FLEET_LEASE_TTL_MS`, `FLEET_SWEEP_MS`); a mounted
`-config` file also works. The image's `HEALTHCHECK` runs `fleet-server -healthcheck`,
which hits `/healthz` on the configured listen port.
