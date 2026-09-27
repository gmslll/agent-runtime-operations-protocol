"""Fenced runtime registration, keepalive and graceful drain."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
import json
import random
import re
from typing import Awaitable, Callable, Mapping, Protocol
from urllib.parse import urlencode, urlsplit

from arop._transport import AsyncTransport, HTTPSJSONTransport, Response, TransportUnavailable
from arop.generated.registry import registry_gen as wire

_IDENTIFIER = re.compile(r"^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$")
_SESSION = re.compile(
    r"^ses_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
_IDEMPOTENCY = re.compile(r"^[!-~]{8,200}$")
_FIXTURE_UUID = "018f5f6e-7b1c-7abc-8def-0123456789ab"


class CredentialSource(Protocol):
    async def credential(self) -> str: ...


class RegistrationError(Exception):
    def __init__(self, status: int, code: str, retryable: bool) -> None:
        super().__init__(f"registration failed: status={status} code={code}")
        self.status = status
        self.code = code
        self.retryable = retryable


@dataclass(frozen=True, slots=True)
class RegistrationState:
    instance_id: str
    session_id: str
    lease_id: str
    generation: int
    heartbeat_sequence: int
    keepalive_interval_seconds: int
    lease_ttl_seconds: int


class RegistrationClient:
    def __init__(
        self,
        base_url: str,
        credential_source: CredentialSource,
        *,
        transport: AsyncTransport | None = None,
        timeout: float = 30.0,
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
            raise ValueError("invalid registration client configuration")
        self.base_url = base_url.rstrip("/")
        self.credentials = credential_source
        self.transport = transport or HTTPSJSONTransport()
        self.timeout = timeout

    async def register(
        self, instance_id: str, idempotency_key: str, request: Mapping[str, object]
    ) -> RegistrationState:
        if not _IDENTIFIER.fullmatch(instance_id) or not _IDEMPOTENCY.fullmatch(
            idempotency_key
        ):
            raise ValueError("invalid registration target")
        _validate_register_request(instance_id, request)
        response = await self._request(
            "PUT",
            f"/v1/registry/instances/{instance_id}",
            request,
            {"Idempotency-Key": idempotency_key},
        )
        if response.status not in {200, 201}:
            raise decode_error(response)
        if response.headers.get("content-type") != ("application/json",):
            raise TransportUnavailable("invalid registration response")
        value = strict_object(response.body)
        if set(value) != {
            "schema_version",
            "instance",
            "lease_ttl_seconds",
            "keepalive_interval_seconds",
            "replay",
        }:
            raise TransportUnavailable("invalid registration response")
        if value.get("schema_version") != 1 or not isinstance(value.get("replay"), bool):
            raise TransportUnavailable("invalid registration response")
        instance = value.get("instance")
        if not isinstance(instance, dict):
            raise TransportUnavailable("invalid registration response")
        encoded = json.dumps(instance, separators=(",", ":"), ensure_ascii=False)
        decoded = wire.decode_runtime_instance(encoded)
        if (
            str(decoded.instance_id) != instance_id
            or str(decoded.session_id) != request.get("session_id")
        ):
            raise TransportUnavailable("invalid registration response")
        ttl = safe_int(value.get("lease_ttl_seconds"), 1, 600)
        interval = safe_int(value.get("keepalive_interval_seconds"), 1, 599)
        if interval >= ttl:
            raise TransportUnavailable("invalid registration response")
        return RegistrationState(
            instance_id,
            str(decoded.session_id),
            str(decoded.lease_id),
            int(decoded.generation),
            0,
            interval,
            ttl,
        )

    async def keepalive(
        self,
        state: RegistrationState,
        *,
        ready: bool,
        active_runs: int,
        available_slots: int,
        queue_depth: int,
    ) -> RegistrationState:
        sequence = state.heartbeat_sequence + 1
        request = {
            "schema_version": 1,
            "instance_id": state.instance_id,
            "session_id": state.session_id,
            "generation": state.generation,
            "heartbeat_sequence": sequence,
            "reported_at": timestamp(datetime.now(UTC)),
            "ready": ready,
            "active_runs": safe_int(active_runs, 0, 9_007_199_254_740_991),
            "available_slots": safe_int(available_slots, 0, 9_007_199_254_740_991),
            "queue_depth": safe_int(queue_depth, 0, 9_007_199_254_740_991),
        }
        response = await self._request(
            "POST", f"/v1/registry/leases/{state.lease_id}/keepalive", request
        )
        if response.status != 200:
            raise decode_error(response)
        if response.headers.get("content-type") != ("application/json",):
            raise TransportUnavailable("invalid keepalive response")
        lease = wire.decode_registry_lease(response.body)
        if (
            str(lease.instance_id) != state.instance_id
            or str(lease.session_id) != state.session_id
            or str(lease.lease_id) != state.lease_id
            or int(lease.generation) != state.generation
            or int(lease.heartbeat_sequence) != sequence
        ):
            raise TransportUnavailable("invalid keepalive response")
        return RegistrationState(
            state.instance_id,
            state.session_id,
            state.lease_id,
            state.generation,
            sequence,
            state.keepalive_interval_seconds,
            state.lease_ttl_seconds,
        )

    async def drain(self, state: RegistrationState, deadline: datetime) -> None:
        response = await self._request(
            "POST",
            f"/v1/registry/instances/{state.instance_id}/drain",
            {
                "schema_version": 1,
                "session_id": state.session_id,
                "lease_id": state.lease_id,
                "generation": state.generation,
                "deadline_at": timestamp(deadline),
            },
        )
        if response.status != 200:
            raise decode_error(response)
        if response.headers.get("content-type") != ("application/json",):
            raise TransportUnavailable("invalid drain response")
        instance = wire.decode_runtime_instance(response.body)
        if (
            not instance.draining
            or str(instance.instance_id) != state.instance_id
            or str(instance.session_id) != state.session_id
            or str(instance.lease_id) != state.lease_id
            or int(instance.generation) != state.generation
        ):
            raise TransportUnavailable("invalid drain response")

    async def deregister(self, state: RegistrationState) -> None:
        query = urlencode(
            {
                "session_id": state.session_id,
                "lease_id": state.lease_id,
                "generation": state.generation,
            }
        )
        response = await self._request(
            "DELETE", f"/v1/registry/instances/{state.instance_id}?{query}", None
        )
        if response.status == 404:
            decode_error(response)
            return
        if response.status != 200:
            raise decode_error(response)
        if response.headers.get("content-type") != ("application/json",):
            raise TransportUnavailable("invalid deregistration response")
        instance = wire.decode_runtime_instance(response.body)
        if (
            str(instance.instance_id) != state.instance_id
            or str(instance.session_id) != state.session_id
            or str(instance.lease_id) != state.lease_id
            or int(instance.generation) != state.generation
            or str(instance.status) != "deregistered"
        ):
            raise TransportUnavailable("invalid deregistration response")

    async def _request(
        self,
        method: str,
        path: str,
        body: Mapping[str, object] | None,
        additional: Mapping[str, str] | None = None,
    ) -> Response:
        credential = await self.credentials.credential()
        if not valid_credential(credential):
            raise TransportUnavailable("credential unavailable")
        encoded = b"" if body is None else canonical_json(body)
        headers = {
            "Authorization": "Bearer " + credential,
            "Accept": "application/json",
        }
        if body is not None:
            headers["Content-Type"] = "application/json"
        if additional:
            headers.update(additional)
        return await self.transport.request(
            method, self.base_url + path, headers, encoded, self.timeout
        )


class RegistrationLoop:
    def __init__(
        self,
        client: RegistrationClient,
        instance_id: str,
        request_factory: Callable[[], Mapping[str, object]],
        capacity: Callable[[], tuple[bool, int, int, int]],
        *,
        session_id: str,
        backoff_minimum: float = 0.1,
        backoff_maximum: float = 10.0,
    ) -> None:
        if (
            not _IDENTIFIER.fullmatch(instance_id)
            or not _SESSION.fullmatch(session_id)
            or backoff_minimum <= 0
            or backoff_maximum < backoff_minimum
        ):
            raise ValueError("invalid registration backoff")
        self.client = client
        self.instance_id = instance_id
        self.request_factory = request_factory
        self.capacity = capacity
        self.session_id = session_id
        self.backoff_minimum = backoff_minimum
        self.backoff_maximum = backoff_maximum
        self.state: RegistrationState | None = None
        self._draining = asyncio.Event()
        self._operation_lock = asyncio.Lock()

    async def run(self) -> None:
        backoff = self.backoff_minimum
        while not self._draining.is_set():
            try:
                if self.state is None:
                    request = dict(self.request_factory())
                    request["session_id"] = self.session_id
                    async with self._operation_lock:
                        if self._draining.is_set():
                            break
                        self.state = await self.client.register(
                            self.instance_id, self.session_id, request
                        )
                ready, active, available, queued = self.capacity()
                await asyncio.wait_for(
                    self._draining.wait(),
                    timeout=self.state.keepalive_interval_seconds,
                )
            except TimeoutError:
                try:
                    async with self._operation_lock:
                        if self._draining.is_set():
                            break
                        assert self.state is not None
                        self.state = await self.client.keepalive(
                            self.state,
                            ready=ready,
                            active_runs=active,
                            available_slots=available,
                            queue_depth=queued,
                        )
                    backoff = self.backoff_minimum
                except (RegistrationError, TransportUnavailable):
                    self.state = None
                    await self._pause(backoff + random.random() * backoff * 0.1)
                    backoff = min(self.backoff_maximum, backoff * 2)
            except (RegistrationError, TransportUnavailable):
                self.state = None
                await self._pause(backoff + random.random() * backoff * 0.1)
                backoff = min(self.backoff_maximum, backoff * 2)

    async def drain(self, timeout: float = 30.0) -> None:
        self._draining.set()
        async with self._operation_lock:
            state = self.state
            if state is None:
                return
            deadline = datetime.now(UTC) + timedelta(seconds=timeout)
            await self.client.drain(state, deadline)
            await self.client.deregister(state)
            self.state = None

    async def _pause(self, seconds: float) -> None:
        try:
            await asyncio.wait_for(self._draining.wait(), seconds)
        except TimeoutError:
            pass


def decode_error(response: Response) -> RegistrationError:
    try:
        if response.headers.get("content-type") != ("application/json",):
            raise ValueError
        value = strict_object(response.body)
        if not set(value).issubset(
            {
                "category",
                "code",
                "message",
                "retryable",
                "retry_after_seconds",
                "details",
                "trace_id",
            }
        ):
            raise ValueError
        code = value["code"]
        retryable = value["retryable"]
        if (
            not isinstance(value.get("category"), str)
            or not isinstance(value.get("message"), str)
            or not isinstance(code, str)
            or not isinstance(retryable, bool)
        ):
            raise ValueError
    except (ValueError, KeyError):
        raise TransportUnavailable("invalid error response") from None
    return RegistrationError(response.status, code, retryable)


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


def canonical_json(value: Mapping[str, object]) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()


def safe_int(value: object, minimum: int, maximum: int) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or not minimum <= value <= maximum:
        raise ValueError("integer outside safe range")
    return value


def valid_credential(value: str) -> bool:
    return bool(value) and len(value) <= 8192 and all(33 <= ord(character) <= 126 for character in value)


def timestamp(value: datetime) -> str:
    if value.tzinfo is None:
        raise ValueError("timezone required")
    return value.astimezone(UTC).isoformat(timespec="microseconds").replace("+00:00", "Z")


def _validate_register_request(
    instance_id: str, request: Mapping[str, object]
) -> None:
    if set(request) != {
        "schema_version",
        "session_id",
        "service_id",
        "environment",
        "endpoint",
        "bindings",
        "runtime",
    }:
        raise ValueError("invalid registration request")
    session_id = request.get("session_id")
    if request.get("schema_version") != 1 or not isinstance(
        session_id, str
    ) or not _SESSION.fullmatch(session_id):
        raise ValueError("invalid registration request")
    candidate = dict(request)
    candidate.update(
        {
            "instance_id": instance_id,
            "generation": 1,
            "resource_version": 1,
            "registry_revision": 1,
            "lease_id": "lease_" + _FIXTURE_UUID,
            "lease_expires_at": "2099-01-01T00:00:00Z",
            "operator": {"enabled": True, "weight": 100, "priority": 0},
            "draining": False,
            "status": "registered",
        }
    )
    wire.decode_runtime_instance(canonical_json(candidate))
