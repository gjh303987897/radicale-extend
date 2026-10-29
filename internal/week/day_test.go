package week

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"radicale-extend/internal/store"
	"radicale-extend/internal/weekimage"
)

func TestDayRequestQueriesCivilDayAndSharesDeliveryCleanup(t *testing.T) {
	for _, tc := range []struct {
		zone  string
		now   time.Time
		hours int
	}{
		{"Asia/Shanghai", time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC), 24},
		{"America/New_York", time.Date(2026, 3, 8, 16, 0, 0, 0, time.UTC), 23},
		{"America/New_York", time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC), 25},
	} {
		t.Run(tc.now.String(), func(t *testing.T) {
			s, q, sender, now := testService(t)
			s.Config.Timezone = tc.zone
			*now = tc.now
			ctx := context.Background()
			renders := 0
			s.Render = func(_ context.Context, o weekimage.Options, e []weekimage.Event) (weekimage.Result, error) {
				renders++
				if o.Days != 1 {
					t.Error("day renderer not selected", o.Days)
				}
				return weekimage.Result{PNG: []byte("PNG"), Events: len(e)}, nil
			}
			if added, err := s.QueueDay(ctx, "same-key"); err != nil || !added {
				t.Fatal(added, err)
			}
			if added, err := s.QueueDay(ctx, "same-key"); err != nil || added {
				t.Fatal("request duplicated", added, err)
			}
			if _, err := s.QueueDay(ctx, "new-key"); !errors.Is(err, store.ErrWeekThrottled) {
				t.Fatal("day request not throttled", err)
			}
			if err := s.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if q.from.Hour() != 0 || q.to.Hour() != 0 || q.to.Sub(q.from) != time.Duration(tc.hours)*time.Hour || renders != 1 || sender.calls["smtp"] != 1 || sender.calls["telegram"] != 1 {
				t.Fatal(q.from, q.to, renders, sender.calls)
			}
			var title string
			var size int
			if err := s.Store.DB.QueryRow("SELECT title,length(png) FROM week_deliveries LIMIT 1").Scan(&title, &size); err != nil || !strings.HasPrefix(title, "今日日程") || size != 0 {
				t.Fatal(title, size, err)
			}
			// Separate view IDs/rate limits let a user request both formats.
			if added, err := s.QueueWeek(ctx, "same-key"); err != nil || !added {
				t.Fatal("day request blocked week", added, err)
			}
		})
	}
}

func TestDayExpiresAtLocalMidnightAndCleansLongRetries(t *testing.T) {
	s, q, sender, now := testService(t)
	loc, _ := time.LoadLocation(s.Config.Timezone)
	*now = time.Date(2026, 10, 30, 23, 59, 30, 0, loc)
	sender.failSMTP = true
	sender.retryAfterSMTP = 30 * time.Minute
	ctx := context.Background()
	if _, err := s.QueueDay(ctx, "late"); err != nil {
		t.Fatal(err)
	}
	var expires int64
	if err := s.Store.DB.QueryRow("SELECT expires FROM week_requests").Scan(&expires); err != nil || expires != time.Date(2026, 10, 31, 0, 0, 0, 0, loc).Unix() {
		t.Fatal(expires, err)
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	var size int
	if err := s.Store.DB.QueryRow("SELECT state,length(png) FROM week_deliveries WHERE channel='smtp'").Scan(&state, &size); err != nil || state != "expired" || size != 0 {
		t.Fatal(state, size, err)
	}
	if q.calls != 1 || sender.calls["smtp"] != 1 || sender.calls["telegram"] != 1 {
		t.Fatal("stale day image regenerated or resent", q.calls, sender.calls)
	}
	s.Config.SMTP.Enabled = false
	s.Config.Telegram.Enabled = false
	if _, err := s.QueueDay(ctx, "disabled"); !errors.Is(err, ErrNoChannels) {
		t.Fatal(err)
	}
}

func TestDayCancelsWhenTimezoneChangeMovesRequestedDate(t *testing.T) {
	s, q, sender, _ := testService(t)
	ctx := context.Background()
	if _, err := s.QueueDay(ctx, "timezone-change"); err != nil {
		t.Fatal(err)
	}
	s.Config.Timezone = "America/New_York"
	if err := s.generate(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.Store.DB.QueryRow("SELECT state FROM week_requests").Scan(&state); err != nil || state != "expired" || q.calls != 0 || len(sender.calls) != 0 {
		t.Fatal(state, q.calls, sender.calls, err)
	}
}
