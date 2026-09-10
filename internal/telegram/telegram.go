// Package telegram is a minimal client for the Telegram Bot API: enough to
// long-poll for messages and reply to them.
//
// It exists so a channel plugin needs no third-party dependency for the most
// commonly requested chat surface — the same reasoning that keeps the LLM
// adapters on net/http rather than a vendor SDK.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"
)

// DefaultAPIBase is Telegram's own API root.
const DefaultAPIBase = "https://api.telegram.org"

// maxMessageLen is Telegram's limit on one message's text, in UTF-16 code
// units. Bytes are used as a conservative proxy: a message this short in
// bytes is never longer in UTF-16 units, so splitting on it never sends an
// oversized chunk, only occasionally a smaller one than strictly necessary.
const maxMessageLen = 4096

// Client talks to the Telegram Bot API using long polling rather than
// webhooks, so the bot needs no public URL or TLS certificate of its own —
// it reaches out to Telegram, not the other way around.
type Client struct {
	// Token is the bot token from @BotFather.
	Token string
	// BaseURL overrides DefaultAPIBase, for tests.
	BaseURL string
	// HTTPClient overrides the client. Its timeout must exceed the longest
	// poll timeout requested, or a long poll waiting on Telegram would be cut
	// off by the client itself before Telegram ever answers.
	HTTPClient *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 65 * time.Second}
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultAPIBase
}

type apiResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
}

// User is a Telegram account — a human, or the bot itself in GetMe.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

// Chat is where a message was sent — a DM, a group, or a channel.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// Message is one incoming or outgoing chat message.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
	Date      int64  `json:"date"`
}

// Update is one item from GetUpdates. Telegram sends other update kinds too
// (edited messages, callback queries); Message is nil for those, and the
// caller skips them.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

func (c *Client) call(ctx context.Context, method string, params map[string]any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("telegram: encode %s: %w", method, err)
	}
	url := c.baseURL() + "/bot" + c.Token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("telegram: %s: read response: %w", method, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("telegram: %s: decode response: %w", method, err)
	}
	return nil
}

// GetMe confirms the token is valid and names the bot, for a startup check —
// the same reason `nemuz doctor` reports the sandbox before any tool runs.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var resp apiResponse[User]
	if err := c.call(ctx, "getMe", nil, &resp); err != nil {
		return User{}, err
	}
	if !resp.OK {
		return User{}, fmt.Errorf("telegram: getMe: %s", resp.Description)
	}
	return resp.Result, nil
}

// GetUpdates long-polls for new messages, since a webhook would require a
// public URL and TLS certificate this process has neither reason nor
// obligation to hold. Telegram waits up to timeoutSecs server-side for a
// message to arrive rather than answering immediately with nothing, so the
// client spends most of its time blocked in one request rather than busy-
// polling.
//
// offset is the update id to resume after — pass the last processed update's
// UpdateID+1. Telegram then never redelivers anything at or before it, which
// is what makes a restart safe without nemuz keeping its own dedup table.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSecs int) ([]Update, error) {
	var resp apiResponse[[]Update]
	params := map[string]any{
		"timeout":         timeoutSecs,
		"allowed_updates": []string{"message"},
	}
	if offset != 0 {
		params["offset"] = offset
	}
	if err := c.call(ctx, "getUpdates", params, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram: getUpdates: %s", resp.Description)
	}
	return resp.Result, nil
}

// SendMessage replies in chatID, splitting text across several messages if
// it exceeds Telegram's own length limit rather than truncating it.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) error {
	for _, chunk := range splitMessage(text) {
		var resp apiResponse[Message]
		params := map[string]any{"chat_id": chatID, "text": chunk}
		if err := c.call(ctx, "sendMessage", params, &resp); err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("telegram: sendMessage: %s", resp.Description)
		}
	}
	return nil
}

// splitMessage breaks text into chunks no longer than maxMessageLen, never
// inside a UTF-8 rune. An empty message is sent as one chunk containing a
// placeholder, since Telegram refuses an empty sendMessage outright, and a
// turn that produced no text should still be visible as an answer rather than
// silently vanish.
func splitMessage(text string) []string {
	if text == "" {
		return []string{"(no output)"}
	}
	if len(text) <= maxMessageLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > maxMessageLen {
		cut := maxMessageLen
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			cut = maxMessageLen // pathological: not expected with valid UTF-8
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}
