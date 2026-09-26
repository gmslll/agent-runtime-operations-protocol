package postgres

import (
	"database/sql"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry/storage/internalstore"
)

type Repository = internalstore.Repository

func New(db *sql.DB, lookup durable.TransactionLookup) (*Repository, error) {
	return internalstore.New(db, lookup, internalstore.Postgres)
}
