package mcpapi

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"radicale-extend/internal/config"
	"radicale-extend/internal/service"
	"strings"
	"testing"
)

func TestHTTPAuthenticationAndSDKTools(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	text := "dav:\n  url: http://127.0.0.1:5232/\n  username: alice\ntoken:\n  value: test-token-long-enough-for-an-http-server\nstorage:\n  state: " + filepath.ToSlash(filepath.Join(dir, "state")) + "\n  queue: " + filepath.ToSlash(filepath.Join(dir, "queue")) + "\n"
	os.WriteFile(path, []byte(text), 0600)
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := service.New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Store.Close()
	httpServer := httptest.NewServer(HTTP(s, New(s)))
	defer httpServer.Close()
	for _, p := range []struct {
		path   string
		status int
	}{{"/mcp", 401}, {"/healthz", 200}, {"/readyz", 503}} {
		r, e := http.Get(httpServer.URL + p.path)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != p.status {
			t.Fatal(p, r.StatusCode)
		}
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req, _ := http.NewRequest("POST", httpServer.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.Token.Value)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(data), "create_event") {
		t.Fatal(r.StatusCode, string(data))
	}
	// Also check normal SDK initialization over stdio-equivalent in-memory transport.
	a, b := mcp.NewInMemoryTransports()
	ss, err := New(s).Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 14 {
		t.Fatal(tools, err)
	}
}
