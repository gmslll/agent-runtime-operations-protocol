"""SQLite implementation of Provider durability and Worker completion ports."""

from __future__ import annotations

import asyncio
from datetime import UTC, datetime
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import stat
from typing import Awaitable, Callable, TypeVar

from arop.provider.durable_store import (
    EffectRecord,
    EffectState,
    InboxRecord,
    InboxState,
    OutboxRecord,
    ProviderConflict,
    ProviderNotFound,
    Transaction,
)
from arop.worker.runner import CompletionRecord

T = TypeVar("T")


class _SQLiteTransaction(Transaction):
    def __init__(self, store: "SQLiteStore") -> None:
        self._store = store

    async def get_inbox(self, run_id: str, attempt_id: str) -> InboxRecord:
        return await self._store._get_inbox(run_id, attempt_id)

    async def create_inbox(self, record: InboxRecord) -> None:
        await self._store.create_inbox(record)

    async def update_inbox(self, record: InboxRecord, expected_version: int) -> None:
        await self._store.update_inbox(record, expected_version)

    async def get_effect(self, effect_id: str) -> EffectRecord:
        return await self._store.get_effect(effect_id)

    async def create_effect(self, record: EffectRecord) -> None:
        await self._store.create_effect(record)

    async def complete_effect(
        self, effect_id: str, request_digest: str, result: bytes, now: datetime
    ) -> None:
        await self._store.complete_effect(effect_id, request_digest, result, now)

    async def append_outbox(self, record: OutboxRecord) -> int:
        return await self._store.append_outbox(record)

    async def mark_outbox_delivered(
        self, run_id: str, attempt_id: str, sequence: int, now: datetime
    ) -> None:
        await self._store.mark_outbox_delivered(run_id, attempt_id, sequence, now)


