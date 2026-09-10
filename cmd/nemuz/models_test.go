package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchModelsOpenAIWire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ok" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"},{"id":""},{"id":"model-c"}]}`))
	}))
	defer srv.Close()

	ids, err := fetchModels(context.Background(), "openai-completions", srv.URL+"/v1", "sk-ok")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != "model-a" || ids[2] != "model-c" {
		t.Errorf("ids = %v", ids)
	}
}

func TestFetchModelsReportsBadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := fetchModels(context.Background(), "openai-completions", srv.URL, "nope")
	if !errors.Is(err, errBadKey) {
		t.Errorf("err = %v, want errBadKey", err)
	}
}

func TestFetchModelsNoListEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := fetchModels(context.Background(), "openai-completions", srv.URL, "k")
	if !errors.Is(err, errNoModelList) {
		t.Errorf("err = %v, want errNoModelList", err)
	}
}

func TestFetchModelsAnthropicWire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-ant" {
			t.Errorf("x-api-key = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"claude-a"},{"id":"claude-b"}]}`))
	}))
	defer srv.Close()

	ids, err := fetchModels(context.Background(), "anthropic", srv.URL, "sk-ant")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[1] != "claude-b" {
		t.Errorf("ids = %v", ids)
	}
}

func TestFetchModelsGeminiWire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if q := r.URL.Query().Get("key"); q != "gk" {
			t.Errorf("key param = %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"name":"models/gemini-x"},{"name":"models/gemini-y"}]}`))
	}))
	defer srv.Close()

	ids, err := fetchModels(context.Background(), "google-ai", srv.URL, "gk")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "gemini-x" {
		t.Errorf("ids = %v", ids)
	}
}

func TestFetchModelsMissingBaseURLIsAnError(t *testing.T) {
	if _, err := fetchModels(context.Background(), "openai-completions", "", "k"); err == nil {
		t.Error("a missing base URL was silently accepted")
	}
}
