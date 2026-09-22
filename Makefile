.PHONY: install validate test-go manifest-digest

install:
	npm ci

validate:
	npm run validate
	go test ./...

test-go:
	go test ./...

manifest-digest:
	go run ./cmd/arop manifest digest $(FILE)
