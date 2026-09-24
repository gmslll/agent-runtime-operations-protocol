package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

// CredentialStore is the durable SQLite/PostgreSQL repository. Every mutation
// requires the transaction-bearing context created by the P09 UnitOfWork; it
// never opens an independent transaction or falls back to memory.
type CredentialStore struct {
	db      *sql.DB
	dialect migrate.Dialect
	lookup  durable.TransactionLookup
}

func NewCredentialStore(db *sql.DB, dialect migrate.Dialect, lookup durable.TransactionLookup) (*CredentialStore, error) {
	if db == nil || lookup == nil {
		return nil, errors.New("credential store requires database and transaction lookup")
	}
	if dialect != migrate.DialectSQLite && dialect != migrate.DialectPostgres {
		return nil, errors.New("credential store dialect is unsupported")
	}
	return &CredentialStore{db: db, dialect: dialect, lookup: lookup}, nil
}

func (store *CredentialStore) Create(ctx context.Context, record *CredentialRecord) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	tx, ok := store.lookup(ctx)
	if !ok {
		return errors.New("credential mutation requires unit-of-work transaction")
	}
	insertPrincipal := `INSERT INTO arop_dev_principals(principal_id,subject_id,status,created_at_ns,updated_at_ns,revision)
VALUES(?,?,?,?,?,1) ON CONFLICT(subject_id) DO NOTHING`
	if store.dialect == migrate.DialectPostgres {
		insertPrincipal = `INSERT INTO arop_dev_principals(principal_id,subject_id,status,created_at_ns,updated_at_ns,revision)
VALUES($1,$2,$3,$4,$5,1) ON CONFLICT(subject_id) DO NOTHING`
	}
	if _, err := tx.ExecContext(ctx, insertPrincipal, record.PrincipalID, record.SubjectID, "active", record.IssuedAt.UnixNano(), record.IssuedAt.UnixNano()); err != nil {
		return fmt.Errorf("ensure development principal: %w", err)
	}
	principalQuery := `SELECT principal_id FROM arop_dev_principals WHERE subject_id=? AND status='active'`
	if store.dialect == migrate.DialectPostgres {
		principalQuery = `SELECT principal_id FROM arop_dev_principals WHERE subject_id=$1 AND status='active'`
	}
	if err := tx.QueryRowContext(ctx, principalQuery, record.SubjectID).Scan(&record.PrincipalID); err != nil {
		return fmt.Errorf("load development principal: %w", err)
	}
	statement := `INSERT INTO arop_credentials(
credential_id,principal_id,credential_kind,audience,scope_canonical,secret_verifier,
issued_at_ns,not_before_at_ns,expires_at_ns,status,revoked_at_ns,replaced_at_ns,
replacement_credential_id,revision,idempotency_key_digest,idempotency_request_digest
) VALUES(?,?,?,?,?,?,?,?,?,'active',NULL,NULL,NULL,1,?,?)`
	if store.dialect == migrate.DialectPostgres {
		statement = `INSERT INTO arop_credentials(
credential_id,principal_id,credential_kind,audience,scope_canonical,secret_verifier,
issued_at_ns,not_before_at_ns,expires_at_ns,status,revoked_at_ns,replaced_at_ns,
replacement_credential_id,revision,idempotency_key_digest,idempotency_request_digest
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'active',NULL,NULL,NULL,1,$10,$11)`
	}
	if _, err := tx.ExecContext(ctx, statement, record.CredentialID, record.PrincipalID, record.Kind, record.Audience, strings.Join(record.Scopes, " "), record.SecretVerifier, record.IssuedAt.UnixNano(), record.NotBefore.UnixNano(), record.ExpiresAt.UnixNano(), record.IdempotencyDigest, record.IdempotencyRequestDigest); err != nil {
		return credentialWriteError("create credential", err)
	}
	return nil
}

func (store *CredentialStore) Get(ctx context.Context, credentialID string) (CredentialRecord, error) {
	if !validCredentialID(credentialID) {
		return CredentialRecord{}, ErrCredentialNotFound
	}
	return store.queryOne(ctx, `c.credential_id=`+store.placeholder(1), credentialID)
}

func (store *CredentialStore) GetByIdempotencyDigest(ctx context.Context, digest string) (CredentialRecord, error) {
	if len(digest) != 64 {
		return CredentialRecord{}, ErrCredentialNotFound
	}
	return store.queryOne(ctx, `c.idempotency_key_digest=`+store.placeholder(1), digest)
}

