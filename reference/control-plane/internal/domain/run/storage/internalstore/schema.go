package internalstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

const (
	sqliteRunSchemaDigest   = "47f516466d71b27b401fc2fc2ffb40b071b6ccc10a55adb5c4783fec95381277"
	postgresRunSchemaDigest = "d2b7efdb058b7f02d41b060a53407a015bfec72dbfd8b0e202ca1c021ed6cbe9"
)

// VerifySchema binds the complete P18 run schema rather than probing only the
// columns currently used by Store. Added, removed, reordered, or weakened
// tables, columns, constraints, indexes, triggers, and functions all change
// the canonical catalog digest and fail readiness closed.
func VerifySchema(dialect Dialect) migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		var lines []string
		var err error
		switch dialect {
		case SQLite:
			lines, err = sqliteSchemaLines(ctx, queryer)
		case Postgres:
			lines, err = postgresSchemaLines(ctx, queryer)
		default:
			return errors.New("unsupported run schema dialect")
		}
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return errors.New("run schema is missing")
		}
		digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		actual := hex.EncodeToString(digest[:])
		want := sqliteRunSchemaDigest
		if dialect == Postgres {
			want = postgresRunSchemaDigest
		}
		if actual != want {
			return fmt.Errorf("%s run schema digest=%s want=%s", dialect, actual, want)
		}
		return nil
	}
}

func sqliteSchemaLines(ctx context.Context, queryer migrate.Queryer) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT type,name,tbl_name,coalesce(sql,'')
FROM sqlite_schema
WHERE (name LIKE 'arop_run%' OR tbl_name LIKE 'arop_run%')
  AND name NOT LIKE 'sqlite_autoindex_%'
ORDER BY type,name`)
	if err != nil {
		return nil, errors.New("inspect SQLite run schema")
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var kind, name, table, ddl string
		if rows.Scan(&kind, &name, &table, &ddl) != nil || strings.ContainsAny(kind+name+table+ddl, "\x00") {
			return nil, errors.New("scan SQLite run schema")
		}
		lines = append(lines, kind+"|"+name+"|"+table+"|"+compactSQL(ddl))
	}
	if rows.Err() != nil {
		return nil, errors.New("scan SQLite run schema")
	}
	return lines, nil
}

func postgresSchemaLines(ctx context.Context, queryer migrate.Queryer) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, `WITH run_tables AS (
  SELECT c.oid,c.relname
  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname=current_schema() AND c.relkind='r' AND c.relname LIKE 'arop_run%'
), catalog_lines AS (
  SELECT format('column|%s|%s|%s|%s|%s|%s|%s',table_name,ordinal_position,column_name,data_type,is_nullable,coalesce(column_default,''),is_identity,is_generated) AS line
  FROM information_schema.columns
  WHERE table_schema=current_schema() AND table_name LIKE 'arop_run%'
  UNION ALL
  SELECT format('constraint|%s|%s|%s|%s|%s|%s',t.relname,c.conname,c.contype,c.condeferrable,c.condeferred,pg_get_constraintdef(c.oid,false))
  FROM pg_constraint c JOIN run_tables t ON t.oid=c.conrelid
  UNION ALL
  SELECT format('index|%s|%s|%s|%s|%s|%s|%s|%s|%s',t.relname,ci.relname,i.indisunique,i.indisprimary,i.indisvalid,i.indisready,(i.indpred IS NOT NULL),(i.indexprs IS NOT NULL),pg_get_indexdef(i.indexrelid))
  FROM pg_index i JOIN run_tables t ON t.oid=i.indrelid JOIN pg_class ci ON ci.oid=i.indexrelid
  UNION ALL
  SELECT format('trigger|%s|%s|%s',t.relname,g.tgname,pg_get_triggerdef(g.oid,false))
  FROM pg_trigger g JOIN run_tables t ON t.oid=g.tgrelid
  WHERE NOT g.tgisinternal
  UNION ALL
  SELECT format('function|%s|%s|%s|%s',p.proname,l.lanname,p.provolatile,p.prosrc)
  FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace JOIN pg_language l ON l.oid=p.prolang
  WHERE n.nspname=current_schema() AND p.proname IN ('arop_run_outbox_no_delete','arop_run_effects_immutable')
)
SELECT line FROM catalog_lines ORDER BY line`)
	if err != nil {
		return nil, errors.New("inspect PostgreSQL run schema")
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if rows.Scan(&line) != nil || strings.ContainsAny(line, "\r\n\x00") {
			return nil, errors.New("scan PostgreSQL run schema")
		}
		lines = append(lines, line)
	}
	if rows.Err() != nil {
		return nil, errors.New("scan PostgreSQL run schema")
	}
	return lines, nil
}

func compactSQL(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}
