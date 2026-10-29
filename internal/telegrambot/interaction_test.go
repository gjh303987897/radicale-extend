package telegrambot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"radicale-extend/internal/service"
	"radicale-extend/internal/store"
)

func callbackUpdate(b *Bot, id int64, data string) Update {
	return Update{ID: id, Callback: &callbackQuery{ID: fmt.Sprint(id), Data: data, From: telegramUser{ID: 123}, Message: &telegramMessage{ID: 42, Date: b.Now().Unix(), Chat: telegramChat{ID: 123, Type: "private"}, From: telegramUser{ID: 456, IsBot: true}}}}
}
func keyboard(t *testing.T, m store.TelegramMessage) [][]inlineButton {
	t.Helper()
	var result struct {
		Rows [][]inlineButton `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(m.ReplyMarkup), &result); err != nil {
		t.Fatal(err)
	}
	for _, row := range result.Rows {
		for _, btn := range row {
			if btn.Data != "" && (len(btn.Data) > 64 || btn.Copy != nil) {
				t.Fatal("invalid inline button", btn)
			}
		}
	}
	return result.Rows
}
func callbackServer(t *testing.T, b *Bot) *int {
	t.Helper()
	acks := new(int)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			t.Error("unexpected Telegram request", r.URL.Path)
		}
		*acks++
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	}))
	t.Cleanup(s.Close)
	b.Config.Telegram.APIURL = s.URL
	return acks
}
func TestConciseListsUseEventButtonsAndKeepEntriesTogether(t *testing.T) {
	b, q, _ := testBot(t)
	for i := 0; i < 20; i++ {
		q.events = append(q.events, service.AgendaEvent{Summary: strings.Repeat("项目", 40), Location: "room", Start: b.Now().Add(time.Duration(i) * time.Hour), End: b.Now().Add(time.Duration(i+1) * time.Hour), Resource: fmt.Sprintf("/alice/a/%d.ics", i), Calendar: "/alice/a/", UID: fmt.Sprint(i), ETag: "etag"})
	}
	q.events[0].Hidden = true
	q.events[0].Summary = "private secret"
	q.events[0].Location = "secret room"
	parts := b.respond(context.Background(), update(1, "/next 20", "private", 123))
	buttons := 0
	for _, m := range parts {
		if strings.Contains(m.Body, "ID：") || strings.Contains(m.Body, "secret") || strings.Contains(m.Body, "/remind") || len([]rune(m.Body)) > 1500 {
			t.Fatal("verbose, unsafe or oversized list", m.Body)
		}
		for _, row := range keyboard(t, m) {
			for _, btn := range row {
				if strings.HasPrefix(btn.Data, "v1:event:") {
					buttons++
					prefix, _, _ := strings.Cut(btn.Text, ". ")
					if !strings.Contains(m.Body, prefix+". ") {
						t.Fatal("button detached from entry", btn, m.Body)
					}
				}
			}
		}
	}
	if buttons != 20 {
		t.Fatal("event buttons missing after pagination", buttons)
	}
}
func TestCallbacksNavigateAndConfigureOnlySelectedOccurrence(t *testing.T) {
	b, e, id := selectedBot(t)
	acks := callbackServer(t, b)
	ctx := context.Background()
	for _, data := range []string{"v1:event:" + id, "v1:rem:" + id, "v1:rule:" + id + ":s"} {
		parts := b.respond(ctx, callbackUpdate(b, 10, data))
		if len(parts) != 1 {
			t.Fatal(data, parts)
		}
		keyboard(t, parts[0])
		if e.writeCalls != 0 {
			t.Fatal("view button modified event")
		}
	}
	for _, preset := range []string{"0s", "5m", "15m", "30m", "1h", "off", "default"} {
		parts := b.respond(ctx, callbackUpdate(b, 11, "v1:set:"+id+":s:"+preset))
		if len(parts) == 0 || e.channel != "smtp" || e.series || e.ref.Instance == "" || !strings.Contains(parts[0].Body, "不会发送") {
			t.Fatal(preset, e, parts)
		}
	}
	if e.writeCalls != 7 || *acks != 10 {
		t.Fatal(e.writeCalls, *acks)
	}
	parts := b.respond(ctx, callbackUpdate(b, 12, "v1:event:"+id))
	copied := ""
	for _, row := range keyboard(t, parts[0]) {
		for _, btn := range row {
			if btn.Copy != nil {
				copied = btn.Copy.Text
			}
		}
	}
	if copied != "/title "+id+" 新标题" {
		t.Fatal("no title command template", copied)
	}
	parts = b.respond(ctx, callbackUpdate(b, 13, "v1:next:10"))
	if len(parts) == 0 {
		t.Fatal("navigation did not reply")
	}
}
func TestCallbacksRejectForeignMalformedStaleAndInaccessibleRequests(t *testing.T) {
	for _, kind := range []string{"foreign user", "foreign chat", "group", "user sender", "other bot", "inaccessible", "inline only", "missing callback id", "missing message id", "oversized data", "bad id", "bad preset", "unknown action", "extra data", "other scope", "expired"} {
		t.Run(kind, func(t *testing.T) {
			b, e, id := selectedBot(t)
			callbackServer(t, b)
			u := callbackUpdate(b, 10, "v1:set:"+id+":t:15m")
			switch kind {
			case "foreign user":
				u.Callback.From.ID = 999
			case "foreign chat":
				u.Callback.Message.Chat.ID = 999
				u.Callback.From.ID = 999
			case "group":
				u.Callback.Message.Chat.Type = "group"
			case "user sender":
				u.Callback.Message.From.IsBot = false
			case "other bot":
				u.Callback.Message.From.ID = 999
			case "inaccessible":
				u.Callback.Message.Date = 0
			case "inline only":
				u.Callback.Message = nil
			case "missing callback id":
				u.Callback.ID = ""
			case "missing message id":
				u.Callback.Message.ID = 0
			case "oversized data":
				u.Callback.Data = strings.Repeat("x", 65)
			case "bad id":
				u.Callback.Data = "v1:set:../../calendar:t:15m"
			case "bad preset":
				u.Callback.Data = "v1:set:" + id + ":t:2h"
			case "unknown action":
				u.Callback.Data = "v1:delete:" + id
			case "extra data":
				u.Callback.Data += "x:extra"
			case "other scope":
				b.scope = "another-bot"
			case "expired":
				now := b.Now().Add(25 * time.Hour)
				b.Now = func() time.Time { return now }
			}
			b.respond(context.Background(), u)
			if e.calls != 0 || e.writeCalls != 0 {
				t.Fatal("invalid callback reached editor", e)
			}
		})
	}
}

func TestButtonNavigationAndWeekRequestUseExistingPipelines(t *testing.T) {
	b, q, _ := testBot(t)
	callbackServer(t, b)
	w := &weekQueue{queued: true}
	b.Week = w
	ctx := context.Background()
	for _, data := range []string{"v1:today", "v1:next:5", "v1:next:10", "v1:next:20", "v1:week", "v1:help"} {
		if parts := b.respond(ctx, callbackUpdate(b, 20, data)); len(parts) == 0 {
			t.Fatal("no reply", data)
		}
	}
	if q.calls != 4 || w.calls != 1 {
		t.Fatal(q.calls, w.calls)
	}
}
