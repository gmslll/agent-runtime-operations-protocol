package internalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSQLiteReaderBindsRunSequenceAndScopesTenant(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE arop_runs(tenant_id TEXT,run_id TEXT,agent_id TEXT,agent_version TEXT,skill_id TEXT,manifest_digest TEXT)`,
		`CREATE TABLE arop_event_run_projections(tenant_id TEXT,run_id TEXT,last_run_sequence INTEGER,terminal_state TEXT)`,
		`CREATE TABLE arop_event_ledger(tenant_id TEXT,run_id TEXT,run_sequence INTEGER,event_type TEXT,envelope_json TEXT)`,
		`INSERT INTO arop_runs VALUES('tenant-a','run_01999999-9999-7999-8999-999999999999','agent-a','1.0.0','answer','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')`,
		`INSERT INTO arop_event_run_projections VALUES('tenant-a','run_01999999-9999-7999-8999-999999999999',1,'succeeded')`,
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	envelope := map[string]any{"specversion": "1.0", "id": "evt_01999999-9999-7999-8999-999999999996", "source": "https://runtime.example.invalid/instances/runtime-a", "type": "io.kinglucky.arop.output.delta.v1", "subject": "runs/run_01999999-9999-7999-8999-999999999999", "time": "2026-09-27T00:00:00Z", "datacontenttype": "application/json", "dataschema": "https://arop.invalid/schemas/v1/events/output-events-v1.schema.json", "runid": "run_01999999-9999-7999-8999-999999999999", "attemptid": "att_01999999-9999-7999-8999-999999999995", "producersequence": 1, "traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "data": map[string]any{"output_id": "answer", "offset": 0, "delta": "x"}}
	encoded, _ := json.Marshal(envelope)
	if _, err = db.Exec(`INSERT INTO arop_event_ledger VALUES(?,?,?,?,?)`, "tenant-a", "run_01999999-9999-7999-8999-999999999999", 1, "io.kinglucky.arop.output.delta.v1", string(encoded)); err != nil {
		t.Fatal(err)
	}
	reader, err := New(db, SQLite)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.Read(context.Background(), "tenant-a", "run_01999999-9999-7999-8999-999999999999", 0, 10)
	if err != nil || len(page.Records) != 1 || page.Records[0].Sequence != 1 || !page.Terminal {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	var wire map[string]any
	_ = json.Unmarshal(page.Records[0].Envelope, &wire)
	if wire["runsequence"] != float64(1) {
		t.Fatalf("wire=%v", wire)
	}
	if _, err = reader.Binding(context.Background(), "tenant-b", "run_01999999-9999-7999-8999-999999999999"); err == nil {
		t.Fatal("cross-tenant read accepted")
	}
}

func TestBindRunSequenceRejectsExistingOrTrailingEnvelope(t *testing.T) {
	for _, value := range [][]byte{[]byte(`{"runsequence":1}`), []byte(`{} {}`)} {
		if _, err := bindRunSequence(value, 1); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}
