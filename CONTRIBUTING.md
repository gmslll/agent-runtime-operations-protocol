# Contributing to AROP

AROP is a vendor-neutral interoperability protocol. Contributions must preserve that boundary and must not make a downstream console, Feishu, cc-connect, a particular framework, or a particular database a protocol dependency.

## Before opening a change

1. Read `AGENTS.md`, `docs/DECISIONS.md`, and the specification sections affected by the change.
2. Open an issue for defects or a discussion for implementation questions.
3. Use an RFC for new normative fields, changed wire semantics, compatibility rules, security boundaries, or conformance requirements.
4. Keep generated SDK models downstream of JSON Schema; do not create a second handwritten source of truth.

## Pull requests

Every pull request must:

- include tests or fixtures for behavior changes;
- update Schema, OpenAPI/AsyncAPI, examples, SDK mappings, and documentation together when applicable;
- preserve backward compatibility unless the accepted RFC explicitly permits a major-version break;
- pass `make validate`;
- avoid credentials, customer data, local paths, internal endpoints, and vendor-only fields in public fixtures;
- contain a Developer Certificate of Origin sign-off.

Add the sign-off with:

```text
git commit -s
```

This adds `Signed-off-by: Name <email>` and certifies the contribution under the [Developer Certificate of Origin 1.1](https://developercertificate.org/).

## Review policy

- Normative protocol and conformance changes require approval from two Maintainers.
- Documentation corrections and implementation-only fixes require one Maintainer unless they alter normative meaning.
- A Maintainer who authored a normative change cannot be its only approver.
- Security-sensitive details must follow `SECURITY.md`, not a public issue.

The initial Maintainer and Reviewer list will be added when the public repository is created.
