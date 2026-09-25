package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

type column struct {
	name, dataType string
	primaryKey     int
}

var columns = []column{
	{"tenant_id", "TEXT", 1}, {"agent_id", "TEXT", 2}, {"version", "TEXT", 3},
	{"manifest_digest", "TEXT", 0}, {"bundle_semantic_digest", "TEXT", 0}, {"canonical_manifest", "BLOB", 0},
	{"publisher_principal_id", "TEXT", 0}, {"idempotency_key_digest", "TEXT", 0},
	{"idempotency_request_digest", "TEXT", 0}, {"published_at_ns", "INTEGER", 0}, {"revision", "INTEGER", 0},
}

var constraintFragments = []string{
	"constraintarop_publications_pkeyprimarykey(tenant_id,agent_id,version)",
	"constraintarop_publications_idempotency_uniqueunique(tenant_id,idempotency_key_digest)",
	"constraintarop_publications_tenant_checkcheck(length(tenant_id)between1and128andtenant_idnotglob'*[^a-z0-9._-]*'andsubstr(tenant_id,1,1)glob'[a-z]'andsubstr(tenant_id,-1,1)glob'[a-z0-9]'andtenant_idnotglob'*[._-][._-]*')",
	"constraintarop_publications_agent_checkcheck(length(agent_id)between1and200andagent_idnotglob'*[^a-z0-9._-]*'andsubstr(agent_id,1,1)glob'[a-z]'andsubstr(agent_id,-1,1)glob'[a-z0-9]'andagent_idnotglob'*[._-][._-]*')",
	"constraintarop_publications_version_checkcheck(length(version)between5and200)",
	"constraintarop_publications_manifest_digest_checkcheck(length(manifest_digest)=71andsubstr(manifest_digest,1,7)='sha256:'andsubstr(manifest_digest,8)notglob'*[^0-9a-f]*')",
	"constraintarop_publications_bundle_digest_checkcheck(length(bundle_semantic_digest)=71andsubstr(bundle_semantic_digest,1,7)='sha256:'andsubstr(bundle_semantic_digest,8)notglob'*[^0-9a-f]*')",
	"constraintarop_publications_manifest_checkcheck(typeof(canonical_manifest)='blob'andlength(canonical_manifest)between2and10485760)",
	"constraintarop_publications_publisher_checkcheck(length(publisher_principal_id)between1and200)",
	"constraintarop_publications_idempotency_key_checkcheck(length(idempotency_key_digest)=64andidempotency_key_digestnotglob'*[^0-9a-f]*')",
	"constraintarop_publications_idempotency_request_checkcheck(length(idempotency_request_digest)=64andidempotency_request_digestnotglob'*[^0-9a-f]*')",
	"constraintarop_publications_published_checkcheck(published_at_ns>0)",
	"constraintarop_publications_revision_checkcheck(revision=1)",
}

func VerifySchema() migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		rows, err := queryer.QueryContext(ctx, `PRAGMA table_info('arop_publications')`)
		if err != nil {
			return errors.New("inspect SQLite publication columns")
		}
		defer rows.Close()
		index := 0
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, dataType string
			var defaultValue sql.NullString
			if rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey) != nil || index >= len(columns) {
				return errors.New("SQLite publication columns are not exact")
			}
			want := columns[index]
			if cid != index || name != want.name || strings.ToUpper(dataType) != want.dataType || notNull != 1 || primaryKey != want.primaryKey || defaultValue.Valid {
				return errors.New("SQLite publication columns are not exact")
			}
			index++
		}
		if rows.Err() != nil || index != len(columns) {
			return errors.New("SQLite publication columns are not exact")
		}
		var ddl string
		if queryer.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='arop_publications'`).Scan(&ddl) != nil {
			return errors.New("SQLite publication table is missing")
		}
		compact := compact(ddl)
		for _, fragment := range constraintFragments {
			if !strings.Contains(compact, fragment) {
				return errors.New("SQLite publication constraints are not exact")
			}
		}
		if strings.Count(compact, "constraint") != len(constraintFragments) || strings.Count(compact, "check(") != 11 || strings.Count(compact, "unique(") != 1 || strings.Count(compact, "primarykey(") != 1 {
			return errors.New("SQLite publication constraints are not exact")
		}
		return verifyIndexes(ctx, queryer)
	}
}

func verifyIndexes(ctx context.Context, queryer migrate.Queryer) error {
	expected := map[string]string{
		"arop_publications_tenant_published_idx": "tenant_id,published_at_ns,agent_id,version",
		"arop_publications_manifest_digest_idx":  "tenant_id,manifest_digest",
	}
	rows, err := queryer.QueryContext(ctx, `PRAGMA index_list('arop_publications')`)
	if err != nil {
		return errors.New("inspect SQLite publication indexes")
	}
	type metadata struct {
		name    string
		unique  int
		partial int
	}
	var indexes []metadata
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		if rows.Scan(&sequence, &name, &unique, &origin, &partial) != nil {
			return errors.New("scan SQLite publication indexes")
		}
		if origin != "c" {
			continue
		}
		indexes = append(indexes, metadata{name: name, unique: unique, partial: partial})
	}
	iterationErr := rows.Err()
	closeErr := rows.Close()
	if iterationErr != nil || closeErr != nil {
		return errors.New("scan SQLite publication indexes")
	}
	found := map[string]string{}
	for _, index := range indexes {
		if index.unique != 0 || index.partial != 0 {
			return errors.New("SQLite publication indexes are not exact")
		}
		columnRows, err := queryer.QueryContext(ctx, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index.name)
		if err != nil {
			return errors.New("inspect SQLite publication index columns")
		}
		var names []string
		for columnRows.Next() {
			var columnName string
			if columnRows.Scan(&columnName) != nil {
				columnRows.Close()
				return errors.New("scan SQLite publication index columns")
			}
			names = append(names, columnName)
		}
		iterationErr := columnRows.Err()
		closeErr := columnRows.Close()
		if iterationErr != nil || closeErr != nil {
			return errors.New("scan SQLite publication index columns")
		}
		found[index.name] = strings.Join(names, ",")
	}
	if len(found) != len(expected) {
		return errors.New("SQLite publication indexes are not exact")
	}
	for name, value := range expected {
		if found[name] != value {
			return errors.New("SQLite publication indexes are not exact")
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
