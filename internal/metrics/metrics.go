// Package metrics records what nemuz is doing and renders it in the Prometheus
// text format.
//
// # Why there is no metrics library here
//
// The Prometheus exposition format is a few lines of text, and nemuz publishes
// about a dozen numbers. Pulling in a client library for that would add several
// megabytes and a tree of dependencies to a binary whose whole argument is that
// it is one small file. The format is written out below instead, and checked by
// tests that assert on the bytes.
//
// The cost of that choice is real: no exemplars, no native histograms, no
// registry other people's libraries can join. If nemuz ever needs those, the
// right move is to adopt the client library then, not to half-build one now.
package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// durationBuckets are the histogram boundaries, in seconds.
//
// They are spread wide because agent turns are: a cached one-word answer lands
// under a second, and a turn that runs a dozen tools can take minutes. Buckets
// clustered around a typical web request would put everything in +Inf.
var durationBuckets = []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600}

// Metrics is nemuz's measurements.
//
// It is a concrete type rather than a general registry: nemuz publishes a fixed
// set of numbers, and naming them makes each one's meaning obvious at the call
// site and impossible to typo.
type Metrics struct {
	mu sync.Mutex

	turns      map[string]uint64 // keyed by outcome
	toolCalls  uint64
	steps      uint64
	tokensIn   uint64
	tokensOut  uint64
	tokensCach uint64

	requests map[string]uint64 // keyed by HTTP status
	duration histogram

	started time.Time
	now     func() time.Time
}

// Outcomes a turn can have.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
)

// New returns an empty set of metrics.
func New() *Metrics {
	m := &Metrics{
		turns:    map[string]uint64{},
		requests: map[string]uint64{},
		duration: newHistogram(durationBuckets),
		now:      time.Now,
	}
	m.started = m.now()
	return m
}

// SetClock replaces the time source, so tests get a stable uptime.
func (m *Metrics) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
	m.started = now()
}

// RecordTurn notes one finished turn.
func (m *Metrics) RecordTurn(outcome string, took time.Duration, steps, toolCalls, in, out, cached int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.turns[outcome]++
	m.duration.observe(took.Seconds())
	m.steps += uint64(max(steps, 0))
	m.toolCalls += uint64(max(toolCalls, 0))
	m.tokensIn += uint64(max(in, 0))
	m.tokensOut += uint64(max(out, 0))
	m.tokensCach += uint64(max(cached, 0))
}

// RecordRequest notes one HTTP response.
func (m *Metrics) RecordRequest(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[strconv.Itoa(status)]++
}

// Render writes the metrics in the Prometheus text exposition format.
func (m *Metrics) Render() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder

	metric(&b, "nemuz_turns_total", "counter",
		"Turns that finished, by outcome.")
	writeLabelled(&b, "nemuz_turns_total", "outcome", m.turns)

	metric(&b, "nemuz_turn_steps_total", "counter",
		"Model rounds across all turns. Rising faster than turns means the agent is looping.")
	fmt.Fprintf(&b, "nemuz_turn_steps_total %d\n", m.steps)

	metric(&b, "nemuz_tool_calls_total", "counter",
		"Tools invoked across all turns.")
	fmt.Fprintf(&b, "nemuz_tool_calls_total %d\n", m.toolCalls)

	metric(&b, "nemuz_tokens_total", "counter",
		"Tokens consumed, by direction. Cached input is reported separately and is a subset of input.")
	writeLabelled(&b, "nemuz_tokens_total", "kind", map[string]uint64{
		"input": m.tokensIn, "output": m.tokensOut, "cached": m.tokensCach,
	})

	metric(&b, "nemuz_turn_duration_seconds", "histogram",
		"How long turns take, end to end.")
	m.duration.render(&b, "nemuz_turn_duration_seconds")

	metric(&b, "nemuz_http_responses_total", "counter",
		"HTTP responses served, by status code.")
	writeLabelled(&b, "nemuz_http_responses_total", "status", m.requests)

	metric(&b, "nemuz_uptime_seconds", "gauge",
		"How long this process has been running.")
	fmt.Fprintf(&b, "nemuz_uptime_seconds %s\n", formatFloat(m.now().Sub(m.started).Seconds()))

	return b.String()
}

// metric writes the HELP and TYPE lines a scraper expects before samples.
func metric(b *strings.Builder, name, kind, help string) {
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

// writeLabelled emits one sample per label value, sorted so a scrape is stable.
//
// A metric with no observations still emits nothing rather than a zero for an
// invented label: a counter that has never fired should be absent, not zero for
// a value that never happened.
func writeLabelled(b *strings.Builder, name, label string, values map[string]uint64) {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, k, values[k])
	}
}

// histogram is a bucketed distribution.
type histogram struct {
	bounds []float64
	counts []uint64
	sum    float64
	total  uint64
}

func newHistogram(bounds []float64) histogram {
	return histogram{bounds: bounds, counts: make([]uint64, len(bounds))}
}

func (h *histogram) observe(v float64) {
	h.sum += v
	h.total++
	for i, upper := range h.bounds {
		if v <= upper {
			h.counts[i]++
		}
	}
}

// render writes the cumulative buckets Prometheus expects.
func (h *histogram) render(b *strings.Builder, name string) {
	for i, upper := range h.bounds {
		fmt.Fprintf(b, "%s_bucket{le=%q} %d\n", name, formatFloat(upper), h.counts[i])
	}
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", name, h.total)
	fmt.Fprintf(b, "%s_sum %s\n", name, formatFloat(h.sum))
	fmt.Fprintf(b, "%s_count %d\n", name, h.total)
}

// formatFloat renders a number the way Prometheus reads it back: no exponent
// for ordinary values, and no trailing zeros.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
