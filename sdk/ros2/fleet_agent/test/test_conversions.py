"""ROS message -> protocol payload mappings, checked without a running graph."""

import math

import pytest
from sensor_msgs.msg import BatteryState, NavSatFix, NavSatStatus

from fleet_agent.conversions import battery_from_state, build_manifest, clamp_twist, pose_from_fix


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


def test_twist_inside_the_limits_is_unchanged():
    assert clamp_twist(0.4, 0.0, -0.2, max_v_mps=1.0, max_w_radps=1.5) == (0.4, 0.0, -0.2)
    assert clamp_twist(-1.0, 0.0, 1.5, max_v_mps=1.0, max_w_radps=1.5) == (-1.0, 0.0, 1.5)


def test_twist_is_clamped_to_the_drive_limits():
    assert clamp_twist(5.0, 0.0, 9.0, max_v_mps=1.0, max_w_radps=1.5) == (1.0, 0.0, 1.5)
    assert clamp_twist(-5.0, 0.0, -9.0, max_v_mps=1.0, max_w_radps=1.5) == (-1.0, 0.0, -1.5)
    # A holonomic setpoint keeps its direction: the vector is scaled, not each axis clipped.
    x, y, w = clamp_twist(3.0, 4.0, 0.0, max_v_mps=1.0, max_w_radps=1.5)
    assert (x, y, w) == pytest.approx((0.6, 0.8, 0.0))


@pytest.mark.parametrize("bad", [float("nan"), float("inf"), float("-inf")])
def test_non_finite_twist_becomes_a_stop(bad):
    for twist in ((bad, 0.0, 0.0), (0.5, bad, 0.0), (0.5, 0.0, bad)):
        assert clamp_twist(*twist, max_v_mps=1.0, max_w_radps=1.5) == (0.0, 0.0, 0.0)


def test_a_stop_is_plain_zero():
    stop = clamp_twist(-0.0, -0.0, -0.0, max_v_mps=1.0, max_w_radps=1.5)
    assert stop == (0.0, 0.0, 0.0) and not any(math.copysign(1.0, c) < 0 for c in stop)
