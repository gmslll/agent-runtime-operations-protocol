package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

type column struct {
	name, dataType string
	primaryKey     int
}

var assetColumns = []column{
	{"tenant_id", "TEXT", 1}, {"principal_id", "TEXT", 0}, {"credential_id", "TEXT", 0}, {"run_id", "TEXT", 0},
	{"asset_id", "TEXT", 2}, {"name", "TEXT", 0}, {"media_type", "TEXT", 0},
	{"size_bytes", "INTEGER", 0}, {"content_digest", "TEXT", 0}, {"content_bytes", "BLOB", 0}, {"object_key", "TEXT", 0}, {"status", "TEXT", 0},
	{"revision", "INTEGER", 0}, {"created_at_ns", "INTEGER", 0}, {"updated_at_ns", "INTEGER", 0},
	{"idempotency_key_digest", "TEXT", 0}, {"idempotency_request_digest", "TEXT", 0},
}

var grantColumns = []column{
	{"tenant_id", "TEXT", 1}, {"principal_id", "TEXT", 0}, {"credential_id", "TEXT", 0}, {"grant_id", "TEXT", 2}, {"asset_id", "TEXT", 0}, {"run_id", "TEXT", 0},
	{"operation", "TEXT", 0}, {"name", "TEXT", 0}, {"media_type", "TEXT", 0}, {"size_bytes", "INTEGER", 0}, {"content_digest", "TEXT", 0},
	{"audience", "TEXT", 0}, {"token_key_id", "TEXT", 0}, {"token_digest", "TEXT", 0}, {"use_limit", "INTEGER", 0}, {"use_count", "INTEGER", 0},
	{"not_before_ns", "INTEGER", 0}, {"expires_at_ns", "INTEGER", 0}, {"status", "TEXT", 0}, {"revision", "INTEGER", 0},
	{"created_at_ns", "INTEGER", 0}, {"updated_at_ns", "INTEGER", 0},
	{"idempotency_key_digest", "TEXT", 0}, {"idempotency_request_digest", "TEXT", 0},
}

func VerifySchema() migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		if err := verifyTable(ctx, queryer, "arop_assets", assetColumns, "ec788f61b26a8c0b66aeba0f10b8a243c31894249c00238743549713b1484c42", 17, 1, map[string]int{
			"pk|1|0|tenant_id,asset_id":                                     1,
			"u|1|0|tenant_id,idempotency_key_digest":                        1,
			"c|0|0|arop_assets_tenant_status_idx|tenant_id,status,asset_id": 1,
			"c|0|0|arop_assets_tenant_digest_idx|tenant_id,content_digest":  1,
		}); err != nil {
			return err
		}
		if err := verifyTable(ctx, queryer, "arop_asset_grants", grantColumns, "ef17500b646069d80ace756023a875d1ab3d6676d1b6f57a1c3a2a0b633f1028", 26, 2, map[string]int{
			"pk|1|0|tenant_id,grant_id":                                          1,
			"u|1|0|tenant_id,idempotency_key_digest":                             1,
			"u|1|0|token_digest":                                                 1,
			"c|0|0|arop_asset_grants_tenant_asset_idx|tenant_id,asset_id,status": 1,
			"c|0|0|arop_asset_grants_tenant_run_idx|tenant_id,run_id,status":     1,
		}); err != nil {
			return err
		}
		var ddl string
		if queryer.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='arop_asset_grants'`).Scan(&ddl) != nil {
			return errors.New("SQLite asset grant table is missing")
		}
		compacted := compact(ddl)
		if !strings.Contains(compacted, "foreignkey(tenant_id,asset_id)referencesarop_assets(tenant_id,asset_id)") || strings.Contains(compacted, "arop_runs") {
			return errors.New("SQLite asset grants reference a run table")
		}
		var runReferences int
		if queryer.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE sql LIKE '%arop_runs%'`).Scan(&runReferences) != nil || runReferences != 0 {
			return errors.New("SQLite asset schema references a run table")
		}
		return nil
	}
}

