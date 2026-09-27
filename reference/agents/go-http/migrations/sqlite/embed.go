// Package sqlitemigration embeds the immutable provider schema migration.
package sqlitemigration

import _ "embed"

// Initial is the exact 0001 provider-state migration.
//
//go:embed 0001_provider_state.sql
var Initial []byte
