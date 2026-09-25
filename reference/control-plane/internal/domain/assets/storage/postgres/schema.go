package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

type column struct{ name, dataType string }

var assetColumns = []column{
	{"tenant_id", "text"}, {"principal_id", "text"}, {"credential_id", "text"}, {"run_id", "text"},
	{"asset_id", "text"}, {"name", "text"}, {"media_type", "text"},
	{"size_bytes", "bigint"}, {"content_digest", "text"}, {"content_bytes", "bytea"}, {"object_key", "text"}, {"status", "text"},
	{"revision", "bigint"}, {"created_at_ns", "bigint"}, {"updated_at_ns", "bigint"},
	{"idempotency_key_digest", "text"}, {"idempotency_request_digest", "text"},
}

var grantColumns = []column{
	{"tenant_id", "text"}, {"principal_id", "text"}, {"credential_id", "text"}, {"grant_id", "text"}, {"asset_id", "text"}, {"run_id", "text"},
	{"operation", "text"}, {"name", "text"}, {"media_type", "text"}, {"size_bytes", "bigint"}, {"content_digest", "text"},
	{"audience", "text"}, {"token_key_id", "text"}, {"token_digest", "text"}, {"use_limit", "bigint"}, {"use_count", "bigint"},
	{"not_before_ns", "bigint"}, {"expires_at_ns", "bigint"}, {"status", "text"}, {"revision", "bigint"},
	{"created_at_ns", "bigint"}, {"updated_at_ns", "bigint"},
	{"idempotency_key_digest", "text"}, {"idempotency_request_digest", "text"},
}

func VerifySchema() migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		if err := verifyColumns(ctx, queryer, "arop_assets", assetColumns); err != nil {
			return err
		}
		if err := verifyColumns(ctx, queryer, "arop_asset_grants", grantColumns); err != nil {
			return err
		}
		if err := verifyConstraints(ctx, queryer, "arop_assets", assetConstraints); err != nil {
			return err
		}
		if err := verifyConstraints(ctx, queryer, "arop_asset_grants", grantConstraints); err != nil {
			return err
		}
		if err := verifyIndexes(ctx, queryer, "arop_assets", map[string]indexExpectation{
			"arop_assets_pkey":               {true, true, "tenant_id,asset_id"},
			"arop_assets_idempotency_unique": {true, false, "tenant_id,idempotency_key_digest"},
			"arop_assets_tenant_status_idx":  {false, false, "tenant_id,status,asset_id"},
			"arop_assets_tenant_digest_idx":  {false, false, "tenant_id,content_digest"},
		}); err != nil {
			return err
		}
		if err := verifyIndexes(ctx, queryer, "arop_asset_grants", map[string]indexExpectation{
			"arop_asset_grants_pkey":               {true, true, "tenant_id,grant_id"},
			"arop_asset_grants_idempotency_unique": {true, false, "tenant_id,idempotency_key_digest"},
			"arop_asset_grants_token_unique":       {true, false, "token_digest"},
			"arop_asset_grants_tenant_asset_idx":   {false, false, "tenant_id,asset_id,status"},
			"arop_asset_grants_tenant_run_idx":     {false, false, "tenant_id,run_id,status"},
		}); err != nil {
			return err
		}
		var referenced string
		err := queryer.QueryRowContext(ctx, `SELECT c.relname FROM pg_constraint fk
JOIN pg_class c ON c.oid=fk.confrelid JOIN pg_class t ON t.oid=fk.conrelid
JOIN pg_namespace n ON n.oid=t.relnamespace
WHERE n.nspname=current_schema() AND t.relname='arop_asset_grants' AND fk.contype='f'`).Scan(&referenced)
		if err != nil || referenced != "arop_assets" {
			return errors.New("PostgreSQL asset grants do not reference assets exactly once")
		}
		var runReferences int
		if queryer.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint fk
JOIN pg_class c ON c.oid=fk.confrelid JOIN pg_class t ON t.oid=fk.conrelid
JOIN pg_namespace n ON n.oid=t.relnamespace
WHERE n.nspname=current_schema() AND t.relname IN ('arop_assets','arop_asset_grants') AND c.relname='arop_runs'`).Scan(&runReferences) != nil || runReferences != 0 {
			return errors.New("PostgreSQL asset schema references a run table")
		}
		return nil
	}
}

func verifyColumns(ctx context.Context, queryer migrate.Queryer, table string, columns []column) error {
	rows, err := queryer.QueryContext(ctx, `SELECT column_name,data_type,is_nullable,column_default,is_identity,is_generated
FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 ORDER BY ordinal_position`, table)
	if err != nil {
		return errors.New("inspect PostgreSQL asset columns")
	}
	index := 0
	for rows.Next() {
		var name, dataType, nullable, identity, generated string
		var defaultValue sql.NullString
		if rows.Scan(&name, &dataType, &nullable, &defaultValue, &identity, &generated) != nil || index >= len(columns) {
			rows.Close()
			return errors.New("PostgreSQL asset columns are not exact")
		}
		want := columns[index]
		if name != want.name || dataType != want.dataType || nullable != "NO" || defaultValue.Valid || identity != "NO" || generated != "NEVER" {
			rows.Close()
			return errors.New("PostgreSQL asset columns are not exact")
		}
		index++
	}
	if rows.Err() != nil || rows.Close() != nil || index != len(columns) {
		return errors.New("PostgreSQL asset columns are not exact")
	}
	return nil
}

func verifyConstraints(ctx context.Context, queryer migrate.Queryer, table string, expected map[string]string) error {
	rows, err := queryer.QueryContext(ctx, `SELECT c.conname,c.contype,c.condeferrable,c.condeferred,pg_get_constraintdef(c.oid,false)
FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace
WHERE n.nspname=current_schema() AND t.relname=$1 ORDER BY c.conname`, table)
	if err != nil {
		return errors.New("inspect PostgreSQL asset constraints")
	}
	found := map[string]string{}
	for rows.Next() {
		var name, kind, definition string
		var deferrable, deferred bool
		if rows.Scan(&name, &kind, &deferrable, &deferred, &definition) != nil || len(kind) != 1 || deferrable || deferred {
			rows.Close()
			return errors.New("scan PostgreSQL asset constraints")
		}
		if _, ok := expected[name]; !ok {
			rows.Close()
			return errors.New("PostgreSQL asset constraints are not exact")
		}
		found[name] = compact(definition)
	}
	if rows.Err() != nil || rows.Close() != nil || len(found) != len(expected) {
		return errors.New("PostgreSQL asset constraints are not exact")
	}
	for name, definition := range expected {
		if found[name] != definition {
			return errors.New("PostgreSQL asset constraints are not exact")
		}
	}
	return nil
}

type indexExpectation struct {
	unique, primary bool
	columns         string
}

func verifyIndexes(ctx context.Context, queryer migrate.Queryer, table string, expected map[string]indexExpectation) error {
	rows, err := queryer.QueryContext(ctx, `SELECT ci.relname,i.indisunique,i.indisprimary,i.indisvalid,i.indisready,
(i.indpred IS NOT NULL),(i.indexprs IS NOT NULL),am.amname,pg_get_indexdef(i.indexrelid)
FROM pg_index i JOIN pg_class t ON t.oid=i.indrelid JOIN pg_class ci ON ci.oid=i.indexrelid
JOIN pg_namespace n ON n.oid=t.relnamespace JOIN pg_am am ON am.oid=ci.relam
WHERE n.nspname=current_schema() AND t.relname=$1 ORDER BY ci.relname`, table)
	if err != nil {
		return errors.New("inspect PostgreSQL asset indexes")
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var name, method, definition string
		var unique, primary, valid, ready, partial, expression bool
		if rows.Scan(&name, &unique, &primary, &valid, &ready, &partial, &expression, &method, &definition) != nil {
			return errors.New("scan PostgreSQL asset indexes")
		}
		wanted, ok := expected[name]
		if !ok || found[name] || unique != wanted.unique || primary != wanted.primary || !valid || !ready || partial || expression || method != "btree" || !strings.HasSuffix(compact(definition), "usingbtree("+wanted.columns+")") {
			return errors.New("PostgreSQL asset indexes are not exact")
		}
		found[name] = true
	}
	if rows.Err() != nil || len(found) != len(expected) {
		return errors.New("PostgreSQL asset indexes are not exact")
	}
	return nil
}

func compact(value string) string {
	value = strings.ToLower(value)
	value = strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "", `"`, "").Replace(value)
	return strings.ReplaceAll(value, "::text", "")
}

