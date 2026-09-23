// Package durable stores each audit entry and trace span in one database row,
// preserving the atomic observation contract across SQLite and PostgreSQL.
package durable

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

type TransactionLookup func(context.Context) (*sql.Tx, bool)
type ReadinessCheck func(context.Context) error

type Store struct {
	db        *sql.DB
	dialect   migrate.Dialect
	lookup    TransactionLookup
	readiness ReadinessCheck
}

func New(db *sql.DB, dialect migrate.Dialect, lookup TransactionLookup, readiness ReadinessCheck) (*Store, error) {
	if db == nil || lookup == nil || readiness == nil {
		return nil, errors.New("durable observation store requires database, transaction lookup, and readiness check")
	}
	if dialect != migrate.DialectSQLite && dialect != migrate.DialectPostgres {
		return nil, errors.New("durable observation store dialect is unsupported")
	}
	return &Store{db: db, dialect: dialect, lookup: lookup, readiness: readiness}, nil
}

func (store *Store) AppendObservation(ctx context.Context, audit observability.AuditEntry, span observability.SpanRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := observability.ValidateObservationPair(audit, span); err != nil {
		return err
	}
	arguments := []any{
		audit.ID, audit.OccurredAt.UnixNano(), audit.RequestID, audit.TraceID,
		audit.Operation, string(audit.Outcome), audit.HTTPStatus, span.SpanID,
		span.ParentSpanID, span.StartedAt.UnixNano(), span.EndedAt.UnixNano(), string(span.Status),
	}
	statement := `INSERT INTO arop_observations(
audit_id,occurred_at_ns,request_id,trace_id,operation,outcome,http_status,
span_id,parent_span_id,started_at_ns,ended_at_ns,span_status
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`
	if store.dialect == migrate.DialectPostgres {
		statement = `INSERT INTO arop_observations(
audit_id,occurred_at_ns,request_id,trace_id,operation,outcome,http_status,
span_id,parent_span_id,started_at_ns,ended_at_ns,span_status
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	}
	if tx, ok := store.lookup(ctx); ok {
		if _, err := tx.ExecContext(ctx, statement, arguments...); err != nil {
			return fmt.Errorf("append durable observation: %w", err)
		}
		return nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin durable observation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, statement, arguments...); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("append durable observation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit durable observation: %w", err)
	}
	return nil
}

func (store *Store) QueryAudit(ctx context.Context, query observability.AuditQuery) ([]observability.AuditEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := query.Validate(); err != nil {
		return nil, err
	}
	where := make([]string, 0, 4)
	arguments := make([]any, 0, 5)
	add := func(column string, value any) {
		arguments = append(arguments, value)
		where = append(where, column+"="+store.placeholder(len(arguments)))
	}
	if query.RequestID != "" {
		add("request_id", query.RequestID)
	}
	if query.TraceID != "" {
		add("trace_id", query.TraceID)
	}
	if query.Operation != "" {
		add("operation", query.Operation)
	}
	if query.Outcome != "" {
		add("outcome", string(query.Outcome))
	}
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	arguments = append(arguments, limit)
	statement := `SELECT audit_id,occurred_at_ns,request_id,trace_id,operation,outcome,http_status FROM arop_observations`
	if len(where) != 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	statement += " ORDER BY occurred_at_ns,audit_id LIMIT " + store.placeholder(len(arguments))
	queryer := migrate.Queryer(store.db)
	if tx, ok := store.lookup(ctx); ok {
		queryer = tx
	}
	rows, err := queryer.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query durable audit: %w", err)
	}
	defer rows.Close()
	var result []observability.AuditEntry
	for rows.Next() {
		var entry observability.AuditEntry
		var occurredAt int64
		if err := rows.Scan(&entry.ID, &occurredAt, &entry.RequestID, &entry.TraceID, &entry.Operation, &entry.Outcome, &entry.HTTPStatus); err != nil {
			return nil, fmt.Errorf("scan durable audit: %w", err)
		}
		entry.OccurredAt = time.Unix(0, occurredAt).UTC()
		if err := entry.Validate(); err != nil {
			return nil, fmt.Errorf("stored audit violates contract: %w", err)
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate durable audit: %w", err)
	}
	return result, nil
}

func (store *Store) QueryTrace(ctx context.Context, query observability.TraceQuery) ([]observability.SpanRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := query.Validate(); err != nil {
		return nil, err
	}
	where := make([]string, 0, 3)
	arguments := make([]any, 0, 4)
	add := func(column string, value any) {
		arguments = append(arguments, value)
		where = append(where, column+"="+store.placeholder(len(arguments)))
	}
	if query.RequestID != "" {
		add("request_id", query.RequestID)
	}
	if query.TraceID != "" {
		add("trace_id", query.TraceID)
	}
	if query.Operation != "" {
		add("operation", query.Operation)
	}
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	arguments = append(arguments, limit)
	statement := `SELECT trace_id,span_id,parent_span_id,request_id,operation,started_at_ns,ended_at_ns,span_status FROM arop_observations`
	if len(where) != 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	statement += " ORDER BY occurred_at_ns,audit_id LIMIT " + store.placeholder(len(arguments))
	queryer := migrate.Queryer(store.db)
	if tx, ok := store.lookup(ctx); ok {
		queryer = tx
	}
	rows, err := queryer.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query durable trace: %w", err)
	}
	defer rows.Close()
	var result []observability.SpanRecord
	for rows.Next() {
		var span observability.SpanRecord
		var startedAt, endedAt int64
		if err := rows.Scan(&span.TraceID, &span.SpanID, &span.ParentSpanID, &span.RequestID, &span.Operation, &startedAt, &endedAt, &span.Status); err != nil {
			return nil, fmt.Errorf("scan durable trace: %w", err)
		}
		span.StartedAt, span.EndedAt = time.Unix(0, startedAt).UTC(), time.Unix(0, endedAt).UTC()
		if err := span.Validate(); err != nil {
			return nil, fmt.Errorf("stored trace violates contract: %w", err)
		}
		result = append(result, span)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate durable trace: %w", err)
	}
	return result, nil
}

func (store *Store) AuditHealth(ctx context.Context) error { return store.health(ctx) }
func (store *Store) TraceHealth(ctx context.Context) error { return store.health(ctx) }

func (store *Store) health(ctx context.Context) error {
	if err := store.readiness(ctx); err != nil {
		return err
	}
	if err := store.db.PingContext(ctx); err != nil {
		return errors.New("durable observation database is unavailable")
	}
	return nil
}

func (store *Store) placeholder(index int) string {
	if store.dialect == migrate.DialectPostgres {
		return fmt.Sprintf("$%d", index)
	}
	return "?"
}

func VerifySchema(dialect migrate.Dialect) migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		if dialect != migrate.DialectSQLite && dialect != migrate.DialectPostgres {
			return errors.New("unsupported durable schema dialect")
		}
		if dialect == migrate.DialectSQLite {
			return verifySQLiteSchema(ctx, queryer)
		}
		return verifyPostgresSchema(ctx, queryer)
	}
}

type expectedColumn struct {
	name, dataType, defaultSQL string
	notNull                    bool
	primaryKey                 int
}

func verifySQLiteSchema(ctx context.Context, queryer migrate.Queryer) error {
	expected := []expectedColumn{
		{"audit_id", "TEXT", "", true, 1}, {"occurred_at_ns", "INTEGER", "", true, 0},
		{"request_id", "TEXT", "", true, 0}, {"trace_id", "TEXT", "", true, 0},
		{"operation", "TEXT", "", true, 0}, {"outcome", "TEXT", "", true, 0},
		{"http_status", "INTEGER", "", true, 0}, {"span_id", "TEXT", "", true, 0},
		{"parent_span_id", "TEXT", "''", true, 0}, {"started_at_ns", "INTEGER", "", true, 0},
		{"ended_at_ns", "INTEGER", "", true, 0}, {"span_status", "TEXT", "", true, 0},
	}
	rows, err := queryer.QueryContext(ctx, `PRAGMA table_info('arop_observations')`)
	if err != nil {
		return errors.New("inspect SQLite durable schema")
	}
	index := 0
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return errors.New("scan SQLite durable columns")
		}
		defaultSQL := ""
		if defaultValue.Valid {
			defaultSQL = defaultValue.String
		}
		if index >= len(expected) || cid != index || name != expected[index].name || strings.ToUpper(dataType) != expected[index].dataType || (notNull != 0) != expected[index].notNull || primaryKey != expected[index].primaryKey || defaultSQL != expected[index].defaultSQL {
			rows.Close()
			return errors.New("SQLite durable columns are not exact")
		}
		index++
	}
	if err := rows.Close(); err != nil || index != len(expected) {
		return errors.New("SQLite durable columns are not exact")
	}
	var ddl string
	if err := queryer.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='arop_observations'`).Scan(&ddl); err != nil {
		return errors.New("SQLite durable table is missing")
	}
	expectedConstraints := []string{
		"arop_observations_pkey", "arop_observations_trace_span_unique", "arop_observations_audit_id_check",
		"arop_observations_request_id_check", "arop_observations_trace_id_check", "arop_observations_span_id_check",
		"arop_observations_parent_span_id_check", "arop_observations_operation_check", "arop_observations_outcome_check",
		"arop_observations_span_status_check", "arop_observations_http_status_check", "arop_observations_timing_check",
		"arop_observations_occurred_check", "arop_observations_truth_check",
	}
	lowerDDL := strings.ToLower(ddl)
	if strings.Count(lowerDDL, "constraint ") != len(expectedConstraints) || strings.Count(lowerDDL, "check(") != 12 {
		return errors.New("SQLite durable constraints are not exact")
	}
	for _, name := range expectedConstraints {
		if !strings.Contains(lowerDDL, "constraint "+name+" ") {
			return errors.New("SQLite durable constraints are not exact")
		}
	}
	return verifySQLiteIndexes(ctx, queryer)
}

