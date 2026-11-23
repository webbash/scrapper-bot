package telegram

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientSendsOnePhotoWithInlineButtons(t *testing.T) {
	api, err := New("123:secret-test")
	if err != nil {
		t.Fatal(err)
	}
	api.http.Transport = fakeTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Host != "api.telegram.org" || !strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		var body struct {
			Chat     int64     `json:"chat_id"`
			Photo    string    `json:"photo"`
			Caption  string    `json:"caption"`
			Keyboard *Keyboard `json:"reply_markup"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Chat != -1234 || body.Photo != "https://example.com/sample.jpg" ||
			body.Caption != "Sample" || body.Keyboard == nil || body.Keyboard.Rows[0][0].Callback != "yes:42" {
			t.Fatalf("unexpected payload: %+v", body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":99}}`))}, nil
	})
	msg, err := api.SendPhoto(t.Context(), -1234, "https://example.com/sample.jpg", "Sample",
		&Keyboard{Rows: [][]Button{{{Text: "Подходит", Callback: "yes:42"}}}})
	if msg.ID != 99 || err != nil {
		t.Fatalf("msg=%+v err=%v", msg, err)
	}
}

func TestClientDoesNotLeakTokenInTransportErrors(t *testing.T) {
	const token = "123:secret-test"
	api, err := New(token)
	if err != nil {
		t.Fatal(err)
	}
	api.http.Transport = fakeTransport(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("failed request to " + r.URL.String())
	})
	_, err = api.Updates(t.Context(), 0)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("expected a redacted error, got %v", err)
	}
}

func TestUpdatesDecodesCallbackAndSendsPollingOffset(t *testing.T) {
	api, err := New("123:secret-test")
	if err != nil {
		t.Fatal(err)
	}
	api.http.Transport = fakeTransport(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Offset  int64
			Timeout int
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Offset != 77 || body.Timeout <= 0 {
			t.Fatalf("expected long polling at offset 77, got %+v", body)
		}
		const result = `{"ok":true,"result":[{"update_id":77,"callback_query":{"id":"click","data":"yes:42","message":{"message_id":99,"chat":{"id":-1234}}}}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(result))}, nil
	})
	updates, err := api.Updates(t.Context(), 77)
	if err != nil || len(updates) != 1 || updates[0].Callback == nil ||
		updates[0].Callback.Message == nil || updates[0].Callback.Message.Chat.ID != -1234 ||
		updates[0].Callback.Data != "yes:42" {
		t.Fatalf("updates=%+v err=%v", updates, err)
	}
}
