package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"radicale-extend/internal/calendar"
)

type AgendaEvent struct {
	Start, End                  time.Time
	Summary, Location, Calendar string
	AllDay, Hidden              bool
}

// Agenda queries the authoritative DAV service instead of serving a stale
// notification snapshot. Only calendars authorized by the DAV client are read.
func (s *Service) Agenda(ctx context.Context, from, to time.Time, limit int) ([]AgendaEvent, error) {
	if !to.After(from) || to.Sub(from) > 367*24*time.Hour || limit < 1 || limit > 101 {
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
		objects, err := s.DAV.All(ctx, col.Path)
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
				out = append(out, AgendaEvent{Start: in.Start, End: in.End, Summary: v.Summary, Location: v.Location, Calendar: col.Path, AllDay: v.AllDay, Hidden: s.Config.Rule(col.Path).HideDetails})
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
