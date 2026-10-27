package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"radicale-extend/internal/config"
	"radicale-extend/internal/store"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestTelegramLimitRetryAndRedaction(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Error("bad Telegram endpoint")
		}
		var body struct {
			Text   string `json:"text"`
			ChatID string `json:"chat_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(utf16.Encode([]rune(body.Text))) > 4096 {
			t.Error("Telegram UTF16 limit exceeded")
		}
		if body.ChatID != "123" {
			t.Error("recipient mismatch")
		}
		if calls == 1 {
			w.WriteHeader(429)
			io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":17},"description":"secret-do-not-log"}`)
		} else {
			io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer server.Close()
	c := &config.Config{}
	c.Telegram.Enabled = true
	c.Telegram.Token.Value = "secret-token"
	c.Telegram.ChatID = "123"
	c.Telegram.APIURL = server.URL
	s := New(c)
	job := store.Job{Channel: "telegram", Title: "Title", Body: strings.Repeat("😀", 5000)}
	err := s.Send(context.Background(), job)
	var e *Error
	if !errors.As(err, &e) || e.RetryAfter != 17*time.Second || e.Permanent {
		t.Fatal(err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("credential or response body leaked")
	}
	if err = s.Send(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}
func TestSMTPWireMessage(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	message := make(chan string, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprint(conn, "220 test\r\n")
		reader := bufio.NewReader(conn)
		for {
			line, e := reader.ReadString('\n')
			if e != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprint(conn, "250 test\r\n")
			case strings.HasPrefix(line, "DATA"):
				fmt.Fprint(conn, "354 go\r\n")
				var body strings.Builder
				for {
					line, e = reader.ReadString('\n')
					if e != nil {
						return
					}
					if line == ".\r\n" {
						break
					}
					body.WriteString(line)
				}
				message <- body.String()
				fmt.Fprint(conn, "250 accepted\r\n")
			case strings.HasPrefix(line, "QUIT"):
				fmt.Fprint(conn, "221 bye\r\n")
				return
			default:
				fmt.Fprint(conn, "250 OK\r\n")
			}
		}
	}()
	c := &config.Config{}
	c.SMTP.Enabled = true
	c.SMTP.Host = "127.0.0.1"
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	c.SMTP.Port, _ = strconv.Atoi(port)
	c.SMTP.Security = "none"
	c.SMTP.From = "calendar@example.test"
	c.SMTP.To = "owner@example.test"
	if err = New(c).Send(context.Background(), store.Job{ID: "stable-id", Channel: "smtp", Title: "会议提醒", Body: "日历提醒"}); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-message:
		if !strings.Contains(body, "Message-ID: <stable-id@radicale-extend>") || !strings.Contains(body, "Content-Transfer-Encoding: base64") {
			t.Fatal("invalid MIME message")
		}
	case <-time.After(time.Second):
		t.Fatal("no message")
	}
}
