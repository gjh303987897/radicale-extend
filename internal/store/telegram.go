package store

import (
	"context"
	"database/sql"
	"time"
)

// migrateTelegramReplyMarkup is additive so existing queued text replies remain
// deliverable. Rows must close before ALTER on the single SQLite connection.
func migrateTelegramReplyMarkup(tx *sql.Tx) error {
	rows, err := tx.Query("PRAGMA table_info(telegram_replies)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "reply_markup" {
			found = true
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if !found {
		_, err = tx.Exec("ALTER TABLE telegram_replies ADD COLUMN reply_markup TEXT NOT NULL DEFAULT ''")
	}
	return err
}

// Command state is separate from reminder jobs: replies do not require a DAV
// resource revision and must not block notification dispatch.
func (s *Store) TelegramOffset(ctx context.Context, scope string) (int64, error) {
	var offset int64
	err := s.DB.QueryRowContext(ctx, "SELECT offset FROM telegram_offsets WHERE scope=?", scope).Scan(&offset)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return offset, err
}

func (s *Store) SaveTelegramUpdate(ctx context.Context, scope string, id int64, replies []string, now time.Time) error {
	messages := make([]TelegramMessage, len(replies))
	for i, body := range replies {
		messages[i].Body = body
	}
	return s.SaveTelegramMessages(ctx, scope, id, messages, now)
}

type TelegramMessage struct {
	Body, ReplyMarkup string
}

// SaveTelegramMessages commits the update cursor and complete message payloads
// together, so a restart cannot drop the buttons while retaining the text.
func (s *Store) SaveTelegramMessages(ctx context.Context, scope string, id int64, replies []TelegramMessage, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var offset int64
	err = tx.QueryRowContext(ctx, "SELECT offset FROM telegram_offsets WHERE scope=?", scope).Scan(&offset)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if id < offset {
		return nil
	}
	for i, message := range replies {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO telegram_replies(scope,update_id,part,body,reply_markup,due) VALUES(?,?,?,?,?,?)", scope, id, i, message.Body, message.ReplyMarkup, now.Unix()); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO telegram_offsets(scope,offset) VALUES(?,?) ON CONFLICT(scope) DO UPDATE SET offset=max(offset,excluded.offset)", scope, id+1)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type TelegramReply struct {
	UpdateID          int64
	Part, Attempts    int
	Body, ReplyMarkup string
}

func (s *Store) NextTelegramReply(ctx context.Context, scope string, now time.Time) (TelegramReply, error) {
	var r TelegramReply
	var due int64
	err := s.DB.QueryRowContext(ctx, "SELECT update_id,part,body,reply_markup,attempts,due FROM telegram_replies WHERE scope=? AND state='pending' ORDER BY update_id,part LIMIT 1", scope).Scan(&r.UpdateID, &r.Part, &r.Body, &r.ReplyMarkup, &r.Attempts, &due)
	if err == nil && due > now.Unix() {
		return TelegramReply{}, sql.ErrNoRows
	}
	return r, err
}

func (s *Store) FinishTelegramReply(ctx context.Context, scope string, r TelegramReply, state string, due time.Time) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE telegram_replies SET state=?,attempts=?,due=?,body=CASE WHEN ?='pending' THEN body ELSE '' END,reply_markup=CASE WHEN ?='pending' THEN reply_markup ELSE '' END WHERE scope=? AND update_id=? AND part=?", state, r.Attempts, due.Unix(), state, state, scope, r.UpdateID, r.Part)
	return err
}
