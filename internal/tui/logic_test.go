package tui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/tunnel"
)

func TestVisibleResources(t *testing.T) {
	rs := []core.Resource{
		{ID: "i-1"},
		{ID: "i-2", Unreachable: "instance stopped"},
		{ID: "db-1"},
	}
	tests := []struct {
		name    string
		showAll bool
		want    []string
	}{
		{"connectable only", false, []string{"i-1", "db-1"}},
		{"show all keeps order", true, []string{"i-1", "i-2", "db-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, r := range visibleResources(rs, tt.showAll) {
				got = append(got, r.ID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
	if n := hiddenCount(rs); n != 1 {
		t.Fatalf("hiddenCount = %d, want 1", n)
	}
}

func TestParsePortSpec(t *testing.T) {
	tests := []struct {
		in      string
		want    core.PortSpec
		wantErr bool
	}{
		{"8080", core.PortSpec{Remote: 8080}, false},
		{" app:3000 ", core.PortSpec{Name: "app", Remote: 3000}, false},
		{"mail pit:8025", core.PortSpec{}, true},
		{":80", core.PortSpec{}, true},
		{"web:http", core.PortSpec{}, true},
		{"0", core.PortSpec{}, true},
		{"65536", core.PortSpec{}, true},
		{"", core.PortSpec{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parsePortSpec(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestStatusText(t *testing.T) {
	tests := []struct {
		name string
		s    tunnel.Session
		want string
	}{
		{"up", tunnel.Session{Status: tunnel.Up}, "up"},
		{"reconnecting with attempts", tunnel.Session{Status: tunnel.Reconnecting, Attempts: 3}, "reconnecting (attempt 3)"},
		{"reconnecting without attempts", tunnel.Session{Status: tunnel.Reconnecting}, "reconnecting"},
		{"failed with error", tunnel.Session{Status: tunnel.Failed, Err: "exit status 255"}, "failed: exit status 255"},
		{"failed without error", tunnel.Session{Status: tunnel.Failed}, "failed"},
		{"auth expired", tunnel.Session{Status: tunnel.AuthExpired}, "auth expired (press L)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusText(tt.s); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLastLogLine(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"Starting session", "Waiting   for connections...", "", "  "}, "Waiting for connections..."},
		{[]string{"only"}, "only"},
	}
	for _, tt := range tests {
		if got := lastLogLine(tt.in); got != tt.want {
			t.Errorf("lastLogLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{45 * time.Second, "45s"},
		{12*time.Minute + 30*time.Second, "12m"},
		{3*time.Hour + 5*time.Minute, "3h05m"},
	}
	for _, tt := range tests {
		if got := humanDuration(tt.d); got != tt.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestAuthExpiredProfiles(t *testing.T) {
	ss := []tunnel.Session{
		{Spec: core.TunnelSpec{Profile: "beta"}, Status: tunnel.AuthExpired},
		{Spec: core.TunnelSpec{Profile: "alpha"}, Status: tunnel.AuthExpired},
		{Spec: core.TunnelSpec{Profile: "beta"}, Status: tunnel.AuthExpired},
		{Spec: core.TunnelSpec{Profile: "gamma"}, Status: tunnel.Up},
	}
	if got, want := authExpiredProfiles(ss), []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTunnelRef(t *testing.T) {
	ec2 := core.Resource{Profile: "example-dev", Region: "us-east-1", ID: "i-0123456789abcdef0"}
	tests := []struct {
		name   string
		spec   core.TunnelSpec
		want   string
		wantOK bool
	}{
		{"ec2 key", core.TunnelSpec{Key: ec2.TunnelKey(8025), Profile: ec2.Profile, Region: ec2.Region, RemotePort: 8025}, ec2.Ref(), true},
		{"profile with slash", core.TunnelSpec{Key: "team/dev/us-east-1/db-1/5432", Profile: "team/dev", Region: "us-east-1", RemotePort: 5432}, "team/dev/us-east-1/db-1", true},
		{"mismatched port", core.TunnelSpec{Key: ec2.TunnelKey(80), Profile: ec2.Profile, Region: ec2.Region, RemotePort: 8080}, "", false},
		{"no id", core.TunnelSpec{Key: "p/r/80", Profile: "p", Region: "r", RemotePort: 80}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tunnelRef(tt.spec)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestBuildGroup(t *testing.T) {
	app := core.Resource{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0aaa", Name: "app-server"}
	db := core.Resource{Kind: core.KindRDS, Profile: "example-dev", Region: "us-east-1", ID: "app-db", Name: "app-db"}
	noName := core.Resource{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0bbb"}
	known := map[string]core.Resource{app.Ref(): app, db.Ref(): db, noName.Ref(): noName}
	lookup := func(ref string) (core.Resource, bool) { r, ok := known[ref]; return r, ok }
	spec := func(r core.Resource, remote int) core.TunnelSpec {
		return core.TunnelSpec{Key: r.TunnelKey(remote), Profile: r.Profile, Region: r.Region, RemotePort: remote}
	}
	missing := core.Resource{Profile: "example-prod", Region: "us-east-2", ID: "i-0ccc"}

	tests := []struct {
		name           string
		group          string
		specs          []core.TunnelSpec
		wantMembers    []core.GroupMember
		wantUnresolved []string
		wantErr        bool
	}{
		{
			name:  "resolves name and kind, dedupes",
			group: " dev ",
			specs: []core.TunnelSpec{spec(app, 8025), spec(db, 5432), spec(app, 8025), spec(noName, 22)},
			wantMembers: []core.GroupMember{
				{Profile: "example-dev", Region: "us-east-1", Name: "app-server", Kind: core.KindEC2, Remote: 8025},
				{Profile: "example-dev", Region: "us-east-1", Name: "app-db", Kind: core.KindRDS, Remote: 5432},
				{Profile: "example-dev", Region: "us-east-1", Name: "i-0bbb", Kind: core.KindEC2, Remote: 22},
			},
		},
		{
			name:           "unresolved reported",
			group:          "mixed",
			specs:          []core.TunnelSpec{spec(app, 80), spec(missing, 80)},
			wantMembers:    []core.GroupMember{{Profile: "example-dev", Region: "us-east-1", Name: "app-server", Kind: core.KindEC2, Remote: 80}},
			wantUnresolved: []string{missing.TunnelKey(80)},
		},
		{name: "nothing resolved", group: "x", specs: []core.TunnelSpec{spec(missing, 80)}, wantUnresolved: []string{missing.TunnelKey(80)}, wantErr: true},
		{name: "empty name", group: "  ", specs: []core.TunnelSpec{spec(app, 80)}, wantErr: true},
		{name: "no specs", group: "x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, unresolved, err := buildGroup(tt.group, tt.specs, lookup)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(unresolved, tt.wantUnresolved) {
				t.Fatalf("unresolved = %v, want %v", unresolved, tt.wantUnresolved)
			}
			if tt.wantErr {
				return
			}
			if want := strings.TrimSpace(tt.group); g.Name != want {
				t.Fatalf("name = %q, want %q", g.Name, want)
			}
			if !reflect.DeepEqual(g.Members, tt.wantMembers) {
				t.Fatalf("members = %+v, want %+v", g.Members, tt.wantMembers)
			}
		})
	}
}

func TestLoginPlan(t *testing.T) {
	all := []core.Profile{
		{Name: "dev", Kind: core.AuthSSO, SSOSession: "awsport"},
		{Name: "prod", Kind: core.AuthSSO, SSOSession: "awsport"},
		{Name: "other", Kind: core.AuthSSO, SSOSession: "corp"},
		{Name: "legacy", Kind: core.AuthSSO},
		{Name: "keys", Kind: core.AuthStatic},
	}
	sessions, profiles, unknown := loginPlan([]string{"dev", "prod", "other", "legacy", "keys", "missing"}, all)
	if want := []string{"awsport", "corp"}; !reflect.DeepEqual(sessions, want) {
		t.Errorf("sessions = %v, want %v", sessions, want)
	}
	if want := []string{"legacy"}; !reflect.DeepEqual(profiles, want) {
		t.Errorf("profiles = %v, want %v", profiles, want)
	}
	if want := []string{"keys", "missing"}; !reflect.DeepEqual(unknown, want) {
		t.Errorf("unknown = %v, want %v", unknown, want)
	}
}

func TestDepsHelpers(t *testing.T) {
	checks := []deps.Check{
		{Name: "aws", OK: true},
		{Name: "session-manager-plugin", OK: false},
		{Name: "extra", OK: false},
	}
	if depsOK(checks) {
		t.Fatal("depsOK = true with failing checks")
	}
	if !depsOK(checks[:1]) || !depsOK(nil) {
		t.Fatal("depsOK = false with passing checks")
	}
	c, ok := firstFailing(checks)
	if !ok || c.Name != "session-manager-plugin" {
		t.Fatalf("firstFailing = %v, %v", c.Name, ok)
	}
	if got, want := failingNames(checks), []string{"session-manager-plugin", "extra"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("failingNames = %v, want %v", got, want)
	}
}

func TestValidateSSO(t *testing.T) {
	tests := []struct {
		name    string
		in      config.SSO
		wantErr bool
	}{
		{"valid", config.SSO{StartURL: "https://d-0000000000.awsapps.com/start", Region: "us-east-1"}, false},
		{"valid gov region", config.SSO{StartURL: "https://d-0000000000.awsapps.com/start", Region: "us-gov-west-1"}, false},
		{"http rejected", config.SSO{StartURL: "http://d-0000000000.awsapps.com/start", Region: "us-east-1"}, true},
		{"missing url", config.SSO{Region: "us-east-1"}, true},
		{"bad region", config.SSO{StartURL: "https://d-0000000000.awsapps.com/start", Region: "useast1"}, true},
		{"bad session", config.SSO{StartURL: "https://d-0000000000.awsapps.com/start", Region: "us-east-1", Session: "my session"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSSO(normalizeSSO(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
	if got := normalizeSSO(config.SSO{StartURL: " https://x.example.com/start "}); got.Session != defaultSSOSession || got.StartURL != "https://x.example.com/start" {
		t.Fatalf("normalizeSSO = %+v", got)
	}
}

func TestWithPort(t *testing.T) {
	ports := []core.PortSpec{{Name: "http", Remote: 80}, {Name: "app", Remote: 8080}}
	got := withPort(ports, core.PortSpec{Name: "web", Remote: 80})
	want := []core.PortSpec{{Name: "app", Remote: 8080}, {Name: "web", Remote: 80}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(ports) != 2 || ports[0].Name != "http" {
		t.Fatalf("input mutated: %v", ports)
	}
}

func TestShellJoin(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{[]string{"sudo", "./aws/install"}, "sudo ./aws/install"},
		{[]string{"msiexec", "/i", "C:\\Program Files\\x.msi"}, `msiexec /i 'C:\Program Files\x.msi'`},
		{[]string{"echo", "it's"}, `echo 'it'\''s'`},
	}
	for _, tt := range tests {
		if got := shellJoin(tt.in); got != tt.want {
			t.Errorf("shellJoin(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
