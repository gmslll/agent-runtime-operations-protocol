package sqlite

import (
	"database/sql"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/worker/storage/internalstore"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

func VerifySchema() migrate.Verifier { return internalstore.VerifySchema(internalstore.SQLite) }

func New(db *sql.DB, lookup durable.TransactionLookup) (*internalstore.Store, error) {
	return internalstore.New(db, lookup, internalstore.SQLite)
}
