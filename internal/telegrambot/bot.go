// Package telegrambot provides the optional private-chat command interface.
package telegrambot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"radicale-extend/internal/config"
	"radicale-extend/internal/notify"
	"radicale-extend/internal/service"
	"radicale-extend/internal/store"
)

type Agenda interface {
	Agenda(context.Context, time.Time, time.Time, int) ([]service.AgendaEvent, error)
}

type EventEditor interface {
	EventRules(context.Context, store.EventReference) (service.EventDetails, error)
	SetEventReminder(context.Context, store.EventReference, string, string, int64, bool) (service.EventDetails, error)
	EditEventTitle(context.Context, store.EventReference, string, bool) (string, error)
}

type Bot struct {
	Config          *config.Config
	Store           *store.Store
	Agenda          Agenda
	Editor          EventEditor
	HTTP            *http.Client
	Sender          notify.Sender
	Now             func() time.Time
	scope, username string
}

func New(s *service.Service) *Bot {
	return &Bot{Config: s.Config, Store: s.Store, Agenda: s, Editor: s, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Sender: notify.New(s.Config), Now: time.Now, scope: service.Hash("telegram-commands", s.Config.Telegram.Token.Value, s.Config.Telegram.ChatID)}
}

func (b *Bot) call(ctx context.Context, method string, args any, result any) error {
	body, err := json.Marshal(args)
	if err != nil {
		return errors.New("Telegram request encoding failed")
	}
	u := strings.TrimRight(b.Config.Telegram.APIURL, "/") + "/bot" + b.Config.Telegram.Token.Value + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return errors.New("Telegram request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return errors.New("Telegram connection failed")
	}
	defer resp.Body.Close()
	var envelope struct {
		OK         bool            `json:"ok"`
		Code       int             `json:"error_code"`
		Result     json.RawMessage `json:"result"`
		Parameters struct {
			Retry int `json:"retry_after"`
		} `json:"parameters"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope)
	if err != nil {
		return errors.New("invalid Telegram response")
	}
	if !envelope.OK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := envelope.Code
		if code == 0 {
			code = resp.StatusCode
		}
		return &notify.Error{Permanent: code >= 400 && code < 500 && code != 429 && code != 409, RetryAfter: time.Duration(max(0, envelope.Parameters.Retry)) * time.Second, Message: fmt.Sprintf("Telegram API failed (status %d)", code)}
	}
	if result != nil && json.Unmarshal(envelope.Result, result) != nil {
		return errors.New("invalid Telegram result")
	}
	return nil
}

func (b *Bot) setup(ctx context.Context) error {
	chat, err := strconv.ParseInt(b.Config.Telegram.ChatID, 10, 64)
	if err != nil || chat <= 0 {
		return errors.New("Telegram commands require a numeric private chat ID")
	}
	var webhook struct {
		URL string `json:"url"`
	}
	if err = b.call(ctx, "getWebhookInfo", map[string]any{}, &webhook); err != nil {
		return err
	}
	if webhook.URL != "" {
		return errors.New("Telegram commands require long polling; an existing webhook is configured")
	}
	var me struct {
		Username string `json:"username"`
	}
	if err = b.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return err
	}
	if me.Username == "" {
		return errors.New("Telegram bot username missing")
	}
	b.username = me.Username
	return b.call(ctx, "setMyCommands", map[string]any{
		"scope": map[string]any{"type": "chat", "chat_id": chat},
		"commands": []map[string]string{
			{"command": "today", "description": "列出今天的日程"},
			{"command": "next", "description": "列出最近日程：/next [N]，默认 5 条，最多 20 条"},
			{"command": "event", "description": "查看事件及提醒规则：/event ID"},
			{"command": "title", "description": "编辑选中事件标题：/title ID 新标题"},
			{"command": "remind", "description": "修改提醒规则：/remind ID smtp|telegram|all 15m|off|default"},
			{"command": "title_series", "description": "编辑整个重复系列标题"},
			{"command": "remind_series", "description": "设置整个重复系列的渠道提醒规则"},
			{"command": "help", "description": "查看使用说明"},
		},
	}, nil)
}

type Update struct {
	ID      int64 `json:"update_id"`
	Message *struct {
		Date int64  `json:"date"`
		Text string `json:"text"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		From struct {
			ID    int64 `json:"id"`
			IsBot bool  `json:"is_bot"`
		} `json:"from"`
	} `json:"message"`
}

