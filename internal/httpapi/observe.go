package httpapi

import (
	"net/http"
	"time"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/metrics"
)

// recorder captures the status a handler wrote, so it can be counted.
//
// It forwards Flush, because the streaming path needs it and a wrapper that
// quietly dropped it would turn every streamed answer into one buffered blob
// delivered at the end.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// observed counts the responses a handler produces.
func (s *Server) observed(next http.Handler) http.Handler {
	if s.opts.Metrics == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.opts.Metrics.RecordRequest(rec.status)
	})
}

// recordTurn notes one finished turn.
func (s *Server) recordTurn(out agent.Outcome, took time.Duration, err error) {
	if s.opts.Metrics == nil {
		return
	}
	outcome := metrics.OutcomeOK
	if err != nil {
		outcome = metrics.OutcomeError
	}
	s.opts.Metrics.RecordTurn(outcome, took,
		out.Steps, out.ToolCalls,
		out.Usage.InputTokens, out.Usage.OutputTokens, out.Usage.CachedTokens)
}

// serveMetrics renders the Prometheus exposition.
//
// It needs no API key, for the same reason /health does not: a scraper is
// usually a sidecar with no credentials, and what is exposed here is counts and
// durations — never a prompt, an answer, or a key. If those counts are
// sensitive in your deployment, do not publish the port.
func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	if s.opts.Metrics == nil {
		http.Error(w, "metrics are not enabled on this server", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(s.opts.Metrics.Render()))
}
