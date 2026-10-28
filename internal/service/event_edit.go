package service

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-ical"
	"radicale-extend/internal/calendar"
	"radicale-extend/internal/dav"
	"radicale-extend/internal/store"
)

var ErrEventChanged = errors.New("event changed; refresh the event list")

func mergeReminderRules(current, updates []store.ReminderRule) []store.ReminderRule {
	index := map[string]store.ReminderRule{}
	for _, r := range current {
		index[r.Instance+"|"+r.Channel] = r
	}
	for _, r := range updates {
		key := r.Instance + "|" + r.Channel
		if r.Mode == "" {
			delete(index, key)
		} else {
			index[key] = r
		}
	}
	out := []store.ReminderRule{}
	for _, r := range index {
		out = append(out, r)
	}
	return out
}

func effectiveReminder(rules []store.ReminderRule, instance, channel string, fallback calendar.Override) calendar.Override {
	result := fallback
	for _, r := range rules {
		if r.Instance == "" && r.Channel == channel {
			result = calendar.Override{Mode: r.Mode, BeforeSeconds: r.BeforeSeconds}
		}
	}
	for _, r := range rules {
		if r.Instance != "" && r.Instance == instance && r.Channel == channel {
			return calendar.Override{Mode: r.Mode, BeforeSeconds: r.BeforeSeconds}
		}
	}
	return result
}

type EventChannelRule struct {
	Channel, Mode, Source string
	BeforeSeconds         int64
	Enabled               bool
	AllDayAt              string
	TriggerTimes          []string
	TriggerCount          int
	SchedulingError       string
}
type EventDetails struct {
	Summary string
	Hidden  bool
	Rules   []EventChannelRule
}

func (s *Service) readEvent(ctx context.Context, ref store.EventReference) (dav.Object, *ical.Calendar, *ical.Component, error) {
	if ref.ETag == "" || ref.ETag == "*" {
		return dav.Object{}, nil, nil, ErrEventChanged
	}
	if _, err := s.DAV.Calendars(ctx); err != nil {
		return dav.Object{}, nil, nil, err
	}
	o, err := s.DAV.Get(ctx, ref.Resource)
	if err != nil {
		return o, nil, nil, err
	}
	_, col, err := s.DAV.Resource(ref.Resource)
	if err != nil || col != ref.Collection {
		return o, nil, nil, errors.New("event collection mismatch")
	}
	if o.ETag != ref.ETag {
		return o, nil, nil, ErrEventChanged
	}
	cal, err := calendar.Parse(o.Data)
	if err != nil {
		return o, nil, nil, err
	}
	master, err := calendar.Master(cal, "VEVENT")
	if err != nil {
		return o, nil, nil, err
	}
	if calendar.Text(master, "UID") != ref.UID {
		return o, nil, nil, errors.New("event UID mismatch")
	}
	if ref.Instance == "" {
		if master.Props.Get("RRULE") != nil || master.Props.Get("RDATE") != nil {
			return o, nil, nil, errors.New("recurring instance selection required")
		}
		return o, cal, master, nil
	}
	loc, _ := time.LoadLocation(s.Config.Timezone)
	rid, err := calendar.InputTime(ref.Instance, s.Config.Timezone, len(ref.Instance) == 10)
	if err != nil {
		return o, nil, nil, err
	}
	instances, err := calendar.Expand(cal, rid.AddDate(-1, 0, 0), rid.AddDate(1, 0, 0), loc, s.Config.Reminder.MaxInstances)
	if err != nil {
		return o, nil, nil, err
	}
	for _, in := range instances {
		if in.Key == ref.Instance {
			return o, cal, in.Component, nil
		}
	}
	return o, nil, nil, errors.New("event instance not found")
}

func (s *Service) EventRules(ctx context.Context, ref store.EventReference) (EventDetails, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _, comp, err := s.readEvent(ctx, ref)
	if err != nil {
		return EventDetails{}, err
	}
	return s.eventRules(ref, comp)
}

