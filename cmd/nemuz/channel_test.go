package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/telegram"
	"github.com/spf13/cobra"
)

func TestAllowedUser(t *testing.T) {
	allow := []int64{7, 42}
	if !allowedUser(allow, 7) || !allowedUser(allow, 42) {
		t.Error("a listed user was refused")
	}
	if allowedUser(allow, 99) {
		t.Error("an unlisted user was accepted")
	}
	if allowedUser(nil, 7) {
		t.Error("an empty allowlist accepted someone")
	}
}

func TestTelegramOffsetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram-offset")

	if got := loadTelegramOffset(path); got != 0 {
		t.Errorf("a never-written offset file gave %d, want 0", got)
	}
	if err := saveTelegramOffset(path, 12345); err != nil {
		t.Fatal(err)
	}
	if got := loadTelegramOffset(path); got != 12345 {
		t.Errorf("got %d, want 12345", got)
	}
}

func TestTelegramOffsetToleratesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram-offset")
	if err := os.WriteFile(path, []byte("not a number"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadTelegramOffset(path); got != 0 {
		t.Errorf("a corrupt offset file should fall back to 0, got %d", got)
	}
}

// fakeTelegramSendMessage answers only sendMessage, recording every call, so a
// test can inspect what the bot actually sent back.
func fakeTelegramSendMessage(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": telegram.Message{}})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestTelegramBotIgnoresAnUnauthorizedUser(t *testing.T) {
	sess := newTestSession(t, nil) // no scripted responses: a call here would fail the test
	srv, calls := fakeTelegramSendMessage(t)

	bot := &telegramBot{
		client:      &telegram.Client{Token: "t", BaseURL: srv.URL},
		session:     sess,
		cmd:         &cobra.Command{},
		out:         io.Discard,
		allowUsers:  []int64{1},
		pollTimeout: 0,
	}
	bot.handle(context.Background(), telegram.Update{
		UpdateID: 1,
		Message: &telegram.Message{
			Chat: telegram.Chat{ID: 55},
			Text: "halo",
			From: &telegram.User{ID: 999, Username: "orang-asing"},
		},
	})

	if len(*calls) != 0 {
		t.Errorf("an unauthorized user's message got a reply: %v", *calls)
	}
	if sess.provider.(*llm.Static).Calls() != 0 {
		t.Error("an unauthorized message reached the model")
	}
}

func TestTelegramBotAnswersAnAllowedUser(t *testing.T) {
	sess := newTestSession(t, []llm.Response{{Text: "empat", StopReason: llm.StopEnd}})
	srv, calls := fakeTelegramSendMessage(t)

	bot := &telegramBot{
		client:      &telegram.Client{Token: "t", BaseURL: srv.URL},
		session:     sess,
		cmd:         &cobra.Command{},
		out:         io.Discard,
		allowUsers:  []int64{7},
		pollTimeout: 0,
	}
	bot.handle(context.Background(), telegram.Update{
		UpdateID: 1,
		Message: &telegram.Message{
			Chat: telegram.Chat{ID: 55},
			Text: "berapa 2+2?",
			From: &telegram.User{ID: 7, Username: "budi"},
		},
	})

	if len(*calls) != 1 {
		t.Fatalf("got %d replies, want 1", len(*calls))
	}
	if (*calls)[0]["text"] != "empat" {
		t.Errorf("reply text = %v", (*calls)[0]["text"])
	}
	if (*calls)[0]["chat_id"] != float64(55) {
		t.Errorf("reply went to chat %v, want 55", (*calls)[0]["chat_id"])
	}
}

func TestTelegramBotSkipsUpdatesWithNoText(t *testing.T) {
	sess := newTestSession(t, nil)
	srv, calls := fakeTelegramSendMessage(t)

	bot := &telegramBot{
		client:      &telegram.Client{Token: "t", BaseURL: srv.URL},
		session:     sess,
		cmd:         &cobra.Command{},
		out:         io.Discard,
		allowUsers:  []int64{7},
		pollTimeout: 0,
	}
	bot.handle(context.Background(), telegram.Update{UpdateID: 1, Message: nil})
	bot.handle(context.Background(), telegram.Update{
		UpdateID: 2,
		Message:  &telegram.Message{Chat: telegram.Chat{ID: 1}, Text: "   ", From: &telegram.User{ID: 7}},
	})

	if len(*calls) != 0 {
		t.Errorf("a message with no text should never reach the model or get a reply, got %d replies", len(*calls))
	}
	if sess.provider.(*llm.Static).Calls() != 0 {
		t.Error("an empty message reached the model")
	}
}

func TestTelegramBotReportsAFailedTurnBackToTheChat(t *testing.T) {
	sess := newTestSession(t, nil) // the script is empty, so the one call fails
	srv, calls := fakeTelegramSendMessage(t)

	bot := &telegramBot{
		client:      &telegram.Client{Token: "t", BaseURL: srv.URL},
		session:     sess,
		cmd:         &cobra.Command{},
		out:         io.Discard,
		allowUsers:  []int64{7},
		pollTimeout: 0,
	}
	bot.handle(context.Background(), telegram.Update{
		UpdateID: 1,
		Message:  &telegram.Message{Chat: telegram.Chat{ID: 55}, Text: "hi", From: &telegram.User{ID: 7}},
	})

	if len(*calls) != 1 {
		t.Fatalf("got %d replies, want 1 explaining the failure", len(*calls))
	}
	if !strings.Contains((*calls)[0]["text"].(string), "failed") {
		t.Errorf("reply should say the turn failed, got: %v", (*calls)[0]["text"])
	}
}

// TestTelegramBotRequiresMentionInGroups covers channels.telegram.groups.*.
// requireMention: in a group the bot stays silent unless addressed by name, so
// two bots in the same room do not argue about every message.
func TestTelegramBotRequiresMentionInGroups(t *testing.T) {
	sess := newTestSession(t, []llm.Response{{Text: "empat", StopReason: llm.StopEnd}})
	srv, calls := fakeTelegramSendMessage(t)

	bot := &telegramBot{
		client:         &telegram.Client{Token: "t", BaseURL: srv.URL},
		session:        sess,
		cmd:            &cobra.Command{},
		out:            io.Discard,
		allowUsers:     []int64{7},
		pollTimeout:    0,
		botUsername:    "nemuz_bot",
		requireMention: true,
	}

	// Allowed user, group chat, no mention: ignored.
	bot.handle(context.Background(), telegram.Update{
		UpdateID: 1,
		Message: &telegram.Message{
			Chat: telegram.Chat{ID: 55, Type: "supergroup"},
			Text: "sip",
			From: &telegram.User{ID: 7},
		},
	})
	// Mentioned: answered.
	bot.handle(context.Background(), telegram.Update{
		UpdateID: 2,
		Message: &telegram.Message{
			Chat: telegram.Chat{ID: 55, Type: "supergroup"},
			Text: "hey @nemuz_bot berapa 2+2?",
			From: &telegram.User{ID: 7},
		},
	})
	// A DM needs no mention even when requireMention is set.
	bot.handle(context.Background(), telegram.Update{
		UpdateID: 3,
		Message: &telegram.Message{
			Chat: telegram.Chat{ID: 99, Type: "private"},
			Text: "berapa 2+2?",
			From: &telegram.User{ID: 7},
		},
	})

	if len(*calls) != 2 {
		t.Errorf("got %d replies, want only the mentioned/private ones", len(*calls))
	}
}
