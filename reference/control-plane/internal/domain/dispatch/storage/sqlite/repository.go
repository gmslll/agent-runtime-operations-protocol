package sqlite

import (
	"database/sql"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch/storage/internalstore"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

func New(db *sql.DB, lookup durable.TransactionLookup, issuer string) (*internalstore.Store, error) {
	return internalstore.New(db, lookup, internalstore.SQLite, issuer)
}

func VerifySchema() migrate.Verifier { return internalstore.VerifySchema(internalstore.SQLite) }
