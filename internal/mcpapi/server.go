package mcpapi

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"radicale-extend/internal/calendar"
	"radicale-extend/internal/dav"
	"radicale-extend/internal/service"
	"radicale-extend/internal/store"
)

type Empty struct{}
type Reference struct {
	Resource     string `json:"resource"`
	RecurrenceID string `json:"recurrence_id,omitempty"`
}
type Search struct {
	Calendar         string `json:"calendar"`
	From             string `json:"from"`
	To               string `json:"to"`
	Query            string `json:"query,omitempty"`
	Limit            int    `json:"limit,omitempty"`
	IncludeCompleted bool   `json:"include_completed,omitempty"`
	IncludeUndated   *bool  `json:"include_undated,omitempty"`
}
type Create struct {
	Calendar string         `json:"calendar"`
	Fields   calendar.Patch `json:"fields"`
}
type Update struct {
	Resource     string         `json:"resource"`
	ETag         string         `json:"etag"`
	RecurrenceID string         `json:"recurrence_id,omitempty"`
	Fields       calendar.Patch `json:"fields"`
}
type Delete struct {
	Resource     string `json:"resource"`
	ETag         string `json:"etag"`
	RecurrenceID string `json:"recurrence_id,omitempty"`
}
type Reminder struct {
	Resource      string `json:"resource"`
	Mode          string `json:"mode"`
	BeforeSeconds int64  `json:"before_seconds,omitempty"`
}
type Result struct {
	Resource string        `json:"resource"`
	ETag     string        `json:"etag"`
	Item     calendar.View `json:"item"`
}
type ReminderStatus struct {
	Jobs            []store.Job `json:"jobs"`
	Synchronized    bool        `json:"synchronized"`
	SchedulingError string      `json:"scheduling_error,omitempty"`
}

