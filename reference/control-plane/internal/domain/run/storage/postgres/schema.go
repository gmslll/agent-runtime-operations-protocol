package postgres

import (
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run/storage/internalstore"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

func VerifySchema() migrate.Verifier { return internalstore.VerifySchema(internalstore.Postgres) }
