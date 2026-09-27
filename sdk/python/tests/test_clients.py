from __future__ import annotations

import asyncio
from dataclasses import replace
from datetime import UTC, datetime, timedelta
import json
import unittest

from arop._transport import Response, TransportUnavailable
from arop.generated.registry import registry_gen as registry
from arop.generated.worker import worker_gen as worker
from arop.registry.registration import RegistrationClient, RegistrationLoop, RegistrationState
from arop.worker import WorkerClient, WorkerOutcome, WorkerRunner, effect_id

UUID = "018f5f6e-7b1c-7abc-8def-0123456789ab"


class Credential:
    async def credential(self) -> str:
        return "opaque-credential"


class QueueTransport:
    def __init__(self, responses: list[Response]) -> None:
        self.responses = responses
        self.requests: list[tuple[str, str, dict[str, str], bytes]] = []

    async def request(self, method, url, headers, body, timeout):
        self.requests.append((method, url, dict(headers), body))
        if not self.responses:
            raise AssertionError("unexpected request")
        return self.responses.pop(0)


def runtime_instance(
    *, draining: bool = False, status: str = "registered"
) -> registry.RuntimeInstance:
    return registry.RuntimeInstance(
        bindings=[
            registry.Binding(
                agent_id="echo.agent",
                agent_version="1.0.0",
                manifest_digest="sha256:" + "a" * 64,
                skill_ids=["echo"],
            )
        ],
        draining=draining,
        endpoint=registry.Endpoint(base_url="https://agent.invalid", health_path="/health/ready"),
        environment="production",
        generation=1,
        instance_id="instance.main",
        lease_expires_at=(datetime.now(UTC) + timedelta(minutes=1)).isoformat().replace("+00:00", "Z"),
        lease_id="lease_" + UUID,
        operator=registry.Operator(enabled=True, priority=0, weight=100),
        registry_revision=1,
        resource_version=1,
        runtime=registry.Runtime(
            capabilities=registry.Capabilities(
                cancellation=True,
                event_outbox="durable",
                status_query=True,
                stream_resume=True,
                streaming=True,
            ),
            capacity=registry.Capacity(
                active_runs=0,
                available_slots=1,
                max_concurrency=1,
                max_queue_depth=10,
                queue_depth=0,
            ),
            healthy=True,
            labels=registry.RuntimeLabels(),
            protocol_versions=["1.0"],
            ready=True,
            runtime_version="1.0.0",
            transport_profiles=["control-plane-proxy"],
        ),
        schema_version=1,
        service_id="echo.service",
        session_id="ses_" + UUID,
        status=status,
    )


def register_request(instance: registry.RuntimeInstance) -> dict[str, object]:
    value = json.loads(registry.encode_runtime_instance(instance))
    return {
        key: value[key]
        for key in (
            "schema_version",
            "session_id",
            "service_id",
            "environment",
            "endpoint",
            "bindings",
            "runtime",
        )
    }


class RegistrationTests(unittest.IsolatedAsyncioTestCase):
    async def test_register_keepalive_drain_deregister(self) -> None:
        instance = runtime_instance()
        registration_body = json.dumps(
            {
                "schema_version": 1,
                "instance": json.loads(registry.encode_runtime_instance(instance)),
                "lease_ttl_seconds": 60,
                "keepalive_interval_seconds": 20,
                "replay": False,
            },
            separators=(",", ":"),
        ).encode()
        lease = registry.RegistryLease(
            generation=1,
            heartbeat_sequence=1,
            instance_id="instance.main",
            keepalive_interval_seconds=20,
            lease_expires_at=(datetime.now(UTC) + timedelta(minutes=1)).isoformat().replace("+00:00", "Z"),
            lease_id="lease_" + UUID,
            lease_ttl_seconds=60,
            registry_revision=2,
            schema_version=1,
            server_time=datetime.now(UTC).isoformat().replace("+00:00", "Z"),
            session_id="ses_" + UUID,
        )
        transport = QueueTransport(
            [
                Response(201, {"content-type": ("application/json",)}, registration_body),
                Response(200, {"content-type": ("application/json",)}, registry.encode_registry_lease(lease).encode()),
                Response(200, {"content-type": ("application/json",)}, registry.encode_runtime_instance(runtime_instance(draining=True)).encode()),
                Response(200, {"content-type": ("application/json",)}, registry.encode_runtime_instance(runtime_instance(draining=True, status="deregistered")).encode()),
            ]
        )
        client = RegistrationClient(
            "https://control.invalid", Credential(), transport=transport
        )
        state = await client.register(
            "instance.main",
            "ses_" + UUID,
            register_request(instance),
        )
        self.assertEqual(state.generation, 1)
        state = await client.keepalive(
            state, ready=True, active_runs=0, available_slots=1, queue_depth=0
        )
        self.assertEqual(state.heartbeat_sequence, 1)
        await client.drain(state, datetime.now(UTC) + timedelta(seconds=30))
        await client.deregister(state)
        self.assertEqual(len(transport.requests), 4)
        for _, _, headers, body in transport.requests:
            self.assertEqual(headers["Authorization"], "Bearer opaque-credential")
            self.assertNotIn(b"opaque-credential", body)

    async def test_registration_error_is_redacted(self) -> None:
        transport = QueueTransport(
            [
                Response(
                    503,
                    {"content-type": ("application/json",), "retry-after": ("1",)},
                    b'{"category":"dependency","code":"DEPENDENCY_UNAVAILABLE","message":"password=secret","retryable":true}',
                )
            ]
        )
        client = RegistrationClient("https://control.invalid", Credential(), transport=transport)
        with self.assertRaises(Exception) as caught:
            await client.register(
                "instance.main", "ses_" + UUID, register_request(runtime_instance())
            )
        self.assertNotIn("password", str(caught.exception))

    async def test_registration_loop_reregisters_after_keepalive_failure(self) -> None:
        class LoopClient:
            def __init__(self):
                self.registrations = 0
                self.keepalives = 0
                self.drained = False

            async def register(self, instance_id, key, request):
                self.registrations += 1
                return RegistrationState(
                    instance_id,
                    request["session_id"],
                    "lease_" + UUID,
                    self.registrations,
                    0,
                    0.001,
                    1,
                )

            async def keepalive(self, state, **capacity):
                self.keepalives += 1
                if self.keepalives == 1:
                    raise TransportUnavailable("disconnected")
                return replace(state, heartbeat_sequence=state.heartbeat_sequence + 1)

            async def drain(self, state, deadline):
                self.drained = True

            async def deregister(self, state):
                self.drained = self.drained and True

        client = LoopClient()
        loop = RegistrationLoop(
            client,
            "instance.main",
            lambda: register_request(runtime_instance()),
            lambda: (True, 0, 1, 0),
            session_id="ses_" + UUID,
            backoff_minimum=0.001,
            backoff_maximum=0.002,
        )
        task = asyncio.create_task(loop.run())
        for _ in range(200):
            if client.registrations >= 2 and client.keepalives >= 2:
                break
            await asyncio.sleep(0.001)
        await loop.drain(0.1)
        await asyncio.wait_for(task, 0.2)
        self.assertGreaterEqual(client.registrations, 2)
        self.assertTrue(client.drained)