const help = "日程管理\n/today — 今天的日程（含全天、跨天和已结束日程）\n/next [N] — 从现在起最近 N 个日程，默认 5 条，N 为 1–20（含进行中日程，查询未来 366 天）\n/event ID — 查看事件及提醒规则\n/title ID 新标题 — 编辑标题\n/remind ID smtp|telegram|all 15m|1h|off|default — 修改提醒规则：提前量、关闭或恢复继承\n重复日程默认仅修改选中的那一次；/title_series 和 /remind_series 修改整个系列（已有单次提醒覆盖仍优先）。\nID 来自日程列表，有效期 24 小时。标题最多 200 字。SMTP 等渠道仍需在服务端启用。\n/help — 使用说明\n按服务器配置的时区显示。"

func (b *Bot) reply(ctx context.Context, u Update) []string {
	m := u.Message
	if m == nil || m.Chat.Type != "private" || strconv.FormatInt(m.Chat.ID, 10) != b.Config.Telegram.ChatID || m.From.ID != m.Chat.ID || m.From.IsBot {
		return nil
	}
	// Old Telegram updates should not produce stale personal replies on startup.
	if m.Date > 0 && b.Now().Sub(time.Unix(m.Date, 0)) > 10*time.Minute {
		return nil
	}
	fields := strings.Fields(m.Text)
	if len(fields) == 0 {
		return nil
	}
	command := fields[0]
	if base, target, ok := strings.Cut(command, "@"); ok {
		if !strings.EqualFold(target, b.username) {
			return nil
		}
		command = base
	}
	if command == "/event" || command == "/reminders" || command == "/title" || command == "/title_series" || command == "/remind" || command == "/remind_series" {
		return b.editReply(ctx, command, fields[1:])
	}
	limit := 5
	if command == "/next" {
		const usage = "用法：/next [N]，N 必须为 1–20 的整数，不填默认 5 条。"
		if len(fields) > 2 {
			return []string{usage}
		}
		if len(fields) == 2 {
			n, err := strconv.Atoi(fields[1])
			if err != nil || n < 1 || n > 20 {
				return []string{usage}
			}
			limit = n
		}
	} else if len(fields) != 1 {
		return nil
	}
	switch command {
	case "/start", "/help":
		return []string{help + "\n时区：" + b.Config.Timezone}
	case "/today", "今日日程", "今天的日程", "/next", "最近5个日程", "最近的5个日程":
	default:
		return nil
	}
	loc, err := time.LoadLocation(b.Config.Timezone)
	if err != nil {
		return []string{"时区配置错误，请联系管理员。"}
	}
	now := b.Now().In(loc)
	from, to := now, now.AddDate(0, 0, 366)
	heading := fmt.Sprintf("最近 %d 个日程（含进行中，未来 366 天）", limit)
	if command == "/today" || command == "今日日程" || command == "今天的日程" {
		from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		to = from.AddDate(0, 0, 1)
		limit = 101
		heading = "今日日程 · " + from.Format("2006-01-02")
	}
	queryCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	events, err := b.Agenda.Agenda(queryCtx, from, to, limit)
	if err != nil {
		return []string{"日程查询失败：日历服务暂不可用、日程数据不受支持或超出查询上限。请稍后重试。"}
	}
	if len(events) > limit {
		events = events[:limit]
	}
	lines := []string{heading, "时区：" + b.Config.Timezone}
	if len(events) == 0 {
		lines = append(lines, "这个时间范围内没有日程。")
	}
	truncated := len(events) > 100
	if truncated {
		events = events[:100]
	}
	refs := make([]store.EventReference, 0, len(events))
	ids := make([]string, len(events))
	for i, e := range events {
		if e.Resource == "" || e.UID == "" || e.ETag == "" {
			continue
		}
		id := service.Hash(b.scope, e.Calendar, e.UID, e.Instance)[:20]
		ids[i] = id
		refs = append(refs, store.EventReference{ID: id, Resource: e.Resource, Collection: e.Calendar, UID: e.UID, Instance: e.Instance, ETag: e.ETag, Expires: b.Now().Add(24 * time.Hour).Unix()})
	}
	if len(refs) > 0 {
		if err = b.Store.SaveEventReferences(ctx, b.scope, refs, b.Now()); err != nil {
			return []string{"暂时无法保存事件 ID，请稍后重试。"}
		}
	}
	for i, e := range events {
		label := clean(e.Summary, 160)
		if label == "" {
			label = "（无标题）"
		}
		if e.Hidden {
			label = "（日程详情已隐藏）"
		}
		start, end := e.Start.In(loc), e.End.In(loc)
		when := start.Format("01-02 15:04")
		if end.After(start) {
			when += " → " + end.Format("01-02 15:04")
		}
		if e.AllDay {
			when = start.Format("01-02") + " 全天"
			if last := end.AddDate(0, 0, -1); last.Format("2006-01-02") != start.Format("2006-01-02") {
				when = start.Format("01-02") + " → " + last.Format("01-02") + " 全天"
			}
		}
		lines = append(lines, fmt.Sprintf("\n%d. %s\n%s", i+1, label, when))
		if ids[i] != "" {
			scope := "事件"
			if e.Instance != "" {
				scope = "重复系列的本次实例"
			}
			lines = append(lines, "ID："+ids[i]+"（"+scope+"）")
		}
		if !e.Hidden && e.Location != "" {
			lines = append(lines, "地点："+clean(e.Location, 120))
		}
	}
	if truncated {
		lines = append(lines, "\n日程较多，仅展示最早的 100 个。")
	}
	if len(refs) > 0 {
		lines = append(lines, "\n查看事件及提醒规则：/event ID\n改标题：/title ID 新标题\n修改提醒规则：/remind ID telegram 15m")
	}
	return pages(lines)
}

