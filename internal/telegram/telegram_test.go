package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeServer answers one Bot API method with a fixed JSON body, recording the
// request it received so a test can assert on the parameters actually sent.
func fakeServer(t *testing.T, method string, respond func(body map[string]any) any) (*httptest.Server, *http.Request) {
	t.Helper()
	var lastReq *http.Request
	var lastBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastReq = r
		if !strings.HasSuffix(r.URL.Path, "/"+method) {
			t.Errorf("called %s, want a call to %s", r.URL.Path, method)
		}
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respond(lastBody))
	}))
	t.Cleanup(srv.Close)
	return srv, lastReq
}

func TestGetMeReturnsTheBotIdentity(t *testing.T) {
	srv, _ := fakeServer(t, "getMe", func(map[string]any) any {
		return map[string]any{"ok": true, "result": User{ID: 42, IsBot: true, Username: "nemuz_bot"}}
	})
	c := &Client{Token: "t", BaseURL: srv.URL}

	me, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if me.ID != 42 || me.Username != "nemuz_bot" {
		t.Errorf("got %+v", me)
	}
}

func TestGetMeSurfacesTelegramsOwnRefusal(t *testing.T) {
	srv, _ := fakeServer(t, "getMe", func(map[string]any) any {
		return map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"}
	})
	c := &Client{Token: "bad-token", BaseURL: srv.URL}

	if _, err := c.GetMe(context.Background()); err == nil {
		t.Fatal("an invalid token was accepted")
	} else if !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("error should explain why, got: %v", err)
	}
}

func TestGetUpdatesSendsTheOffsetSoTelegramNeverRedelivers(t *testing.T) {
	var seenOffset any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenOffset = body["offset"]
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []Update{}})
	}))
	defer srv.Close()
	c := &Client{Token: "t", BaseURL: srv.URL}

	if _, err := c.GetUpdates(context.Background(), 100, 0); err != nil {
		t.Fatal(err)
	}
	if seenOffset != float64(100) {
		t.Errorf("offset sent = %v, want 100", seenOffset)
	}
}

func TestGetUpdatesOmitsOffsetWhenZero(t *testing.T) {
	var sawOffsetKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, sawOffsetKey = body["offset"]
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []Update{}})
	}))
	defer srv.Close()
	c := &Client{Token: "t", BaseURL: srv.URL}

	if _, err := c.GetUpdates(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
	if sawOffsetKey {
		t.Error("offset=0 (never polled before) should not be sent at all")
	}
}

func TestGetUpdatesParsesIncomingMessages(t *testing.T) {
	srv, _ := fakeServer(t, "getUpdates", func(map[string]any) any {
		return map[string]any{"ok": true, "result": []Update{
			{UpdateID: 5, Message: &Message{MessageID: 1, Chat: Chat{ID: 99}, Text: "halo", From: &User{ID: 7, FirstName: "Budi"}}},
		}}
	})
	c := &Client{Token: "t", BaseURL: srv.URL}

	updates, err := c.GetUpdates(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Message.Text != "halo" || updates[0].Message.From.ID != 7 {
		t.Errorf("got %+v", updates)
	}
}

func TestSendMessageSplitsLongText(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": Message{}})
	}))
	defer srv.Close()
	c := &Client{Token: "t", BaseURL: srv.URL}

	long := strings.Repeat("a", maxMessageLen*2+10)
	if err := c.SendMessage(context.Background(), 1, long); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("sendMessage called %d times, want 3 chunks", calls)
	}
}

func TestSendMessageSurfacesFailure(t *testing.T) {
	srv, _ := fakeServer(t, "sendMessage", func(map[string]any) any {
		return map[string]any{"ok": false, "description": "chat not found"}
	})
	c := &Client{Token: "t", BaseURL: srv.URL}

	if err := c.SendMessage(context.Background(), 1, "hi"); err == nil {
		t.Fatal("a rejected sendMessage was not reported")
	}
}

func TestSplitMessageNeverCutsARune(t *testing.T) {
	// A multi-byte rune placed right at the boundary must stay whole.
	text := strings.Repeat("a", maxMessageLen-1) + "— long dash — " + strings.Repeat("b", 100)
	for _, chunk := range splitMessage(text) {
		if !isValidUTF8Boundary(chunk) {
			t.Errorf("a chunk ends mid-rune: %q", chunk[len(chunk)-10:])
		}
	}
}

func isValidUTF8Boundary(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return false
		}
		i += size
	}
	return true
}

func TestSplitMessageNeverSendsAnEmptyBody(t *testing.T) {
	chunks := splitMessage("")
	if len(chunks) != 1 || chunks[0] == "" {
		t.Errorf("an empty answer must still send something visible, got %v", chunks)
	}
}
