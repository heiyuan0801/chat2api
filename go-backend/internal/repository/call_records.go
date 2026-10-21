package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// CallRecordWrite is a final immutable invocation record. Account Repository
// mutations and Call Record appends keep independent transaction boundaries.
type CallRecordWrite struct {
	Payload                                                                          Document
	Outcome, Endpoint, Model, AccountEmail, ConversationID, BusinessKind, SearchText string
}
type CallRecords struct{ database *Database }

func NewCallRecords(database *Database) *CallRecords { return &CallRecords{database: database} }
func callRecordID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func callRecordSchema(postgres bool) []string {
	sequence, payload, stamp := "INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT", "JSON", "DATETIME"
	if postgres {
		sequence, payload, stamp = "SERIAL PRIMARY KEY", "JSON", "TIMESTAMP WITH TIME ZONE"
	}
	statements := []string{
		"CREATE TABLE IF NOT EXISTS call_records (sequence " + sequence + ", id VARCHAR(64) NOT NULL UNIQUE, event_time VARCHAR(32) NOT NULL, event_day VARCHAR(10) NOT NULL, type VARCHAR(32) NOT NULL, outcome VARCHAR(32) NOT NULL, endpoint VARCHAR(255) NOT NULL, model VARCHAR(255) NOT NULL, account_email VARCHAR(320) NOT NULL, conversation_id VARCHAR(255) NOT NULL, business_kind VARCHAR(64) NOT NULL, search_text TEXT NOT NULL, payload " + payload + " NOT NULL, inserted_at " + stamp + " NOT NULL)",
		"CREATE TABLE IF NOT EXISTS call_record_state (id INTEGER NOT NULL PRIMARY KEY, generation VARCHAR(64) NOT NULL)",
	}
	for _, index := range []struct{ name, column string }{{"type", "type"}, {"day", "event_day"}, {"time", "event_time"}, {"outcome", "outcome"}, {"endpoint", "endpoint"}, {"model", "model"}, {"account", "account_email"}, {"conversation", "conversation_id"}} {
		statements = append(statements, "CREATE INDEX IF NOT EXISTS ix_call_records_"+index.name+"_sequence ON call_records("+index.column+",sequence)")
	}
	return statements
}

// Append writes the compatible Python payload and query columns in one commit.
// It never changes the generation cursor; only explicit destructive repository
// operations may rotate that cursor in the later management stage.
func (r *CallRecords) Append(ctx context.Context, record CallRecordWrite) (Document, error) {
	payload := Clone(record.Payload)
	clean := func(value any) string { v, _ := value.(string); return strings.TrimSpace(v) }
	id, stamp := clean(payload["id"]), clean(payload["time"])
	if stamp == "" {
		return nil, errors.New("call record time is required")
	}
	var err error
	if id == "" {
		id, err = callRecordID()
		if err != nil {
			return nil, err
		}
	}
	payload["id"] = id
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrInvalid
	}
	day := stamp
	if len(day) > 10 {
		day = day[:10]
	}
	now := time.Now().UTC()
	var inserted any = now
	if !r.database.postgres {
		inserted = now.Format("2006-01-02 15:04:05.000000")
	}
	err = r.database.transaction(ctx, true, "", func(q queryer) error {
		_, err := q.ExecContext(ctx, r.database.bind("INSERT INTO call_records(id,event_time,event_day,type,outcome,endpoint,model,account_email,conversation_id,business_kind,search_text,payload,inserted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)"), id, stamp, day, clean(payload["type"]), strings.TrimSpace(record.Outcome), strings.TrimSpace(record.Endpoint), strings.TrimSpace(record.Model), strings.TrimSpace(record.AccountEmail), strings.TrimSpace(record.ConversationID), strings.TrimSpace(record.BusinessKind), strings.ToLower(strings.TrimSpace(record.SearchText)), string(encoded), inserted)
		return err
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}
