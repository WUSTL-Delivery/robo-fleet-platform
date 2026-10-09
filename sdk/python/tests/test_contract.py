"""Layer 1 contract tests (docs/TESTING.md): the Python side of the cross-language handshake.

Validates every golden fixture in protocol/fixtures against the same JSON Schemas and
catalog.json the Go server uses (sdk/go/protocol/contract_test.go) and the TS
SDK uses (sdk/typescript/test/contract.test.ts): envelope schema, known type, then the
payload schema the catalog maps that type to.

It also checks the generated fleet/protocol.py: that it is current with the schemas,
that its message types match the catalog, and that every valid fixture's payload fits
the generated TypedDict for its type (the Python counterpart of the Go round-trip).
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from typing import Any, Literal, Union, get_args, get_origin

import pytest
from jsonschema import Draft202012Validator
from jsonschema.exceptions import best_match
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

from fleet import protocol

SDK_DIR = Path(__file__).resolve().parents[1]
PROTOCOL_DIR = SDK_DIR.parent.parent / "protocol"
SCHEMA_BASE = "https://fleetplatform.local/v0/"

VALID_DIR = PROTOCOL_DIR / "fixtures" / "valid"
INVALID_DIR = PROTOCOL_DIR / "fixtures" / "invalid"
VALID_FIXTURES = sorted(p.name for p in VALID_DIR.glob("*.json"))
INVALID_FIXTURES = sorted(p.name for p in INVALID_DIR.glob("*.json"))


def read_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


CATALOG = read_json(PROTOCOL_DIR / "catalog.json")


class Validators:
    def __init__(self) -> None:
        schema_files = sorted((PROTOCOL_DIR / "schemas").glob("*.schema.json"))
        if not schema_files:
            raise RuntimeError("no schemas found")
        resources = []
        for f in schema_files:
            doc = read_json(f)
            Draft202012Validator.check_schema(doc)
            # Registered under the same base URI as the Go test, by file name.
            resources.append((SCHEMA_BASE + f.name, Resource.from_contents(doc, default_specification=DRAFT202012)))
        self.registry = Registry().with_resources(resources)

        self.envelope = self.compile(CATALOG["envelope"])
        self.by_type = {typ: self.compile(ref) for typ, ref in CATALOG["messages"].items()}

    def compile(self, ref: str) -> Draft202012Validator:
        uri = SCHEMA_BASE + ref.removeprefix("schemas/")
        self.registry.resolver().lookup(uri)  # raises if the catalog ref does not resolve
        return Draft202012Validator({"$ref": uri}, registry=self.registry)

    def check(self, msg: Any) -> str | None:
        """None when `msg` is a valid envelope with a valid payload, else a reason."""
        err = best_match(self.envelope.iter_errors(msg))
        if err is not None:
            return f"envelope: {err.message}"
        typ = msg["type"]
        v = self.by_type.get(typ)
        if v is None:
            return f"unknown message type: {typ}"
        err = best_match(v.iter_errors(msg["payload"]))
        if err is not None:
            return f"{typ} payload at {err.json_path}: {err.message}"
        return None


@pytest.fixture(scope="module")
def validators() -> Validators:
    return Validators()


def test_finds_fixtures_in_both_directories() -> None:
    assert VALID_FIXTURES
    assert INVALID_FIXTURES


@pytest.mark.parametrize("name", VALID_FIXTURES)
def test_valid_fixture_validates(validators: Validators, name: str) -> None:
    assert validators.check(read_json(VALID_DIR / name)) is None


@pytest.mark.parametrize("name", INVALID_FIXTURES)
def test_invalid_fixture_is_rejected(validators: Validators, name: str) -> None:
    assert validators.check(read_json(INVALID_DIR / name)) is not None, (
        "fixture in fixtures/invalid/ unexpectedly validated"
    )


# ---- catalog coverage ---------------------------------------------------------


def test_sdk_message_types_match_catalog(validators: Validators) -> None:
    assert set(protocol.MESSAGE_TYPES) == set(validators.by_type)
    assert set(protocol.PAYLOAD_TYPES) == set(validators.by_type)
    assert protocol.PROTOCOL_VERSION == CATALOG["v"]


def test_every_valid_fixture_type_is_a_catalog_type(validators: Validators) -> None:
    for name in VALID_FIXTURES:
        typ = read_json(VALID_DIR / name)["type"]
        assert typ in validators.by_type, f"{name}: type {typ}"


# ---- generated types ----------------------------------------------------------


def test_generated_protocol_is_current() -> None:
    proc = subprocess.run(
        [sys.executable, str(SDK_DIR / "scripts" / "gen.py"), "--check"],
        capture_output=True,
        text=True,
    )
    assert proc.returncode == 0, proc.stderr


def _is_typeddict(tp: Any) -> bool:
    return isinstance(tp, type) and issubclass(tp, dict) and hasattr(tp, "__required_keys__")


def _unwrap_not_required(tp: Any) -> Any:
    return tp.__args__[0] if getattr(tp, "__origin__", None) is protocol.NotRequired else tp


def mismatches(value: Any, tp: Any, path: str = "$") -> list[str]:
    """Structural check of a JSON value against a generated type. Empty list = fits."""
    if tp is Any:
        return []
    origin = get_origin(tp)
    if origin is Literal:
        ok = any(value == a and type(value) is type(a) for a in get_args(tp))
        return [] if ok else [f"{path}: {value!r} not in {get_args(tp)}"]
    if origin is Union:
        branches = [mismatches(value, b, path) for b in get_args(tp)]
        return [] if any(not b for b in branches) else [f"{path}: fits no branch of {tp}: {branches}"]
    if origin is list:
        if not isinstance(value, list):
            return [f"{path}: expected list, got {type(value).__name__}"]
        (item,) = get_args(tp)
        return [m for i, v in enumerate(value) for m in mismatches(v, item, f"{path}[{i}]")]
    if origin is dict:
        if not isinstance(value, dict):
            return [f"{path}: expected object, got {type(value).__name__}"]
        _, val = get_args(tp)
        return [m for k, v in value.items() for m in mismatches(v, val, f"{path}.{k}")]
    if _is_typeddict(tp):
        if not isinstance(value, dict):
            return [f"{path}: expected {tp.__name__}, got {type(value).__name__}"]
        fields = tp.__annotations__
        out = [f"{path}: missing required key {k!r} of {tp.__name__}" for k in tp.__required_keys__ if k not in value]
        for k, v in value.items():
            if k not in fields:
                out.append(f"{path}: key {k!r} not declared on {tp.__name__}")
            else:
                out.extend(mismatches(v, _unwrap_not_required(fields[k]), f"{path}.{k}"))
        return out
    if tp is float:
        ok = isinstance(value, (int, float)) and not isinstance(value, bool)
    elif tp is int:
        ok = isinstance(value, int) and not isinstance(value, bool)
    elif tp is type(None) or tp is None:
        ok = value is None
    elif tp in (str, bool):
        ok = isinstance(value, tp)
    else:
        raise AssertionError(f"{path}: test does not understand generated type {tp!r}")
    return [] if ok else [f"{path}: expected {getattr(tp, '__name__', tp)}, got {value!r}"]


@pytest.mark.parametrize("name", VALID_FIXTURES)
def test_valid_fixture_fits_generated_types(name: str) -> None:
    msg = read_json(VALID_DIR / name)
    assert mismatches(msg, protocol.RawEnvelope) == []
    payload_type = protocol.PAYLOAD_TYPES[msg["type"]]
    assert mismatches(msg["payload"], payload_type) == []
