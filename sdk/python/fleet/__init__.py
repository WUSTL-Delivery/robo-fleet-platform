"""Python SDK for fleet-server.

A robot (or a service) dials out to the server, enrolls once with a fleet key,
and from then on says hello with the same token on every connect. See
docs/INTEGRATION.md for the wire protocol this package wraps.
"""

from .channel import (
    CHANNEL_NAME_PATTERN,
    DEFAULT_NOT_FOUND_WINDOW,
    Channel,
    ChannelMessage,
    ChannelTargetNotFound,
)
from .client import (
    PROTOCOL_VERSION,
    Backoff,
    ClientKind,
    ConnectionState,
    Envelope,
    FleetClient,
    FleetClientError,
    StateChange,
)
from .token_store import Credentials, FileTokenStore, MemoryTokenStore, TokenStore

__version__ = "0.0.1"

__all__ = [
    "CHANNEL_NAME_PATTERN",
    "DEFAULT_NOT_FOUND_WINDOW",
    "PROTOCOL_VERSION",
    "Backoff",
    "Channel",
    "ChannelMessage",
    "ChannelTargetNotFound",
    "ClientKind",
    "ConnectionState",
    "Credentials",
    "Envelope",
    "FileTokenStore",
    "FleetClient",
    "FleetClientError",
    "MemoryTokenStore",
    "StateChange",
    "TokenStore",
]
