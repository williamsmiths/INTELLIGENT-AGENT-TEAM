package hookinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
)

// FailOpen is one line of the local hook log: harnessd did not answer the hook
// client. For an enrolled workspace a pre-tool is refused (Closed, AC-S2-005);
// every other event is let through (limitation L3, AC-H-071).
type FailOpen struct {
	At     time.Time `json:"at"` // RFC 3339
	Agent  string    `json:"agent"`
	Event  string    `json:"event"`            // prompt-submit, session-start, pre-tool, post-tool or stop
	Closed bool      `json:"closed,omitempty"` // the tool call was refused instead of let through
}

const (
	// maxIngestBytes bounds what one IngestFailOpen call reads; the rest waits for the next call.
	maxIngestBytes = 1 << 20
	maxAgentLen    = 128
)

// AppendFailOpen is the client side: it adds one JSON line to the log,
// creating the file (0600) and its parent directories when needed.
func AppendFailOpen(path string, f FailOpen) error {
	if !f.valid() {
		return fmt.Errorf("hookinstall: fail-open record needs a time, an agent and a known hook event")
	}
	f.At = f.At.UTC()
	line, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("hookinstall: encode fail-open record: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("hookinstall: fail-open log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("hookinstall: open fail-open log: %w", err)
	}
	// One write per line: O_APPEND keeps concurrent hook clients from interleaving.
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("hookinstall: append fail-open log: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("hookinstall: close fail-open log: %w", err)
	}
	return nil
}

// ReadFailOpenLog returns every well-formed record of the log. A missing file
// is an empty log. Lines that are not a valid record, and a last line without
// its newline yet, are skipped: the file is writable by the agent and must not
// be able to stop the reader.
func ReadFailOpenLog(path string) ([]FailOpen, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []FailOpen{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hookinstall: read fail-open log: %w", err)
	}
	records, _ := parse(data)
	return records, nil
}

// IngestFailOpen is the server side of AC-H-071: it journals one HOOK_FAILOPEN
// event (payload: agent, hook event, time) for every record appended since the
// last call and returns how many it journaled. The byte offset it reached is
// kept in tail_cursor under a name derived from the path, so a line is never
// ingested twice; a log that shrank is read again from the start. The
// transaction must run under store.Runtime.
func IngestFailOpen(ctx context.Context, tx pgx.Tx, path string) (int, error) {
	name := "hook_failopen:" + filepath.Clean(path)
	// Create the cursor first so two concurrent ingests queue on its row lock.
	if _, err := tx.Exec(ctx, `INSERT INTO tail_cursor (name, value) VALUES ($1, '0') ON CONFLICT (name) DO NOTHING`, name); err != nil {
		return 0, fmt.Errorf("hookinstall: fail-open cursor: %w", err)
	}
	var value string
	if err := tx.QueryRow(ctx, `SELECT value FROM tail_cursor WHERE name = $1 FOR UPDATE`, name).Scan(&value); err != nil {
		return 0, fmt.Errorf("hookinstall: fail-open cursor: %w", err)
	}
	offset, err := strconv.ParseInt(value, 10, 64)
	if err != nil || offset < 0 {
		offset = 0
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("hookinstall: open fail-open log: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("hookinstall: stat fail-open log: %w", err)
	}
	if info.Size() < offset {
		offset = 0
	}
	chunk, err := io.ReadAll(io.NewSectionReader(file, offset, maxIngestBytes))
	if err != nil {
		return 0, fmt.Errorf("hookinstall: read fail-open log: %w", err)
	}
	records, used := parse(chunk)
	if used == 0 && len(chunk) == maxIngestBytes {
		used = len(chunk) // one line longer than the limit: skip it instead of stalling forever
	}
	for _, f := range records {
		typ := EventFailOpen
		if f.Closed {
			typ = EventFailClosed
		}
		_, err := journal.Append(ctx, tx, journal.Event{
			Type:    typ,
			Payload: map[string]any{"agent": f.Agent, "event": f.Event, "at": f.At.UTC().Format(time.RFC3339Nano)},
		})
		if err != nil {
			return 0, fmt.Errorf("hookinstall: journal %s: %w", typ, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE tail_cursor SET value = $2, updated_at = now() WHERE name = $1`,
		name, strconv.FormatInt(offset+int64(used), 10)); err != nil {
		return 0, fmt.Errorf("hookinstall: fail-open cursor: %w", err)
	}
	return len(records), nil
}

// parse reads complete lines (ending in a newline) and returns the valid
// records and how many bytes those complete lines cover. A last line that is
// still being written is left for the next read.
func parse(data []byte) (records []FailOpen, used int) {
	records = []FailOpen{}
	for {
		end := bytes.IndexByte(data[used:], '\n')
		if end < 0 {
			return records, used
		}
		line := data[used : used+end]
		used += end + 1
		var f FailOpen
		if json.Unmarshal(line, &f) == nil && f.valid() {
			records = append(records, f)
		}
	}
}

// valid keeps the journal payload to an id, a known event code and a time (PM-H-20).
func (f FailOpen) valid() bool {
	if f.At.IsZero() || f.Agent == "" || len(f.Agent) > maxAgentLen || hasControl(f.Agent) {
		return false
	}
	for _, e := range events {
		if f.Event == e.Arg {
			return true
		}
	}
	return false
}