var assetConstraints = map[string]string{
	"arop_assets_pkey":                      "primarykey(tenant_id,asset_id)",
	"arop_assets_idempotency_unique":        "unique(tenant_id,idempotency_key_digest)",
	"arop_assets_tenant_check":              "check(((tenant_id~'^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$')and(length(tenant_id)<=128)))",
	"arop_assets_principal_check":           "check((principal_id~'^prn_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))",
	"arop_assets_credential_check":          "check((credential_id~'^cred_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))",
	"arop_assets_run_id_check":              "check((run_id~'^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))",
	"arop_assets_asset_id_check":            "check((asset_id~'^asset_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))",
	"arop_assets_name_check":                "check((((length(name)>=1)and(length(name)<=512))and(name!~'\\.\\.')))",
	"arop_assets_media_type_check":          "check(((media_type~'^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$')and(length(media_type)<=127)))",
	"arop_assets_size_check":                "check(((size_bytes>=0)and(size_bytes<=104857600)))",
	"arop_assets_digest_check":              "check((content_digest~'^sha256:[0-9a-f]{64}$'))",
	"arop_assets_content_check":             "check(((((status='pending')and(octet_length(content_bytes)=0))or((status='available')and(octet_length(content_bytes)=size_bytes)))or((status='isolated')and(octet_length(content_bytes)=0))))",
	"arop_assets_object_key_check":          "check(((object_key~'^[a-z0-9][a-z0-9._/-]{0,255}$')and(object_key!~'\\.\\.')))",
	"arop_assets_status_check":              "check((status=any(array['pending','available','isolated'])))",
	"arop_assets_revision_check":            "check((revision>0))",
	"arop_assets_created_check":             "check((created_at_ns>0))",
	"arop_assets_updated_check":             "check((updated_at_ns>=created_at_ns))",
	"arop_assets_idempotency_key_check":     "check((idempotency_key_digest~'^[0-9a-f]{64}$'))",
	"arop_assets_idempotency_request_check": "check((idempotency_request_digest~'^[0-9a-f]{64}$'))",
}

var grantConstraints = map[string]string{
	"arop_asset_grants_pkey":                      "primarykey(tenant_id,grant_id)",
	"arop_asset_grants_idempotency_unique":        "unique(tenant_id,idempotency_key_digest)",
	"arop_asset_grants_token_unique":              "unique(token_digest)",
	"arop_asset_grants_asset_fkey":                "foreignkey(tenant_id,asset_id)referencesarop_assets(tenant_id,asset_id)onupdaterestrictondeleterestrict",
	"arop_asset_grants_tenant_check":              "check(((tenant_id~'^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$')and(length(tenant_id)<=128)))",
	"arop_asset_grants_principal_check":           "check((principal_id~'^prn_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))",
	"arop_asset_grants_credential_check":          "check((credential_id~'^cred_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))",
	"arop_asset_grants_grant_id_check":            "check((grant_id~'^grant_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))",
	"arop_asset_grants_asset_id_check":            "check((asset_id~'^asset_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))",
	"arop_asset_grants_run_id_check":              "check((run_id~'^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))",
	"arop_asset_grants_operation_check":           "check((operation=any(array['upload','download'])))",
	"arop_asset_grants_name_check":                "check(((((length(name)>=1)and(length(name)<=512))and(name!~'\\.\\.'))and(name!~'[/\\\\]')))",
	"arop_asset_grants_media_type_check":          "check(((media_type~'^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$')and(length(media_type)<=127)))",
	"arop_asset_grants_size_check":                "check(((size_bytes>=0)and(size_bytes<=104857600)))",
	"arop_asset_grants_digest_check":              "check((content_digest~'^sha256:[0-9a-f]{64}$'))",
	"arop_asset_grants_audience_check":            "check(((audience~'^[a-z][a-z0-9]*(?:[._:-][a-z0-9]+)*$')and(length(audience)<=100)))",
	"arop_asset_grants_token_key_check":           "check((token_key_id~'^atk_[a-za-z0-9._-]{1,60}$'))",
	"arop_asset_grants_token_check":               "check((token_digest~'^[0-9a-f]{64}$'))",
	"arop_asset_grants_use_limit_check":           "check(((use_limit>=1)and(use_limit<=8)))",
	"arop_asset_grants_use_count_check":           "check(((use_count>=0)and(use_count<=use_limit)))",
	"arop_asset_grants_active_check":              "check(((status<>'active')or(use_count<use_limit)))",
	"arop_asset_grants_consumed_check":            "check(((status<>'consumed')or(use_count=use_limit)))",
	"arop_asset_grants_not_before_check":          "check((not_before_ns>0))",
	"arop_asset_grants_expires_check":             "check(((expires_at_ns>not_before_ns)and((expires_at_ns-not_before_ns)<=900000000000)))",
	"arop_asset_grants_status_check":              "check((status=any(array['active','consumed','revoked'])))",
	"arop_asset_grants_revision_check":            "check((revision>0))",
	"arop_asset_grants_created_check":             "check((created_at_ns>0))",
	"arop_asset_grants_updated_check":             "check((updated_at_ns>=created_at_ns))",
	"arop_asset_grants_idempotency_key_check":     "check((idempotency_key_digest~'^[0-9a-f]{64}$'))",
	"arop_asset_grants_idempotency_request_check": "check((idempotency_request_digest~'^[0-9a-f]{64}$'))",
}