func (s *Service) eventRules(ref store.EventReference, comp *ical.Component) (EventDetails, error) {
	rules, err := s.Store.ReminderRules(ref.Collection, ref.UID)
	if err != nil {
		return EventDetails{}, err
	}
	fallback, err := s.Override(ref.Collection, ref.UID)
	if err != nil {
		return EventDetails{}, err
	}
	result := EventDetails{Summary: calendar.Text(comp, "SUMMARY"), Hidden: s.Config.Rule(ref.Collection).HideDetails}
	loc, _ := time.LoadLocation(s.Config.Timezone)
	view, viewErr := calendar.Describe(comp, loc)
	var start, end time.Time
	if viewErr == nil {
		start, viewErr = calendar.InputTime(view.Start, s.Config.Timezone, view.AllDay)
		if viewErr == nil {
			end, viewErr = calendar.InputTime(view.End, s.Config.Timezone, view.AllDay)
		}
	}
	hasAlarm := false
	for _, child := range comp.Children {
		if child.Name == "VALARM" {
			hasAlarm = true
		}
	}
	for _, channel := range []string{"smtp", "telegram"} {
		rule := effectiveReminder(rules, ref.Instance, channel, fallback)
		source := "继承日历/服务端默认"
		if fallback.Mode != "" && fallback.Mode != "calendar" {
			source = "服务端覆盖"
		}
		for _, r := range rules {
			if r.Instance == "" && r.Channel == channel {
				source = "整个系列/事件"
			}
		}
		for _, r := range rules {
			if r.Instance != "" && r.Instance == ref.Instance && r.Channel == channel {
				source = "仅本次实例"
			}
		}
		enabled := false
		for _, ch := range s.Config.Channels(ref.Collection) {
			if ch == channel {
				enabled = true
			}
		}
		if v := s.Config.Rule(ref.Collection).Reminders; v != nil && !*v {
			enabled = false
		}
		detail := EventChannelRule{Channel: channel, Mode: rule.Mode, BeforeSeconds: rule.BeforeSeconds, Source: source, Enabled: enabled}
		if rule.Mode == "" || rule.Mode == "calendar" {
			if hasAlarm {
				detail.Mode = "valarm"
				detail.Source = "日历 VALARM"
			} else {
				detail.Mode = "default"
				detail.Source = "服务端默认"
				detail.BeforeSeconds = int64(s.Config.Reminder.DefaultBefore.Value() / time.Second)
				if view.AllDay {
					detail.AllDayAt = s.Config.Reminder.AllDayAt
				}
			}
		}
		if detail.Mode != "off" {
			if viewErr != nil {
				detail.SchedulingError = "事件时间无法解析，无法可靠计算提醒触发时间"
			} else {
				alarms, err := calendar.Alarms(calendar.Instance{Start: start, End: end, Component: comp, View: view}, s.Config, rule)
				if err != nil {
					detail.SchedulingError = "日历提醒格式不受支持，无法可靠计算提醒触发时间"
				} else {
					sort.SliceStable(alarms, func(i, j int) bool { return alarms[i].At.Before(alarms[j].At) })
					detail.TriggerCount = len(alarms)
					for _, alarm := range alarms[:min(3, len(alarms))] {
						detail.TriggerTimes = append(detail.TriggerTimes, alarm.At.In(loc).Format("2006-01-02 15:04:05 -07:00"))
					}
				}
			}
		}
		result.Rules = append(result.Rules, detail)
	}
	return result, nil
}

// SetEventReminder commits channel-specific overrides and rebuilt jobs together.
// An empty instance selects the whole resource; existing instance overrides win.
func (s *Service) SetEventReminder(ctx context.Context, ref store.EventReference, channel, mode string, before int64, series bool) (EventDetails, error) {
	if channel != "smtp" && channel != "telegram" && channel != "all" {
		return EventDetails{}, errors.New("invalid reminder channel")
	}
	if (mode != "before" && mode != "off" && mode != "default") || before < 0 || before > 366*86400 {
		return EventDetails{}, errors.New("invalid reminder rule")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, _, comp, err := s.readEvent(ctx, ref)
	if err != nil {
		return EventDetails{}, err
	}
	instance := ref.Instance
	if series {
		instance = ""
	}
	if mode == "default" {
		mode = ""
	}
	updates := []store.ReminderRule{}
	for _, ch := range []string{"smtp", "telegram"} {
		if channel == "all" || channel == ch {
			updates = append(updates, store.ReminderRule{Collection: ref.Collection, UID: ref.UID, Instance: instance, Channel: ch, Mode: mode, BeforeSeconds: before})
		}
	}
	if err = s.projectReminderRules(o, ref.Collection, true, updates); err != nil {
		return EventDetails{}, err
	}
	return s.eventRules(ref, comp)
}

// EditEventTitle uses the ETag shown with the selection. A title edit never
// replaces the raw resource with a new calendar or drops unknown properties.
func (s *Service) EditEventTitle(ctx context.Context, ref store.EventReference, title string, series bool) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > 200 || strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return "", errors.New("invalid event title")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, cal, comp, err := s.readEvent(ctx, ref)
	if err != nil {
		return "", err
	}
	if !series && calendar.Text(comp, "SUMMARY") == title {
		return o.ETag, nil
	}
	if series {
		same := true
		for _, c := range cal.Children {
			if c.Name == "VEVENT" && calendar.Text(c, "SUMMARY") != title {
				same = false
			}
		}
		if same {
			return o.ETag, nil
		}
	}
	patch := calendar.Patch{Summary: &title}
	if series {
		for _, c := range cal.Children {
			if c.Name == "VEVENT" {
				if err = calendar.Apply(c, patch, s.Config.Timezone); err != nil {
					return "", err
				}
			}
		}
	} else if err = calendar.Mutate(cal, "VEVENT", ref.Instance, patch, false, s.Config.Timezone); err != nil {
		return "", err
	}
	data, err := dav.Encode(cal)
	if err != nil {
		return "", err
	}
	saved, err := s.DAV.Put(ctx, ref.Resource, data, ref.ETag, false)
	if dav.IsStatus(err, 412) {
		return "", ErrEventChanged
	}
	if err != nil {
		return "", err
	}
	// The DAV write is already authoritative. Queue/sync will repair scheduling
	// if projection fails; do not claim that the calendar edit was rolled back.
	if err = s.project(saved, ref.Collection, false); err != nil {
		slog.Warn("edited event scheduling requires resync", "resource_id", Hash(ref.Resource))
		return saved.ETag, nil
	}
	return saved.ETag, nil
}
