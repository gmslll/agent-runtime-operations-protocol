.PHONY: install validate spec-index-check blueprint-check test-go manifest-digest

install:
	npm ci

validate: spec-index-check blueprint-check
	npm run validate
	go test ./...

spec-index-check:
	node scripts/spec-index-check.mjs

blueprint-check:
	node scripts/blueprint-check.mjs

test-go:
	go test ./...

manifest-digest:
	go run ./cmd/arop manifest digest $(FILE)
