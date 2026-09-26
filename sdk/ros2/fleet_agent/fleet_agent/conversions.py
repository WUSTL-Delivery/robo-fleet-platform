"""Pure conversions between ROS sensor values and platform telemetry values.

Deliberately free of ROS imports: every function here takes plain numbers, so the unit
tests run under bare ``pytest`` with no ROS installation. ``agent_node`` unpacks the
messages and calls in here.

Two rules this module exists to enforce:

1. **Nothing non-finite reaches the wire.** A NavSatFix with no lock carries NaN, and
   ``json.dumps`` happily emits a bare ``NaN`` token that is not valid JSON and that the
   server's schema rejects. Every value goes through :func:`finite` first.
2. **Angles are radians, CCW, 0 = East** (docs/INTEGRATION.md:564), which is what an ENU
   ``odom``/``map`` frame already gives you. Degrees-from-north headings do not belong
   here; convert them at the driver.
"""

from __future__ import annotations

import math

__all__ = [
    "battery_pct",
    "clamp",
    "finite",
    "navsat_fix_quality",
    "yaw_from_quaternion",
]


def finite(value: float | None) -> float | None:
    """Return the value as a float, or None if it is missing, NaN or infinite.

    Telemetry is optional field by field, so dropping a bad reading is always better
    than sending one: the console simply shows nothing for it.
    """
    if value is None:
        return None
    try:
        out = float(value)
    except (TypeError, ValueError):
        return None
    return out if math.isfinite(out) else None


def yaw_from_quaternion(x: float, y: float, z: float, w: float) -> float | None:
    """Yaw in radians (CCW, 0 = +x) from a quaternion, or None if it is unusable.

    An all-zero quaternion is the usual "orientation not set" placeholder, and
    normalising it would divide by zero, so it reads as None rather than 0.0 -- a robot
    with unknown heading should show no heading, not a confident East.
    """
    parts = [finite(v) for v in (x, y, z, w)]
    if any(p is None for p in parts):
        return None
    qx, qy, qz, qw = parts  # type: ignore[misc]
    norm = math.sqrt(qx * qx + qy * qy + qz * qz + qw * qw)
    if norm < 1e-9:
        return None
    qx, qy, qz, qw = qx / norm, qy / norm, qz / norm, qw / norm
    siny_cosp = 2.0 * (qw * qz + qx * qy)
    cosy_cosp = 1.0 - 2.0 * (qy * qy + qz * qz)
    return math.atan2(siny_cosp, cosy_cosp)


def battery_pct(percentage: float | None, scale: str = "auto") -> float | None:
    """``sensor_msgs/BatteryState.percentage`` as 0-100, or None if unknown.

    REP 147 declares ``percentage`` as a 0..1 fraction, but drivers disagree often
    enough that ``"auto"`` treats anything above 1.0 as already being a percentage.
    Force it with ``scale="fraction"`` or ``scale="percent"`` when you know your driver.
    The result is clamped to the schema's 0-100 range.
    """
    value = finite(percentage)
    if value is None:
        return None
    if scale == "fraction" or (scale == "auto" and value <= 1.0):
        value *= 100.0
    return max(0.0, min(100.0, value))


def navsat_fix_quality(status: int | None) -> str | None:
    """``sensor_msgs/NavSatStatus.status`` as a health string, or None if unrecognised.

    ublox_dgnss reports an RTK solution as a ground-based augmentation fix, so ``"gbas"``
    is the one to look for on the club robot. This lands in ``telemetry.health``, which is
    a free-form map: it is diagnostic, not platform vocabulary, and no platform behaviour
    keys off it.
    """
    return {-1: "no_fix", 0: "fix", 1: "sbas", 2: "gbas"}.get(status)  # type: ignore[arg-type]


def clamp(value: float, limit: float) -> float:
    """Clamp to +/- abs(limit); a non-finite value clamps to 0.0.

    The manifest tells the console the robot's limits and the server checks them, but the
    robot is the last line of defence on its own envelope and does not assume either one
    is honest.
    """
    out = finite(value)
    if out is None:
        return 0.0
    bound = abs(finite(limit) or 0.0)
    return max(-bound, min(bound, out))
