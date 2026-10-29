package telegrambot

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type weekQueue struct {
	calls  int
	key    string
	queued bool
	err    error
}

func (q *weekQueue) QueueWeek(_ context.Context, key string) (bool, error) {
	q.calls++
	q.key = key
	return q.queued, q.err
}
func (q *weekQueue) QueueDay(ctx context.Context, key string) (bool, error) {
	return q.QueueWeek(ctx, key)
}

func TestDayCommandAndButtonUseAuthorizedImageQueue(t *testing.T) {
	b, _, _ := testBot(t)
	q := &weekQueue{queued: true}
	b.Day = q
	ctx := context.Background()
	callbackServer(t, b)
	for _, u := range []Update{update(30, "/day@test_bot", "private", 123), callbackUpdate(b, 31, "v1:day")} {
		parts := b.respond(ctx, u)
		if len(parts) != 1 || !strings.Contains(parts[0].Body, "图片队列") {
			t.Fatal(parts)
		}
	}
	if q.calls != 2 {
		t.Fatal(q.calls)
	}
	for _, u := range []Update{update(32, "/day", "private", 999), update(33, "/day", "group", 123), update(34, "/day@other_bot", "private", 123), update(35, "/day extra", "private", 123)} {
		b.respond(ctx, u)
	}
	if q.calls != 2 {
		t.Fatal("unauthorized or invalid day request queued", q.calls)
	}
	b.Day = nil
	if text := strings.Join(b.reply(ctx, update(36, "/day", "private", 123)), ""); !strings.Contains(text, "不可用") {
		t.Fatal(text)
	}
}
func TestWeekCommandQueuesOnlyAuthorizedPrivateRequests(t *testing.T) {
	b, _, _ := testBot(t)
	q := &weekQueue{queued: true}
	b.Week = q
	ctx := context.Background()
	text := strings.Join(b.reply(ctx, update(10, "/week@test_bot", "private", 123)), "\n")
	if q.calls != 1 || q.key == "" || !strings.Contains(text, "图片队列") {
		t.Fatal(q, text)
	}
	first := q.key
	q.queued = false
	if text = strings.Join(b.reply(ctx, update(10, "/week", "private", 123)), ""); q.key != first || !strings.Contains(text, "无需重复") {
		t.Fatal(q, text)
	}
	for _, u := range []Update{update(11, "/week", "private", 999), update(12, "/week", "group", 123), update(13, "/week@other_bot", "private", 123)} {
		if got := b.reply(ctx, u); len(got) != 0 {
			t.Fatal(got)
		}
	}
	if q.calls != 2 {
		t.Fatal("unauthorized week request queued", q.calls)
	}
	if text = strings.Join(b.reply(ctx, update(14, "/week 7", "private", 123)), ""); q.calls != 2 || !strings.Contains(text, "用法") {
		t.Fatal(q, text)
	}
	q.err = errors.New("private connection details")
	if text = strings.Join(b.reply(ctx, update(15, "/week", "private", 123)), ""); !strings.Contains(text, "未能") || strings.Contains(text, "private") {
		t.Fatal(text)
	}
}
