"""Token stores: pure logic, no server."""

from __future__ import annotations

import os
import stat

from fleet import Credentials, FileTokenStore, MemoryTokenStore, TokenStore

CREDS = Credentials(token="fp-tk-abc", client_id="r_1", fleet_id="f_1")


def test_memory_store_round_trip():
    s = MemoryTokenStore()
    assert isinstance(s, TokenStore)
    assert s.load() is None
    s.save(CREDS)
    assert s.load() == CREDS
    s.clear()
    assert s.load() is None


def test_file_store_round_trip_mode_0600_and_creates_parents(tmp_path):
    path = tmp_path / "a" / "b" / "creds.json"
    s = FileTokenStore(path)
    assert isinstance(s, TokenStore)
    assert s.load() is None
    s.save(CREDS)
    assert stat.S_IMODE(path.stat().st_mode) == 0o600
    assert FileTokenStore(path).load() == CREDS
    s.clear()
    s.clear()  # idempotent
    assert s.load() is None


def test_file_store_overwrite_is_atomic_and_leaves_no_temp_files(tmp_path):
    path = tmp_path / "creds.json"
    s = FileTokenStore(path)
    s.save(CREDS)
    os.chmod(path, 0o644)  # a replace must not inherit a looser mode
    newer = Credentials(token="fp-tk-new", client_id="r_1", fleet_id="f_1")
    s.save(newer)
    assert s.load() == newer
    assert stat.S_IMODE(path.stat().st_mode) == 0o600
    assert sorted(p.name for p in tmp_path.iterdir()) == ["creds.json"]


def test_file_store_corrupt_or_foreign_reads_as_empty(tmp_path):
    path = tmp_path / "creds.json"
    for raw in ["not json", "[]", '{"token": 1, "client_id": "r", "fleet_id": "f"}', '{"token": "t"}']:
        path.write_text(raw)
        assert FileTokenStore(path).load() is None
