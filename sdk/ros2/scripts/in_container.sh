#!/usr/bin/env bash
# Runs INSIDE the dev container (see ../test.sh, which is the command to use).
# Builds fleet-server and the colcon workspace from the repo mounted read-only at
# /repo, then runs every test of every package under sdk/ros2.
#
#   in_container.sh            build + test
#   in_container.sh bash       build, then an interactive shell with the workspace sourced
#   in_container.sh <cmd...>   build, then run <cmd> with the workspace sourced
set -eo pipefail   # no -u: ROS setup scripts read unset variables

REPO=${REPO:-/repo}
WORK=${WORK:-/work}

step() { printf '\n==> %s\n' "$*"; }

# shellcheck disable=SC1090
source "/opt/ros/${ROS_DISTRO}/setup.bash"

# Work on a copy: the mount is read-only, and colcon, pip and pytest all write
# next to the sources. Heavy or host-specific directories are left behind.
step "copying sources from ${REPO}"
rm -rf "${WORK}/repo" "${WORK}/ws"
mkdir -p "${WORK}/repo" "${WORK}/ws/src" "${WORK}/bin"
tar -C "${REPO}" \
    --exclude=.git --exclude=.worktrees --exclude=node_modules --exclude=.venv \
    --exclude=__pycache__ --exclude='*.db' --exclude=./console --exclude=./sim \
    --exclude=./server/bin \
    -cf - . | tar -C "${WORK}/repo" -xf -

step "installing the Python SDK (sdk/python)"
# The same two commands as README.md step 1. Ubuntu 22.04's pip (22.0) cannot build
# the SDK: its isolated build environment still sees the system's old `packaging`.
python3 -m pip install --quiet --user --upgrade pip
python3 -m pip install --quiet --user "${WORK}/repo/sdk/python"

step "building fleet-server ($(go version | cut -d' ' -f3))"
(cd "${WORK}/repo/server" && go build -o "${WORK}/bin/fleet-server" ./cmd/fleet-server)
export FLEET_SERVER_BIN="${WORK}/bin/fleet-server"

step "colcon build (ROS ${ROS_DISTRO})"
ln -s "${WORK}/repo/sdk/ros2" "${WORK}/ws/src/ros2"
cd "${WORK}/ws"
colcon build --event-handlers console_cohesion+
# shellcheck disable=SC1091
source install/setup.bash

if [ "$#" -gt 0 ]; then
    exec "$@"
fi

step "colcon test"
# Launch tests each start their own fleet-server from $FLEET_SERVER_BIN.
status=0
colcon test --event-handlers console_direct+ --return-code-on-test-failure || status=$?
colcon test-result --all --verbose || status=$?
exit "${status}"
