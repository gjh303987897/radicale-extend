package store

import (
	"context"
	"database/sql"
	"time"
)

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
	for i, body := range replies {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO telegram_replies(scope,update_id,part,body,due) VALUES(?,?,?,?,?)", scope, id, i, body, now.Unix()); err != nil {
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
	UpdateID       int64
	Part, Attempts int
	Body           string
}

func (s *Store) NextTelegramReply(ctx context.Context, scope string, now time.Time) (TelegramReply, error) {
	var r TelegramReply
	var due int64
	err := s.DB.QueryRowContext(ctx, "SELECT update_id,part,body,attempts,due FROM telegram_replies WHERE scope=? AND state='pending' ORDER BY update_id,part LIMIT 1", scope).Scan(&r.UpdateID, &r.Part, &r.Body, &r.Attempts, &due)
	if err == nil && due > now.Unix() {
		return TelegramReply{}, sql.ErrNoRows
	}
	return r, err
}

func (s *Store) FinishTelegramReply(ctx context.Context, scope string, r TelegramReply, state string, due time.Time) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE telegram_replies SET state=?,attempts=?,due=?,body=CASE WHEN ?='pending' THEN body ELSE '' END WHERE scope=? AND update_id=? AND part=?", state, r.Attempts, due.Unix(), state, scope, r.UpdateID, r.Part)
	return err
}
