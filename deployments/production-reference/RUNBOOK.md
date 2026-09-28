# Production reference upgrade and recovery

This deployment is a reference baseline, not a credential store. Replace every `${...}` value through a protected deployment system and pin both images by verified `sha256` digest. Never commit rendered Secrets.

## Preflight

1. Verify the candidate image digest, SBOM/provenance, P37 Production profile, and migration checksums.
2. Confirm PostgreSQL 16 readiness, free capacity, the active migration version, and that no migration is dirty.
3. Take `pg_dump --format=custom --no-owner --no-privileges` to encrypted, access-controlled storage. Record its digest and byte length outside Git.
4. Restore that backup into an isolated PostgreSQL 16 instance and run the same schema verifier plus required Production profile. A backup is not accepted until restore succeeds.

## Rolling upgrade

1. Render the immutable Control Plane image digest; keep `maxUnavailable: 0`, `maxSurge: 1` and the readiness probe unchanged.
2. Start one candidate Pod. Its startup path acquires the migration lock, verifies the full catalog, applies pending migrations, and stays unready on any checksum/schema error.
3. Run the Production profile against the candidate Service. Required scenarios may not be skipped.
4. Continue one Pod at a time. Watch readiness, durable audit, request errors, connection pool saturation, and migration history.
5. Stop immediately if readiness drops, migration history changes unexpectedly, or old/new instances disagree on protocol output.

## Rollback and restore

- If no forward-only migration committed, roll the Deployment back to the previous immutable digest and rerun the Production profile.
- If a forward-only migration committed, do not fake a down migration. Scale the Control Plane to zero, preserve the failed database for analysis, restore the verified preflight backup into a new database, point the Secret-managed DSN to it, deploy the previous image digest, and run schema verification plus the Production profile before reopening traffic.
- If restore or verification fails, keep traffic closed. Never fall back to memory, SQLite, a dirty database, or an unverified backup.

## Evidence and cleanup

Record image/config/migration/backup digests, timestamps, operator identity, readiness transitions, profile report digest, and rollback outcome. Do not record DSNs, passwords, bearer tokens, asset keys, Secret values, database contents, or absolute workstation paths.
