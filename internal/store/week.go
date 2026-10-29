package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrWeekThrottled = errors.New("weekly image request throttled")

type WeekRequest struct {
	ID, StartDate, Kind, State   string
	Created, Due, Expires, Lease int64
	Attempts                     int
}
type WeekDelivery struct {
	RequestID, Channel, Title, Caption, Policy, State string
	PNG                                               []byte
	Attempts                                          int
	Due, Lease, Expires                               int64
	Kind                                              string
}

func (s *Store) QueueWeek(ctx context.Context, r WeekRequest) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM week_requests WHERE id=?", r.ID).Scan(&exists); err != nil {
		return false, err
	}
	if exists > 0 {
		return false, nil
	}
	if r.Kind == "manual" || r.Kind == "manual_day" {
		var recent int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM week_requests WHERE kind=? AND created>? AND created<=?", r.Kind, r.Created-60, r.Created).Scan(&recent); err != nil {
			return false, err
		}
		if recent > 0 {
			return false, ErrWeekThrottled
		}
	}
	var active int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM week_requests WHERE state IN ('pending','processing') OR id IN (SELECT request_id FROM week_deliveries WHERE state IN ('pending','processing'))").Scan(&active); err != nil {
		return false, err
	}
	if active >= 20 {
		return false, ErrWeekThrottled
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO week_requests(id,start_date,kind,created,due,expires) VALUES(?,?,?,?,?,?)", r.ID, r.StartDate, r.Kind, r.Created, r.Due, r.Expires)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) ClaimWeek(ctx context.Context, now time.Time) (WeekRequest, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return WeekRequest{}, err
	}
	defer tx.Rollback()
	var r WeekRequest
	err = tx.QueryRowContext(ctx, "SELECT id,start_date,kind,created,due,expires,attempts FROM week_requests WHERE (state='pending' AND due<=?) OR (state='processing' AND lease<=?) ORDER BY created LIMIT 1", now.Unix(), now.Unix()).Scan(&r.ID, &r.StartDate, &r.Kind, &r.Created, &r.Due, &r.Expires, &r.Attempts)
	if err != nil {
		return r, err
	}
	r.Lease = now.Add(3 * time.Minute).Unix()
	_, err = tx.ExecContext(ctx, "UPDATE week_requests SET state='processing',lease=? WHERE id=?", r.Lease, r.ID)
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}

func (s *Store) FinishWeek(ctx context.Context, r WeekRequest, state string, due time.Time, deliveries []WeekDelivery) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE week_requests SET state=?,attempts=?,due=?,lease=0 WHERE id=? AND state='processing' AND lease=?", state, r.Attempts, due.Unix(), r.ID, r.Lease)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("weekly generation lease lost")
	}
	for _, d := range deliveries {
		if len(d.PNG) > 4<<20 {
			return errors.New("weekly PNG limit exceeded")
		}
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO week_deliveries(request_id,channel,title,caption,png,policy,due) VALUES(?,?,?,?,?,?,?)", r.ID, d.Channel, d.Title, d.Caption, d.PNG, d.Policy, d.Due)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ClaimWeekDelivery(ctx context.Context, now time.Time) (WeekDelivery, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return WeekDelivery{}, err
	}
	defer tx.Rollback()
	var d WeekDelivery
	err = tx.QueryRowContext(ctx, "SELECT d.request_id,d.channel,d.title,d.caption,d.png,d.policy,d.attempts,r.expires,r.kind FROM week_deliveries d JOIN week_requests r ON r.id=d.request_id WHERE (d.state='pending' AND d.due<=?) OR (d.state='processing' AND d.lease<=?) ORDER BY d.due,d.channel LIMIT 1", now.Unix(), now.Unix()).Scan(&d.RequestID, &d.Channel, &d.Title, &d.Caption, &d.PNG, &d.Policy, &d.Attempts, &d.Expires, &d.Kind)
	if err != nil {
		return d, err
	}
	d.Lease = now.Add(time.Minute).Unix()
	_, err = tx.ExecContext(ctx, "UPDATE week_deliveries SET state='processing',lease=? WHERE request_id=? AND channel=?", d.Lease, d.RequestID, d.Channel)
	if err != nil {
		return d, err
	}
	return d, tx.Commit()
}

func (s *Store) FinishWeekDelivery(ctx context.Context, d WeekDelivery, state string, due time.Time) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE week_deliveries SET state=?,attempts=?,due=?,lease=0,png=CASE WHEN ?='pending' THEN png ELSE X'' END WHERE request_id=? AND channel=? AND state='processing' AND lease=?", state, d.Attempts, due.Unix(), state, d.RequestID, d.Channel, d.Lease)
	return err
}

func (s *Store) CleanWeeks(ctx context.Context, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Expiry is independent of the next retry time (e.g. a long retry_after).
	// Never interrupt a live sender/generator lease; it clears its own payload.
	if _, err = tx.ExecContext(ctx, "UPDATE week_deliveries SET state='expired',lease=0,png=X'' WHERE request_id IN (SELECT id FROM week_requests WHERE expires<=?) AND (state='pending' OR (state='processing' AND lease<=?))", now.Unix(), now.Unix()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE week_requests SET state='expired',lease=0 WHERE expires<=? AND (state='pending' OR (state='processing' AND lease<=?))", now.Unix(), now.Unix()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE week_deliveries SET png=X'' WHERE state NOT IN ('pending','processing') AND length(png)>0"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM week_requests WHERE created<? AND state NOT IN ('pending','processing') AND NOT EXISTS (SELECT 1 FROM week_deliveries WHERE request_id=week_requests.id AND state IN ('pending','processing'))", now.AddDate(0, 0, -7).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckpointWeekStorage releases the physical WAL file when image traffic is
// idle. Freed main-database pages are reused by later reports; no heavyweight
// full-database VACUUM is performed during notification processing.
func (s *Store) CheckpointWeekStorage(ctx context.Context, now time.Time) error {
	var active int
	err := s.DB.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM week_deliveries WHERE state IN ('pending','processing') AND length(png)>0)+(SELECT count(*) FROM week_requests WHERE state='processing' AND lease>?)+(SELECT count(*) FROM jobs WHERE state='processing' AND lease>?)", now.Unix(), now.Unix()).Scan(&active)
	if err != nil || active > 0 {
		return err
	}
	var busy, log, checkpointed int
	err = s.DB.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed)
	// Other connections can keep read snapshots. A busy result is normal: the
	// next hourly pass and SQLite's ordinary checkpoints will retry safely.
	return err
}

func WeekEmpty(err error) bool { return errors.Is(err, sql.ErrNoRows) }
