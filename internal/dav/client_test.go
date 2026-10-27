package dav

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"radicale-extend/internal/config"
	"testing"
)

func testClient(t *testing.T, url string) *Client {
	t.Helper()
	c := &config.Config{}
	c.DAV.URL = url + "/"
	c.DAV.Username = "alice"
	c.DAV.Password.Value = "test-secret"
	client, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	client.allowed["/alice/work/"] = true
	return client
}
func TestPathAndRedirectGuards(t *testing.T) {
	received := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received++ }))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, foreign.URL+"/steal", 302) }))
	defer server.Close()
	c := testClient(t, server.URL)
	for _, p := range []string{foreign.URL + "/alice/work/a.ics", "/bob/work/a.ics", "/alice/work/%2e%2e/a.ics", "/alice/work/a.ics?token=x", "/alice/work/%5c..%5c/a.ics"} {
		if _, _, err := c.Resource(p); err == nil {
			t.Errorf("accepted unsafe path %s", p)
		}
	}
	_, err := c.Get(context.Background(), "/alice/work/a.ics")
	if !IsStatus(err, 302) || received != 0 {
		t.Fatal("redirect followed", err, received)
	}
}
func TestConditionalWrites(t *testing.T) {
	seen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "alice" || pass != "test-secret" {
			t.Error("missing auth")
		}
		if r.Method == "PUT" {
			if r.Header.Get("If-Match") != "\"etag\"" {
				t.Error("missing If-Match")
			}
			seen = true
			w.WriteHeader(204)
			return
		}
		w.Header().Set("ETag", "\"new\"")
		io.WriteString(w, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n")
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	if _, err := c.Put(context.Background(), "/alice/work/a.ics", "calendar", "", false); err == nil {
		t.Fatal("missing ETag accepted")
	}
	if _, err := c.Put(context.Background(), "/alice/work/a.ics", "calendar", "\"etag\"", false); err != nil || !seen {
		t.Fatal(err)
	}
}
