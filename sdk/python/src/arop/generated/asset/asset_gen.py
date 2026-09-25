# Generated from schemas/resources/asset-exchange-v1.schema.json using the
# arop-wire-model-v1 object, const discriminator, and closed-field rules.
from __future__ import annotations

from dataclasses import dataclass
import json
import re
from typing import Literal, TypeAlias

_RUN = re.compile(r"^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")
_ASSET = re.compile(r"^asset_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")
_GRANT = re.compile(r"^grant_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")
_DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")
_MEDIA = re.compile(r"^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$")
_NAME_FORBIDDEN = re.compile(r"(?:^|[\\/])\.\.(?:[\\/]|$)")
_TOKEN = re.compile(r"^agt_[A-Za-z0-9_-]{8,128}$")
_BROKER = re.compile(r"^/v1/asset-content/grant_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")
_TIME = re.compile(r"^[0-9]{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12][0-9]|3[01])T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$")


def _object(value: object, allowed: set[str], required: set[str]) -> dict[str, object]:
    if not isinstance(value, dict) or not all(isinstance(key, str) for key in value):
        raise ValueError("expected object")
    unknown = set(value) - allowed
    if unknown:
        raise ValueError("unknown fields: " + ",".join(sorted(unknown)))
    missing = required - set(value)
    if missing:
        raise ValueError("missing fields: " + ",".join(sorted(missing)))
    return value


def _name(value: object) -> str:
    if not isinstance(value, str) or not 1 <= len(value) <= 512 or _NAME_FORBIDDEN.search(value):
        raise ValueError("invalid name")
    return value


def _media(value: object) -> str:
    if not isinstance(value, str) or _MEDIA.fullmatch(value) is None:
        raise ValueError("invalid media type")
    return value


def _bytes(value: object) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0 or value > 9007199254740991:
        raise ValueError("invalid size")
    return value


def _when(value: object) -> str:
    if not isinstance(value, str) or _TIME.fullmatch(value) is None:
        raise ValueError("invalid date-time")
    return value


@dataclass(slots=True)
class AssetRef:
    asset_id: str
    name: str
    media_type: str
    size_bytes: int
    access_mode: Literal["brokered"]
    digest: str | None = None
    access_expires_at: str | None = None

    @classmethod
    def decode(cls, raw: object) -> "AssetRef":
        value = _object(raw, {"asset_id", "name", "media_type", "size_bytes", "digest", "access"}, {"asset_id", "name", "media_type", "size_bytes", "access"})
        if not isinstance(value["asset_id"], str) or _ASSET.fullmatch(value["asset_id"]) is None:
            raise ValueError("invalid asset id")
        digest = value.get("digest")
        if digest is not None and (not isinstance(digest, str) or _DIGEST.fullmatch(digest) is None):
            raise ValueError("invalid digest")
        access = _object(value["access"], {"mode", "expires_at"}, {"mode"})
        if access.get("mode") != "brokered":
            raise ValueError("invalid access")
        expires = access.get("expires_at")
        if expires is not None:
            expires = _when(expires)
        return cls(value["asset_id"], _name(value["name"]), _media(value["media_type"]), _bytes(value["size_bytes"]), "brokered", digest if isinstance(digest, str) else None, expires)


@dataclass(slots=True)
class UploadRequest:
    kind: Literal["upload_request"]
    run_id: str
    name: str
    media_type: str
    size_bytes: int
    digest: str | None = None


@dataclass(slots=True)
class DownloadRequest:
    kind: Literal["download_request"]
    run_id: str
    asset_id: str


@dataclass(slots=True)
class GrantResponse:
    kind: Literal["grant"]
    asset: AssetRef
    grant_id: str
    method: Literal["upload", "download"]
    broker_path: str
    bearer_token: str
    expires_at: str
    max_uses: int


AssetExchange: TypeAlias = UploadRequest | DownloadRequest | GrantResponse


def decode_asset_exchange(data: str | bytes) -> AssetExchange:
    value = _object(json.loads(data), {"kind", "run_id", "name", "media_type", "size_bytes", "digest", "asset_id", "asset", "grant_id", "method", "broker_path", "bearer_token", "expires_at", "max_uses"}, {"kind"})
    kind = value["kind"]
    if kind == "upload_request":
        body = _object(value, {"kind", "run_id", "name", "media_type", "size_bytes", "digest"}, {"kind", "run_id", "name", "media_type", "size_bytes"})
        if not isinstance(body["run_id"], str) or _RUN.fullmatch(body["run_id"]) is None:
            raise ValueError("invalid run id")
        digest = body.get("digest")
        if digest is not None and (not isinstance(digest, str) or _DIGEST.fullmatch(digest) is None):
            raise ValueError("invalid digest")
        return UploadRequest("upload_request", body["run_id"], _name(body["name"]), _media(body["media_type"]), _bytes(body["size_bytes"]), digest if isinstance(digest, str) else None)
    if kind == "download_request":
        body = _object(value, {"kind", "run_id", "asset_id"}, {"kind", "run_id", "asset_id"})
        if not isinstance(body["run_id"], str) or _RUN.fullmatch(body["run_id"]) is None or not isinstance(body["asset_id"], str) or _ASSET.fullmatch(body["asset_id"]) is None:
            raise ValueError("invalid download request")
        return DownloadRequest("download_request", body["run_id"], body["asset_id"])
    if kind == "grant":
        body = _object(value, {"kind", "asset", "grant_id", "method", "broker_path", "bearer_token", "expires_at", "max_uses"}, {"kind", "asset", "grant_id", "method", "broker_path", "bearer_token", "expires_at", "max_uses"})
        method = body["method"]
        if method not in ("upload", "download"):
            raise ValueError("invalid method")
        if not isinstance(body["grant_id"], str) or _GRANT.fullmatch(body["grant_id"]) is None or not isinstance(body["broker_path"], str) or _BROKER.fullmatch(body["broker_path"]) is None or not isinstance(body["bearer_token"], str) or _TOKEN.fullmatch(body["bearer_token"]) is None:
            raise ValueError("invalid grant")
        uses = body["max_uses"]
        if isinstance(uses, bool) or not isinstance(uses, int) or uses < 1 or uses > 9007199254740991:
            raise ValueError("invalid max uses")
        return GrantResponse("grant", AssetRef.decode(body["asset"]), body["grant_id"], method, body["broker_path"], body["bearer_token"], _when(body["expires_at"]), uses)
    raise ValueError("unknown asset exchange discriminator")
