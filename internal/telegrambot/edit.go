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
		return []string{"事件已被修改，请重新发送 /today 或 /next 获取事件 ID 后重试，避免覆盖其他客户端的更新。"}
	}
	if dav.IsStatus(err, 404) {
		return []string{"事件已删除或移动，请刷新日程列表。"}
	}
	return []string{"操作未能确认完成，请刷新日程列表核对；可能是日历服务暂不可用或日程数据不受支持。"}
}

func ruleLines(details service.EventDetails) []string {
	label := clean(details.Summary, 200)
	if details.Hidden {
		label = "（日程详情已隐藏）"
	}
	lines := []string{"事件：" + label}
	for _, r := range details.Rules {
		value := "日历 VALARM / 默认提醒"
		switch r.Mode {
		case "off":
			value = "关闭"
		case "before":
			value = "提前 " + (time.Duration(r.BeforeSeconds) * time.Second).String()
		case "default":
			if r.AllDayAt != "" {
				value = "默认：当天 " + r.AllDayAt
			} else {
				value = "默认提前 " + (time.Duration(r.BeforeSeconds) * time.Second).String()
			}
		case "valarm":
			value = "使用日历自带 VALARM"
		}
		status := ""
		if !r.Enabled {
			status = "；渠道未启用或已被日历规则禁用，不会发送"
		}
		lines = append(lines, fmt.Sprintf("%s：%s（%s%s）", r.Channel, value, r.Source, status))
		if r.SchedulingError != "" {
			lines = append(lines, "提醒解析状态："+r.SchedulingError)
		} else if len(r.TriggerTimes) > 0 {
			lines = append(lines, "规则触发时间："+strings.Join(r.TriggerTimes, "；"))
			if r.TriggerCount > len(r.TriggerTimes) {
				lines = append(lines, fmt.Sprintf("共 %d 次触发，显示前 %d 次。", r.TriggerCount, len(r.TriggerTimes)))
			}
		}
	}
	lines = append(lines, "触发时间按配置时区显示；实际发送还受渠道开关、免打扰、提醒宽限期和任务状态影响。")
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
		return []string{"事件 ID 无效或已过期。请先发送 /today 或 /next，再使用列表中的 ID。"}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	scope := "此事件"
	if ref.Instance != "" {
		scope = "仅本次重复实例"
	}
	switch command {
	case "/event", "/reminders":
		if len(args) != 1 {
			return []string{"用法：" + command + " ID"}
		}
		details, err := b.Editor.EventRules(ctx, ref)
		if err != nil {
			return editError(err)
		}
		return pages(append([]string{"当前提醒规则", "ID：" + ref.ID, "查询范围：" + scope}, ruleLines(details)...))
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
			return []string{"标题已写入日历，但事件 ID 更新失败；请重新获取日程列表。"}
		}
		if series && ref.Instance != "" {
			scope = "整个重复系列（含单次例外标题）"
		}
		return []string{"标题已更新（" + scope + "）。可继续用此 ID 设置提醒；其他实例 ID 请刷新列表。"}
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
				return []string{"提前量需为 0s、15m、1h、24h 等，精确到秒且不超过 366 天；off 关闭，default 恢复继承。"}
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
			scope = "整个重复系列；已有本次覆盖仍优先"
		}
		return pages(append([]string{"提醒规则已保存（" + scope + "）。", "default 仅移除当前范围的覆盖，恢复继承。"}, ruleLines(details)...))
	}
	return nil
}