func (store *CredentialStore) Replace(ctx context.Context, current CredentialRecord, replacement *CredentialRecord, at time.Time) error {
	if err := validateRecord(replacement); err != nil {
		return err
	}
	if current.Status != CredentialActive || current.PrincipalID != replacement.PrincipalID || current.SubjectID != replacement.SubjectID || at.IsZero() || at.Location() != time.UTC {
		return ErrCredentialConflict
	}
	tx, ok := store.lookup(ctx)
	if !ok {
		return errors.New("credential mutation requires unit-of-work transaction")
	}
	statement := `UPDATE arop_credentials SET status='replaced',replaced_at_ns=?,replacement_credential_id=?,revision=revision+1
WHERE credential_id=? AND status='active' AND revision=?`
	if store.dialect == migrate.DialectPostgres {
		statement = `UPDATE arop_credentials SET status='replaced',replaced_at_ns=$1,replacement_credential_id=$2,revision=revision+1
WHERE credential_id=$3 AND status='active' AND revision=$4`
	}
	result, err := tx.ExecContext(ctx, statement, at.UnixNano(), replacement.CredentialID, current.CredentialID, current.Revision)
	if err != nil {
		return fmt.Errorf("replace credential: %w", err)
	}
	if err := requireOneRow(result); err != nil {
		return err
	}
	return store.insertReplacement(ctx, tx, replacement)
}

func (store *CredentialStore) Revoke(ctx context.Context, current CredentialRecord, at time.Time) error {
	if current.Status != CredentialActive && current.Status != CredentialReplaced {
		return ErrCredentialConflict
	}
	if at.IsZero() || at.Location() != time.UTC {
		return errors.New("revocation time must be non-zero UTC")
	}
	tx, ok := store.lookup(ctx)
	if !ok {
		return errors.New("credential mutation requires unit-of-work transaction")
	}
	statement := `UPDATE arop_credentials SET status='revoked',revoked_at_ns=?,replaced_at_ns=NULL,replacement_credential_id=NULL,revision=revision+1
WHERE credential_id=? AND status=? AND revision=?`
	if store.dialect == migrate.DialectPostgres {
		statement = `UPDATE arop_credentials SET status='revoked',revoked_at_ns=$1,replaced_at_ns=NULL,replacement_credential_id=NULL,revision=revision+1
WHERE credential_id=$2 AND status=$3 AND revision=$4`
	}
	result, err := tx.ExecContext(ctx, statement, at.UnixNano(), current.CredentialID, string(current.Status), current.Revision)
	if err != nil {
		return fmt.Errorf("revoke credential: %w", err)
	}
	return requireOneRow(result)
}

func (store *CredentialStore) insertReplacement(ctx context.Context, tx *sql.Tx, record *CredentialRecord) error {
	statement := `INSERT INTO arop_credentials(
credential_id,principal_id,credential_kind,audience,scope_canonical,secret_verifier,
issued_at_ns,not_before_at_ns,expires_at_ns,status,revoked_at_ns,replaced_at_ns,
replacement_credential_id,revision,idempotency_key_digest,idempotency_request_digest
) VALUES(?,?,?,?,?,?,?,?,?,'active',NULL,NULL,NULL,1,?,?)`
	if store.dialect == migrate.DialectPostgres {
		statement = `INSERT INTO arop_credentials(
credential_id,principal_id,credential_kind,audience,scope_canonical,secret_verifier,
issued_at_ns,not_before_at_ns,expires_at_ns,status,revoked_at_ns,replaced_at_ns,
replacement_credential_id,revision,idempotency_key_digest,idempotency_request_digest
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'active',NULL,NULL,NULL,1,$10,$11)`
	}
	if _, err := tx.ExecContext(ctx, statement, record.CredentialID, record.PrincipalID, record.Kind, record.Audience, strings.Join(record.Scopes, " "), record.SecretVerifier, record.IssuedAt.UnixNano(), record.NotBefore.UnixNano(), record.ExpiresAt.UnixNano(), record.IdempotencyDigest, record.IdempotencyRequestDigest); err != nil {
		return credentialWriteError("create replacement credential", err)
	}
	return nil
}

