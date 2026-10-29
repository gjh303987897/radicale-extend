package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf16"

	"radicale-extend/internal/store"
)

type ImageSender interface {
	SendImage(context.Context, store.Job, []byte) error
}

func (c *Channels) SendImage(ctx context.Context, j store.Job, picture []byte) error {
	if len(picture) > 4<<20 || !bytes.HasPrefix(picture, []byte("\x89PNG\r\n\x1a\n")) {
		return &Error{Permanent: true, Message: "invalid or oversized PNG"}
	}
	switch j.Channel {
	case "smtp":
		return c.sendSMTPContent(ctx, j, picture)
	case "telegram":
		return c.sendPhoto(ctx, j, picture)
	default:
		return &Error{Permanent: true, Message: "unknown image channel"}
	}
}

func (c *Channels) sendPhoto(ctx context.Context, j store.Job, picture []byte) error {
	if !c.Config.Telegram.Enabled {
		return &Error{Permanent: true, Message: "Telegram disabled"}
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	form := multipart.NewWriter(writer)
	caption := j.Title + "\n" + j.Body
	units := 0
	var clipped strings.Builder
	for _, r := range caption {
		n := utf16.RuneLen(r)
		if units+n > 1000 {
			break
		}
		clipped.WriteRune(r)
		units += n
	}
	go func() {
		err := form.WriteField("chat_id", c.Config.Telegram.ChatID)
		if err == nil {
			err = form.WriteField("caption", clipped.String())
		}
		if err == nil {
			var part io.Writer
			part, err = form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="photo"; filename="calendar.png"`}, "Content-Type": {"image/png"}})
			if err == nil {
				_, err = part.Write(picture)
			}
		}
		if err == nil {
			err = form.Close()
		}
		_ = writer.CloseWithError(err)
	}()
	url := strings.TrimRight(c.Config.Telegram.APIURL, "/") + "/bot" + c.Config.Telegram.Token.Value + "/sendPhoto"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reader)
	if err != nil {
		return &Error{Message: "Telegram image request failed"}
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Message: "Telegram image connection failed"}
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
	return &Error{Permanent: code >= 400 && code < 500 && code != 429, RetryAfter: time.Duration(max(0, result.Parameters.Retry)) * time.Second, Message: fmt.Sprintf("Telegram image delivery failed (status %d)", code)}
}

// MIME and base64 are streamed directly to SMTP; the full encoded image is
// never duplicated into a message-sized string or buffer.
func writeImageMail(out io.Writer, j store.Job, from, to string, picture []byte) error {
	root := multipart.NewWriter(out)
	title := strings.NewReplacer("\r", " ", "\n", " ").Replace(j.Title)
	_, err := fmt.Fprintf(out, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@radicale-extend>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/related; boundary=%q\r\n\r\n", from, to, mime.QEncoding.Encode("UTF-8", title), time.Now().UTC().Format(time.RFC1123Z), j.ID, root.Boundary())
	if err != nil {
		return err
	}
	var alternative bytes.Buffer
	alt := multipart.NewWriter(&alternative)
	for _, body := range []struct{ kind, text string }{{"text/plain", j.Body + "\n\n日程见附图。"}, {"text/html", "<html><body><h2>" + html.EscapeString(j.Title) + "</h2><p>" + html.EscapeString(j.Body) + "</p><img src=\"cid:week-image\" alt=\"日程图\" style=\"max-width:100%;height:auto\"></body></html>"}} {
		part, e := alt.CreatePart(textproto.MIMEHeader{"Content-Type": {body.kind + "; charset=UTF-8"}, "Content-Transfer-Encoding": {"base64"}})
		if e != nil {
			return e
		}
		if e = writeBase64(part, []byte(body.text)); e != nil {
			return e
		}
	}
	if err = alt.Close(); err != nil {
		return err
	}
	part, err := root.CreatePart(textproto.MIMEHeader{"Content-Type": {"multipart/alternative; boundary=" + alt.Boundary()}})
	if err != nil {
		return err
	}
	if _, err = part.Write(alternative.Bytes()); err != nil {
		return err
	}
	part, err = root.CreatePart(textproto.MIMEHeader{"Content-Type": {"image/png"}, "Content-Transfer-Encoding": {"base64"}, "Content-Disposition": {`inline; filename="calendar.png"`}, "Content-ID": {"<week-image>"}})
	if err != nil {
		return err
	}
	if err = writeBase64(part, picture); err != nil {
		return err
	}
	return root.Close()
}

type base64Lines struct {
	out    io.Writer
	column int
}

func (w *base64Lines) Write(p []byte) (int, error) {
	count := 0
	for len(p) > 0 {
		n := min(76-w.column, len(p))
		written, err := w.out.Write(p[:n])
		count += written
		w.column += written
		if err != nil {
			return count, err
		}
		if written != n {
			return count, io.ErrShortWrite
		}
		p = p[n:]
		if w.column == 76 {
			if _, err = io.WriteString(w.out, "\r\n"); err != nil {
				return count, err
			}
			w.column = 0
		}
	}
	return count, nil
}
func writeBase64(out io.Writer, data []byte) error {
	lines := &base64Lines{out: out}
	encoder := base64.NewEncoder(base64.StdEncoding, lines)
	if _, err := encoder.Write(data); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	if lines.column > 0 {
		_, err := io.WriteString(out, "\r\n")
		return err
	}
	return nil
}
