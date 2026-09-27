"""Redirect-proof Worker Pull HTTP client."""

from __future__ import annotations

import json
import re
from typing import Mapping, Protocol
from urllib.parse import urlsplit

from arop._transport import (
    AsyncTransport,
    HTTPSJSONTransport,
    Response,
    TransportUnavailable,
    one_header,
)
from arop.generated.worker import worker_gen as wire

_WORKER = re.compile(r"^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$")
_CLAIM = re.compile(r"^clm_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")
_KEY = re.compile(r"^[A-Za-z0-9._~-]{16,200}$")


class CredentialSource(Protocol):
    async def credential(self) -> str: ...


class WorkerRemoteError(Exception):
    def __init__(
        self,
        status: int,
        category: str,
        code: str,
        retryable: bool,
        retry_after_seconds: int | None,
    ) -> None:
        super().__init__(f"worker request failed: status={status} code={code}")
        self.status = status
        self.category = category
        self.code = code
        self.retryable = retryable
        self.retry_after_seconds = retry_after_seconds


class WorkerClient:
    def __init__(
        self,
        base_url: str,
        credential_source: CredentialSource,
        *,
        transport: AsyncTransport | None = None,
        timeout: float = 35.0,
    ) -> None:
        parsed = urlsplit(base_url)
        if (
            parsed.scheme != "https"
            or not parsed.hostname
            or parsed.username is not None
            or parsed.path not in {"", "/"}
            or parsed.query
            or parsed.fragment
            or timeout < 1
            or timeout > 300
        ):
            raise ValueError("invalid worker client configuration")
        self.base_url = base_url.rstrip("/")
        self.credentials = credential_source
        self.transport = transport or HTTPSJSONTransport()
        self.timeout = timeout

    async def claim(
        self, worker_id: str, request: wire.WorkerClaimRequest
    ) -> wire.WorkerClaim | None:
        self._worker(worker_id)
        response = await self._request(
            "POST",
            f"/v1/workers/{worker_id}/claims:next",
            validated(
                request,
                wire.encode_worker_claim_request,
                wire.decode_worker_claim_request,
            ),
        )
        if response.status == 204:
            if response.body:
                raise TransportUnavailable("invalid empty worker response")
            return None
        if response.status != 200:
            raise decode_error(response)
        if (
            one_header(response, "cache-control") != "no-store"
            or one_header(response, "content-type") != "application/json"
        ):
            raise TransportUnavailable("invalid worker cache response")
        claim = wire.decode_worker_claim(response.body)
        if (
            str(claim.worker_id) != worker_id
            or str(claim.session_id) != str(request.session_id)
            or int(claim.generation) != int(request.generation)
        ):
            raise TransportUnavailable("invalid worker claim response")
        return claim

    async def renew(
        self, worker_id: str, claim_id: str, request: wire.WorkerRenewRequest
    ) -> wire.WorkerClaim:
        self._target(worker_id, claim_id)
        response = await self._request(
            "POST",
            f"/v1/workers/{worker_id}/claims/{claim_id}:renew",
            validated(
                request,
                wire.encode_worker_renew_request,
                wire.decode_worker_renew_request,
            ),
        )
        if response.status != 200:
            raise decode_error(response)
        if (
            one_header(response, "cache-control") != "no-store"
            or one_header(response, "content-type") != "application/json"
        ):
            raise TransportUnavailable("invalid worker cache response")
        claim = wire.decode_worker_claim(response.body)
        if (
            str(claim.worker_id) != worker_id
            or str(claim.claim_id) != claim_id
            or str(claim.lease_token) != str(request.lease_token)
            or int(claim.fencing_token) != int(request.fencing_token)
        ):
            raise TransportUnavailable("invalid worker renew response")
        return claim

    async def complete(
        self,
        worker_id: str,
        claim_id: str,
        idempotency_key: str,
        request: wire.WorkerComplete,
    ) -> None:
        self._target(worker_id, claim_id)
        if not _KEY.fullmatch(idempotency_key) or str(request.claim_id) != claim_id:
            raise ValueError("invalid worker completion target")
        response = await self._request(
            "POST",
            f"/v1/workers/{worker_id}/claims/{claim_id}:complete",
            validated(request, wire.encode_worker_complete, wire.decode_worker_complete),
            {"Idempotency-Key": idempotency_key},
        )
        if response.status != 204:
            raise decode_error(response)
        if response.body:
            raise TransportUnavailable("invalid empty worker response")

    async def release(
        self, worker_id: str, claim_id: str, request: wire.WorkerReleaseRequest
    ) -> None:
        self._target(worker_id, claim_id)
        response = await self._request(
            "POST",
            f"/v1/workers/{worker_id}/claims/{claim_id}:release",
            validated(
                request,
                wire.encode_worker_release_request,
                wire.decode_worker_release_request,
            ),
        )
        if response.status != 204:
            raise decode_error(response)
        if response.body:
            raise TransportUnavailable("invalid empty worker response")

    async def _request(
        self,
        method: str,
        path: str,
        body: bytes,
        additional: Mapping[str, str] | None = None,
    ) -> Response:
        credential = await self.credentials.credential()
        if not valid_credential(credential):
            raise TransportUnavailable("credential unavailable")
        headers = {
            "Authorization": "Bearer " + credential,
            "Accept": "application/json",
            "Content-Type": "application/json",
        }
        if additional:
            headers.update(additional)
        return await self.transport.request(
            method, self.base_url + path, headers, body, self.timeout
        )

    @staticmethod
    def _worker(worker_id: str) -> None:
        if not _WORKER.fullmatch(worker_id):
            raise ValueError("invalid worker id")

    @classmethod
    def _target(cls, worker_id: str, claim_id: str) -> None:
        cls._worker(worker_id)
        if not _CLAIM.fullmatch(claim_id):
            raise ValueError("invalid claim id")


def decode_error(response: Response) -> WorkerRemoteError:
    try:
        if one_header(response, "content-type") != "application/json":
            raise ValueError
        value = strict_object(response.body)
        category = value["category"]
        code = value["code"]
        retryable = value["retryable"]
        retry = value.get("retry_after_seconds")
        if (
            not isinstance(category, str)
            or not isinstance(code, str)
            or not isinstance(retryable, bool)
            or (retry is not None and (isinstance(retry, bool) or not isinstance(retry, int)))
        ):
            raise ValueError
        if response.status == 401 and not one_header(response, "www-authenticate").startswith("Bearer"):
            raise ValueError
        header_retry = one_header(response, "retry-after", required=False)
        if header_retry and (not header_retry.isdigit() or retry != int(header_retry)):
            raise ValueError
        return WorkerRemoteError(response.status, category, code, retryable, retry)
    except (KeyError, ValueError, TransportUnavailable):
        raise TransportUnavailable("invalid worker error response") from None


def strict_object(data: bytes) -> dict[str, object]:
    def pairs(values: list[tuple[str, object]]) -> dict[str, object]:
        output: dict[str, object] = {}
        for key, value in values:
            if key in output:
                raise ValueError("duplicate JSON key")
            output[key] = value
        return output

    value = json.loads(data, object_pairs_hook=pairs)
    if not isinstance(value, dict):
        raise ValueError("expected object")
    return value


def valid_credential(value: str) -> bool:
    return bool(value) and len(value) <= 8192 and all(33 <= ord(character) <= 126 for character in value)


def validated(value, encode, decode) -> bytes:
    encoded = encode(value).encode()
    decode(encoded)
    return encoded
