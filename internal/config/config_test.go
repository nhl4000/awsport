package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/ghfetch"
)

func setDirs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvConfigDir, dir)
	t.Setenv(EnvStateDir, filepath.Join(dir, "state"))
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const teamYAML = `sso:
  start_url: https://d-0000000000.awsapps.com/start
  region: us-east-1
  session: example
regions: [us-east-2, us-west-2]
presets:
  - {name: web, remote: 80}
  - {name: grafana, remote: 3001}
groups:
  - name: dev
    members:
      - {profile: example-dev, name: app, kind: ec2, remote: 8080}
  - name: data
    members:
      - {profile: example-dev, name: db, kind: rds, remote: 5432}
shell_command: bash
max_failures: 3
`

func TestPaths(t *testing.T) {
	dir := setDirs(t)
	cp, err := ConfigPath()
	if err != nil || cp != filepath.Join(dir, "config.yaml") {
		t.Fatalf("ConfigPath = %q, %v", cp, err)
	}
	tp, err := TeamConfigPath()
	if err != nil || tp != filepath.Join(dir, "team.yaml") {
		t.Fatalf("TeamConfigPath = %q, %v", tp, err)
	}
	sd, err := StateDir()
	if err != nil || sd != filepath.Join(dir, "state") {
		t.Fatalf("StateDir = %q, %v", sd, err)
	}
}

func TestXDGPaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG paths apply on Linux only")
	}
	base := t.TempDir()
	t.Setenv(EnvConfigDir, "")
	t.Setenv(EnvStateDir, "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "st"))
	if d, _ := ConfigDir(); d != filepath.Join(base, "cfg", "awsport") {
		t.Fatalf("ConfigDir = %q", d)
	}
	if d, _ := StateDir(); d != filepath.Join(base, "st", "awsport") {
		t.Fatalf("StateDir = %q", d)
	}
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", "relative")
	t.Setenv("XDG_STATE_HOME", "")
	if d, _ := ConfigDir(); d != filepath.Join(base, ".config", "awsport") {
		t.Fatalf("ConfigDir fallback = %q", d)
	}
	if d, _ := StateDir(); d != filepath.Join(base, ".local", "state", "awsport") {
		t.Fatalf("StateDir fallback = %q", d)
	}
}

