package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
	"unicode/utf16"

	"radicale-extend/internal/config"
	"radicale-extend/internal/store"
)

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func TestPhotoMultipartRateLimitAndRedaction(t *testing.T) {
	picture := tinyPNG(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			t.Error("wrong photo endpoint")
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		fields := map[string][]byte{}
		for {
			part, e := reader.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Error(e)
				return
			}
			data, e := io.ReadAll(part)
			if e != nil {
				t.Error(e)
			}
			fields[part.FormName()] = data
		}
		if string(fields["chat_id"]) != "123" || !bytes.Equal(fields["photo"], picture) || len(utf16.Encode([]rune(string(fields["caption"])))) > 1024 {
			t.Error("invalid image multipart fields")
		}
		if calls == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":45},"description":"private response"}`)
		} else {
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer server.Close()
	c := &config.Config{}
	c.Telegram.Enabled = true
	c.Telegram.Token.Value = "test-secret"
	c.Telegram.ChatID = "123"
	c.Telegram.APIURL = server.URL
	channels := New(c)
	job := store.Job{Channel: "telegram", Title: "Weekly", Body: strings.Repeat("😀", 1500)}
	err := channels.SendImage(context.Background(), job, picture)
	var failure *Error
	if !errors.As(err, &failure) || failure.RetryAfter.Seconds() != 45 || failure.Permanent || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
		t.Fatal(err)
	}
	if err = channels.SendImage(context.Background(), job, picture); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err = channels.SendImage(context.Background(), job, picture); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}
func TestImageMailMIMEAndCID(t *testing.T) {
	picture := tinyPNG(t)
	var out bytes.Buffer
	if err := writeImageMail(&out, store.Job{ID: "week-id", Title: "周日程", Body: "未来七天"}, "from@example.test", "to@example.test", picture); err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	kind, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/related" {
		t.Fatal(kind, err)
	}
	root := multipart.NewReader(message.Body, params["boundary"])
	alternative, err := root.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	kind, params, err = mime.ParseMediaType(alternative.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/alternative" {
		t.Fatal(kind, err)
	}
	parts := multipart.NewReader(alternative, params["boundary"])
	htmlFound := false
	for {
		part, e := parts.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		decoded, e := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
		if e != nil {
			t.Fatal(e)
		}
		if strings.HasPrefix(part.Header.Get("Content-Type"), "text/html") {
			htmlFound = bytes.Contains(decoded, []byte("cid:week-image"))
		}
	}
	attachment, err := root.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, attachment))
	if err != nil || !htmlFound || !bytes.Equal(decoded, picture) || attachment.Header.Get("Content-ID") != "<week-image>" {
		t.Fatal("image attachment/CID invalid", err)
	}
}
