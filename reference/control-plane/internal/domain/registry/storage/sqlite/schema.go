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

type columnExpectation struct {
	name, dataType string
	primaryKey     int
}

type tableExpectation struct {
	columns []columnExpectation
	digest  string
	indexes map[string]int
}

var registryTables = map[string]tableExpectation{
	"arop_registry_meta": {
		columns: []columnExpectation{{"singleton", "INTEGER", 1}, {"revision", "INTEGER", 0}, {"compaction_watermark", "INTEGER", 0}},
		digest:  "87fd1ea2e4528e73251a8ebdca119beef696ebbabc3dc65bc84a0a82ff99555e",
		indexes: map[string]int{},
	},
	"arop_registry_instances": {
		columns: []columnExpectation{
			{"tenant_id", "TEXT", 1}, {"instance_id", "TEXT", 2}, {"session_id", "TEXT", 0}, {"service_id", "TEXT", 0}, {"environment", "TEXT", 0},
			{"generation", "INTEGER", 0}, {"resource_version", "INTEGER", 0}, {"registry_revision", "INTEGER", 0}, {"lease_id", "TEXT", 0}, {"lease_expires_at", "TEXT", 0},
			{"heartbeat_sequence", "INTEGER", 0}, {"endpoint_base_url", "TEXT", 0}, {"endpoint_health_path", "TEXT", 0}, {"bindings_json", "TEXT", 0}, {"runtime_json", "TEXT", 0},
			{"operator_json", "TEXT", 0}, {"draining", "INTEGER", 0}, {"drain_deadline_at", "TEXT", 0}, {"status", "TEXT", 0}, {"created_at", "TEXT", 0}, {"updated_at", "TEXT", 0},
		},
		digest: "6091d7790b7fffffa938945c8ab57a268cdde058e885da9d11fea210888b3e0d",
		indexes: map[string]int{
			"pk|1|0|tenant_id,instance_id": 1,
			"u|1|0|lease_id":               1,
			"c|0|0|arop_registry_instances_discovery_idx|tenant_id,status,lease_expires_at,draining,instance_id": 1,
		},
	},
	"arop_registry_sessions": {
		columns: []columnExpectation{{"tenant_id", "TEXT", 1}, {"instance_id", "TEXT", 2}, {"session_id", "TEXT", 3}, {"generation", "INTEGER", 0}, {"created_at", "TEXT", 0}},
		digest:  "951a75028f6fba1eb615dd5bf7c9138a26541e40ba56bcee5d995bcc773ef872",
		indexes: map[string]int{"pk|1|0|tenant_id,instance_id,session_id": 1},
	},
	"arop_registry_events": {
		columns: []columnExpectation{{"revision", "INTEGER", 1}, {"event_id", "TEXT", 0}, {"tenant_id", "TEXT", 0}, {"event_type", "TEXT", 0}, {"instance_id", "TEXT", 0}, {"session_id", "TEXT", 0}, {"generation", "INTEGER", 0}, {"occurred_at", "TEXT", 0}},
		digest:  "b4e34e2b8e7ae82852c0d0fdf799b079a1edaa19c035f5c16db67f862eeaba4a",
		indexes: map[string]int{"u|1|0|event_id": 1, "c|0|0|arop_registry_events_tenant_revision_idx|tenant_id,revision": 1},
	},
	"arop_registry_idempotency": {
		columns: []columnExpectation{{"tenant_id", "TEXT", 1}, {"operation", "TEXT", 2}, {"key_digest", "TEXT", 3}, {"request_digest", "TEXT", 0}, {"result_json", "TEXT", 0}, {"result_revision", "INTEGER", 0}, {"created_at", "TEXT", 0}},
		digest:  "8925585cb1f94861d0a48032bfba723cc5361def4ae4633699340302e3ae707d",
		indexes: map[string]int{"pk|1|0|tenant_id,operation,key_digest": 1},
	},
}

var registryTriggers = map[string]string{
	"arop_registry_events_no_update": compact(`CREATE TRIGGER arop_registry_events_no_update BEFORE UPDATE ON arop_registry_events BEGIN SELECT RAISE(ABORT, 'arop_registry_events is append-only'); END`),
	"arop_registry_events_no_delete": compact(`CREATE TRIGGER arop_registry_events_no_delete BEFORE DELETE ON arop_registry_events BEGIN SELECT RAISE(ABORT, 'arop_registry_events is append-only'); END`),
}