func (store *CredentialStore) queryOne(ctx context.Context, predicate string, argument any) (CredentialRecord, error) {
	statement := `SELECT c.credential_id,c.principal_id,p.subject_id,c.credential_kind,c.audience,c.scope_canonical,
c.secret_verifier,c.issued_at_ns,c.not_before_at_ns,c.expires_at_ns,c.status,c.revoked_at_ns,
c.replaced_at_ns,c.replacement_credential_id,c.revision,c.idempotency_key_digest,c.idempotency_request_digest
FROM arop_credentials c JOIN arop_dev_principals p ON p.principal_id=c.principal_id WHERE ` + predicate
	queryer := migrate.Queryer(store.db)
	if tx, ok := store.lookup(ctx); ok {
		queryer = tx
	}
	var record CredentialRecord
	var issued, notBefore, expires int64
	var revoked, replaced sql.NullInt64
	var replacement sql.NullString
	var scope string
	err := queryer.QueryRowContext(ctx, statement, argument).Scan(&record.CredentialID, &record.PrincipalID, &record.SubjectID, &record.Kind, &record.Audience, &scope, &record.SecretVerifier, &issued, &notBefore, &expires, &record.Status, &revoked, &replaced, &replacement, &record.Revision, &record.IdempotencyDigest, &record.IdempotencyRequestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialRecord{}, ErrCredentialNotFound
	}
	if err != nil {
		return CredentialRecord{}, fmt.Errorf("load credential: %w", err)
	}
	record.Scopes = strings.Fields(scope)
	record.IssuedAt, record.NotBefore, record.ExpiresAt = time.Unix(0, issued).UTC(), time.Unix(0, notBefore).UTC(), time.Unix(0, expires).UTC()
	if revoked.Valid {
		record.RevokedAt = time.Unix(0, revoked.Int64).UTC()
	}
	if replaced.Valid {
		record.ReplacedAt = time.Unix(0, replaced.Int64).UTC()
	}
	if replacement.Valid {
		record.ReplacementID = replacement.String
	}
	if err := validateStoredRecord(&record); err != nil {
		return CredentialRecord{}, fmt.Errorf("stored credential violates contract: %w", err)
	}
	return record, nil
}

func (store *CredentialStore) placeholder(position int) string {
	if store.dialect == migrate.DialectPostgres {
		return fmt.Sprintf("$%d", position)
	}
	return "?"
}

func validateRecord(record *CredentialRecord) error {
	if record == nil || !validCredentialID(record.CredentialID) || !strings.HasPrefix(record.PrincipalID, "prn_") || !domainIDPattern.MatchString(record.PrincipalID) || !identifierPattern.MatchString(record.SubjectID) || !identifierPattern.MatchString(record.Kind) || !identifierPattern.MatchString(record.Audience) {
		return errors.New("credential record identity fields are invalid")
	}
	if record.Status != CredentialActive || record.Revision != 1 || !digestPattern.MatchString(record.SecretVerifier) || !digestPattern.MatchString(record.IdempotencyDigest) || !digestPattern.MatchString(record.IdempotencyRequestDigest) || len(record.Scopes) == 0 {
		return errors.New("new credential record lifecycle fields are invalid")
	}
	if record.IssuedAt.IsZero() || record.IssuedAt.Location() != time.UTC || record.NotBefore.Location() != time.UTC || record.ExpiresAt.Location() != time.UTC || record.NotBefore.Before(record.IssuedAt) || !record.NotBefore.Before(record.ExpiresAt) {
		return errors.New("credential record time bounds are invalid")
	}
	return nil
}

func validateStoredRecord(record *CredentialRecord) error {
	if record == nil || !validCredentialID(record.CredentialID) || !strings.HasPrefix(record.PrincipalID, "prn_") || !domainIDPattern.MatchString(record.PrincipalID) || !digestPattern.MatchString(record.SecretVerifier) || !digestPattern.MatchString(record.IdempotencyDigest) || !digestPattern.MatchString(record.IdempotencyRequestDigest) || record.Revision < 1 || len(record.Scopes) == 0 {
		return errors.New("credential record is invalid")
	}
	if record.NotBefore.Before(record.IssuedAt) || !record.NotBefore.Before(record.ExpiresAt) {
		return errors.New("credential record time bounds are invalid")
	}
	switch record.Status {
	case CredentialActive:
		if !record.RevokedAt.IsZero() || !record.ReplacedAt.IsZero() || record.ReplacementID != "" {
			return errors.New("active credential has terminal metadata")
		}
	case CredentialRevoked:
		if record.RevokedAt.IsZero() || !record.ReplacedAt.IsZero() || record.ReplacementID != "" {
			return errors.New("revoked credential metadata is invalid")
		}
	case CredentialReplaced:
		if record.ReplacedAt.IsZero() || record.ReplacementID == "" || !record.RevokedAt.IsZero() {
			return errors.New("replaced credential metadata is invalid")
		}
	default:
		return errors.New("credential status is invalid")
	}
	return nil
}

func requireOneRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect credential mutation: %w", err)
	}
	if count != 1 {
		return ErrCredentialConflict
	}
	return nil
}