func verifySQLiteIndexes(ctx context.Context, queryer migrate.Queryer) error {
	rows, err := queryer.QueryContext(ctx, `PRAGMA index_list('arop_observations')`)
	if err != nil {
		return errors.New("inspect SQLite durable indexes")
	}
	type indexContract struct {
		unique  int
		origin  string
		columns []string
	}
	expectedNamed := map[string]indexContract{
		"arop_observations_request_idx":   {0, "c", []string{"request_id", "occurred_at_ns", "audit_id"}},
		"arop_observations_trace_idx":     {0, "c", []string{"trace_id", "occurred_at_ns", "audit_id"}},
		"arop_observations_operation_idx": {0, "c", []string{"operation", "occurred_at_ns", "audit_id"}},
	}
	seenNamed := map[string]bool{}
	automatic := map[string]int{"pk": 0, "u": 0}
	count := 0
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return errors.New("scan SQLite durable indexes")
		}
		count++
		if contract, ok := expectedNamed[name]; ok {
			if unique != contract.unique || origin != contract.origin || partial != 0 || seenNamed[name] {
				rows.Close()
				return errors.New("SQLite durable index contract is incompatible")
			}
			seenNamed[name] = true
			continue
		}
		if _, ok := automatic[origin]; !ok || unique != 1 || partial != 0 {
			rows.Close()
			return errors.New("SQLite durable index set is not exact")
		}
		automatic[origin]++
	}
	if err := rows.Close(); err != nil || count != 5 || len(seenNamed) != len(expectedNamed) || automatic["pk"] != 1 || automatic["u"] != 1 {
		return errors.New("SQLite durable index set is not exact")
	}
	for name, contract := range expectedNamed {
		columns, err := sqliteIndexColumns(ctx, queryer, name)
		if err != nil || strings.Join(columns, ",") != strings.Join(contract.columns, ",") {
			return errors.New("SQLite durable index columns are not exact")
		}
	}
	return nil
}

