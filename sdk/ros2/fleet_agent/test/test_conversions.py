"""Unit tests for the pure conversions. No ROS needed: `pytest test/test_conversions.py`."""

import math

import pytest

from fleet_agent.conversions import (
    battery_pct,
    clamp,
    finite,
    navsat_fix_quality,
    yaw_from_quaternion,
)


class TestFinite:
    """Nothing non-finite may reach the wire; JSON cannot represent it."""

    @pytest.mark.parametrize("value", [float("nan"), float("inf"), float("-inf"), None])
    def test_rejects_unusable(self, value):
        assert finite(value) is None

    @pytest.mark.parametrize("value", [0.0, -1.5, 38.6488, 1e12])
    def test_passes_real_numbers(self, value):
        assert finite(value) == value

    def test_coerces_ints_and_strings(self):
        assert finite(3) == 3.0
        assert finite("2.5") == 2.5

    def test_rejects_nonsense(self):
        assert finite("north") is None
        assert finite(object()) is None


class TestYawFromQuaternion:
    """Yaw is radians, CCW, 0 = +x (East in ENU)."""

    def test_identity_is_zero(self):
        assert yaw_from_quaternion(0.0, 0.0, 0.0, 1.0) == pytest.approx(0.0)

    @pytest.mark.parametrize("angle", [0.0, 0.5, math.pi / 2, -math.pi / 2, 2.5])
    def test_round_trips_a_yaw_only_rotation(self, angle):
        # A rotation of `angle` about z is (0, 0, sin(a/2), cos(a/2)).
        z, w = math.sin(angle / 2), math.cos(angle / 2)
        assert yaw_from_quaternion(0.0, 0.0, z, w) == pytest.approx(angle)

    def test_normalises_an_unnormalised_quaternion(self):
        angle = 1.2
        scale = 7.0
        z, w = math.sin(angle / 2) * scale, math.cos(angle / 2) * scale
        assert yaw_from_quaternion(0.0, 0.0, z, w) == pytest.approx(angle)

    def test_all_zero_is_unknown_not_east(self):
        # The usual "orientation not set" placeholder. Reporting 0.0 would put a
        # confident heading on the map for a robot that has none.
        assert yaw_from_quaternion(0.0, 0.0, 0.0, 0.0) is None

    def test_nan_is_unknown(self):
        assert yaw_from_quaternion(0.0, 0.0, float("nan"), 1.0) is None

    def test_wraps_to_pi(self):
        yaw = yaw_from_quaternion(0.0, 0.0, 1.0, 0.0)
        assert abs(yaw) == pytest.approx(math.pi)


class TestBatteryPct:
    """REP 147 says 0..1, but drivers disagree, so `auto` reads above 1.0 as percent."""

    def test_auto_scales_a_fraction(self):
        assert battery_pct(0.87) == pytest.approx(87.0)

    def test_auto_passes_a_percentage(self):
        assert battery_pct(87.0) == pytest.approx(87.0)

    def test_auto_boundary_is_treated_as_a_fraction(self):
        assert battery_pct(1.0) == pytest.approx(100.0)

    def test_explicit_percent_does_not_rescale(self):
        assert battery_pct(0.87, scale="percent") == pytest.approx(0.87)

    def test_explicit_fraction_always_rescales(self):
        assert battery_pct(1.0, scale="fraction") == pytest.approx(100.0)

    def test_clamps_to_the_schema_range(self):
        assert battery_pct(140.0) == 100.0
        assert battery_pct(-3.0, scale="percent") == 0.0

    def test_unknown_is_none(self):
        assert battery_pct(float("nan")) is None
        assert battery_pct(None) is None


class TestNavsatFixQuality:
    """RTK from ublox_dgnss arrives as a ground-based augmentation fix."""

    @pytest.mark.parametrize(
        "status,expected",
        [(-1, "no_fix"), (0, "fix"), (1, "sbas"), (2, "gbas")],
    )
    def test_known_statuses(self, status, expected):
        assert navsat_fix_quality(status) == expected

    def test_unknown_status_is_none(self):
        assert navsat_fix_quality(99) is None
        assert navsat_fix_quality(None) is None


class TestClamp:
    """The robot is the last line of defence on its own envelope."""

    def test_passes_values_inside_the_envelope(self):
        assert clamp(0.4, 1.0) == 0.4
        assert clamp(-0.4, 1.0) == -0.4

    def test_clamps_both_directions(self):
        assert clamp(5.0, 1.5) == 1.5
        assert clamp(-5.0, 1.5) == -1.5

    def test_limit_sign_is_ignored(self):
        assert clamp(5.0, -1.5) == 1.5

    def test_non_finite_commands_stop(self):
        # A NaN setpoint must become zero velocity, never an unclamped pass-through.
        assert clamp(float("nan"), 1.0) == 0.0
        assert clamp(float("inf"), 1.0) == 0.0

    def test_zero_limit_pins_to_zero(self):
        assert clamp(2.0, 0.0) == 0.0