func credentialWriteError(operation string, err error) error {
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "unique constraint") || strings.Contains(lower, "duplicate key") || strings.Contains(lower, "constraint failed") {
		return fmt.Errorf("%s: %w", operation, ErrCredentialConflict)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

var _ CredentialRepository = (*CredentialStore)(nil)

// VerifySchema returns the production identity schema verifier composed into
// the P09 migration runner. Errors intentionally identify only the failed
// contract boundary; catalog SQL and database diagnostics are not exposed.
func VerifySchema(dialect migrate.Dialect) migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		if dialect == migrate.DialectSQLite {
			return verifySQLiteIdentitySchema(ctx, queryer)
		}
		if dialect == migrate.DialectPostgres {
			return verifyPostgresIdentitySchema(ctx, queryer)
		}
		return errors.New("identity schema dialect is unsupported")
	}
}

type identityColumn struct {
	name, dataType string
	notNull        bool
	primaryKey     int
}

var identityTables = map[string][]identityColumn{
	"arop_dev_principals": {
		{"principal_id", "TEXT", true, 1}, {"subject_id", "TEXT", true, 0},
		{"status", "TEXT", true, 0}, {"created_at_ns", "INTEGER", true, 0},
		{"updated_at_ns", "INTEGER", true, 0}, {"revision", "INTEGER", true, 0},
	},
	"arop_credentials": {
		{"credential_id", "TEXT", true, 1}, {"principal_id", "TEXT", true, 0},
		{"credential_kind", "TEXT", true, 0}, {"audience", "TEXT", true, 0},
		{"scope_canonical", "TEXT", true, 0}, {"secret_verifier", "TEXT", true, 0},
		{"issued_at_ns", "INTEGER", true, 0}, {"not_before_at_ns", "INTEGER", true, 0},
		{"expires_at_ns", "INTEGER", true, 0}, {"status", "TEXT", true, 0},
		{"revoked_at_ns", "INTEGER", false, 0}, {"replaced_at_ns", "INTEGER", false, 0},
		{"replacement_credential_id", "TEXT", false, 0}, {"revision", "INTEGER", true, 0},
		{"idempotency_key_digest", "TEXT", true, 0}, {"idempotency_request_digest", "TEXT", true, 0},
	},
}

var identityConstraintFragments = map[string][]string{
	"arop_dev_principals": {
		"constraintarop_dev_principals_pkeyprimarykey(principal_id)",
		"constraintarop_dev_principals_subject_uniqueunique(subject_id)",
		"constraintarop_dev_principals_principal_id_checkcheck(length(principal_id)between1and200)",
		"constraintarop_dev_principals_subject_id_checkcheck(length(subject_id)between1and200)",
		"constraintarop_dev_principals_status_checkcheck(statusin('active','disabled'))",
		"constraintarop_dev_principals_created_checkcheck(created_at_ns>0)",
		"constraintarop_dev_principals_updated_checkcheck(updated_at_ns>=created_at_ns)",
		"constraintarop_dev_principals_revision_checkcheck(revision>0)",
	},
	"arop_credentials": {
		"constraintarop_credentials_pkeyprimarykey(credential_id)",
		"constraintarop_credentials_principal_fkeyforeignkey(principal_id)referencesarop_dev_principals(principal_id)onupdaterestrictondeleterestrict",
		"constraintarop_credentials_replacement_fkeyforeignkey(replacement_credential_id)referencesarop_credentials(credential_id)onupdaterestrictondeleterestrictdeferrableinitiallydeferred",
		"constraintarop_credentials_verifier_uniqueunique(secret_verifier)",
		"constraintarop_credentials_idempotency_uniqueunique(idempotency_key_digest)",
		"constraintarop_credentials_replacement_uniqueunique(replacement_credential_id)",
		"constraintarop_credentials_id_checkcheck(length(credential_id)between1and200)",
		"constraintarop_credentials_kind_checkcheck(length(credential_kind)between1and100)",
		"constraintarop_credentials_audience_checkcheck(length(audience)between1and500)",
		"constraintarop_credentials_scope_checkcheck(length(scope_canonical)between1and4096)",
		"constraintarop_credentials_verifier_checkcheck(length(secret_verifier)=64andsecret_verifiernotglob'*[^0-9a-f]*')",
		"constraintarop_credentials_idempotency_checkcheck(length(idempotency_key_digest)=64andidempotency_key_digestnotglob'*[^0-9a-f]*')",
		"constraintarop_credentials_idempotency_request_checkcheck(length(idempotency_request_digest)=64andidempotency_request_digestnotglob'*[^0-9a-f]*')",
		"constraintarop_credentials_time_checkcheck(issued_at_ns>0andnot_before_at_ns>=issued_at_nsandexpires_at_ns>not_before_at_ns)",
		"constraintarop_credentials_status_checkcheck(statusin('active','revoked','replaced'))",
		"constraintarop_credentials_lifecycle_checkcheck((status='active'andrevoked_at_nsisnullandreplaced_at_nsisnullandreplacement_credential_idisnull)or(status='revoked'andrevoked_at_nsisnotnullandrevoked_at_ns>=issued_at_nsandreplaced_at_nsisnullandreplacement_credential_idisnull)or(status='replaced'andrevoked_at_nsisnullandreplaced_at_nsisnotnullandreplaced_at_ns>=issued_at_nsandreplacement_credential_idisnotnullandreplacement_credential_id<>credential_id))",
		"constraintarop_credentials_revision_checkcheck(revision>0)",
	},
}

