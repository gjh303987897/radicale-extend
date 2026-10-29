package telegrambot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"radicale-extend/internal/config"
	"radicale-extend/internal/notify"
	"radicale-extend/internal/service"
	"radicale-extend/internal/store"
)

type query struct {
	from, to     time.Time
	limit, calls int
	events       []service.AgendaEvent
	err          error
}

func (q *query) Agenda(_ context.Context, from, to time.Time, limit int) ([]service.AgendaEvent, error) {
	q.from, q.to, q.limit = from, to, limit
	q.calls++
	return q.events, q.err
}

type sender struct {
	messages []string
	err      error
}

func (s *sender) Send(_ context.Context, j store.Job) error {
	s.messages = append(s.messages, j.Body)
	return s.err
}

func testBot(t *testing.T) (*Bot, *query, *sender) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"), filepath.Join(dir, "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	c := &config.Config{Timezone: "Asia/Shanghai"}
	c.Telegram.ChatID = "123"
	c.Telegram.Token.Value = "fake-secret"
	c.Telegram.Enabled = true
	c.Telegram.CommandsEnabled = true
	q, s := &query{}, &sender{}
	now := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	b := &Bot{Config: c, Store: db, Agenda: q, Sender: s, Now: func() time.Time { return now }, scope: "test-bot", username: "test_bot", botID: 456, HTTP: &http.Client{Timeout: time.Second}}
	return b, q, s
}
func update(id int64, text, kind string, chat int64) Update {
	data, _ := json.Marshal(map[string]any{"update_id": id, "message": map[string]any{"text": text, "chat": map[string]any{"id": chat, "type": kind}, "from": map[string]any{"id": chat}}})
	var u Update
	_ = json.Unmarshal(data, &u)
	return u
}
func TestPrivateCommandsTimeBoundsAndFormatting(t *testing.T) {
	b, q, _ := testBot(t)
	q.events = []service.AgendaEvent{{Summary: "<planning> 😀", Start: b.Now(), End: b.Now().Add(time.Hour), Location: "room"}, {Summary: "secret", Location: "secret room", Start: b.Now(), End: b.Now().AddDate(0, 0, 2), AllDay: true, Hidden: true}}
	result := strings.Join(b.reply(context.Background(), update(1, "/today@test_bot", "private", 123)), "\n")
	if q.from.Hour() != 0 || q.to.Sub(q.from) != 24*time.Hour || q.limit != 101 || !strings.Contains(result, "09:00") || strings.Contains(result, "secret") || !strings.Contains(result, "全天") || !strings.Contains(result, "<planning>") {
		t.Fatal(result, q)
	}
	b.reply(context.Background(), update(2, "/next", "private", 123))
	if q.limit != 5 || !q.from.Equal(b.Now()) || q.to.Sub(q.from) != 366*24*time.Hour {
		t.Fatal(q)
	}
	q.calls = 0
	for _, u := range []Update{update(3, "/today", "private", 999), update(4, "/next", "group", 123), update(5, "/today@other_bot", "private", 123), update(6, "/today extra", "private", 123)} {
		if got := b.reply(context.Background(), u); len(got) != 0 {
			t.Fatal("unauthorized command replied", got)
		}
	}
	if q.calls != 0 {
		t.Fatal("unauthorized user reached DAV")
	}
	stale := update(7, "/today", "private", 123)
	stale.Message.Date = b.Now().Add(-11 * time.Minute).Unix()
	if len(b.reply(context.Background(), stale)) != 0 {
		t.Fatal("stale command replied")
	}
	q.events = nil
	if got := strings.Join(b.reply(context.Background(), update(8, "/today", "private", 123)), ""); !strings.Contains(got, "没有日程") {
		t.Fatal(got)
	}
	q.err = errors.New("private DAV details")
	if got := strings.Join(b.reply(context.Background(), update(9, "/next", "private", 123)), ""); !strings.Contains(got, "查询失败") || strings.Contains(got, "private DAV") {
		t.Fatal(got)
	}
}

func TestTodayUsesCivilDayAcrossDST(t *testing.T) {
	b, q, _ := testBot(t)
	b.Config.Timezone = "America/New_York"
	b.Now = func() time.Time { return time.Date(2026, 3, 8, 16, 0, 0, 0, time.UTC) }
	b.reply(context.Background(), update(1, "/today", "private", 123))
	if q.to.Sub(q.from) != 23*time.Hour {
		t.Fatal(q.from, q.to)
	}
}

