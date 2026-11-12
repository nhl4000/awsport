package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nhl4000/awsport/internal/agent"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/tunnel"
)

const testStartURL = "https://d-0000000000.awsapps.com/start"

func writeToken(t *testing.T, dir, name, startURL string, exp time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"startUrl":%q,"accessToken":"tok","expiresAt":%q}`, startURL, exp.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTokenCheck(t *testing.T) {
	now := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		setup     func(t *testing.T, dir string)
		status    Status
		title     string
		fixPrefix string
	}{
		{name: "no cache dir", setup: func(*testing.T, string) {}, status: Fail, title: "not signed in to SSO", fixPrefix: "awsport login"},
		{name: "other portal only", setup: func(t *testing.T, dir string) {
			writeToken(t, dir, "a.json", "https://example.com/start", now.Add(8*time.Hour))
		}, status: Fail, title: "not signed in to SSO", fixPrefix: "awsport login"},
		{name: "expired", setup: func(t *testing.T, dir string) {
			writeToken(t, dir, "a.json", testStartURL, now.Add(-2*time.Hour))
		}, status: Fail, title: "SSO sign-in expired 2h00m ago (at 08:00 UTC)", fixPrefix: "awsport login"},
		{name: "expiring within the hour", setup: func(t *testing.T, dir string) {
			writeToken(t, dir, "a.json", testStartURL, now.Add(42*time.Minute))
		}, status: Warn, title: "SSO sign-in expires in 42m (at 10:42 UTC)", fixPrefix: "awsport login"},
		{name: "expiring within a minute", setup: func(t *testing.T, dir string) {
			writeToken(t, dir, "a.json", testStartURL, now.Add(20*time.Second))
		}, status: Warn, title: "SSO sign-in expires in <1m", fixPrefix: "awsport login"},
		{name: "valid, longest wins", setup: func(t *testing.T, dir string) {
			writeToken(t, dir, "a.json", testStartURL, now.Add(10*time.Minute))
			writeToken(t, dir, "b.json", testStartURL+"/", now.Add(26*time.Hour))
		}, status: OK, title: "signed in to SSO; expires in 26h00m (Mar 5 12:00 UTC)"},
		{name: "cache unreadable", setup: func(t *testing.T, dir string) {
			if err := os.WriteFile(dir, []byte("not a dir"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, status: Fail, title: "can't read the SSO token cache", fixPrefix: "fix it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			tt.setup(t, dir)
			c := tokenCheck(ssoInputs{
				cfg:         &config.Config{SSO: config.SSO{StartURL: testStartURL, Region: "us-east-1"}},
				cacheDir:    dir,
				now:         now,
				writeAdvice: func(p string) (string, string) { return p, "fix it" },
			})
			if c.Status != tt.status || !strings.HasPrefix(c.Title, tt.title) {
				t.Errorf("got %s %q, want %s %q…", c.Status, c.Title, tt.status, tt.title)
			}
			if !strings.HasPrefix(c.Fix, tt.fixPrefix) || (tt.status != OK && c.Fix == "") {
				t.Errorf("fix = %q, want prefix %q", c.Fix, tt.fixPrefix)
			}
		})
	}
}

func TestSSOChecksNotConfigured(t *testing.T) {
	tests := []struct {
		name       string
		cfg        config.Config
		awsSession string
		fix        string
	}{
		{name: "nothing to go on", fix: "awsport init --from " + teamConfigExample},
		{name: "existing sso-session", awsSession: "corp", fix: "awsport init (adopts [sso-session corp]"},
		{name: "team config without sso", cfg: config.Config{TeamSource: "github:acme/infra/awsport.yaml"},
			fix: "ask the maintainer of the team config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ssoChecks(ssoInputs{cfg: &tt.cfg, awsSession: tt.awsSession})
			if len(got) != 1 || got[0].Status != Fail || !strings.HasPrefix(got[0].Fix, tt.fix) {
				t.Errorf("got %+v, want one failure with fix %q…", got, tt.fix)
			}
		})
	}
}

func TestSSOChecksNotConfiguredWithOtherProfilesIsAWarning(t *testing.T) {
	got := ssoChecks(ssoInputs{cfg: &config.Config{}, nonSSO: 3})
	if len(got) != 1 || got[0].Status != Warn || got[0].Fix == "" {
		t.Fatalf("got %+v, want one warning with a fix", got)
	}
}

func TestSSOChecksConfigured(t *testing.T) {
	cfg := &config.Config{SSO: config.SSO{StartURL: testStartURL, Region: "us-east-1"}, TeamSource: "github:acme/infra/awsport.yaml"}
	got := ssoChecks(ssoInputs{cfg: cfg, cacheDir: filepath.Join(t.TempDir(), "none"), now: time.Now()})
	if len(got) != 3 {
		t.Fatalf("got %d checks, want portal, team source, token: %+v", len(got), got)
	}
	if got[0].Status != OK || !strings.Contains(got[0].Title, `sso-session "awsport"`) {
		t.Errorf("portal check = %+v", got[0])
	}
	if got[1].Title != "team config from github:acme/infra/awsport.yaml" {
		t.Errorf("team check = %+v", got[1])
	}
	if got[2].Status != Fail {
		t.Errorf("token check = %+v, want failure", got[2])
	}
}

func TestPermissionCheck(t *testing.T) {
	const principal = "arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Developer_0123456789abcdef/jane"
	rds := &awsx.PermissionError{Profile: "example-dev", Region: "us-east-1", Action: "rds:DescribeDBInstances",
		What: "list RDS databases", Principal: principal}
	ssm := &awsx.PermissionError{Profile: "example-dev", Region: "us-east-1", Action: "ssm:DescribeInstanceInformation",
		What: "read SSM agent status", Principal: principal}

	one := permissionCheck([]*awsx.PermissionError{rds})
	if one.Status != Warn || one.Title != "Can't list RDS databases in us-east-1 — missing rds:DescribeDBInstances" || one.Detail != "" {
		t.Errorf("single = %+v", one)
	}
	if !strings.Contains(one.Fix, "rds:DescribeDBInstances") || !strings.Contains(one.Fix, `"Developer" permission set`) {
		t.Errorf("single fix = %q", one.Fix)
	}

	two := permissionCheck([]*awsx.PermissionError{rds, ssm})
	if two.Title != "2 IAM permissions missing" || len(lines(two.Detail)) != 2 {
		t.Errorf("multiple = %+v", two)
	}
	if lines(two.Fix)[0] != awsx.Hints([]*awsx.PermissionError{rds, ssm})[0] || len(lines(two.Fix)) != 1 {
		t.Errorf("multiple fix = %q, want one combined hint", two.Fix)
	}
}

func TestDiscoveryChecks(t *testing.T) {
	target := profileTarget{profile: core.Profile{Name: "example-dev", Kind: core.AuthSSO, Managed: true}, region: "us-east-1"}
	denied := &awsx.PermissionError{Profile: "example-dev", Region: "us-east-1", Action: "rds:DescribeDBClusters", What: "list Aurora clusters"}
	resources := []core.Resource{
		{Kind: core.KindEC2, Name: "app"},
		{Kind: core.KindEC2, Name: "old", Unreachable: "instance stopped"},
		{Kind: core.KindRDS, Name: "db"},
	}
	tests := []struct {
		name     string
		err      error
		statuses []Status
		last     string
	}{
		{name: "success", statuses: []Status{OK}, last: "permissions ok: 2 EC2 instances (1 reachable via SSM), 1 database in us-east-1"},
		{name: "denied only", err: fmt.Errorf("discover: %w", errors.Join(denied)), statuses: []Status{Warn}, last: "Can't list Aurora clusters"},
		{name: "denied and expired", err: errors.Join(denied, fmt.Errorf("%w: boom", awsx.ErrCredentials)),
			statuses: []Status{Warn, Fail}, last: "credentials expired or invalid"},
		{name: "network", err: errors.New("dial tcp: no route to host"), statuses: []Status{Fail}, last: "discovery in us-east-1 failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := discoveryChecks(context.Background(), target, resources, tt.err)
			if len(got) != len(tt.statuses) {
				t.Fatalf("got %+v, want statuses %v", got, tt.statuses)
			}
			for i, s := range tt.statuses {
				if got[i].Status != s {
					t.Errorf("check %d status = %s, want %s", i, got[i].Status, s)
				}
				if s != OK && got[i].Fix == "" {
					t.Errorf("check %d has no fix: %+v", i, got[i])
				}
			}
			if last := got[len(got)-1].Title; !strings.HasPrefix(last, tt.last) {
				t.Errorf("last title = %q, want prefix %q", last, tt.last)
			}
		})
	}
}

func TestCredentialCheckFix(t *testing.T) {
	expired := fmt.Errorf("%w: token expired", awsx.ErrCredentials)
	tests := []struct {
		name    string
		profile core.Profile
		err     error
		fix     string
	}{
		{"managed sso", core.Profile{Name: "example-dev", Kind: core.AuthSSO, Managed: true}, expired, "awsport login"},
		{"own sso session", core.Profile{Name: "x", Kind: core.AuthSSO, SSOSession: "awsport"}, expired, "awsport login"},
		{"foreign sso", core.Profile{Name: "legacy", Kind: core.AuthSSO, SSOSession: "other"}, expired, "awsport login legacy"},
		{"assume role", core.Profile{Name: "ops", Kind: core.AuthAssumeRole}, expired, "renew the credentials of the profile's source_profile"},
		{"static", core.Profile{Name: "ci", Kind: core.AuthStatic}, expired, "the access key is invalid"},
		{"network", core.Profile{Name: "ci", Kind: core.AuthStatic}, errors.New("dial tcp: timeout"), "check network access"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := credentialCheck(context.Background(), profileTarget{profile: tt.profile, ssoSession: "awsport"}, tt.err)
			if c.Status != Fail || !strings.HasPrefix(c.Fix, tt.fix) {
				t.Errorf("got %+v, want fix %q…", c, tt.fix)
			}
			if tt.fix == "awsport login" && c.Fix != tt.fix {
				t.Errorf("fix = %q, want exactly %q", c.Fix, tt.fix)
			}
		})
	}
}

func TestVersionChecks(t *testing.T) {
	tests := []struct {
		name, version, latest string
		n                     int
		title                 string
	}{
		{"offline", "v0.3.0", "", 1, "awsport v0.3.0 (linux/amd64)"},
		{"up to date", "0.3.0", "v0.3.0", 1, "awsport 0.3.0 (linux/amd64), the latest release"},
		{"newer local", "v0.4.0", "v0.3.9", 1, "awsport v0.4.0 (linux/amd64), the latest release"},
		{"update available", "v0.3.0", "v0.10.0", 2, "awsport v0.10.0 is available (this is v0.3.0)"},
		{"dev build", "dev", "v0.3.0", 1, "awsport dev (linux/amd64)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := versionChecks(tt.version, "linux", "amd64", tt.latest)
			if len(got) != tt.n || got[len(got)-1].Title != tt.title {
				t.Fatalf("got %+v, want %d checks ending %q", got, tt.n, tt.title)
			}
			if tt.n == 2 && (got[1].Status != Warn || got[1].Fix != "awsport update") {
				t.Errorf("update check = %+v", got[1])
			}
		})
	}
}

func TestLatestRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"tag_name":"v1.2.3"}`)
	}))
	defer srv.Close()
	tag, err := latestRelease(context.Background(), srv.Client(), srv.URL, "secret")
	if err != nil || tag != "v1.2.3" {
		t.Fatalf("got (%q, %v), want v1.2.3", tag, err)
	}
	if _, err := latestRelease(context.Background(), srv.Client(), srv.URL, ""); err == nil {
		t.Fatal("want error for 404")
	}
}

