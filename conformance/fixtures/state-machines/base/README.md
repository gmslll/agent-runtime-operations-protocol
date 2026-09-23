# Base protocol fixtures

These JSON files are the language-neutral behavior authority for the P06
foundation. Implementations must evaluate every `accepted` and `rejected`
vector; they must not infer behavior from the Go harness itself.

- `run.json`, `attempt.json`, and `registry.json` define the base transition
  graphs. Terminal states are irreversible within one generation/execution.
- `utf8-offsets.json` defines `output.delta.offset` as a UTF-8 byte offset,
  including Chinese, emoji, combining characters, duplicate delivery, and
  invalid-boundary cases.
- `wire-vectors.json` fixes v1 resource prefixes and W3C `traceparent`
  semantic negatives. In particular, `boot_` and `attempt_` are not aliases
  for `ses_` and `att_`.
- `identifiers.json` and `discovery.json` fix the complete accepted/rejected
  identifier corpus and each independent discovery eligibility dimension.
- `go-test-inventory.json` pins the exact Go package and testcase terminal
  event set exercised by the P06 harness. The harness also pins this file's
  SHA-256 digest, so deleting tests and editing the inventory together cannot
  silently weaken the phase gate.

All fixture files are strict JSON: duplicate keys and trailing values are
invalid even for a forward-compatible consumer.
