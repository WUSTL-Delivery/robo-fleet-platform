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

import json
import math
import re
from typing import Any, Optional, Sequence

from fleet import CHANNEL_NAME_PATTERN, geo_pose

__all__ = [
    "battery_from_state",
    "build_manifest",
    "channel_names",
    "clamp_twist",
    "decode_channel_data",
    "encode_channel_data",
    "help_context",
    "pose_from_fix",
]

#: A channel name that is also one token of a ROS topic name: it starts with a
#: letter and has no '.', no '-', no doubled and no trailing underscore.
_TOPIC_TOKEN = re.compile(r"^[a-z][a-z0-9]*(_[a-z0-9]+)*$")

#: ``sensor_msgs/NavSatStatus.STATUS_NO_FIX``.
_STATUS_NO_FIX = -1


def build_manifest(
    *,
    drive_type: str = "",
    max_v_mps: float = 0.0,
    max_w_radps: float = 0.0,
    battery: bool = False,
    channels: Sequence[str] = (),
) -> dict[str, Any]:
    """The capability manifest for this robot (``defs.schema.json#/$defs/manifest``).

    The console renders only what the manifest declares, so a capability is
    listed only when the node is configured for it: ``drive`` when ``drive_type``
    is set, ``battery`` when a battery topic is, ``channels`` when any are bridged
    (declaring a channel is what makes its broadcasts reach this robot). Raises ValueError for a drive
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
    if channels:
        manifest["channels"] = list(channels)
    return manifest


def channel_names(channels: Sequence[str], raw_channels: Sequence[str] = ()) -> tuple[list[str], set[str]]:
    """The ``channels`` and ``raw_channels`` parameters, checked: (names in order, the raw ones).

    A name becomes part of two topic names (``fleet/ch/<name>/in`` and ``/out``),
    so it must be valid twice: as a protocol channel name and as a ROS topic
    token. The protocol also allows a leading digit, '.' and '-'; ROS does not,
    and the node does not rename channels behind the application's back. Raises
    ValueError for a name that is not both, a duplicate, or a raw channel that
    is not bridged at all.
    """
    names: list[str] = []
    for name in channels:
        if not isinstance(name, str) or not CHANNEL_NAME_PATTERN.match(name) or not _TOPIC_TOKEN.match(name):
            raise ValueError(
                f"channels: {name!r} cannot be bridged; a name must start with a lowercase letter and "
                "hold only lowercase letters, digits and single underscores (at most 64 characters)"
            )
        if name in names:
            raise ValueError(f"channels: {name!r} is listed twice")
        names.append(name)
    for name in raw_channels:
        if name not in names:
            raise ValueError(f"raw_channels: {name!r} is not in channels")
    return names, set(raw_channels)


def encode_channel_data(data: Any) -> str:
    """Channel data as the JSON text carried in ``ChannelMsg.data``."""
    return json.dumps(data, separators=(",", ":"), ensure_ascii=False)


def _no_constant(name: str) -> Any:
    raise ValueError(f"{name} is not JSON")


def decode_channel_data(text: str) -> Any:
    """The JSON value in ``ChannelMsg.data``. Raises ValueError if it is not JSON.

    NaN and Infinity are refused although Python's parser accepts them: they
    are not JSON, and the server would reject the publish.
    """
    return json.loads(text, parse_constant=_no_constant)


def help_context(text: str) -> Optional[dict[str, Any]]:
    """``RequestHelp.context`` as the help.request context: None when empty.

    Raises ValueError unless it is the JSON text of an object, which is the only
    shape the protocol accepts.
    """
    if not text.strip():
        return None
    try:
        context = decode_channel_data(text)
    except ValueError as e:
        raise ValueError(f"context is not JSON ({e})") from None
    if not isinstance(context, dict):
        raise ValueError("context must be the JSON text of an object, for example {\"attempts\": 3}")
    return context


def clamp_twist(
    x_mps: float, y_mps: float, w_radps: float, *, max_v_mps: float, max_w_radps: float
) -> tuple[float, float, float]:
    """An operator setpoint limited to the manifest's drive limits: (x, y, angular z).

    The planar linear velocity is scaled down as a vector when its magnitude
    exceeds ``max_v_mps``, so the direction of travel is kept; the yaw rate is
    clamped to ``max_w_radps`` either way. A setpoint with any non-finite
    component is not a setpoint: it becomes a stop.
    """
    x, y, w = float(x_mps), float(y_mps), float(w_radps)
    if not (math.isfinite(x) and math.isfinite(y) and math.isfinite(w)):
        return 0.0, 0.0, 0.0
    speed = math.hypot(x, y)
    if speed > max_v_mps:
        scale = max_v_mps / speed
        x, y = x * scale, y * scale
    w = min(max_w_radps, max(-max_w_radps, w))
    # "+ 0.0" turns -0.0 into 0.0, so a stop compares equal to (0.0, 0.0, 0.0).
    return x + 0.0, y + 0.0, w + 0.0


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
