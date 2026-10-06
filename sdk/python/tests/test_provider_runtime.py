from __future__ import annotations

import asyncio
from datetime import UTC, datetime, timedelta
import json
import os
from pathlib import Path
import tempfile
import sqlite3
import unittest

from arop.asgi import ASGIRuntime
from arop.generated.run import run_gen as wire
from arop.generated.streaming import streaming_gen as stream_wire
from arop.provider import Claims, Execution, InboxRecord, ProviderRuntime
from arop.provider.durable_store import InboxState, ProviderNotFound
from arop.provider.runtime import AuthenticationError, sha256_digest
from arop.worker import CompletionRecord
from arop_agent.storage import SQLiteStore


UUID = "018f5f6e-7b1c-7abc-8def-0123456789ab"
RUN_ID = "run_" + UUID
ATTEMPT_ID = "att_" + UUID
DEPLOYMENT_ID = "dep_" + UUID


def migration() -> Path:
    return (
        Path(__file__).resolve().parents[3]
        / "reference"
        / "agents"
        / "python-http"
        / "migrations"
        / "sqlite"
        / "0001_provider_state.sql"
    )


def run_request(deadline: datetime | None = None) -> wire.RunRequest:
    return wire.RunRequest(
        agent=wire.AgentBinding(
            id="echo.agent",
            manifest_digest="sha256:" + "a" * 64,
            skill_id="echo",
            version="1.0.0",
        ),
        deadline_at=(deadline or datetime.now(UTC) + timedelta(minutes=5))
        .isoformat()
        .replace("+00:00", "Z"),
        effects=wire.EffectsNone(level="none"),
        input=[wire.AROPV1ContentPartText(text="hello", type="text")],
        schema_version=1,
        trace=wire.AROPV1W3CTraceContext(
            traceparent="00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
        ),
    )


def claims(scopes: tuple[str, ...] = ("agent:invoke", "run:stream")) -> Claims:
    now = datetime.now(UTC)
    return Claims(
        issuer="https://issuer.invalid",
        audience="https://control.invalid/deployments/" + DEPLOYMENT_ID,
        subject="prn_" + UUID,
        authorized_party="cred_" + UUID,
        token_id="tok_" + UUID,
        run_id=RUN_ID,
        attempt_id=ATTEMPT_ID,
        agent_id="echo.agent",
        agent_version="1.0.0",
        skill_id="echo",
        deployment_id=DEPLOYMENT_ID,
        instance_id="instance.main",
        transport_profile="proxy",
        endpoint="https://agent.invalid/v1/runs",
        generation=1,
        fencing_token=1,
        scopes=scopes,
        issued_at=now - timedelta(seconds=1),
        expires_at=now + timedelta(minutes=4),
    )


class Verifier:
    def __init__(self) -> None:
        self.calls: list[tuple[str, str, str]] = []
        self.value = claims()

    async def verify(self, token, method, path, required_scope, now):
        if token != "opaque-token":
            raise ValueError("bad token")
        self.calls.append((method, path, required_scope))
        return self.value


class Handler:
    def __init__(self) -> None:
        self.calls = 0
        self.effects = 0

    async def execute(self, execution: Execution, request: wire.RunRequest) -> wire.RunResult:
        self.calls += 1

        async def operation():
            self.effects += 1
            return {"receipt": "ok"}

        replay = await execution.effect("eff_send.receipt", {"value": 1}, operation)
        self.assert_replay = replay
        await execution.emit(
            "evt_" + UUID.replace("-", ""),
            "io.arop.progress.updated.v1",
            {"progress": 100},
        )
        return wire.RunResult(
            completed_at=datetime.now(UTC).isoformat().replace("+00:00", "Z"),
            run_id=RUN_ID,
            schema_version=1,
            state="succeeded",
            usage=wire.Usage(duration_ms=1, input_tokens=1, output_tokens=1),
            snapshot=wire.Snapshot(
                content=list(request.input), digest="sha256:" + "b" * 64, revision=1
            ),
        )


class SlowHandler:
    async def execute(self, execution, request):
        await asyncio.Event().wait()
        return wire.RunResult(
            completed_at=datetime.now(UTC).isoformat().replace("+00:00", "Z"),
            run_id=RUN_ID,
            schema_version=1,
            state="succeeded",
            usage=wire.Usage(duration_ms=1, input_tokens=1, output_tokens=1),
            snapshot=wire.Snapshot(
                content=list(request.input), digest="sha256:" + "b" * 64, revision=1
            ),
        )


class ProviderRuntimeTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.store = SQLiteStore(Path(self.temporary.name).resolve() / "state.db", migration())
        self.verifier = Verifier()
        self.handler = Handler()
        self.runtime = ProviderRuntime(self.store, self.verifier, self.handler)

    async def asyncTearDown(self) -> None:
        await self.runtime.drain(1)
        await self.store.close()
        self.temporary.cleanup()

    async def test_create_replay_effect_and_outbox_are_durable(self) -> None:
        body = wire.encode_run_request(run_request()).encode()
        first = await self.runtime.create("opaque-token", ATTEMPT_ID, body)
        second = await self.runtime.create("opaque-token", ATTEMPT_ID, body)
        self.assertEqual((first.status, second.status), (202, 202))
        await self.runtime.drain(2)
        record = await self.store.get_inbox(RUN_ID, ATTEMPT_ID)
        self.assertEqual(record.state, InboxState.SUCCEEDED)
        status = wire.decode_run_status((await self.runtime.status("opaque-token", RUN_ID)).body)
        self.assertEqual(str(status.run_id), RUN_ID)
        self.assertEqual(str(status.state), "succeeded")
        self.assertEqual(self.handler.calls, 1)
        self.assertEqual(self.handler.effects, 1)
        events = await self.store.list_outbox(RUN_ID, ATTEMPT_ID, 0, 20)
        self.assertGreaterEqual(len(events), 3)
        self.assertEqual([item.sequence for item in events], list(range(1, len(events) + 1)))
        self.assertNotIn(b"opaque-token", (Path(self.temporary.name) / "state.db").read_bytes())

    async def test_idempotency_digest_conflict_fails_closed(self) -> None:
        body = wire.encode_run_request(run_request()).encode()
        self.assertEqual((await self.runtime.create("opaque-token", ATTEMPT_ID, body)).status, 202)
        changed = run_request()
        changed.input[0].text = "different"
        conflict = await self.runtime.create(
            "opaque-token", ATTEMPT_ID, wire.encode_run_request(changed).encode()
        )
        self.assertEqual(conflict.status, 409)

    async def test_deadline_cancels_handler_and_persists_timed_out_result(self) -> None:
        runtime = ProviderRuntime(self.store, self.verifier, SlowHandler())
        body = wire.encode_run_request(
            run_request(datetime.now(UTC) + timedelta(milliseconds=20))
        ).encode()
        self.assertEqual(
            (await runtime.create("opaque-token", ATTEMPT_ID, body)).status, 202
        )
        await runtime.drain(1)
        record = await self.store.get_inbox(RUN_ID, ATTEMPT_ID)
        self.assertEqual(record.state, InboxState.TIMED_OUT)
        result = wire.decode_run_result(record.result_json)
        self.assertEqual(str(result.state), "timed_out")

    async def test_recover_accepted_record_after_restart(self) -> None:
        now = datetime.now(UTC)
        request = wire.encode_run_request(run_request()).encode()
        value = claims()
        record = InboxRecord(
            RUN_ID,
            ATTEMPT_ID,
            sha256_digest(request),
            "sha256:" + "c" * 64,
            request,
            None,
            value.agent_id,
            value.agent_version,
            value.skill_id,
            value.deployment_id,
            value.instance_id,
            1,
            1,
            1,
            "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
            "",
            InboxState.ACCEPTED,
            now + timedelta(minutes=5),
            now,
            now,
        )

        async def insert(tx):
            await tx.create_inbox(record)

        await self.store.within(insert)
        await self.runtime.recover()
        await self.runtime.drain(2)
        self.assertEqual((await self.store.get_inbox(RUN_ID, ATTEMPT_ID)).state, InboxState.SUCCEEDED)

    async def test_authentication_error_never_contains_token(self) -> None:
        with self.assertRaises(AuthenticationError) as caught:
            await self.runtime.status("secret-bearer", RUN_ID)
        self.assertNotIn("secret-bearer", str(caught.exception))

    async def test_sqlite_rollback_and_readiness(self) -> None:
        now = datetime.now(UTC)
        value = claims()
        record = InboxRecord(
            RUN_ID,
            ATTEMPT_ID,
            "sha256:" + "a" * 64,
            "sha256:" + "b" * 64,
            wire.encode_run_request(run_request()).encode(),
            None,
            value.agent_id,
            value.agent_version,
            value.skill_id,
            value.deployment_id,
            value.instance_id,
            1,
            1,
            1,
            "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
            "",
            InboxState.ACCEPTED,
            now + timedelta(minutes=1),
            now,
            now,
        )

        async def rollback(tx):
            await tx.create_inbox(record)
            raise RuntimeError("rollback")

        with self.assertRaisesRegex(RuntimeError, "rollback"):
            await self.store.within(rollback)
        with self.assertRaises(ProviderNotFound):
            await self.store.get_inbox(RUN_ID, ATTEMPT_ID)
        await self.store.ready()
        self.store._connection.execute("PRAGMA foreign_keys = OFF")
        with self.assertRaises(RuntimeError):
            await self.store.ready()

    async def test_sqlite_reopen_history_and_schema_tamper_fail_closed(self) -> None:
        database = Path(self.temporary.name).resolve() / "state.db"
        completion = CompletionRecord(
            run_id=RUN_ID,
            attempt_id=ATTEMPT_ID,
            claim_id="clm_" + UUID,
            completion_id="cmp_" + UUID,
            idempotency_key="complete-" + UUID,
            result_json=b'{"state":"succeeded"}',
            effect_ids=("eff_restart-safe",),
        )
        await self.store.save(completion)
        await self.store.close()
        self.store = SQLiteStore(database, migration())
        await self.store.ready()
        self.assertEqual(await self.store.load(RUN_ID, ATTEMPT_ID), completion)
        self.store._connection.execute("CREATE TABLE unexpected(value TEXT) STRICT")
        with self.assertRaisesRegex(RuntimeError, "schema mismatch"):
            await self.store.ready()

    async def test_sqlite_rejects_dirty_unversioned_database(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            database = Path(temporary).resolve() / "dirty.db"
            connection = sqlite3.connect(database)
            connection.execute("CREATE TABLE unexpected(value TEXT)")
            connection.close()
            with self.assertRaisesRegex(RuntimeError, "not empty"):
                SQLiteStore(database, migration())

    async def test_sqlite_rejects_symlinked_parent(self) -> None:
        with tempfile.TemporaryDirectory() as outside:
            link = Path(self.temporary.name).resolve() / "linked"
            os.symlink(outside, link)
            with self.assertRaisesRegex(ValueError, "non-symlink"):
                SQLiteStore(link / "state.db", migration())


class ASGITests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.store = SQLiteStore(Path(self.temporary.name).resolve() / "state.db", migration())
        self.runtime = ProviderRuntime(self.store, Verifier(), Handler())
        self.app = ASGIRuntime(self.runtime, max_body_bytes=4096)

    async def asyncTearDown(self) -> None:
        await self.runtime.drain(1)
        await self.store.close()
        self.temporary.cleanup()

    async def test_asgi_create_and_strict_headers(self) -> None:
        body = wire.encode_run_request(run_request()).encode()
        messages = [{"type": "http.request", "body": body, "more_body": False}]
        sent: list[dict[str, object]] = []

        async def receive():
            return messages.pop(0)

        async def send(message):
            sent.append(message)

        await self.app(
            {
                "type": "http",
                "method": "POST",
                "path": "/v1/runs",
                "query_string": b"",
                "headers": [
                    (b"content-type", b"application/json"),
                    (b"authorization", b"Bearer opaque-token"),
                    (b"idempotency-key", ATTEMPT_ID.encode()),
                ],
            },
            receive,
            send,
        )
        self.assertEqual(sent[0]["status"], 202)
        self.assertIn((b"cache-control", b"no-store"), sent[0]["headers"])

    async def test_asgi_oversize_and_duplicate_header_fail_closed(self) -> None:
        async def send(message):
            sent.append(message)

        sent: list[dict[str, object]] = []
        chunks = [{"type": "http.request", "body": b"x" * 4097, "more_body": False}]

        async def receive():
            return chunks.pop(0)

        await self.app(
            {
                "type": "http",
                "method": "POST",
                "path": "/v1/runs",
                "query_string": b"",
                "headers": [
                    (b"content-type", b"application/json"),
                    (b"authorization", b"Bearer opaque-token"),
                    (b"idempotency-key", ATTEMPT_ID.encode()),
                ],
            },
            receive,
            send,
        )
        self.assertEqual(sent[0]["status"], 413)

        sent.clear()

        async def no_body():
            raise AssertionError("invalid headers must fail before reading the body")

        await self.app(
            {
                "type": "http",
                "method": "POST",
                "path": "/v1/runs",
                "query_string": b"",
                "headers": [
                    (b"content-type", b"application/json"),
                    (b"content-type", b"application/json"),
                    (b"authorization", b"Bearer opaque-token"),
                    (b"idempotency-key", ATTEMPT_ID.encode()),
                ],
            },
            no_body,
            send,
        )
        self.assertEqual(sent[0]["status"], 400)

    async def test_stream_authentication_fails_before_response_start(self) -> None:
        sent: list[dict[str, object]] = []

        async def receive():
            raise AssertionError("GET must not read a request body")

        async def send(message):
            sent.append(message)

        await self.app(
            {
                "type": "http",
                "method": "GET",
                "path": f"/v1/runs/{RUN_ID}/events",
                "query_string": b"",
                "headers": [
                    (b"accept", b"text/event-stream"),
                    (b"authorization", b"Bearer invalid-token"),
                ],
            },
            receive,
            send,
        )
        self.assertEqual([message.get("status") for message in sent], [401, None])

    async def test_stream_emits_bound_cloudevent_envelopes(self) -> None:
        body = wire.encode_run_request(run_request()).encode()
        self.assertEqual(
            (await self.runtime.create("opaque-token", ATTEMPT_ID, body)).status, 202
        )
        await self.runtime.drain(2)
        sent: list[dict[str, object]] = []

        async def receive():
            raise AssertionError("GET must not read a request body")

        async def send(message):
            sent.append(message)

        await self.app(
            {
                "type": "http",
                "method": "GET",
                "path": f"/v1/runs/{RUN_ID}/events",
                "query_string": b"",
                "headers": [
                    (b"accept", b"text/event-stream"),
                    (b"authorization", b"Bearer opaque-token"),
                    (b"last-event-id", b"0"),
                ],
            },
            receive,
            send,
        )
        self.assertEqual(sent[0]["status"], 200)
        envelopes = []
        for message in sent[1:]:
            payload = message.get("body", b"")
            if not payload:
                continue
            lines = payload.decode().splitlines()
            sequence = int(lines[0].removeprefix("id: "))
            event_type = lines[1].removeprefix("event: ")
            envelope = stream_wire.decode_stream_event(
                lines[2].removeprefix("data: ")
            )
            self.assertEqual(int(envelope.producersequence), sequence)
            self.assertEqual(str(envelope.type), event_type)
            self.assertEqual(str(envelope.runid), RUN_ID)
            self.assertEqual(str(envelope.attemptid), ATTEMPT_ID)
            self.assertIsInstance(envelope.runsequence, stream_wire.UnsetType)
            envelopes.append(envelope)
        self.assertEqual(
            [str(event.type) for event in envelopes],
            [
                "io.arop.run.accepted.v1",
                "io.arop.run.started.v1",
                "io.arop.progress.updated.v1",
                "io.arop.run.succeeded.v1",
            ],
        )
        self.assertEqual(envelopes[-1].data["state"], "succeeded")
        self.assertIn("usage", envelopes[-1].data)
        self.assertIn("completed_at", envelopes[-1].data)

        ahead: list[dict[str, object]] = []

        async def send_ahead(message):
            ahead.append(message)

        await self.app(
            {
                "type": "http",
                "method": "GET",
                "path": f"/v1/runs/{RUN_ID}/events",
                "query_string": b"",
                "headers": [
                    (b"accept", b"text/event-stream"),
                    (b"authorization", b"Bearer opaque-token"),
                    (b"last-event-id", b"999"),
                ],
            },
            receive,
            send_ahead,
        )
        self.assertEqual([message.get("status") for message in ahead], [409, None])


if __name__ == "__main__":
    unittest.main()
