"""Driver-free transactional persistence contract for Provider runtimes."""

from __future__ import annotations

from dataclasses import dataclass, replace
from datetime import datetime
from enum import StrEnum
from typing import Awaitable, Callable, Protocol, TypeVar


class ProviderNotFound(Exception):
    """Requested durable state does not exist."""


class ProviderConflict(Exception):
    """A compare-and-set or idempotency precondition failed."""


class EffectUncertain(Exception):
    """A prior process started an effect without recording its result."""


class InboxState(StrEnum):
    ACCEPTED = "accepted"
    RUNNING = "running"
    CANCEL_REQUESTED = "cancel_requested"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    CANCELLED = "cancelled"
    TIMED_OUT = "timed_out"

    @property
    def terminal(self) -> bool:
        return self in {
            self.SUCCEEDED,
            self.FAILED,
            self.CANCELLED,
            self.TIMED_OUT,
        }


class EffectState(StrEnum):
    STARTED = "started"
    COMPLETED = "completed"


@dataclass(frozen=True, slots=True)
class InboxRecord:
    run_id: str
    attempt_id: str
    request_digest: str
    authorization_digest: str
    request_json: bytes
    result_json: bytes | None
    agent_id: str
    agent_version: str
    skill_id: str
    deployment_id: str
    instance_id: str
    generation: int
    fencing_token: int
    state_version: int
    traceparent: str
    tracestate: str
    state: InboxState
    deadline_at: datetime
    created_at: datetime
    updated_at: datetime
    cancel_requested_at: datetime | None = None

    def with_changes(self, **changes: object) -> "InboxRecord":
        return replace(self, **changes)


@dataclass(frozen=True, slots=True)
class EffectRecord:
    effect_id: str
    run_id: str
    attempt_id: str
    request_digest: str
    state: EffectState
    result: bytes | None
    started_at: datetime
    updated_at: datetime


@dataclass(frozen=True, slots=True)
class OutboxRecord:
    run_id: str
    attempt_id: str
    event_id: str
    event_type: str
    sequence: int
    envelope: bytes
    created_at: datetime
    delivered_at: datetime | None = None


class Transaction(Protocol):
    async def get_inbox(self, run_id: str, attempt_id: str) -> InboxRecord: ...
    async def create_inbox(self, record: InboxRecord) -> None: ...
    async def update_inbox(self, record: InboxRecord, expected_version: int) -> None: ...
    async def get_effect(self, effect_id: str) -> EffectRecord: ...
    async def create_effect(self, record: EffectRecord) -> None: ...
    async def complete_effect(
        self, effect_id: str, request_digest: str, result: bytes, now: datetime
    ) -> None: ...
    async def append_outbox(self, record: OutboxRecord) -> int: ...
    async def mark_outbox_delivered(
        self, run_id: str, attempt_id: str, sequence: int, now: datetime
    ) -> None: ...


T = TypeVar("T")


class DurableStore(Protocol):
    """Production implementations must provide real commit/rollback isolation."""

    async def within(self, operation: Callable[[Transaction], Awaitable[T]]) -> T: ...
    async def get_inbox(self, run_id: str, attempt_id: str) -> InboxRecord: ...
    async def list_recoverable(self, before: datetime, limit: int) -> list[InboxRecord]: ...
    async def list_outbox(
        self, run_id: str, attempt_id: str, after: int, limit: int
    ) -> list[OutboxRecord]: ...
    async def ready(self) -> None: ...
