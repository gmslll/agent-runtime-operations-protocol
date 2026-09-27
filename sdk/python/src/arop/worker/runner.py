"""At-least-once fenced Worker runner with crash-resumable completion state."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from datetime import UTC, datetime
import hashlib
import inspect
import json
import re
import secrets
from typing import Awaitable, Callable, Protocol

from arop._transport import TransportUnavailable
from arop.generated.worker import worker_gen as wire

from .client import WorkerClient, WorkerRemoteError

_WORKER = re.compile(r"^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$")
_SESSION = re.compile(
    r"^ses_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
_EFFECT = re.compile(r"^eff_[A-Za-z0-9._:-]{4,196}$")


class _LeaseLost(Exception):
    pass


@dataclass(frozen=True, slots=True)
class WorkerOutcome:
    result: wire.AROPV1TerminalRunResult
    effect_ids: tuple[str, ...] = ()


@dataclass(frozen=True, slots=True)
class CompletionRecord:
    run_id: str
    attempt_id: str
    claim_id: str
    completion_id: str
    idempotency_key: str
    result_json: bytes
    effect_ids: tuple[str, ...]


class CompletionStore(Protocol):
    async def load(self, run_id: str, attempt_id: str) -> CompletionRecord | None: ...
    async def save(self, record: CompletionRecord) -> None: ...
    async def delete(self, run_id: str, attempt_id: str) -> None: ...


class WorkerHandler(Protocol):
    async def handle(self, claim: wire.WorkerClaim) -> WorkerOutcome: ...


class WorkerRunner:
    def __init__(
        self,
        client: WorkerClient,
        handler: WorkerHandler,
        completion_store: CompletionStore,
        *,
        worker_id: str,
        session_id: str,
        generation: int,
        supported_bindings: list[wire.AgentBinding],
        concurrency: int = 1,
        wait_seconds: int = 20,
        lease_seconds: int = 60,
        renew_interval: float | None = None,
        backoff_minimum: float = 0.1,
        backoff_maximum: float = 5.0,
    ) -> None:
        if (
            not _WORKER.fullmatch(worker_id)
            or not _SESSION.fullmatch(session_id)
            or not 1 <= generation <= 9_007_199_254_740_991
            or not supported_bindings
            or not 1 <= concurrency <= 1024
            or not 0 <= wait_seconds <= 30
            or not 15 <= lease_seconds <= 300
            or backoff_minimum <= 0
            or backoff_maximum < backoff_minimum
        ):
            raise ValueError("invalid worker runner configuration")
        self.client = client
        self.handler = handler
        self.completion_store = completion_store
        self.worker_id = worker_id
        self.session_id = session_id
        self.generation = generation
        self.supported_bindings = list(supported_bindings)
        self.concurrency = concurrency
        self.wait_seconds = wait_seconds
        self.lease_seconds = lease_seconds
        self.renew_interval = renew_interval or lease_seconds / 3
        if not 1 <= self.renew_interval < lease_seconds:
            raise ValueError("invalid renew interval")
        self.backoff_minimum = backoff_minimum
        self.backoff_maximum = backoff_maximum
        self._draining = asyncio.Event()
        self._active: set[asyncio.Task[None]] = set()

    async def run(self) -> None:
        backoff = self.backoff_minimum
        while not self._draining.is_set():
            if len(self._active) >= self.concurrency:
                await asyncio.wait(self._active, return_when=asyncio.FIRST_COMPLETED)
                continue
            request = wire.WorkerClaimRequest(
                schema_version=1,
                session_id=self.session_id,
                generation=self.generation,
                available_slots=self.concurrency - len(self._active),
                supported_bindings=list(self.supported_bindings),
                wait_seconds=self.wait_seconds,
                lease_seconds=self.lease_seconds,
            )
            try:
                claim = await self._claim_or_drain(request)
                if self._draining.is_set():
                    break
                backoff = self.backoff_minimum
            except (TransportUnavailable, WorkerRemoteError):
                await self._sleep(backoff)
                backoff = min(self.backoff_maximum, backoff * 2)
                continue
            if claim is None:
                await self._sleep(self.backoff_minimum)
                continue
            task = asyncio.create_task(self._handle(claim))
            self._active.add(task)
            task.add_done_callback(self._active.discard)
        if self._active:
            await asyncio.gather(*self._active, return_exceptions=True)

    async def drain(self, timeout: float | None = None) -> None:
        self._draining.set()
        if not self._active:
            return
        try:
            await asyncio.wait_for(
                asyncio.gather(*self._active, return_exceptions=True), timeout
            )
        except TimeoutError:
            for task in self._active:
                task.cancel()
            await asyncio.gather(*self._active, return_exceptions=True)

    async def _handle(self, claim: wire.WorkerClaim) -> None:
        deadline = datetime.fromisoformat(str(claim.run_request.deadline_at).replace("Z", "+00:00"))
        if deadline.tzinfo is None or deadline <= datetime.now(UTC):
            await self._release(claim, "retryable_failure")
            return
        renew = asyncio.create_task(self._renew(claim))
        try:
            durable = await self.completion_store.load(str(claim.run_id), str(claim.attempt_id))
            if durable is None:
                outcome = await asyncio.wait_for(
                    self._with_lease(
                        self.handler.handle(claim),
                        renew,
                    ),
                    timeout=max(
                        0.001, (deadline - datetime.now(UTC)).total_seconds()
                    ),
                )
                if (
                    str(outcome.result.run_id) != str(claim.run_id)
                    or any(not _EFFECT.fullmatch(value) for value in outcome.effect_ids)
                    or len(set(outcome.effect_ids)) != len(outcome.effect_ids)
                ):
                    raise ValueError("invalid worker outcome")
                result_json = json.dumps(
                    wire._to_wire(outcome.result),
                    sort_keys=True,
                    separators=(",", ":"),
                ).encode()
                validated_result = wire._decode_AROPV1TerminalRunResult(
                    json.loads(result_json), False
                )
                if str(validated_result.run_id) != str(claim.run_id):
                    raise ValueError("invalid worker outcome")
                completion_id = "cmp_" + uuid7()
                durable = CompletionRecord(
                    run_id=str(claim.run_id),
                    attempt_id=str(claim.attempt_id),
                    claim_id=str(claim.claim_id),
                    completion_id=completion_id,
                    idempotency_key="complete-" + completion_id.removeprefix("cmp_"),
                    result_json=result_json,
                    effect_ids=tuple(outcome.effect_ids),
                )
                await self.completion_store.save(durable)
            result = wire._decode_AROPV1TerminalRunResult(
                json.loads(durable.result_json), False
            )
            if (
                durable.run_id != str(claim.run_id)
                or durable.attempt_id != str(claim.attempt_id)
                or str(result.run_id) != str(claim.run_id)
            ):
                raise ValueError("durable worker completion binding mismatch")
            request = wire.WorkerComplete(
                schema_version=1,
                completion_id=durable.completion_id,
                claim_id=claim.claim_id,
                attempt_id=claim.attempt_id,
                fencing_token=claim.fencing_token,
                lease_token=claim.lease_token,
                result=result,
                completed_at=str(result.completed_at),
                effect_ids=list(durable.effect_ids) if durable.effect_ids else wire.UNSET,
            )
            backoff = self.backoff_minimum
            while True:
                try:
                    await self._before_deadline(
                        self.client.complete(
                            self.worker_id,
                            str(claim.claim_id),
                            durable.idempotency_key,
                            request,
                        ),
                        renew,
                        deadline,
                    )
                    await self.completion_store.delete(durable.run_id, durable.attempt_id)
                    return
                except TransportUnavailable:
                    await self._before_deadline(self._sleep(backoff), renew, deadline)
                    backoff = min(self.backoff_maximum, backoff * 2)
                except WorkerRemoteError as error:
                    if not error.retryable:
                        return
                    delay = backoff
                    if error.retry_after_seconds is not None:
                        delay = min(self.backoff_maximum, max(delay, error.retry_after_seconds))
                    await self._before_deadline(self._sleep(delay), renew, deadline)
                    backoff = min(self.backoff_maximum, backoff * 2)
        except (asyncio.CancelledError, TimeoutError):
            await self._release(claim, "worker_draining" if self._draining.is_set() else "retryable_failure")
        except Exception:
            await self._release(claim, "retryable_failure")
        finally:
            renew.cancel()
            await asyncio.gather(renew, return_exceptions=True)

    async def _renew(self, claim: wire.WorkerClaim) -> None:
        while True:
            await asyncio.sleep(self.renew_interval)
            request = wire.WorkerRenewRequest(
                schema_version=1,
                fencing_token=claim.fencing_token,
                lease_token=claim.lease_token,
                lease_seconds=self.lease_seconds,
            )
            await self.client.renew(self.worker_id, str(claim.claim_id), request)

    async def _with_lease(self, operation: Awaitable, renewal: asyncio.Task[None]):
        task = asyncio.ensure_future(operation)
        try:
            done, _ = await asyncio.wait(
                {task, renewal}, return_when=asyncio.FIRST_COMPLETED
            )
        except asyncio.CancelledError:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
            raise
        if renewal in done:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
            await asyncio.gather(renewal, return_exceptions=True)
            raise _LeaseLost("worker lease renewal failed")
        return await task

    async def _before_deadline(
        self,
        operation: Awaitable,
        renewal: asyncio.Task[None],
        deadline: datetime,
    ):
        remaining = (deadline - datetime.now(UTC)).total_seconds()
        if remaining <= 0:
            if inspect.iscoroutine(operation):
                operation.close()
            raise TimeoutError
        return await asyncio.wait_for(
            self._with_lease(operation, renewal), timeout=remaining
        )

    async def _claim_or_drain(
        self, request: wire.WorkerClaimRequest
    ) -> wire.WorkerClaim | None:
        claim = asyncio.create_task(self.client.claim(self.worker_id, request))
        draining = asyncio.create_task(self._draining.wait())
        done, _ = await asyncio.wait(
            {claim, draining}, return_when=asyncio.FIRST_COMPLETED
        )
        if draining in done:
            claim.cancel()
            await asyncio.gather(claim, return_exceptions=True)
            return None
        draining.cancel()
        await asyncio.gather(draining, return_exceptions=True)
        return await claim

    async def _release(self, claim: wire.WorkerClaim, reason: str) -> None:
        try:
            await self.client.release(
                self.worker_id,
                str(claim.claim_id),
                wire.WorkerReleaseRequest(
                    schema_version=1,
                    fencing_token=claim.fencing_token,
                    lease_token=claim.lease_token,
                    reason=reason,
                ),
            )
        except Exception:
            pass

    async def _sleep(self, seconds: float) -> None:
        try:
            await asyncio.wait_for(self._draining.wait(), seconds)
        except TimeoutError:
            pass


def effect_id(run_id: str, operation: str) -> str:
    if not run_id or not operation or len(operation) > 200:
        raise ValueError("invalid effect identity")
    return "eff_" + hashlib.sha256((run_id + "\0" + operation).encode()).hexdigest()


def uuid7() -> str:
    milliseconds = int(datetime.now(UTC).timestamp() * 1000)
    value = bytearray(milliseconds.to_bytes(6, "big") + secrets.token_bytes(10))
    value[6] = (value[6] & 0x0F) | 0x70
    value[8] = (value[8] & 0x3F) | 0x80
    hexadecimal = value.hex()
    return f"{hexadecimal[:8]}-{hexadecimal[8:12]}-{hexadecimal[12:16]}-{hexadecimal[16:20]}-{hexadecimal[20:]}"
