package telegrambot

import (
	"context"
	"strings"
	"testing"
	"time"

	"radicale-extend/internal/service"
	"radicale-extend/internal/store"
)

type editor struct {
	calls                int
	writeCalls           int
	channel, mode, title string
	before               int64
	series               bool
	ref                  store.EventReference
	err                  error
}

func (e *editor) EventRules(_ context.Context, r store.EventReference) (service.EventDetails, error) {
	e.calls++
	e.ref = r
	return service.EventDetails{Summary: "event", Rules: []service.EventChannelRule{{Channel: "smtp", Mode: "before", BeforeSeconds: 1800, Enabled: false}, {Channel: "telegram", Mode: "off", Enabled: true}}}, e.err
}
func (e *editor) SetEventReminder(ctx context.Context, r store.EventReference, ch, mode string, before int64, series bool) (service.EventDetails, error) {
	e.writeCalls++
	e.channel, e.mode, e.before, e.series = ch, mode, before, series
	return e.EventRules(ctx, r)
}
func (e *editor) EditEventTitle(_ context.Context, r store.EventReference, title string, series bool) (string, error) {
	e.writeCalls++
	e.calls++
	e.ref, e.title, e.series = r, title, series
	return "\"updated\"", e.err
}

func selectedBot(t *testing.T) (*Bot, *editor, string) {
	t.Helper()
	b, q, _ := testBot(t)
	e := &editor{}
	b.Editor = e
	q.events = []service.AgendaEvent{{Summary: "event", Start: b.Now().Add(time.Hour), End: b.Now().Add(2 * time.Hour), Resource: "/alice/work/a.ics", Calendar: "/alice/work/", UID: "a", Instance: "2026-10-28T02:00:00Z", ETag: "\"old\""}}
	text := strings.Join(b.reply(context.Background(), update(1, "/next", "private", 123)), "\n")
	var id string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "ID：") {
			id = strings.Split(strings.TrimPrefix(line, "ID："), "（")[0]
		}
	}
	if len(id) != 20 {
		t.Fatal("missing event ID", text)
	}
	return b, e, id
}

func TestEventReminderAndTitleCommands(t *testing.T) {
	b, e, id := selectedBot(t)
	ctx := context.Background()
	text := strings.Join(b.reply(ctx, update(2, "/remind "+id+" smtp 30m", "private", 123)), "\n")
	if e.channel != "smtp" || e.before != 1800 || e.mode != "before" || e.series || !strings.Contains(text, "不会发送") {
		t.Fatal(e, text)
	}
	b.reply(ctx, update(3, "/remind "+id+" telegram off", "private", 123))
	if e.channel != "telegram" || e.mode != "off" || e.series {
		t.Fatal(e)
	}
	b.reply(ctx, update(4, "/remind_series "+id+" all default", "private", 123))
	if e.channel != "all" || e.mode != "default" || !e.series {
		t.Fatal(e)
	}
	b.reply(ctx, update(5, "/title@test_bot "+id+" 新的 会议 标题", "private", 123))
	if e.title != "新的 会议 标题" || e.series || e.ref.Instance == "" {
		t.Fatal(e)
	}
	ref, err := b.Store.EventReference(ctx, b.scope, id, b.Now())
	if err != nil || ref.ETag != "\"updated\"" {
		t.Fatal("title edit did not update selection", ref, err)
	}
	b.reply(ctx, update(6, "/title_series "+id+" 全系列标题", "private", 123))
	if !e.series {
		t.Fatal(e)
	}
	b.reply(ctx, update(7, "/event "+id, "private", 123))
	if e.ref.UID != "a" {
		t.Fatal(e)
	}
}

