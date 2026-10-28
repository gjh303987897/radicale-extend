package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"radicale-extend/internal/calendar"
	"radicale-extend/internal/dav"
)

type AgendaEvent struct {
	Start, End                    time.Time
	Summary, Location, Calendar   string
	AllDay, Hidden                bool
	Resource, ETag, UID, Instance string
	CalendarName                  string
}

// Agenda queries the authoritative DAV service instead of serving a stale
// notification snapshot. Only calendars authorized by the DAV client are read.
func (s *Service) Agenda(ctx context.Context, from, to time.Time, limit int) ([]AgendaEvent, error) {
	return s.agenda(ctx, from, to, limit, 101, false)
}

func (s *Service) WeeklyAgenda(ctx context.Context, from, to time.Time, limit int) ([]AgendaEvent, error) {
	if to.Sub(from) > 8*24*time.Hour {
		return nil, errors.New("invalid weekly time range")
	}
	return s.agenda(ctx, from, to, limit, 2001, true)
}

func (s *Service) agenda(ctx context.Context, from, to time.Time, limit, maxLimit int, windowed bool) ([]AgendaEvent, error) {
	if !to.After(from) || to.Sub(from) > 367*24*time.Hour || limit < 1 || limit > maxLimit {
		return nil, errors.New("invalid agenda query bounds")
	}
	calendars, err := s.DAV.Calendars(ctx)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(s.Config.Timezone)
	if err != nil {
		return nil, err
	}
	out := []AgendaEvent{}
	count := 0
	for _, col := range calendars {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var objects []dav.Object
		if windowed {
			objects, err = s.DAV.EventsWindow(ctx, col.Path, from, to)
		} else {
			objects, err = s.DAV.All(ctx, col.Path)
		}
		if err != nil {
			return nil, err
		}
		for _, obj := range objects {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			cal, err := calendar.Parse(obj.Data)
			if err != nil {
				return nil, err
			}
			if calendar.Kind(cal) != "VEVENT" {
				continue
			}
			master, err := calendar.Master(cal, "VEVENT")
			if err != nil {
				return nil, err
			}
			recurring := master.Props.Get("RRULE") != nil || master.Props.Get("RDATE") != nil
			instances, err := calendar.ExpandOverlapping(cal, from, to, loc, s.Config.Reminder.MaxInstances)
			if err != nil {
				return nil, errors.New("agenda contains unsupported calendar data")
			}
			for _, in := range instances {
				count++
				if count > s.Config.Reminder.MaxInstances {
					return nil, errors.New("agenda instance limit exceeded")
				}
				v := in.View
				instance := ""
				if recurring {
					instance = in.Key
				}
				out = append(out, AgendaEvent{Start: in.Start, End: in.End, Summary: v.Summary, Location: v.Location, Calendar: col.Path, AllDay: v.AllDay, Hidden: s.Config.Rule(col.Path).HideDetails, Resource: obj.Path, ETag: obj.ETag, UID: v.UID, Instance: instance, CalendarName: col.Name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		if out[i].Calendar != out[j].Calendar {
			return out[i].Calendar < out[j].Calendar
		}
		return out[i].Summary < out[j].Summary
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
