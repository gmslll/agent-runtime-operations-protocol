.PHONY: install validate spec-index-check blueprint-check verify-report test-report-verifier test-evidence-lineage planning-audit gate-check test-go manifest-digest

install:
	npm ci

validate: spec-index-check blueprint-check test-report-verifier test-evidence-lineage
	npm run validate
	go test ./...

spec-index-check:
	AROP_CHECK_COMMAND="make spec-index-check" go run ./internal/tooling/cmd/arop-spec-index-check

blueprint-check:
	AROP_CHECK_COMMAND="make blueprint-check" go run ./internal/tooling/cmd/arop-blueprint-check

verify-report:
	ALLOW_ANCESTOR="$(ALLOW_ANCESTOR)" go run ./internal/tooling/cmd/arop-verify-report "$(REPORT)"

test-report-verifier: spec-index-check blueprint-check
	AROP_VERIFY_CURRENT=1 go test -count=1 -run TestVerifier ./internal/tooling/report

test-evidence-lineage:
	go test -count=1 -run TestEvidenceLineage -v ./internal/tooling/evidence

planning-audit:
	AROP_CHECK_COMMAND="make planning-audit EVIDENCE=<external>" EVIDENCE="$(EVIDENCE)" TRUSTED_KEYS="$(TRUSTED_KEYS)" TRUSTED_CHANNEL_CONFIRMATION="$(TRUSTED_CHANNEL_CONFIRMATION)" go run ./internal/tooling/cmd/arop-planning-audit

gate-check:
	AROP_CHECK_COMMAND="make gate-check GATE=$(GATE) EVIDENCE=<external> P03_EVIDENCE=<external>" GATE="$(GATE)" EVIDENCE="$(EVIDENCE)" TRUSTED_KEYS="$(TRUSTED_KEYS)" TRUSTED_CHANNEL_CONFIRMATION="$(TRUSTED_CHANNEL_CONFIRMATION)" P03_EVIDENCE="$(P03_EVIDENCE)" P03_TRUSTED_KEYS="$(P03_TRUSTED_KEYS)" P03_TRUSTED_CHANNEL_CONFIRMATION="$(P03_TRUSTED_CHANNEL_CONFIRMATION)" go run ./internal/tooling/cmd/arop-gate-check

test-go:
	go test ./...

manifest-digest:
	go run ./cmd/arop manifest digest $(FILE)
