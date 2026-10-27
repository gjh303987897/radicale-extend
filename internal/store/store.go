package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ DB, Queue *sql.DB }

func open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=1000; PRAGMA foreign_keys=ON;"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
func Open(state, queue string) (*Store, error) {
	a, _ := filepath.Abs(state)
	b, _ := filepath.Abs(queue)
	if a == b {
		return nil, errors.New("state and queue paths must differ")
	}
	db, err := open(state)
	if err != nil {
		return nil, err
	}
	q, err := open(queue)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db, q}
	if err = s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) migrate() error {
	for _, entry := range []struct {
		db     *sql.DB
		schema string
	}{{s.Queue, `
 CREATE TABLE IF NOT EXISTS protocol (id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL);
 INSERT OR IGNORE INTO protocol VALUES(1,1);
 CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT,payload TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending',attempts INTEGER NOT NULL DEFAULT 0,next_at INTEGER NOT NULL DEFAULT 0,lease INTEGER NOT NULL DEFAULT 0);
 CREATE INDEX IF NOT EXISTS events_due ON events(state,next_at);
 `}, {s.DB, `
 CREATE TABLE IF NOT EXISTS collections(path TEXT PRIMARY KEY,token TEXT NOT NULL DEFAULT '',initialized INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS resources(path TEXT PRIMARY KEY,collection TEXT NOT NULL,uid TEXT NOT NULL,etag TEXT NOT NULL,data TEXT NOT NULL,revision TEXT NOT NULL,semantic TEXT NOT NULL,error TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS overrides(collection TEXT NOT NULL,uid TEXT NOT NULL,mode TEXT NOT NULL,before_seconds INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(collection,uid));
 CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY,resource TEXT NOT NULL,collection TEXT NOT NULL,uid TEXT NOT NULL,instance TEXT NOT NULL,kind TEXT NOT NULL,channel TEXT NOT NULL,source TEXT NOT NULL,revision TEXT NOT NULL,at INTEGER NOT NULL,due INTEGER NOT NULL,expires INTEGER NOT NULL,title TEXT NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending',attempts INTEGER NOT NULL DEFAULT 0,lease INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '');
 CREATE INDEX IF NOT EXISTS jobs_due ON jobs(state,due);
 `}} {
		var version int
		if err := entry.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if version > 1 {
			return errors.New("unsupported database schema version")
		}
		tx, err := entry.db.Begin()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(entry.schema); err == nil {
			_, err = tx.Exec("PRAGMA user_version=1")
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	var protocol int
	if err := s.Queue.QueryRow("SELECT version FROM protocol WHERE id=1").Scan(&protocol); err != nil {
		return err
	}
	if protocol != 1 {
		return errors.New("unsupported Hook protocol version")
	}
	return nil
}
func (s *Store) Close() { s.DB.Close(); s.Queue.Close() }

type Resource struct{ Path, Collection, UID, ETag, Data, Revision, Semantic, Error string }

func (s *Store) Resource(path string) (Resource, error) {
	var r Resource
	err := s.DB.QueryRow("SELECT path,collection,uid,etag,data,revision,semantic,error FROM resources WHERE path=?", path).Scan(&r.Path, &r.Collection, &r.UID, &r.ETag, &r.Data, &r.Revision, &r.Semantic, &r.Error)
	return r, err
}
func (s *Store) Resources(col string) ([]Resource, error) {
	rows, err := s.DB.Query("SELECT path,collection,uid,etag,data,revision,semantic,error FROM resources WHERE collection=?", col)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Resource{}
	for rows.Next() {
		var r Resource
		if err = rows.Scan(&r.Path, &r.Collection, &r.UID, &r.ETag, &r.Data, &r.Revision, &r.Semantic, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Job struct {
	ID         string `json:"id"`
	Resource   string `json:"resource"`
	Collection string `json:"collection"`
	UID        string `json:"uid"`
	Instance   string `json:"instance"`
	Kind       string `json:"kind"`
	Channel    string `json:"channel"`
	Source     string `json:"source"`
	Revision   string `json:"-"`
	At         int64  `json:"at"`
	Due        int64  `json:"due"`
	Expires    int64  `json:"expires"`
	Title      string `json:"-"`
	Body       string `json:"-"`
	State      string `json:"state"`
	Attempts   int    `json:"attempts"`
}

func SaveJob(tx *sql.Tx, j Job) error {
	_, err := tx.Exec(`INSERT INTO jobs(id,resource,collection,uid,instance,kind,channel,source,revision,at,due,expires,title,body) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET resource=excluded.resource,revision=excluded.revision,title=excluded.title,body=excluded.body,expires=excluded.expires,due=CASE WHEN jobs.kind='change' OR jobs.state='cancelled' THEN excluded.due ELSE jobs.due END,state=CASE WHEN jobs.state='cancelled' OR (jobs.kind='change' AND jobs.revision!=excluded.revision) THEN 'pending' ELSE jobs.state END,attempts=CASE WHEN jobs.kind='change' AND jobs.revision!=excluded.revision THEN 0 ELSE jobs.attempts END`, j.ID, j.Resource, j.Collection, j.UID, j.Instance, j.Kind, j.Channel, j.Source, j.Revision, j.At, j.Due, j.Expires, j.Title, j.Body)
	return err
}
func (s *Store) Apply(r Resource, jobs []Job, changed bool, remove bool) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	valid := map[string]bool{}
	for _, j := range jobs {
		if j.Kind == "reminder" {
			valid[j.ID] = true
		}
	}
	rows, e := tx.Query("SELECT id FROM jobs WHERE resource=? AND state IN ('pending','retry','processing') AND kind='reminder'", r.Path)
	if e != nil {
		return e
	}
	stale := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		if !valid[id] {
			stale = append(stale, id)
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range stale {
		if _, err = tx.Exec("UPDATE jobs SET state='cancelled' WHERE id=?", id); err != nil {
			return err
		}
	}
	if remove {
		_, err = tx.Exec("DELETE FROM resources WHERE path=?", r.Path)
	} else {
		_, err = tx.Exec(`INSERT INTO resources(path,collection,uid,etag,data,revision,semantic,error) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET uid=excluded.uid,etag=excluded.etag,data=excluded.data,revision=excluded.revision,semantic=excluded.semantic,error=excluded.error`, r.Path, r.Collection, r.UID, r.ETag, r.Data, r.Revision, r.Semantic, r.Error)
	}
	if err != nil {
		return err
	}
	if !remove && !changed {
		if _, err = tx.Exec("UPDATE jobs SET revision=? WHERE resource=? AND kind='change' AND revision!='deleted' AND state IN ('pending','retry','processing')", r.Revision, r.Path); err != nil {
			return err
		}
	}
	if changed {
		if _, err = tx.Exec("UPDATE jobs SET state='cancelled' WHERE resource=? AND kind='change' AND state IN ('pending','retry','processing')", r.Path); err != nil {
			return err
		}
	}
	for _, j := range jobs {
		if err = SaveJob(tx, j); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) ClaimJob(ctx context.Context, now time.Time) (*Job, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var j Job
	err = tx.QueryRow(`SELECT id,resource,collection,uid,instance,kind,channel,source,revision,at,due,expires,title,body,state,attempts FROM jobs WHERE (state IN ('pending','retry') AND due<=?) OR (state='processing' AND lease<=?) ORDER BY due LIMIT 1`, now.Unix(), now.Unix()).Scan(&j.ID, &j.Resource, &j.Collection, &j.UID, &j.Instance, &j.Kind, &j.Channel, &j.Source, &j.Revision, &j.At, &j.Due, &j.Expires, &j.Title, &j.Body, &j.State, &j.Attempts)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE jobs SET state='processing',lease=? WHERE id=?", now.Add(time.Minute).Unix(), j.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &j, nil
}
func (s *Store) Finish(j *Job, state string, due time.Time, message string) error {
	_, err := s.DB.Exec("UPDATE jobs SET state=?,due=?,attempts=?,lease=0,error=? WHERE id=? AND revision=? AND state='processing'", state, due.Unix(), j.Attempts, message, j.ID, j.Revision)
	return err
}
func (s *Store) ListJobs(resource string) ([]Job, error) {
	rows, err := s.DB.Query("SELECT id,resource,collection,uid,instance,kind,channel,source,at,due,expires,state,attempts,revision FROM jobs WHERE resource=? ORDER BY CASE WHEN state IN ('pending','retry','processing') THEN 0 ELSE 1 END,due LIMIT 1000", resource)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var j Job
		if err = rows.Scan(&j.ID, &j.Resource, &j.Collection, &j.UID, &j.Instance, &j.Kind, &j.Channel, &j.Source, &j.At, &j.Due, &j.Expires, &j.State, &j.Attempts, &j.Revision); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

type Event struct {
	ID       int64
	Payload  string
	Attempts int
}

func (s *Store) ClaimEvent(now time.Time) (*Event, error) {
	tx, err := s.Queue.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var e Event
	err = tx.QueryRow("SELECT id,payload,attempts FROM events WHERE (state IN ('pending','retry') AND next_at<=?) OR (state='processing' AND lease<=?) ORDER BY id LIMIT 1", now.Unix(), now.Unix()).Scan(&e.ID, &e.Payload, &e.Attempts)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE events SET state='processing',lease=? WHERE id=?", now.Add(time.Minute).Unix(), e.ID); err != nil {
		return nil, err
	}
	return &e, tx.Commit()
}
func (s *Store) FinishEvent(e *Event, success bool, now time.Time) error {
	state := "done"
	if !success {
		state = "retry"
		e.Attempts++
		if e.Attempts >= 8 {
			state = "failed"
		}
	}
	_, err := s.Queue.Exec("UPDATE events SET state=?,attempts=?,next_at=?,lease=0 WHERE id=?", state, e.Attempts, now.Add(time.Minute).Unix(), e.ID)
	return err
}
