# P06 base harness

Run from the repository root:

```sh
make test-protocol-foundation
make verify-report REPORT=build/reports/P06/report.json
```

The harness runs the exact pinned P06 Go package/test inventory
with `go test -count=1 -run=. -json`, rejecting missing, extra, failed, skipped,
cached, duplicate-terminal, and no-test results. A static Go AST pass rejects
custom `TestMain`, nested uninventoried test packages, and any mismatch between
top-level `TestXxx` declarations and the pinned inventory. It checks the language-neutral
state-machine and wire fixtures, compares Go and Node Manifest digests, proves
invalid Manifests produce no digest, and writes both JSON and JUnit reports. It
uses no prior phase report as runtime input; actual Go-test and cross-language
Manifest results are bound as runtime evidence.
