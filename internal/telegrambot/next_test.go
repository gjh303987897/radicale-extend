package telegrambot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"radicale-extend/internal/service"
)

func TestNextOptionalCount(t *testing.T) {
	for _, tc := range []struct {
		command string
		count   int
	}{
		{"/next", 5}, {"/next 1", 1}, {"/next 10", 10}, {"/next 20", 20}, {"/next@test_bot 3", 3}, {" /next   7 \n", 7}, {"最近5个日程", 5},
	} {
		t.Run(tc.command, func(t *testing.T) {
			b, q, _ := testBot(t)
			for i := 1; i <= 25; i++ {
				q.events = append(q.events, service.AgendaEvent{Summary: fmt.Sprintf("event-%02d", i), Start: b.Now().Add(time.Duration(i) * time.Hour), End: b.Now().Add(time.Duration(i+1) * time.Hour), Resource: fmt.Sprintf("/alice/work/%d.ics", i), Calendar: "/alice/work/", UID: fmt.Sprint(i), ETag: "\"snapshot\""})
			}
			text := strings.Join(b.reply(context.Background(), update(1, tc.command, "private", 123)), "\n")
			if q.calls != 1 || q.limit != tc.count || !q.from.Equal(b.Now()) || q.to.Sub(q.from) != 366*24*time.Hour {
				t.Fatal("wrong agenda bounds or count", q)
			}
			if !strings.Contains(text, fmt.Sprintf("最近 %d 个日程", tc.count)) || strings.Count(text, "ID：") != tc.count || strings.Contains(text, fmt.Sprintf("event-%02d", tc.count+1)) {
				t.Fatal("reply count did not match N", text)
			}
			var refs int
			if err := b.Store.DB.QueryRow("SELECT count(*) FROM telegram_event_refs WHERE scope=?", b.scope).Scan(&refs); err != nil || refs != tc.count {
				t.Fatal("saved references outside result limit", refs, err)
			}
		})
	}
}

func TestNextRejectsInvalidCountBeforeQuery(t *testing.T) {
	for _, command := range []string{"/next 0", "/next -1", "/next 21", "/next 1.5", "/next abc", "/next 999999999999999999999999", "/next 5 extra"} {
		t.Run(command, func(t *testing.T) {
			b, q, _ := testBot(t)
			text := strings.Join(b.reply(context.Background(), update(1, command, "private", 123)), "\n")
			if q.calls != 0 || !strings.Contains(text, "用法：/next [N]") || !strings.Contains(text, "1–20") {
				t.Fatal("invalid count reached DAV or had no usage", q.calls, text)
			}
		})
	}
	b, q, _ := testBot(t)
	for _, u := range []Update{update(2, "/next 20", "private", 999), update(3, "/next 20", "group", 123), update(4, "/next@other_bot 20", "private", 123)} {
		if reply := b.reply(context.Background(), u); len(reply) != 0 {
			t.Fatal("unauthorized query replied", reply)
		}
	}
	if q.calls != 0 {
		t.Fatal("unauthorized query reached DAV")
	}
}
