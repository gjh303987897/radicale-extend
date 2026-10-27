package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuietHours(t *testing.T) {
	c := &Config{Timezone: "Asia/Shanghai"}
	c.Quiet.Start = "22:00"
	c.Quiet.End = "08:00"
	for _, spec := range []struct{ now, want string }{{"2026-10-28T23:00:00+08:00", "2026-10-29T08:00:00+08:00"}, {"2026-10-28T07:00:00+08:00", "2026-10-28T08:00:00+08:00"}, {"2026-10-28T12:00:00+08:00", "2026-10-28T12:00:00+08:00"}} {
		n, _ := time.Parse(time.RFC3339, spec.now)
		if got := c.QuietUntil(n).Format(time.RFC3339); got != spec.want {
			t.Fatal(got, spec.want)
		}
	}
}
func TestSecretsAndStrictConfig(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	os.WriteFile(secret, []byte("secret-value\n"), 0600)
	t.Setenv("DAV_PASSWORD_FILE", secret)
	file := filepath.Join(dir, "config.yaml")
	os.WriteFile(file, []byte("dav:\n  url: http://localhost:5232/\n  username: alice\n"), 0600)
	c, err := Load(file)
	if err != nil || c.DAV.Password.Value != "secret-value" {
		t.Fatal(err)
	}
	os.WriteFile(file, []byte("unexpected: true\n"), 0600)
	if _, err = Load(file); err == nil {
		t.Fatal("unknown fields accepted")
	}
}
