package index

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultSearchLimit is how many hits a search returns.
const DefaultSearchLimit = 20

// Hit is one search result.
type Hit struct {
	Turn Turn
	// Snippet is the matching text with the query terms marked by «».
	Snippet string
}

// Search finds turns whose prompt or answer matches every word in query.
//
// The query is treated as a list of literal words rather than an FTS5
// expression: someone searching their own history is asking a question, not
// writing a query language, and "what's the deploy script?" should find things
// rather than fail to parse.
func (ix *Index) Search(query string, limit int) ([]Hit, error) {
	return ix.search(query, limit, false)
}

// SearchAll is Search, including the agent's own background reviews.
//
// They are excluded by default because someone searching their history is
// looking for their own conversations, and every reviewed turn has a review
// quoting it back — so including them roughly doubles the results while adding
// nothing the original turn did not already say.
func (ix *Index) SearchAll(query string, limit int) ([]Hit, error) {
	return ix.search(query, limit, true)
}

func (ix *Index) search(query string, limit int, includeInternal bool) ([]Hit, error) {
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	match := quoteFTS(query)
	if match == "" {
		return nil, nil
	}

	roleFilter := `AND t.role = ''`
	if includeInternal {
		roleFilter = ``
	}

	rows, err := ix.db.Query(`
		SELECT t.id, t.started_at, t.model, t.prompt, t.answer, t.steps, t.tool_calls,
		       t.input_tokens, t.output_tokens, t.cached_tokens, t.errored, t.sandbox, t.role,
		       snippet(turns_fts, -1, '«', '»', ' … ', 12)
		  FROM turns_fts
		  JOIN turns t ON t.id = turns_fts.id
		 WHERE turns_fts MATCH ? `+roleFilter+`
		 ORDER BY bm25(turns_fts), t.started_at DESC
		 LIMIT ?`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("index: search: %w", err)
	}
	defer rows.Close()

	var hits []Hit
	for rows.Next() {
		var h Hit
		var startedAt int64
		var errored int
		if err := rows.Scan(&h.Turn.ID, &startedAt, &h.Turn.Model, &h.Turn.Prompt, &h.Turn.Answer,
			&h.Turn.Steps, &h.Turn.ToolCalls, &h.Turn.InputTokens, &h.Turn.OutputTokens,
			&h.Turn.CachedTokens, &errored, &h.Turn.Sandbox, &h.Turn.Role, &h.Snippet); err != nil {
			return nil, fmt.Errorf("index: read hit: %w", err)
		}
		h.Turn.StartedAt = time.UnixMilli(startedAt).UTC()
		h.Turn.Errored = errored != 0
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// ModelUsage is what one model consumed.
type ModelUsage struct {
	Model        string
	Turns        int
	InputTokens  int
	OutputTokens int
	CachedTokens int
}

// Total returns the model's token total. Cached input is part of input, not an
// addition to it, so it is not counted again here.
func (m ModelUsage) Total() int { return m.InputTokens + m.OutputTokens }

// UsageReport summarises consumption over a period.
type UsageReport struct {
	Since time.Time
	// Models is sorted by total tokens, largest first.
	Models []ModelUsage
	// Turns, Errored and Reviews count the turns behind the totals. Reviews
	// are separated because a background review is nemuz spending tokens on
	// its own behalf, not work the user asked for.
	Turns   int
	Errored int
	Reviews int
	// Tools is how often each tool ran, most used first.
	Tools []ToolUsage
}

// ToolUsage is how often one tool ran.
type ToolUsage struct {
	Name   string
	Calls  int
	Failed int
}

// Totals adds up every model.
func (r UsageReport) Totals() (in, out, cached int) {
	for _, m := range r.Models {
		in += m.InputTokens
		out += m.OutputTokens
		cached += m.CachedTokens
	}
	return in, out, cached
}

// Usage reports what has been consumed since a time. A zero time covers
// everything.
//
// It reports tokens and nothing else. Turning tokens into money needs a price
// list, and nemuz does not ship one: prices change without notice, and a
// confident figure computed from a stale table is worse than no figure at all.
func (ix *Index) Usage(since time.Time) (UsageReport, error) {
	report := UsageReport{Since: since}
	cutoff := int64(0)
	if !since.IsZero() {
		cutoff = since.UTC().UnixMilli()
	}

	if err := ix.db.QueryRow(`
		SELECT count(*),
		       coalesce(sum(errored), 0),
		       coalesce(sum(CASE WHEN role = 'review' THEN 1 ELSE 0 END), 0)
		  FROM turns WHERE started_at >= ?`, cutoff,
	).Scan(&report.Turns, &report.Errored, &report.Reviews); err != nil {
		return report, fmt.Errorf("index: count turns: %w", err)
	}

	models, err := ix.db.Query(`
		SELECT coalesce(nullif(model, ''), '(unknown)') AS m,
		       count(*), sum(input_tokens), sum(output_tokens), sum(cached_tokens)
		  FROM turns WHERE started_at >= ?
		 GROUP BY m`, cutoff)
	if err != nil {
		return report, fmt.Errorf("index: usage by model: %w", err)
	}
	defer models.Close()

	for models.Next() {
		var m ModelUsage
		if err := models.Scan(&m.Model, &m.Turns, &m.InputTokens, &m.OutputTokens, &m.CachedTokens); err != nil {
			return report, fmt.Errorf("index: read model usage: %w", err)
		}
		report.Models = append(report.Models, m)
	}
	if err := models.Err(); err != nil {
		return report, err
	}
	sort.Slice(report.Models, func(i, j int) bool {
		if report.Models[i].Total() != report.Models[j].Total() {
			return report.Models[i].Total() > report.Models[j].Total()
		}
		return report.Models[i].Model < report.Models[j].Model
	})

	tools, err := ix.db.Query(`
		SELECT c.name, count(*), coalesce(sum(c.is_error), 0)
		  FROM tool_calls c JOIN turns t ON t.id = c.turn_id
		 WHERE t.started_at >= ?
		 GROUP BY c.name`, cutoff)
	if err != nil {
		return report, fmt.Errorf("index: usage by tool: %w", err)
	}
	defer tools.Close()

	for tools.Next() {
		var t ToolUsage
		if err := tools.Scan(&t.Name, &t.Calls, &t.Failed); err != nil {
			return report, fmt.Errorf("index: read tool usage: %w", err)
		}
		report.Tools = append(report.Tools, t)
	}
	if err := tools.Err(); err != nil {
		return report, err
	}
	sort.Slice(report.Tools, func(i, j int) bool {
		if report.Tools[i].Calls != report.Tools[j].Calls {
			return report.Tools[i].Calls > report.Tools[j].Calls
		}
		return report.Tools[i].Name < report.Tools[j].Name
	})

	return report, nil
}

// Recent returns the most recently started turns.
func (ix *Index) Recent(limit int) ([]Turn, error) {
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	rows, err := ix.db.Query(`
		SELECT id, started_at, model, prompt, answer, steps, tool_calls,
		       input_tokens, output_tokens, cached_tokens, errored, sandbox, role
		  FROM turns ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("index: recent: %w", err)
	}
	defer rows.Close()
	return scanTurns(rows)
}

func scanTurns(rows *sql.Rows) ([]Turn, error) {
	var out []Turn
	for rows.Next() {
		var t Turn
		var startedAt int64
		var errored int
		if err := rows.Scan(&t.ID, &startedAt, &t.Model, &t.Prompt, &t.Answer, &t.Steps,
			&t.ToolCalls, &t.InputTokens, &t.OutputTokens, &t.CachedTokens,
			&errored, &t.Sandbox, &t.Role); err != nil {
			return nil, fmt.Errorf("index: read turn: %w", err)
		}
		t.StartedAt = time.UnixMilli(startedAt).UTC()
		t.Errored = errored != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// OneLine renders a turn's prompt for a list, collapsed to a single line.
func (t Turn) OneLine(limit int) string {
	text := strings.Join(strings.Fields(t.Prompt), " ")
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
