package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

var botToken = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

func New(token string) (*Client, error) {
	if !botToken.MatchString(token) {
		return nil, errors.New("TELEGRAM_BOT_TOKEN is missing or invalid")
	}
	return &Client{
		endpoint: "https://api.telegram.org/bot" + token + "/",
		token:    token,
		http: &http.Client{
			Timeout: 35 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

type APIError struct {
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Telegram API %d: %s", e.Code, e.Description)
}

// Rejected is true only for an explicit client rejection from the Bot API,
// not for transport failures, unexpected responses or server errors.
func (e *APIError) Rejected() bool { return e.Code >= 400 && e.Code < 500 }

type Button struct {
	Text     string `json:"text"`
	URL      string `json:"url,omitempty"`
	Callback string `json:"callback_data,omitempty"`
}

type Keyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

type Message struct {
	ID       int64             `json:"message_id"`
	Text     string            `json:"text"`
	Caption  string            `json:"caption"`
	Photo    []json.RawMessage `json:"photo"`
	Keyboard *Keyboard         `json:"reply_markup"`
	Chat     struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
}

type Callback struct {
	ID      string   `json:"id"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

type Update struct {
	ID       int64     `json:"update_id"`
	Message  *Message  `json:"message"`
	Callback *Callback `json:"callback_query"`
}

func (c *Client) Updates(ctx context.Context, offset int64) ([]Update, error) {
	var updates []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": 25, "allowed_updates": []string{"message", "callback_query"},
	}, &updates)
	return updates, err
}

func (c *Client) SendText(ctx context.Context, chatID int64, text, format string, keyboard *Keyboard) (Message, error) {
	params := map[string]any{
		"chat_id": chatID, "text": text, "link_preview_options": map[string]bool{"is_disabled": true},
	}
	if format != "" {
		params["parse_mode"] = format
	}
	if keyboard != nil {
		params["reply_markup"] = keyboard
	}
	var message Message
	err := c.call(ctx, "sendMessage", params, &message)
	return message, err
}

func (c *Client) SendPhoto(ctx context.Context, chatID int64, photoURL, caption string, keyboard *Keyboard) (Message, error) {
	var message Message
	err := c.call(ctx, "sendPhoto", map[string]any{
		"chat_id": chatID, "photo": photoURL, "caption": caption, "reply_markup": keyboard,
	}, &message)
	return message, err
}

func (c *Client) Answer(ctx context.Context, callbackID, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]string{
		"callback_query_id": callbackID, "text": text,
	}, nil)
}

func (c *Client) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	return c.call(ctx, "deleteMessage", map[string]int64{"chat_id": chatID, "message_id": messageID}, nil)
}

// CollapseMessage replaces the text or photo caption and clears its buttons.
func (c *Client) CollapseMessage(ctx context.Context, chatID, messageID int64, hasPhoto bool) error {
	method, field := "editMessageText", "text"
	if hasPhoto {
		method, field = "editMessageCaption", "caption"
	}
	return c.call(ctx, method, map[string]any{
		"chat_id": chatID, "message_id": messageID, field: "Не подходит",
		"reply_markup": Keyboard{Rows: [][]Button{}},
	}, nil)
}

func (c *Client) call(ctx context.Context, method string, input any, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Telegram %s request: %w", method, c.safe(err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Telegram %s: %w", method, c.safe(err))
	}
	defer resp.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("invalid Telegram %s response (HTTP %d)", method, resp.StatusCode)
	}
	if !envelope.OK && envelope.Code != 0 {
		return &APIError{Code: envelope.Code, Description: c.redact(envelope.Description)}
	}
	if resp.StatusCode != http.StatusOK || !envelope.OK {
		return fmt.Errorf("unexpected Telegram %s response (HTTP %d)", method, resp.StatusCode)
	}
	if output != nil {
		if err := json.Unmarshal(envelope.Result, output); err != nil {
			return fmt.Errorf("invalid Telegram %s result", method)
		}
	}
	return nil
}

// net/http errors can contain the API endpoint, which embeds the bot token.
func (c *Client) safe(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	return errors.New(c.redact(err.Error()))
}

func (c *Client) redact(text string) string {
	return strings.ReplaceAll(text, c.token, "[redacted]")
}
