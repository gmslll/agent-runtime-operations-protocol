.PHONY: install validate spec-index-check blueprint-check verify-report test-report-verifier test-evidence-lineage planning-audit gate-check test-go test-go-workspace test-protocol-foundation test-codegen-pipeline test-control-plane-platform test-storage-migrations test-identity-secrets test-publication-contracts test-publication-service test-asset-broker test-registry-core test-registry-api manifest-digest

install:
	npm ci

validate: spec-index-check blueprint-check test-report-verifier test-evidence-lineage
	env -u NODE_OPTIONS -u NODE_PATH -u NPM_CONFIG_NODE_OPTIONS -u node_options -u node_path -u npm_config_node_options npm run validate
	env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go test ./...

spec-index-check:
	AROP_CHECK_COMMAND="make spec-index-check" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run ./internal/tooling/cmd/arop-spec-index-check

blueprint-check:
	AROP_CHECK_COMMAND="make blueprint-check" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run ./internal/tooling/cmd/arop-blueprint-check

verify-report:
	ALLOW_ANCESTOR="$(ALLOW_ANCESTOR)" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run ./internal/tooling/cmd/arop-verify-report "$(REPORT)"

test-report-verifier: spec-index-check blueprint-check
	AROP_VERIFY_CURRENT=1 env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go test -count=1 -run TestVerifier ./internal/tooling/report

test-evidence-lineage:
	env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go test -count=1 -run TestEvidenceLineage -v ./internal/tooling/evidence

planning-audit:
	AROP_CHECK_COMMAND="make planning-audit EVIDENCE=<external>" EVIDENCE="$(EVIDENCE)" TRUSTED_KEYS="$(TRUSTED_KEYS)" TRUSTED_CHANNEL_CONFIRMATION="$(TRUSTED_CHANNEL_CONFIRMATION)" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run ./internal/tooling/cmd/arop-planning-audit

gate-check:
	AROP_CHECK_COMMAND="make gate-check GATE=$(GATE) EVIDENCE=<external> P03_EVIDENCE=<external>" GATE="$(GATE)" EVIDENCE="$(EVIDENCE)" TRUSTED_KEYS="$(TRUSTED_KEYS)" TRUSTED_CHANNEL_CONFIRMATION="$(TRUSTED_CHANNEL_CONFIRMATION)" P03_EVIDENCE="$(P03_EVIDENCE)" P03_TRUSTED_KEYS="$(P03_TRUSTED_KEYS)" P03_TRUSTED_CHANNEL_CONFIRMATION="$(P03_TRUSTED_CHANNEL_CONFIRMATION)" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run ./internal/tooling/cmd/arop-gate-check

test-go:
	env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go test ./...

test-go-workspace:
	AROP_CHECK_COMMAND="make test-go-workspace" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run ./internal/tooling/cmd/arop-go-proxy-bootstrap

test-protocol-foundation:
	AROP_CHECK_COMMAND="make test-protocol-foundation" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run ./conformance/harness/base

test-codegen-pipeline:
	AROP_CHECK_COMMAND="make test-codegen-pipeline" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT -u NODE_OPTIONS -u NODE_PATH -u NPM_CONFIG_NODE_OPTIONS -u PYTHONHOME -u PYTHONPATH -u PYTHONSTARTUP -u PYTHONINSPECT -u PYTHONWARNINGS -u PYTHONUSERBASE GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run ./conformance/fixtures/codegen-spike/testdata/harness

test-control-plane-platform:
	AROP_CHECK_COMMAND="make test-control-plane-platform" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT -u NODE_OPTIONS -u NODE_PATH -u NPM_CONFIG_NODE_OPTIONS GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run -modfile="$(CURDIR)/go.mod" "$(CURDIR)/reference/control-plane/internal/app/platform/testdata/harness/main.go"

test-storage-migrations:
	AROP_CHECK_COMMAND="make test-storage-migrations" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT -u NODE_OPTIONS -u NODE_PATH -u NPM_CONFIG_NODE_OPTIONS -u PGHOST -u PGHOSTADDR -u PGPORT -u PGDATABASE -u PGUSER -u PGPASSWORD -u PGSERVICE -u PGSERVICEFILE -u PGPASSFILE -u PGOPTIONS -u PGCONNECT_TIMEOUT -u PGAPPNAME -u PGTARGETSESSIONATTRS -u PGREQUIRESSL -u PGSSLMODE -u PGSSLCERT -u PGSSLKEY -u PGSSLROOTCERT -u PGSSLCRL -u PGSSLCRLDIR -u PGGSSENCMODE -u PGCHANNELBINDING -u PGKRBSRVNAME -u PGSYSCONFDIR -u PGLOCALEDIR -u PGCLIENTENCODING GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run -modfile="$(CURDIR)/go.mod" "$(CURDIR)/reference/control-plane/internal/storage/migrate/testdata/engine-versions/harness/main.go"

