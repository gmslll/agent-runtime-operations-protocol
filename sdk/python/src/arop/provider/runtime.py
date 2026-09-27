"""Provider runtime state machine shared by ASGI and reference agents."""

from __future__ import annotations

import asyncio
import hashlib
import inspect
import json
import re
from dataclasses import asdict, dataclass
from datetime import UTC, datetime
from typing import Awaitable, Callable, Protocol, TypeVar
from urllib.parse import urlsplit

from arop.generated.run import run_gen as wire

from .durable_store import (
    DurableStore,
    EffectRecord,
    EffectState,
    EffectUncertain,
    InboxRecord,
    InboxState,
    OutboxRecord,
    ProviderConflict,
    ProviderNotFound,
    Transaction,
)

SAFE_INTEGER = 9_007_199_254_740_991
_IDENTIFIER = re.compile(r"^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$")
_UUID7 = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
_SCOPE = re.compile(r"^[a-z][a-z0-9]*(?:[.:_-][a-z0-9]+)*$")
_EFFECT = re.compile(r"^eff_[A-Za-z0-9._:-]{4,196}$")
_EVENT_TYPE = re.compile(
    r"^io\.kinglucky\.arop\.(?:run|output|progress|usage)\.[a-z_]+\.v1$"
)


def _prefixed(prefix: str, value: str) -> bool:
    return value.startswith(prefix) and bool(_UUID7.fullmatch(value.removeprefix(prefix)))


class AuthenticationError(Exception):
    """Stable authentication failure without token material."""


class DependencyUnavailable(Exception):
    """Stable dependency failure without driver or connection details."""


@dataclass(frozen=True, slots=True)
class Claims:
    issuer: str
    audience: str
    subject: str
    authorized_party: str
    token_id: str
    run_id: str
    attempt_id: str
    agent_id: str
    agent_version: str
    skill_id: str
    deployment_id: str
    instance_id: str
    transport_profile: str
    endpoint: str
    generation: int
    fencing_token: int
    scopes: tuple[str, ...]
    issued_at: datetime
    expires_at: datetime


class TokenVerifier(Protocol):
    async def verify(
        self, token: str, method: str, path: str, required_scope: str, now: datetime
    ) -> Claims: ...


class Handler(Protocol):
    async def execute(
        self, execution: "Execution", request: wire.RunRequest
    ) -> wire.RunResult: ...


R = TypeVar("R")


class Execution:
    """Attempt-scoped effect and outbox helper."""

    def __init__(self, store: DurableStore, record: InboxRecord, clock: Callable[[], datetime]):
        self._store = store
        self.record = record
        self._clock = clock

    async def effect(
        self,
        effect_id: str,
        request: object,
        operation: Callable[[], Awaitable[R] | R],
    ) -> R:
        if not _EFFECT.fullmatch(effect_id):
            raise ValueError("invalid effect id")
        request_bytes = canonical_json(request)
        digest = sha256_digest(request_bytes)
        now = self._clock()

        async def reserve(tx: Transaction) -> bytes | None:
            try:
                prior = await tx.get_effect(effect_id)
            except ProviderNotFound:
                await tx.create_effect(
                    EffectRecord(
                        effect_id=effect_id,
                        run_id=self.record.run_id,
                        attempt_id=self.record.attempt_id,
                        request_digest=digest,
                        state=EffectState.STARTED,
                        result=None,
                        started_at=now,
                        updated_at=now,
                    )
                )
                return None
            if prior.request_digest != digest or prior.run_id != self.record.run_id:
                raise ProviderConflict("effect request digest conflict")
            if prior.state is EffectState.STARTED or prior.result is None:
                raise EffectUncertain("effect outcome is uncertain")
            return prior.result

        replay = await self._store.within(reserve)
        if replay is not None:
            return json.loads(replay)
        result = operation()
        if inspect.isawaitable(result):
            result = await result
        encoded = canonical_json(result)

        async def complete(tx: Transaction) -> None:
            await tx.complete_effect(effect_id, digest, encoded, self._clock())

        await self._store.within(complete)
        return result  # type: ignore[return-value]

    async def emit(self, event_id: str, event_type: str, data: object) -> int:
        if not event_id or not _EVENT_TYPE.fullmatch(event_type):
            raise ValueError("invalid event")
        envelope = canonical_json(data)

        async def append(tx: Transaction) -> int:
            return await tx.append_outbox(
                OutboxRecord(
                    run_id=self.record.run_id,
                    attempt_id=self.record.attempt_id,
                    event_id=event_id,
                    event_type=event_type,
                    sequence=0,
                    envelope=envelope,
                    created_at=self._clock(),
                )
            )

        return await self._store.within(append)


