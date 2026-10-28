package dav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
	"radicale-extend/internal/config"
)

type Object struct {
	Path string `json:"path"`
	ETag string `json:"etag"`
	Data string `json:"data,omitempty"`
}
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("DAV returned HTTP %d", e.Code) }
func IsStatus(err error, code int) bool {
	var e *StatusError
	return errors.As(err, &e) && e.Code == code
}

type Client struct {
	base               *url.URL
	home               string
	username, password string
	http               *http.Client
	cal                *caldav.Client
	mu                 sync.RWMutex
	allowed            map[string]bool
	selected           map[string]bool
}

type reportLimitKey struct{}
type boundedBody struct {
	io.Reader
	io.Closer
}

func New(c *config.Config) (*Client, error) {
	u, err := url.Parse(c.DAV.URL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	home := c.DAV.Home
	if home == "" {
		home = u.Path + url.PathEscape(c.DAV.Username) + "/"
	}
	cl := &Client{base: u, username: c.DAV.Username, password: c.DAV.Password.Value, allowed: map[string]bool{}, selected: map[string]bool{}, http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	h, err := cl.safePath(home)
	if err != nil {
		return nil, err
	}
	cl.home = strings.TrimRight(h, "/") + "/"
	for _, r := range c.Calendars {
		p, err := cl.safePath(r.Path)
		if err != nil || !strings.HasPrefix(p, cl.home) {
			return nil, errors.New("calendar outside configured DAV home")
		}
		cl.selected[strings.TrimRight(p, "/")+"/"] = true
	}
	cl.cal, err = caldav.NewClient(cl, u.String())
	return cl, err
}
func (c *Client) safePath(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid DAV path")
	}
	u = c.base.ResolveReference(u)
	if u.Scheme != c.base.Scheme || u.Host != c.base.Host {
		return "", errors.New("DAV origin mismatch")
	}
	p := u.Path
	if strings.ContainsAny(p, "\\\x00\r\n") {
		return "", errors.New("invalid DAV path")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." {
			return "", errors.New("DAV traversal rejected")
		}
	}
	if !strings.HasPrefix(p, strings.TrimRight(c.base.Path, "/")+"/") {
		return "", errors.New("DAV path outside base")
	}
	return p, nil
}
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	p, err := c.safePath(req.URL.String())
	if err != nil {
		return nil, err
	}
	if c.home != "" && !strings.HasPrefix(p, c.home) {
		return nil, errors.New("DAV path outside home")
	}
	req = req.Clone(req.Context())
	req.SetBasicAuth(c.username, c.password)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("DAV connection failed")
	}
	if limit, ok := req.Context().Value(reportLimitKey{}).(int64); ok && limit > 0 {
		resp.Body = &boundedBody{Reader: io.LimitReader(resp.Body, limit), Closer: resp.Body}
	}
	return resp, nil
}
func (c *Client) Calendars(ctx context.Context) ([]caldav.Calendar, error) {
	list, err := c.cal.FindCalendars(ctx, c.home)
	if err != nil {
		return nil, errors.New("DAV calendar discovery failed")
	}
	out := []caldav.Calendar{}
	allowed := map[string]bool{}
	for _, v := range list {
		p, err := c.safePath(v.Path)
		if err != nil || !strings.HasPrefix(p, c.home) {
			return nil, errors.New("invalid calendar discovery result")
		}
		p = strings.TrimRight(p, "/") + "/"
		if len(c.selected) > 0 && !c.selected[p] {
			continue
		}
		v.Path = p
		allowed[p] = true
		out = append(out, v)
	}
	c.mu.Lock()
	c.allowed = allowed
	c.mu.Unlock()
	return out, nil
}
func (c *Client) Collection(raw string) (string, error) {
	p, err := c.safePath(raw)
	if err != nil {
		return "", err
	}
	p = strings.TrimRight(p, "/") + "/"
	c.mu.RLock()
	ok := c.allowed[p]
	c.mu.RUnlock()
	if !ok {
		return "", errors.New("calendar is not in authorized collection set")
	}
	return p, nil
}
func (c *Client) Resource(raw string) (string, string, error) {
	p, err := c.safePath(raw)
	if err != nil {
		return "", "", err
	}
	col := path.Dir(p) + "/"
	if _, err = c.Collection(col); err != nil {
		return "", "", err
	}
	if path.Base(p) == "." || strings.HasSuffix(p, "/") {
		return "", "", errors.New("resource path required")
	}
	return p, col, nil
}
func (c *Client) Request(ctx context.Context, method, p, body, etag string, create bool) (*http.Response, error) {
	u := *c.base
	u.Path = p
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	}
	if create {
		req.Header.Set("If-None-Match", "*")
	} else if method == "PUT" || method == "DELETE" {
		if etag == "" || etag == "*" {
			return nil, errors.New("specific ETag required")
		}
		req.Header.Set("If-Match", etag)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &StatusError{resp.StatusCode}
	}
	return resp, nil
}
func (c *Client) Get(ctx context.Context, raw string) (Object, error) {
	p, _, err := c.Resource(raw)
	if err != nil {
		return Object{}, err
	}
	r, err := c.Request(ctx, "GET", p, "", "", false)
	if err != nil {
		return Object{}, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024+1))
	if err != nil || len(b) > 4*1024*1024 {
		return Object{}, errors.New("invalid or oversized DAV object")
	}
	return Object{p, r.Header.Get("ETag"), string(b)}, nil
}
func (c *Client) Put(ctx context.Context, raw, data, etag string, create bool) (Object, error) {
	p, _, err := c.Resource(raw)
	if err != nil {
		return Object{}, err
	}
	r, err := c.Request(ctx, "PUT", p, data, etag, create)
	if err != nil {
		return Object{}, err
	}
	r.Body.Close()
	return c.Get(ctx, p)
}
func (c *Client) Delete(ctx context.Context, raw, etag string) error {
	p, _, err := c.Resource(raw)
	if err != nil {
		return err
	}
	r, err := c.Request(ctx, "DELETE", p, "", etag, false)
	if err == nil {
		r.Body.Close()
	}
	return err
}
func Encode(cal *ical.Calendar) (string, error) {
	var b bytes.Buffer
	err := ical.NewEncoder(&b).Encode(cal)
	return b.String(), err
}
func (c *Client) All(ctx context.Context, col string) ([]Object, error) {
	return c.query(ctx, col, &caldav.CalendarQuery{CompFilter: caldav.CompFilter{Name: "VCALENDAR"}})
}

