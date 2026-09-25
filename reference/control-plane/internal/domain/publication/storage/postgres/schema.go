package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

type column struct {
	name, dataType string
}

var columns = []column{
	{"tenant_id", "text"}, {"agent_id", "text"}, {"version", "text"}, {"manifest_digest", "text"},
	{"bundle_semantic_digest", "text"}, {"canonical_manifest", "bytea"}, {"publisher_principal_id", "text"},
	{"idempotency_key_digest", "text"}, {"idempotency_request_digest", "text"}, {"published_at_ns", "bigint"}, {"revision", "bigint"},
}

var constraints = map[string]string{
	"arop_publications_pkey":                      "primarykey(tenant_id,agent_id,version)",
	"arop_publications_idempotency_unique":        "unique(tenant_id,idempotency_key_digest)",
	"arop_publications_tenant_check":              "check(((tenant_id~'^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$')and(length(tenant_id)<=128)))",
	"arop_publications_agent_check":               "check(((agent_id~'^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$')and(length(agent_id)<=200)))",
	"arop_publications_version_check":             "check(((length(version)>=5)and(length(version)<=200)))",
	"arop_publications_manifest_digest_check":     "check((manifest_digest~'^sha256:[0-9a-f]{64}$'))",
	"arop_publications_bundle_digest_check":       "check((bundle_semantic_digest~'^sha256:[0-9a-f]{64}$'))",
	"arop_publications_manifest_check":            "check(((octet_length(canonical_manifest)>=2)and(octet_length(canonical_manifest)<=10485760)))",
	"arop_publications_publisher_check":           "check(((length(publisher_principal_id)>=1)and(length(publisher_principal_id)<=200)))",
	"arop_publications_idempotency_key_check":     "check((idempotency_key_digest~'^[0-9a-f]{64}$'))",
	"arop_publications_idempotency_request_check": "check((idempotency_request_digest~'^[0-9a-f]{64}$'))",
	"arop_publications_published_check":           "check((published_at_ns>0))",
	"arop_publications_revision_check":            "check((revision=1))",
}

func VerifySchema() migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		rows, err := queryer.QueryContext(ctx, `SELECT column_name,data_type,is_nullable,column_default,is_identity,is_generated
FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='arop_publications' ORDER BY ordinal_position`)
		if err != nil {
			return errors.New("inspect PostgreSQL publication columns")
		}
		index := 0
		for rows.Next() {
			var name, dataType, nullable, identity, generated string
			var defaultValue sql.NullString
			if rows.Scan(&name, &dataType, &nullable, &defaultValue, &identity, &generated) != nil || index >= len(columns) {
				rows.Close()
				return errors.New("PostgreSQL publication columns are not exact")
			}
			want := columns[index]
			if name != want.name || dataType != want.dataType || nullable != "NO" || defaultValue.Valid || identity != "NO" || generated != "NEVER" {
				rows.Close()
				return errors.New("PostgreSQL publication columns are not exact")
			}
			index++
		}
		iterationErr := rows.Err()
		closeErr := rows.Close()
		if iterationErr != nil || closeErr != nil || index != len(columns) {
			return errors.New("PostgreSQL publication columns are not exact")
		}
		constraintRows, err := queryer.QueryContext(ctx, `SELECT c.conname,c.contype,c.condeferrable,c.condeferred,pg_get_constraintdef(c.oid,false)
FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace
WHERE n.nspname=current_schema() AND t.relname='arop_publications' ORDER BY c.conname`)
		if err != nil {
			return errors.New("inspect PostgreSQL publication constraints")
		}
		found := map[string]string{}
		for constraintRows.Next() {
			var name, kind, definition string
			var deferrable, deferred bool
			if constraintRows.Scan(&name, &kind, &deferrable, &deferred, &definition) != nil || len(kind) != 1 || deferrable || deferred {
				constraintRows.Close()
				return errors.New("scan PostgreSQL publication constraints")
			}
			if _, expected := constraints[name]; !expected {
				constraintRows.Close()
				return errors.New("PostgreSQL publication constraints are not exact")
			}
			found[name] = compact(definition)
		}
		iterationErr = constraintRows.Err()
		closeErr = constraintRows.Close()
		if iterationErr != nil || closeErr != nil || len(found) != len(constraints) {
			return errors.New("PostgreSQL publication constraints are not exact")
		}
		for name, definition := range constraints {
			if found[name] != definition {
				return errors.New("PostgreSQL publication constraints are not exact")
			}
		}
		return verifyIndexes(ctx, queryer)
	}
}

func verifyIndexes(ctx context.Context, queryer migrate.Queryer) error {
	type expectation struct {
		unique, primary bool
		columns         string
	}
	expected := map[string]expectation{
		"arop_publications_pkey":                 {true, true, "tenant_id,agent_id,version"},
		"arop_publications_idempotency_unique":   {true, false, "tenant_id,idempotency_key_digest"},
		"arop_publications_tenant_published_idx": {false, false, "tenant_id,published_at_ns,agent_id,version"},
		"arop_publications_manifest_digest_idx":  {false, false, "tenant_id,manifest_digest"},
	}
	rows, err := queryer.QueryContext(ctx, `SELECT ci.relname,i.indisunique,i.indisprimary,i.indisvalid,i.indisready,
(i.indpred IS NOT NULL),(i.indexprs IS NOT NULL),am.amname,pg_get_indexdef(i.indexrelid)
FROM pg_index i JOIN pg_class t ON t.oid=i.indrelid JOIN pg_class ci ON ci.oid=i.indexrelid
JOIN pg_namespace n ON n.oid=t.relnamespace JOIN pg_am am ON am.oid=ci.relam
WHERE n.nspname=current_schema() AND t.relname='arop_publications' ORDER BY ci.relname`)
	if err != nil {
		return errors.New("inspect PostgreSQL publication indexes")
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var name, method, definition string
		var unique, primary, valid, ready, partial, expression bool
		if rows.Scan(&name, &unique, &primary, &valid, &ready, &partial, &expression, &method, &definition) != nil {
			return errors.New("scan PostgreSQL publication indexes")
		}
		wanted, ok := expected[name]
		if !ok || found[name] || unique != wanted.unique || primary != wanted.primary || !valid || !ready || partial || expression || method != "btree" || !strings.HasSuffix(compact(definition), "usingbtree("+wanted.columns+")") {
			return errors.New("PostgreSQL publication indexes are not exact")
		}
		found[name] = true
	}
	if rows.Err() != nil || len(found) != len(expected) {
		return errors.New("PostgreSQL publication indexes are not exact")
	}
	return nil
}

func compact(value string) string {
	value = strings.ToLower(value)
	value = strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "", `"`, "").Replace(value)
	return strings.ReplaceAll(value, "::text", "")
}