@dataclass(frozen=True, slots=True)
class RuntimeReply:
    status: int
    body: bytes = b""
    headers: tuple[tuple[str, str], ...] = ()


class ProviderRuntime:
    def __init__(
        self,
        store: DurableStore,
        verifier: TokenVerifier,
        handler: Handler,
        *,
        clock: Callable[[], datetime] | None = None,
    ) -> None:
        self.store = store
        self.verifier = verifier
        self.handler = handler
        self.clock = clock or (lambda: datetime.now(UTC))
        self._tasks: dict[tuple[str, str], asyncio.Task[None]] = {}
        self._task_lock = asyncio.Lock()

    async def create(self, token: str, idempotency_key: str, body: bytes) -> RuntimeReply:
        if not _prefixed("att_", idempotency_key):
            return problem(400, "INVALID_IDEMPOTENCY_KEY", "validation")
        claims = await self._verify(token, "POST", "/v1/runs", "agent:invoke")
        if claims.attempt_id != idempotency_key:
            raise AuthenticationError("authorization binding failed")
        try:
            request = wire.decode_run_request(body)
            normalized = wire.encode_run_request(request).encode()
            deadline = parse_time(str(request.deadline_at))
        except (ValueError, TypeError):
            return problem(400, "INVALID_RUN_REQUEST", "validation")
        if deadline <= self.clock() or not request_binds(claims, request):
            return problem(400, "INVALID_RUN_REQUEST", "validation")
        now = self.clock()
        record = InboxRecord(
            run_id=claims.run_id,
            attempt_id=claims.attempt_id,
            request_digest=sha256_digest(normalized),
            authorization_digest=claims_digest(claims),
            request_json=normalized,
            result_json=None,
            agent_id=claims.agent_id,
            agent_version=claims.agent_version,
            skill_id=claims.skill_id,
            deployment_id=claims.deployment_id,
            instance_id=claims.instance_id,
            generation=claims.generation,
            fencing_token=claims.fencing_token,
            state_version=1,
            traceparent=str(request.trace.traceparent),
            tracestate="" if isinstance(request.trace.tracestate, wire.UnsetType) else str(request.trace.tracestate),
            state=InboxState.ACCEPTED,
            deadline_at=deadline,
            created_at=now,
            updated_at=now,
        )
        created = False

        async def insert(tx: Transaction) -> InboxRecord:
            nonlocal created
            try:
                prior = await tx.get_inbox(record.run_id, record.attempt_id)
            except ProviderNotFound:
                await tx.create_inbox(record)
                await tx.append_outbox(lifecycle(record, "accepted", now))
                created = True
                return record
            if (
                prior.request_digest != record.request_digest
                or prior.authorization_digest != record.authorization_digest
            ):
                raise ProviderConflict("idempotency conflict")
            return prior

        try:
            record = await self.store.within(insert)
        except ProviderConflict:
            return problem(409, "IDEMPOTENCY_CONFLICT", "conflict")
        if created:
            await self._start(record)
        return RuntimeReply(
            202,
            status_json(record),
            (("location", f"/v1/runs/{record.run_id}"),),
        )

    async def status(self, token: str, run_id: str) -> RuntimeReply:
        claims = await self._verify(token, "GET", f"/v1/runs/{run_id}", "agent:invoke")
        if claims.run_id != run_id:
            raise AuthenticationError("authorization binding failed")
        try:
            record = await self.store.get_inbox(run_id, claims.attempt_id)
        except ProviderNotFound:
            return problem(404, "RUN_NOT_FOUND", "not_found")
        return RuntimeReply(200, status_json(record))

    async def cancel(
        self, token: str, run_id: str, expected_state_version: int
    ) -> RuntimeReply:
        claims = await self._verify(
            token, "POST", f"/v1/runs/{run_id}/commands", "agent:invoke"
        )
        if (
            claims.run_id != run_id
            or expected_state_version < 1
            or expected_state_version > SAFE_INTEGER
        ):
            raise AuthenticationError("authorization binding failed")
        now = self.clock()

        async def update(tx: Transaction) -> InboxRecord:
            current = await tx.get_inbox(run_id, claims.attempt_id)
            if current.state.terminal or current.state is InboxState.CANCEL_REQUESTED:
                return current
            if current.state_version != expected_state_version:
                raise ProviderConflict("state version conflict")
            changed = current.with_changes(
                state=InboxState.CANCEL_REQUESTED,
                state_version=current.state_version + 1,
                updated_at=now,
                cancel_requested_at=now,
            )
            await tx.update_inbox(changed, current.state_version)
            await tx.append_outbox(lifecycle(changed, "cancel_requested", now))
            return changed

        try:
            record = await self.store.within(update)
        except ProviderConflict:
            return problem(409, "STATE_VERSION_CONFLICT", "conflict")
        task = self._tasks.get((run_id, claims.attempt_id))
        if task is not None:
            task.cancel()
        return RuntimeReply(202, status_json(record))

    async def events(
        self, token: str, run_id: str, after: int, limit: int = 256
    ) -> tuple[InboxRecord, list[OutboxRecord]]:
        if after < 0 or after > SAFE_INTEGER or limit < 1 or limit > 256:
            raise ValueError("invalid event cursor")
        claims = await self._verify(
            token, "GET", f"/v1/runs/{run_id}/events", "run:stream"
        )
        if claims.run_id != run_id:
            raise AuthenticationError("authorization binding failed")
        record = await self.store.get_inbox(run_id, claims.attempt_id)
        events = await self.store.list_outbox(run_id, claims.attempt_id, after, limit)
        expected = after + 1
        for event in events:
            if event.sequence != expected:
                raise ProviderConflict("event sequence gap")
            expected += 1
        if not events and after != 0 and record.state.terminal:
            latest = 0
            while True:
                page = await self.store.list_outbox(
                    run_id, claims.attempt_id, latest, 256
                )
                for event in page:
                    if event.sequence != latest + 1:
                        raise ProviderConflict("event sequence gap")
                    latest = event.sequence
                if len(page) < 256:
                    break
            if after > latest:
                raise ProviderConflict("event cursor ahead")
        return record, events

    async def recover(self, limit: int = 100) -> None:
        if limit < 1 or limit > 1000:
            raise ValueError("invalid recovery limit")
        now = self.clock()
        for record in await self.store.list_recoverable(now, limit):
            if record.deadline_at <= now:
                await self._finalize(record, timed_out(record.run_id, now), InboxState.TIMED_OUT)
            else:
                await self._start(record)

    async def drain(self, timeout: float | None = None) -> None:
        async with self._task_lock:
            tasks = list(self._tasks.values())
        if not tasks:
            return
        try:
            await asyncio.wait_for(asyncio.gather(*tasks, return_exceptions=True), timeout)
        except TimeoutError:
            for task in tasks:
                task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)

    async def _verify(
        self, token: str, method: str, path: str, scope: str
    ) -> Claims:
        if not token or any(character.isspace() for character in token) or len(token) > 8192:
            raise AuthenticationError("authentication failed")
        try:
            now = self.clock()
            claims = await self.verifier.verify(token, method, path, scope, now)
            valid = valid_claims(claims, scope, now)
        except Exception:
            raise AuthenticationError("authentication failed") from None
        if not valid:
            raise AuthenticationError("authentication failed")
        return claims

    async def _start(self, record: InboxRecord) -> None:
        key = (record.run_id, record.attempt_id)
        async with self._task_lock:
            if key in self._tasks:
                return
            task = asyncio.create_task(self._execute(record))
            self._tasks[key] = task
            task.add_done_callback(lambda _: self._tasks.pop(key, None))

    async def _execute(self, record: InboxRecord) -> None:
        try:
            record = await self._mark_running(record)
        except Exception:
            try:
                current = await self.store.get_inbox(record.run_id, record.attempt_id)
                if current.state is InboxState.CANCEL_REQUESTED:
                    await self._finalize(
                        current,
                        cancelled(current.run_id, self.clock()),
                        InboxState.CANCELLED,
                    )
            except Exception:
                pass
            return
        try:
            request = wire.decode_run_request(record.request_json)
            execution = Execution(self.store, record, self.clock)
            remaining = (record.deadline_at - self.clock()).total_seconds()
            if remaining <= 0:
                raise TimeoutError
            result = await asyncio.wait_for(
                self.handler.execute(execution, request), timeout=remaining
            )
            encoded = wire.encode_run_result(result).encode()
            decoded = wire.decode_run_result(encoded)
            if str(decoded.run_id) != record.run_id or str(decoded.state) != "succeeded":
                raise ValueError("handler returned an invalid terminal result")
            current = await self.store.get_inbox(record.run_id, record.attempt_id)
            if current.state is InboxState.CANCEL_REQUESTED:
                await self._finalize(
                    current,
                    cancelled(current.run_id, self.clock()),
                    InboxState.CANCELLED,
                )
            else:
                await self._finalize(current, encoded, InboxState.SUCCEEDED)
        except TimeoutError:
            current = await self.store.get_inbox(record.run_id, record.attempt_id)
            await self._finalize(
                current, timed_out(record.run_id, self.clock()), InboxState.TIMED_OUT
            )
        except asyncio.CancelledError:
            current = await self.store.get_inbox(record.run_id, record.attempt_id)
            await self._finalize(current, cancelled(record.run_id, self.clock()), InboxState.CANCELLED)
        except Exception:
            current = await self.store.get_inbox(record.run_id, record.attempt_id)
            await self._finalize(current, failed(record.run_id, self.clock()), InboxState.FAILED)

    async def _mark_running(self, record: InboxRecord) -> InboxRecord:
        now = self.clock()

        async def mark(tx: Transaction) -> InboxRecord:
            current = await tx.get_inbox(record.run_id, record.attempt_id)
            if current.state.terminal or current.state is InboxState.CANCEL_REQUESTED:
                raise ProviderConflict("provider state conflict")
            changed = current.with_changes(
                state=InboxState.RUNNING,
                state_version=current.state_version + 1,
                updated_at=now,
            )
            await tx.update_inbox(changed, current.state_version)
            await tx.append_outbox(lifecycle(changed, "running", now))
            return changed

        return await self.store.within(mark)

    async def _finalize(
        self, record: InboxRecord, result: bytes, state: InboxState
    ) -> None:
        now = self.clock()

        async def finish(tx: Transaction) -> None:
            current = await tx.get_inbox(record.run_id, record.attempt_id)
            if current.state.terminal:
                return
            changed = current.with_changes(
                result_json=result,
                state=state,
                state_version=current.state_version + 1,
                updated_at=now,
            )
            await tx.update_inbox(changed, current.state_version)
            await tx.append_outbox(lifecycle(changed, state.value, now))

        await self.store.within(finish)


