package calendar

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/google/uuid"
	"github.com/teambition/rrule-go"
	"radicale-extend/internal/config"
)

type View struct {
	UID          string `json:"uid"`
	Kind         string `json:"kind"`
	Summary      string `json:"summary"`
	Description  string `json:"description,omitempty"`
	Location     string `json:"location,omitempty"`
	Status       string `json:"status,omitempty"`
	Start        string `json:"start,omitempty"`
	End          string `json:"end,omitempty"`
	Due          string `json:"due,omitempty"`
	AllDay       bool   `json:"all_day"`
	RecurrenceID string `json:"recurrence_id,omitempty"`
	RRULE        string `json:"rrule,omitempty"`
}
type Patch struct {
	Summary     *string   `json:"summary,omitempty"`
	Description *string   `json:"description,omitempty"`
	Location    *string   `json:"location,omitempty"`
	Status      *string   `json:"status,omitempty"`
	Start       *string   `json:"start,omitempty"`
	End         *string   `json:"end,omitempty"`
	Due         *string   `json:"due,omitempty"`
	AllDay      *bool     `json:"all_day,omitempty"`
	Timezone    string    `json:"timezone,omitempty"`
	RRULE       *string   `json:"rrule,omitempty"`
	RDates      *[]string `json:"rdates,omitempty"`
	ExDates     *[]string `json:"exdates,omitempty"`
}
type Instance struct {
	Key        string
	Start, End time.Time
	Component  *ical.Component
	View       View
}
type Alarm struct {
	Key string
	At  time.Time
}
type Override struct {
	Mode          string `json:"mode"`
	BeforeSeconds int64  `json:"before_seconds"`
}

