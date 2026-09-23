# Go Reference Control Plane

This directory contains the vendor-neutral Go reference backend for AROP. The current repository-baseline slice only exposes liveness and readiness endpoints; Registry, Run/Dispatch, Event Ledger, and SSE are added after their machine-readable contracts land.

Run it locally with a temporary workspace copied from `go.work.example`, or from the nested module after its exact root pseudo-version has been bootstrapped into a Go proxy:

```bash
cd reference/control-plane
GOWORK=off go run ./cmd/aropd --listen 127.0.0.1:8080
curl http://127.0.0.1:8080/v1/health/live
curl http://127.0.0.1:8080/v1/health/ready
```

This service must not import KingLucky Console packages or implement organization, Feishu, approval, billing, or product UI rules.
