package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTelegramReplyOrderingAndAtomicCursor(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state.db"), filepath.Join(dir, "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, now := context.Background(), time.Now()
	markup := `{"inline_keyboard":[[{"text":"今天","callback_data":"today"}]]}`
	messages := []TelegramMessage{{Body: "one", ReplyMarkup: markup}, {Body: "two", ReplyMarkup: markup}}
	_, err = s.DB.Exec("CREATE TRIGGER reject_second_reply BEFORE INSERT ON telegram_replies WHEN NEW.part=1 BEGIN SELECT RAISE(ABORT,'test failure'); END")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTelegramMessages(ctx, "bot", 10, messages, now); err == nil {
		t.Fatal("transaction unexpectedly committed")
	}
	if offset, err := s.TelegramOffset(ctx, "bot"); err != nil || offset != 0 {
		t.Fatal("failed transaction advanced offset", offset, err)
	}
	if _, err = s.NextTelegramReply(ctx, "bot", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("partial reply left behind", err)
	}
	if _, err = s.DB.Exec("DROP TRIGGER reject_second_reply"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTelegramMessages(ctx, "bot", 10, messages, now); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTelegramUpdate(ctx, "bot", 10, []string{"duplicate"}, now); err != nil {
		t.Fatal(err)
	}
	r, err := s.NextTelegramReply(ctx, "bot", now)
	if err != nil || r.Part != 0 || r.Body != "one" || r.ReplyMarkup != markup {
		t.Fatal(r, err)
	}
	r.Attempts = 1
	if err = s.FinishTelegramReply(ctx, "bot", r, "pending", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextTelegramReply(ctx, "bot", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("later page bypassed retry_after", err)
	}
	if retry, err := s.NextTelegramReply(ctx, "bot", now.Add(time.Minute)); err != nil || retry.Body != "one" || retry.ReplyMarkup != markup || retry.Attempts != 1 {
		t.Fatal("retry lost message payload", retry, err)
	}
	if err = s.FinishTelegramReply(ctx, "bot", r, "done", now); err != nil {
		t.Fatal(err)
	}
	if r, err = s.NextTelegramReply(ctx, "bot", now); err != nil || r.Part != 1 || r.Body != "two" || r.ReplyMarkup != markup {
		t.Fatal(r, err)
	}
	if err = s.FinishTelegramReply(ctx, "bot", r, "failed", now); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err = s.DB.QueryRow("SELECT count(*) FROM telegram_replies WHERE body!='' OR reply_markup!=''").Scan(&retained); err != nil || retained != 0 {
		t.Fatal("terminal reply retained private text or buttons", retained, err)
	}
	if offset, err := s.TelegramOffset(ctx, "other-bot"); err != nil || offset != 0 {
		t.Fatal("bot offset leaked", offset, err)
	}
}

func TestTelegramReplyMarkupMigratesLegacyAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	state, queue := filepath.Join(dir, "state.db"), filepath.Join(dir, "queue.db")
	now, ctx := time.Unix(1800000000, 0), context.Background()
	legacy, err := sql.Open("sqlite", state)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE telegram_replies(scope TEXT NOT NULL,update_id INTEGER NOT NULL,part INTEGER NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending',attempts INTEGER NOT NULL DEFAULT 0,due INTEGER NOT NULL,PRIMARY KEY(scope,update_id,part)); PRAGMA user_version=1;`)
	if err == nil {
		_, err = legacy.Exec("INSERT INTO telegram_replies(scope,update_id,part,body,attempts,due) VALUES('bot',1,0,'old reply',2,?)", now.Unix())
	}
	legacy.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(state, queue)
	if err != nil {
		t.Fatal("legacy migration failed", err)
	}
	defer func() { s.Close() }()
	r, err := s.NextTelegramReply(ctx, "bot", now)
	if err != nil || r.Body != "old reply" || r.ReplyMarkup != "" || r.Attempts != 2 {
		t.Fatal("legacy pending reply damaged", r, err)
	}
	if err = s.FinishTelegramReply(ctx, "bot", r, "done", now); err != nil {
		t.Fatal(err)
	}
	markup := `{"inline_keyboard":[[{"text":"详情","callback_data":"event:a"}]]}`
	if err = s.SaveTelegramMessages(ctx, "bot", 2, []TelegramMessage{{Body: "new reply", ReplyMarkup: markup}}, now); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(state, queue)
	if err != nil {
		t.Fatal("migration was not idempotent", err)
	}
	r, err = s.NextTelegramReply(ctx, "bot", now)
	if err != nil || r.Body != "new reply" || r.ReplyMarkup != markup || r.UpdateID != 2 {
		t.Fatal("restart lost message payload", r, err)
	}
	if offset, err := s.TelegramOffset(ctx, "bot"); err != nil || offset != 3 {
		t.Fatal("restart lost cursor", offset, err)
	}
	var protocol, version int
	if err = s.Queue.QueryRow("SELECT version FROM protocol WHERE id=1").Scan(&protocol); err != nil || protocol != 1 {
		t.Fatal("Hook protocol changed", protocol, err)
	}
	if err = s.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatal("unexpected schema version", version, err)
	}
	if err = s.SaveTelegramUpdate(ctx, "other-bot", 3, []string{"plain"}, now); err != nil {
		t.Fatal(err)
	}
	if plain, err := s.NextTelegramReply(ctx, "other-bot", now); err != nil || plain.Body != "plain" || plain.ReplyMarkup != "" {
		t.Fatal("plain reply compatibility broken", plain, err)
	}
}
