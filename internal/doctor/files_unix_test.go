//go:build unix

package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nhl4000/awsport/internal/config"
)

// Unix-only: a file where the cache directory should be is "not a directory"
// here, while Windows reports it as missing (treated as not signed in).
func TestTokenCheckCacheUnreadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(dir, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := tokenCheck(ssoInputs{
		cfg:         &config.Config{SSO: config.SSO{StartURL: testStartURL, Region: "us-east-1"}},
		cacheDir:    dir,
		now:         time.Now(),
		writeAdvice: func(p string) (string, string) { return p, "fix it" },
	})
	if c.Status != Fail || !strings.HasPrefix(c.Title, "can't read the SSO token cache") || c.Fix != "fix it" {
		t.Fatalf("got %+v", c)
	}
}

func TestWritableCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write read-only directories")
	}
	root := t.TempDir()
	writable := filepath.Join(root, "rw")
	readOnly := filepath.Join(root, "ro")
	file := filepath.Join(root, "file")
	for _, d := range []string{writable, readOnly} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })

	advice := func(p string) (string, string) { return p, "chown " + p }
	tests := []struct {
		name   string
		dir    string
		status Status
		title  string
		fix    string
	}{
		{name: "writable", dir: writable, status: OK, title: writable + " is writable"},
		{name: "missing under writable", dir: filepath.Join(writable, "sso", "cache"), status: OK,
			title: filepath.Join(writable, "sso", "cache") + " can be created (" + writable + " is writable)"},
		{name: "read-only", dir: readOnly, status: Fail, title: "can't write " + readOnly, fix: "chown " + readOnly},
		{name: "missing under read-only", dir: filepath.Join(readOnly, "cache"), status: Fail,
			title: "can't write " + readOnly, fix: "chown " + readOnly},
		{name: "not a directory", dir: file, status: Fail, title: "can't write " + file, fix: "chown " + file},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := writableCheck(tt.dir, advice)
			if c.Status != tt.status || c.Title != tt.title || c.Fix != tt.fix {
				t.Errorf("got %+v, want %s %q fix %q", c, tt.status, tt.title, tt.fix)
			}
		})
	}
	entries, err := os.ReadDir(writable)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".awsport-doctor-") {
			t.Errorf("probe file %s left behind", e.Name())
		}
	}
}

func TestConfigFileCheck(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	if c := configFileCheck(cfg, false); c.Status != Warn || !strings.HasPrefix(c.Fix, "awsport login") {
		t.Errorf("missing = %+v", c)
	}
	if err := os.WriteFile(cfg, []byte("[default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := configFileCheck(cfg, true); c.Status != OK || c.Detail != "set by AWS_CONFIG_FILE" {
		t.Errorf("readable = %+v", c)
	}
	if os.Geteuid() == 0 {
		return
	}
	if err := os.Chmod(cfg, 0o200); err != nil {
		t.Fatal(err)
	}
	if c := configFileCheck(cfg, false); c.Status != Fail || c.Fix == "" {
		t.Errorf("unreadable = %+v", c)
	}
}
