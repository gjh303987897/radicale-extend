package telegrambot

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"radicale-extend/internal/store"
)

type inlineButton struct {
	Text string    `json:"text"`
	Data string    `json:"callback_data,omitempty"`
	Copy *copyText `json:"copy_text,omitempty"`
}
type copyText struct {
	Text string `json:"text"`
}

func button(text, data string) inlineButton { return inlineButton{Text: text, Data: "v1:" + data} }
func navigation() [][]inlineButton {
	return [][]inlineButton{{button("今天", "today"), button("今日图", "day"), button("最近日程", "next:5")}, {button("七天图", "week"), button("帮助", "help")}}
}
func message(body string, rows [][]inlineButton) store.TelegramMessage {
	markup, _ := json.Marshal(map[string]any{"inline_keyboard": rows})
	return store.TelegramMessage{Body: body, ReplyMarkup: string(markup)}
}
func validEventID(id string) bool {
	if len(id) != 20 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func eventButtons(id string) [][]inlineButton {
	return [][]inlineButton{{button("提醒设置", "rem:"+id), {Text: "复制改标题命令", Copy: &copyText{Text: "/title " + id + " 新标题"}}}}
}

// List IDs are internal markers: keep them beside their complete text entry
// while paginating, then turn them into buttons before durable delivery.
func interactiveReplies(parts []string, list bool, eventID string, next bool) []store.TelegramMessage {
	if len(parts) > 0 && !strings.HasPrefix(parts[0], "事件及提醒规则") && !strings.HasPrefix(parts[0], "标题已更新") && !strings.HasPrefix(parts[0], "提醒已更新") && !strings.HasPrefix(parts[0], "已恢复继承") {
		eventID = ""
	}
	result := make([]store.TelegramMessage, 0, len(parts))
	for _, part := range parts {
		lines := []string{}
		buttons := []inlineButton{}
		title := "查看事件"
		for _, line := range strings.Split(part, "\n") {
			if n, _, ok := strings.Cut(line, ". "); ok {
				if _, err := strconv.Atoi(n); err == nil {
					title = clean(line, 18)
				}
			}
			if list && strings.HasPrefix(line, "ID：") && validEventID(strings.TrimPrefix(line, "ID：")) {
				buttons = append(buttons, button(title, "event:"+strings.TrimPrefix(line, "ID：")))
				continue
			}
			lines = append(lines, line)
		}
		rows := [][]inlineButton{}
		for i := 0; i < len(buttons); i += 2 {
			rows = append(rows, buttons[i:min(i+2, len(buttons))])
		}
		if validEventID(eventID) {
			rows = append(rows, eventButtons(eventID)...)
		}
		if next {
			rows = append(rows, []inlineButton{button("5 条", "next:5"), button("10 条", "next:10"), button("20 条", "next:20")})
		}
		rows = append(rows, navigation()...)
		result = append(result, message(strings.TrimSpace(strings.Join(lines, "\n")), rows))
	}
	return result
}

func (b *Bot) respond(ctx context.Context, u Update) []store.TelegramMessage {
	if u.Callback != nil {
		return b.callbackReply(ctx, u)
	}
	parts := b.reply(ctx, u)
	if len(parts) == 0 {
		return nil
	}
	fields := strings.Fields(u.Message.Text)
	command, _, _ := strings.Cut(fields[0], "@")
	list := command == "/today" || command == "/next" || command == "今日日程" || command == "今天的日程" || command == "最近5个日程" || command == "最近的5个日程"
	id := ""
	if len(fields) > 1 && (command == "/event" || command == "/title" || command == "/title_series" || command == "/remind" || command == "/remind_series") {
		id = fields[1]
	}
	return interactiveReplies(parts, list, id, command == "/next")
}

func (b *Bot) acknowledge(ctx context.Context, id, text string) {
	if id == "" || len(id) > 256 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// An expired acknowledgement must not prevent durable command processing.
	_ = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
func (b *Bot) callbackReply(ctx context.Context, u Update) []store.TelegramMessage {
	q := u.Callback
	m := q.Message
	if q.ID == "" || len(q.ID) > 256 || m == nil || m.ID <= 0 || m.Date <= 0 || m.Chat.Type != "private" || strconv.FormatInt(m.Chat.ID, 10) != b.Config.Telegram.ChatID || q.From.ID != m.Chat.ID || q.From.IsBot || b.botID <= 0 || !m.From.IsBot || m.From.ID != b.botID {
		b.acknowledge(ctx, q.ID, "此按钮不可用")
		return nil
	}
	if len(q.Data) > 64 || !strings.HasPrefix(q.Data, "v1:") {
		b.acknowledge(ctx, q.ID, "按钮已失效，请重新获取日程")
		return nil
	}
	b.acknowledge(ctx, q.ID, "")
	fields := strings.Split(strings.TrimPrefix(q.Data, "v1:"), ":")
	action := fields[0]
	command := ""
	switch action {
	case "today", "day", "week", "help":
		if len(fields) == 1 {
			command = "/" + action
		}
	case "next":
		if len(fields) == 2 && (fields[1] == "5" || fields[1] == "10" || fields[1] == "20") {
			command = "/next " + fields[1]
		}
	}
	if command != "" {
		return b.respond(ctx, Update{ID: u.ID, Message: &telegramMessage{Date: b.Now().Unix(), Text: command, Chat: m.Chat, From: q.From}})
	}
	if len(fields) < 2 || !validEventID(fields[1]) {
		return []store.TelegramMessage{message("按钮已失效，请重新获取日程。", navigation())}
	}
	id := fields[1]
	ref, err := b.Store.EventReference(ctx, b.scope, id, b.Now())
	if err != nil {
		return []store.TelegramMessage{message("事件 ID 无效或已过期，请重新获取日程。", navigation())}
	}
	scope := "此事件"
	if ref.Instance != "" {
		scope = "仅本次重复实例"
	}
	switch action {
	case "event":
		if len(fields) == 2 {
			return interactiveReplies(b.editReply(ctx, "/event", []string{id}), false, id, false)
		}
	case "rem":
		if len(fields) == 2 {
			mail := "邮件"
			if !b.Config.SMTP.Enabled {
				mail += "（未启用）"
			}
			rows := [][]inlineButton{{button("Telegram", "rule:"+id+":t"), button(mail, "rule:"+id+":s")}, {button("两个渠道", "rule:"+id+":a")}, {button("返回事件", "event:"+id)}}
			return []store.TelegramMessage{message("提醒设置 · "+scope+"\n选择渠道；未启用的渠道只保存规则，不会发送。", rows)}
		}
	case "rule":
		if len(fields) == 3 {
			channel, name := callbackChannel(fields[2])
			if channel == "" {
				break
			}
			prefix := "set:" + id + ":" + fields[2] + ":"
			rows := [][]inlineButton{{button("开始时", prefix+"0s"), button("5 分钟", prefix+"5m"), button("15 分钟", prefix+"15m")}, {button("30 分钟", prefix+"30m"), button("1 小时", prefix+"1h")}, {button("关闭提醒", prefix+"off"), button("恢复默认", prefix+"default")}, {button("返回渠道", "rem:"+id), button("查看事件", "event:"+id)}}
			return []store.TelegramMessage{message(fmt.Sprintf("%s提醒 · %s\n选择提前量，或关闭／恢复继承。", name, scope), rows)}
		}
	case "set":
		if len(fields) == 4 {
			channel, _ := callbackChannel(fields[2])
			if channel == "" {
				break
			}
			switch fields[3] {
			case "0s", "5m", "15m", "30m", "1h", "off", "default":
				return interactiveReplies(b.editReply(ctx, "/remind", []string{id, channel, fields[3]}), false, id, false)
			}
		}
	}
	return []store.TelegramMessage{message("按钮已失效，请重新获取日程。", navigation())}
}
func callbackChannel(value string) (string, string) {
	switch value {
	case "t":
		return "telegram", "Telegram"
	case "s":
		return "smtp", "邮件"
	case "a":
		return "all", "双渠道"
	}
	return "", ""
}
