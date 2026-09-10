package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Default bases for the two presets that do not ship a BaseURL in the
// catalogue; used only when listing models during setup.
const (
	defaultAnthropicBase = "https://api.anthropic.com"
	defaultGeminiBase    = "https://generativelanguage.googleapis.com"
)

// errNoModelList is returned when the endpoint answers but has no model
// listing (a private gateway or a proxy). The key still validated — it is a
// 401/403 that is a bad key — so the caller falls back to typing the model.
var errNoModelList = errors.New("this provider does not expose a model list")

// errBadKey means the endpoint answered 401 or 403 — the key was rejected.
var errBadKey = errors.New("the API key was rejected")

func defaultBase(wire, baseURL string) string {
	if baseURL != "" {
		return strings.TrimRight(baseURL, "/")
	}
	if wire == "anthropic" {
		return defaultAnthropicBase
	}
	if wire == "google-ai" {
		return defaultGeminiBase
	}
	return ""
}

// fetchModels validates the key and returns the provider's model ids in one
// round trip: a 401/403 is reported as errBadKey, a working endpoint without a
// model list as errNoModelList, and anything else as a plain error.
func fetchModels(ctx context.Context, wire, baseURL, key string) ([]string, error) {
	base := defaultBase(wire, baseURL)

	var (
		url     string
		headers = map[string]string{}
	)
	switch wire {
	case "anthropic":
		url = base + "/v1/models"
		if strings.HasSuffix(base, "/v1") {
			url = base + "/models"
		}
		headers["x-api-key"] = key
		headers["anthropic-version"] = "2023-06-01"
	case "google-ai":
		url = base + "/v1beta/models?key=" + key
	default:
		url = base + "/models"
		headers["Authorization"] = "Bearer " + key
	}

	status, body, err := doGet(ctx, url, headers)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, errBadKey
	}
	if status != http.StatusOK {
		if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			return nil, errNoModelList
		}
		return nil, fmt.Errorf("%s answered %d — the key may still be fine, but no model list came back", url, status)
	}

	var ids []string
	switch wire {
	case "anthropic":
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("parse %s: %w", url, err)
		}
		for _, m := range out.Data {
			if m.ID != "" {
				ids = append(ids, m.ID)
			}
		}
	case "google-ai":
		var out struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("parse %s: %w", url, err)
		}
		for _, m := range out.Models {
			if id := strings.TrimPrefix(m.Name, "models/"); id != "" {
				ids = append(ids, id)
			}
		}
	default:
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("parse %s: %w", url, err)
		}
		for _, m := range out.Data {
			if m.ID != "" {
				ids = append(ids, m.ID)
			}
		}
	}
	if len(ids) == 0 {
		return nil, errNoModelList
	}
	return ids, nil
}

func doGet(ctx context.Context, url string, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}
