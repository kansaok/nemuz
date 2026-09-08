// Package httpapi serves nemuz over the OpenAI chat-completions shape, so any
// client that already speaks to OpenAI can speak to a nemuz agent instead.
//
// Every request becomes a recorded turn, and the completion id it returns is
// the turn id. That is not cosmetic: a caller who gets a strange answer can
// hand that id to `nemuz replay` and see exactly what happened, which is not
// something an OpenAI-shaped endpoint normally offers.
//
// # What it does not do
//
// The agent loop produces an answer whole rather than token by token, so a
// streaming request is answered by sending the finished text as one content
// chunk followed by the terminator. Clients that require `stream: true` work;
// they simply do not see the answer arrive gradually. Saying so here is better
// than implying a streaming pipeline that does not exist.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/kansaok/nemuz/internal/agent"
)

// Runner is the part of an agent this package needs.
//
// It is an interface rather than a concrete type so the server can be tested
// without a provider, and so this package does not depend on the whole runtime.
type Runner interface {
	// Run executes one turn and returns its outcome, including the turn id.
	Run(ctx context.Context, prompt string) (agent.Outcome, error)
}

// Options configures a Server.
type Options struct {
	// Runner executes turns. Required.
	Runner Runner
	// Model is reported in responses and by /v1/models.
	Model string
	// APIKey is required in an Authorization header. It may be empty only
	// when the listener is bound to loopback; NewServer enforces that.
	APIKey string
	// Addr is the listen address, for the loopback check.
	Addr string
	// Timeout bounds one request. Zero uses DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout bounds a single request.
const DefaultTimeout = 10 * time.Minute

// ErrKeyRequired means the server would have been reachable off this machine
// without authentication.
var ErrKeyRequired = errors.New("httpapi: an API key is required when not bound to loopback")

// Server answers chat-completion requests by running agent turns.
type Server struct {
	opts Options
}

// NewServer validates the options and returns a server.
//
// It refuses to start unauthenticated on a non-loopback address. That is the
// mistake worth making impossible: an agent endpoint with no key is a remote
// shell with extra steps, and the default should never be able to become one by
// accident.
func NewServer(opts Options) (*Server, error) {
	if opts.Runner == nil {
		return nil, errors.New("httpapi: Options.Runner is required")
	}
	if opts.Model == "" {
		return nil, errors.New("httpapi: Options.Model is required")
	}
	if opts.APIKey == "" && !isLoopback(opts.Addr) {
		return nil, fmt.Errorf("%w (listening on %s)", ErrKeyRequired, opts.Addr)
	}
	return &Server{opts: opts}, nil
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /health/detailed", s.healthDetailed)
	mux.Handle("GET /v1/models", s.authenticated(http.HandlerFunc(s.listModels)))
	mux.Handle("POST /v1/chat/completions", s.authenticated(http.HandlerFunc(s.completions)))
	return mux
}

// isLoopback reports whether addr binds only to this machine.
//
// An empty host means every interface, which is the dangerous case and is
// treated as such.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "localhost":
		return true
	case "", "0.0.0.0", "::", "[::]":
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// authenticated rejects requests without the configured key.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.APIKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		// Constant time, so a wrong key cannot be found one character at a
		// time by measuring how long the rejection took.
		if subtle.ConstantTimeCompare([]byte(presented), []byte(s.opts.APIKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid_api_key",
				"Provide the server's API key as: Authorization: Bearer <key>")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) healthDetailed(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"model":         s.opts.Model,
		"authenticated": s.opts.APIKey != "",
		"streaming":     "answers are produced whole, then sent as one chunk",
	})
}

func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, modelList{
		Object: "list",
		Data: []modelInfo{{
			ID: s.opts.Model, Object: "model", OwnedBy: "nemuz",
			Created: time.Now().Unix(),
		}},
	})
}

// completions runs one turn for a chat-completions request.
func (s *Server) completions(w http.ResponseWriter, r *http.Request) {
	var req completionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Could not read the request body: "+err.Error())
		return
	}

	prompt, err := req.prompt()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	timeout := s.opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	out, err := s.opts.Runner.Run(ctx, prompt)
	if err != nil {
		// The turn is recorded even when it fails, so the id is worth
		// returning: it is how the caller finds out what went wrong.
		message := err.Error()
		if out.TurnID != "" {
			message += " (recorded as turn " + out.TurnID + ")"
		}
		writeError(w, http.StatusInternalServerError, "agent_error", message)
		return
	}

	if req.Stream {
		s.streamCompletion(w, out)
		return
	}
	writeJSON(w, http.StatusOK, buildCompletion(out, s.opts.Model))
}

// maxRequestBytes caps a request body.
const maxRequestBytes = 8 << 20 // 8 MiB

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError returns an error in the shape OpenAI clients already parse.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": code, "code": code},
	})
}
