#!/usr/bin/env bash
# One command to test sdk/ros2 without a local ROS 2 install: builds the dev image,
# then inside it builds fleet-server and the colcon workspace and runs the tests
# (the launch tests start a real fleet-server). Needs only Docker.
#
#   sdk/ros2/test.sh                    # build + test on ROS 2 Humble
#   ROS_DISTRO=jazzy sdk/ros2/test.sh   # same on Jazzy
#   sdk/ros2/test.sh bash               # build, then a shell in the container
#
# CI runs exactly this (.github/workflows/ci.yml, job sdk-ros2).
set -euo pipefail

ROS_DISTRO=${ROS_DISTRO:-humble}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "${HERE}/../.." && pwd)
IMAGE=fleet-agent-dev:${ROS_DISTRO}

docker build -t "${IMAGE}" --build-arg "ROS_DISTRO=${ROS_DISTRO}" "${HERE}"

tty=()
if [ -t 0 ] && [ -t 1 ]; then tty=(-it); fi

# The repo is mounted read-only; the container works on its own copy. The named
# volume only caches Go modules and build output between runs.
exec docker run --rm ${tty[@]+"${tty[@]}"} \
    -v "${REPO}:/repo:ro" \
    -v fleet-agent-dev-go:/root/go \
    -e GOCACHE=/root/go/build-cache \
    "${IMAGE}" /repo/sdk/ros2/scripts/in_container.sh "$@"
