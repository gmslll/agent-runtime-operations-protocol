"""Minimal, bounded ASGI HTTP/SSE transport."""

from __future__ import annotations

import asyncio
from datetime import UTC
import hashlib
import json
import re
from typing import Any, Awaitable, Callable

from arop.generated.streaming import streaming_gen as stream_wire
from arop.provider.durable_store import ProviderConflict, ProviderNotFound
from arop.provider.runtime import (
    AuthenticationError,
    ProviderRuntime,
    RuntimeReply,
    canonical_json,
    problem,
)

Receive = Callable[[], Awaitable[dict[str, Any]]]
Send = Callable[[dict[str, Any]], Awaitable[None]]
_RUN_ID = re.compile(
    r"^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)


class ASGIRuntime:
    def __init__(self, runtime: ProviderRuntime, *, max_body_bytes: int = 8 << 20) -> None:
        if max_body_bytes < 1024 or max_body_bytes > 8 << 20:
            raise ValueError("invalid ASGI body limit")
        self.runtime = runtime
        self.max_body_bytes = max_body_bytes

    async def __call__(self, scope: dict[str, Any], receive: Receive, send: Send) -> None:
        if scope.get("type") != "http":
            raise RuntimeError("ASGI runtime only supports HTTP")
        method = str(scope.get("method", ""))
        path = str(scope.get("path", ""))
        query = scope.get("query_string", b"")
        try:
            headers = parse_headers(scope.get("headers", []))
            if query:
                await self._reply(send, problem(400, "INVALID_REQUEST", "validation"))
                return
            if method == "GET" and path == "/health/live":
                await self._reply(send, RuntimeReply(204))
                return
            if method == "GET" and path == "/health/ready":
                try:
                    await self.runtime.store.ready()
                except Exception:
                    await self._reply(send, problem(503, "DEPENDENCY_UNAVAILABLE", "dependency"))
                    return
                await self._reply(send, RuntimeReply(204))
                return
            if method == "POST" and path == "/v1/runs":
                if one_header(headers, "content-type") != "application/json":
                    await self._reply(send, problem(415, "UNSUPPORTED_MEDIA_TYPE", "protocol"))
                    return
                body = await read_body(receive, self.max_body_bytes)
                key = one_header(headers, "idempotency-key")
                token = bearer(headers)
                reply = await self.runtime.create(token, key, body)
                await self._reply(send, reply)
                return
            if path.startswith("/v1/runs/"):
                await self._run_route(method, path, headers, receive, send)
                return
            await self._reply(send, problem(404, "NOT_FOUND", "not_found"))
        except AuthenticationError:
            await self._reply(
                send,
                RuntimeReply(
                    401,
                    problem(401, "AUTHENTICATION_FAILED", "authentication").body,
                    (("www-authenticate", "Bearer"),),
                ),
            )
        except BodyTooLarge:
            await self._reply(send, problem(413, "REQUEST_TOO_LARGE", "validation"))
        except ProviderNotFound:
            await self._reply(send, problem(404, "RUN_NOT_FOUND", "not_found"))
        except ProviderConflict:
            await self._reply(send, problem(409, "STATE_CONFLICT", "conflict"))
        except ValueError:
            await self._reply(send, problem(400, "INVALID_REQUEST", "validation"))
        except Exception:
            await self._reply(send, problem(503, "DEPENDENCY_UNAVAILABLE", "dependency"))

    async def _run_route(
        self,
        method: str,
        path: str,
        headers: dict[str, list[str]],
        receive: Receive,
        send: Send,
    ) -> None:
        remainder = path.removeprefix("/v1/runs/")
        if "/" not in remainder and method == "GET" and _RUN_ID.fullmatch(remainder):
            await self._reply(send, await self.runtime.status(bearer(headers), remainder))
            return
        if remainder.endswith("/commands") and method == "POST":
            run_id = remainder[: -len("/commands")]
            if not _RUN_ID.fullmatch(run_id):
                await self._reply(send, problem(404, "NOT_FOUND", "not_found"))
                return
            if one_header(headers, "content-type") != "application/json":
                await self._reply(
                    send, problem(415, "UNSUPPORTED_MEDIA_TYPE", "protocol")
                )
                return
            body = await read_body(receive, self.max_body_bytes)
            try:
                value = strict_object(body)
                if value.get("schema_version") != 1 or value.get("type") != "run.cancel":
                    raise ValueError("invalid cancel command")
                version = value.get("expected_state_version")
                if isinstance(version, bool) or not isinstance(version, int):
                    raise ValueError("invalid state version")
            except ValueError:
                await self._reply(
                    send, problem(400, "INVALID_RUN_COMMAND", "validation")
                )
                return
            await self._reply(
                send, await self.runtime.cancel(bearer(headers), run_id, version)
            )
            return
        if remainder.endswith("/events") and method == "GET":
            run_id = remainder[: -len("/events")]
            if not _RUN_ID.fullmatch(run_id) or one_header(headers, "accept") != "text/event-stream":
                await self._reply(send, problem(406, "INVALID_ACCEPT", "protocol"))
                return
            cursor = one_header(headers, "last-event-id", required=False)
            try:
                after = 0 if cursor == "" else parse_cursor(cursor)
            except ValueError:
                await self._reply(
                    send, problem(400, "INVALID_STREAM_CURSOR", "validation")
                )
                return
            await self._stream(send, bearer(headers), run_id, after)
            return
        await self._reply(send, problem(404, "NOT_FOUND", "not_found"))

    async def _stream(self, send: Send, token: str, run_id: str, after: int) -> None:
        # Authenticate and resolve the run before committing the response status.
        # Once ASGI emits http.response.start, an authentication or dependency
        # failure can no longer be represented by the typed HTTP error contract.
        record, events = await self.runtime.events(token, run_id, after)
        prepared = [direct_envelope(record, event) for event in events]
        await send(
            {
                "type": "http.response.start",
                "status": 200,
                "headers": [
                    (b"content-type", b"text/event-stream"),
                    (b"cache-control", b"no-store"),
                    (b"x-accel-buffering", b"no"),
                ],
            }
        )
        try:
            idle = 0
            while idle < 300:
                for event, (event_type, envelope) in zip(events, prepared, strict=True):
                    payload = (
                        f"id: {event.sequence}\nevent: {event_type}\ndata: "
                        + envelope.decode("utf-8")
                        + "\n\n"
                    ).encode()
                    await send(
                        {"type": "http.response.body", "body": payload, "more_body": True}
                    )
                    after = event.sequence
                if record.state.terminal and not events:
                    break
                idle += 1
                await asyncio.sleep(0.05)
                record, events = await self.runtime.events(token, run_id, after)
                prepared = [direct_envelope(record, event) for event in events]
        except (AuthenticationError, ProviderConflict, ProviderNotFound, ValueError):
            # The response is already committed. Fail closed by ending the
            # stream instead of attempting an invalid second response start.
            pass
        await send({"type": "http.response.body", "body": b"", "more_body": False})

    async def _reply(self, send: Send, reply: RuntimeReply) -> None:
        headers = [(b"cache-control", b"no-store")]
        if reply.body:
            headers.append((b"content-type", b"application/json"))
        headers.extend((name.encode(), value.encode()) for name, value in reply.headers)
        await send({"type": "http.response.start", "status": reply.status, "headers": headers})
        await send({"type": "http.response.body", "body": reply.body, "more_body": False})


class BodyTooLarge(Exception):
    pass


async def read_body(receive: Receive, limit: int) -> bytes:
    output = bytearray()
    while True:
        message = await receive()
        if message.get("type") == "http.disconnect":
            raise asyncio.CancelledError
        if message.get("type") != "http.request":
            raise ValueError("invalid ASGI request message")
        chunk = message.get("body", b"")
        if not isinstance(chunk, bytes):
            raise ValueError("invalid ASGI body")
        output.extend(chunk)
        if len(output) > limit:
            output.clear()
            raise BodyTooLarge
        if not message.get("more_body", False):
            return bytes(output)


def parse_headers(values: list[tuple[bytes, bytes]]) -> dict[str, list[str]]:
    headers: dict[str, list[str]] = {}
    for raw_name, raw_value in values:
        try:
            name = raw_name.decode("ascii").lower()
            value = raw_value.decode("ascii")
        except UnicodeDecodeError as exc:
            raise ValueError("non-ASCII header") from exc
        if "\r" in value or "\n" in value:
            raise ValueError("invalid header")
        headers.setdefault(name, []).append(value)
    return headers


def one_header(
    headers: dict[str, list[str]], name: str, *, required: bool = True
) -> str:
    values = headers.get(name, [])
    if len(values) == 0 and not required:
        return ""
    if len(values) != 1 or not values[0] or values[0].strip() != values[0]:
        raise ValueError("invalid header")
    return values[0]


def bearer(headers: dict[str, list[str]]) -> str:
    value = one_header(headers, "authorization")
    if not value.startswith("Bearer ") or value.count(" ") != 1:
        raise AuthenticationError("authentication failed")
    return value[7:]


def parse_cursor(value: str) -> int:
    if (
        not value.isascii()
        or not value.isdigit()
        or (value != "0" and value.startswith("0"))
        or len(value) > 16
    ):
        raise ValueError("invalid cursor")
    cursor = int(value)
    if cursor > 9_007_199_254_740_991:
        raise ValueError("invalid cursor")
    return cursor


def direct_envelope(record, event) -> tuple[str, bytes]:
    """Bind durable event data to the immutable public CloudEvents envelope."""
    if (
        event.sequence < 1
        or event.sequence > 9_007_199_254_740_991
        or event.run_id != record.run_id
        or event.attempt_id != record.attempt_id
    ):
        raise ProviderConflict("invalid outbox event binding")
    try:
        decoded = stream_wire.decode_stream_event(event.envelope)
    except (TypeError, ValueError, UnicodeDecodeError):
        decoded = None
    if decoded is not None:
        if (
            int(decoded.producersequence) != event.sequence
            or not isinstance(decoded.runsequence, stream_wire.UnsetType)
            or str(decoded.runid) != record.run_id
            or str(decoded.attemptid) != record.attempt_id
            or str(decoded.type) != event.event_type
        ):
            raise ProviderConflict("outbox envelope mismatch")
        return str(decoded.type), stream_wire.encode_stream_event(decoded).encode()

    try:
        event_type = canonical_event_type(event.event_type)
        schema = event_schema(event_type)
        data = strict_object(event.envelope)
        if not event_type or not schema:
            raise ValueError("invalid outbox event type")
        envelope = stream_wire.StreamEvent(
            attemptid=record.attempt_id,
            data=data,
            datacontenttype="application/json",
            dataschema=schema,
            id=canonical_event_id(event),
            producersequence=event.sequence,
            runid=record.run_id,
            source="https://runtime.arop.invalid/instances/" + record.instance_id,
            specversion="1.0",
            subject="runs/" + record.run_id,
            time=event.created_at.astimezone(UTC).isoformat().replace("+00:00", "Z"),
            traceparent=record.traceparent,
            type=event_type,
        )
        encoded = stream_wire.encode_stream_event(envelope).encode()
        stream_wire.decode_stream_event(encoded)
        return event_type, encoded
    except (TypeError, ValueError, UnicodeDecodeError):
        raise ProviderConflict("invalid outbox event") from None


def canonical_event_type(value: str) -> str:
    if value.startswith("io.arop."):
        return value
    if value.startswith("arop.run."):
        return "io.arop.run." + value.removeprefix("arop.run.") + ".v1"
    return ""


def event_schema(event_type: str) -> str:
    for category in ("run", "output", "progress", "usage"):
        if event_type.startswith(f"io.arop.{category}."):
            name = "lifecycle" if category == "run" else category
            return f"https://arop.invalid/schemas/v1/events/{name}-events-v1.schema.json"
    return ""


def canonical_event_id(event) -> str:
    if re.fullmatch(
        r"evt_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}",
        event.event_id,
    ):
        return event.event_id
    value = bytearray(
        hashlib.sha256(
            f"{event.run_id}\0{event.attempt_id}\0{event.sequence}\0{event.event_id}".encode()
        ).digest()[:16]
    )
    value[6] = (value[6] & 0x0F) | 0x70
    value[8] = (value[8] & 0x3F) | 0x80
    hexadecimal = value.hex()
    return (
        "evt_"
        + hexadecimal[:8]
        + "-"
        + hexadecimal[8:12]
        + "-"
        + hexadecimal[12:16]
        + "-"
        + hexadecimal[16:20]
        + "-"
        + hexadecimal[20:]
    )


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
        raise ValueError("expected JSON object")
    return value
