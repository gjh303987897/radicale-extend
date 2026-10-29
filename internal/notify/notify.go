package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"radicale-extend/internal/config"
	"radicale-extend/internal/store"
)

type Error struct {
	Permanent  bool
	RetryAfter time.Duration
	Message    string
}

func (e *Error) Error() string { return e.Message }

type Sender interface {
	Send(context.Context, store.Job) error
}
type Channels struct {
	Config *config.Config
	HTTP   *http.Client
}

func New(c *config.Config) *Channels {
	return &Channels{c, &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Channels) Send(ctx context.Context, j store.Job) error {
	switch j.Channel {
	case "smtp":
		return c.sendSMTP(ctx, j)
	case "telegram":
		return c.sendTelegram(ctx, j)
	default:
		return &Error{Permanent: true, Message: "unknown channel"}
	}
}
func (c *Channels) sendTelegram(ctx context.Context, j store.Job) error {
	if !c.Config.Telegram.Enabled {
		return &Error{Permanent: true, Message: "Telegram disabled"}
	}
	text := j.Body
	if j.Title != "" {
		text = j.Title
		if j.Body != "" {
			text += "\n" + j.Body
		}
	}
	var out strings.Builder
	units := 0
	for _, r := range text {
		n := utf16.RuneLen(r)
		if units+n > 3800 {
			out.WriteString("\n…")
			break
		}
		units += n
		out.WriteRune(r)
	}
	args := map[string]any{
		"chat_id":              c.Config.Telegram.ChatID,
		"text":                 out.String(),
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if j.TelegramReplyMarkup != "" {
		var markup map[string]json.RawMessage
		if err := json.Unmarshal([]byte(j.TelegramReplyMarkup), &markup); err != nil || markup == nil {
			return &Error{Permanent: true, Message: "invalid Telegram reply markup"}
		}
		args["reply_markup"] = markup
	}
	body, err := json.Marshal(args)
	if err != nil {
		return &Error{Permanent: true, Message: "Telegram request encoding failed"}
	}
	url := strings.TrimRight(c.Config.Telegram.APIURL, "/") + "/bot" + c.Config.Telegram.Token.Value + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return &Error{Message: "Telegram request failed"}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Message: "Telegram connection failed"}
	}
	defer resp.Body.Close()
	var result struct {
		OK         bool `json:"ok"`
		Code       int  `json:"error_code"`
		Parameters struct {
			Retry int `json:"retry_after"`
		} `json:"parameters"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&result)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && err == nil && result.OK {
		return nil
	}
	code := resp.StatusCode
	if result.Code != 0 {
		code = result.Code
	}
	retry := time.Duration(result.Parameters.Retry) * time.Second
	if retry < 0 {
		retry = 0
	}
	return &Error{Permanent: code >= 400 && code < 500 && code != 429, RetryAfter: retry, Message: fmt.Sprintf("Telegram delivery failed (status %d)", code)}
}
func (c *Channels) sendSMTP(ctx context.Context, j store.Job) (err error) {
	return c.sendSMTPContent(ctx, j, nil)
}
func (c *Channels) sendSMTPContent(ctx context.Context, j store.Job, picture []byte) (err error) {
	cfg := c.Config.SMTP
	if !cfg.Enabled {
		return &Error{Permanent: true, Message: "SMTP disabled"}
	}
	from, e := mail.ParseAddress(cfg.From)
	if e != nil {
		return &Error{Permanent: true, Message: "invalid sender"}
	}
	to, e := mail.ParseAddress(cfg.To)
	if e != nil {
		return &Error{Permanent: true, Message: "invalid recipient"}
	}
	defer func() {
		if err != nil {
			var pe *textproto.Error
			permanent := errors.As(err, &pe) && pe.Code >= 500 && pe.Code < 600
			err = &Error{Permanent: permanent, Message: "SMTP delivery failed"}
		}
	}()
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, e := dialer.DialContext(ctx, "tcp", address)
	if e != nil {
		return e
	}
	defer conn.Close()
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	if cfg.Security == "tls" {
		secure := tls.Client(conn, tlsCfg)
		if err = secure.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = secure
	}
	client, e := smtp.NewClient(conn, cfg.Host)
	if e != nil {
		return e
	}
	defer client.Close()
	if cfg.Security == "starttls" {
		if err = client.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if cfg.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password.Value, cfg.Host)); err != nil {
			return err
		}
	}
	if err = client.Mail(from.Address); err != nil {
		return err
	}
	if err = client.Rcpt(to.Address); err != nil {
		return err
	}
	writer, e := client.Data()
	if e != nil {
		return e
	}
	var message bytes.Buffer
	if picture != nil {
		err = writeImageMail(writer, j, from.String(), to.String(), picture)
		closeErr := writer.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return client.Quit()
	}
	fmt.Fprintf(&message, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@radicale-extend>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", from.String(), to.String(), mime.QEncoding.Encode("UTF-8", strings.ReplaceAll(strings.ReplaceAll(j.Title, "\r", " "), "\n", " ")), time.Now().UTC().Format(time.RFC1123Z), j.ID)
	encoded := base64.StdEncoding.EncodeToString([]byte(j.Body))
	for len(encoded) > 76 {
		fmt.Fprintf(&message, "%s\r\n", encoded[:76])
		encoded = encoded[76:]
	}
	fmt.Fprintf(&message, "%s\r\n", encoded)
	_, err = writer.Write(message.Bytes())
	closeErr := writer.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return client.Quit()
}
