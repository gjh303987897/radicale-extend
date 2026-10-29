package telegrambot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"radicale-extend/internal/notify"
	"radicale-extend/internal/store"
)

func TestInteractivePollingDeliveryRetryRestartAndReplay(t *testing.T) {
	b, e, id := selectedBot(t)
	ctx := context.Background()
	updates := []Update{update(100, "/next", "private", 123)}
	var sent []store.TelegramMessage
	acks := 0
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var args struct {
				Allowed []string `json:"allowed_updates"`
			}
			json.NewDecoder(r.Body).Decode(&args)
			if strings.Join(args.Allowed, ",") != "message,callback_query" {
				t.Error("callbacks not requested", args)
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": updates})
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			acks++
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var args struct {
				Text   string          `json:"text"`
				Markup json.RawMessage `json:"reply_markup"`
			}
			json.NewDecoder(r.Body).Decode(&args)
			sent = append(sent, store.TelegramMessage{Body: args.Text, ReplyMarkup: string(args.Markup)})
			if fail {
				fail = false
				w.WriteHeader(429)
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 429, "parameters": map[string]int{"retry_after": 30}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]int{"message_id": 44}})
		default:
			t.Error("unexpected API method")
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	b.Config.Telegram.APIURL = srv.URL
	b.Sender = notify.New(b.Config)
	if err := b.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.flush(ctx); err != nil || len(sent) != 1 {
		t.Fatal(err, len(sent))
	}
	if strings.Contains(sent[0].Body, "ID：") || len(keyboard(t, sent[0])) == 0 {
		t.Fatal("list lost interactive formatting", sent[0])
	}
	var state string
	if err := b.Store.DB.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	b.Store.Close()
	db, err := store.Open(state, filepath.Join(filepath.Dir(state), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b.Store = db
	if err = b.flush(ctx); err != nil || len(sent) != 1 {
		t.Fatal("restart ignored retry_after", err, len(sent))
	}
	now := b.Now().Add(31 * time.Second)
	b.Now = func() time.Time { return now }
	if err = b.flush(ctx); err != nil || len(sent) != 2 || sent[0] != sent[1] {
		t.Fatal("restart changed queued buttons", err, sent)
	}
	var retained int
	if err = db.DB.QueryRow("SELECT count(*) FROM telegram_replies WHERE body!='' OR reply_markup!=''").Scan(&retained); err != nil || retained != 0 {
		t.Fatal("private reply or buttons retained", retained, err)
	}
	updates = []Update{callbackUpdate(b, 101, "v1:event:"+id)}
	if err = b.poll(ctx); err != nil || e.calls != 1 || e.writeCalls != 0 || acks != 1 {
		t.Fatal("read callback failed", err, e, acks)
	}
	updates = []Update{callbackUpdate(b, 102, "v1:set:"+id+":t:15m")}
	if err = b.poll(ctx); err != nil || e.writeCalls != 1 || e.before != 900 || e.series {
		t.Fatal("write callback failed", err, e)
	}
	if err = b.poll(ctx); err != nil || e.writeCalls != 1 || acks != 2 {
		t.Fatal("saved callback replayed", err, e.writeCalls, acks)
	}
	if offset, err := db.TelegramOffset(ctx, b.scope); err != nil || offset != 103 {
		t.Fatal(offset, err)
	}
}
