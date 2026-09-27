"""Composition root for the dependency-free reference Python Agent."""

from __future__ import annotations

from datetime import UTC, datetime
import hashlib
from pathlib import Path

from arop.asgi import ASGIRuntime
from arop.generated.run import run_gen as wire
from arop.provider import Execution, ProviderRuntime
from arop.provider.runtime import Handler, TokenVerifier

from .storage import SQLiteStore


class EchoHandler:
    async def execute(
        self, execution: Execution, request: wire.RunRequest
    ) -> wire.RunResult:
        content = list(request.input)
        digest = "sha256:" + hashlib.sha256(
            wire.encode_run_request(request).encode()
        ).hexdigest()
        return wire.RunResult(
            completed_at=datetime.now(UTC).isoformat().replace("+00:00", "Z"),
            run_id=execution.record.run_id,
            schema_version=1,
            state="succeeded",
            usage=wire.Usage(duration_ms=0, input_tokens=0, output_tokens=0),
            snapshot=wire.Snapshot(content=content, digest=digest, revision=1),
        )


def create_app(
    database_path: str | Path,
    verifier: TokenVerifier,
    *,
    handler: Handler | None = None,
    migration_path: str | Path | None = None,
) -> ASGIRuntime:
    database = Path(database_path)
    migration = (
        Path(migration_path)
        if migration_path is not None
        else Path(__file__).resolve().parents[2]
        / "migrations"
        / "sqlite"
        / "0001_provider_state.sql"
    )
    store = SQLiteStore(database, migration)
    runtime = ProviderRuntime(store, verifier, handler or EchoHandler())
    return ASGIRuntime(runtime)
