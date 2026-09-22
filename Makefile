.PHONY: install validate spec-index-check blueprint-check planning-audit gate-check test-go manifest-digest

install:
	npm ci

validate: spec-index-check blueprint-check
	npm run validate
	go test ./...

spec-index-check:
	AROP_CHECK_COMMAND="make spec-index-check" node scripts/spec-index-check.mjs

blueprint-check:
	AROP_CHECK_COMMAND="make blueprint-check" node scripts/blueprint-check.mjs

planning-audit:
	AROP_CHECK_COMMAND="make planning-audit EVIDENCE=$(EVIDENCE)" EVIDENCE="$(EVIDENCE)" node scripts/planning-audit.mjs

gate-check:
	AROP_CHECK_COMMAND="make gate-check GATE=$(GATE) EVIDENCE=$(EVIDENCE)" GATE="$(GATE)" EVIDENCE="$(EVIDENCE)" node scripts/gate-check.mjs

test-go:
	go test ./...

manifest-digest:
	go run ./cmd/arop manifest digest $(FILE)