func sqliteIndexColumns(ctx context.Context, queryer migrate.Queryer, name string) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		result = append(result, column)
	}
	return result, rows.Err()
}

func verifyPostgresSchema(ctx context.Context, queryer migrate.Queryer) error {
	expected := []expectedColumn{
		{"audit_id", "text", "", true, 0}, {"occurred_at_ns", "bigint", "", true, 0}, {"request_id", "text", "", true, 0},
		{"trace_id", "text", "", true, 0}, {"operation", "text", "", true, 0}, {"outcome", "text", "", true, 0},
		{"http_status", "integer", "", true, 0}, {"span_id", "text", "", true, 0}, {"parent_span_id", "text", "''::text", true, 0},
		{"started_at_ns", "bigint", "", true, 0}, {"ended_at_ns", "bigint", "", true, 0}, {"span_status", "text", "", true, 0},
	}
	rows, err := queryer.QueryContext(ctx, `SELECT a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,COALESCE(pg_get_expr(d.adbin,d.adrelid),'') FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid='public.arop_observations'::regclass AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`)
	if err != nil {
		return errors.New("inspect PostgreSQL durable columns")
	}
	index := 0
	for rows.Next() {
		var name, dataType, defaultSQL string
		var notNull bool
		if err := rows.Scan(&name, &dataType, &notNull, &defaultSQL); err != nil {
			rows.Close()
			return errors.New("scan PostgreSQL durable columns")
		}
		if index >= len(expected) || name != expected[index].name || dataType != expected[index].dataType || notNull != expected[index].notNull || defaultSQL != expected[index].defaultSQL {
			rows.Close()
			return errors.New("PostgreSQL durable columns are not exact")
		}
		index++
	}
	if err := rows.Close(); err != nil || index != len(expected) {
		return errors.New("PostgreSQL durable columns are not exact")
	}
	constraints, err := queryer.QueryContext(ctx, `SELECT conname,contype,pg_get_constraintdef(oid,true) FROM pg_constraint WHERE conrelid='public.arop_observations'::regclass ORDER BY conname`)
	if err != nil {
		return errors.New("inspect PostgreSQL durable constraints")
	}
	expectedKinds := map[string]string{
		"arop_observations_pkey": "p", "arop_observations_trace_span_unique": "u",
		"arop_observations_audit_id_check": "c", "arop_observations_request_id_check": "c", "arop_observations_trace_id_check": "c",
		"arop_observations_span_id_check": "c", "arop_observations_parent_span_id_check": "c", "arop_observations_operation_check": "c",
		"arop_observations_outcome_check": "c", "arop_observations_span_status_check": "c", "arop_observations_http_status_check": "c",
		"arop_observations_timing_check": "c", "arop_observations_occurred_check": "c", "arop_observations_truth_check": "c",
	}
	seen := map[string]bool{}
	for constraints.Next() {
		var name, kind, definition string
		if err := constraints.Scan(&name, &kind, &definition); err != nil {
			constraints.Close()
			return errors.New("scan PostgreSQL durable constraints")
		}
		if expectedKinds[name] != kind || seen[name] || !postgresConstraintLooksExact(name, definition) {
			constraints.Close()
			return errors.New("PostgreSQL durable constraints are not exact")
		}
		seen[name] = true
	}
	if err := constraints.Close(); err != nil || len(seen) != len(expectedKinds) {
		return errors.New("PostgreSQL durable constraints are not exact")
	}
	return verifyPostgresIndexes(ctx, queryer)
}

