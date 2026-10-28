package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

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
