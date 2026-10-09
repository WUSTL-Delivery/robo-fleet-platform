#!/usr/bin/env bash
# Runs fake_robot.py with settings from examples/.env (copy .env.example to .env first).
# Extra arguments pass through: ./run_fake_robot.sh --name fake-02 --help-after 30
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
sdk="$(dirname "$here")"

if [[ -f "$here/.env" ]]; then
  set -a; source "$here/.env"; set +a
else
  echo "no $here/.env; using defaults (local server). cp .env.example .env to point elsewhere." >&2
fi

# One venv for the SDK, created on first run.
if [[ ! -x "$sdk/.venv/bin/python" ]]; then
  echo "creating $sdk/.venv and installing the SDK..." >&2
  python3 -m venv "$sdk/.venv"
  # With the webrtc extra (twist over a data channel); without it if aiortc will not install here.
  "$sdk/.venv/bin/pip" install -q -e "$sdk[webrtc]" || "$sdk/.venv/bin/pip" install -q -e "$sdk"
fi

export PYTHONUNBUFFERED=1
exec "$sdk/.venv/bin/python" "$here/fake_robot.py" "$@"
