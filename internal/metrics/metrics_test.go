package metrics

import (
	"strings"
	"testing"
	"time"
)

// The exposition format is a contract with whatever scrapes it, so these tests
// assert on the actual bytes rather than on internal counters.

func fixed(t *testing.T) *Metrics {
	t.Helper()
	m := New()
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	var calls int
	m.SetClock(func() time.Time {
		calls++
		// The first call sets the start; later ones are 90 seconds on.
		if calls == 1 {
			return base
		}
		return base.Add(90 * time.Second)
	})
	return m
}

func lines(out string) map[string]string {
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.LastIndex(line, " "); i > 0 {
			got[line[:i]] = line[i+1:]
		}
	}
	return got
}

func TestRenderedOutputIsValidExposition(t *testing.T) {
	m := fixed(t)
	m.RecordTurn(OutcomeOK, 3*time.Second, 2, 1, 100, 20, 64)
	m.RecordRequest(200)

	out := m.Render()

	// Every metric needs its HELP and TYPE before any sample.
	for _, name := range []string{
		"nemuz_turns_total", "nemuz_turn_steps_total", "nemuz_tool_calls_total",
		"nemuz_tokens_total", "nemuz_turn_duration_seconds",
		"nemuz_http_responses_total", "nemuz_uptime_seconds",
	} {
		if !strings.Contains(out, "# HELP "+name+" ") {
			t.Errorf("%s has no HELP line", name)
		}
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("%s has no TYPE line", name)
		}
	}

	// Samples must be name-then-value, one per line, with nothing else.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Count(line, " ") == 0 {
			t.Errorf("sample line has no value: %q", line)
		}
	}
}

func TestTurnsAreCountedByOutcome(t *testing.T) {
	m := fixed(t)
	m.RecordTurn(OutcomeOK, time.Second, 1, 0, 10, 2, 0)
	m.RecordTurn(OutcomeOK, time.Second, 1, 0, 10, 2, 0)
	m.RecordTurn(OutcomeError, time.Second, 1, 0, 10, 0, 0)

	got := lines(m.Render())
	if got[`nemuz_turns_total{outcome="ok"}`] != "2" {
		t.Errorf("ok turns are %q", got[`nemuz_turns_total{outcome="ok"}`])
	}
	if got[`nemuz_turns_total{outcome="error"}`] != "1" {
		t.Errorf("error turns are %q", got[`nemuz_turns_total{outcome="error"}`])
	}
}

func TestTokensAreReportedByDirection(t *testing.T) {
	m := fixed(t)
	m.RecordTurn(OutcomeOK, time.Second, 1, 2, 500, 60, 320)
	m.RecordTurn(OutcomeOK, time.Second, 1, 1, 200, 40, 0)

	got := lines(m.Render())
	for label, want := range map[string]string{"input": "700", "output": "100", "cached": "320"} {
		key := `nemuz_tokens_total{kind="` + label + `"}`
		if got[key] != want {
			t.Errorf("%s is %q, want %s", key, got[key], want)
		}
	}
	if got["nemuz_tool_calls_total"] != "3" {
		t.Errorf("tool calls are %q", got["nemuz_tool_calls_total"])
	}
}

// TestHistogramBucketsAreCumulative is the part of the format that is easy to
// get subtly wrong: a Prometheus bucket counts everything at or below its
// boundary, not everything that fell inside it.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	m := fixed(t)
	for _, d := range []time.Duration{300 * time.Millisecond, 3 * time.Second, 45 * time.Second} {
		m.RecordTurn(OutcomeOK, d, 1, 0, 0, 0, 0)
	}

	got := lines(m.Render())
	for le, want := range map[string]string{
		"0.5": "1", // just the 300ms turn
		"1":   "1",
		"2":   "1",
		"5":   "2", // plus the 3s turn
		"30":  "2",
		"60":  "3", // plus the 45s turn
	} {
		key := `nemuz_turn_duration_seconds_bucket{le="` + le + `"}`
		if got[key] != want {
			t.Errorf("bucket le=%s is %q, want %s", le, got[key], want)
		}
	}
	if got[`nemuz_turn_duration_seconds_bucket{le="+Inf"}`] != "3" {
		t.Error("the +Inf bucket must hold every observation")
	}
	if got["nemuz_turn_duration_seconds_count"] != "3" {
		t.Errorf("count is %q", got["nemuz_turn_duration_seconds_count"])
	}
	if got["nemuz_turn_duration_seconds_sum"] != "48.3" {
		t.Errorf("sum is %q, want 48.3", got["nemuz_turn_duration_seconds_sum"])
	}
}

// TestUnobservedLabelsAreAbsent guards a real misreading: a counter emitted as
// zero for a label that never occurred looks like something happened and
// measured nothing.
func TestUnobservedLabelsAreAbsent(t *testing.T) {
	m := fixed(t)
	m.RecordTurn(OutcomeOK, time.Second, 1, 0, 0, 0, 0)

	out := m.Render()
	if strings.Contains(out, `outcome="error"`) {
		t.Error("an outcome that never happened was reported as zero")
	}
	if strings.Contains(out, "nemuz_http_responses_total{") {
		t.Error("HTTP responses were reported before any were served")
	}
}

func TestUptimeIsReported(t *testing.T) {
	m := fixed(t)
	got := lines(m.Render())
	if got["nemuz_uptime_seconds"] != "90" {
		t.Errorf("uptime is %q, want 90", got["nemuz_uptime_seconds"])
	}
}

func TestNumbersAvoidScientificNotation(t *testing.T) {
	m := fixed(t)
	// A tiny duration would render as 1e-06 with %v, which some scrapers
	// accept and some do not.
	m.RecordTurn(OutcomeOK, time.Microsecond, 1, 0, 0, 0, 0)

	out := m.Render()
	if strings.Contains(out, "e-") || strings.Contains(out, "e+") {
		t.Errorf("output uses scientific notation:\n%s", out)
	}
}

func TestConcurrentRecordingIsSafe(t *testing.T) {
	m := New()
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 200; j++ {
				m.RecordTurn(OutcomeOK, time.Second, 1, 1, 1, 1, 0)
				m.RecordRequest(200)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}

	got := lines(m.Render())
	if got[`nemuz_turns_total{outcome="ok"}`] != "1600" {
		t.Errorf("turns are %q, want 1600", got[`nemuz_turns_total{outcome="ok"}`])
	}
}