def worker_request() -> worker.AROPV1RunRequest:
    return worker.AROPV1RunRequest(
        agent=worker.AgentBinding(
            id="echo.agent",
            manifest_digest="sha256:" + "a" * 64,
            skill_id="echo",
            version="1.0.0",
        ),
        deadline_at=(datetime.now(UTC) + timedelta(minutes=5)).isoformat().replace("+00:00", "Z"),
        effects=worker.EffectsNone(level="none"),
        input=[worker.AROPV1ContentPartText(text="hello", type="text")],
        schema_version=1,
        trace=worker.AROPV1W3CTraceContext(
            traceparent="00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
        ),
    )


def claim() -> worker.WorkerClaim:
    return worker.WorkerClaim(
        attempt_id="att_" + UUID,
        attempt_number=1,
        claim_id="clm_" + UUID,
        claimed_at=datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        fencing_token=1,
        generation=1,
        lease_expires_at=(datetime.now(UTC) + timedelta(minutes=1)).isoformat().replace("+00:00", "Z"),
        lease_token="wlt_" + "A" * 43,
        run_id="run_" + UUID,
        run_request=worker_request(),
        schema_version=1,
        session_id="ses_" + UUID,
        worker_id="worker.main",
    )


class WorkerClientTests(unittest.IsolatedAsyncioTestCase):
    async def test_claim_complete_and_fencing(self) -> None:
        value = claim()
        transport = QueueTransport(
            [
                Response(200, {"cache-control": ("no-store",), "content-type": ("application/json",)}, worker.encode_worker_claim(value).encode()),
                Response(204, {}, b""),
            ]
        )
        client = WorkerClient("https://control.invalid", Credential(), transport=transport)
        request = worker.WorkerClaimRequest(
            available_slots=1,
            generation=1,
            schema_version=1,
            session_id=value.session_id,
            supported_bindings=[value.run_request.agent],
            wait_seconds=0,
            lease_seconds=60,
        )
        received = await client.claim("worker.main", request)
        self.assertEqual(received.claim_id, value.claim_id)
        result = worker.AROPV1TerminalRunResult(
            completed_at=datetime.now(UTC).isoformat().replace("+00:00", "Z"),
            run_id=value.run_id,
            schema_version=1,
            state="succeeded",
            usage=worker.Usage(duration_ms=1, input_tokens=1, output_tokens=1),
            snapshot=worker.Snapshot(
                content=[worker.AROPV1ContentPartText(text="ok", type="text")],
                digest="sha256:" + "b" * 64,
                revision=1,
            ),
        )
        complete = worker.WorkerComplete(
            attempt_id=value.attempt_id,
            claim_id=value.claim_id,
            completed_at=datetime.now(UTC).isoformat().replace("+00:00", "Z"),
            completion_id="cmp_" + UUID,
            fencing_token=1,
            lease_token=value.lease_token,
            result=result,
            schema_version=1,
        )
        await client.complete("worker.main", str(value.claim_id), "completion-key-123", complete)
        self.assertEqual(len(transport.requests), 2)
        self.assertNotIn(b"opaque-credential", transport.requests[1][3])