func verifySQLiteIdentitySchema(ctx context.Context, queryer migrate.Queryer) error {
	for table, expected := range identityTables {
		if err := verifySQLiteIdentityColumns(ctx, queryer, table, expected); err != nil {
			return err
		}
		var ddl string
		if err := queryer.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl); err != nil {
			return errors.New("SQLite identity table is missing")
		}
		compact := compactIdentitySQL(stripSQLComments(ddl))
		for _, fragment := range identityConstraintFragments[table] {
			if !strings.Contains(compact, fragment) {
				return errors.New("SQLite identity constraints are not exact")
			}
		}
		expectedCounts := map[string]map[string]int{
			"arop_dev_principals": {"constraint": 8, "primarykey(": 1, "unique(": 1, "foreignkey(": 0, "check(": 6},
			"arop_credentials":    {"constraint": 17, "primarykey(": 1, "unique(": 3, "foreignkey(": 2, "check(": 11},
		}
		for token, count := range expectedCounts[table] {
			if strings.Count(compact, token) != count {
				return errors.New("SQLite identity constraints are not exact")
			}
		}
	}
	if err := verifySQLiteIdentityForeignKeys(ctx, queryer); err != nil {
		return err
	}
	return verifySQLiteIdentityIndexes(ctx, queryer)
}

func verifySQLiteIdentityColumns(ctx context.Context, queryer migrate.Queryer, table string, expected []identityColumn) error {
	rows, err := queryer.QueryContext(ctx, `PRAGMA table_info('`+table+`')`)
	if err != nil {
		return errors.New("inspect SQLite identity columns")
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return errors.New("scan SQLite identity columns")
		}
		if index >= len(expected) || cid != index || name != expected[index].name || strings.ToUpper(dataType) != expected[index].dataType || (notNull != 0) != expected[index].notNull || primaryKey != expected[index].primaryKey || defaultValue.Valid {
			return errors.New("SQLite identity columns are not exact")
		}
		index++
	}
	if rows.Err() != nil || index != len(expected) {
		return errors.New("SQLite identity columns are not exact")
	}
	return nil
}

func verifySQLiteIdentityForeignKeys(ctx context.Context, queryer migrate.Queryer) error {
	rows, err := queryer.QueryContext(ctx, `PRAGMA foreign_key_list('arop_credentials')`)
	if err != nil {
		return errors.New("inspect SQLite identity foreign keys")
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var id, sequence int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &sequence, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return errors.New("scan SQLite identity foreign keys")
		}
		if sequence != 0 || strings.ToUpper(onUpdate) != "RESTRICT" || strings.ToUpper(onDelete) != "RESTRICT" {
			return errors.New("SQLite identity foreign keys are not exact")
		}
		found[from] = table + "." + to
	}
	if rows.Err() != nil || len(found) != 2 || found["principal_id"] != "arop_dev_principals.principal_id" || found["replacement_credential_id"] != "arop_credentials.credential_id" {
		return errors.New("SQLite identity foreign keys are not exact")
	}
	return nil
}

