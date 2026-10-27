package config

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	*d = Duration(v)
	return err
}
func (d Duration) Value() time.Duration { return time.Duration(d) }

type Secret struct {
	Value string `yaml:"value"`
	File  string `yaml:"file"`
}

func (s *Secret) resolve(env string) error {
	if v, ok := os.LookupEnv(env); ok {
		s.Value = v
		s.File = ""
	}
	if v := os.Getenv(env + "_FILE"); v != "" {
		s.File = v
	}
	if s.File != "" {
		b, err := os.ReadFile(s.File)
		if err != nil {
			return fmt.Errorf("read %s secret file: %w", env, err)
		}
		s.Value = strings.TrimRight(string(b), "\r\n")
	}
	return nil
}

type Calendar struct {
	Path        string   `yaml:"path"`
	Channels    []string `yaml:"channels"`
	Changes     *bool    `yaml:"changes"`
	Reminders   *bool    `yaml:"reminders"`
	HideDetails bool     `yaml:"hide_details"`
}
type Config struct {
	Listen string `yaml:"listen"`
	Token  Secret `yaml:"token"`
	DAV    struct {
		URL      string `yaml:"url"`
		Username string `yaml:"username"`
		Password Secret `yaml:"password"`
		Home     string `yaml:"home"`
	} `yaml:"dav"`
	Storage struct {
		State string `yaml:"state"`
		Queue string `yaml:"queue"`
	} `yaml:"storage"`
	Timezone  string     `yaml:"timezone"`
	Calendars []Calendar `yaml:"calendars"`
	Reminder  struct {
		DefaultBefore Duration `yaml:"default_before"`
		AllDayAt      string   `yaml:"all_day_at"`
		HorizonDays   int      `yaml:"horizon_days"`
		MaxInstances  int      `yaml:"max_instances"`
		Grace         Duration `yaml:"grace"`
	} `yaml:"reminder"`
	SyncInterval Duration `yaml:"sync_interval"`
	Debounce     Duration `yaml:"debounce"`
	Quiet        struct {
		Start string `yaml:"start"`
		End   string `yaml:"end"`
	} `yaml:"quiet"`
	SMTP struct {
		Enabled  bool   `yaml:"enabled"`
		Host     string `yaml:"host"`
		Port     int    `yaml:"port"`
		Security string `yaml:"security"`
		Username string `yaml:"username"`
		Password Secret `yaml:"password"`
		From     string `yaml:"from"`
		To       string `yaml:"to"`
	} `yaml:"smtp"`
	Telegram struct {
		CommandsEnabled bool   `yaml:"commands_enabled"`
		Enabled         bool   `yaml:"enabled"`
		Token           Secret `yaml:"token"`
		ChatID          string `yaml:"chat_id"`
		APIURL          string `yaml:"api_url"`
	} `yaml:"telegram"`
}

