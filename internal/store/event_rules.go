package store

import (
	"context"
	"time"
)

type ReminderRule struct {
	Collection, UID, Instance, Channel, Mode string
	BeforeSeconds                            int64
}

func (s *Store) ReminderRules(col, uid string) ([]ReminderRule, error) {
	rows, err := s.DB.Query("SELECT instance,channel,mode,before_seconds FROM channel_reminders WHERE collection=? AND uid=?", col, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReminderRule{}
	for rows.Next() {
		r := ReminderRule{Collection: col, UID: uid}
		if err = rows.Scan(&r.Instance, &r.Channel, &r.Mode, &r.BeforeSeconds); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type EventReference struct {
	ID, Resource, Collection, UID, Instance, ETag string
	Expires                                       int64
}

func (s *Store) SaveEventReferences(ctx context.Context, scope string, refs []EventReference, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM telegram_event_refs WHERE expires<?", now.Unix()); err != nil {
		return err
	}
	for _, r := range refs {
		_, err = tx.ExecContext(ctx, "INSERT INTO telegram_event_refs(scope,id,resource,collection,uid,instance,etag,expires) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(scope,id) DO UPDATE SET resource=excluded.resource,etag=excluded.etag,expires=excluded.expires", scope, r.ID, r.Resource, r.Collection, r.UID, r.Instance, r.ETag, r.Expires)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) EventReference(ctx context.Context, scope, id string, now time.Time) (EventReference, error) {
	var r EventReference
	err := s.DB.QueryRowContext(ctx, "SELECT id,resource,collection,uid,instance,etag,expires FROM telegram_event_refs WHERE scope=? AND id=? AND expires>?", scope, id, now.Unix()).Scan(&r.ID, &r.Resource, &r.Collection, &r.UID, &r.Instance, &r.ETag, &r.Expires)
	return r, err
}
