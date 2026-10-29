// Package week schedules and delivers rolling seven-day calendar images.
package week

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"radicale-extend/internal/config"
	"radicale-extend/internal/notify"
	"radicale-extend/internal/service"
	"radicale-extend/internal/store"
	"radicale-extend/internal/weekimage"
)

var ErrNoChannels = errors.New("no weekly delivery channels enabled")

type Source interface {
	WeeklyAgenda(context.Context, time.Time, time.Time, int) ([]service.AgendaEvent, error)
}
type Service struct {
	Config         *config.Config
	Store          *store.Store
	Source         Source
	Sender         notify.ImageSender
	Render         func(context.Context, weekimage.Options, []weekimage.Event) (weekimage.Result, error)
	Now            func() time.Time
	mu             sync.Mutex
	lastScheduled  string
	lastClean      time.Time
	lastCheckpoint time.Time
}

func New(s *service.Service) *Service {
	return &Service{Config: s.Config, Store: s.Store, Source: s, Sender: notify.New(s.Config), Render: weekimage.Render, Now: time.Now}
}
func (s *Service) channels() []string {
	result := []string{}
	for _, ch := range s.Config.Weekly.Channels {
		if ch == "smtp" && s.Config.SMTP.Enabled || ch == "telegram" && s.Config.Telegram.Enabled {
			result = append(result, ch)
		}
	}
	return result
}
func (s *Service) policy() string {
	data, _ := json.Marshal([]any{s.Config.Timezone, s.Config.Calendars, s.Config.Weekly.Channels, s.Config.SMTP.To, s.Config.Telegram.ChatID})
	return service.Hash(string(data))
}
func (s *Service) QueueWeek(ctx context.Context, key string) (bool, error) {
	if len(s.channels()) == 0 {
		return false, ErrNoChannels
	}
	loc, _ := time.LoadLocation(s.Config.Timezone)
	now := s.Now()
	date := now.In(loc).Format("2006-01-02")
	return s.Store.QueueWeek(ctx, store.WeekRequest{ID: service.Hash("week-manual", key), StartDate: date, Kind: "manual", Created: now.Unix(), Due: now.Unix(), Expires: now.Add(s.Config.Weekly.CatchUp.Value()).Unix()})
}

