// Package index builds a searchable view of the journal.
//
// # The index is derived, never authoritative
//
// Everything here can be reconstructed from the journal, and nothing is stored
// here that is not already recorded there. That is what makes the answer to a
// corrupt or missing database simply "delete it and rebuild" — no backup, no
// migration, no data loss. It also means the index can gain columns whenever
// there is a reason to, because filling them in is a rebuild rather than a
// schema migration over data nobody can recover.
//
// SQLite arrives through modernc.org/sqlite, which is a pure-Go translation
// rather than a binding, so nemuz keeps its cgo-free static binary. It happens
// to ship SQLite 3.53.4 with FTS5 — the same version Hermes Agent has to
// compile from source at image build time to get.
package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/journal"

	_ "modernc.org/sqlite"
)

// schemaVersion is bumped whenever the tables change. Because the index is
// derived, an older version is dropped and rebuilt rather than migrated.
const schemaVersion = 1

// Index is a searchable view of recorded turns.
type Index struct {
	db   *sql.DB
	path string
}

// Open prepares the index at path, creating or rebuilding the schema as needed.
func Open(path string) (*Index, error) {
	if path == "" {
		return nil, errors.New("index: empty database path")
	}
	// WAL keeps a reader from blocking the writer, which matters because a
	// search may run while a turn is being recorded.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("index: open %s: %w", path, err)
	}

	ix := &Index{db: db, path: path}
	if err := ix.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return ix, nil
}

// Close releases the database.
func (ix *Index) Close() error { return ix.db.Close() }

// Path returns the database file's location.
func (ix *Index) Path() string { return ix.path }