def canonical_json(value: object) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode()


def sha256_digest(value: bytes) -> str:
    return "sha256:" + hashlib.sha256(value).hexdigest()


def claims_digest(claims: Claims) -> str:
    value = asdict(claims)
    value["issued_at"] = claims.issued_at.astimezone(UTC).isoformat().replace("+00:00", "Z")
    value["expires_at"] = claims.expires_at.astimezone(UTC).isoformat().replace("+00:00", "Z")
    value["scopes"] = sorted(claims.scopes)
    return sha256_digest(canonical_json(value))


def valid_claims(claims: Claims, scope: str, now: datetime) -> bool:
    issuer = urlsplit(claims.issuer)
    audience = urlsplit(claims.audience)
    endpoint = urlsplit(claims.endpoint)
    scopes = set(claims.scopes)
    return (
        issuer.scheme == "https"
        and bool(issuer.hostname)
        and issuer.username is None
        and issuer.path == ""
        and not issuer.query
        and not issuer.fragment
        and audience.scheme == "https"
        and bool(audience.hostname)
        and audience.username is None
        and audience.path == "/deployments/" + claims.deployment_id
        and not audience.query
        and not audience.fragment
        and endpoint.scheme == "https"
        and bool(endpoint.hostname)
        and endpoint.username is None
        and endpoint.path == "/v1/runs"
        and not endpoint.query
        and not endpoint.fragment
        and _prefixed("prn_", claims.subject)
        and _prefixed("cred_", claims.authorized_party)
        and _prefixed("tok_", claims.token_id)
        and _prefixed("run_", claims.run_id)
        and _prefixed("att_", claims.attempt_id)
        and bool(_IDENTIFIER.fullmatch(claims.agent_id))
        and bool(claims.agent_version)
        and bool(_IDENTIFIER.fullmatch(claims.skill_id))
        and _prefixed("dep_", claims.deployment_id)
        and bool(_IDENTIFIER.fullmatch(claims.instance_id))
        and claims.transport_profile in {"direct", "proxy"}
        and 0 < claims.generation <= SAFE_INTEGER
        and 0 < claims.fencing_token <= SAFE_INTEGER
        and claims.issued_at.tzinfo is not None
        and claims.expires_at.tzinfo is not None
        and claims.issued_at.utcoffset().total_seconds() == 0
        and claims.expires_at.utcoffset().total_seconds() == 0
        and claims.issued_at <= now < claims.expires_at
        and (claims.expires_at - claims.issued_at).total_seconds() <= 300
        and 0 < len(claims.scopes) <= 16
        and len(scopes) == len(claims.scopes)
        and all(_SCOPE.fullmatch(value) for value in scopes)
        and scope in scopes
    )


