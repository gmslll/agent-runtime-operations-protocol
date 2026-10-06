# Go Reference Control Plane

This directory contains the vendor-neutral Go reference backend for AROP. The current repository-baseline slice only exposes liveness and readiness endpoints; Registry, Run/Dispatch, Event Ledger, and SSE are added after their machine-readable contracts land.

The P08 platform foundation already applies the production-facing assembly rules to those two endpoints: fail-closed configuration, bounded HTTP server settings, a per-request deadline, server-owned request IDs, W3C trace-context validation through the public protocol core, typed Audit/Trace records, deterministic Clock/ID/Fault/UoW seams, truthful readiness, draining, and bounded graceful shutdown. Its observability store is deliberately in-memory and ephemeral. A readiness response therefore reports `scope: platform-bootstrap` and `durability: ephemeral`; it does not claim that migrations or durable storage are ready.

Run it locally with a temporary workspace copied from `go.work.example`, or from the nested module after its exact root pseudo-version has been bootstrapped into a Go proxy:

```bash
cd reference/control-plane
GOWORK=off go run ./cmd/aropd --listen 127.0.0.1:8080
curl http://127.0.0.1:8080/v1/health/live
curl http://127.0.0.1:8080/v1/health/ready
```

The default bind is loopback-only. Configuration can use the documented long flags or their `AROP_CP_*` environment equivalents, but the same setting cannot be supplied by both sources. Unknown `AROP_CP_*` keys, duplicate flags, unsafe non-loopback binds, invalid durations or limits, and unequal Audit/Trace capacities are rejected before the listener starts. Run the phase acceptance with:

```bash
make test-control-plane-platform
make verify-report REPORT=build/reports/P08/report.json
```

Health requests do not accept bodies. `/v1/health/live` proves only that the process and HTTP stack respond. `/v1/health/ready` additionally checks initialization, draining state, the ephemeral Audit/Trace store, and all injected platform probes; any failed or timed-out required check returns `503` without exposing internal error text. P09 replaces the ephemeral bootstrap with migration-aware durable readiness.

This service must not import downstream console packages or implement organization, Feishu, approval, billing, or product UI rules.
