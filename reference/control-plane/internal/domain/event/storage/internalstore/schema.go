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
	sqliteEventSchemaDigest   = "f358e2fee8d62a62dbc68fea4171ae079cc307f16935e9647079ea23cbdf3b6b"
	postgresEventSchemaDigest = "2a42fed361e6ff4243c4b2d5afc18408e17146ce3b14c2788d47f85d749c0896"
)

func VerifySchema(dialect Dialect) migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		var lines []string
		var err error
		switch dialect {
		case SQLite:
			lines, err = sqliteEventSchemaLines(ctx, queryer)
		case Postgres:
			lines, err = postgresEventSchemaLines(ctx, queryer)
		default:
			return errors.New("unsupported event schema dialect")
		}
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return errors.New("event schema is missing")
		}
		digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		actual := hex.EncodeToString(digest[:])
		want := sqliteEventSchemaDigest
		if dialect == Postgres {
			want = postgresEventSchemaDigest
		}
		if actual != want {
			return fmt.Errorf("%s event schema digest=%s want=%s", dialect, actual, want)
		}
		return nil
	}
}

func sqliteEventSchemaLines(ctx context.Context, queryer migrate.Queryer) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT type,name,tbl_name,coalesce(sql,'') FROM sqlite_schema WHERE (name LIKE 'arop_event%' OR tbl_name LIKE 'arop_event%') AND name NOT LIKE 'sqlite_autoindex_%' ORDER BY type,name`)
	if err != nil {
		return nil, errors.New("inspect SQLite event schema")
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var kind, name, table, ddl string
		if rows.Scan(&kind, &name, &table, &ddl) != nil || strings.ContainsAny(kind+name+table+ddl, "\x00") {
			return nil, errors.New("scan SQLite event schema")
		}
		lines = append(lines, kind+"|"+name+"|"+table+"|"+compactSQL(ddl))
	}
	if rows.Err() != nil {
		return nil, errors.New("scan SQLite event schema")
	}
	return lines, nil
}

func postgresEventSchemaLines(ctx context.Context, queryer migrate.Queryer) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, `WITH event_tables AS (SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind='r' AND c.relname LIKE 'arop_event%'), catalog_lines AS (
SELECT format('column|%s|%s|%s|%s|%s|%s|%s',table_name,ordinal_position,column_name,data_type,is_nullable,coalesce(column_default,''),is_identity,is_generated) AS line FROM information_schema.columns WHERE table_schema=current_schema() AND table_name LIKE 'arop_event%'
UNION ALL SELECT format('constraint|%s|%s|%s|%s|%s|%s',t.relname,c.conname,c.contype,c.condeferrable,c.condeferred,pg_get_constraintdef(c.oid,false)) FROM pg_constraint c JOIN event_tables t ON t.oid=c.conrelid
UNION ALL SELECT format('index|%s|%s|%s|%s|%s|%s|%s|%s|%s',t.relname,ci.relname,i.indisunique,i.indisprimary,i.indisvalid,i.indisready,(i.indpred IS NOT NULL),(i.indexprs IS NOT NULL),pg_get_indexdef(i.indexrelid)) FROM pg_index i JOIN event_tables t ON t.oid=i.indrelid JOIN pg_class ci ON ci.oid=i.indexrelid
UNION ALL SELECT format('trigger|%s|%s|%s',t.relname,g.tgname,pg_get_triggerdef(g.oid,false)) FROM pg_trigger g JOIN event_tables t ON t.oid=g.tgrelid WHERE NOT g.tgisinternal
UNION ALL SELECT format('function|%s|%s|%s|%s',p.proname,l.lanname,p.provolatile,p.prosrc) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace JOIN pg_language l ON l.oid=p.prolang WHERE n.nspname=current_schema() AND p.proname IN ('arop_event_ledger_immutable','arop_event_capacity_releases_immutable')) SELECT line FROM catalog_lines ORDER BY line`)
	if err != nil {
		return nil, errors.New("inspect PostgreSQL event schema")
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if rows.Scan(&line) != nil || strings.ContainsAny(line, "\r\n\x00") {
			return nil, errors.New("scan PostgreSQL event schema")
		}
		lines = append(lines, line)
	}
	if rows.Err() != nil {
		return nil, errors.New("scan PostgreSQL event schema")
	}
	return lines, nil
}
func compactSQL(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}
