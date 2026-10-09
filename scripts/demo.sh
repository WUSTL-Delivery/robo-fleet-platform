#!/usr/bin/env bash
# The two-operator demo, from source: builds fleet-server with the console
# embedded, starts it on a throwaway database, creates a fleet and operator
# invites, and starts a sim fleet whose robots ask for help at random.
# Walkthrough: docs/TESTING.md, "The two-operator demo".
#
#   ./scripts/demo.sh            # then open the printed URL in two browser profiles
#   ./scripts/demo.sh --help
#
# Ctrl-C stops the server and the sim and deletes the database.
set -euo pipefail

DEFAULT_PORT=8090
port=""
count=20
help_rate=0.15
operators=2
lan=0
keep=0

usage() {
  cat <<EOF
Usage: scripts/demo.sh [options]

  --port <n>        port for the console and server (default: the first free port from $DEFAULT_PORT)
  --count <n>       sim robots (default $count)
  --help-rate <r>   mean help requests per robot per minute (default $help_rate)
  --operators <n>   operator invites to create (default $operators)
  --lan             listen on every interface, so a second machine on this network can join
                    (default: this machine only)
  --keep            keep the database and logs after exit instead of deleting them
  -h, --help        show this help

Needs Go 1.26+, Node 20+ with npm, and curl.
EOF
}

die() { echo "demo: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --port|--count|--help-rate|--operators)
      [[ $# -ge 2 ]] || die "$1 needs a value"
      case "$1" in
        --port) port="$2" ;;
        --count) count="$2" ;;
        --help-rate) help_rate="$2" ;;
        --operators) operators="$2" ;;
      esac
      shift 2 ;;
    --lan) lan=1; shift ;;
    --keep) keep=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown option: $1" ;;
  esac
done

