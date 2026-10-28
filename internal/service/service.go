package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	"radicale-extend/internal/calendar"
	"radicale-extend/internal/config"
	"radicale-extend/internal/dav"
	"radicale-extend/internal/notify"
	"radicale-extend/internal/store"
)

type Service struct {
	Config   *config.Config
	DAV      *dav.Client
	Store    *store.Store
	Sender   notify.Sender
	Now      func() time.Time
	mu       sync.Mutex
	lastSync time.Time
	syncOK   bool
}

func New(c *config.Config) (*Service, error) {
	db, err := store.Open(c.Storage.State, c.Storage.Queue)
	if err != nil {
		return nil, err
	}
	client, err := dav.New(c)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Service{Config: c, DAV: client, Store: db, Sender: notify.New(c), Now: time.Now}, nil
}
func Hash(parts ...string) string {
	h := sha256.New()
	for _, v := range parts {
		fmt.Fprintf(h, "%d:%s", len(v), v)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func Revision(data string) string {
	cal, err := calendar.Parse(data)
	if err != nil {
		return Hash(data)
	}
	encoded, err := dav.Encode(cal)
	if err != nil {
		return Hash(data)
	}
	return Hash(encoded)
}
func semantic(cal *ical.Calendar) string {
	parts := []string{}
	for _, c := range cal.Children {
		if c.Name != "VEVENT" && c.Name != "VTODO" {
			continue
		}
		p := map[string]any{"kind": c.Name}
		for _, key := range []string{"UID", "SUMMARY", "DESCRIPTION", "LOCATION", "STATUS", "COMPLETED", "DTSTART", "DTEND", "DUE", "DURATION", "RRULE", "RDATE", "EXDATE", "RECURRENCE-ID"} {
			if val := c.Props[key]; len(val) > 0 {
				p[key] = val
			}
		}
		alarms := []*ical.Component{}
		for _, ch := range c.Children {
			if ch.Name == "VALARM" {
				alarms = append(alarms, ch)
			}
		}
		p["alarms"] = alarms
		b, _ := json.Marshal(p)
		parts = append(parts, string(b))
	}
	sort.Strings(parts)
	return Hash(parts...)
}
func (s *Service) Override(col, uid string) (calendar.Override, error) {
	var r calendar.Override
	err := s.Store.DB.QueryRow("SELECT mode,before_seconds FROM overrides WHERE collection=? AND uid=?", col, uid).Scan(&r.Mode, &r.BeforeSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	return r, err
}
func (s *Service) project(o dav.Object, col string, baseline bool) error {
	return s.projectReminderRules(o, col, baseline, nil)
}

func (s *Service) projectReminderRules(o dav.Object, col string, baseline bool, updates []store.ReminderRule) error {
	old, err := s.Store.Resource(o.Path)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	cal, err := calendar.Parse(o.Data)
	if err != nil {
		return err
	}
	kind := calendar.Kind(cal)
	if kind == "" {
		return s.remove(o.Path, true)
	}
	master, err := calendar.Master(cal, kind)
	if err != nil {
		return err
	}
	uid := calendar.Text(master, "UID")
	channelRules, err := s.Store.ReminderRules(col, uid)
	if err != nil {
		return err
	}
	channelRules = mergeReminderRules(channelRules, updates)
	r := store.Resource{Path: o.Path, Collection: col, UID: uid, ETag: o.ETag, Data: o.Data, Revision: Revision(o.Data), Semantic: semantic(cal)}
	changed := !baseline && (!exists || old.Semantic != r.Semantic)
	now := s.Now()
	jobs := []store.Job{}
	rule := s.Config.Rule(col)
	loc, _ := time.LoadLocation(s.Config.Timezone)
	if rule.Reminders == nil || *rule.Reminders {
		override, e := s.Override(col, uid)
		if e != nil {
			return e
		}
		horizon := now.AddDate(0, 0, s.Config.Reminder.HorizonDays)
		advance := s.Config.Reminder.DefaultBefore.Value()
		if override.Mode == "before" {
			advance = time.Duration(override.BeforeSeconds) * time.Second
		}
		for _, r := range channelRules {
			if r.Mode == "before" && time.Duration(r.BeforeSeconds)*time.Second > advance {
				advance = time.Duration(r.BeforeSeconds) * time.Second
			}
		}
		for _, component := range cal.Children {
			for _, alarm := range component.Children {
				if alarm.Name != "VALARM" {
					continue
				}
				if p := alarm.Props.Get("TRIGGER"); p != nil && p.ValueType() != ical.ValueDateTime {
					if d, e := p.Duration(); e == nil && -d > advance {
						advance = -d
					}
				}
			}
		}
		until := horizon.Add(advance)
		// A one-off absolute alarm can precede a distant event by any amount.
		if master.Props.Get("RRULE") == nil && master.Props.Get("RDATE") == nil {
			for _, alarm := range master.Children {
				p := alarm.Props.Get("TRIGGER")
				if alarm.Name == "VALARM" && p != nil && p.ValueType() == ical.ValueDateTime {
					start, e := calendar.Date(master, "DTSTART", loc)
					if e == nil && start.After(until) {
						until = start.AddDate(0, 0, 1)
					}
				}
			}
		}
		instances, e := calendar.Expand(cal, now.AddDate(-1, 0, 0), until, loc, s.Config.Reminder.MaxInstances)
		if e != nil {
			r.Error = e.Error()
		} else {
			for _, in := range instances {
				for _, ch := range s.Config.Channels(col) {
					alarms, e := calendar.Alarms(in, s.Config, effectiveReminder(channelRules, in.Key, ch, override))
					if e != nil {
						r.Error = e.Error()
						break
					}
					for _, a := range alarms {
						if a.At.Before(now.Add(-s.Config.Reminder.Grace.Value())) || !a.At.Before(horizon) {
							continue
						}
						title, body := s.render("日程提醒", in.View, col, rule.HideDetails)
						if kind == "VTODO" {
							title, body = s.render("待办提醒", in.View, col, rule.HideDetails)
						}
						due := s.Config.QuietUntil(a.At)
						jobs = append(jobs, store.Job{ID: Hash("reminder", col, uid, in.Key, a.Key, ch, a.At.UTC().Format(time.RFC3339)), Resource: o.Path, Collection: col, UID: uid, Instance: in.Key, Kind: "reminder", Channel: ch, Source: a.Key, Revision: r.Revision, At: a.At.Unix(), Due: due.Unix(), Expires: a.At.Add(s.Config.Reminder.Grace.Value()).Unix(), Title: title, Body: body})
					}
				}
				if r.Error != "" {
					break
				}
			}
		}
	}
	if r.Error != "" {
		jobs = nil
		slog.Warn("calendar scheduling unsupported", "resource_id", Hash(o.Path), "reason", r.Error)
	}
	if changed && (rule.Changes == nil || *rule.Changes) {
		v, e := calendar.Describe(master, loc)
		if e == nil {
			action := "日程已更新"
			if !exists {
				action = "日程已创建"
			}
			title, body := s.render(action, v, col, rule.HideDetails)
			for _, ch := range s.Config.Channels(col) {
				jobs = append(jobs, store.Job{ID: Hash("change", o.Path, ch), Resource: o.Path, Collection: col, UID: uid, Kind: "change", Channel: ch, Revision: r.Revision, At: now.Unix(), Due: s.Config.QuietUntil(now.Add(s.Config.Debounce.Value())).Unix(), Title: title, Body: body})
			}
		}
	}
	if len(updates) > 0 && r.Error != "" {
		return errors.New("cannot apply reminder rules to unsupported calendar data")
	}
	return s.Store.ApplyReminderRules(r, jobs, changed, false, updates)
}
func (s *Service) render(action string, v calendar.View, col string, hidden bool) (string, string) {
	if hidden {
		return action, "日历中有一项活动发生变化或需要提醒。"
	}
	loc, _ := time.LoadLocation(s.Config.Timezone)
	format := func(raw string) string {
		if t, e := time.Parse(time.RFC3339, raw); e == nil {
			return t.In(loc).Format("2006-01-02 15:04 MST")
		}
		return raw
	}
	body := "日历：" + col + "\n标题：" + v.Summary
	if v.Start != "" {
		body += "\n开始：" + format(v.Start)
	}
	if v.End != "" {
		body += "\n结束：" + format(v.End)
	}
	if v.Due != "" {
		body += "\n截止：" + format(v.Due)
	}
	if v.Location != "" {
		body += "\n地点：" + v.Location
	}
	if v.Status != "" {
		body += "\n状态：" + v.Status
	}
	return action + "：" + v.Summary, body
}
func (s *Service) remove(path string, baseline bool) error {
	old, err := s.Store.Resource(path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	jobs := []store.Job{}
	now := s.Now()
	rule := s.Config.Rule(old.Collection)
	if !baseline && (rule.Changes == nil || *rule.Changes) {
		cal, e := calendar.Parse(old.Data)
		if e == nil {
			master, e := calendar.Master(cal, calendar.Kind(cal))
			if e == nil {
				loc, _ := time.LoadLocation(s.Config.Timezone)
				v, e := calendar.Describe(master, loc)
				if e == nil {
					title, body := s.render("日程已删除", v, old.Collection, rule.HideDetails)
					for _, ch := range s.Config.Channels(old.Collection) {
						jobs = append(jobs, store.Job{ID: Hash("change", path, ch), Resource: path, Collection: old.Collection, UID: old.UID, Kind: "change", Channel: ch, Revision: "deleted", At: now.Unix(), Due: s.Config.QuietUntil(now.Add(s.Config.Debounce.Value())).Unix(), Title: title, Body: body})
					}
				}
			}
		}
	}
	return s.Store.Apply(old, jobs, true, true)
}
func (s *Service) syncCollection(ctx context.Context, col string, refresh bool) error {
	var token string
	var initialized int
	err := s.Store.DB.QueryRow("SELECT token,initialized FROM collections WHERE path=?", col).Scan(&token, &initialized)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	items, deleted, next, full, err := s.DAV.Sync(ctx, col, token)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, o := range items {
		seen[o.Path] = true
		if err = s.project(o, col, initialized == 0); err != nil {
			return err
		}
	}
	if full {
		stored, e := s.Store.Resources(col)
		if e != nil {
			return e
		}
		for _, r := range stored {
			if !seen[r.Path] {
				deleted = append(deleted, r.Path)
			}
		}
	}
	for _, p := range deleted {
		if err = s.remove(p, initialized == 0); err != nil {
			return err
		}
	}
	if refresh && !full {
		resources, e := s.Store.Resources(col)
		if e != nil {
			return e
		}
		for _, r := range resources {
			if seen[r.Path] {
				continue
			}
			if err = s.project(dav.Object{Path: r.Path, ETag: r.ETag, Data: r.Data}, col, true); err != nil {
				return err
			}
		}
	}
	_, err = s.Store.DB.Exec("INSERT INTO collections(path,token,initialized) VALUES(?,?,1) ON CONFLICT(path) DO UPDATE SET token=excluded.token,initialized=1", col, next)
	return err
}
func (s *Service) Sync(ctx context.Context, refresh bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.DAV.Calendars(ctx)
	if err != nil {
		s.syncOK = false
		return err
	}
	seen := map[string]bool{}
	for _, c := range list {
		seen[c.Path] = true
		if err = s.syncCollection(ctx, c.Path, refresh); err != nil {
			s.syncOK = false
			return err
		}
	}
	rows, err := s.Store.DB.Query("SELECT path FROM collections")
	if err != nil {
		return err
	}
	missing := []string{}
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		if !seen[p] {
			missing = append(missing, p)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range missing {
		if _, err = s.Store.DB.Exec("UPDATE jobs SET state='cancelled' WHERE collection=? AND state IN ('pending','retry','processing')", p); err != nil {
			return err
		}
	}
	s.lastSync = s.Now()
	s.syncOK = true
	return nil
}
func (s *Service) Consume(ctx context.Context) error {
	e, err := s.Store.ClaimEvent(s.Now())
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var envelope struct {
		Version int `json:"version"`
		Item    struct {
			Point string `json:"point"`
		} `json:"item"`
	}
	err = json.Unmarshal([]byte(e.Payload), &envelope)
	if err == nil && envelope.Version != 1 {
		err = errors.New("unsupported queue protocol")
	}
	if err == nil {
		if col, e := s.DAV.Collection("/" + strings.Trim(envelope.Item.Point, "/") + "/"); e == nil {
			s.mu.Lock()
			err = s.syncCollection(ctx, col, false)
			s.mu.Unlock()
		}
	}
	if finishErr := s.Store.FinishEvent(e, err == nil, s.Now()); finishErr != nil {
		return finishErr
	}
	return err
}
func (s *Service) Dispatch(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	j, err := s.Store.ClaimJob(ctx, now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	finish := func(state string, due time.Time, message string) error { return s.Store.Finish(j, state, due, message) }
	rule := s.Config.Rule(j.Collection)
	allowed := false
	for _, channel := range s.Config.Channels(j.Collection) {
		if channel == j.Channel {
			allowed = true
		}
	}
	if !allowed || (j.Kind == "change" && rule.Changes != nil && !*rule.Changes) || (j.Kind == "reminder" && rule.Reminders != nil && !*rule.Reminders) {
		return finish("cancelled", now, "")
	}
	if rule.HideDetails {
		j.Title = "日历通知"
		j.Body = "日历中有一项活动发生变化或需要提醒。"
	}
	if j.Expires > 0 && now.Unix() > j.Expires {
		return finish("expired", now, "")
	}
	if quiet := s.Config.QuietUntil(now); quiet.After(now) {
		return finish("pending", quiet, "")
	}
	current, err := s.DAV.Get(ctx, j.Resource)
	if err != nil && !dav.IsStatus(err, 404) {
		return s.retry(j, &notify.Error{Message: "DAV validation unavailable"}, now)
	}
	if j.Revision == "deleted" {
		if err == nil {
			return finish("cancelled", now, "")
		}
	} else {
		if dav.IsStatus(err, 404) {
			return finish("cancelled", now, "")
		}
		if Revision(current.Data) != j.Revision {
			old, e := s.Store.Resource(j.Resource)
			if e != nil {
				return e
			}
			if e = s.project(current, old.Collection, false); e != nil {
				return e
			}
			_, err = s.Store.DB.Exec("UPDATE jobs SET state='pending',lease=0 WHERE id=? AND state='processing'", j.ID)
			return err
		}
	}
	// Recheck DB after any projection; only the claimed version may be delivered.
	var state, revision string
	if err = s.Store.DB.QueryRow("SELECT state,revision FROM jobs WHERE id=?", j.ID).Scan(&state, &revision); err != nil {
		return err
	}
	if state != "processing" || revision != j.Revision {
		return nil
	}
	if err = s.Sender.Send(ctx, *j); err != nil {
		return s.retry(j, err, now)
	}
	return finish("success", now, "")
}
func (s *Service) retry(j *store.Job, err error, now time.Time) error {
	j.Attempts++
	state := "retry"
	var detail *notify.Error
	permanent := errors.As(err, &detail) && detail.Permanent
	if permanent || j.Attempts >= 8 {
		state = "failed"
	}
	delay := time.Duration(1<<min(j.Attempts, 12)) * time.Second
	if delay > time.Hour {
		delay = time.Hour
	}
	if detail != nil && detail.RetryAfter > delay {
		delay = detail.RetryAfter
	}
	slog.Warn("delivery attempt failed", "job", j.ID, "channel", j.Channel, "attempt", j.Attempts, "terminal", state == "failed")
	return s.Store.Finish(j, state, now.Add(delay), "delivery failed")
}
func (s *Service) Run(ctx context.Context) {
	if err := s.Sync(ctx, true); err != nil {
		slog.Warn("initial sync failed")
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastPoll := s.Now()
	lastRefresh := s.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			now := s.Now()
			if now.Sub(lastPoll) >= s.Config.SyncInterval.Value() || now.Sub(lastRefresh) >= time.Hour {
				refresh := now.Sub(lastRefresh) >= time.Hour
				if err := s.Sync(ctx, refresh); err != nil {
					slog.Warn("periodic sync failed")
				}
				lastPoll = now
				if refresh {
					lastRefresh = now
				}
			}
			if err := s.Consume(ctx); err != nil {
				slog.Warn("queue event processing failed")
			}
			for i := 0; i < 20; i++ {
				if err := s.Dispatch(ctx); err != nil {
					slog.Warn("job processing failed")
					break
				}
			}
		}
	}
}
func (s *Service) Ready() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, failed, events, failedEvents, unsupported := 0, 0, 0, 0, 0
	s.Store.DB.QueryRow("SELECT count(*) FROM jobs WHERE state IN ('pending','retry','processing')").Scan(&pending)
	s.Store.DB.QueryRow("SELECT count(*) FROM jobs WHERE state='failed'").Scan(&failed)
	s.Store.Queue.QueryRow("SELECT count(*) FROM events WHERE state IN ('pending','retry','processing')").Scan(&events)
	s.Store.Queue.QueryRow("SELECT count(*) FROM events WHERE state='failed'").Scan(&failedEvents)
	s.Store.DB.QueryRow("SELECT count(*) FROM resources WHERE error!=''").Scan(&unsupported)
	pendingReplies, failedReplies := 0, 0
	s.Store.DB.QueryRow("SELECT count(*) FROM telegram_replies WHERE state='pending'").Scan(&pendingReplies)
	s.Store.DB.QueryRow("SELECT count(*) FROM telegram_replies WHERE state='failed'").Scan(&failedReplies)
	ok := s.syncOK && !s.lastSync.IsZero() && s.Store.DB.Ping() == nil && s.Store.Queue.Ping() == nil
	return map[string]any{"ready": ok, "last_sync": s.lastSync, "pending_jobs": pending, "failed_jobs": failed, "queued_events": events, "failed_events": failedEvents, "unsupported_resources": unsupported, "telegram_pending_replies": pendingReplies, "telegram_failed_replies": failedReplies}
}
func (s *Service) SetReminder(ctx context.Context, resource string, override calendar.Override) error {
	if override.Mode != "calendar" && override.Mode != "off" && override.Mode != "before" {
		return errors.New("mode must be calendar, off or before")
	}
	if override.BeforeSeconds < 0 || override.BeforeSeconds > 366*86400 {
		return errors.New("invalid reminder advance")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.DAV.Get(ctx, resource)
	if err != nil {
		return err
	}
	_, col, err := s.DAV.Resource(resource)
	if err != nil {
		return err
	}
	cal, err := calendar.Parse(o.Data)
	if err != nil {
		return err
	}
	master, err := calendar.Master(cal, calendar.Kind(cal))
	if err != nil {
		return err
	}
	uid := calendar.Text(master, "UID")
	_, err = s.Store.DB.Exec("INSERT INTO overrides(collection,uid,mode,before_seconds) VALUES(?,?,?,?) ON CONFLICT(collection,uid) DO UPDATE SET mode=excluded.mode,before_seconds=excluded.before_seconds", col, uid, override.Mode, override.BeforeSeconds)
	if err != nil {
		return err
	}
	return s.project(o, col, true)
}