func register[I, O any](server *mcp.Server, name, desc string, fn func(context.Context, I) (O, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: desc}, func(ctx context.Context, _ *mcp.CallToolRequest, in I) (*mcp.CallToolResult, O, error) {
		out, err := fn(ctx, in)
		return nil, out, err
	})
}
func New(s *service.Service) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "radicale-extend", Version: "0.1.0"}, nil)
	discover := func(ctx context.Context) error { _, err := s.DAV.Calendars(ctx); return err }
	register(server, "list_calendars", "List authorized CalDAV calendars", func(ctx context.Context, _ Empty) (any, error) { return s.DAV.Calendars(ctx) })
	for _, spec := range []struct{ prefix, kind string }{{"event", "VEVENT"}, {"task", "VTODO"}} {
		kind := spec.kind
		prefix := spec.prefix
		listName := "search_events"
		if kind == "VTODO" {
			listName = "list_tasks"
		}
		register(server, listName, "Search within [from,to), RFC3339 dates, maximum 366 days; returns at most limit (default 100, maximum 1000) occurrences", func(ctx context.Context, in Search) ([]Result, error) {
			if err := discover(ctx); err != nil {
				return nil, err
			}
			from, err := time.Parse(time.RFC3339, in.From)
			if err != nil {
				return nil, errors.New("from must be RFC3339")
			}
			to, err := time.Parse(time.RFC3339, in.To)
			if err != nil || !to.After(from) || to.Sub(from) > 366*24*time.Hour {
				return nil, errors.New("invalid search time range")
			}
			limit := in.Limit
			if limit == 0 {
				limit = 100
			}
			if limit < 1 || limit > 1000 {
				return nil, errors.New("limit must be 1..1000")
			}
			objects, err := s.DAV.All(ctx, in.Calendar)
			if err != nil {
				return nil, err
			}
			loc, _ := time.LoadLocation(s.Config.Timezone)
			out := []Result{}
			for _, o := range objects {
				cal, e := calendar.Parse(o.Data)
				if e != nil {
					return nil, e
				}
				if calendar.Kind(cal) != kind {
					continue
				}
				if kind == "VTODO" && (in.IncludeUndated == nil || *in.IncludeUndated) {
					master, e := calendar.Master(cal, kind)
					if e != nil {
						return nil, e
					}
					v, e := calendar.Describe(master, loc)
					if e != nil {
						return nil, e
					}
					if v.Start == "" && v.Due == "" {
						if (in.IncludeCompleted || v.Status != "COMPLETED") && v.Status != "CANCELLED" && (in.Query == "" || strings.Contains(strings.ToLower(v.Summary+" "+v.Description), strings.ToLower(in.Query))) {
							out = append(out, Result{o.Path, o.ETag, v})
						}
						continue
					}
				}
				instances, e := calendar.ExpandWithStatus(cal, from, to, loc, s.Config.Reminder.MaxInstances, in.IncludeCompleted)
				if e != nil {
					return nil, e
				}
				for _, i := range instances {
					if in.Query != "" && !strings.Contains(strings.ToLower(i.View.Summary+" "+i.View.Description), strings.ToLower(in.Query)) {
						continue
					}
					out = append(out, Result{o.Path, o.ETag, i.View})
					if len(out) > s.Config.Reminder.MaxInstances {
						return nil, errors.New("search instance limit exceeded; narrow the time range")
					}
				}
			}
			sort.SliceStable(out, func(i, j int) bool {
				a, b := out[i].Item.Start, out[j].Item.Start
				if a == "" {
					a = out[i].Item.Due
				}
				if b == "" {
					b = out[j].Item.Due
				}
				return a < b
			})
			if len(out) > limit {
				out = out[:limit]
			}
			return out, nil
		})
		register(server, "get_"+prefix, "Read an event/task resource, ETag and optional recurrence instance", func(ctx context.Context, in Reference) (Result, error) {
			if err := discover(ctx); err != nil {
				return Result{}, err
			}
			o, err := s.DAV.Get(ctx, in.Resource)
			if err != nil {
				return Result{}, err
			}
			return describe(s, o, kind, in.RecurrenceID)
		})
		register(server, "create_"+prefix, "Create with If-None-Match:*. fields use RFC3339 times, or YYYY-MM-DD when all_day=true", func(ctx context.Context, in Create) (Result, error) {
			if err := discover(ctx); err != nil {
				return Result{}, err
			}
			col, err := s.DAV.Collection(in.Calendar)
			if err != nil {
				return Result{}, err
			}
			cal, err := calendar.Create(kind, in.Fields, s.Config.Timezone)
			if err != nil {
				return Result{}, err
			}
			master, _ := calendar.Master(cal, kind)
			data, err := dav.Encode(cal)
			if err != nil {
				return Result{}, err
			}
			o, err := s.DAV.Put(ctx, col+calendar.Text(master, "UID")+".ics", data, "", true)
			if err != nil {
				return Result{}, err
			}
			return describe(s, o, kind, "")
		})
		register(server, "update_"+prefix, "Update with an exact ETag. Set recurrence_id to edit one occurrence; omit to edit series. Unknown properties are preserved", func(ctx context.Context, in Update) (Result, error) {
			if err := discover(ctx); err != nil {
				return Result{}, err
			}
			return update(ctx, s, kind, in, false)
		})
		register(server, "delete_"+prefix, "Delete with exact ETag. recurrence_id excludes just one occurrence; omission deletes the entire series", func(ctx context.Context, in Delete) (map[string]bool, error) {
			if err := discover(ctx); err != nil {
				return nil, err
			}
			_, err := update(ctx, s, kind, Update{Resource: in.Resource, ETag: in.ETag, RecurrenceID: in.RecurrenceID}, true)
			return map[string]bool{"deleted": err == nil}, err
		})
	}
	register(server, "complete_task", "Complete a task or one recurrence instance, with exact ETag", func(ctx context.Context, in Delete) (Result, error) {
		if err := discover(ctx); err != nil {
			return Result{}, err
		}
		status := "COMPLETED"
		return update(ctx, s, "VTODO", Update{Resource: in.Resource, ETag: in.ETag, RecurrenceID: in.RecurrenceID, Fields: calendar.Patch{Status: &status}}, false)
	})
	register(server, "set_reminder", "Set service override for this calendar UID: calendar (VALARM/default), off, before. before_seconds is 0..31622400", func(ctx context.Context, in Reminder) (map[string]bool, error) {
		if err := discover(ctx); err != nil {
			return nil, err
		}
		err := s.SetReminder(ctx, in.Resource, calendar.Override{Mode: in.Mode, BeforeSeconds: in.BeforeSeconds})
		return map[string]bool{"saved": err == nil}, err
	})
	register(server, "list_reminders", "List persisted reminder/delivery states and scheduling errors; requires running serve process to populate", func(ctx context.Context, in Reference) (ReminderStatus, error) {
		if err := discover(ctx); err != nil {
			return ReminderStatus{}, err
		}
		p, _, err := s.DAV.Resource(in.Resource)
		if err != nil {
			return ReminderStatus{}, err
		}
		jobs, err := s.Store.ListJobs(p)
		if err != nil {
			return ReminderStatus{}, err
		}
		resource, err := s.Store.Resource(p)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ReminderStatus{}, err
		}
		return ReminderStatus{Jobs: jobs, Synchronized: err == nil, SchedulingError: resource.Error}, nil
	})
	return server
}
func describe(s *service.Service, o dav.Object, kind, rid string) (Result, error) {
	cal, err := calendar.Parse(o.Data)
	if err != nil {
		return Result{}, err
	}
	master, err := calendar.Master(cal, kind)
	if err != nil {
		return Result{}, err
	}
	loc, _ := time.LoadLocation(s.Config.Timezone)
	v, err := calendar.Describe(master, loc)
	if err != nil {
		return Result{}, err
	}
	if rid != "" {
		day := len(rid) == 10
		t, e := calendar.InputTime(rid, s.Config.Timezone, day)
		if e != nil {
			return Result{}, e
		}
		instances, e := calendar.Expand(cal, t.AddDate(-1, 0, 0), t.AddDate(1, 0, 0), loc, 100000)
		if e != nil {
			return Result{}, e
		}
		found := false
		for _, in := range instances {
			if in.Key == rid || in.Key == t.UTC().Format(time.RFC3339) {
				v = in.View
				found = true
				break
			}
		}
		if !found {
			return Result{}, errors.New("recurrence instance not found")
		}
	}
	return Result{o.Path, o.ETag, v}, nil
}
func update(ctx context.Context, s *service.Service, kind string, in Update, remove bool) (Result, error) {
	if in.ETag == "" || in.ETag == "*" {
		return Result{}, errors.New("specific ETag required")
	}
	o, err := s.DAV.Get(ctx, in.Resource)
	if err != nil {
		return Result{}, err
	}
	if o.ETag != in.ETag {
		return Result{}, &dav.StatusError{Code: 412}
	}
	cal, err := calendar.Parse(o.Data)
	if err != nil {
		return Result{}, err
	}
	if _, err = calendar.Master(cal, kind); err != nil {
		return Result{}, err
	}
	if remove && in.RecurrenceID == "" {
		return Result{}, s.DAV.Delete(ctx, in.Resource, in.ETag)
	}
	if err = calendar.Mutate(cal, kind, in.RecurrenceID, in.Fields, remove, s.Config.Timezone); err != nil {
		return Result{}, err
	}
	data, err := dav.Encode(cal)
	if err != nil {
		return Result{}, err
	}
	saved, err := s.DAV.Put(ctx, in.Resource, data, in.ETag, false)
	if err != nil {
		return Result{}, err
	}
	return describe(s, saved, kind, "")
}
func HTTP(s *service.Service, server *mcp.Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		status := s.Ready()
		w.Header().Set("Content-Type", "application/json")
		if status["ready"] != true {
			w.WriteHeader(503)
		}
		writeJSON(w, status)
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	protected := http.NewCrossOriginProtection().Handler(handler)
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(token), []byte(s.Config.Token.Value)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", 401)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4*1024*1024)
		protected.ServeHTTP(w, r)
	}))
	return mux
}
