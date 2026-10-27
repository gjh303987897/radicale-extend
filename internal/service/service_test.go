package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"radicale-extend/internal/config"
	"radicale-extend/internal/dav"
	"radicale-extend/internal/notify"
	"radicale-extend/internal/store"
	"strings"
	"testing"
	"time"
)

func testService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	text := "dav:\n  url: http://localhost:5232/\n  username: alice\nstorage:\n  state: " + filepath.ToSlash(filepath.Join(dir, "state")) + "\n  queue: " + filepath.ToSlash(filepath.Join(dir, "queue")) + "\n"
	os.WriteFile(p, []byte(text), 0600)
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	c.SMTP.Enabled = true
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Store.Close)
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	return s
}
func object(path, start, summary string) dav.Object {
	return dav.Object{Path: path, ETag: "\"1\"", Data: "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTART:" + start + "\r\nSUMMARY:" + summary + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"}
}
func TestBaselineRetryAndMoveProjection(t *testing.T) {
	s := testService(t)
	o := object("/alice/work/a.ics", "20261028T010000Z", "A")
	if err := s.project(o, "/alice/work/", true); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.Store.ListJobs(o.Path)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "reminder" {
		t.Fatal(jobs, err)
	}
	now := s.Now().Add(time.Hour)
	job, err := s.Store.ClaimJob(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	job.Attempts = 2
	retryAt := now.Add(30 * time.Minute)
	s.Store.Finish(job, "retry", retryAt, "test")
	if err = s.project(o, "/alice/work/", true); err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.Store.ListJobs(o.Path)
	if jobs[0].State != "retry" || jobs[0].Due != retryAt.Unix() {
		t.Fatal("refresh reset retry backoff", jobs[0])
	}
	moved := o
	moved.Path = "/alice/work/moved.ics"
	if err = s.project(moved, "/alice/work/", true); err != nil {
		t.Fatal(err)
	}
	if err = s.remove(o.Path, true); err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.Store.ListJobs(moved.Path)
	if len(jobs) != 1 || jobs[0].State != "retry" {
		t.Fatal("MOVE lost reminders", jobs)
	}
}
func TestChangeDedupeAndDeletion(t *testing.T) {
	s := testService(t)
	o := object("/alice/work/a.ics", "20261028T010000Z", "A")
	s.project(o, "/alice/work/", true)
	changed := object(o.Path, "20261028T020000Z", "Changed")
	if err := s.project(changed, "/alice/work/", false); err != nil {
		t.Fatal(err)
	}
	s.project(changed, "/alice/work/", false)
	metadataOnly := changed
	metadataOnly.Data = strings.Replace(changed.Data, "END:VEVENT", "DTSTAMP:20261028T000001Z\r\nEND:VEVENT", 1)
	if err := s.project(metadataOnly, "/alice/work/", false); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Store.ListJobs(o.Path)
	active, changes := 0, 0
	for _, j := range jobs {
		if j.State == "pending" {
			active++
			if j.Kind == "change" {
				changes++
				if j.Revision != Revision(metadataOnly.Data) {
					t.Fatal("metadata refresh left a stale change version")
				}
			}
		}
	}
	if active != 2 || changes != 1 {
		t.Fatal("duplicate or stale jobs", jobs)
	}
	if err := s.remove(o.Path, false); err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.Store.ListJobs(o.Path)
	for _, j := range jobs {
		if j.Kind == "reminder" && j.State == "pending" {
			t.Fatal("deletion retained reminder")
		}
	}
}
func TestAdvanceBeyondHorizonAndOff(t *testing.T) {
	s := testService(t)
	o := object("/alice/work/a.ics", "20261207T000000Z", "A")
	s.Store.DB.Exec("INSERT INTO overrides VALUES('/alice/work/','a','before',?)", 40*86400)
	if err := s.project(o, "/alice/work/", true); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Store.ListJobs(o.Path)
	if len(jobs) != 1 || jobs[0].At != s.Now().Unix() {
		t.Fatal("advance beyond horizon lost", jobs)
	}
	s.Store.DB.Exec("UPDATE overrides SET mode='off'")
	s.project(o, "/alice/work/", true)
	jobs, _ = s.Store.ListJobs(o.Path)
	if jobs[0].State != "cancelled" {
		t.Fatal("off did not cancel")
	}
}
func TestPermanentFailureAndRateLimit(t *testing.T) {
	s := testService(t)
	r := store.Resource{Path: "/alice/work/a.ics", Collection: "/alice/work/", UID: "a", Revision: "v"}
	now := s.Now()
	j := store.Job{ID: "job", Resource: r.Path, Kind: "reminder", Channel: "smtp", Revision: "v", Due: now.Unix()}
	s.Store.Apply(r, []store.Job{j}, false, false)
	claimed, err := s.Store.ClaimJob(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.retry(claimed, &notify.Error{RetryAfter: 17 * time.Second, Message: "limited"}, now); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Store.ListJobs(r.Path)
	if jobs[0].Due != now.Add(17*time.Second).Unix() || jobs[0].Attempts != 1 {
		t.Fatal(jobs)
	}
	claimed, err = s.Store.ClaimJob(context.Background(), now.Add(18*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	s.retry(claimed, &notify.Error{Permanent: true, Message: "invalid recipient"}, now)
	jobs, _ = s.Store.ListJobs(r.Path)
	if jobs[0].State != "failed" {
		t.Fatal(jobs)
	}
	if _, err = s.Store.ClaimJob(context.Background(), now.Add(time.Hour)); err == nil || errors.Is(err, context.Canceled) {
		t.Fatal("terminal job claimed", err)
	}
}