is_int() { [[ "$1" =~ ^[0-9]+$ ]]; }
is_int "$count" && (( count >= 1 )) || die "--count must be a whole number, 1 or more"
is_int "$operators" && (( operators >= 1 )) || die "--operators must be a whole number, 1 or more"
[[ "$help_rate" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "--help-rate must be a number, e.g. 0.15"
if [[ -n "$port" ]]; then
  is_int "$port" && (( port >= 1 && port <= 65535 )) || die "--port must be between 1 and 65535"
fi

# ---- prerequisites ----------------------------------------------------------

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 not found. $2"
}
need go "Install Go 1.26 or newer: https://go.dev/dl/"
need node "Install Node 20 or newer: https://nodejs.org/"
need npm "npm ships with Node: https://nodejs.org/"
need curl "Install curl."

go_ver="$(go env GOVERSION)"; go_ver="${go_ver#go}"
go_major="${go_ver%%.*}"; go_rest="${go_ver#*.}"; go_minor="${go_rest%%[!0-9]*}"
if (( go_major < 1 || (go_major == 1 && go_minor < 26) )); then
  die "Go $go_ver found, but the server needs Go 1.26 or newer: https://go.dev/dl/"
fi
node_major="$(node -p 'process.versions.node.split(".")[0]')"
(( node_major >= 20 )) || die "Node $(node --version) found, but the console and sim need Node 20 or newer."

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[[ -f "$root/server/go.mod" && -f "$root/console/package.json" ]] || die "run this from a fleet-platform checkout"

# ---- port -------------------------------------------------------------------

# True when something already accepts connections on the port.
port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

if [[ -n "$port" ]]; then
  port_busy "$port" && die "port $port is already in use. Pick another with --port."
else
  port=$DEFAULT_PORT
  while port_busy "$port"; do
    (( port < DEFAULT_PORT + 20 )) || die "no free port from $DEFAULT_PORT to $port. Pick one with --port."
    port=$((port + 1))
  done
fi

# ---- cleanup ----------------------------------------------------------------

tmp="${TMPDIR:-/tmp}"
work="$(mktemp -d "${tmp%/}/fleet-demo.XXXXXX")"
server_pid=""
sim_pid=""

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  for pid in "$sim_pid" "$server_pid"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  for pid in "$sim_pid" "$server_pid"; do
    [[ -n "$pid" ]] && wait "$pid" 2>/dev/null || true
  done
  if (( keep )); then
    echo "demo: stopped. Database and logs kept in $work"
  else
    rm -rf "$work"
    echo "demo: stopped. Database deleted."
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Runs a build step quietly; on failure shows its output.
step() {
  local label="$1"; shift
  printf 'demo: %s...\n' "$label"
  if ! "$@" >"$work/build.log" 2>&1; then
    cat "$work/build.log" >&2
    keep=1
    die "$label failed (output above)"
  fi
}

npm_install() { # <dir>: install dependencies once
  [[ -d "$1/node_modules" ]] || (cd "$1" && npm ci --no-audit --no-fund)
}

# ---- build ------------------------------------------------------------------

started=$SECONDS
step "building the TypeScript SDK" bash -c "
  set -e; cd '$root/sdk/typescript'; [[ -d node_modules ]] || npm ci --no-audit --no-fund; npm run build"
step "building the console and embedding it in the server" bash -c "
  set -e; cd '$root/console'; [[ -d node_modules ]] || npm ci --no-audit --no-fund; npm run embed"
step "installing the sim" npm_install "$root/sim"
step "building fleet-server and fleetctl" bash -c "
  set -e; cd '$root/server'
  go build -o '$work/fleet-server' ./cmd/fleet-server
  go build -o '$work/fleetctl' ./cmd/fleetctl"

# ---- server -----------------------------------------------------------------

secret() { node -e 'console.log(require("node:crypto").randomBytes(24).toString("hex"))'; }
admin_token="$(secret)"
enroll_key="$(secret)"
fleet="demo"

if (( lan )); then listen=":$port"; else listen="127.0.0.1:$port"; fi
base="http://127.0.0.1:$port"

FLEET_LISTEN="$listen" \
FLEET_DB="$work/fleet.db" \
FLEET_ADMIN_TOKEN="$admin_token" \
FLEET_BOOTSTRAP_FLEET="$fleet" \
FLEET_BOOTSTRAP_ENROLL_KEY="$enroll_key" \
  "$work/fleet-server" >"$work/server.log" 2>&1 &
server_pid=$!

for _ in $(seq 1 50); do
  curl -fsS "$base/healthz" >/dev/null 2>&1 && break
  kill -0 "$server_pid" 2>/dev/null || { cat "$work/server.log" >&2; keep=1; die "fleet-server exited (log above)"; }
  sleep 0.2
done
curl -fsS "$base/healthz" >/dev/null 2>&1 || { cat "$work/server.log" >&2; keep=1; die "fleet-server did not answer on $base"; }
# A build without the console answers / with a placeholder; the demo needs the real one.
curl -fsS "$base/" | grep -q '<div id="root">' || die "the console is not embedded in this build (console/ 'npm run embed' did not take)"

# ---- operator invites -------------------------------------------------------

invites=()
for _ in $(seq 1 "$operators"); do
  out="$(FLEETCTL_SERVER="$base" FLEETCTL_TOKEN="$admin_token" "$work/fleetctl" invite operator --fleet "$fleet" --ttl 24h)" \
    || die "could not create an operator invite"
  key="$(grep -o 'fp-oi-[A-Za-z0-9_-]*' <<<"$out" | head -n 1)"
  [[ -n "$key" ]] || die "fleetctl printed no invite key"
  invites+=("$key")
done

# ---- sim fleet --------------------------------------------------------------

(
  cd "$root/sim"
  exec node --import tsx src/cli.ts \
    --url "ws://127.0.0.1:$port/ws" --enroll-key "$enroll_key" \
    --count "$count" --help-rate "$help_rate" --wander --ephemeral
) >"$work/sim.log" 2>&1 &
sim_pid=$!

for _ in $(seq 1 100); do
  grep -q "robots online" "$work/sim.log" 2>/dev/null && break
  kill -0 "$sim_pid" 2>/dev/null || { cat "$work/sim.log" >&2; keep=1; die "the sim exited (log above)"; }
  sleep 0.2
done
grep -q "robots online" "$work/sim.log" || { cat "$work/sim.log" >&2; keep=1; die "the sim robots did not come online"; }

# ---- what to do next --------------------------------------------------------

url="http://localhost:$port/"
echo
echo "Ready in $((SECONDS - started))s: $count sim robots are online and will start asking for help."
echo
echo "  Console:  $url"
if (( lan )); then
  lan_ip="$(node -e '
    const nets = Object.values(require("node:os").networkInterfaces()).flat();
    const a = nets.find((n) => n && n.family === "IPv4" && !n.internal);
    if (a) console.log(a.address);')"
  [[ -n "$lan_ip" ]] && echo "            http://$lan_ip:$port/   (from another machine on this network)"
fi
echo
echo "  Operator invites (each works once; paste one on the sign-in screen):"
i=1
for key in "${invites[@]}"; do
  echo "    operator $i:  $key"
  i=$((i + 1))
done
echo
echo "  Two tabs of one browser profile are the same operator. For two operators use two"
echo "  profiles, or a normal and a private window. Alone on this machine, the second"
echo "  operator can also use http://127.0.0.1:$port/ (a different address signs in separately)."
echo
echo "  Logs: $work/server.log  $work/sim.log"
echo "  Ctrl-C stops the server and the sim$( (( keep )) || echo ' and deletes the database')."
echo

# Stay up until Ctrl-C, or until the server or the sim dies.
while true; do
  kill -0 "$server_pid" 2>/dev/null || { tail -n 20 "$work/server.log" >&2; keep=1; die "fleet-server exited (last log lines above)"; }
  kill -0 "$sim_pid" 2>/dev/null || { tail -n 20 "$work/sim.log" >&2; keep=1; die "the sim exited (last log lines above)"; }
  sleep 1
done