// migrate creates the schema, discarding an older one.
func (ix *Index) migrate() error {
	var version int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("index: read schema version: %w", err)
	}
	if version == schemaVersion {
		return nil
	}

	// Any version that is not the current one is dropped rather than migrated.
	// An index holds nothing the journal does not, so this cannot lose data —
	// and it covers version 0, which means either a fresh file or one written
	// before the schema was versioned at all. Dropping tables that do not
	// exist costs nothing, so the two cases need no distinguishing.
	for _, table := range []string{"turns_fts", "tool_calls", "turns"} {
		if _, err := ix.db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			return fmt.Errorf("index: drop %s: %w", table, err)
		}
	}

	if _, err := ix.db.Exec(schema); err != nil {
		return fmt.Errorf("index: create schema: %w", err)
	}
	if _, err := ix.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("index: set schema version: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS turns (
    id            TEXT PRIMARY KEY,
    started_at    INTEGER NOT NULL,
    model         TEXT    NOT NULL DEFAULT '',
    prompt        TEXT    NOT NULL DEFAULT '',
    answer        TEXT    NOT NULL DEFAULT '',
    steps         INTEGER NOT NULL DEFAULT 0,
    tool_calls    INTEGER NOT NULL DEFAULT 0,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    errored       INTEGER NOT NULL DEFAULT 0,
    sandbox       TEXT    NOT NULL DEFAULT '',
    role          TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS turns_started_at ON turns(started_at);
CREATE INDEX IF NOT EXISTS turns_model      ON turns(model);

CREATE TABLE IF NOT EXISTS tool_calls (
    turn_id  TEXT    NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
    name     TEXT    NOT NULL,
    is_error INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS tool_calls_turn ON tool_calls(turn_id);
CREATE INDEX IF NOT EXISTS tool_calls_name ON tool_calls(name);

-- A standalone FTS table rather than an external-content one. External content
-- avoids storing the text twice, at the cost of triggers that must stay in step
-- with every write. Since this whole database is disposable, the simpler and
-- harder-to-break option wins.
CREATE VIRTUAL TABLE IF NOT EXISTS turns_fts USING fts5(
    id UNINDEXED,
    prompt,
    answer,
    tokenize = 'unicode61'
);
`

// Turn is one indexed turn.
type Turn struct {
	ID           string
	StartedAt    time.Time
	Model        string
	Prompt       string
	Answer       string
	Steps        int
	ToolCalls    int
	InputTokens  int
	OutputTokens int
	CachedTokens int
	Errored      bool
	Sandbox      string
	// Role is empty for a conversation turn, "review" for a background review.
	Role string
}

// Ingest adds or replaces one turn.
func (ix *Index) Ingest(t Turn, tools []ToolCall) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return fmt.Errorf("index: begin: %w", err)
	}
	defer tx.Rollback()

	if err := insertTurn(tx, t, tools); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("index: commit: %w", err)
	}
	return nil
}

// ToolCall is one tool invocation within a turn.
type ToolCall struct {
	Name    string
	IsError bool
}

func insertTurn(tx *sql.Tx, t Turn, tools []ToolCall) error {
	// Re-indexing a turn must replace it rather than duplicate it, which is
	// what makes a rebuild safe to run at any time.
	for _, stmt := range []string{
		`DELETE FROM tool_calls WHERE turn_id = ?`,
		`DELETE FROM turns_fts WHERE id = ?`,
		`DELETE FROM turns WHERE id = ?`,
	} {
		if _, err := tx.Exec(stmt, t.ID); err != nil {
			return fmt.Errorf("index: clear %s: %w", t.ID, err)
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO turns (id, started_at, model, prompt, answer, steps, tool_calls,
		                   input_tokens, output_tokens, cached_tokens, errored, sandbox, role)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.StartedAt.UTC().UnixMilli(), t.Model, t.Prompt, t.Answer,
		t.Steps, t.ToolCalls, t.InputTokens, t.OutputTokens, t.CachedTokens,
		boolToInt(t.Errored), t.Sandbox, t.Role,
	); err != nil {
		return fmt.Errorf("index: insert turn %s: %w", t.ID, err)
	}

	if _, err := tx.Exec(`INSERT INTO turns_fts (id, prompt, answer) VALUES (?,?,?)`,
		t.ID, t.Prompt, t.Answer); err != nil {
		return fmt.Errorf("index: index text for %s: %w", t.ID, err)
	}

	for _, call := range tools {
		if _, err := tx.Exec(`INSERT INTO tool_calls (turn_id, name, is_error) VALUES (?,?,?)`,
			t.ID, call.Name, boolToInt(call.IsError)); err != nil {
			return fmt.Errorf("index: insert tool call for %s: %w", t.ID, err)
		}
	}
	return nil
}

// Count returns how many turns are indexed.
func (ix *Index) Count() (int, error) {
	var n int
	if err := ix.db.QueryRow(`SELECT count(*) FROM turns`).Scan(&n); err != nil {
		return 0, fmt.Errorf("index: count: %w", err)
	}
	return n, nil
}

// Has reports whether a turn is already indexed.
func (ix *Index) Has(turnID string) (bool, error) {
	var n int
	if err := ix.db.QueryRow(`SELECT count(*) FROM turns WHERE id = ?`, turnID).Scan(&n); err != nil {
		return false, fmt.Errorf("index: lookup %s: %w", turnID, err)
	}
	return n > 0, nil
}

// IngestJournal reads one recorded turn and indexes it.
func (ix *Index) IngestJournal(path string, bs *blob.Store) error {
	turn, tools, err := ReadTurn(path, bs)
	if err != nil {
		return err
	}
	return ix.Ingest(turn, tools)
}

// Rebuild indexes every turn in a journal directory, replacing what is there.
//
// This is always safe: the journal is the source of truth, and the index holds
// nothing that is not derived from it.
func (ix *Index) Rebuild(ctx context.Context, journalDir string, bs *blob.Store) (indexed int, skipped []string, err error) {
	turns, err := journal.List(journalDir)
	if err != nil {
		return 0, nil, err
	}

	tx, err := ix.db.Begin()
	if err != nil {
		return 0, nil, fmt.Errorf("index: begin rebuild: %w", err)
	}
	defer tx.Rollback()

	for _, stmt := range []string{`DELETE FROM tool_calls`, `DELETE FROM turns_fts`, `DELETE FROM turns`} {
		if _, err := tx.Exec(stmt); err != nil {
			return 0, nil, fmt.Errorf("index: clear for rebuild: %w", err)
		}
	}

	for _, t := range turns {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		turn, tools, err := ReadTurn(t.Path, bs)
		if err != nil {
			// One unreadable journal must not stop the rebuild. It is named
			// in the result so the operator can look at it.
			skipped = append(skipped, fmt.Sprintf("%s: %v", t.ID, err))
			continue
		}
		if err := insertTurn(tx, turn, tools); err != nil {
			return 0, nil, err
		}
		indexed++
	}

	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("index: commit rebuild: %w", err)
	}
	return indexed, skipped, nil
}

// ReadTurn extracts the indexable facts from one recorded turn.
func ReadTurn(path string, bs *blob.Store) (Turn, []ToolCall, error) {
	events, err := journal.Read(path)
	if err != nil {
		return Turn{}, nil, err
	}
	if len(events) == 0 {
		return Turn{}, nil, errors.New("the journal is empty")
	}

	turn := Turn{ID: events[0].TurnID, StartedAt: events[0].TS}
	var tools []ToolCall

	for _, e := range events {
		body, err := e.Content(bs)
		if err != nil {
			// A missing blob costs one field, not the whole turn.
			continue
		}
		switch e.Kind {
		case journal.KindTurnStart:
			var start struct {
				Prompt string            `json:"prompt"`
				Model  string            `json:"model"`
				Env    map[string]string `json:"env"`
			}
			if json.Unmarshal(body, &start) == nil {
				turn.Prompt, turn.Model = start.Prompt, start.Model
				turn.Sandbox, turn.Role = start.Env["sandbox"], start.Env["role"]
			}
		case journal.KindModelResponse:
			var resp struct {
				Text  string `json:"text"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					CachedTokens int `json:"cached_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(body, &resp) == nil {
				turn.InputTokens += resp.Usage.InputTokens
				turn.OutputTokens += resp.Usage.OutputTokens
				turn.CachedTokens += resp.Usage.CachedTokens
				if resp.Text != "" {
					// The last non-empty response is the answer; earlier ones
					// were the model thinking out loud between tool calls.
					turn.Answer = resp.Text
				}
			}
		case journal.KindToolCall:
			var call struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(body, &call) == nil && call.Name != "" {
				tools = append(tools, ToolCall{Name: call.Name})
			}
		case journal.KindToolResult:
			var result struct {
				Name    string `json:"name"`
				IsError bool   `json:"is_error"`
			}
			if json.Unmarshal(body, &result) == nil && result.IsError {
				for i := range tools {
					if tools[i].Name == result.Name {
						tools[i].IsError = true
					}
				}
			}
		case journal.KindError:
			turn.Errored = true
		case journal.KindTurnEnd:
			var end struct {
				Steps     int `json:"steps"`
				ToolCalls int `json:"tool_calls"`
			}
			if json.Unmarshal(body, &end) == nil {
				turn.Steps, turn.ToolCalls = end.Steps, end.ToolCalls
			}
		}
	}
	return turn, tools, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Remove deletes the index file, so the next Open rebuilds from nothing.
func Remove(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("index: remove %s: %w", path+suffix, err)
		}
	}
	return nil
}

// quoteFTS makes an operator's words safe to hand to FTS5.
//
// FTS5 has its own query language — NEAR, wildcards, boolean operators, column
// filters — and a bare apostrophe or hyphen from a normal question is a syntax
// error in it. Every word is therefore quoted as a literal term, which turns
// the whole query into "all of these words" and can never fail to parse.
func quoteFTS(query string) string {
	fields := strings.Fields(query)
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " ")
}