func TestToolChecks(t *testing.T) {
	got := toolChecks([]deps.Check{
		{Name: "aws", Found: true, OK: true, Version: "2.15.30", Path: "/usr/local/bin/aws"},
		{Name: "session-manager-plugin", Problem: "session-manager-plugin not found in PATH", Install: deps.Install{
			Description: "Download the plugin.",
			Commands:    [][]string{{"curl", "-fsSL", "https://example.com/p.deb", "-o", "/tmp/p.deb"}, {"sudo", "dpkg", "-i", "/tmp/p.deb"}},
			DocsURL:     "https://example.com/docs",
		}},
		{Name: "aws", Problem: "AWS CLI 2.7.0 is too old", Install: deps.Install{DocsURL: "https://example.com/aws"}},
	})
	if got[0].Status != OK || got[0].Title != "AWS CLI 2.15.30" {
		t.Errorf("ok tool = %+v", got[0])
	}
	if got[1].Status != Fail || got[1].Fix != "curl -fsSL https://example.com/p.deb -o /tmp/p.deb && sudo dpkg -i /tmp/p.deb" {
		t.Errorf("missing tool = %+v", got[1])
	}
	if got[2].Fix != "install it following https://example.com/aws" {
		t.Errorf("manual install fix = %q", got[2].Fix)
	}
}

