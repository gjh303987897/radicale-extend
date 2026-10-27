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
	_, err = s.DB.Exec("CREATE TRIGGER reject_second_reply BEFORE INSERT ON telegram_replies WHEN NEW.part=1 BEGIN SELECT RAISE(ABORT,'test failure'); END")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTelegramUpdate(ctx, "bot", 10, []string{"one", "two"}, now); err == nil {
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
	if err = s.SaveTelegramUpdate(ctx, "bot", 10, []string{"one", "two"}, now); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTelegramUpdate(ctx, "bot", 10, []string{"duplicate"}, now); err != nil {
		t.Fatal(err)
	}
	r, err := s.NextTelegramReply(ctx, "bot", now)
	if err != nil || r.Part != 0 || r.Body != "one" {
		t.Fatal(r, err)
	}
	r.Attempts = 1
	if err = s.FinishTelegramReply(ctx, "bot", r, "pending", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextTelegramReply(ctx, "bot", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("later page bypassed retry_after", err)
	}
	if err = s.FinishTelegramReply(ctx, "bot", r, "done", now); err != nil {
		t.Fatal(err)
	}
	if r, err = s.NextTelegramReply(ctx, "bot", now); err != nil || r.Part != 1 || r.Body != "two" {
		t.Fatal(r, err)
	}
	if offset, err := s.TelegramOffset(ctx, "other-bot"); err != nil || offset != 0 {
		t.Fatal("bot offset leaked", offset, err)
	}
}