func postgresConstraintLooksExact(name, definition string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(definition), " "))
	required := map[string][]string{
		"arop_observations_pkey":                 {"primary key", "audit_id"},
		"arop_observations_trace_span_unique":    {"unique", "trace_id", "span_id"},
		"arop_observations_audit_id_check":       {"check", "audit_id", "aud_"},
		"arop_observations_request_id_check":     {"check", "request_id", "req_"},
		"arop_observations_trace_id_check":       {"check", "trace_id", "32", "00000000000000000000000000000000"},
		"arop_observations_span_id_check":        {"check", "span_id", "16", "0000000000000000"},
		"arop_observations_parent_span_id_check": {"check", "parent_span_id", "16", "0000000000000000"},
		"arop_observations_operation_check":      {"check", "operation", "100"},
		"arop_observations_outcome_check":        {"check", "outcome", "succeeded", "rejected", "failed"},
		"arop_observations_span_status_check":    {"check", "span_status", "ok", "error"},
		"arop_observations_http_status_check":    {"check", "http_status", "100", "599"},
		"arop_observations_timing_check":         {"check", "started_at_ns", "ended_at_ns"},
		"arop_observations_occurred_check":       {"check", "occurred_at_ns", "ended_at_ns"},
		"arop_observations_truth_check":          {"check", "http_status", "outcome", "span_status", "400", "499", "500"},
	}
	for _, token := range required[name] {
		if !strings.Contains(normalized, token) {
			return false
		}
	}
	return true
}

func verifyPostgresIndexes(ctx context.Context, queryer migrate.Queryer) error {
	rows, err := queryer.QueryContext(ctx, `SELECT indexname,indexdef FROM pg_indexes WHERE schemaname='public' AND tablename='arop_observations' ORDER BY indexname`)
	if err != nil {
		return errors.New("inspect PostgreSQL durable indexes")
	}
	defer rows.Close()
	expected := map[string][]string{
		"arop_observations_pkey":              {"unique", "audit_id"},
		"arop_observations_trace_span_unique": {"unique", "trace_id", "span_id"},
		"arop_observations_request_idx":       {"request_id", "occurred_at_ns", "audit_id"},
		"arop_observations_trace_idx":         {"trace_id", "occurred_at_ns", "audit_id"},
		"arop_observations_operation_idx":     {"operation", "occurred_at_ns", "audit_id"},
	}
	seen := map[string]bool{}
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			return errors.New("scan PostgreSQL durable indexes")
		}
		tokens, ok := expected[name]
		if !ok || seen[name] {
			return errors.New("PostgreSQL durable index set is not exact")
		}
		lower := strings.ToLower(definition)
		for _, token := range tokens {
			if !strings.Contains(lower, token) {
				return errors.New("PostgreSQL durable index definition is incompatible")
			}
		}
		seen[name] = true
	}
	if err := rows.Err(); err != nil || len(seen) != len(expected) {
		return errors.New("PostgreSQL durable index set is not exact")
	}
	return nil
}

var _ observability.Store = (*Store)(nil)