func (s *Service) Schedule(ctx context.Context) error {
	if !s.Config.Weekly.Enabled || len(s.channels()) == 0 {
		return nil
	}
	loc, _ := time.LoadLocation(s.Config.Timezone)
	now := s.Now().In(loc)
	date := now.Format("2006-01-02")
	if s.lastScheduled == date {
		return nil
	}
	clock, err := time.Parse("15:04", s.Config.Weekly.At)
	if err != nil {
		return err
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
	// A nonexistent local clock time on a DST transition is skipped. Repeated
	// local times share a date-based request ID and cannot create a second report.
	if at.Hour() != clock.Hour() || at.Minute() != clock.Minute() {
		s.lastScheduled = date
		return nil
	}
	if now.Before(at) {
		return nil
	}
	if !now.Before(at.Add(s.Config.Weekly.CatchUp.Value())) {
		s.lastScheduled = date
		return nil
	}
	_, err = s.Store.QueueWeek(ctx, store.WeekRequest{ID: service.Hash("week-daily", s.Config.Timezone, date), StartDate: date, Kind: "daily", Created: now.Unix(), Due: s.Config.QuietUntil(at).Unix(), Expires: at.Add(s.Config.Weekly.CatchUp.Value()).Unix()})
	if err == nil {
		s.lastScheduled = date
	}
	return err
}

func delay(attempt int, err error) time.Duration {
	d := min(time.Hour, time.Duration(1<<min(attempt, 12))*time.Second)
	var e *notify.Error
	if errors.As(err, &e) && e.RetryAfter > d {
		d = e.RetryAfter
	}
	return d
}

func (s *Service) generate(ctx context.Context) error {
	now := s.Now()
	request, err := s.Store.ClaimWeek(ctx, now)
	if store.WeekEmpty(err) {
		return nil
	}
	if err != nil {
		return err
	}
	finish := func(state string, deliveries []store.WeekDelivery) error {
		return s.Store.FinishWeek(ctx, request, state, s.Now(), deliveries)
	}
	if now.Unix() >= request.Expires {
		return finish("expired", nil)
	}
	channels := s.channels()
	if len(channels) == 0 {
		return finish("cancelled", nil)
	}
	queryCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	loc, _ := time.LoadLocation(s.Config.Timezone)
	start, err := time.ParseInLocation("2006-01-02", request.StartDate, loc)
	var agenda []service.AgendaEvent
	if err == nil {
		agenda, err = s.Source.WeeklyAgenda(queryCtx, start, start.AddDate(0, 0, 7), s.Config.Weekly.MaxEvents+1)
	}
	deliveries := []store.WeekDelivery{}
	cache := map[string]weekimage.Result{}
	if err == nil {
		truncated := len(agenda) > s.Config.Weekly.MaxEvents
		if truncated {
			agenda = agenda[:s.Config.Weekly.MaxEvents]
		}
		for _, channel := range channels {
			events := []weekimage.Event{}
			for _, e := range agenda {
				allowed := false
				for _, ch := range s.Config.Channels(e.Calendar) {
					if ch == channel {
						allowed = true
					}
				}
				if allowed {
					hidden := e.Hidden || s.Config.Rule(e.Calendar).HideDetails
					title, location, name := weekimage.Text(e.Summary, 160), weekimage.Text(e.Location, 120), weekimage.Text(e.CalendarName, 64)
					if hidden {
						title = "私人日程"
						location = ""
						name = "隐藏日历"
					}
					events = append(events, weekimage.Event{Start: e.Start, End: e.End, Title: title, Location: location, Calendar: e.Calendar, CalendarName: name, AllDay: e.AllDay, Hidden: hidden})
				}
			}
			data, _ := json.Marshal(events)
			key := service.Hash(string(data))
			image, ok := cache[key]
			if !ok {
				image, err = s.Render(queryCtx, weekimage.Options{Start: start, Generated: now, FontFile: s.Config.Weekly.FontFile, FontIndex: s.Config.Weekly.FontIndex, Truncated: truncated}, events)
				if err != nil {
					break
				}
				cache[key] = image
			}
			title := fmt.Sprintf("未来七天日程 · %s — %s", start.Format("01/02"), start.AddDate(0, 0, 6).Format("01/02"))
			caption := fmt.Sprintf("%s 时区 · %d 项日程 · 生成于 %s", s.Config.Timezone, image.Events, now.In(loc).Format("01-02 15:04"))
			if image.Collapsed > 0 || truncated {
				caption += "；密集或超量日程已在图中标注。"
			}
			deliveries = append(deliveries, store.WeekDelivery{Channel: channel, Title: title, Caption: caption, PNG: image.PNG, Policy: s.policy(), Due: now.Unix()})
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		request.Attempts++
		state := "pending"
		if request.Attempts >= 8 {
			state = "failed"
		}
		slog.Warn("weekly image generation failed", "request", request.ID, "attempt", request.Attempts)
		return s.Store.FinishWeek(ctx, request, state, now.Add(delay(request.Attempts, err)), nil)
	}
	return finish("done", deliveries)
}

func (s *Service) dispatch(ctx context.Context) error {
	now := s.Now()
	d, err := s.Store.ClaimWeekDelivery(ctx, now)
	if store.WeekEmpty(err) {
		return nil
	}
	if err != nil {
		return err
	}
	finish := func(state string, due time.Time) error { return s.Store.FinishWeekDelivery(ctx, d, state, due) }
	allowed := false
	for _, ch := range s.channels() {
		if ch == d.Channel {
			allowed = true
		}
	}
	if !allowed || d.Policy != s.policy() {
		return finish("cancelled", now)
	}
	if now.Unix() >= d.Expires {
		return finish("expired", now)
	}
	if d.Kind == "daily" {
		if due := s.Config.QuietUntil(now); due.After(now) {
			return finish("pending", due)
		}
	}
	err = s.Sender.SendImage(ctx, store.Job{ID: service.Hash(d.RequestID, d.Channel), Channel: d.Channel, Title: d.Title, Body: d.Caption}, d.PNG)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return finish("done", now)
	}
	d.Attempts++
	state := "pending"
	var detail *notify.Error
	if d.Attempts >= 8 || (errors.As(err, &detail) && detail.Permanent) {
		state = "failed"
	}
	slog.Warn("weekly image delivery failed", "request", d.RequestID, "channel", d.Channel, "attempt", d.Attempts, "terminal", state == "failed")
	return finish(state, now.Add(delay(d.Attempts, err)))
}

func (s *Service) Step(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastClean.IsZero() || s.Now().Sub(s.lastClean) >= time.Minute {
		if err := s.Store.CleanWeeks(ctx, s.Now()); err != nil {
			return err
		}
		s.lastClean = s.Now()
	}
	if err := s.Schedule(ctx); err != nil {
		return err
	}
	if err := s.generate(ctx); err != nil {
		return err
	}
	// Each channel is claimed independently. A retry does not resend a channel
	// that has already succeeded, and images are cleared when terminal.
	for i := 0; i < 2; i++ {
		if err := s.dispatch(ctx); err != nil {
			return err
		}
	}
	if s.lastCheckpoint.IsZero() || s.Now().Sub(s.lastCheckpoint) >= time.Hour {
		if err := s.Store.CheckpointWeekStorage(ctx, s.Now()); err != nil {
			return err
		}
		s.lastCheckpoint = s.Now()
	}
	return nil
}
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.Step(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("weekly image worker failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
