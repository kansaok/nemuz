// Package provider holds the adapters that translate between nemuz's IR and
// each vendor's wire format.
//
// Adapters are the only place that knows a vendor exists. Everything above them
// — the turn loop, the journal, the tools — sees llm.Request and llm.Response
// and nothing else. That is what lets one recorded turn be replayed against a
// different provider, and what keeps adding a provider from touching the core.
//
// # Retries and the journal
//
// A retried HTTP attempt is not a separate model call. The journal records the
// logical exchange — one request, one response — so a turn that succeeded on
// its third attempt replays as a turn that succeeded, which is what a replay is
// supposed to reproduce. Transport noise belongs in logs, not in the record of
// what the agent decided.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
)

// DefaultTimeout bounds a single HTTP attempt. Model calls are slow, but a call
// that has produced nothing for this long is not going to.
const DefaultTimeout = 10 * time.Minute

// Retry policy defaults. Backoff is deterministic — no jitter — because a
// single client is not a thundering herd, and reproducible timing is easier to
// reason about when something goes wrong.
const (
	DefaultMaxAttempts = 4
	DefaultBackoffBase = 500 * time.Millisecond
	maxBackoff         = 30 * time.Second
)

// maxErrorBody caps how much of an error response is read, so a provider
// returning an HTML error page cannot fill memory or a log line.
const maxErrorBody = 8 << 10

// APIError is a provider's refusal, normalised.
type APIError struct {
	Provider   string
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: HTTP %d (%s): %s", e.Provider, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Message)
}

// Retryable reports whether trying again could plausibly succeed.
//
// Rate limits and server faults are worth retrying. A 4xx that is not a rate
// limit means the request itself is wrong, and sending it again just wastes
// quota and time.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Config is what every adapter needs to reach its provider.
type Config struct {
	// BaseURL is the API root, without a trailing slash.
	BaseURL string
	// APIKey authenticates the caller.
	APIKey string
	// Model is the default model id for requests that do not name one.
	Model string
	// HTTPClient overrides the client, mainly so tests can point at httptest.
	HTTPClient *http.Client
	// MaxAttempts overrides DefaultMaxAttempts. One means no retries.
	MaxAttempts int
	// BackoffBase overrides DefaultBackoffBase.
	BackoffBase time.Duration
	// Headers are added to every request.
	Headers map[string]string
}

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: DefaultTimeout}
}

func (c Config) attempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (c Config) backoffBase() time.Duration {
	if c.BackoffBase > 0 {
		return c.BackoffBase
	}
	return DefaultBackoffBase
}

// transport performs JSON calls against one provider, with retries.
type transport struct {
	name string
	cfg  Config
	// decodeError turns a provider's error body into a code and message.
	// Each vendor shapes these differently.
	decodeError func(body []byte) (code, message string)
}

// postJSON sends body to path and decodes the response into out.
func (t *transport) postJSON(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s: encode request: %w", t.name, err)
	}

	var lastErr error
	attempts := t.cfg.attempts()
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, t.backoffFor(attempt, lastErr)); err != nil {
				return err
			}
		}

		respBody, err := t.attempt(ctx, path, payload)
		if err == nil {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("%s: decode response: %w", t.name, err)
			}
			return nil
		}
		lastErr = err

		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable() {
			return err
		}
	}
	return fmt.Errorf("%s: gave up after %d attempts: %w", t.name, attempts, lastErr)
}

// attempt performs one HTTP request.
func (t *transport) attempt(ctx context.Context, path string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", t.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := t.cfg.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, t.apiError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", t.name, err)
	}
	return body, nil
}

func (t *transport) apiError(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	apiErr := &APIError{
		Provider:   t.name,
		Status:     resp.StatusCode,
		Message:    string(bytes.TrimSpace(body)),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
	if t.decodeError != nil {
		if code, msg := t.decodeError(body); msg != "" {
			apiErr.Code, apiErr.Message = code, msg
		}
	}
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(resp.StatusCode)
	}
	return apiErr
}

// backoffFor returns how long to wait before the given attempt.
//
// A provider's own Retry-After wins over the computed backoff: it knows when it
// will be ready and we do not.
func (t *transport) backoffFor(attempt int, lastErr error) time.Duration {
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.RetryAfter > 0 {
		return min(apiErr.RetryAfter, maxBackoff)
	}
	base := t.cfg.backoffBase()
	d := time.Duration(float64(base) * math.Pow(2, float64(attempt-2)))
	return min(d, maxBackoff)
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// firstJSONField returns the first of several paths that yields a string.
//
// Providers agree on the OpenAI shape until they do not. Gateways in particular
// tend to answer with a flat {"message": "..."} instead of the nested
// {"error": {"message": "..."}}, and falling back to dumping the raw body turns
// a clear refusal into noise the operator has to read JSON to understand.
func firstJSONField(body []byte, paths ...[]string) string {
	for _, path := range paths {
		if v := jsonErrorField(body, path...); v != "" {
			return v
		}
	}
	return ""
}

// jsonErrorField pulls a nested string out of a provider's error body, which is
// how most of them report a code and a message.
func jsonErrorField(body []byte, path ...string) string {
	var node any
	if err := json.Unmarshal(body, &node); err != nil {
		return ""
	}
	for _, key := range path {
		obj, ok := node.(map[string]any)
		if !ok {
			return ""
		}
		node, ok = obj[key]
		if !ok {
			return ""
		}
	}
	s, _ := node.(string)
	return s
}
