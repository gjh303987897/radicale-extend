package telegrambot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"radicale-extend/internal/dav"
	"radicale-extend/internal/service"
	"radicale-extend/internal/store"
)

func editError(err error) []string {
	if errors.Is(err, service.ErrEventChanged) || dav.IsStatus(err, 412) {
		return []string{"事件已被修改，请刷新 /today 或 /next 后重试。"}
	}
	if dav.IsStatus(err, 404) {
		return []string{"事件已删除或移动，请刷新日程列表。"}
	}
	return []string{"操作结果未确认，请刷新日程列表核对。"}
}

func reminderDuration(seconds int64) string {
	if seconds == 0 {
		return "0s"
	}
	if seconds%3600 == 0 {
		return fmt.Sprintf("%dh", seconds/3600)
	}
	if seconds%60 == 0 {
		if seconds < 3600 {
			return fmt.Sprintf("%dm", seconds/60)
		}
		return fmt.Sprintf("%dh%dm", seconds/3600, seconds%3600/60)
	}
	return (time.Duration(seconds) * time.Second).String()
}

func triggerSummary(values []string) string {
	// Reminders from a repeating VALARM often share one date and offset.
	// Show that context once without dropping nonzero seconds.
	parsed := make([]time.Time, len(values))
	for i, value := range values {
		var err error
		parsed[i], err = time.Parse("2006-01-02 15:04:05 -07:00", value)
		if err != nil {
			return strings.Join(values, "；")
		}
		if i > 0 && parsed[i].Format("2006-01-02 -07:00") != parsed[0].Format("2006-01-02 -07:00") {
			return strings.Join(values, "；")
		}
	}
	if len(parsed) == 0 {
		return ""
	}
	clocks := make([]string, len(parsed))
	for i, value := range parsed {
		format := "15:04"
		if value.Second() != 0 {
			format = "15:04:05"
		}
		clocks[i] = value.Format(format)
	}
	return parsed[0].Format("2006-01-02 ") + strings.Join(clocks, "、") + parsed[0].Format(" -07:00")
}

func ruleLines(details service.EventDetails) []string {
	label := clean(details.Summary, 200)
	if label == "" {
		label = "无标题日程"
	}
	if details.Hidden {
		label = "私人日程"
	}
	lines := []string{label}
	hasTiming := false
	for _, r := range details.Rules {
		value := "继承日历提醒"
		switch r.Mode {
		case "off":
			value = "关闭"
		case "before":
			value = "提前 " + reminderDuration(r.BeforeSeconds)
		case "default":
			if r.AllDayAt != "" {
				value = "默认当天 " + r.AllDayAt
			} else {
				value = "默认提前 " + reminderDuration(r.BeforeSeconds)
			}
		case "valarm":
			value = "日历自带 VALARM"
		}
		channel := r.Channel
		switch channel {
		case "smtp":
			channel = "邮件"
		case "telegram":
			channel = "Telegram"
		}
		status := "渠道启用"
		if !r.Enabled {
			status = "渠道停用，不会发送"
		}
		source := ""
		if r.Source != "" {
			source = " · " + r.Source
		}
		lines = append(lines, fmt.Sprintf("%s：%s%s（%s）", channel, value, source, status))
		if r.SchedulingError != "" {
			lines = append(lines, "⚠ "+clean(r.SchedulingError, 160))
			hasTiming = true
		} else if len(r.TriggerTimes) > 0 {
			times := r.TriggerTimes[:min(3, len(r.TriggerTimes))]
			trigger := "触发：" + triggerSummary(times)
			if r.TriggerCount > len(times) {
				trigger += fmt.Sprintf("（共 %d 次触发，前 %d 次）", r.TriggerCount, len(times))
			}
			lines = append(lines, trigger)
			hasTiming = true
		}
	}
	if hasTiming {
		lines = append(lines, "触发按配置时区计算；实际发送受免打扰等限制。")
	}
	return lines
}

func (b *Bot) editReply(ctx context.Context, command string, args []string) []string {
	if b.Editor == nil {
		return []string{"事件编辑暂不可用。"}
	}
	if len(args) == 0 {
		return []string{help}
	}
	ref, err := b.Store.EventReference(ctx, b.scope, args[0], b.Now())
	if err != nil {
		return []string{"事件 ID 无效或已过期，请刷新 /today 或 /next。"}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	scope := "此事件"
	if ref.Instance != "" {
		scope = "仅本次"
	}
	switch command {
	case "/event":
		if len(args) != 1 {
			return []string{"用法：/event ID"}
		}
		details, err := b.Editor.EventRules(ctx, ref)
		if err != nil {
			return editError(err)
		}
		return pages(append([]string{"事件及提醒规则 · " + scope, "ID：" + ref.ID}, ruleLines(details)...))
	case "/title", "/title_series":
		if len(args) < 2 {
			return []string{"用法：/title ID 新标题；整个系列用 /title_series ID 新标题"}
		}
		title := strings.Join(args[1:], " ")
		if utf8.RuneCountInString(title) > 200 {
			return []string{"标题最多 200 字。"}
		}
		series := command == "/title_series"
		etag, err := b.Editor.EditEventTitle(ctx, ref, title, series)
		if err != nil {
			return editError(err)
		}
		ref.ETag = etag
		if err = b.Store.SaveEventReferences(ctx, b.scope, []store.EventReference{ref}, b.Now()); err != nil {
			return []string{"标题已写入，但事件 ID 更新失败，请刷新日程列表。"}
		}
		if series && ref.Instance != "" {
			scope = "整个系列，含单次例外"
		}
		message := "标题已更新（" + scope + "）。"
		if ref.Instance != "" {
			message += "其他实例请刷新。"
		}
		return []string{message}
	case "/remind", "/remind_series":
		if len(args) != 3 {
			return []string{"用法：/remind ID smtp|telegram|all 15m|1h|off|default；整个系列用 /remind_series"}
		}
		channel, mode := strings.ToLower(args[1]), strings.ToLower(args[2])
		if channel != "smtp" && channel != "telegram" && channel != "all" {
			return []string{"渠道只能是 smtp、telegram 或 all。"}
		}
		var before int64
		if mode != "off" && mode != "default" {
			duration, err := time.ParseDuration(mode)
			if err != nil || duration < 0 || duration > 366*24*time.Hour || duration%time.Second != 0 {
				return []string{"提前量用 0s、15m、1h 等（精确到秒，最长 366 天）；off 关闭，default 继承。"}
			}
			before = int64(duration / time.Second)
			mode = "before"
		}
		series := command == "/remind_series"
		details, err := b.Editor.SetEventReminder(ctx, ref, channel, mode, before, series)
		if err != nil {
			return editError(err)
		}
		if series && ref.Instance != "" {
			scope = "整个系列，单次覆盖仍优先"
		}
		message := "提醒已更新（" + scope + "）。"
		if mode == "default" {
			message = "已恢复继承（" + scope + "）。"
		}
		return pages(append([]string{message}, ruleLines(details)...))
	}
	return nil
}