func verifySQLiteIdentityIndexes(ctx context.Context, queryer migrate.Queryer) error {
	type indexMetadata struct {
		name, origin    string
		unique, partial int
	}
	expected := map[string]map[string]int{
		"arop_dev_principals": {
			"pk|1|0|principal_id": 1,
			"u|1|0|subject_id":    1,
		},
		"arop_credentials": {
			"pk|1|0|credential_id":            1,
			"u|1|0|secret_verifier":           1,
			"u|1|0|idempotency_key_digest":    1,
			"u|1|0|replacement_credential_id": 1,
			"c|0|0|arop_credentials_principal_status_expiry_idx|principal_id,status,expires_at_ns": 1,
			"c|0|0|arop_credentials_audience_status_expiry_idx|audience,status,expires_at_ns":      1,
		},
	}
	for table, wanted := range expected {
		rows, err := queryer.QueryContext(ctx, `PRAGMA index_list('`+table+`')`)
		if err != nil {
			return errors.New("inspect SQLite identity indexes")
		}
		var indexes []indexMetadata
		for rows.Next() {
			var sequence, unique, partial int
			var name, origin string
			if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
				_ = rows.Close()
				return errors.New("scan SQLite identity indexes")
			}
			indexes = append(indexes, indexMetadata{name: name, origin: origin, unique: unique, partial: partial})
		}
		iterationErr := rows.Err()
		closeErr := rows.Close()
		if iterationErr != nil || closeErr != nil {
			return errors.New("SQLite identity indexes are not exact")
		}
		found := map[string]int{}
		for _, metadata := range indexes {
			columns := sqliteIndexColumns(ctx, queryer, metadata.name)
			if len(columns) == 0 {
				return errors.New("SQLite identity indexes are not exact")
			}
			signature := fmt.Sprintf("%s|%d|%d|", metadata.origin, metadata.unique, metadata.partial)
			if metadata.origin == "c" {
				signature += metadata.name + "|"
			}
			signature += strings.Join(columns, ",")
			found[signature]++
		}
		if !equalStringCounts(found, wanted) {
			return errors.New("SQLite identity indexes are not exact")
		}
	}
	return nil
}

func sqliteIndexColumns(ctx context.Context, queryer migrate.Queryer, name string) []string {
	rows, err := queryer.QueryContext(ctx, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if rows.Scan(&column) != nil {
			return nil
		}
		columns = append(columns, column)
	}
	if rows.Err() != nil {
		return nil
	}
	return columns
}

func verifyPostgresIdentitySchema(ctx context.Context, queryer migrate.Queryer) error {
	for table, sqliteColumns := range identityTables {
		rows, err := queryer.QueryContext(ctx, `SELECT column_name,data_type,is_nullable,column_default,is_identity,is_generated FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 ORDER BY ordinal_position`, table)
		if err != nil {
			return errors.New("inspect PostgreSQL identity columns")
		}
		index := 0
		for rows.Next() {
			var name, dataType, nullable, identity, generated string
			var defaultValue sql.NullString
			if rows.Scan(&name, &dataType, &nullable, &defaultValue, &identity, &generated) != nil || index >= len(sqliteColumns) {
				rows.Close()
				return errors.New("PostgreSQL identity columns are not exact")
			}
			expectedType := "text"
			if sqliteColumns[index].dataType == "INTEGER" {
				expectedType = "bigint"
			}
			if name != sqliteColumns[index].name || dataType != expectedType || (nullable == "NO") != sqliteColumns[index].notNull || defaultValue.Valid || identity != "NO" || generated != "NEVER" {
				rows.Close()
				return errors.New("PostgreSQL identity columns are not exact")
			}
			index++
		}
		iterationErr := rows.Err()
		if rows.Close() != nil || iterationErr != nil || index != len(sqliteColumns) {
			return errors.New("PostgreSQL identity columns are not exact")
		}
	}
	if err := verifyPostgresIdentityConstraints(ctx, queryer); err != nil {
		return err
	}
	return verifyPostgresIdentityIndexes(ctx, queryer)
}