test-identity-secrets:
	AROP_CHECK_COMMAND="make test-identity-secrets" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT -u NODE_OPTIONS -u NODE_PATH -u NPM_CONFIG_NODE_OPTIONS -u PGHOST -u PGHOSTADDR -u PGPORT -u PGDATABASE -u PGUSER -u PGPASSWORD -u PGSERVICE -u PGSERVICEFILE -u PGPASSFILE -u PGOPTIONS -u PGCONNECT_TIMEOUT -u PGAPPNAME -u PGTARGETSESSIONATTRS -u PGREQUIRESSL -u PGSSLMODE -u PGSSLCERT -u PGSSLKEY -u PGSSLROOTCERT -u PGSSLCRL -u PGSSLCRLDIR -u PGGSSENCMODE -u PGCHANNELBINDING -u PGKRBSRVNAME -u PGSYSCONFDIR -u PGLOCALEDIR -u PGCLIENTENCODING GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run -modfile="$(CURDIR)/go.mod" "$(CURDIR)/reference/control-plane/internal/identity/testdata/harness/main.go"

test-publication-contracts:
	AROP_CHECK_COMMAND="make test-publication-contracts" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT -u NODE_OPTIONS -u NODE_PATH -u NPM_CONFIG_NODE_OPTIONS -u PYTHONHOME -u PYTHONPATH -u PYTHONSTARTUP -u PYTHONINSPECT -u PYTHONWARNINGS -u PYTHONUSERBASE GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run ./conformance/fixtures/contracts/control-plane-publication/testdata/harness

test-publication-service:
	AROP_CHECK_COMMAND="make test-publication-service" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT -u NODE_OPTIONS -u NODE_PATH -u NPM_CONFIG_NODE_OPTIONS -u PYTHONHOME -u PYTHONPATH -u PYTHONSTARTUP -u PYTHONINSPECT -u PYTHONWARNINGS -u PYTHONUSERBASE -u PGHOST -u PGHOSTADDR -u PGPORT -u PGDATABASE -u PGUSER -u PGPASSWORD -u PGSERVICE -u PGSERVICEFILE -u PGPASSFILE -u PGOPTIONS GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run -modfile="$(CURDIR)/go.mod" "$(CURDIR)/reference/control-plane/internal/domain/publication/testdata/harness/main.go"

test-asset-broker:
	AROP_CHECK_COMMAND="make test-asset-broker" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT -u NODE_OPTIONS -u NODE_PATH -u NPM_CONFIG_NODE_OPTIONS -u PYTHONHOME -u PYTHONPATH -u PYTHONSTARTUP -u PYTHONINSPECT -u PYTHONWARNINGS -u PYTHONUSERBASE -u PGHOST -u PGHOSTADDR -u PGPORT -u PGDATABASE -u PGUSER -u PGPASSWORD -u PGSERVICE -u PGSERVICEFILE -u PGPASSFILE -u PGOPTIONS GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run -modfile="$(CURDIR)/go.mod" "$(CURDIR)/reference/control-plane/internal/domain/assets/testdata/harness/main.go"

test-registry-core:
	AROP_CHECK_COMMAND="make test-registry-core" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT -u NODE_OPTIONS -u NODE_PATH -u NPM_CONFIG_NODE_OPTIONS -u PYTHONHOME -u PYTHONPATH -u PYTHONSTARTUP -u PYTHONINSPECT -u PYTHONWARNINGS -u PYTHONUSERBASE -u PGHOST -u PGHOSTADDR -u PGPORT -u PGDATABASE -u PGUSER -u PGPASSWORD -u PGSERVICE -u PGSERVICEFILE -u PGPASSFILE -u PGOPTIONS GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run -modfile="$(CURDIR)/go.mod" "$(CURDIR)/reference/control-plane/internal/domain/registry/testdata/harness/main.go"

test-registry-api:
	AROP_CHECK_COMMAND="make test-registry-api" env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT -u NODE_OPTIONS -u NODE_PATH -u NPM_CONFIG_NODE_OPTIONS -u PYTHONHOME -u PYTHONPATH -u PYTHONSTARTUP -u PYTHONINSPECT -u PYTHONWARNINGS -u PYTHONUSERBASE -u PGHOST -u PGHOSTADDR -u PGPORT -u PGDATABASE -u PGUSER -u PGPASSWORD -u PGSERVICE -u PGSERVICEFILE -u PGPASSFILE -u PGOPTIONS GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run -modfile="$(CURDIR)/go.mod" "$(CURDIR)/reference/control-plane/internal/app/registryapi/testdata/harness/main.go"

manifest-digest: export AROP_MANIFEST_FILE := $(value FILE)
manifest-digest:
	@env -u GOFLAGS -u GOENV -u GOWORK -u GOCACHE -u GOCACHEPROG -u GOMODCACHE -u GOTMPDIR -u GOROOT -u GOTOOLCHAIN -u GOEXPERIMENT GOENV=off GOFLAGS=-mod=readonly GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go run ./cmd/arop manifest digest-env
