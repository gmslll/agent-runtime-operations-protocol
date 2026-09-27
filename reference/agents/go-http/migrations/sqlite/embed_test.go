package sqlitemigration

import (
	"bytes"
	"testing"
)

func TestInitialMigrationIsEmbeddedAndTransactional(t *testing.T) {
	for _, required := range [][]byte{
		[]byte("CREATE TABLE provider_schema_history"),
		[]byte("CREATE TABLE provider_inbox"),
		[]byte("CREATE TABLE provider_effects"),
		[]byte("CREATE TABLE provider_outbox"),
	} {
		if bytes.Count(Initial, required) != 1 {
			t.Fatalf("migration does not contain exactly one %q", required)
		}
	}
}
