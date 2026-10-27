package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestLeaseRecoveryAndChannelIndependence(t *testing.T) {
	dir := t.TempDir()
	state, queue := filepath.Join(dir, "state.db"), filepath.Join(dir, "queue.db")
	s, err := Open(state, queue)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := Resource{Path: "/a/cal/a.ics", Collection: "/a/cal/", UID: "a", Revision: "v1"}
	jobs := []Job{{ID: "smtp", Resource: r.Path, Kind: "reminder", Channel: "smtp", Revision: "v1", At: now.Unix(), Due: now.Unix()}, {ID: "telegram", Resource: r.Path, Kind: "reminder", Channel: "telegram", Revision: "v1", At: now.Unix(), Due: now.Unix()}}
	if err = s.Apply(r, jobs, false, false); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimJob(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(state, queue)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := s.ClaimJob(context.Background(), now)
	if err != nil || other.ID == claimed.ID {
		t.Fatal("active lease reclaimed", err)
	}
	s.Finish(other, "success", now, "")
	again, err := s.ClaimJob(context.Background(), now.Add(2*time.Minute))
	if err != nil || again.ID != claimed.ID {
		t.Fatal("expired lease lost", err)
	}
	s.Finish(again, "success", now, "")
	if err = s.Apply(r, jobs, false, false); err != nil {
		t.Fatal(err)
	}
	_, err = s.ClaimJob(context.Background(), now.Add(3*time.Minute))
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("successful delivery regenerated", err)
	}
}
func TestProtocolRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state"), filepath.Join(dir, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	s.Queue.Exec("UPDATE protocol SET version=2")
	s.Close()
	if s, err = Open(filepath.Join(dir, "state"), filepath.Join(dir, "queue")); err == nil {
		s.Close()
		t.Fatal("future protocol accepted")
	}
}