func clean(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	r := []rune(strings.TrimSpace(text))
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return string(r)
}

// At most 1500 Unicode scalars per page, hence at most 3000 UTF-16 units.
func pages(lines []string) []string {
	out := []string{}
	page := ""
	for _, line := range lines {
		if len([]rune(page))+len([]rune(line))+1 > 1500 {
			out = append(out, page)
			page = ""
		}
		if page != "" {
			page += "\n"
		}
		page += line
	}
	if page != "" {
		out = append(out, page)
	}
	return out
}

func (b *Bot) poll(ctx context.Context) error {
	offset, err := b.Store.TelegramOffset(ctx, b.scope)
	if err != nil {
		return err
	}
	var updates []Update
	if err = b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 20, "limit": 20, "allowed_updates": []string{"message"}}, &updates); err != nil {
		return err
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].ID < updates[j].ID })
	for _, u := range updates {
		if u.ID < offset {
			continue
		}
		replies := b.reply(ctx, u)
		if err = b.Store.SaveTelegramUpdate(ctx, b.scope, u.ID, replies, b.Now()); err != nil {
			return err
		}
		offset = u.ID + 1
	}
	return nil
}

func (b *Bot) flush(ctx context.Context) error {
	for i := 0; i < 10; i++ {
		r, err := b.Store.NextTelegramReply(ctx, b.scope, b.Now())
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		err = b.Sender.Send(ctx, store.Job{Channel: "telegram", Body: r.Body})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state, due := "done", b.Now()
		if err != nil {
			r.Attempts++
			state = "pending"
			due = due.Add(time.Duration(1<<min(r.Attempts, 10)) * time.Second)
			var detail *notify.Error
			if errors.As(err, &detail) {
				if detail.RetryAfter > due.Sub(b.Now()) {
					due = b.Now().Add(detail.RetryAfter)
				}
				if detail.Permanent {
					state = "failed"
				}
			}
			if r.Attempts >= 8 {
				state = "failed"
			}
			slog.Warn("Telegram command reply failed", "update", r.UpdateID, "attempt", r.Attempts, "terminal", state == "failed")
		}
		if err = b.Store.FinishTelegramReply(ctx, b.scope, r, state, due); err != nil {
			return err
		}
	}
	return nil
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (b *Bot) Run(ctx context.Context) {
	if !b.Config.Telegram.Enabled || !b.Config.Telegram.CommandsEnabled {
		return
	}
	for ctx.Err() == nil {
		if err := b.setup(ctx); err == nil {
			break
		} else {
			// All API errors are sanitized; never log request URLs or chat text.
			slog.Warn("Telegram command setup failed", "reason", err.Error())
			delay := 30 * time.Second
			var detail *notify.Error
			if errors.As(err, &detail) && detail.RetryAfter > delay {
				delay = detail.RetryAfter
			}
			if !wait(ctx, delay) {
				return
			}
		}
	}
	slog.Info("Telegram commands started")
	for ctx.Err() == nil {
		if err := b.flush(ctx); err != nil {
			if !wait(ctx, 5*time.Second) {
				return
			}
			continue
		}
		if err := b.poll(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("Telegram command polling failed", "reason", err.Error())
			delay := 5 * time.Second
			var detail *notify.Error
			if errors.As(err, &detail) && detail.RetryAfter > delay {
				delay = detail.RetryAfter
			}
			if !wait(ctx, delay) {
				return
			}
		} else if !wait(ctx, 200*time.Millisecond) {
			return
		}
	}
}
