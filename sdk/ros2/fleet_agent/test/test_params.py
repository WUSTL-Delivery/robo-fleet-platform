"""The node's parameters, checked by constructing it: nothing connects until start().

Covers what a parameter file can get wrong at startup (the process exits with
status 2 instead of failing later on the wire) and the ``data_channel`` switch.
"""

import importlib.util
import os

import pytest
import rclpy
from rclpy.parameter import Parameter

from fleet_agent.node import FleetAgent

HAVE_AIORTC = importlib.util.find_spec("aiortc") is not None


@pytest.fixture(scope="module")
def ros():
    rclpy.init()
    yield
    rclpy.shutdown()


@pytest.fixture
def agent(ros, tmp_path):
    made = []

    def make(**parameters):
        base = {"name": "params-bot", "token_file": str(tmp_path / "token.json")}
        overrides = [Parameter(k, value=v) for k, v in {**base, **parameters}.items()]
        made.append(FleetAgent(parameter_overrides=overrides))
        return made[-1]

    yield make
    for node in made:
        node.destroy_node()


DRIVE = {"drive.type": "twist", "drive.max_v_mps": 1.0, "drive.max_w_radps": 1.5}


def test_data_channel_auto_follows_whether_aiortc_is_installed(agent):
    node = agent(**DRIVE)  # auto is the default
    assert node._robot.data_channel_enabled is HAVE_AIORTC


def test_the_harness_runs_with_the_webrtc_extra():
    # scripts/in_container.sh sets this when it installed the SDK with [webrtc], so
    # the deadman tests in this run are known to have run with the data channel on.
    if os.environ.get("FLEET_AGENT_EXPECT_WEBRTC") != "1":
        pytest.skip("not told to expect the webrtc extra")
    assert HAVE_AIORTC


def test_data_channel_off_keeps_twist_on_the_bus(agent):
    assert agent(**DRIVE, data_channel="off")._robot.data_channel_enabled is False


def test_channels_become_topics_a_service_and_a_manifest_entry(agent):
    node = agent(channels=["jobs", "delivery_status"], raw_channels=["jobs"])
    assert node._robot.manifest == {"channels": ["jobs", "delivery_status"]}
    topics = dict(node.get_topic_names_and_types())
    for name in ("jobs", "delivery_status"):
        for end in ("in", "out"):
            assert topics[f"/fleet/ch/{name}/{end}"] == ["fleet_agent_msgs/msg/ChannelMsg"]
    services = dict(node.get_service_names_and_types())
    assert services["/fleet/request_help"] == ["fleet_agent_msgs/srv/RequestHelp"]


def test_no_channels_is_the_default(agent):
    node = agent()
    assert node._robot.manifest == {}
    assert node._in_pubs == {}


@pytest.mark.parametrize(
    "parameters",
    [
        {"data_channel": "on"},
        {"channels": ["edge.report"]},
        {"channels": ["jobs", "jobs"]},
        {"channels": ["jobs"], "raw_channels": ["status"]},
    ],
)
def test_bad_parameters_fail_at_startup(ros, tmp_path, parameters):
    overrides = [Parameter(k, value=v) for k, v in {"token_file": str(tmp_path / "t.json"), **parameters}.items()]
    with pytest.raises(ValueError):
        FleetAgent(parameter_overrides=overrides)
