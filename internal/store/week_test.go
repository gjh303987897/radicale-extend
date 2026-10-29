package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func queueTestImage(t *testing.T, s *Store, now time.Time, id string, expires, due time.Time, png []byte) {
	t.Helper()
	ctx := context.Background()
	if added, err := s.QueueWeek(ctx, WeekRequest{ID: id, Kind: "daily", StartDate: "2026-10-29", Created: now.Unix(), Due: now.Unix(), Expires: expires.Unix()}); err != nil || !added {
		t.Fatal(added, err)
	}
	r, err := s.ClaimWeek(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishWeek(ctx, r, "done", now, []WeekDelivery{{Channel: "telegram", PNG: png, Due: due.Unix()}}); err != nil {
		t.Fatal(err)
	}
}

func TestWeekExpiryClearsDelayedRetriesButPreservesLiveLease(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state"), filepath.Join(dir, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	queueTestImage(t, s, now, "delayed", now.Add(time.Second), now.Add(24*time.Hour), []byte("retry image"))
	queueTestImage(t, s, now, "live", now.Add(time.Second), now, []byte("in flight"))
	queueTestImage(t, s, now, "future", now.Add(48*time.Hour), now.Add(24*time.Hour), []byte("needed for retry"))
	d, err := s.ClaimWeekDelivery(ctx, now)
	if err != nil || d.RequestID != "live" {
		t.Fatal(d, err)
	}
	if err = s.CleanWeeks(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertImage := func(id, state string, want int) {
		t.Helper()
		var gotState string
		var size int
		if err := s.DB.QueryRow("SELECT state,length(png) FROM week_deliveries WHERE request_id=?", id).Scan(&gotState, &size); err != nil || gotState != state || size != want {
			t.Fatal(id, gotState, size, err)
		}
	}
	assertImage("delayed", "expired", 0)
	assertImage("live", "processing", len("in flight"))
	assertImage("future", "pending", len("needed for retry"))
	if err = s.CleanWeeks(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertImage("live", "expired", 0)
	// An old worker must not resurrect an expired payload when acknowledging.
	if err = s.FinishWeekDelivery(ctx, d, "pending", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertImage("live", "expired", 0)
	assertImage("future", "pending", len("needed for retry"))
}

func TestWeekImageStorageReusesPagesAndTruncatesIdleWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	s, err := Open(path, filepath.Join(dir, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	var limit int
	if err = s.DB.QueryRow("PRAGMA journal_size_limit").Scan(&limit); err != nil || limit != 8<<20 {
		t.Fatal(limit, err)
	}
	image := bytes.Repeat([]byte("x"), 1<<20)
	firstPages := 0
	for n := 0; n < 4; n++ {
		queueTestImage(t, s, now, fmt.Sprintf("reuse-%d", n), now.Add(time.Hour), now, image)
		d, err := s.ClaimWeekDelivery(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.FinishWeekDelivery(ctx, d, "done", now); err != nil {
			t.Fatal(err)
		}
		var pages int
		if err = s.DB.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			firstPages = pages
		}
		if pages > firstPages+16 {
			t.Fatalf("image pages accumulated: first %d, now %d", firstPages, pages)
		}
		if err = s.CheckpointWeekStorage(ctx, now); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path + "-wal")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err == nil && info.Size() != 0 {
			t.Fatalf("idle WAL retained %d bytes", info.Size())
		}
	}
}

func TestWeekQueueLeasesAndImageRetention(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state"), filepath.Join(dir, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	r := WeekRequest{ID: "manual-1", Kind: "manual", StartDate: "2026-10-29", Created: now.Unix(), Due: now.Unix(), Expires: now.Add(time.Hour).Unix()}
	if added, err := s.QueueWeek(ctx, r); err != nil || !added {
		t.Fatal(added, err)
	}
	if added, err := s.QueueWeek(ctx, r); err != nil || added {
		t.Fatal("request replay duplicated", added, err)
	}
	r.ID = "manual-2"
	if _, err = s.QueueWeek(ctx, r); !errors.Is(err, ErrWeekThrottled) {
		t.Fatal("manual requests not throttled", err)
	}
	claimed, err := s.ClaimWeek(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimWeek(ctx, now.Add(time.Minute)); !WeekEmpty(err) {
		t.Fatal("active generation lease reclaimed", err)
	}
	recovered, err := s.ClaimWeek(ctx, now.Add(4*time.Minute))
	if err != nil || recovered.ID != claimed.ID {
		t.Fatal(recovered, err)
	}
	if err = s.FinishWeek(ctx, claimed, "done", now, nil); err == nil {
		t.Fatal("old lease committed")
	}
	image := []byte("image-bytes")
	if err = s.FinishWeek(ctx, recovered, "done", now, []WeekDelivery{{Channel: "smtp", PNG: image, Due: now.Unix()}, {Channel: "telegram", PNG: image, Due: now.Unix()}}); err != nil {
		t.Fatal(err)
	}
	d, err := s.ClaimWeekDelivery(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.ClaimWeekDelivery(ctx, now)
	if err != nil || other.Channel == d.Channel {
		t.Fatal("channel lease isolation failed", other, err)
	}
	if err = s.FinishWeekDelivery(ctx, d, "done", now); err != nil {
		t.Fatal(err)
	}
	var size int
	if err = s.DB.QueryRow("SELECT length(png) FROM week_deliveries WHERE request_id=? AND channel=?", d.RequestID, d.Channel).Scan(&size); err != nil || size != 0 {
		t.Fatal("successful image retained", size, err)
	}
	if err = s.FinishWeekDelivery(ctx, other, "failed", now); err != nil {
		t.Fatal(err)
	}
	if err = s.CleanWeeks(ctx, now.AddDate(0, 0, 8)); err != nil {
		t.Fatal(err)
	}
	var count int
	s.DB.QueryRow("SELECT count(*) FROM week_deliveries").Scan(&count)
	if count != 0 {
		t.Fatal("old image metadata not cleaned", count)
	}
}
