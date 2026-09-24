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
replacement_credential_id,revision,idempotency_key_digest
) VALUES(?,?,?,?,?,?,?,?,?,'active',NULL,NULL,NULL,1,?)`
	if store.dialect == migrate.DialectPostgres {
		statement = `INSERT INTO arop_credentials(
credential_id,principal_id,credential_kind,audience,scope_canonical,secret_verifier,
issued_at_ns,not_before_at_ns,expires_at_ns,status,revoked_at_ns,replaced_at_ns,
replacement_credential_id,revision,idempotency_key_digest
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'active',NULL,NULL,NULL,1,$10)`
	}
	if _, err := tx.ExecContext(ctx, statement, record.CredentialID, record.PrincipalID, record.Kind, record.Audience, strings.Join(record.Scopes, " "), record.SecretVerifier, record.IssuedAt.UnixNano(), record.NotBefore.UnixNano(), record.ExpiresAt.UnixNano(), record.IdempotencyDigest); err != nil {
		return fmt.Errorf("create credential: %w", err)
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
	if current.Status != CredentialActive || current.PrincipalID != replacement.PrincipalID || current.SubjectID != replacement.SubjectID || !at.Equal(at.UTC()) {
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
replacement_credential_id,revision,idempotency_key_digest
) VALUES(?,?,?,?,?,?,?,?,?,'active',NULL,NULL,NULL,1,?)`
	if store.dialect == migrate.DialectPostgres {
		statement = `INSERT INTO arop_credentials(
credential_id,principal_id,credential_kind,audience,scope_canonical,secret_verifier,
issued_at_ns,not_before_at_ns,expires_at_ns,status,revoked_at_ns,replaced_at_ns,
replacement_credential_id,revision,idempotency_key_digest
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'active',NULL,NULL,NULL,1,$10)`
	}
	if _, err := tx.ExecContext(ctx, statement, record.CredentialID, record.PrincipalID, record.Kind, record.Audience, strings.Join(record.Scopes, " "), record.SecretVerifier, record.IssuedAt.UnixNano(), record.NotBefore.UnixNano(), record.ExpiresAt.UnixNano(), record.IdempotencyDigest); err != nil {
		return fmt.Errorf("create replacement credential: %w", err)
	}
	return nil
}

func (store *CredentialStore) queryOne(ctx context.Context, predicate string, argument any) (CredentialRecord, error) {
	statement := `SELECT c.credential_id,c.principal_id,p.subject_id,c.credential_kind,c.audience,c.scope_canonical,
c.secret_verifier,c.issued_at_ns,c.not_before_at_ns,c.expires_at_ns,c.status,c.revoked_at_ns,
c.replaced_at_ns,c.replacement_credential_id,c.revision,c.idempotency_key_digest
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
	err := queryer.QueryRowContext(ctx, statement, argument).Scan(&record.CredentialID, &record.PrincipalID, &record.SubjectID, &record.Kind, &record.Audience, &scope, &record.SecretVerifier, &issued, &notBefore, &expires, &record.Status, &revoked, &replaced, &replacement, &record.Revision, &record.IdempotencyDigest)
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
	if record.Status != CredentialActive || record.Revision != 1 || len(record.SecretVerifier) != 64 || len(record.IdempotencyDigest) != 64 || len(record.Scopes) == 0 {
		return errors.New("new credential record lifecycle fields are invalid")
	}
	if record.IssuedAt.IsZero() || record.IssuedAt.Location() != time.UTC || record.NotBefore.Location() != time.UTC || record.ExpiresAt.Location() != time.UTC || record.NotBefore.Before(record.IssuedAt) || !record.NotBefore.Before(record.ExpiresAt) {
		return errors.New("credential record time bounds are invalid")
	}
	return nil
}

func validateStoredRecord(record *CredentialRecord) error {
	if record == nil || !validCredentialID(record.CredentialID) || !strings.HasPrefix(record.PrincipalID, "prn_") || !domainIDPattern.MatchString(record.PrincipalID) || len(record.SecretVerifier) != 64 || len(record.IdempotencyDigest) != 64 || record.Revision < 1 || len(record.Scopes) == 0 {
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

var _ CredentialRepository = (*CredentialStore)(nil)