class SQLiteStore(Transaction):
    def __init__(self, path: str | Path, migration: str | Path) -> None:
        database = Path(path)
        if not database.is_absolute():
            raise ValueError("database path must be absolute and non-symlink")
        reject_symlink_components(database)
        database.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        reject_symlink_components(database)
        script = Path(migration).read_bytes()
        if not script.strip():
            raise ValueError("provider migration must not be empty")
        self._migration = script
        self._connection = sqlite3.connect(database, isolation_level=None, timeout=5)
        self._connection.row_factory = sqlite3.Row
        self._connection.execute("PRAGMA foreign_keys = ON")
        self._connection.execute("PRAGMA busy_timeout = 5000")
        self._connection.execute("PRAGMA journal_mode = WAL")
        self._lock = asyncio.Lock()
        try:
            self._migrate()
        except BaseException:
            self._connection.close()
            raise

    async def close(self) -> None:
        async with self._lock:
            self._connection.close()

    async def within(self, operation: Callable[[Transaction], Awaitable[T]]) -> T:
        async with self._lock:
            self._connection.execute("BEGIN IMMEDIATE")
            try:
                result = await operation(_SQLiteTransaction(self))
            except BaseException:
                self._connection.execute("ROLLBACK")
                raise
            self._connection.execute("COMMIT")
            return result

    async def ready(self) -> None:
        async with self._lock:
            self._verify_schema()

    async def get_inbox(self, run_id: str, attempt_id: str) -> InboxRecord:
        async with self._lock:
            return await self._get_inbox(run_id, attempt_id)

    async def _get_inbox(self, run_id: str, attempt_id: str) -> InboxRecord:
        row = self._connection.execute(
            "SELECT * FROM provider_inbox WHERE run_id=? AND attempt_id=?",
            (run_id, attempt_id),
        ).fetchone()
        if row is None:
            raise ProviderNotFound("provider state not found")
        return inbox(row)

    async def create_inbox(self, record: InboxRecord) -> None:
        try:
            self._connection.execute(
                """INSERT INTO provider_inbox VALUES
                (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
                inbox_values(record),
            )
        except sqlite3.IntegrityError:
            raise ProviderConflict("provider state conflict") from None

    async def update_inbox(self, record: InboxRecord, expected_version: int) -> None:
        cursor = self._connection.execute(
            """UPDATE provider_inbox SET result_json=?, state_version=?, state=?,
            updated_at=?, cancel_requested_at=?
            WHERE run_id=? AND attempt_id=? AND state_version=?""",
            (
                record.result_json,
                record.state_version,
                record.state.value,
                timestamp(record.updated_at),
                optional_time(record.cancel_requested_at),
                record.run_id,
                record.attempt_id,
                expected_version,
            ),
        )
        if cursor.rowcount != 1:
            raise ProviderConflict("provider state conflict")

    async def list_recoverable(self, before: datetime, limit: int) -> list[InboxRecord]:
        async with self._lock:
            rows = self._connection.execute(
                """SELECT * FROM provider_inbox
                WHERE state IN ('accepted','running','cancel_requested')
                ORDER BY updated_at, run_id, attempt_id LIMIT ?""",
                (limit,),
            ).fetchall()
            return [inbox(row) for row in rows]

    async def get_effect(self, effect_id: str) -> EffectRecord:
        row = self._connection.execute(
            "SELECT * FROM provider_effects WHERE effect_id=?", (effect_id,)
        ).fetchone()
        if row is None:
            raise ProviderNotFound("provider effect not found")
        return EffectRecord(
            row["effect_id"],
            row["run_id"],
            row["attempt_id"],
            row["request_digest"],
            EffectState(row["state"]),
            row["result"],
            parse_time(row["started_at"]),
            parse_time(row["updated_at"]),
        )

    async def create_effect(self, record: EffectRecord) -> None:
        try:
            self._connection.execute(
                "INSERT INTO provider_effects VALUES (?,?,?,?,?,?,?,?)",
                (
                    record.effect_id,
                    record.run_id,
                    record.attempt_id,
                    record.request_digest,
                    record.state.value,
                    record.result,
                    timestamp(record.started_at),
                    timestamp(record.updated_at),
                ),
            )
        except sqlite3.IntegrityError:
            raise ProviderConflict("provider effect conflict") from None

    async def complete_effect(
        self, effect_id: str, request_digest: str, result: bytes, now: datetime
    ) -> None:
        cursor = self._connection.execute(
            """UPDATE provider_effects SET state='completed', result=?, updated_at=?
            WHERE effect_id=? AND request_digest=? AND state='started'""",
            (result, timestamp(now), effect_id, request_digest),
        )
        if cursor.rowcount != 1:
            raise ProviderConflict("provider effect conflict")

    async def append_outbox(self, record: OutboxRecord) -> int:
        sequence = self._connection.execute(
            "SELECT COALESCE(MAX(sequence),0)+1 FROM provider_outbox WHERE run_id=? AND attempt_id=?",
            (record.run_id, record.attempt_id),
        ).fetchone()[0]
        try:
            self._connection.execute(
                "INSERT INTO provider_outbox VALUES (?,?,?,?,?,?,?,?)",
                (
                    record.run_id,
                    record.attempt_id,
                    sequence,
                    record.event_id,
                    record.event_type,
                    record.envelope,
                    timestamp(record.created_at),
                    optional_time(record.delivered_at),
                ),
            )
        except sqlite3.IntegrityError:
            raise ProviderConflict("provider outbox conflict") from None
        return sequence

    async def list_outbox(
        self, run_id: str, attempt_id: str, after: int, limit: int
    ) -> list[OutboxRecord]:
        async with self._lock:
            rows = self._connection.execute(
                """SELECT * FROM provider_outbox
                WHERE run_id=? AND attempt_id=? AND sequence>?
                ORDER BY sequence LIMIT ?""",
                (run_id, attempt_id, after, limit),
            ).fetchall()
            return [outbox(row) for row in rows]

    async def mark_outbox_delivered(
        self, run_id: str, attempt_id: str, sequence: int, now: datetime
    ) -> None:
        cursor = self._connection.execute(
            """UPDATE provider_outbox SET delivered_at=?
            WHERE run_id=? AND attempt_id=? AND sequence=? AND delivered_at IS NULL""",
            (timestamp(now), run_id, attempt_id, sequence),
        )
        if cursor.rowcount != 1:
            raise ProviderConflict("provider outbox conflict")

    async def load(self, run_id: str, attempt_id: str) -> CompletionRecord | None:
        async with self._lock:
            row = self._connection.execute(
                "SELECT * FROM worker_completions WHERE run_id=? AND attempt_id=?",
                (run_id, attempt_id),
            ).fetchone()
            if row is None:
                return None
            return CompletionRecord(
                row["run_id"],
                row["attempt_id"],
                row["claim_id"],
                row["completion_id"],
                row["idempotency_key"],
                row["result_json"],
                tuple(json.loads(row["effect_ids_json"])),
            )

    async def save(self, record: CompletionRecord) -> None:
        async with self._lock:
            try:
                self._connection.execute(
                    "INSERT INTO worker_completions VALUES (?,?,?,?,?,?,?)",
                    (
                        record.run_id,
                        record.attempt_id,
                        record.claim_id,
                        record.completion_id,
                        record.idempotency_key,
                        record.result_json,
                        json.dumps(record.effect_ids, separators=(",", ":")).encode(),
                    ),
                )
            except sqlite3.IntegrityError:
                row = self._connection.execute(
                    "SELECT * FROM worker_completions WHERE run_id=? AND attempt_id=?",
                    (record.run_id, record.attempt_id),
                ).fetchone()
                prior = None if row is None else CompletionRecord(
                    row["run_id"], row["attempt_id"], row["claim_id"],
                    row["completion_id"], row["idempotency_key"], row["result_json"],
                    tuple(json.loads(row["effect_ids_json"])),
                )
                if prior != record:
                    raise ProviderConflict("worker completion conflict") from None

    async def delete(self, run_id: str, attempt_id: str) -> None:
        async with self._lock:
            self._connection.execute(
                "DELETE FROM worker_completions WHERE run_id=? AND attempt_id=?",
                (run_id, attempt_id),
            )

    def _verify_schema(self) -> None:
        if self._connection.execute("PRAGMA foreign_keys").fetchone()[0] != 1:
            raise RuntimeError("sqlite foreign keys disabled")
        checksum = migration_digest(self._migration)
        history = self._connection.execute(
            "SELECT version,checksum FROM provider_schema_history ORDER BY version"
        ).fetchall()
        if [(row[0], row[1]) for row in history] != [(1, checksum)]:
            raise RuntimeError("provider migration history mismatch")
        oracle = sqlite3.connect(":memory:", isolation_level=None)
        try:
            oracle.execute("PRAGMA foreign_keys = ON")
            for statement in migration_statements(self._migration):
                oracle.execute(statement)
            oracle.execute(
                "INSERT INTO provider_schema_history(version,checksum,applied_at) VALUES(1,?,?)",
                (checksum, "1970-01-01T00:00:01.000000Z"),
            )
            if schema_objects(self._connection) != schema_objects(oracle):
                raise RuntimeError("provider database schema mismatch")
        finally:
            oracle.close()
        if self._connection.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
            raise RuntimeError("provider database integrity failure")
        if self._connection.execute("PRAGMA foreign_key_check").fetchone() is not None:
            raise RuntimeError("provider database foreign key violation")

    def _migrate(self) -> None:
        checksum = migration_digest(self._migration)
        self._connection.execute("BEGIN IMMEDIATE")
        try:
            history = self._connection.execute(
                "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='provider_schema_history'"
            ).fetchone()[0]
            if history == 0:
                objects = self._connection.execute(
                    "SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'"
                ).fetchone()[0]
                if objects != 0:
                    raise RuntimeError("provider database is not empty")
                for statement in migration_statements(self._migration):
                    self._connection.execute(statement)
                self._connection.execute(
                    "INSERT INTO provider_schema_history(version,checksum,applied_at) VALUES(1,?,?)",
                    (checksum, timestamp(datetime.now(UTC))),
                )
            elif history == 1:
                rows = self._connection.execute(
                    "SELECT version,checksum FROM provider_schema_history ORDER BY version"
                ).fetchall()
                if [(row[0], row[1]) for row in rows] != [(1, checksum)]:
                    raise RuntimeError("provider migration history mismatch")
            else:
                raise RuntimeError("provider migration history is ambiguous")
            self._connection.execute("COMMIT")
        except BaseException:
            self._connection.execute("ROLLBACK")
            raise
        self._verify_schema()


def inbox_values(record: InboxRecord) -> tuple[object, ...]:
    return (
        record.run_id,
        record.attempt_id,
        record.request_digest,
        record.authorization_digest,
        record.request_json,
        record.result_json,
        record.agent_id,
        record.agent_version,
        record.skill_id,
        record.deployment_id,
        record.instance_id,
        record.generation,
        record.fencing_token,
        record.state_version,
        record.traceparent,
        record.tracestate,
        record.state.value,
        timestamp(record.deadline_at),
        timestamp(record.created_at),
        timestamp(record.updated_at),
        optional_time(record.cancel_requested_at),
    )


def reject_symlink_components(path: Path) -> None:
    """Reject an existing symlink at any point in an absolute database path."""
    current = Path(path.anchor)
    components = path.parts[1:]
    for index, part in enumerate(components):
        current /= part
        try:
            mode = os.lstat(current).st_mode
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(mode):
            raise ValueError("database path must be absolute and non-symlink")
        if index + 1 == len(components):
            if not stat.S_ISREG(mode):
                raise ValueError("database path must name a regular file")
        elif not stat.S_ISDIR(mode):
            raise ValueError("database parent must be a directory")


def migration_statements(script: bytes) -> list[str]:
    try:
        text = script.decode("utf-8", errors="strict")
    except UnicodeDecodeError as error:
        raise RuntimeError("provider migration is not UTF-8") from error
    statements: list[str] = []
    buffer = ""
    for line in text.splitlines(keepends=True):
        buffer += line
        if sqlite3.complete_statement(buffer):
            statement = buffer.strip()
            if statement:
                statements.append(statement)
            buffer = ""
    if buffer.strip() or not statements:
        raise RuntimeError("provider migration contains an incomplete statement")
    return statements


def migration_digest(script: bytes) -> str:
    return "sha256:" + hashlib.sha256(script).hexdigest()


def schema_objects(connection: sqlite3.Connection) -> list[tuple[str, str, str, str]]:
    rows = connection.execute(
        "SELECT type,name,tbl_name,coalesce(sql,'') FROM sqlite_master "
        "WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name"
    ).fetchall()
    return [
        (str(row[0]), str(row[1]), str(row[2]), " ".join(str(row[3]).lower().split()))
        for row in rows
    ]


def inbox(row: sqlite3.Row) -> InboxRecord:
    return InboxRecord(
        run_id=row["run_id"],
        attempt_id=row["attempt_id"],
        request_digest=row["request_digest"],
        authorization_digest=row["authorization_digest"],
        request_json=row["request_json"],
        result_json=row["result_json"],
        agent_id=row["agent_id"],
        agent_version=row["agent_version"],
        skill_id=row["skill_id"],
        deployment_id=row["deployment_id"],
        instance_id=row["instance_id"],
        generation=row["generation"],
        fencing_token=row["fencing_token"],
        state_version=row["state_version"],
        traceparent=row["traceparent"],
        tracestate=row["tracestate"],
        state=InboxState(row["state"]),
        deadline_at=parse_time(row["deadline_at"]),
        created_at=parse_time(row["created_at"]),
        updated_at=parse_time(row["updated_at"]),
        cancel_requested_at=None
        if row["cancel_requested_at"] is None
        else parse_time(row["cancel_requested_at"]),
    )


def outbox(row: sqlite3.Row) -> OutboxRecord:
    return OutboxRecord(
        row["run_id"],
        row["attempt_id"],
        row["event_id"],
        row["event_type"],
        row["sequence"],
        row["envelope"],
        parse_time(row["created_at"]),
        None if row["delivered_at"] is None else parse_time(row["delivered_at"]),
    )


def timestamp(value: datetime) -> str:
    return value.astimezone(UTC).isoformat(timespec="microseconds").replace("+00:00", "Z")


def optional_time(value: datetime | None) -> str | None:
    return None if value is None else timestamp(value)


def parse_time(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00")).astimezone(UTC)