func TestEventEditAuthorizationAndStaleSelections(t *testing.T) {
	b, e, id := selectedBot(t)
	ctx := context.Background()
	for _, text := range []string{"/remind " + id + " smtp -1m", "/remind " + id + " smtp 0.1s", "/remind " + id + " smtp 9000h", "/remind " + id + " unknown 5m", "/title " + id + " " + strings.Repeat("x", 201), "/title missing new"} {
		b.reply(ctx, update(2, text, "private", 123))
	}
	if e.calls != 0 {
		t.Fatal("invalid command reached editor", e.calls)
	}
	for _, u := range []Update{update(3, "/title "+id+" new", "private", 999), update(4, "/remind "+id+" all off", "group", 123), update(5, "/title@other_bot "+id+" new", "private", 123)} {
		if got := b.reply(ctx, u); len(got) != 0 {
			t.Fatal(got)
		}
	}
	if e.calls != 0 {
		t.Fatal("unauthorized editing", e.calls)
	}
	e.err = service.ErrEventChanged
	if got := strings.Join(b.reply(ctx, update(6, "/title "+id+" new", "private", 123)), ""); !strings.Contains(got, "事件已被修改") {
		t.Fatal(got)
	}
	// The same ID belongs only to this Bot/chat scope, and expires after 24h.
	b.scope = "other-chat"
	if got := strings.Join(b.reply(ctx, update(7, "/event "+id, "private", 123)), ""); !strings.Contains(got, "已过期") {
		t.Fatal(got)
	}
	b.scope = "test-bot"
	now := b.Now().Add(25 * time.Hour)
	b.Now = func() time.Time { return now }
	if got := strings.Join(b.reply(ctx, update(8, "/event "+id, "private", 123)), ""); !strings.Contains(got, "已过期") {
		t.Fatal(got)
	}
}

func TestEventQueryIsReadOnlyAndLegacyCommandIsRemoved(t *testing.T) {
	b, e, id := selectedBot(t)
	text := strings.Join(b.reply(context.Background(), update(2, "/event@test_bot "+id, "private", 123)), "\n")
	for _, command := range []string{"/reminders", "/reminders " + id, "/reminders@test_bot " + id} {
		if reply := b.reply(context.Background(), update(3, command, "private", 123)); len(reply) != 0 {
			t.Fatal("removed command is still handled", command, reply)
		}
	}
	if e.calls != 1 || e.writeCalls != 0 || !strings.Contains(text, "事件及提醒规则") || !strings.Contains(text, "smtp：提前 30m") || !strings.Contains(text, "telegram：关闭") || !strings.Contains(text, "不会发送") {
		t.Fatal(e, text)
	}
	if e.ref.Instance == "" {
		t.Fatal("lost occurrence scope")
	}
	for _, u := range []Update{update(3, "/event "+id, "private", 999), update(4, "/event "+id, "group", 123), update(5, "/event@other_bot "+id, "private", 123)} {
		if reply := b.reply(context.Background(), u); len(reply) != 0 {
			t.Fatal("unauthorized reminder lookup", reply)
		}
	}
	if e.calls != 1 || e.writeCalls != 0 {
		t.Fatal("unauthorized request reached editor", e)
	}
	if strings.Contains(help, "/reminders") || !strings.Contains(help, "/event ID") {
		t.Fatal("help advertises duplicate query command", help)
	}
}

func TestRuleViewShowsConcreteDefaultsAndWarnings(t *testing.T) {
	details := service.EventDetails{Summary: "private title", Hidden: true, Rules: []service.EventChannelRule{
		{Channel: "smtp", Mode: "default", AllDayAt: "09:00", Enabled: true},
		{Channel: "telegram", Mode: "default", BeforeSeconds: 900, Enabled: true},
	}}
	text := strings.Join(ruleLines(details), "\n")
	if !strings.Contains(text, "当天 09:00") || !strings.Contains(text, "默认提前 15m") || strings.Contains(text, "private title") {
		t.Fatal(text)
	}
	details.Rules = []service.EventChannelRule{{Channel: "telegram", Mode: "valarm", Enabled: true, TriggerCount: 5, TriggerTimes: []string{"2026-10-28 09:55:00 +08:00", "2026-10-28 09:57:00 +08:00", "2026-10-28 09:59:00 +08:00"}}}
	text = strings.Join(ruleLines(details), "\n")
	if !strings.Contains(text, "日历自带 VALARM") || !strings.Contains(text, "共 5 次触发") || !strings.Contains(text, "+08:00") {
		t.Fatal(text)
	}
	details.Rules[0].TriggerTimes = nil
	details.Rules[0].SchedulingError = "无法可靠计算"
	if text = strings.Join(ruleLines(details), "\n"); !strings.Contains(text, "无法可靠计算") {
		t.Fatal(text)
	}
}
