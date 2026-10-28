package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"radicale-extend/internal/config"
	"radicale-extend/internal/mcpapi"
	"radicale-extend/internal/service"
	"radicale-extend/internal/store"
)

type capture struct{ jobs []store.Job }

func (c *capture) Send(_ context.Context, j store.Job) error { c.jobs = append(c.jobs, j); return nil }
func TestNativeRadicaleMCPAndNotifications(t *testing.T) {
	python := os.Getenv("RADICALE_PYTHON")
	if python == "" {
		t.Skip("set RADICALE_PYTHON to a Python with Radicale 3.9.0 installed; no Docker is used")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, python, "-c", "import importlib.metadata; print(importlib.metadata.version('Radicale'))").Output()
	if err != nil || strings.TrimSpace(string(version)) != "3.9.0" {
		t.Fatal("Radicale 3.9.0 required", err)
	}
	root, _ := filepath.Abs("..")
	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/", port)
	cfgPath := filepath.Join(dir, "app.yaml")
	yaml := fmt.Sprintf("dav:\n  url: %s\n  username: alice\n  password:\n    value: test-only\nstorage:\n  state: %s\n  queue: %s\ntoken:\n  value: test-token-with-more-than-thirty-two-characters\nsmtp:\n  enabled: true\n  host: 127.0.0.1\n  port: 2525\n  security: none\n  from: test@example.test\n  to: test@example.test\ntelegram:\n  enabled: true\n  token:\n    value: test-only\n  chat_id: '12345'\n", endpoint, filepath.ToSlash(filepath.Join(dir, "state.db")), filepath.ToSlash(filepath.Join(dir, "queue.db")))
	if err = os.WriteFile(cfgPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	s, err := service.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Store.Close() }()
	now := time.Now().UTC().Truncate(time.Second)
	s.Now = func() time.Time { return now }
	sink := &capture{}
	s.Sender = sink
	htpasswd := filepath.Join(dir, "users")
	hash, hashErr := exec.CommandContext(ctx, python, "-c", "import bcrypt; print(bcrypt.hashpw(b'test-only', bcrypt.gensalt(rounds=4)).decode())").Output()
	if hashErr != nil {
		t.Fatal("bcrypt is required for the native production-auth integration test", hashErr)
	}
	os.WriteFile(htpasswd, []byte("alice:"+strings.TrimSpace(string(hash))+"\n"), 0600)
	radicaleCfg := filepath.Join(dir, "radicale.conf")
	radicaleText := fmt.Sprintf("[server]\nhosts=127.0.0.1:%d\n[auth]\ntype=htpasswd\nhtpasswd_filename=%s\nhtpasswd_encryption=bcrypt\n[rights]\ntype=owner_only\n[storage]\nfilesystem_folder=%s\n[web]\ntype=none\n[hook]\ntype=radicale_extend_hook\n[logging]\nlevel=error\n", port, filepath.ToSlash(htpasswd), filepath.ToSlash(filepath.Join(dir, "collections")))
	os.WriteFile(radicaleCfg, []byte(radicaleText), 0600)
	cmd := exec.CommandContext(ctx, python, "-m", "radicale", "--config", radicaleCfg)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(root, "hook"), "RADICALE_EXTEND_QUEUE="+cfg.Storage.Queue)
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	httpClient := &http.Client{Timeout: 10 * time.Second}
	request := func(method, p, body string, headers map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, endpoint+strings.TrimPrefix(p, "/"), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("alice", "test-only")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		r, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for i := 0; i < 100; i++ {
		conn, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if e == nil {
			conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
		if i == 99 {
			t.Fatal("native Radicale startup timeout")
		}
	}
	if err = s.Sync(ctx, true); err != nil {
		t.Fatal("empty account bootstrap", err)
	}
	r := request("MKCALENDAR", "/alice/work/", `<c:mkcalendar xmlns:c="urn:ietf:params:xml:ns:caldav"/>`, map[string]string{"Content-Type": "application/xml"})
	r.Body.Close()
	if r.StatusCode != 201 {
		t.Fatal("MKCALENDAR", r.StatusCode)
	}
	if err = s.Sync(ctx, true); err != nil {
		t.Fatal("baseline sync", err)
	}
	server := mcpapi.New(s)
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(name string, in any, out any) {
		t.Helper()
		result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
		if e != nil || result.IsError {
			t.Fatalf("%s: %v %+v", name, e, result)
		}
		if out != nil {
			raw, e := json.Marshal(result.StructuredContent)
			if e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal(raw, out); e != nil {
				t.Fatal(e)
			}
		}
	}
	list, err := session.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 14 {
		t.Fatalf("tools registration: %d %v", len(list.Tools), err)
	}
	start := now.Add(time.Minute).Format(time.RFC3339)
	end := now.Add(time.Hour).Format(time.RFC3339)
	var created mcpapi.Result
	call("create_event", map[string]any{"calendar": "/alice/work/", "fields": map[string]any{"summary": "Native integration", "start": start, "end": end}}, &created)
	if created.ETag == "" || created.Item.UID == "" {
		t.Fatal("missing resource identity")
	}
	agenda, err := s.Agenda(ctx, now.Add(2*time.Minute), now.AddDate(0, 0, 1), 5)
	if err != nil || len(agenda) != 1 || agenda[0].Summary != "Native integration" {
		t.Fatal("live DAV agenda must include an ongoing event", agenda, err)
	}
	// Exercise task completion and undated task search through real MCP + DAV.
	var task mcpapi.Result
	call("create_task", map[string]any{"calendar": "/alice/work/", "fields": map[string]any{"summary": "Undated task"}}, &task)
	var tasks []mcpapi.Result
	call("list_tasks", map[string]any{"calendar": "/alice/work/", "from": now.Format(time.RFC3339), "to": now.AddDate(0, 0, 3).Format(time.RFC3339)}, &tasks)
	if len(tasks) != 1 {
		t.Fatal("undated task missing", tasks)
	}
	call("complete_task", map[string]any{"resource": task.Resource, "etag": task.ETag}, &task)
	if task.Item.Status != "COMPLETED" {
		t.Fatal("task completion not stored")
	}
	call("delete_task", map[string]any{"resource": task.Resource, "etag": task.ETag}, nil)
	// A single-occurrence edit/delete must preserve the rest of the series.
	var series mcpapi.Result
	seriesStart := now.Add(2 * time.Hour)
	call("create_event", map[string]any{"calendar": "/alice/work/", "fields": map[string]any{"summary": "Series", "start": seriesStart.Format(time.RFC3339), "end": seriesStart.Add(time.Hour).Format(time.RFC3339), "rrule": "FREQ=DAILY;COUNT=3"}}, &series)
	secondID := seriesStart.AddDate(0, 0, 1).Format(time.RFC3339)
	call("update_event", map[string]any{"resource": series.Resource, "etag": series.ETag, "recurrence_id": secondID, "fields": map[string]any{"summary": "Single exception"}}, &series)
	var instance mcpapi.Result
	call("get_event", map[string]any{"resource": series.Resource, "recurrence_id": secondID}, &instance)
	if instance.Item.Summary != "Single exception" {
		t.Fatal("single exception not preserved")
	}
	call("delete_event", map[string]any{"resource": series.Resource, "etag": series.ETag, "recurrence_id": seriesStart.AddDate(0, 0, 2).Format(time.RFC3339)}, nil)
	call("get_event", map[string]any{"resource": series.Resource}, &series)
	agenda, err = s.Agenda(ctx, now.Add(2*time.Minute), now.AddDate(0, 0, 4), 5)
	if err != nil || len(agenda) != 3 || agenda[0].Summary != "Native integration" || agenda[1].Summary != "Series" || agenda[2].Summary != "Single exception" {
		t.Fatal("agenda recurrence, ordering or excluded instance incorrect", agenda, err)
	}
	// Bot edits use the same selected instance, keep client properties/alarms,
	// and reject stale selections rather than overwriting another DAV client.
	selected := agenda[2]
	ref := store.EventReference{Resource: selected.Resource, Collection: selected.Calendar, UID: selected.UID, Instance: selected.Instance, ETag: selected.ETag}
	objForBot, err := s.DAV.Get(ctx, ref.Resource)
	if err != nil {
		t.Fatal(err)
	}
	preserved := strings.Replace(objForBot.Data, "END:VEVENT", "X-CLIENT:keep\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT10M\r\nEND:VALARM\r\nEND:VEVENT", 1)
	objForBot, err = s.DAV.Put(ctx, ref.Resource, preserved, objForBot.ETag, false)
	if err != nil {
		t.Fatal(err)
	}
	ref.ETag = objForBot.ETag
	cfg.SMTP.Enabled = false
	disabledDetails, ruleErr := s.SetEventReminder(ctx, ref, "smtp", "before", 1800, false)
	if ruleErr != nil || disabledDetails.Rules[0].Enabled {
		t.Fatal("disabled SMTP was enabled by Bot rule", disabledDetails, ruleErr)
	}
	cfg.SMTP.Enabled = true
	if _, err = s.SetEventReminder(ctx, ref, "telegram", "off", 0, false); err != nil {
		t.Fatal(err)
	}
	jobsForBot, err := s.Store.ListJobs(ref.Resource)
	if err != nil {
		t.Fatal(err)
	}
	countSMTP := 0
	for _, job := range jobsForBot {
		if job.Kind != "reminder" || job.State != "pending" || job.Instance != ref.Instance {
			continue
		}
		if job.Channel == "telegram" {
			t.Fatal("disabled Telegram occurrence was scheduled")
		}
		if job.Channel == "smtp" {
			countSMTP++
			at, _ := time.Parse(time.RFC3339, ref.Instance)
			if job.At != at.Add(-30*time.Minute).Unix() {
				t.Fatal(job)
			}
		}
	}
	if countSMTP != 1 {
		t.Fatal("SMTP rule did not create the selected instance reminder", countSMTP)
	}
	newETag, err := s.EditEventTitle(ctx, ref, "Bot instance title", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EditEventTitle(ctx, ref, "stale overwrite", false); err != service.ErrEventChanged {
		t.Fatal("stale Bot title selection accepted", err)
	}
	ref.ETag = newETag
	objForBot, err = s.DAV.Get(ctx, ref.Resource)
	if err != nil || !strings.Contains(objForBot.Data, "X-CLIENT:keep") || !strings.Contains(objForBot.Data, "BEGIN:VALARM") {
		t.Fatal("Bot title edit dropped client properties", err)
	}
	call("get_event", map[string]any{"resource": ref.Resource, "recurrence_id": secondID}, &instance)
	if instance.Item.Summary != "Bot instance title" {
		t.Fatal("Bot changed the wrong occurrence", instance)
	}
	call("get_event", map[string]any{"resource": ref.Resource}, &series)
	if series.Item.Summary != "Series" {
		t.Fatal("single Bot title edit changed series master")
	}
	// A whole-series title edit intentionally includes exception summaries.
	newETag, err = s.EditEventTitle(ctx, ref, "Bot entire series", true)
	if err != nil {
		t.Fatal(err)
	}
	ref.ETag = newETag
	call("get_event", map[string]any{"resource": ref.Resource, "recurrence_id": secondID}, &instance)
	call("get_event", map[string]any{"resource": ref.Resource}, &series)
	if instance.Item.Summary != "Bot entire series" || series.Item.Summary != "Bot entire series" {
		t.Fatal("whole series edit omitted exception")
	}
	if _, err = s.SetEventReminder(ctx, ref, "all", "default", 0, false); err != nil {
		t.Fatal(err)
	}
	if rules, err := s.Store.ReminderRules(ref.Collection, ref.UID); err != nil || len(rules) != 0 {
		t.Fatal("default did not clear channel overrides", rules, err)
	}
	call("delete_event", map[string]any{"resource": series.Resource, "etag": series.ETag}, nil)
	// All-day recurrence IDs use dates, not UTC timestamps.
	day := now.AddDate(0, 0, 3).Format("2006-01-02")
	dayEnd := now.AddDate(0, 0, 4).Format("2006-01-02")
	var dayEvent mcpapi.Result
	call("create_event", map[string]any{"calendar": "/alice/work/", "fields": map[string]any{"summary": "All day", "all_day": true, "start": day, "end": dayEnd, "rrule": "FREQ=DAILY;COUNT=2"}}, &dayEvent)
	dayRef := store.EventReference{Resource: dayEvent.Resource, Collection: "/alice/work/", UID: dayEvent.Item.UID, Instance: day, ETag: dayEvent.ETag}
	dayETag, err := s.EditEventTitle(ctx, dayRef, "All-day instance", false)
	if err != nil {
		t.Fatal("all-day title edit", err)
	}
	call("get_event", map[string]any{"resource": dayEvent.Resource, "recurrence_id": day}, &instance)
	if instance.Item.Summary != "All-day instance" || !instance.Item.AllDay {
		t.Fatal("all-day edit lost date semantics", instance)
	}
	call("delete_event", map[string]any{"resource": dayEvent.Resource, "etag": dayETag}, nil)
	if err = s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	var events int
	s.Store.Queue.QueryRow("SELECT count(*) FROM events").Scan(&events)
	if events == 0 {
		t.Fatal("Python Hook did not enqueue")
	}
	call("set_reminder", map[string]any{"resource": created.Resource, "mode": "before", "before_seconds": 60}, nil)
	// Exercise the real event consumer and duplicate/current-state reconciliation.
	for i := 0; i < events+1; i++ {
		if err = s.Consume(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if err = s.Dispatch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.jobs) != 2 {
		t.Fatalf("expected one reminder per channel, got %d", len(sink.jobs))
	}
	s.Store.Close()
	s, err = service.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	s.Sender = sink
	if err = s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	s.Dispatch(ctx)
	if len(sink.jobs) != 2 {
		t.Fatal("restart duplicated successful reminders")
	}
	// A stale ETag must be refused even when the operation looks harmless.
	bad, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "update_event", Arguments: map[string]any{"resource": created.Resource, "etag": "\"wrong\"", "fields": map[string]any{"summary": "wrong"}}})
	if err != nil || !bad.IsError {
		t.Fatal("stale ETag accepted", err)
	}
	// The first SDK session references the pre-restart service; close it and use DAV below.
	obj, err := s.DAV.Get(ctx, created.Resource)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(obj.Data, "SUMMARY:Native integration", "SUMMARY:Changed", 1) + ""
	saved, err := s.DAV.Put(ctx, obj.Path, changed, obj.ETag, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	moved := "/alice/work/moved.ics"
	r = request("MOVE", saved.Path, "", map[string]string{"Destination": endpoint + strings.TrimPrefix(moved, "/"), "Overwrite": "F"})
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 201 && r.StatusCode != 204 {
		t.Fatal("MOVE", r.StatusCode)
	}
	if err = s.Sync(ctx, false); err != nil {
		t.Fatal("MOVE reconciliation", err)
	}
	if _, err = s.Store.Resource(moved); err != nil {
		t.Fatal("MOVE not reconciled", err)
	}
	movedObject, err := s.DAV.Get(ctx, moved)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DAV.Delete(ctx, moved, movedObject.ETag); err != nil {
		t.Fatal(err)
	}
	if err = s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	var live int
	s.Store.DB.QueryRow("SELECT count(*) FROM jobs WHERE kind='reminder' AND state IN ('pending','retry','processing')").Scan(&live)
	if live != 0 {
		t.Fatal("deleted event retained live reminders", live)
	}
	// Whole-calendar imports can bypass per-item hooks in upstream; sync must reconcile.
	r = request("MKCALENDAR", "/alice/imported/", `<c:mkcalendar xmlns:c="urn:ietf:params:xml:ns:caldav"/>`, map[string]string{"Content-Type": "application/xml"})
	r.Body.Close()
	if r.StatusCode != 201 {
		t.Fatal("import calendar creation", r.StatusCode)
	}
	if err = s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:imported\r\nDTSTART:" + now.Add(2*time.Hour).Format("20060102T150405Z") + "\r\nSUMMARY:Imported\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	r = request("PUT", "/alice/imported/", data, map[string]string{"Content-Type": "text/calendar"})
	r.Body.Close()
	if r.StatusCode != 201 && r.StatusCode != 204 {
		t.Fatal("whole-calendar import", r.StatusCode)
	}
	if err = s.Sync(ctx, false); err != nil {
		t.Fatal("whole-calendar sync", err)
	}
	imported, err := s.Store.Resources("/alice/imported/")
	if err != nil || len(imported) != 1 {
		t.Fatal("import not reconciled", imported, err)
	}
	// A bad sync-token must recover through a full scan.
	s.Store.DB.Exec("UPDATE collections SET token='invalid-token' WHERE path='/alice/imported/'")
	if err = s.Sync(ctx, false); err != nil {
		t.Fatal("expired sync-token recovery", err)
	}
	r = request("DELETE", "/alice/imported/", "", nil)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal("collection delete", r.StatusCode)
	}
	if err = s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	s.Store.DB.QueryRow("SELECT count(*) FROM jobs WHERE collection='/alice/imported/' AND kind='reminder' AND state IN ('pending','retry','processing')").Scan(&live)
	if live != 0 {
		t.Fatal("collection delete retained reminders")
	}
}