func verifyPostgresIdentityConstraints(ctx context.Context, queryer migrate.Queryer) error {
	type constraintExpectation struct {
		table                string
		kind                 byte
		deferrable, deferred bool
	}
	expected := map[string]constraintExpectation{
		"arop_dev_principals_pkey": {"arop_dev_principals", 'p', false, false}, "arop_dev_principals_subject_unique": {"arop_dev_principals", 'u', false, false},
		"arop_dev_principals_principal_id_check": {"arop_dev_principals", 'c', false, false}, "arop_dev_principals_subject_id_check": {"arop_dev_principals", 'c', false, false},
		"arop_dev_principals_status_check": {"arop_dev_principals", 'c', false, false}, "arop_dev_principals_created_check": {"arop_dev_principals", 'c', false, false},
		"arop_dev_principals_updated_check": {"arop_dev_principals", 'c', false, false}, "arop_dev_principals_revision_check": {"arop_dev_principals", 'c', false, false},
		"arop_credentials_pkey": {"arop_credentials", 'p', false, false}, "arop_credentials_principal_fkey": {"arop_credentials", 'f', false, false},
		"arop_credentials_replacement_fkey": {"arop_credentials", 'f', true, true}, "arop_credentials_verifier_unique": {"arop_credentials", 'u', false, false},
		"arop_credentials_idempotency_unique": {"arop_credentials", 'u', false, false}, "arop_credentials_replacement_unique": {"arop_credentials", 'u', false, false},
		"arop_credentials_id_check": {"arop_credentials", 'c', false, false}, "arop_credentials_kind_check": {"arop_credentials", 'c', false, false},
		"arop_credentials_audience_check": {"arop_credentials", 'c', false, false}, "arop_credentials_scope_check": {"arop_credentials", 'c', false, false},
		"arop_credentials_verifier_check": {"arop_credentials", 'c', false, false}, "arop_credentials_idempotency_check": {"arop_credentials", 'c', false, false},
		"arop_credentials_idempotency_request_check": {"arop_credentials", 'c', false, false}, "arop_credentials_time_check": {"arop_credentials", 'c', false, false},
		"arop_credentials_status_check": {"arop_credentials", 'c', false, false}, "arop_credentials_lifecycle_check": {"arop_credentials", 'c', false, false},
		"arop_credentials_revision_check": {"arop_credentials", 'c', false, false},
	}
	rows, err := queryer.QueryContext(ctx, `SELECT t.relname,c.conname,c.contype,c.condeferrable,c.condeferred,pg_get_constraintdef(c.oid,false) FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=current_schema() AND t.relname IN ('arop_dev_principals','arop_credentials')`)
	if err != nil {
		return errors.New("inspect PostgreSQL identity constraints")
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var table, name, kind, definition string
		var deferrable, deferred bool
		if rows.Scan(&table, &name, &kind, &deferrable, &deferred, &definition) != nil || len(kind) != 1 {
			return errors.New("PostgreSQL identity constraints are not exact")
		}
		wanted, ok := expected[name]
		if !ok || wanted.table != table || wanted.kind != kind[0] || wanted.deferrable != deferrable || wanted.deferred != deferred {
			return errors.New("PostgreSQL identity constraints are not exact")
		}
		if _, duplicate := found[name]; duplicate {
			return errors.New("PostgreSQL identity constraints are not exact")
		}
		found[name] = compactIdentitySQL(definition)
	}
	if rows.Err() != nil || len(found) != len(expected) {
		return errors.New("PostgreSQL identity constraints are not exact")
	}
	for name := range expected {
		if _, ok := found[name]; !ok {
			return errors.New("PostgreSQL identity constraints are not exact")
		}
	}
	definitions := map[string]string{
		"arop_dev_principals_pkey":                   "primarykey(principal_id)",
		"arop_dev_principals_subject_unique":         "unique(subject_id)",
		"arop_dev_principals_principal_id_check":     "check(((length(principal_id)>=1)and(length(principal_id)<=200)))",
		"arop_dev_principals_subject_id_check":       "check(((length(subject_id)>=1)and(length(subject_id)<=200)))",
		"arop_dev_principals_status_check":           "check((status=any(array['active','disabled'])))",
		"arop_dev_principals_created_check":          "check((created_at_ns>0))",
		"arop_dev_principals_updated_check":          "check((updated_at_ns>=created_at_ns))",
		"arop_dev_principals_revision_check":         "check((revision>0))",
		"arop_credentials_pkey":                      "primarykey(credential_id)",
		"arop_credentials_principal_fkey":            "foreignkey(principal_id)referencesarop_dev_principals(principal_id)onupdaterestrictondeleterestrict",
		"arop_credentials_replacement_fkey":          "foreignkey(replacement_credential_id)referencesarop_credentials(credential_id)onupdaterestrictondeleterestrictdeferrableinitiallydeferred",
		"arop_credentials_verifier_unique":           "unique(secret_verifier)",
		"arop_credentials_idempotency_unique":        "unique(idempotency_key_digest)",
		"arop_credentials_replacement_unique":        "unique(replacement_credential_id)",
		"arop_credentials_id_check":                  "check(((length(credential_id)>=1)and(length(credential_id)<=200)))",
		"arop_credentials_kind_check":                "check(((length(credential_kind)>=1)and(length(credential_kind)<=100)))",
		"arop_credentials_audience_check":            "check(((length(audience)>=1)and(length(audience)<=500)))",
		"arop_credentials_scope_check":               "check(((length(scope_canonical)>=1)and(length(scope_canonical)<=4096)))",
		"arop_credentials_verifier_check":            "check((secret_verifier~'^[0-9a-f]{64}$'))",
		"arop_credentials_idempotency_check":         "check((idempotency_key_digest~'^[0-9a-f]{64}$'))",
		"arop_credentials_idempotency_request_check": "check((idempotency_request_digest~'^[0-9a-f]{64}$'))",
		"arop_credentials_time_check":                "check(((issued_at_ns>0)and(not_before_at_ns>=issued_at_ns)and(expires_at_ns>not_before_at_ns)))",
		"arop_credentials_status_check":              "check((status=any(array['active','revoked','replaced'])))",
		"arop_credentials_lifecycle_check":           "check((((status='active')and(revoked_at_nsisnull)and(replaced_at_nsisnull)and(replacement_credential_idisnull))or((status='revoked')and(revoked_at_nsisnotnull)and(revoked_at_ns>=issued_at_ns)and(replaced_at_nsisnull)and(replacement_credential_idisnull))or((status='replaced')and(revoked_at_nsisnull)and(replaced_at_nsisnotnull)and(replaced_at_ns>=issued_at_ns)and(replacement_credential_idisnotnull)and(replacement_credential_id<>credential_id))))",
		"arop_credentials_revision_check":            "check((revision>0))",
	}
	for name, definition := range definitions {
		if found[name] != definition {
			return errors.New("PostgreSQL identity constraint definitions are not exact")
		}
	}
	return nil
}