func Load(path string) (*Config, error) {
	c := &Config{Listen: "127.0.0.1:8080", Timezone: "Asia/Shanghai", SyncInterval: Duration(5 * time.Minute), Debounce: Duration(30 * time.Second)}
	c.Storage.State = "data/state.db"
	c.Storage.Queue = "data/queue.db"
	c.Reminder.DefaultBefore = Duration(15 * time.Minute)
	c.Reminder.AllDayAt = "09:00"
	c.Reminder.HorizonDays = 30
	c.Reminder.MaxInstances = 10000
	c.Reminder.Grace = Duration(10 * time.Minute)
	c.SMTP.Port = 587
	c.SMTP.Security = "starttls"
	c.Telegram.APIURL = "https://api.telegram.org"
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err = dec.Decode(c); err != nil {
		return nil, errors.New("invalid YAML configuration: check field names and value types")
	}
	for _, s := range []struct {
		p   *Secret
		env string
	}{{&c.Token, "MCP_TOKEN"}, {&c.DAV.Password, "DAV_PASSWORD"}, {&c.SMTP.Password, "SMTP_PASSWORD"}, {&c.Telegram.Token, "TELEGRAM_TOKEN"}} {
		if err = s.p.resolve(s.env); err != nil {
			return nil, err
		}
	}
	for key, target := range map[string]*string{"DAV_URL": &c.DAV.URL, "DAV_USERNAME": &c.DAV.Username, "DAV_HOME": &c.DAV.Home, "TELEGRAM_CHAT_ID": &c.Telegram.ChatID, "LISTEN": &c.Listen} {
		if v := os.Getenv(key); v != "" {
			*target = v
		}
	}
	return c, c.Validate()
}
func (c *Config) Validate() error {
	u, err := url.Parse(c.DAV.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("dav.url must be an HTTP(S) URL without credentials, query or fragment")
	}
	if c.DAV.Username == "" {
		return errors.New("dav.username required")
	}
	if _, err = time.LoadLocation(c.Timezone); err != nil {
		return errors.New("invalid timezone")
	}
	if c.Storage.Queue == c.Storage.State || c.Storage.Queue == "" || c.Storage.State == "" {
		return errors.New("distinct state and queue database paths required")
	}
	if c.SyncInterval.Value() < time.Second || c.Debounce.Value() < 0 || c.Reminder.HorizonDays < 1 || c.Reminder.HorizonDays > 366 || c.Reminder.MaxInstances < 1 || c.Reminder.MaxInstances > 100000 || c.Reminder.Grace.Value() < 0 || c.Reminder.DefaultBefore.Value() < 0 {
		return errors.New("invalid scheduling limits")
	}
	if _, err = time.Parse("15:04", c.Reminder.AllDayAt); err != nil {
		return errors.New("invalid all_day_at")
	}
	if (c.Quiet.Start == "") != (c.Quiet.End == "") {
		return errors.New("both quiet start and end are required")
	}
	if c.Quiet.Start != "" {
		for _, s := range []string{c.Quiet.Start, c.Quiet.End} {
			if _, err = time.Parse("15:04", s); err != nil {
				return errors.New("invalid quiet hours")
			}
		}
		if c.Quiet.Start == c.Quiet.End {
			return errors.New("quiet hours cannot cover the entire day")
		}
	}
	if c.SMTP.Enabled {
		if c.SMTP.Host == "" || c.SMTP.From == "" || c.SMTP.To == "" || c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
			return errors.New("SMTP host, port, from and to required")
		}
		if c.SMTP.Security != "tls" && c.SMTP.Security != "starttls" && c.SMTP.Security != "none" {
			return errors.New("invalid SMTP security")
		}
		if strings.ContainsAny(c.SMTP.Host+c.SMTP.From+c.SMTP.To, "\r\n") {
			return errors.New("invalid SMTP header or host")
		}
		if _, err = mail.ParseAddress(c.SMTP.From); err != nil {
			return errors.New("invalid SMTP from address")
		}
		if _, err = mail.ParseAddress(c.SMTP.To); err != nil {
			return errors.New("invalid SMTP recipient")
		}
		if c.SMTP.Security == "none" && c.SMTP.Username != "" {
			return errors.New("SMTP authentication requires TLS or STARTTLS")
		}
	}
	if c.Telegram.Enabled {
		u, err = url.Parse(c.Telegram.APIURL)
		if c.Telegram.Token.Value == "" || strings.ContainsAny(c.Telegram.Token.Value, "/\r\n?#") || c.Telegram.ChatID == "" || err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("invalid Telegram configuration")
		}
	}
	if c.Telegram.CommandsEnabled {
		id, err := strconv.ParseInt(c.Telegram.ChatID, 10, 64)
		if !c.Telegram.Enabled || err != nil || id <= 0 {
			return errors.New("Telegram commands require enabled Telegram and a numeric private chat_id")
		}
	}
	for _, r := range c.Calendars {
		if r.Path == "" {
			return errors.New("calendar path required")
		}
		for _, ch := range r.Channels {
			if ch != "smtp" && ch != "telegram" {
				return errors.New("unknown notification channel")
			}
		}
	}
	return nil
}
func (c *Config) Rule(path string) Calendar {
	for _, r := range c.Calendars {
		if strings.Trim(r.Path, "/") == strings.Trim(path, "/") {
			return r
		}
	}
	return Calendar{Path: path}
}
func (c *Config) Channels(path string) []string {
	r := c.Rule(path)
	result := []string{}
	for _, ch := range []string{"smtp", "telegram"} {
		enabled := ch == "smtp" && c.SMTP.Enabled || ch == "telegram" && c.Telegram.Enabled
		if !enabled {
			continue
		}
		if r.Channels == nil {
			result = append(result, ch)
			continue
		}
		for _, v := range r.Channels {
			if v == ch {
				result = append(result, ch)
			}
		}
	}
	return result
}
func (c *Config) QuietUntil(now time.Time) time.Time {
	if c.Quiet.Start == "" {
		return now
	}
	loc, _ := time.LoadLocation(c.Timezone)
	n := now.In(loc)
	a, _ := time.Parse("15:04", c.Quiet.Start)
	b, _ := time.Parse("15:04", c.Quiet.End)
	start := time.Date(n.Year(), n.Month(), n.Day(), a.Hour(), a.Minute(), 0, 0, loc)
	end := time.Date(n.Year(), n.Month(), n.Day(), b.Hour(), b.Minute(), 0, 0, loc)
	if !end.After(start) {
		if n.Before(end) {
			start = start.AddDate(0, 0, -1)
		} else {
			end = end.AddDate(0, 0, 1)
		}
	}
	if !n.Before(start) && n.Before(end) {
		return end
	}
	return now
}