func Parse(data string) (*ical.Calendar, error) {
	cal, err := ical.NewDecoder(strings.NewReader(data)).Decode()
	if err != nil {
		return nil, errors.New("invalid iCalendar data")
	}
	return cal, nil
}
func Text(c *ical.Component, p string) string { v, _ := c.Props.Text(p); return v }
func Kind(c *ical.Calendar) string {
	for _, v := range c.Children {
		if v.Name == "VEVENT" || v.Name == "VTODO" {
			return v.Name
		}
	}
	return ""
}
func Master(cal *ical.Calendar, kind string) (*ical.Component, error) {
	var master *ical.Component
	uid := ""
	for _, c := range cal.Children {
		if c.Name != kind {
			continue
		}
		if uid != "" && Text(c, "UID") != uid {
			return nil, errors.New("multiple UIDs in resource")
		}
		uid = Text(c, "UID")
		if c.Props.Get("RECURRENCE-ID") == nil {
			if master != nil {
				return nil, errors.New("multiple master components")
			}
			master = c
		}
	}
	if master == nil || uid == "" {
		return nil, errors.New("calendar master and UID required")
	}
	return master, nil
}
func Date(c *ical.Component, name string, loc *time.Location) (time.Time, error) {
	p := c.Props.Get(name)
	if p == nil {
		return time.Time{}, nil
	}
	v, err := p.DateTime(loc)
	if err != nil {
		return v, fmt.Errorf("unsupported or invalid %s time/timezone", name)
	}
	return v, nil
}
func dateKey(t time.Time, day bool) string {
	if day {
		return t.Format("2006-01-02")
	}
	return t.UTC().Format(time.RFC3339)
}
func bounds(c *ical.Component, loc *time.Location) (time.Time, time.Time, bool, error) {
	start, err := Date(c, "DTSTART", loc)
	if err != nil {
		return start, start, false, err
	}
	if start.IsZero() && c.Name == "VTODO" {
		start, err = Date(c, "DUE", loc)
	}
	if err != nil {
		return start, start, false, err
	}
	p := c.Props.Get("DTSTART")
	if p == nil {
		p = c.Props.Get("DUE")
	}
	day := p != nil && p.ValueType() == ical.ValueDate
	endName := "DTEND"
	if c.Name == "VTODO" {
		endName = "DUE"
	}
	end, err := Date(c, endName, loc)
	if err != nil {
		return start, end, day, err
	}
	if end.IsZero() {
		if d := c.Props.Get("DURATION"); d != nil {
			dur, e := d.Duration()
			if e != nil {
				return start, end, day, e
			}
			end = start.Add(dur)
		} else if day {
			end = start.AddDate(0, 0, 1)
		} else {
			end = start
		}
	}
	if !start.IsZero() && end.Before(start) {
		return start, end, day, errors.New("end precedes start")
	}
	return start, end, day, nil
}
func Describe(c *ical.Component, loc *time.Location) (View, error) {
	start, end, day, err := bounds(c, loc)
	if err != nil {
		return View{}, err
	}
	v := View{UID: Text(c, "UID"), Kind: c.Name, Summary: Text(c, "SUMMARY"), Description: Text(c, "DESCRIPTION"), Location: Text(c, "LOCATION"), Status: Text(c, "STATUS"), AllDay: day}
	if !start.IsZero() {
		if c.Props.Get("DTSTART") != nil {
			v.Start = dateKey(start, day)
		}
		if c.Name == "VTODO" {
			if c.Props.Get("DUE") != nil {
				v.Due = dateKey(end, day)
			}
		} else {
			v.End = dateKey(end, day)
		}
	}
	if p := c.Props.Get("RRULE"); p != nil {
		v.RRULE = p.Value
	}
	if p := c.Props.Get("RECURRENCE-ID"); p != nil {
		rid, e := p.DateTime(loc)
		if e != nil {
			return v, e
		}
		v.RecurrenceID = dateKey(rid, day)
	}
	return v, nil
}
func dates(props []ical.Prop, loc *time.Location) ([]time.Time, error) {
	out := []time.Time{}
	for _, p := range props {
		if p.ValueType() == ical.ValuePeriod {
			return nil, errors.New("RDATE PERIOD unsupported")
		}
		for _, value := range strings.Split(p.Value, ",") {
			copy := p
			copy.Value = value
			t, err := copy.DateTime(loc)
			if err != nil {
				return nil, errors.New("invalid recurrence date/timezone")
			}
			out = append(out, t)
		}
	}
	return out, nil
}
func clone(c *ical.Component) *ical.Component {
	r := ical.NewComponent(c.Name)
	for key, props := range c.Props {
		for _, p := range props {
			n := p
			n.Params = ical.Params{}
			for k, v := range p.Params {
				n.Params[k] = append([]string{}, v...)
			}
			r.Props[key] = append(r.Props[key], n)
		}
	}
	for _, v := range c.Children {
		r.Children = append(r.Children, clone(v))
	}
	return r
}
func setTime(c *ical.Component, name string, t time.Time, day bool) {
	p := ical.NewProp(name)
	if day {
		p.SetDate(t)
	} else {
		p.SetDateTime(t)
	}
	c.Props.Set(p)
}
func Expand(cal *ical.Calendar, from, to time.Time, loc *time.Location, limit int) ([]Instance, error) {
	return ExpandWithStatus(cal, from, to, loc, limit, false)
}
func ExpandWithStatus(cal *ical.Calendar, from, to time.Time, loc *time.Location, limit int, includeCompleted bool) ([]Instance, error) {
	kind := Kind(cal)
	master, err := Master(cal, kind)
	if err != nil {
		return nil, err
	}
	start, end, day, err := bounds(master, loc)
	if err != nil {
		return nil, err
	}
	if start.IsZero() {
		return []Instance{}, nil
	}
	for _, comp := range cal.Children {
		if p := comp.Props.Get("RECURRENCE-ID"); p != nil && p.Params.Get("RANGE") != "" {
			return nil, errors.New("RANGE recurrence exceptions unsupported")
		}
	}
	candidates := map[string]time.Time{dateKey(start, day): start}
	if p := master.Props.Get("RRULE"); p != nil {
		opt, e := rrule.StrToROptionInLocation(p.Value, start.Location())
		if e != nil {
			return nil, errors.New("invalid RRULE")
		}
		opt.Dtstart = start
		rule, e := rrule.NewRRule(*opt)
		if e != nil {
			return nil, e
		}
		next := rule.Iterator()
		count := 0
		for {
			t, ok := next()
			if !ok || t.After(to) {
				break
			}
			count++
			if count > 1000000 {
				return nil, errors.New("recurrence iteration limit exceeded")
			}
			if !t.Before(from) {
				candidates[dateKey(t, day)] = t
				if len(candidates) > limit {
					return nil, errors.New("recurrence instance limit exceeded")
				}
			}
		}
	}
	add, e := dates(master.Props["RDATE"], loc)
	if e != nil {
		return nil, e
	}
	for _, t := range add {
		candidates[dateKey(t, day)] = t
	}
	excluded, e := dates(master.Props["EXDATE"], loc)
	if e != nil {
		return nil, e
	}
	for _, t := range excluded {
		delete(candidates, dateKey(t, day))
	}
	exceptions := map[string]*ical.Component{}
	for _, comp := range cal.Children {
		if comp.Name != kind || comp.Props.Get("RECURRENCE-ID") == nil {
			continue
		}
		t, e := Date(comp, "RECURRENCE-ID", loc)
		if e != nil {
			return nil, e
		}
		key := dateKey(t, day)
		exceptions[key] = comp
		candidates[key] = t
	}
	out := []Instance{}
	for key, t := range candidates {
		comp := clone(master)
		s, finish := t, t.Add(end.Sub(start))
		if day {
			civilStart := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
			civilEnd := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
			days := int(civilEnd.Sub(civilStart).Hours() / 24)
			finish = t.AddDate(0, 0, days)
		}
		if kind != "VTODO" || master.Props.Get("DTSTART") != nil {
			setTime(comp, "DTSTART", s, day)
		}
		if kind == "VTODO" {
			setTime(comp, "DUE", finish, day)
		} else {
			setTime(comp, "DTEND", finish, day)
		}
		if ex := exceptions[key]; ex != nil {
			comp = ex
			s, finish, _, err = bounds(comp, loc)
			if err != nil {
				return nil, err
			}
		}
		if Text(comp, "STATUS") == "CANCELLED" || (!includeCompleted && (Text(comp, "STATUS") == "COMPLETED" || comp.Props.Get("COMPLETED") != nil)) {
			continue
		}
		if s.Before(from) || !s.Before(to) {
			continue
		}
		view, e := Describe(comp, loc)
		if e != nil {
			return nil, e
		}
		view.RecurrenceID = key
		out = append(out, Instance{key, s, finish, comp, view})
		if len(out) > limit {
			return nil, errors.New("recurrence instance limit exceeded")
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}
func Alarms(in Instance, cfg *config.Config, override Override) ([]Alarm, error) {
	if override.Mode == "off" {
		return nil, nil
	}
	if override.Mode == "before" {
		return []Alarm{{fmt.Sprintf("override:%d", override.BeforeSeconds), in.Start.Add(-time.Duration(override.BeforeSeconds) * time.Second)}}, nil
	}
	alarms := []Alarm{}
	has := false
	for _, c := range in.Component.Children {
		if c.Name != "VALARM" {
			continue
		}
		has = true
		p := c.Props.Get("TRIGGER")
		if p == nil {
			return nil, errors.New("VALARM trigger missing")
		}
		base := in.Start
		if p.Params.Get("RELATED") == "END" {
			base = in.End
		}
		var at time.Time
		var err error
		if p.ValueType() == ical.ValueDateTime {
			at, err = p.DateTime(time.UTC)
		} else {
			var d time.Duration
			d, err = p.Duration()
			at = base.Add(d)
		}
		if err != nil {
			return nil, errors.New("invalid VALARM trigger")
		}
		repeat := 0
		interval := time.Duration(0)
		if rp := c.Props.Get("REPEAT"); rp != nil {
			n, e := rp.Int()
			if e != nil || n < 0 || n > 100 {
				return nil, errors.New("VALARM repeat limit exceeded")
			}
			repeat = n
			dp := c.Props.Get("DURATION")
			if dp == nil {
				return nil, errors.New("VALARM repeat duration missing")
			}
			interval, err = dp.Duration()
			if err != nil || interval <= 0 {
				return nil, errors.New("invalid VALARM repeat duration")
			}
		}
		for i := 0; i <= repeat; i++ {
			v := at.Add(time.Duration(i) * interval)
			sum := sha256.Sum256([]byte(p.Value + "|" + p.Params.Get("RELATED") + fmt.Sprint(i)))
			alarms = append(alarms, Alarm{hex.EncodeToString(sum[:8]), v})
		}
	}
	if !has {
		at := in.Start.Add(-cfg.Reminder.DefaultBefore.Value())
		if in.View.AllDay {
			clock, _ := time.Parse("15:04", cfg.Reminder.AllDayAt)
			at = time.Date(in.Start.Year(), in.Start.Month(), in.Start.Day(), clock.Hour(), clock.Minute(), 0, 0, in.Start.Location())
		}
		alarms = append(alarms, Alarm{"default", at})
	}
	return alarms, nil
}
func InputTime(s, zone string, day bool) (time.Time, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, errors.New("unknown timezone")
	}
	if day {
		return time.ParseInLocation("2006-01-02", s, loc)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return t, errors.New("date-time must be RFC3339 with offset")
	}
	return t.In(loc), nil
}
func Create(kind string, p Patch, zone string) (*ical.Calendar, error) {
	cal := ical.NewCalendar()
	cal.Props.SetText("VERSION", "2.0")
	cal.Props.SetText("PRODID", "-//radicale-extend//EN")
	comp := ical.NewComponent(kind)
	comp.Props.SetText("UID", uuid.NewString())
	cal.Children = append(cal.Children, comp)
	if p.Summary == nil || *p.Summary == "" {
		return nil, errors.New("summary required")
	}
	if kind == "VEVENT" && p.Start == nil {
		return nil, errors.New("event start required")
	}
	if err := Apply(comp, p, zone); err != nil {
		return nil, err
	}
	return cal, nil
}
func Apply(comp *ical.Component, p Patch, zone string) error {
	if (comp.Name == "VEVENT" && p.Due != nil) || (comp.Name == "VTODO" && p.End != nil) {
		return errors.New("VEVENT uses end; VTODO uses due")
	}
	if p.Status != nil {
		allowed := map[string]bool{"": true, "CANCELLED": true}
		if comp.Name == "VEVENT" {
			allowed["TENTATIVE"] = true
			allowed["CONFIRMED"] = true
		} else {
			allowed["NEEDS-ACTION"] = true
			allowed["IN-PROCESS"] = true
			allowed["COMPLETED"] = true
		}
		if !allowed[*p.Status] {
			return errors.New("invalid component status")
		}
	}
	if p.Timezone != "" {
		zone = p.Timezone
	}
	day := false
	if prop := comp.Props.Get("DTSTART"); prop != nil {
		day = prop.ValueType() == ical.ValueDate
	} else if prop := comp.Props.Get("DUE"); prop != nil {
		day = prop.ValueType() == ical.ValueDate
	}
	if p.AllDay != nil {
		if *p.AllDay != day && comp.Props.Get("DTSTART") != nil && p.Start == nil {
			return errors.New("changing all_day requires start")
		}
		day = *p.AllDay
	}
	for key, v := range map[string]*string{"SUMMARY": p.Summary, "DESCRIPTION": p.Description, "LOCATION": p.Location, "STATUS": p.Status} {
		if v != nil {
			if strings.ContainsAny(*v, "\x00") {
				return errors.New("invalid text")
			}
			comp.Props.SetText(key, *v)
		}
	}
	for key, v := range map[string]*string{"DTSTART": p.Start, "DTEND": p.End, "DUE": p.Due} {
		if v == nil {
			continue
		}
		if *v == "" {
			comp.Props.Del(key)
			continue
		}
		t, err := InputTime(*v, zone, day)
		if err != nil {
			return err
		}
		setTime(comp, key, t, day)
	}
	if p.End != nil || p.Due != nil {
		comp.Props.Del("DURATION")
	}
	if p.RRULE != nil {
		if *p.RRULE == "" {
			comp.Props.Del("RRULE")
		} else {
			if _, err := rrule.StrToROption(*p.RRULE); err != nil {
				return errors.New("invalid RRULE")
			}
			prop := ical.NewProp("RRULE")
			prop.Value = *p.RRULE
			comp.Props.Set(prop)
		}
	}
	for key, values := range map[string]*[]string{"RDATE": p.RDates, "EXDATE": p.ExDates} {
		if values == nil {
			continue
		}
		comp.Props.Del(key)
		for _, v := range *values {
			t, err := InputTime(v, zone, day)
			if err != nil {
				return err
			}
			prop := ical.NewProp(key)
			if day {
				prop.SetDate(t)
			} else {
				prop.SetDateTime(t)
			}
			comp.Props.Add(prop)
		}
	}
	if Text(comp, "STATUS") == "COMPLETED" {
		comp.Props.SetDateTime("COMPLETED", time.Now().UTC())
		comp.Props.SetText("PERCENT-COMPLETE", "100")
	} else {
		comp.Props.Del("COMPLETED")
		if p.Status != nil {
			comp.Props.Del("PERCENT-COMPLETE")
		}
	}
	comp.Props.SetDateTime("DTSTAMP", time.Now().UTC())
	comp.Props.SetDateTime("LAST-MODIFIED", time.Now().UTC())
	seq := 0
	if prop := comp.Props.Get("SEQUENCE"); prop != nil {
		seq, _ = prop.Int()
	}
	sequence := ical.NewProp("SEQUENCE")
	sequence.Value = strconv.Itoa(seq + 1)
	comp.Props.Set(sequence)
	return validateBounds(comp, zone)
}
func validateBounds(comp *ical.Component, zone string) error {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return err
	}
	_, _, _, err = bounds(comp, loc)
	return err
}
func Mutate(cal *ical.Calendar, kind, rid string, p Patch, remove bool, zone string) error {
	master, err := Master(cal, kind)
	if err != nil {
		return err
	}
	if rid == "" {
		if remove {
			return errors.New("whole resource deletion belongs to DAV")
		}
		return Apply(master, p, zone)
	}
	if p.RRULE != nil || p.RDates != nil || p.ExDates != nil {
		return errors.New("recurrence rule edits require a whole-series update")
	}
	loc, _ := time.LoadLocation(zone)
	_, _, day, err := bounds(master, loc)
	if err != nil {
		return err
	}
	t, err := InputTime(rid, zone, day)
	if err != nil {
		return err
	}
	key := dateKey(t, day)
	instances, err := ExpandWithStatus(cal, t.Add(-366*24*time.Hour), t.Add(366*24*time.Hour), loc, 100000, true)
	if err != nil {
		return err
	}
	var target *ical.Component
	for _, in := range instances {
		if in.Key == key {
			target = clone(in.Component)
			break
		}
	}
	if target == nil {
		return errors.New("recurrence instance not found")
	}
	children := cal.Children[:0]
	for _, c := range cal.Children {
		if c.Name == kind && c.Props.Get("RECURRENCE-ID") != nil {
			r, e := Date(c, "RECURRENCE-ID", loc)
			if e == nil && dateKey(r, day) == key {
				continue
			}
		}
		children = append(children, c)
	}
	cal.Children = children
	if remove {
		prop := ical.NewProp("EXDATE")
		if day {
			prop.SetDate(t)
		} else {
			prop.SetDateTime(t)
		}
		master.Props.Add(prop)
		return nil
	}
	for _, name := range []string{"RRULE", "RDATE", "EXDATE"} {
		target.Props.Del(name)
	}
	setTime(target, "RECURRENCE-ID", t, day)
	if err = Apply(target, p, zone); err != nil {
		return err
	}
	cal.Children = append(cal.Children, target)
	return nil
}