// VerifySchema rejects any missing, added, reordered, weakened, or rewritten
// registry table, constraint, or index. It intentionally verifies the entire
// P14-owned schema instead of probing only the columns used by Repository.
func VerifySchema() migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		rows, err := queryer.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'arop_registry_%' ORDER BY name`)
		if err != nil {
			return errors.New("inspect SQLite registry tables")
		}
		found := map[string]bool{}
		for rows.Next() {
			var name string
			if rows.Scan(&name) != nil {
				rows.Close()
				return errors.New("scan SQLite registry tables")
			}
			if _, ok := registryTables[name]; !ok || found[name] {
				rows.Close()
				return errors.New("SQLite registry tables are not exact")
			}
			found[name] = true
		}
		if rows.Err() != nil || rows.Close() != nil || len(found) != len(registryTables) {
			return errors.New("SQLite registry tables are not exact")
		}
		for name, expected := range registryTables {
			if err := verifyTable(ctx, queryer, name, expected); err != nil {
				return err
			}
		}
		return verifyTriggers(ctx, queryer)
	}
}

func verifyTriggers(ctx context.Context, queryer migrate.Queryer) error {
	rows, err := queryer.QueryContext(ctx, `SELECT name,sql FROM sqlite_master WHERE type='trigger' AND tbl_name='arop_registry_events' ORDER BY name`)
	if err != nil {
		return errors.New("inspect SQLite registry triggers")
	}
	found := map[string]string{}
	for rows.Next() {
		var name, ddl string
		if rows.Scan(&name, &ddl) != nil {
			rows.Close()
			return errors.New("scan SQLite registry triggers")
		}
		if _, duplicate := found[name]; duplicate {
			rows.Close()
			return errors.New("SQLite registry triggers are not exact")
		}
		found[name] = compact(ddl)
	}
	if rows.Err() != nil || rows.Close() != nil || len(found) != len(registryTriggers) {
		return errors.New("SQLite registry triggers are not exact")
	}
	for name, expected := range registryTriggers {
		if found[name] != expected {
			return errors.New("SQLite registry triggers are not exact")
		}
	}
	return nil
}

func verifyTable(ctx context.Context, queryer migrate.Queryer, table string, expected tableExpectation) error {
	rows, err := queryer.QueryContext(ctx, `PRAGMA table_info('`+table+`')`)
	if err != nil {
		return errors.New("inspect SQLite registry columns")
	}
	index := 0
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue sql.NullString
		if rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey) != nil || index >= len(expected.columns) {
			rows.Close()
			return errors.New("SQLite registry columns are not exact")
		}
		want := expected.columns[index]
		expectedNotNull := 1
		if name == "drain_deadline_at" {
			expectedNotNull = 0
		}
		if cid != index || name != want.name || strings.ToUpper(dataType) != want.dataType || notNull != expectedNotNull || primaryKey != want.primaryKey || defaultValue.Valid {
			rows.Close()
			return errors.New("SQLite registry columns are not exact")
		}
		index++
	}
	if rows.Err() != nil || rows.Close() != nil || index != len(expected.columns) {
		return errors.New("SQLite registry columns are not exact")
	}
	var ddl string
	if queryer.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl) != nil {
		return errors.New("SQLite registry table is missing")
	}
	digest := sha256.Sum256([]byte(compact(ddl)))
	if actual := hex.EncodeToString(digest[:]); actual != expected.digest {
		return fmt.Errorf("SQLite registry table %s definition digest=%s want=%s", table, actual, expected.digest)
	}
	if err := verifyIndexes(ctx, queryer, table, expected.indexes); err != nil {
		return fmt.Errorf("%s: %w", table, err)
	}
	return nil
}

func verifyIndexes(ctx context.Context, queryer migrate.Queryer, table string, expected map[string]int) error {
	rows, err := queryer.QueryContext(ctx, `PRAGMA index_list('`+table+`')`)
	if err != nil {
		return errors.New("inspect SQLite registry indexes")
	}
	type metadata struct {
		name, origin    string
		unique, partial int
	}
	var indexes []metadata
	for rows.Next() {
		var sequence int
		var item metadata
		if rows.Scan(&sequence, &item.name, &item.unique, &item.origin, &item.partial) != nil {
			rows.Close()
			return errors.New("scan SQLite registry indexes")
		}
		indexes = append(indexes, item)
	}
	if rows.Err() != nil || rows.Close() != nil {
		return errors.New("scan SQLite registry indexes")
	}
	found := map[string]int{}
	for _, item := range indexes {
		if item.partial != 0 || item.origin != "c" && item.origin != "u" && item.origin != "pk" {
			return errors.New("SQLite registry indexes are not exact")
		}
		columnRows, err := queryer.QueryContext(ctx, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, item.name)
		if err != nil {
			return errors.New("inspect SQLite registry index columns")
		}
		var names []string
		for columnRows.Next() {
			var name string
			if columnRows.Scan(&name) != nil {
				columnRows.Close()
				return errors.New("scan SQLite registry index columns")
			}
			names = append(names, name)
		}
		if columnRows.Err() != nil || columnRows.Close() != nil {
			return errors.New("scan SQLite registry index columns")
		}
		signature := fmt.Sprintf("%s|%d|%d|", item.origin, item.unique, item.partial)
		if item.origin == "c" {
			signature += item.name + "|"
		}
		found[signature+strings.Join(names, ",")]++
	}
	if len(found) != len(expected) {
		return errors.New("SQLite registry indexes are not exact")
	}
	for signature, count := range expected {
		if found[signature] != count {
			return errors.New("SQLite registry indexes are not exact")
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
