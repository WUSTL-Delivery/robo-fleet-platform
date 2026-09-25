"""Where a client keeps the credentials enrollment handed it.

A token is minted once by ``enroll.request`` and must survive restarts: the
server never shows it again, and re-enrolling mints a NEW identity (a new
client_id). So the client loads from a :class:`TokenStore` before connecting and
only enrolls when the store is empty.
"""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Protocol, runtime_checkable


@dataclass(frozen=True)
class Credentials:
    """What enrollment hands back and every later ``hello`` needs."""

    token: str
    client_id: str
    fleet_id: str

    def to_json(self) -> str:
        return json.dumps(asdict(self))

    @classmethod
    def from_json(cls, raw: str | bytes) -> Credentials | None:
        """Parses stored JSON; None unless it has the three string fields."""
        try:
            v = json.loads(raw)
        except (ValueError, UnicodeDecodeError):
            return None
        if not isinstance(v, dict):
            return None
        fields = (v.get("token"), v.get("client_id"), v.get("fleet_id"))
        if not all(isinstance(f, str) for f in fields):
            return None
        return cls(*fields)  # type: ignore[arg-type]


@runtime_checkable
class TokenStore(Protocol):
    """Persistence for :class:`Credentials`. ``load`` returns None when empty."""

    def load(self) -> Credentials | None: ...

    def save(self, credentials: Credentials) -> None: ...

    def clear(self) -> None: ...


class MemoryTokenStore:
    """Keeps credentials in memory only: lost when the process exits."""

    def __init__(self, initial: Credentials | None = None) -> None:
        self._credentials = initial

    def load(self) -> Credentials | None:
        return self._credentials

    def save(self, credentials: Credentials) -> None:
        self._credentials = credentials

    def clear(self) -> None:
        self._credentials = None


class FileTokenStore:
    """Keeps credentials in one JSON file, readable only by its owner (mode 0600).

    Writes are atomic: the new contents go to a temp file in the same directory,
    which is fsynced and then renamed over the old one, so a crash mid-write
    never leaves a truncated token behind. A missing, unreadable or corrupt file
    reads as "nothing stored". Parent directories are created (mode 0700) on save.
    """

    def __init__(self, path: str | os.PathLike[str]) -> None:
        self.path = Path(path).expanduser()

    def load(self) -> Credentials | None:
        try:
            raw = self.path.read_bytes()
        except (FileNotFoundError, IsADirectoryError, PermissionError):
            return None
        return Credentials.from_json(raw)

    def save(self, credentials: Credentials) -> None:
        directory = self.path.parent
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        # mkstemp creates the file with mode 0600 already.
        fd, tmp = tempfile.mkstemp(prefix=f".{self.path.name}.", suffix=".tmp", dir=directory)
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                f.write(credentials.to_json())
                f.flush()
                os.fsync(f.fileno())
            os.chmod(tmp, 0o600)
            os.replace(tmp, self.path)
        except BaseException:
            try:
                os.unlink(tmp)
            except FileNotFoundError:
                pass
            raise
        _fsync_dir(directory)

    def clear(self) -> None:
        try:
            self.path.unlink()
        except FileNotFoundError:
            pass


def _fsync_dir(directory: Path) -> None:
    """Makes the rename durable where the platform allows it (not on Windows)."""
    try:
        fd = os.open(directory, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    except OSError:
        pass
    finally:
        os.close(fd)
