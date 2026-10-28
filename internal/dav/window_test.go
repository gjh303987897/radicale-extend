package dav

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWeeklyQueryUsesTimeRangeAndResponseBound(t *testing.T) {
	seen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "REPORT" {
			body, _ := io.ReadAll(r.Body)
			text := string(body)
			seen = strings.Contains(text, "time-range") && strings.Contains(text, "VEVENT")
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, `<d:multistatus xmlns:d="DAV:"/>`)
		} else {
			_, _ = io.WriteString(w, strings.Repeat("x", 100))
		}
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	start := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	items, err := c.EventsWindow(context.Background(), "/alice/work/", start, start.AddDate(0, 0, 7))
	if err != nil || !seen || len(items) != 0 {
		t.Fatal(seen, items, err)
	}
	ctx := context.WithValue(context.Background(), reportLimitKey{}, int64(64))
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/alice/work/a.ics", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || len(data) != 64 {
		t.Fatal("response limit ignored", len(data), err)
	}
}
