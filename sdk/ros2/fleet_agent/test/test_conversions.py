"""ROS message -> protocol payload mappings, checked without a running graph."""

import math

import pytest
from sensor_msgs.msg import BatteryState, NavSatFix, NavSatStatus

from fleet_agent.conversions import battery_from_state, build_manifest, pose_from_fix


def _fix(lat, lon, alt=float("nan"), status=NavSatStatus.STATUS_FIX):
    fix = NavSatFix()
    fix.status.status = status
    fix.latitude, fix.longitude, fix.altitude = lat, lon, alt
    return fix


def test_manifest_declares_only_what_is_configured():
    assert build_manifest() == {}
    assert build_manifest(battery=True) == {"battery": {}}
    assert build_manifest(drive_type="twist", max_v_mps=1.5, max_w_radps=2.0, battery=True) == {
        "drive": {"type": "twist", "max_v_mps": 1.5, "max_w_radps": 2.0},
        "battery": {},
    }


@pytest.mark.parametrize(
    "kwargs",
    [
        {"drive_type": "ackermann", "max_v_mps": 1.0, "max_w_radps": 1.0},
        {"drive_type": "twist", "max_v_mps": 0.0, "max_w_radps": 1.0},
        {"drive_type": "twist", "max_v_mps": 1.0, "max_w_radps": -1.0},
        {"drive_type": "twist", "max_v_mps": math.nan, "max_w_radps": 1.0},
    ],
)
def test_manifest_rejects_a_drive_the_schema_would_reject(kwargs):
    with pytest.raises(ValueError):
        build_manifest(**kwargs)


def test_fix_becomes_a_geographic_pose():
    assert pose_from_fix(_fix(38.6488, -90.3108, 160.5)) == {
        "frame": "geographic",
        "lat": 38.6488,
        "lon": -90.3108,
        "alt_m": 160.5,
    }
    # Unknown altitude is NaN in ROS; JSON has no NaN, so the field is left out.
    assert pose_from_fix(_fix(38.6488, -90.3108)) == {"frame": "geographic", "lat": 38.6488, "lon": -90.3108}


@pytest.mark.parametrize(
    "fix",
    [
        _fix(38.6488, -90.3108, status=NavSatStatus.STATUS_NO_FIX),
        _fix(float("nan"), -90.3108),
        _fix(38.6488, float("inf")),
        _fix(91.0, 0.0),
        _fix(0.0, 181.0),
    ],
)
def test_unusable_fix_gives_no_pose(fix):
    assert pose_from_fix(fix) is None


def test_battery_fraction_becomes_percent():
    assert battery_from_state(BatteryState(percentage=0.5, voltage=12.0)) == {"pct": 50.0, "voltage": 12.0}
    assert battery_from_state(BatteryState(percentage=float("nan"), voltage=12.0)) == {"voltage": 12.0}
    assert battery_from_state(BatteryState(percentage=1.5, voltage=float("nan"))) == {"pct": 100.0}
    assert battery_from_state(BatteryState(percentage=float("nan"), voltage=float("nan"))) is None
