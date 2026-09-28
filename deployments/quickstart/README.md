# SQLite Quickstart

The quickstart composes the existing Reference Control Plane and example agents; it does not define protocol behavior.

```sh
arop init ./my-agent
AROP_CONTROL_PLANE_BINARY=/absolute/path/to/aropd \
AROP_MIGRATION_ROOT=/absolute/path/to/reference/control-plane/migrations \
  arop dev ./my-agent
arop doctor ./my-agent
```

`init` writes a deterministic, credential-free workspace. `dev` creates one ephemeral local key, starts the SQLite Control Plane on loopback, waits for readiness, and removes the key on shutdown. `doctor` emits JSON and exits non-zero if configuration, containment, or the sample Manifest is invalid.

This bundle is for local development only. It does not weaken TLS, authentication, authorization, migration, or durable-audit requirements for production deployments.