class MemoryCompletions:
    def __init__(self) -> None:
        self.value = None

    async def load(self, run_id, attempt_id):
        return self.value

    async def save(self, value):
        self.value = value

    async def delete(self, run_id, attempt_id):
        self.value = None


class RunnerClient:
    def __init__(self) -> None:
        self.completions = []
        self.releases = []

    async def complete(self, worker_id, claim_id, key, request):
        self.completions.append((key, request.completion_id, request.lease_token))
        if len(self.completions) == 1:
            raise TransportUnavailable("ambiguous completion")

    async def renew(self, worker_id, claim_id, request):
        return claim()

    async def release(self, worker_id, claim_id, request):
        self.releases.append(request.reason)


class RunnerHandler:
    def __init__(self) -> None:
        self.calls = 0

    async def handle(self, value):
        self.calls += 1
        return WorkerOutcome(
            worker.AROPV1TerminalRunResult(
                completed_at=datetime.now(UTC).isoformat().replace("+00:00", "Z"),
                run_id=value.run_id,
                schema_version=1,
                state="succeeded",
                usage=worker.Usage(duration_ms=1, input_tokens=1, output_tokens=1),
                snapshot=worker.Snapshot(
                    content=[worker.AROPV1ContentPartText(text="ok", type="text")],
                    digest="sha256:" + "b" * 64,
                    revision=1,
                ),
            ),
            (effect_id(str(value.run_id), "send"),),
        )


class BlockingHandler:
    async def handle(self, value):
        await asyncio.Event().wait()


class WorkerRunnerTests(unittest.IsolatedAsyncioTestCase):
    async def test_drain_cancels_blocked_claim(self) -> None:
        class BlockingClaimClient(RunnerClient):
            def __init__(self) -> None:
                super().__init__()
                self.started = asyncio.Event()
                self.cancelled = False

            async def claim(self, worker_id, request):
                self.started.set()
                try:
                    await asyncio.Event().wait()
                except asyncio.CancelledError:
                    self.cancelled = True
                    raise

        api = BlockingClaimClient()
        runner = WorkerRunner(
            api,
            RunnerHandler(),
            MemoryCompletions(),
            worker_id="worker.main",
            session_id=claim().session_id,
            generation=1,
            supported_bindings=[claim().run_request.agent],
        )
        running = asyncio.create_task(runner.run())
        await asyncio.wait_for(api.started.wait(), 0.2)
        await runner.drain(0.2)
        await asyncio.wait_for(running, 0.2)
        self.assertTrue(api.cancelled)

    async def test_completion_retry_reuses_identity_and_clears_outbox(self) -> None:
        api = RunnerClient()
        handler = RunnerHandler()
        store = MemoryCompletions()
        runner = WorkerRunner(
            api,
            handler,
            store,
            worker_id="worker.main",
            session_id=claim().session_id,
            generation=1,
            supported_bindings=[claim().run_request.agent],
            renew_interval=59,
            backoff_minimum=0.001,
            backoff_maximum=0.002,
        )
        await runner._handle(claim())
        self.assertEqual(handler.calls, 1)
        self.assertEqual(len(api.completions), 2)
        self.assertEqual(api.completions[0][:2], api.completions[1][:2])
        self.assertIsNone(store.value)

    def test_effect_id_is_attempt_independent(self) -> None:
        first = effect_id("run_" + UUID, "send")
        second = effect_id("run_" + UUID, "send")
        self.assertEqual(first, second)
        self.assertTrue(first.startswith("eff_"))

    async def test_renewal_failure_cancels_handler_and_releases_claim(self) -> None:
        class FailingRenewClient(RunnerClient):
            async def renew(self, worker_id, claim_id, request):
                raise TransportUnavailable("renewal failed")

        api = FailingRenewClient()
        runner = WorkerRunner(
            api,
            BlockingHandler(),
            MemoryCompletions(),
            worker_id="worker.main",
            session_id=claim().session_id,
            generation=1,
            supported_bindings=[claim().run_request.agent],
            renew_interval=1,
        )
        runner.renew_interval = 0.001
        await asyncio.wait_for(runner._handle(claim()), 0.2)
        self.assertEqual(api.releases, ["retryable_failure"])

    async def test_expired_claim_releases_without_invoking_handler(self) -> None:
        api = RunnerClient()
        handler = RunnerHandler()
        value = claim()
        value.run_request.deadline_at = (
            datetime.now(UTC) - timedelta(seconds=1)
        ).isoformat().replace("+00:00", "Z")
        runner = WorkerRunner(
            api,
            handler,
            MemoryCompletions(),
            worker_id="worker.main",
            session_id=value.session_id,
            generation=1,
            supported_bindings=[value.run_request.agent],
        )
        await runner._handle(value)
        self.assertEqual(handler.calls, 0)
        self.assertEqual(api.releases, ["retryable_failure"])


if __name__ == "__main__":
    unittest.main()
