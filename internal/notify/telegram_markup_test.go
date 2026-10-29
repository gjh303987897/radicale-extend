package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"radicale-extend/internal/config"
	"radicale-extend/internal/store"
)

func TestTelegramReplyMarkupAndPlainText(t *testing.T) {
	var payload map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.Telegram.Enabled = true
	cfg.Telegram.Token.Value = "fake-secret"
	cfg.Telegram.ChatID = "123"
	cfg.Telegram.APIURL = server.URL
	sender := New(cfg)
	for _, tc := range []struct {
		name, title, body, markup, want string
	}{
		{"inline buttons", "", "<review> & [notes] 😀", `{"inline_keyboard":[[{"text":"查看 & 编辑","callback_data":"event:abc"}]]}`, "<review> & [notes] 😀"},
		{"reply prompt", "", "请输入新标题", `{"force_reply":true,"input_field_placeholder":"标题"}`, "请输入新标题"},
		{"notification", "Reminder", "Meeting", "", "Reminder\nMeeting"},
		{"title only", "Reminder", "", "", "Reminder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload = nil
			if err := sender.Send(context.Background(), store.Job{Channel: "telegram", Title: tc.title, Body: tc.body, TelegramReplyMarkup: tc.markup}); err != nil {
				t.Fatal(err)
			}
			var text string
			if err := json.Unmarshal(payload["text"], &text); err != nil || text != tc.want {
				t.Fatal("changed reply text or unwanted newline", text, err)
			}
			if _, ok := payload["parse_mode"]; ok {
				t.Fatal("calendar text must stay plain text")
			}
			var preview struct {
				Disabled bool `json:"is_disabled"`
			}
			if err := json.Unmarshal(payload["link_preview_options"], &preview); err != nil || !preview.Disabled {
				t.Fatal("link previews were not disabled", err)
			}
			if tc.markup == "" {
				if _, ok := payload["reply_markup"]; ok {
					t.Fatal("normal notification gained markup")
				}
			} else {
				var got, want any
				if err := json.Unmarshal(payload["reply_markup"], &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(tc.markup), &want); err != nil {
					t.Fatal(err)
				}
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if string(gotJSON) != string(wantJSON) {
					t.Fatal("reply markup changed", string(gotJSON))
				}
			}
		})
	}
}

func TestTelegramRejectsInvalidReplyMarkupBeforeSending(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.Telegram.Enabled = true
	cfg.Telegram.Token.Value = "fake-secret"
	cfg.Telegram.APIURL = server.URL
	sender := New(cfg)
	for _, markup := range []string{`{"private title":`, `[]`, `"private title"`, `null`, `true`, `123`, `{"x":[]} trailing-private`} {
		err := sender.Send(context.Background(), store.Job{Channel: "telegram", Body: "private body", TelegramReplyMarkup: markup})
		var detail *Error
		if !errors.As(err, &detail) || !detail.Permanent || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "fake-secret") {
			t.Fatal("invalid markup did not fail safely", err)
		}
	}
	if calls != 0 {
		t.Fatal("invalid markup reached Telegram", calls)
	}
}
