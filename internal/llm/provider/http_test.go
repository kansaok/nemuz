package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/llm"
)

// Every test here runs against httptest. Nothing in this package's test suite
// touches the network, which is what lets CI block egress and still prove the
// adapters work.

// countingServer replies with a fixed sequence of statuses, then success.
func countingServer(t *testing.T, statuses []int, success string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1))
		if n <= len(statuses) {
			w.WriteHeader(statuses[n-1])
			_, _ = w.Write([]byte(`{"error":{"message":"slow down","code":"rate_limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(success))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const okCompletion = `{"model":"m","choices":[{"message":{"content":"hai"},"finish_reason":"stop"}],
	"usage":{"prompt_tokens":10,"completion_tokens":2}}`

func fastConfig(base string) Config {
	return Config{
		BaseURL:     base,
		APIKey:      "test-key",
		Model:       "m",
		BackoffBase: time.Millisecond,
	}
}

func TestRetriesRateLimitsAndServerFaults(t *testing.T) {
	srv, hits := countingServer(t, []int{429, 503}, okCompletion)
	p := NewOpenAI("test", fastConfig(srv.URL))

	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "halo"}},
	})
	if err != nil {
		t.Fatalf("a call that eventually succeeded returned an error: %v", err)
	}
	if resp.Text != "hai" {
		t.Errorf("got %q", resp.Text)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server saw %d attempts, want 3 (two failures then success)", got)
	}
}

// TestDoesNotRetryClientErrors matters for cost: resending a malformed request
// burns quota and time without any chance of a different answer.
func TestDoesNotRetryClientErrors(t *testing.T) {
	srv, hits := countingServer(t, []int{400, 400, 400, 400}, okCompletion)
	p := NewOpenAI("test", fastConfig(srv.URL))

	_, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "halo"}},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != 400 {
		t.Errorf("status is %d, want 400", apiErr.Status)
	}
	if apiErr.Retryable() {
		t.Error("a 400 reported itself as retryable")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server saw %d attempts, want exactly 1", got)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	srv, hits := countingServer(t, []int{500, 500, 500, 500, 500}, okCompletion)
	cfg := fastConfig(srv.URL)
	cfg.MaxAttempts = 3
	p := NewOpenAI("test", cfg)

	_, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "halo"}},
	})
	if err == nil {
		t.Fatal("a persistently failing provider reported success")
	}
	if !strings.Contains(err.Error(), "gave up after 3 attempts") {
		t.Errorf("the error should say how many attempts were made, got: %v", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server saw %d attempts, want 3", got)
	}
}

func TestErrorBodyIsSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","code":"invalid_api_key"}}`))
	}))
	defer srv.Close()

	_, err := NewOpenAI("test", fastConfig(srv.URL)).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "halo"}},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T", err)
	}
	if apiErr.Code != "invalid_api_key" {
		t.Errorf("code is %q, want invalid_api_key", apiErr.Code)
	}
	if !strings.Contains(apiErr.Message, "Incorrect API key") {
		t.Errorf("message is %q; the provider's own wording should survive", apiErr.Message)
	}
}

func TestRetryAfterHeaderIsHonoured(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(okCompletion))
	}))
	defer srv.Close()

	start := time.Now()
	if _, err := NewOpenAI("test", fastConfig(srv.URL)).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "halo"}},
	}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("waited %v; Retry-After asked for 1s and the provider knows best", elapsed)
	}
}

func TestContextCancellationStopsRetrying(t *testing.T) {
	srv, _ := countingServer(t, []int{503, 503, 503, 503}, okCompletion)
	cfg := fastConfig(srv.URL)
	cfg.BackoffBase = 2 * time.Second
	p := NewOpenAI("test", cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := p.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: "halo"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context deadline to surface, got %v", err)
	}
}

func TestParseRetryAfterAcceptsBothForms(t *testing.T) {
	if got := parseRetryAfter("5"); got != 5*time.Second {
		t.Errorf("seconds form gave %v", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("empty header gave %v", got)
	}
	if got := parseRetryAfter("not-a-date"); got != 0 {
		t.Errorf("garbage gave %v", got)
	}
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > 4*time.Second {
		t.Errorf("HTTP-date form gave %v", got)
	}
}

// TestFlatErrorBodiesAreUnderstood covers a real gateway: it answers
// {"status":401,"message":"..."} rather than the nested OpenAI shape, and
// falling back to dumping the raw body turned a clear refusal into JSON the
// operator had to read by eye.
func TestFlatErrorBodiesAreUnderstood(t *testing.T) {
	cases := map[string]struct{ body, wantMessage, wantCode string }{
		"nested OpenAI": {
			`{"error":{"message":"Incorrect API key provided","code":"invalid_api_key"}}`,
			"Incorrect API key provided", "invalid_api_key",
		},
		"flat gateway": {
			`{"status":401,"message":"API Key tidak valid","data":{}}`,
			"API Key tidak valid", "",
		},
		"detail only": {
			`{"detail":"quota exhausted"}`,
			"quota exhausted", "",
		},
	}

	for name, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(tc.body))
		}))

		_, err := NewOpenAI("test", fastConfig(srv.URL)).Complete(context.Background(), llm.Request{
			Messages: []llm.Message{{Role: llm.RoleUser, Text: "halo"}},
		})
		srv.Close()

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Errorf("%s: want *APIError, got %v", name, err)
			continue
		}
		if apiErr.Message != tc.wantMessage {
			t.Errorf("%s: message is %q, want %q", name, apiErr.Message, tc.wantMessage)
		}
		if apiErr.Code != tc.wantCode {
			t.Errorf("%s: code is %q, want %q", name, apiErr.Code, tc.wantCode)
		}
	}
}

// TestNonJSONErrorBodiesSurviveIntact keeps a plain-text 404 from becoming an
// empty message.
func TestNonJSONErrorBodiesSurviveIntact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}))
	defer srv.Close()

	_, err := NewOpenAI("test", fastConfig(srv.URL)).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "halo"}},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.Message != "404 page not found" {
		t.Errorf("message is %q", apiErr.Message)
	}
}
