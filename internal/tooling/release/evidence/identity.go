package evidence

// Repository is the single canonical GitHub repository identity every release
// trust surface binds: envelope repository_uri, Sigstore identity claims,
// external release configuration, and the release workflow guard. Schema
// consts mirror these values; the P40 harness enforces the lockstep so a
// partial rename cannot compile-and-pass. Changing the canonical repository
// invalidates every previously issued external trust artifact (role-registry
// bundles, external configs, envelopes), which must then be re-issued.
const (
	Repository    = "gmslll/agent-runtime-operations-protocol"
	RepositoryURI = "https://github.com/" + Repository

	// ReleaseWorkflowEvent is the only GitHub event the pinned release
	// workflow can emit; identities claiming any other event are untrusted.
	ReleaseWorkflowEvent = "workflow_dispatch"
)