func verifyTable(ctx context.Context, queryer migrate.Queryer, table string, columns []column, ddlSHA256 string, checks, uniques int, indexes map[string]int) error {
	rows, err := queryer.QueryContext(ctx, `PRAGMA table_info('`+table+`')`)
	if err != nil {
		return errors.New("inspect SQLite asset columns")
	}
	index := 0
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue sql.NullString
		if rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey) != nil || index >= len(columns) {
			rows.Close()
			return errors.New("SQLite asset columns are not exact")
		}
		want := columns[index]
		if cid != index || name != want.name || strings.ToUpper(dataType) != want.dataType || notNull != 1 || primaryKey != want.primaryKey || defaultValue.Valid {
			rows.Close()
			return errors.New("SQLite asset columns are not exact")
		}
		index++
	}
	if rows.Err() != nil || rows.Close() != nil || index != len(columns) {
		return errors.New("SQLite asset columns are not exact")
	}
	var ddl string
	if queryer.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl) != nil {
		return errors.New("SQLite asset table is missing")
	}
	compacted := compact(ddl)
	if strings.Count(compacted, "check(") != checks || strings.Count(compacted, "unique(") != uniques || strings.Count(compacted, "primarykey(") != 1 {
		return errors.New("SQLite asset constraints are not exact")
	}
	digest := sha256.Sum256([]byte(compacted))
	if actual := hex.EncodeToString(digest[:]); actual != ddlSHA256 {
		return fmt.Errorf("SQLite asset table %s definition digest=%s want=%s", table, actual, ddlSHA256)
	}
	return verifyIndexes(ctx, queryer, table, indexes)
}

func verifyIndexes(ctx context.Context, queryer migrate.Queryer, table string, expected map[string]int) error {
	rows, err := queryer.QueryContext(ctx, `PRAGMA index_list('`+table+`')`)
	if err != nil {
		return errors.New("inspect SQLite asset indexes")
	}
	type metadata struct {
		name, origin    string
		unique, partial int
	}
	var indexes []metadata
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		if rows.Scan(&sequence, &name, &unique, &origin, &partial) != nil {
			rows.Close()
			return errors.New("scan SQLite asset indexes")
		}
		indexes = append(indexes, metadata{name: name, unique: unique, partial: partial, origin: origin})
	}
	if rows.Err() != nil || rows.Close() != nil {
		return errors.New("scan SQLite asset indexes")
	}
	found := map[string]int{}
	for _, index := range indexes {
		if index.partial != 0 || index.origin != "c" && index.origin != "u" && index.origin != "pk" {
			return errors.New("SQLite asset indexes are not exact")
		}
		columnRows, err := queryer.QueryContext(ctx, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index.name)
		if err != nil {
			return errors.New("inspect SQLite asset index columns")
		}
		var names []string
		for columnRows.Next() {
			var columnName string
			if columnRows.Scan(&columnName) != nil {
				columnRows.Close()
				return errors.New("scan SQLite asset index columns")
			}
			names = append(names, columnName)
		}
		if columnRows.Err() != nil || columnRows.Close() != nil {
			return errors.New("scan SQLite asset index columns")
		}
		signature := index.origin + "|" + string(rune('0'+index.unique)) + "|" + string(rune('0'+index.partial)) + "|"
		if index.origin == "c" {
			signature += index.name + "|"
		}
		signature += strings.Join(names, ",")
		found[signature]++
	}
	if len(found) != len(expected) {
		return errors.New("SQLite asset indexes are not exact")
	}
	for signature, count := range expected {
		if found[signature] != count {
			return errors.New("SQLite asset indexes are not exact")
		}
	}
	return nil
}

func compact(value string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsSpace(character) || character == '`' || character == '"' {
			return -1
		}
		return unicode.ToLower(character)
	}, value)
}