func TestProfileSummary(t *testing.T) {
	all := []core.Profile{
		{Name: "a", Kind: core.AuthSSO, Managed: true},
		{Name: "b", Kind: core.AuthSSO},
		{Name: "c", Kind: core.AuthStatic},
	}
	checks, selected := profileSummary(all, nil, []string{"c", "missing"}, true)
	if checks[0].Title != "3 profiles: 2 sso (1 synced by awsport), 1 static" || checks[0].Detail != startSessionNote {
		t.Errorf("summary = %+v", checks[0])
	}
	if len(checks) != 2 || checks[1].Status != Fail || !strings.Contains(checks[1].Title, `"missing"`) {
		t.Errorf("missing profile check = %+v", checks)
	}
	if len(selected) != 1 || selected[0].Name != "c" {
		t.Errorf("selected = %+v", selected)
	}
	if checks, _ := profileSummary(nil, nil, nil, false); checks[0].Status != Fail || !strings.HasPrefix(checks[0].Fix, "set up SSO first") {
		t.Errorf("no profiles = %+v", checks)
	}
}

func TestAgentChecks(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if got := agentChecks(nil, nil, nil, "v1", "/s/agent.log", now); len(got) != 1 || got[0].Status != OK {
		t.Errorf("not running = %+v", got)
	}
	info := &agent.Info{Version: "v0", PID: 42, Started: now.Add(-3 * time.Hour), Sessions: 3}
	sessions := []tunnel.Session{
		{Spec: core.TunnelSpec{Label: "app http"}, Status: tunnel.Up},
		{Spec: core.TunnelSpec{Label: "db"}, Status: tunnel.AuthExpired},
		{Spec: core.TunnelSpec{Label: "mail"}, Status: tunnel.Failed, Err: "port closed"},
	}
	got := agentChecks(info, sessions, nil, "v1", "/s/agent.log", now)
	want := []struct {
		status Status
		title  string
	}{
		{OK, "agent running: pid 42, awsport v0, up 3h00m"},
		{Warn, "the agent runs awsport v0 (this is v1)"},
		{OK, "1 active tunnel"},
		{Warn, "1 tunnel waits for a new SSO sign-in"},
		{Warn, "1 tunnel failed"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if got[i].Status != w.status || got[i].Title != w.title {
			t.Errorf("check %d = %s %q, want %s %q", i, got[i].Status, got[i].Title, w.status, w.title)
		}
	}
	if got[4].Detail != "mail: port closed" || !strings.Contains(got[3].Fix, "awsport ps") {
		t.Errorf("session details = %+v / %+v", got[4], got[3])
	}
}

func TestEnvNames(t *testing.T) {
	got := envNames([]string{"AWS_PROFILE=x", "HOME=/h", "AWS_REGION=", "AWSPORT_CONFIG_DIR=/c", "AWS_PROFILE=y", "PATH=/bin"})
	if strings.Join(got, ",") != "AWSPORT_CONFIG_DIR,AWS_PROFILE" {
		t.Errorf("envNames = %v", got)
	}
}
