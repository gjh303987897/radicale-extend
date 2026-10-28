package week

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"radicale-extend/internal/config"
	"radicale-extend/internal/notify"
	"radicale-extend/internal/service"
	"radicale-extend/internal/store"
	"radicale-extend/internal/weekimage"
)

type source struct {
	events   []service.AgendaEvent
	from, to time.Time
	calls    int
}

func (q *source) WeeklyAgenda(_ context.Context, from, to time.Time, _ int) ([]service.AgendaEvent, error) {
	q.calls++
	q.from, q.to = from, to
	return q.events, nil
}

type images struct {
	calls    map[string]int
	failSMTP bool
}

func (i *images) SendImage(_ context.Context, j store.Job, _ []byte) error {
	i.calls[j.Channel]++
	if j.Channel == "smtp" && i.failSMTP {
		return &notify.Error{RetryAfter: 30 * time.Second, Message: "temporary"}
	}
	return nil
}
func testService(t *testing.T) (*Service, *source, *images, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	data := "dav:\n  url: http://127.0.0.1:5232/\n  username: alice\nstorage:\n  state: " + filepath.ToSlash(filepath.Join(dir, "state")) + "\n  queue: " + filepath.ToSlash(filepath.Join(dir, "queue")) + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Weekly.Enabled = false
	c.SMTP.Enabled = true
	c.Telegram.Enabled = true
	db, err := store.Open(c.Storage.State, c.Storage.Queue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	q := &source{events: []service.AgendaEvent{{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Summary: "Private", Calendar: "/alice/work/", CalendarName: "Work"}}}
	i := &images{calls: map[string]int{}}
	s := &Service{Config: c, Store: db, Source: q, Sender: i, Now: func() time.Time { return now }, Render: func(_ context.Context, _ weekimage.Options, e []weekimage.Event) (weekimage.Result, error) {
		return weekimage.Result{PNG: []byte("PNG"), Events: len(e)}, nil
	}}
	return s, q, i, &now
}
func TestManualImageChannelRetriesSurviveRestart(t *testing.T) {
	s, q, sender, now := testService(t)
	ctx := context.Background()
	sender.failSMTP = true
	renders := 0
	s.Render = func(_ context.Context, _ weekimage.Options, e []weekimage.Event) (weekimage.Result, error) {
		renders++
		return weekimage.Result{PNG: []byte("PNG"), Events: len(e)}, nil
	}
	if _, err := s.QueueWeek(ctx, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if q.calls != 1 || renders != 1 || sender.calls["smtp"] != 1 || sender.calls["telegram"] != 1 {
		t.Fatal(q.calls, renders, sender.calls)
	}
	if q.from.Hour() != 0 || q.to.Sub(q.from) != 7*24*time.Hour {
		t.Fatal(q.from, q.to)
	}
	*now = now.Add(10 * time.Second)
	if err := s.Step(ctx); err != nil || sender.calls["smtp"] != 1 {
		t.Fatal("retry_after ignored", err, sender.calls)
	}
	s.Store.Close()
	db, err := store.Open(s.Config.Storage.State, s.Config.Storage.Queue)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s.Store = db
	*now = now.Add(21 * time.Second)
	sender.failSMTP = false
	if err = s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.calls["smtp"] != 2 || sender.calls["telegram"] != 1 || renders != 1 {
		t.Fatal("restart duplicated a channel or render", sender.calls, renders)
	}
	var bytes int
	s.Store.DB.QueryRow("SELECT sum(length(png)) FROM week_deliveries").Scan(&bytes)
	if bytes != 0 {
		t.Fatal("terminal images not cleared", bytes)
	}
}
func TestDailyScheduleTimezoneDedupCatchupAndManualDisabled(t *testing.T) {
	s, _, _, now := testService(t)
	ctx := context.Background()
	s.Config.Weekly.Enabled = true
	*now = now.Add(-time.Minute)
	if err := s.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM week_requests").Scan(&count)
	if count != 0 {
		t.Fatal("scheduled before 08:00")
	}
	*now = now.Add(time.Minute)
	if err := s.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	s.lastScheduled = ""
	if err := s.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	s.Store.DB.QueryRow("SELECT count(*) FROM week_requests").Scan(&count)
	if count != 1 {
		t.Fatal("repeated 08:00 daily job", count)
	}
	*now = now.AddDate(0, 0, 1).Add(2 * time.Hour)
	s.lastScheduled = ""
	if err := s.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	s.Store.DB.QueryRow("SELECT count(*) FROM week_requests").Scan(&count)
	if count != 1 {
		t.Fatal("missed report sent after catchup window", count)
	}
	s.Config.SMTP.Enabled = false
	s.Config.Telegram.Enabled = false
	if _, err := s.QueueWeek(ctx, "none"); !errors.Is(err, ErrNoChannels) {
		t.Fatal(err)
	}
}
func TestChannelFilteringPolicyChangeAndDSTRange(t *testing.T) {
	s, q, sender, now := testService(t)
	ctx := context.Background()
	s.Config.Calendars = []config.Calendar{{Path: "/alice/work/", Channels: []string{"smtp"}, HideDetails: true}}
	renders := [][]weekimage.Event{}
	s.Render = func(_ context.Context, _ weekimage.Options, e []weekimage.Event) (weekimage.Result, error) {
		renders = append(renders, e)
		return weekimage.Result{PNG: []byte("PNG"), Events: len(e)}, nil
	}
	if _, err := s.QueueWeek(ctx, "filtered"); err != nil {
		t.Fatal(err)
	}
	if err := s.generate(ctx); err != nil {
		t.Fatal(err)
	}
	if len(renders) != 2 || len(renders[0]) != 1 || len(renders[1]) != 0 || !renders[0][0].Hidden || renders[0][0].Title == "Private" {
		t.Fatal("calendar channel privacy filter bypassed", renders)
	}
	s.Config.Telegram.ChatID = "changed-recipient"
	if err := s.dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 0 {
		t.Fatal("old image sent after privacy/recipient policy change", sender.calls)
	}
	s.Config.Timezone = "America/New_York"
	s.Config.Calendars = nil
	loc, _ := time.LoadLocation(s.Config.Timezone)
	*now = time.Date(2026, 10, 31, 8, 0, 0, 0, loc)
	if _, err := s.QueueWeek(ctx, "dst"); err != nil {
		t.Fatal(err)
	}
	if err := s.generate(ctx); err != nil {
		t.Fatal(err)
	}
	if q.to.Sub(q.from) != 169*time.Hour {
		t.Fatal("seven local dates did not cross DST", q.from, q.to)
	}
}

func TestCustomSendTimeAndQuietHours(t *testing.T) {
	s, _, sender, now := testService(t)
	ctx := context.Background()
	s.Config.Weekly.Enabled = true
	s.Config.Weekly.At = "09:15"
	s.Config.Quiet.Start = "09:00"
	s.Config.Quiet.End = "10:00"
	if err := s.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM week_requests").Scan(&count)
	if count != 0 {
		t.Fatal("custom time ignored")
	}
	*now = now.Add(75 * time.Minute)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 0 {
		t.Fatal("automatic image ignored quiet hours")
	}
	*now = now.Add(45 * time.Minute)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.calls["smtp"] != 1 || sender.calls["telegram"] != 1 {
		t.Fatal("quiet-hours release failed", sender.calls)
	}
}
