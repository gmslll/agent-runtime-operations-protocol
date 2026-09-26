package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

const registrySchemaDigest = "14b08212df513a352bb4c7938810bad496f66d83ecb2b3537d8bc04f61e07f1c"

// VerifySchema binds the complete PostgreSQL 16 catalog surface owned by P14:
// table/column order and types, every constraint definition, and every index.
// It therefore rejects extra objects as well as weakened objects retaining a
// familiar name.
func VerifySchema() migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		rows, err := queryer.QueryContext(ctx, `WITH registry_tables AS (
  SELECT c.oid,c.relname
  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname=current_schema() AND c.relkind='r' AND c.relname LIKE 'arop_registry_%'
), catalog_lines AS (
  SELECT format('column|%s|%s|%s|%s|%s|%s|%s|%s', table_name,ordinal_position,column_name,data_type,is_nullable,coalesce(column_default,''),is_identity,is_generated) AS line
  FROM information_schema.columns
  WHERE table_schema=current_schema() AND table_name LIKE 'arop_registry_%'
  UNION ALL
  SELECT format('constraint|%s|%s|%s|%s|%s|%s', t.relname,c.conname,c.contype,c.condeferrable,c.condeferred,pg_get_constraintdef(c.oid,false))
  FROM pg_constraint c JOIN registry_tables t ON t.oid=c.conrelid
  UNION ALL
  SELECT format('index|%s|%s|%s|%s|%s|%s|%s|%s|%s', t.relname,ci.relname,i.indisunique,i.indisprimary,i.indisvalid,i.indisready,(i.indpred IS NOT NULL),(i.indexprs IS NOT NULL),pg_get_indexdef(i.indexrelid))
  FROM pg_index i JOIN registry_tables t ON t.oid=i.indrelid JOIN pg_class ci ON ci.oid=i.indexrelid
)
SELECT line FROM catalog_lines ORDER BY line`)
		if err != nil {
			return errors.New("inspect PostgreSQL registry schema")
		}
		var lines []string
		for rows.Next() {
			var line string
			if rows.Scan(&line) != nil || strings.ContainsAny(line, "\r\n") {
				rows.Close()
				return errors.New("scan PostgreSQL registry schema")
			}
			lines = append(lines, line)
		}
		if rows.Err() != nil || rows.Close() != nil || len(lines) == 0 {
			return errors.New("scan PostgreSQL registry schema")
		}
		digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		actual := hex.EncodeToString(digest[:])
		if actual != registrySchemaDigest {
			return fmt.Errorf("PostgreSQL registry schema digest=%s want=%s", actual, registrySchemaDigest)
		}
		return nil
	}
}
