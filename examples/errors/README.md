# Error examples

`valid/` contains complete AROP v1 wire errors. `invalid/` contains negative
authoring examples that must fail before transmission. Consumers may preserve
or ignore explicitly allowed future optional fields, but publishers must not
emit them against the v1 error schema.

Programs branch on `code`, `category`, `retryable`, and
`retry_after_seconds`; `message` is presentation text and must not contain
credentials, full request bodies, internal stack traces, or customer data.
