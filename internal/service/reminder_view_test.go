package service

import (
	"strings"
	"testing"

	"radicale-extend/internal/calendar"
	"radicale-extend/internal/store"
)

func TestReminderViewDefaultAllDayAndVALARM(t *testing.T) {
	s := testService(t)
	ref := store.EventReference{Collection: "/alice/work/", UID: "a"}
	inspect := func(data string) EventDetails {
		t.Helper()
		cal, err := calendar.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		comp, err := calendar.Master(cal, "VEVENT")
		if err != nil {
			t.Fatal(err)
		}
		details, err := s.eventRules(ref, comp)
		if err != nil {
			t.Fatal(err)
		}
		return details
	}
	o := object("/alice/work/a.ics", "20261028T010000Z", "private title")
	details := inspect(o.Data)
	if details.Rules[0].Mode != "default" || details.Rules[0].BeforeSeconds != 900 || details.Rules[0].TriggerTimes[0] != "2026-10-28 08:45:00 +08:00" {
		t.Fatal(details)
	}
	if details.Rules[1].Enabled {
		t.Fatal("disabled Telegram reported as enabled")
	}
	day := strings.Replace(o.Data, "DTSTART:20261028T010000Z", "DTSTART;VALUE=DATE:20261028\r\nDTEND;VALUE=DATE:20261029", 1)
	details = inspect(day)
	if details.Rules[0].AllDayAt != "09:00" || details.Rules[0].TriggerTimes[0] != "2026-10-28 09:00:00 +08:00" {
		t.Fatal(details)
	}
	alarm := strings.Replace(o.Data, "END:VEVENT", "DTEND:20261028T020000Z\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER;RELATED=END:-PT5M\r\nREPEAT:4\r\nDURATION:PT2M\r\nEND:VALARM\r\nEND:VEVENT", 1)
	details = inspect(alarm)
	r := details.Rules[0]
	if r.Mode != "valarm" || r.TriggerCount != 5 || len(r.TriggerTimes) != 3 || r.TriggerTimes[0] != "2026-10-28 09:55:00 +08:00" || r.TriggerTimes[2] != "2026-10-28 09:59:00 +08:00" {
		t.Fatal(r)
	}
	broken := strings.Replace(alarm, "TRIGGER;RELATED=END:-PT5M", "TRIGGER:invalid", 1)
	details = inspect(broken)
	if details.Rules[0].SchedulingError == "" || len(details.Rules[0].TriggerTimes) != 0 {
		t.Fatal("unsupported alarm guessed a trigger time", details)
	}
	if rules, err := s.Store.ReminderRules(ref.Collection, ref.UID); err != nil || len(rules) != 0 {
		t.Fatal("inspection created rules", rules, err)
	}
}

func TestReminderViewIncludesEffectiveOverrideSource(t *testing.T) {
	s := testService(t)
	o := object("/alice/work/a.ics", "20261028T010000Z", "A")
	updates := []store.ReminderRule{{Collection: "/alice/work/", UID: "a", Channel: "smtp", Mode: "before", BeforeSeconds: 1800}, {Collection: "/alice/work/", UID: "a", Channel: "telegram", Instance: "2026-10-28T01:00:00Z", Mode: "off"}}
	if err := s.projectReminderRules(o, "/alice/work/", true, updates); err != nil {
		t.Fatal(err)
	}
	cal, _ := calendar.Parse(o.Data)
	comp, _ := calendar.Master(cal, "VEVENT")
	details, err := s.eventRules(store.EventReference{Collection: "/alice/work/", UID: "a", Instance: "2026-10-28T01:00:00Z"}, comp)
	if err != nil {
		t.Fatal(err)
	}
	if details.Rules[0].Mode != "before" || details.Rules[0].Source != "整个系列/事件" || details.Rules[0].BeforeSeconds != 1800 {
		t.Fatal(details)
	}
	if details.Rules[1].Mode != "off" || details.Rules[1].Source != "仅本次实例" || details.Rules[1].TriggerCount != 0 {
		t.Fatal(details)
	}
}
