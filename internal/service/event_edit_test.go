package service

import (
	"strings"
	"testing"
	"time"

	"radicale-extend/internal/calendar"
	"radicale-extend/internal/config"
	"radicale-extend/internal/store"
)

func TestChannelReminderProjectionAndInheritance(t *testing.T) {
	s := testService(t)
	s.Config.Telegram.Enabled = true
	o := object("/alice/work/repeat.ics", "20261028T010000Z", "Series")
	o.Data = strings.Replace(o.Data, "END:VEVENT", "RRULE:FREQ=DAILY;COUNT=2\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT10M\r\nEND:VALARM\r\nEND:VEVENT", 1)
	if err := s.project(o, "/alice/work/", true); err != nil {
		t.Fatal(err)
	}
	first := "2026-10-28T01:00:00Z"
	updates := []store.ReminderRule{
		{Collection: "/alice/work/", UID: "a", Channel: "smtp", Mode: "before", BeforeSeconds: 1800},
		{Collection: "/alice/work/", UID: "a", Instance: first, Channel: "telegram", Mode: "off"},
	}
	if err := s.projectReminderRules(o, "/alice/work/", true, updates); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.Store.ListJobs(o.Path)
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, j := range jobs {
		if j.State != "pending" || j.Kind != "reminder" {
			continue
		}
		live++
		if j.Channel == "telegram" && j.Instance == first {
			t.Fatal("disabled instance still has a reminder")
		}
		at, _ := time.Parse(time.RFC3339, j.Instance)
		before := 10 * time.Minute
		if j.Channel == "smtp" {
			before = 30 * time.Minute
		}
		if j.At != at.Add(-before).Unix() {
			t.Fatal("wrong per-channel trigger", j)
		}
	}
	if live != 3 {
		t.Fatal("channels or instances affected each other", live)
	}
	// Clearing a single override must restore VALARM without changing SMTP.
	updates = []store.ReminderRule{{Collection: "/alice/work/", UID: "a", Instance: first, Channel: "telegram"}}
	if err = s.projectReminderRules(o, "/alice/work/", true, updates); err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.Store.ListJobs(o.Path)
	live = 0
	for _, j := range jobs {
		if j.State == "pending" && j.Kind == "reminder" {
			live++
		}
	}
	if live != 4 {
		t.Fatal("default failed to restore alarm", live)
	}
	// A DAV refresh must retain explicit channel rules.
	if err = s.project(o, "/alice/work/", true); err != nil {
		t.Fatal(err)
	}
	rules, err := s.Store.ReminderRules("/alice/work/", "a")
	if err != nil || len(rules) != 1 || rules[0].Channel != "smtp" {
		t.Fatal(rules, err)
	}
	// Priority: occurrence channel > series channel > MCP override.
	rules = append(rules, store.ReminderRule{Channel: "smtp", Instance: first, Mode: "off"})
	if got := effectiveReminder(rules, first, "smtp", calendar.Override{Mode: "before", BeforeSeconds: 1}); got.Mode != "off" {
		t.Fatal(got)
	}
	if got := effectiveReminder(rules, "other", "smtp", calendar.Override{Mode: "before", BeforeSeconds: 1}); got.BeforeSeconds != 1800 {
		t.Fatal(got)
	}
}

func TestReminderRulesAndJobsRollbackTogether(t *testing.T) {
	s := testService(t)
	o := object("/alice/work/a.ics", "20261028T010000Z", "A")
	if err := s.project(o, "/alice/work/", true); err != nil {
		t.Fatal(err)
	}
	// Reject job creation after the rule is staged, to exercise rollback.
	_, err := s.Store.DB.Exec("CREATE TRIGGER reject_new_reminder BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT,'test failure'); END")
	if err != nil {
		t.Fatal(err)
	}
	err = s.projectReminderRules(o, "/alice/work/", true, []store.ReminderRule{{Collection: "/alice/work/", UID: "a", Channel: "smtp", Mode: "before", BeforeSeconds: 1800}})
	if err == nil {
		t.Fatal("failed reminder transaction committed")
	}
	rules, err := s.Store.ReminderRules("/alice/work/", "a")
	if err != nil || len(rules) != 0 {
		t.Fatal("rule escaped failed transaction", rules, err)
	}
	jobs, _ := s.Store.ListJobs(o.Path)
	if len(jobs) != 1 || jobs[0].State != "pending" {
		t.Fatal("original reminder cancelled on rollback", jobs)
	}
}

func TestChannelRuleHorizonAndRestart(t *testing.T) {
	s := testService(t)
	start := s.Now().AddDate(0, 0, 40)
	o := object("/alice/work/future.ics", start.Format("20060102T150405Z"), "Future")
	updates := []store.ReminderRule{{Collection: "/alice/work/", UID: "a", Channel: "smtp", Mode: "before", BeforeSeconds: 20 * 86400}}
	if err := s.projectReminderRules(o, "/alice/work/", true, updates); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.Store.ListJobs(o.Path)
	if err != nil || len(jobs) != 1 || jobs[0].At != start.AddDate(0, 0, -20).Unix() {
		t.Fatal("channel advance did not extend occurrence window", jobs, err)
	}
	s.Store.Close()
	var openErr error
	s.Store, openErr = store.Open(s.Config.Storage.State, s.Config.Storage.Queue)
	if openErr != nil {
		t.Fatal(openErr)
	}
	t.Cleanup(s.Store.Close)
	if err = s.project(o, "/alice/work/", true); err != nil {
		t.Fatal(err)
	}
	rules, err := s.Store.ReminderRules("/alice/work/", "a")
	if err != nil || len(rules) != 1 || rules[0].BeforeSeconds != 20*86400 {
		t.Fatal("restart lost channel override", rules, err)
	}
	// Per-event rules cannot bypass a calendar-level reminder switch.
	disabled := false
	s.Config.Calendars = append(s.Config.Calendars, config.Calendar{Path: "/alice/work/", Reminders: &disabled})
	if err = s.project(o, "/alice/work/", true); err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.Store.ListJobs(o.Path)
	for _, job := range jobs {
		if job.State == "pending" {
			t.Fatal("calendar reminder switch bypassed", job)
		}
	}
}
