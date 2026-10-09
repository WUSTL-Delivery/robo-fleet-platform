"""Pure mappings between ROS data and the fleet protocol. No rclpy, no sockets.

Everything here returns plain dicts shaped by ``protocol/schemas`` (the manifest
in ``defs.schema.json``, telemetry in ``robot.schema.json``) or ``None`` when
there is nothing valid to send. The server validates every message against
those schemas and rejects a bad one, so these functions never emit NaN or an
out-of-range value.

Messages are read by attribute only (duck typed), so the functions work on any
ROS distro's ``sensor_msgs`` and are testable without a running graph.
"""

from __future__ import annotations

import math
from typing import Any, Optional

from fleet import geo_pose

__all__ = ["battery_from_state", "build_manifest", "pose_from_fix"]

#: ``sensor_msgs/NavSatStatus.STATUS_NO_FIX``.
_STATUS_NO_FIX = -1


def build_manifest(
    *,
    drive_type: str = "",
    max_v_mps: float = 0.0,
    max_w_radps: float = 0.0,
    battery: bool = False,
) -> dict[str, Any]:
    """The capability manifest for this robot (``defs.schema.json#/$defs/manifest``).

    The console renders only what the manifest declares, so a capability is
    listed only when the node is configured for it: ``drive`` when ``drive_type``
    is set, ``battery`` when a battery topic is. Raises ValueError for a drive
    the schema would reject, so a bad parameter file fails at startup and not
    as a server error after connecting.
    """
    manifest: dict[str, Any] = {}
    if drive_type:
        if drive_type != "twist":
            raise ValueError(f"drive.type must be 'twist' or empty, got {drive_type!r}")
        for name, value in (("drive.max_v_mps", max_v_mps), ("drive.max_w_radps", max_w_radps)):
            if not math.isfinite(value) or value <= 0:
                raise ValueError(f"{name} must be a number greater than 0 when drive.type is set, got {value!r}")
        manifest["drive"] = {
            "type": "twist",
            "max_v_mps": float(max_v_mps),
            "max_w_radps": float(max_w_radps),
        }
    if battery:
        manifest["battery"] = {}
    return manifest


def pose_from_fix(fix: Any) -> Optional[dict[str, Any]]:
    """A geographic pose from a ``sensor_msgs/NavSatFix``, or None without a usable fix.

    None when the receiver reports no fix or the coordinates are not finite or
    out of range. NavSatFix has no heading, so the pose carries no ``yaw_rad``.
    Altitude is included only when finite (receivers publish NaN for "unknown").
    """
    if fix.status.status == _STATUS_NO_FIX:
        return None
    lat, lon = float(fix.latitude), float(fix.longitude)
    if not (math.isfinite(lat) and math.isfinite(lon)):
        return None
    if not (-90.0 <= lat <= 90.0 and -180.0 <= lon <= 180.0):
        return None
    alt = float(fix.altitude)
    return dict(geo_pose(lat, lon, alt_m=alt if math.isfinite(alt) else None))


def battery_from_state(state: Any) -> Optional[dict[str, Any]]:
    """Telemetry ``battery`` from a ``sensor_msgs/BatteryState``, or None if it says nothing.

    ``BatteryState.percentage`` is a 0..1 fraction (NaN when unmeasured); the
    protocol's ``pct`` is 0..100. A fraction slightly outside 0..1 is clamped.
    ``voltage`` is included when finite.
    """
    battery: dict[str, Any] = {}
    fraction = float(state.percentage)
    if math.isfinite(fraction):
        battery["pct"] = min(100.0, max(0.0, fraction * 100.0))
    voltage = float(state.voltage)
    if math.isfinite(voltage):
        battery["voltage"] = voltage
    return battery or None
