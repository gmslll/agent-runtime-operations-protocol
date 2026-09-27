package internalstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/streaming"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	streamwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/streaming"
)

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

type Reader struct {
	db      *sql.DB
	dialect Dialect
}

func New(db *sql.DB, dialect Dialect) (*Reader, error) {
	if db == nil || dialect != SQLite && dialect != Postgres {
		return nil, errors.New("stream reader dependencies are required")
	}
	return &Reader{db: db, dialect: dialect}, nil
}

func (reader *Reader) Binding(ctx context.Context, tenantID, runID string) (run.AgentBinding, error) {
	var value run.AgentBinding
	err := reader.db.QueryRowContext(ctx, reader.query(`SELECT agent_id,agent_version,skill_id,manifest_digest FROM arop_runs WHERE tenant_id=? AND run_id=?`), tenantID, runID).Scan(&value.ID, &value.Version, &value.SkillID, &value.ManifestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return run.AgentBinding{}, streaming.NewError(streaming.CategoryNotFound, streaming.ReasonRunNotFound)
	}
	if err != nil || value.Validate() != nil {
		return run.AgentBinding{}, streaming.NewError(streaming.CategoryDependency, streaming.ReasonDependencyUnavailable)
	}
	return value, nil
}

func (reader *Reader) Read(ctx context.Context, tenantID, runID string, after uint64, limit int) (streaming.Page, error) {
	if limit < 1 || limit > streaming.MaxReplayEvents || after > streaming.MaxSafeInteger {
		return streaming.Page{}, streaming.NewError(streaming.CategoryValidation, streaming.ReasonInvalidRequest)
	}
	var page streaming.Page
	var terminal sql.NullString
	err := reader.db.QueryRowContext(ctx, reader.query(`SELECT last_run_sequence,terminal_state FROM arop_event_run_projections WHERE tenant_id=? AND run_id=?`), tenantID, runID).Scan(&page.Latest, &terminal)
	if errors.Is(err, sql.ErrNoRows) {
		if _, bindErr := reader.Binding(ctx, tenantID, runID); bindErr != nil {
			return streaming.Page{}, bindErr
		}
		return streaming.Page{}, nil
	}
	if err != nil {
		return streaming.Page{}, streaming.NewError(streaming.CategoryDependency, streaming.ReasonDependencyUnavailable)
	}
	page.Terminal = terminal.Valid
	var first sql.NullInt64
	if err = reader.db.QueryRowContext(ctx, reader.query(`SELECT MIN(run_sequence) FROM arop_event_ledger WHERE tenant_id=? AND run_id=?`), tenantID, runID).Scan(&first); err != nil {
		return streaming.Page{}, streaming.NewError(streaming.CategoryDependency, streaming.ReasonDependencyUnavailable)
	}
	if first.Valid {
		page.FirstAvailable = uint64(first.Int64)
	} else if page.Latest < streaming.MaxSafeInteger {
		page.FirstAvailable = page.Latest + 1
	} else {
		page.FirstAvailable = page.Latest
	}
	rows, err := reader.db.QueryContext(ctx, reader.query(`SELECT run_sequence,event_type,envelope_json FROM arop_event_ledger WHERE tenant_id=? AND run_id=? AND run_sequence>? ORDER BY run_sequence LIMIT ?`), tenantID, runID, after, limit)
	if err != nil {
		return streaming.Page{}, streaming.NewError(streaming.CategoryDependency, streaming.ReasonDependencyUnavailable)
	}
	defer rows.Close()
	var previous uint64
	for rows.Next() {
		var record streaming.Record
		var encoded []byte
		if err = rows.Scan(&record.Sequence, &record.EventType, &encoded); err != nil {
			return streaming.Page{}, streaming.NewError(streaming.CategoryDependency, streaming.ReasonDependencyUnavailable)
		}
		record.Envelope, err = bindRunSequence(encoded, record.Sequence)
		if err != nil {
			return streaming.Page{}, streaming.NewError(streaming.CategoryDependency, streaming.ReasonDependencyUnavailable)
		}
		if record.Validate(runID) != nil || previous != 0 && record.Sequence != previous+1 {
			return streaming.Page{}, streaming.NewError(streaming.CategoryDependency, streaming.ReasonDependencyUnavailable)
		}
		previous = record.Sequence
		page.Records = append(page.Records, record)
	}
	if rows.Err() != nil {
		return streaming.Page{}, streaming.NewError(streaming.CategoryDependency, streaming.ReasonDependencyUnavailable)
	}
	return page, nil
}

func bindRunSequence(encoded []byte, sequence uint64) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var object map[string]json.RawMessage
	if decoder.Decode(&object) != nil || object == nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		return nil, errors.New("invalid stored envelope")
	}
	if _, exists := object["runsequence"]; exists {
		return nil, errors.New("stored envelope assigned runsequence")
	}
	value, _ := json.Marshal(sequence)
	object["runsequence"] = value
	result, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	decoded, err := streamwire.DecodeStreamEvent(result)
	if err != nil || decoded.Runsequence == nil || uint64(*decoded.Runsequence) != sequence {
		return nil, errors.New("invalid streamed envelope")
	}
	return result, nil
}

func (reader *Reader) query(value string) string {
	if reader.dialect == SQLite {
		return value
	}
	var builder strings.Builder
	index := 1
	for _, character := range value {
		if character == '?' {
			fmt.Fprintf(&builder, "$%d", index)
			index++
		} else {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}