func TestLoadMergePrecedence(t *testing.T) {
	no := false
	tests := []struct {
		name  string
		team  string
		user  string
		check func(t *testing.T, c *Config)
	}{
		{
			name: "no files gives defaults",
			check: func(t *testing.T, c *Config) {
				if !reflect.DeepEqual(c.Presets, DefaultPresets) {
					t.Fatalf("presets = %v", c.Presets)
				}
				if len(c.Regions) != 0 || len(c.Groups) != 0 {
					t.Fatalf("unexpected regions/groups: %v %v", c.Regions, c.Groups)
				}
				if !c.ReconnectEnabled() || c.MaxFail() != DefaultMaxFailures || c.EffectiveShell() != core.DefaultShellCommand {
					t.Fatalf("defaults wrong: %+v", c)
				}
			},
		},
		{
			name: "team only",
			team: teamYAML,
			check: func(t *testing.T, c *Config) {
				if c.SSO.Session != "example" || c.SSO.Region != "us-east-1" {
					t.Fatalf("sso = %+v", c.SSO)
				}
				if !reflect.DeepEqual(c.Regions, []string{"us-east-2", "us-west-2"}) {
					t.Fatalf("regions = %v", c.Regions)
				}
				if c.EffectiveShell() != "bash" || c.MaxFail() != 3 {
					t.Fatalf("scalars = %q %d", c.EffectiveShell(), c.MaxFail())
				}
			},
		},
		{
			name: "user wins over team",
			team: teamYAML,
			user: "sso:\n  region: eu-west-1\nregions: [ap-southeast-2]\nshell_command: zsh\nreconnect: false\nmax_failures: 9\n",
			check: func(t *testing.T, c *Config) {
				want := SSO{StartURL: "https://d-0000000000.awsapps.com/start", Region: "eu-west-1", Session: "example"}
				if c.SSO != want {
					t.Fatalf("sso = %+v", c.SSO)
				}
				if !reflect.DeepEqual(c.Regions, []string{"ap-southeast-2"}) {
					t.Fatalf("regions = %v", c.Regions)
				}
				if c.EffectiveShell() != "zsh" || c.MaxFail() != 9 || c.ReconnectEnabled() != no {
					t.Fatalf("scalars = %q %d %v", c.EffectiveShell(), c.MaxFail(), c.ReconnectEnabled())
				}
			},
		},
		{
			name: "empty user regions keep team regions",
			team: teamYAML,
			user: "shell_command: zsh\n",
			check: func(t *testing.T, c *Config) {
				if !reflect.DeepEqual(c.Regions, []string{"us-east-2", "us-west-2"}) {
					t.Fatalf("regions = %v", c.Regions)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setDirs(t)
			if tt.team != "" {
				writeFile(t, filepath.Join(dir, "team.yaml"), tt.team)
			}
			if tt.user != "" {
				writeFile(t, filepath.Join(dir, "config.yaml"), tt.user)
			}
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}

func TestLoadPresetAndGroupMerge(t *testing.T) {
	dir := setDirs(t)
	writeFile(t, filepath.Join(dir, "team.yaml"), teamYAML)
	writeFile(t, filepath.Join(dir, "config.yaml"), `presets:
  - {name: grafana-user, remote: 3001}
  - {name: redis, remote: 6379}
groups:
  - name: dev
    members:
      - {profile: example-dev, name: app, kind: ec2, remote: 3000}
  - name: mine
    members:
      - {profile: example-prod, name: app, kind: ec2, remote: 22}
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	wantPresets := []core.PortSpec{
		{Name: "mailpit", Remote: 8025},
		{Name: "web", Remote: 80},
		{Name: "https", Remote: 443},
		{Name: "app", Remote: 8080},
		{Name: "node", Remote: 3000},
		{Name: "grafana-user", Remote: 3001},
		{Name: "redis", Remote: 6379},
	}
	if !reflect.DeepEqual(c.Presets, wantPresets) {
		t.Fatalf("presets =\n%v\nwant\n%v", c.Presets, wantPresets)
	}
	var names []string
	for _, g := range c.Groups {
		names = append(names, g.Name)
	}
	if !reflect.DeepEqual(names, []string{"dev", "data", "mine"}) {
		t.Fatalf("group names = %v", names)
	}
	if got := c.Groups[0].Members[0].Remote; got != 3000 {
		t.Fatalf("user dev group should win, remote = %d", got)
	}
}

func TestLoadPortDocumentsAndBastionNames(t *testing.T) {
	dir := setDirs(t)
	writeFile(t, filepath.Join(dir, "team.yaml"), `bastion_names: [acme-bastion, "acme-jump-*"]
port_documents:
  - {name: "acme-staging-*", remote: 8025, document: acme-staging-mailpit-port-forward}
  - {profile: acme-prod, name: acme-web, remote: 8080, document: acme-prod-web-port-forward}
`)
	writeFile(t, filepath.Join(dir, "config.yaml"), `port_documents:
  - {profile: acme-prod, name: acme-web, remote: 8080, document: my-web-port-forward}
  - {name: acme-web, remote: 8080, document: other-port-forward}
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []core.PortDocument{
		{Name: "acme-staging-*", Remote: 8025, Document: "acme-staging-mailpit-port-forward"},
		{Profile: "acme-prod", Name: "acme-web", Remote: 8080, Document: "my-web-port-forward"},
		{Name: "acme-web", Remote: 8080, Document: "other-port-forward"},
	}
	if !reflect.DeepEqual(c.PortDocuments, want) {
		t.Fatalf("port_documents =\n%v\nwant\n%v", c.PortDocuments, want)
	}
	if !reflect.DeepEqual(c.BastionNames, []string{"acme-bastion", "acme-jump-*"}) {
		t.Fatalf("bastion_names = %v", c.BastionNames)
	}

	writeFile(t, filepath.Join(dir, "config.yaml"), "bastion_names: [my-bastion]\n")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.BastionNames, []string{"my-bastion"}) {
		t.Fatalf("user bastion_names should replace team's, got %v", c.BastionNames)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := setDirs(t)
	writeFile(t, filepath.Join(dir, "config.yaml"), "bogus: 1\n")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestSaveUserRoundtrip(t *testing.T) {
	dir := setDirs(t)
	yes := true
	in := &Config{
		SSO:        SSO{StartURL: "https://d-0000000000.awsapps.com/start", Region: "us-east-1"},
		Regions:    []string{"us-east-2"},
		Presets:    []core.PortSpec{{Name: "redis", Remote: 6379}},
		Groups:     []core.Group{{Name: "g", Members: []core.GroupMember{{Profile: "example", Name: "app", Kind: core.ResourceKind("ec2"), Remote: 80}}}},
		Reconnect:  &yes,
		TeamSource: "https://example.com/team.yaml",
	}
	if err := SaveUser(in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadUser()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("roundtrip mismatch:\n%+v\n%+v", in, out)
	}
	fi, err := os.Stat(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestSaveUserCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cfg")
	t.Setenv(EnvConfigDir, dir)
	if err := SaveUser(&Config{ShellCommand: "bash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestImportTeamFromFile(t *testing.T) {
	dir := setDirs(t)
	src := filepath.Join(t.TempDir(), "awsport.team.yaml")
	writeFile(t, src, teamYAML)
	writeFile(t, filepath.Join(dir, "config.yaml"), "shell_command: zsh\n")

	team, err := ImportTeam(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if team.SSO.Session != "example" || len(team.Groups) != 2 {
		t.Fatalf("team = %+v", team)
	}
	got, err := os.ReadFile(filepath.Join(dir, "team.yaml"))
	if err != nil || string(got) != teamYAML {
		t.Fatalf("team.yaml = %q, %v", got, err)
	}
	user, err := LoadUser()
	if err != nil {
		t.Fatal(err)
	}
	if user.TeamSource != src || user.ShellCommand != "zsh" {
		t.Fatalf("user = %+v", user)
	}
}

func TestImportTeamFromHTTP(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "valid", status: http.StatusOK, body: teamYAML},
		{name: "not found", status: http.StatusNotFound, body: teamYAML, wantErr: true},
		{name: "html page", status: http.StatusOK, body: "<html><body>login</body></html>", wantErr: true},
		{name: "unknown key", status: http.StatusOK, body: "sso:\n  start_url: x\nextra: true\n", wantErr: true},
		{name: "empty", status: http.StatusOK, body: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setDirs(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			url := srv.URL + "/team.yaml"

			team, err := ImportTeam(context.Background(), url)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if _, serr := os.Stat(filepath.Join(dir, "team.yaml")); serr == nil {
					t.Fatal("team.yaml written despite error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(team.Regions, []string{"us-east-2", "us-west-2"}) {
				t.Fatalf("regions = %v", team.Regions)
			}
			user, err := LoadUser()
			if err != nil {
				t.Fatal(err)
			}
			if user.TeamSource != url {
				t.Fatalf("TeamSource = %q", user.TeamSource)
			}
			eff, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if eff.SSO.Session != "example" || eff.TeamSource != url {
				t.Fatalf("effective = %+v", eff)
			}
		})
	}
}

// useGitHub points GitHub imports at srv with no ambient credentials.
func useGitHub(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	old := gitHub
	gitHub = &ghfetch.Client{BaseURL: srv.URL, GHToken: func(context.Context) string { return "" }}
	t.Cleanup(func() { gitHub = old })
}

func TestImportTeamFromGitHub(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		wantPath string
		wantRef  string
	}{
		{"short form", "github:acme/platform/deploy/awsport.team.yaml@main", "/repos/acme/platform/contents/deploy/awsport.team.yaml", "main"},
		{"short form default branch", "github:acme/platform/awsport.team.yaml", "/repos/acme/platform/contents/awsport.team.yaml", ""},
		{"web URL", "https://github.com/acme/platform/blob/v1/awsport.team.yaml", "/repos/acme/platform/contents/awsport.team.yaml", "v1"},
		{"raw URL", "https://raw.githubusercontent.com/acme/platform/main/awsport.team.yaml", "/repos/acme/platform/contents/awsport.team.yaml", "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setDirs(t)
			var gotPath, gotRef, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotRef, gotAuth = r.URL.Path, r.URL.Query().Get("ref"), r.Header.Get("Authorization")
				_, _ = w.Write([]byte(teamYAML))
			}))
			defer srv.Close()
			useGitHub(t, srv)
			t.Setenv("GH_TOKEN", "t-gh")

			team, err := ImportTeam(context.Background(), "  "+tt.src+"\n")
			if err != nil {
				t.Fatal(err)
			}
			if gotPath != tt.wantPath || gotRef != tt.wantRef || gotAuth != "Bearer t-gh" {
				t.Fatalf("request path=%q ref=%q auth=%q", gotPath, gotRef, gotAuth)
			}
			if len(team.Groups) != 2 {
				t.Fatalf("team = %+v", team)
			}
			got, err := os.ReadFile(filepath.Join(dir, "team.yaml"))
			if err != nil || string(got) != teamYAML {
				t.Fatalf("team.yaml = %q, %v", got, err)
			}
			user, err := LoadUser()
			if err != nil {
				t.Fatal(err)
			}
			if user.TeamSource != tt.src {
				t.Fatalf("TeamSource = %q, want %q", user.TeamSource, tt.src)
			}
		})
	}
}

func TestImportTeamFromGitHubErrors(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		status int
		body   string
		want   string
	}{
		{"not found", "github:acme/platform/missing.yaml@main", http.StatusNotFound, `{"message":"Not Found"}`,
			"import team config: can't read acme/platform/missing.yaml@main: the file is missing or acme/platform is private"},
		{"invalid YAML", "github:acme/platform/a.yaml", http.StatusOK, "sso:\n  start_url: x\nextra: true\n",
			"import team config from github:acme/platform/a.yaml: invalid awsport config YAML"},
		{"too large", "github:acme/platform/a.yaml", http.StatusOK, strings.Repeat("#", maxTeamConfigSize+1),
			"can't read acme/platform/a.yaml: read: exceeds"},
		{"malformed source", "github:acme/platform", http.StatusOK, teamYAML, "invalid GitHub source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setDirs(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			useGitHub(t, srv)

			_, err := ImportTeam(context.Background(), tt.src)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
			if _, serr := os.Stat(filepath.Join(dir, "team.yaml")); serr == nil {
				t.Fatal("team.yaml written despite error")
			}
		})
	}
}

func TestLoadTeam(t *testing.T) {
	dir := setDirs(t)
	if team, err := LoadTeam(); err != nil || team != nil {
		t.Fatalf("missing team.yaml: %+v, %v", team, err)
	}
	writeFile(t, filepath.Join(dir, "team.yaml"), teamYAML)
	team, err := LoadTeam()
	if err != nil || len(team.Regions) != 2 {
		t.Fatalf("LoadTeam = %+v, %v", team, err)
	}
}