def request_binds(claims: Claims, request: wire.RunRequest) -> bool:
    return (
        str(request.agent.id) == claims.agent_id
        and str(request.agent.version) == claims.agent_version
        and str(request.agent.skill_id) == claims.skill_id
    )


def parse_time(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timezone required")
    return parsed.astimezone(UTC)


def lifecycle(record: InboxRecord, state: str, now: datetime) -> OutboxRecord:
    wire_state = "started" if state == "running" else state
    event_type = f"io.kinglucky.arop.run.{wire_state}.v1"
    seed = f"{record.run_id}\0{record.attempt_id}\0{record.state_version}\0{state}".encode()
    data: dict[str, object] = {"state": wire_state}
    if record.state.terminal:
        if record.result_json is None:
            raise ValueError("terminal lifecycle event requires a result")
        result = json.loads(record.result_json)
        if not isinstance(result, dict):
            raise ValueError("terminal lifecycle result is not an object")
        data = {
            key: result[key]
            for key in ("state", "usage", "completed_at", "snapshot", "result_ref", "error")
            if key in result
        }
    return OutboxRecord(
        run_id=record.run_id,
        attempt_id=record.attempt_id,
        event_id="evt_" + hashlib.sha256(seed).hexdigest()[:32],
        event_type=event_type,
        sequence=0,
        envelope=canonical_json(data),
        created_at=now,
    )


def status_json(record: InboxRecord) -> bytes:
    if record.result_json is not None and record.state.terminal:
        result = json.loads(record.result_json)
    else:
        result = None
    request = wire.decode_run_request(record.request_json)
    value: dict[str, object] = {
        "schema_version": 1,
        "run_id": record.run_id,
        "agent": json.loads(wire.encode_run_request(request))["agent"],
        "authorization_snapshot_digest": record.authorization_digest,
        "state": "dispatching" if record.state is InboxState.ACCEPTED else record.state.value,
        "state_version": record.state_version,
        "created_at": timestamp(record.created_at),
        "updated_at": timestamp(record.updated_at),
        "deadline_at": timestamp(record.deadline_at),
        "trace": {
            "traceparent": record.traceparent,
            **({"tracestate": record.tracestate} if record.tracestate else {}),
        },
    }
    if record.cancel_requested_at is not None:
        value["cancel_requested_at"] = timestamp(record.cancel_requested_at)
    if result is not None:
        value["result"] = result
    encoded = canonical_json(value)
    wire.decode_run_status(encoded)
    return encoded


def terminal_result(
    run_id: str,
    now: datetime,
    state: str,
    category: str,
    code: str,
    message: str,
) -> bytes:
    return canonical_json(
        {
            "schema_version": 1,
            "run_id": run_id,
            "state": state,
            "completed_at": timestamp(now),
            "usage": {"duration_ms": 0, "input_tokens": 0, "output_tokens": 0},
            "error": {
                "category": category,
                "code": code,
                "message": message,
                "retryable": False,
            },
        }
    )


def cancelled(run_id: str, now: datetime) -> bytes:
    return terminal_result(
        run_id, now, "cancelled", "cancelled", "RUN_CANCELLED", "run was cancelled"
    )


def failed(run_id: str, now: datetime) -> bytes:
    return terminal_result(
        run_id,
        now,
        "failed",
        "internal",
        "AGENT_EXECUTION_FAILED",
        "agent execution failed",
    )


def timed_out(run_id: str, now: datetime) -> bytes:
    return terminal_result(
        run_id, now, "timed_out", "timeout", "RUN_TIMED_OUT", "run deadline elapsed"
    )


def timestamp(value: datetime) -> str:
    return value.astimezone(UTC).isoformat(timespec="microseconds").replace("+00:00", "Z")


def problem(status: int, code: str, category: str) -> RuntimeReply:
    retry = status in {429, 503, 504}
    value: dict[str, object] = {
        "category": category,
        "code": code,
        "message": "request failed",
        "retryable": retry,
    }
    headers: tuple[tuple[str, str], ...] = ()
    if retry:
        value["retry_after_seconds"] = 1
        headers = (("retry-after", "1"),)
    return RuntimeReply(
        status,
        canonical_json(value),
        headers,
    )
