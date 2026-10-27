package calendar

import (
	"radicale-extend/internal/config"
	"strings"
	"testing"
	"time"
)

func fixture(body string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" + strings.ReplaceAll(strings.TrimSpace(body), "\n", "\r\n") + "\r\nEND:VCALENDAR\r\n"
}
func TestRecurrencesDSTExceptions(t *testing.T) {
	cal, err := Parse(fixture(`BEGIN:VEVENT
UID:test
DTSTART;TZID=America/New_York:20260307T090000
DTEND;TZID=America/New_York:20260307T100000
SUMMARY:Meeting
RRULE:FREQ=DAILY;COUNT=4
EXDATE;TZID=America/New_York:20260309T090000,20260310T090000
RDATE;TZID=America/New_York:20260312T090000
END:VEVENT
BEGIN:VEVENT
UID:test
RECURRENCE-ID;TZID=America/New_York:20260308T090000
DTSTART;TZID=America/New_York:20260308T120000
DTEND;TZID=America/New_York:20260308T130000
SUMMARY:Moved
END:VEVENT`))
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/New_York")
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
	instances, err := Expand(cal, from, from.AddDate(0, 1, 0), loc, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 3 {
		t.Fatalf("got %d instances", len(instances))
	}
	if instances[0].Start.UTC().Hour() != 14 || instances[1].Start.UTC().Hour() != 16 || instances[2].Start.UTC().Hour() != 13 {
		t.Fatal("DST or exception shift incorrect", instances)
	}
	if instances[1].Key != "2026-03-08T13:00:00Z" {
		t.Fatal("instance key must retain original recurrence ID", instances[1].Key)
	}
}
func TestAlarmPriorityAndRepeat(t *testing.T) {
	cal, _ := Parse(fixture(`BEGIN:VEVENT
UID:a
DTSTART:20261028T120000Z
DTEND:20261028T130000Z
SUMMARY:A
BEGIN:VALARM
ACTION:DISPLAY
TRIGGER;RELATED=END:-PT5M
REPEAT:2
DURATION:PT1M
END:VALARM
END:VEVENT`))
	from := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	instances, err := Expand(cal, from, from.AddDate(0, 0, 1), time.UTC, 10)
	if err != nil {
		t.Fatal(err)
	}
	c := &config.Config{}
	c.Reminder.DefaultBefore = config.Duration(15 * time.Minute)
	alarms, err := Alarms(instances[0], c, Override{})
	if err != nil || len(alarms) != 3 || alarms[0].At.Hour() != 12 || alarms[0].At.Minute() != 55 {
		t.Fatal(alarms, err)
	}
	alarms, err = Alarms(instances[0], c, Override{Mode: "before", BeforeSeconds: 60})
	if err != nil || len(alarms) != 1 || alarms[0].At.Hour() != 11 || alarms[0].At.Minute() != 59 {
		t.Fatal(alarms, err)
	}
	alarms, _ = Alarms(instances[0], c, Override{Mode: "off"})
	if len(alarms) != 0 {
		t.Fatal("off must override VALARM")
	}
}
func TestMutationPreservesUnknownAndSingleDelete(t *testing.T) {
	cal, _ := Parse(fixture(`BEGIN:VEVENT
UID:a
DTSTART:20261028T120000Z
DTEND:20261028T130000Z
RRULE:FREQ=DAILY;COUNT=3
SUMMARY:A
X-CUSTOM:keep
END:VEVENT`))
	summary := "Updated"
	if err := Mutate(cal, "VEVENT", "2026-10-29T12:00:00Z", Patch{Summary: &summary}, false, "UTC"); err != nil {
		t.Fatal(err)
	}
	if len(cal.Children) != 2 || Text(cal.Children[1], "X-CUSTOM") != "keep" || Text(cal.Children[0], "SUMMARY") != "A" {
		t.Fatal("series or unknown fields corrupted")
	}
	if err := Mutate(cal, "VEVENT", "2026-10-30T12:00:00Z", Patch{}, true, "UTC"); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	items, err := Expand(cal, from, from.AddDate(0, 0, 5), time.UTC, 10)
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
}
func TestAllDayFloatingAndUnsupported(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	cal, _ := Parse(fixture(`BEGIN:VEVENT
UID:a
DTSTART;VALUE=DATE:20261028
DTEND;VALUE=DATE:20261029
SUMMARY:A
END:VEVENT`))
	items, err := Expand(cal, time.Date(2026, 10, 27, 0, 0, 0, 0, loc), time.Date(2026, 10, 30, 0, 0, 0, 0, loc), loc, 10)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	c := &config.Config{}
	c.Reminder.AllDayAt = "09:00"
	alarms, _ := Alarms(items[0], c, Override{})
	if alarms[0].At.In(loc).Hour() != 9 {
		t.Fatal("all-day fallback")
	}
	cal.Children[0].Props.Get("DTSTART").Params.Set("TZID", "Unrecognized/Timezone")
	cal.Children[0].Props.Get("DTSTART").Value = "20261028T090000"
	cal.Children[0].Props.Get("DTSTART").Params.Set("VALUE", "DATE-TIME")
	if _, err = Expand(cal, time.Now(), time.Now().AddDate(1, 0, 0), loc, 100); err == nil {
		t.Fatal("unsupported timezone silently accepted")
	}
}
func TestUndatedAndCompletedTasks(t *testing.T) {
	summary := "Task"
	cal, err := Create("VTODO", Patch{Summary: &summary}, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	master, _ := Master(cal, "VTODO")
	v, err := Describe(master, time.UTC)
	if err != nil || v.Start != "" || v.Due != "" {
		t.Fatal(v, err)
	}
	due := "2026-10-28T12:00:00Z"
	status := "COMPLETED"
	if err = Mutate(cal, "VTODO", "", Patch{Due: &due, Status: &status}, false, "UTC"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	items, err := Expand(cal, now, now.AddDate(0, 0, 1), time.UTC, 10)
	if err != nil || len(items) != 0 {
		t.Fatal("completed tasks still schedule", err)
	}
}

func TestAbsoluteAlarmAndFloatingTime(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	cal, _ := Parse(fixture(`BEGIN:VEVENT
UID:absolute
DTSTART:20261028T090000
SUMMARY:Floating
BEGIN:VALARM
ACTION:DISPLAY
TRIGGER;VALUE=DATE-TIME:20261028T003000Z
END:VALARM
END:VEVENT`))
	from := time.Date(2026, 10, 28, 0, 0, 0, 0, loc)
	instances, err := Expand(cal, from, from.AddDate(0, 0, 1), loc, 10)
	if err != nil || len(instances) != 1 {
		t.Fatal(instances, err)
	}
	if instances[0].Start.UTC().Hour() != 1 {
		t.Fatal("floating time did not use configured timezone")
	}
	alarms, err := Alarms(instances[0], &config.Config{}, Override{})
	if err != nil || len(alarms) != 1 || alarms[0].At.UTC().Hour() != 0 || alarms[0].At.Minute() != 30 {
		t.Fatal(alarms, err)
	}
}