func TestPollingOutboxRetryAndRestart(t *testing.T) {
	b, q, s := testBot(t)
	q.events = []service.AgendaEvent{{Summary: "event", Start: b.Now(), End: b.Now().Add(time.Hour)}}
	var seenOffset int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&args)
		if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
			t.Error("wrong API")
		}
		_ = json.Unmarshal(args["offset"], &seenOffset)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []Update{update(10, "/next", "private", 123), update(11, "/today", "private", 999)}})
	}))
	defer server.Close()
	b.Config.Telegram.APIURL = server.URL
	if err := b.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seenOffset != 0 || q.calls != 1 {
		t.Fatal(seenOffset, q.calls)
	}
	// Offset and reply survive closing and reopening the same database.
	var state, queue string
	rows, err := b.Store.DB.Query("PRAGMA database_list")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int
		var name, file string
		_ = rows.Scan(&seq, &name, &file)
		if name == "main" {
			state = file
		}
	}
	rows.Close()
	queue = filepath.Join(filepath.Dir(state), "queue.db")
	b.Store.Close()
	if b.Store, err = store.Open(state, queue); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Store.Close)
	if err = b.poll(context.Background()); err != nil || seenOffset != 12 || q.calls != 1 {
		t.Fatal(err, seenOffset, q.calls)
	}
	s.err = &notify.Error{RetryAfter: 30 * time.Second, Message: "limited"}
	if err = b.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = b.flush(context.Background()); err != nil || len(s.messages) != 1 {
		t.Fatal("retry_after ignored", err, len(s.messages))
	}
	now := b.Now().Add(31 * time.Second)
	b.Now = func() time.Time { return now }
	s.err = nil
	if err = b.flush(context.Background()); err != nil || len(s.messages) != 2 {
		t.Fatal(err, len(s.messages))
	}
	if err = b.flush(context.Background()); err != nil || len(s.messages) != 2 {
		t.Fatal("reply sent twice", err)
	}
	var stored string
	_ = b.Store.DB.QueryRow("SELECT body FROM telegram_replies").Scan(&stored)
	if stored != "" {
		t.Fatal("sent private reply retained")
	}
}

func TestSetupMenuWebhookAndRedaction(t *testing.T) {
	b, _, _ := testBot(t)
	webhook := false
	menu := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result any
		switch {
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			url := ""
			if webhook {
				url = "https://existing.example"
			}
			result = map[string]any{"url": url}
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			result = map[string]any{"id": 456, "username": "test_bot"}
		case strings.HasSuffix(r.URL.Path, "/setMyCommands"):
			var body struct {
				Scope struct {
					Type   string
					ChatID int64 `json:"chat_id"`
				}
				Commands []struct {
					Command string `json:"command"`
				}
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Scope.Type != "chat" || body.Scope.ChatID != 123 || len(body.Commands) != 10 {
				t.Error("invalid menu scope", body)
			}
			names := map[string]bool{}
			for _, command := range body.Commands {
				names[command.Command] = true
			}
			if !names["event"] || !names["remind"] || !names["week"] || !names["day"] || names["reminders"] {
				t.Error("menu must advertise canonical event/remind commands", names)
			}
			menu = true
			result = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	b.Config.Telegram.APIURL = server.URL
	if err := b.setup(context.Background()); err != nil || !menu {
		t.Fatal(err, menu)
	}
	webhook = true
	if err := b.setup(context.Background()); err == nil {
		t.Fatal("existing webhook overridden")
	}
	server.Close()
	if err := b.call(context.Background(), "getMe", nil, nil); err == nil || strings.Contains(err.Error(), "fake-secret") {
		t.Fatal("unsafe error", err)
	}
}

func TestLongRepliesPaginateAndStop(t *testing.T) {
	b, q, _ := testBot(t)
	for i := 0; i < 101; i++ {
		q.events = append(q.events, service.AgendaEvent{Start: b.Now(), Summary: strings.Repeat("😀", 300), Location: strings.Repeat("x", 200)})
	}
	parts := b.reply(context.Background(), update(1, "/today", "private", 123))
	if len(parts) < 2 {
		t.Fatal("not paginated")
	}
	for _, p := range parts {
		if len([]rune(p)) > 1500 {
			t.Fatal("oversize reply")
		}
	}
	if !strings.Contains(parts[len(parts)-1], "100 个") {
		t.Fatal("missing truncation notice")
	}
	b.Config.Telegram.CommandsEnabled = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.Run(ctx)
}

func TestPollingCancellationAndRateLimit(t *testing.T) {
	b, _, _ := testBot(t)
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		switch {
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"url": ""}})
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": 456, "username": "test_bot"}})
		case strings.HasSuffix(r.URL.Path, "/setMyCommands"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			close(started)
			<-r.Context().Done()
		default:
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 429, "parameters": map[string]int{"retry_after": 90}})
		}
	}))
	defer server.Close()
	b.Config.Telegram.APIURL = server.URL
	err := b.call(context.Background(), "limited", nil, nil)
	var limited *notify.Error
	if !errors.As(err, &limited) || limited.RetryAfter != 90*time.Second || limited.Permanent {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); b.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("poll never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll did not stop after cancellation")
	}
}