// EventsWindow bounds the server response and avoids loading calendar history.
// A timezone buffer includes floating/date-only values regardless of the DAV
// server's fallback timezone. The caller applies the exact local window again.
func (c *Client) EventsWindow(ctx context.Context, col string, from, to time.Time) ([]Object, error) {
	if !to.After(from) || to.Sub(from) > 8*24*time.Hour {
		return nil, errors.New("invalid event query window")
	}
	ctx = context.WithValue(ctx, reportLimitKey{}, int64(32<<20))
	return c.query(ctx, col, &caldav.CalendarQuery{CompFilter: caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{Name: "VEVENT", Start: from.Add(-26 * time.Hour).UTC(), End: to.Add(26 * time.Hour).UTC()}}}})
}

func (c *Client) query(ctx context.Context, col string, query *caldav.CalendarQuery) ([]Object, error) {
	col, err := c.Collection(col)
	if err != nil {
		return nil, err
	}
	items, err := c.cal.QueryCalendar(ctx, col, query)
	if err != nil {
		return nil, errors.New("DAV calendar query failed")
	}
	out := []Object{}
	for _, i := range items {
		p, got, err := c.Resource(i.Path)
		if err != nil || got != col {
			return nil, errors.New("invalid query resource")
		}
		data, err := Encode(i.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, Object{p, fmt.Sprintf("%q", i.ETag), data})
	}
	return out, nil
}

type syncResponse struct {
	Token     string `xml:"sync-token"`
	Responses []struct {
		Href   string `xml:"href"`
		Status string `xml:"status"`
		Props  []struct {
			Status string `xml:"status"`
			Value  struct {
				ETag string `xml:"getetag"`
				Data string `xml:"calendar-data"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// Sync returns a complete snapshot on the first sync or after an expired token.
func (c *Client) Sync(ctx context.Context, col, token string) (items []Object, deleted []string, next string, full bool, err error) {
	col, err = c.Collection(col)
	if err != nil {
		return
	}
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(token))
	body := `<d:sync-collection xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:sync-token>` + b.String() + `</d:sync-token><d:sync-level>1</d:sync-level><d:prop><d:getetag/><c:calendar-data/></d:prop></d:sync-collection>`
	u := *c.base
	u.Path = col
	req, _ := http.NewRequestWithContext(ctx, "REPORT", u.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	resp, e := c.Do(req)
	if e != nil {
		err = e
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 {
		if token != "" && (resp.StatusCode == 403 || resp.StatusCode == 409) {
			return c.Sync(ctx, col, "")
		}
		if resp.StatusCode == 405 || resp.StatusCode == 501 {
			items, err = c.All(ctx, col)
			full = true
			return
		}
		err = &StatusError{resp.StatusCode}
		return
	}
	var result syncResponse
	if err = xml.NewDecoder(io.LimitReader(resp.Body, 32*1024*1024)).Decode(&result); err != nil {
		return
	}
	full = token == ""
	next = result.Token
	for _, r := range result.Responses {
		p, got, e := c.Resource(r.Href)
		if e != nil || got != col {
			err = errors.New("invalid sync resource")
			return
		}
		if strings.Contains(r.Status, " 404 ") {
			deleted = append(deleted, p)
			continue
		}
		found := false
		for _, prop := range r.Props {
			if strings.Contains(prop.Status, " 200 ") && prop.Value.Data != "" {
				items = append(items, Object{p, prop.Value.ETag, prop.Value.Data})
				found = true
				break
			}
		}
		if !found {
			err = errors.New("incomplete DAV sync response")
			return
		}
	}
	return
}