func verifyPostgresIdentityIndexes(ctx context.Context, queryer migrate.Queryer) error {
	type indexExpectation struct {
		table           string
		unique, primary bool
		columns         string
	}
	expected := map[string]indexExpectation{
		"arop_dev_principals_pkey":                     {"arop_dev_principals", true, true, "principal_id"},
		"arop_dev_principals_subject_unique":           {"arop_dev_principals", true, false, "subject_id"},
		"arop_credentials_pkey":                        {"arop_credentials", true, true, "credential_id"},
		"arop_credentials_verifier_unique":             {"arop_credentials", true, false, "secret_verifier"},
		"arop_credentials_idempotency_unique":          {"arop_credentials", true, false, "idempotency_key_digest"},
		"arop_credentials_replacement_unique":          {"arop_credentials", true, false, "replacement_credential_id"},
		"arop_credentials_principal_status_expiry_idx": {"arop_credentials", false, false, "principal_id,status,expires_at_ns"},
		"arop_credentials_audience_status_expiry_idx":  {"arop_credentials", false, false, "audience,status,expires_at_ns"},
	}
	rows, err := queryer.QueryContext(ctx, `SELECT t.relname,ci.relname,i.indisunique,i.indisprimary,i.indisvalid,i.indisready,(i.indpred IS NOT NULL),(i.indexprs IS NOT NULL),am.amname,pg_get_indexdef(i.indexrelid) FROM pg_index i JOIN pg_class t ON t.oid=i.indrelid JOIN pg_class ci ON ci.oid=i.indexrelid JOIN pg_namespace n ON n.oid=t.relnamespace JOIN pg_am am ON am.oid=ci.relam WHERE n.nspname=current_schema() AND t.relname IN ('arop_dev_principals','arop_credentials') ORDER BY t.relname,ci.relname`)
	if err != nil {
		return errors.New("inspect PostgreSQL identity indexes")
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var table, name, method, definition string
		var unique, primary, valid, ready, partial, expression bool
		if rows.Scan(&table, &name, &unique, &primary, &valid, &ready, &partial, &expression, &method, &definition) != nil {
			return errors.New("scan PostgreSQL identity indexes")
		}
		wanted, ok := expected[name]
		compact := compactIdentitySQL(definition)
		if !ok || found[name] || wanted.table != table || wanted.unique != unique || wanted.primary != primary || !valid || !ready || partial || expression || method != "btree" || !strings.HasSuffix(compact, "usingbtree("+wanted.columns+")") {
			return errors.New("PostgreSQL identity indexes are not exact")
		}
		found[name] = true
	}
	if rows.Err() != nil || len(found) != len(expected) {
		return errors.New("PostgreSQL identity indexes are not exact")
	}
	return nil
}

func equalStringCounts(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, count := range left {
		if right[key] != count {
			return false
		}
	}
	return true
}

func stripSQLComments(value string) string {
	var result strings.Builder
	inQuote := false
	for index := 0; index < len(value); {
		if value[index] == '\'' {
			inQuote = !inQuote
			result.WriteByte(value[index])
			index++
			continue
		}
		if !inQuote && index+1 < len(value) && value[index:index+2] == "--" {
			for index < len(value) && value[index] != '\n' {
				index++
			}
			continue
		}
		if !inQuote && index+1 < len(value) && value[index:index+2] == "/*" {
			end := strings.Index(value[index+2:], "*/")
			if end < 0 {
				return ""
			}
			index += end + 4
			continue
		}
		result.WriteByte(value[index])
		index++
	}
	return result.String()
}

func compactIdentitySQL(value string) string {
	value = strings.ToLower(value)
	value = strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "", `"`, "").Replace(value)
	value = strings.ReplaceAll(value, "::text", "")
	return value
}
