# Dev/test image for sdk/ros2: a ROS 2 toolchain plus the Go toolchain that builds
# fleet-server. It holds NO source: test.sh
# bind-mounts the repo read-only and scripts/in_container.sh builds and tests it,
# so editing code never rebuilds the image.
#
# Build it through ./test.sh, or by hand (the context is this directory):
#   docker build -t fleet-agent-dev:humble --build-arg ROS_DISTRO=humble sdk/ros2
#
# ROS_DISTRO defaults to humble (Ubuntu 22.04, Python 3.10: the robot's stack and
# the SDK's oldest supported Python). jazzy (Ubuntu 24.04, Python 3.12) also works.
ARG ROS_DISTRO=humble
# Must satisfy the `go` line in server/go.mod.
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION} AS go

FROM ros:${ROS_DISTRO}-ros-base

# ros-base already carries rclpy, sensor_msgs, launch and colcon; they are listed
# anyway so the image does not depend on what that metapackage happens to include.
RUN apt-get update && apt-get install -y --no-install-recommends \
      python3-pip \
      python3-pytest \
      python3-colcon-common-extensions \
      ros-${ROS_DISTRO}-rclpy \
      ros-${ROS_DISTRO}-sensor-msgs \
      ros-${ROS_DISTRO}-geometry-msgs \
      ros-${ROS_DISTRO}-std-msgs \
      ros-${ROS_DISTRO}-launch \
      ros-${ROS_DISTRO}-launch-ros \
      ros-${ROS_DISTRO}-launch-testing \
      ros-${ROS_DISTRO}-ros2run \
    && rm -rf /var/lib/apt/lists/*

# in_container.sh installs the Python SDK with pip, the same way a native machine
# does. Ubuntu 24.04 (jazzy) marks the system Python as externally managed; this
# is a throwaway container, so installing into it is fine.
ENV PIP_BREAK_SYSTEM_PACKAGES=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_ROOT_USER_ACTION=ignore \
    PIP_NO_WARN_SCRIPT_LOCATION=1

COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH} \
    GOFLAGS=-buildvcs=false

WORKDIR /work
