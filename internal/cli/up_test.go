package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in      string
		want    upTarget
		wantErr bool
	}{
		{in: "dev-stack", want: upTarget{Group: "dev-stack"}},
		{in: "example-dev/app", want: upTarget{Profile: "example-dev", Name: "app"}},
		{in: "example-dev/app:8025", want: upTarget{Profile: "example-dev", Name: "app", Port: "8025"}},
		{in: "example-dev/app:mailpit", want: upTarget{Profile: "example-dev", Name: "app", Port: "mailpit"}},
		{in: "example-dev/team/app:80", want: upTarget{Profile: "example-dev", Name: "team/app", Port: "80"}},
		{in: "example-dev/a:b:443", want: upTarget{Profile: "example-dev", Name: "a:b", Port: "443"}},
		{in: " example-dev/db ", want: upTarget{Profile: "example-dev", Name: "db"}},
		{in: "", wantErr: true},
		{in: "/app", wantErr: true},
		{in: "example-dev/", wantErr: true},
		{in: "example-dev/app:", wantErr: true},
		{in: "example-dev/:80", wantErr: true},
		{in: "example-dev/app:0", wantErr: true},
		{in: "example-dev/app:70000", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseTarget(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTarget(%q) = %+v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTarget(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("parseTarget(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRemotePort(t *testing.T) {
	ec2 := core.Resource{Kind: core.KindEC2, Profile: "p", Name: "app",
		Ports: []core.PortSpec{{Name: "mailpit", Remote: 8025}, {Name: "app", Remote: 8080}}}
	single := core.Resource{Kind: core.KindEC2, Name: "one", Ports: []core.PortSpec{{Name: "http", Remote: 80}}}
	rds := core.Resource{Kind: core.KindRDS, Name: "db", Port: 5432}
	tests := []struct {
		name    string
		r       core.Resource
		port    string
		want    int
		wantErr string
	}{
		{"number", ec2, "9000", 9000, ""},
		{"port name", ec2, "MailPit", 8025, ""},
		{"unknown name", ec2, "redis", 0, "no port named"},
		{"ambiguous default", ec2, "", 0, "several ports"},
		{"single default", single, "", 80, ""},
		{"rds default", rds, "", 5432, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := remotePort(tt.r, tt.port)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("remotePort = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

func TestOwnKeys(t *testing.T) {
	spec := func(key string) core.TunnelSpec { return core.TunnelSpec{Key: key} }
	before := map[string]bool{"running": true}
	got := ownKeys(before, []core.TunnelSpec{spec("new"), spec("running"), spec("other"), spec("new")})
	if !slices.Equal(got, []string{"new", "other"}) {
		t.Fatalf("ownKeys = %v, want [new other]", got)
	}
	if got := ownKeys(before, []core.TunnelSpec{spec("running")}); len(got) != 0 {
		t.Fatalf("ownKeys = %v, want none", got)
	}
}

func TestActiveKeys(t *testing.T) {
	sessions := []tunnel.Session{
		{Spec: core.TunnelSpec{Key: "up"}, Status: tunnel.Up},
		{Spec: core.TunnelSpec{Key: "failed"}, Status: tunnel.Failed},
		{Spec: core.TunnelSpec{Key: "starting"}, Status: tunnel.Starting},
	}
	got := activeKeys(sessions)
	if len(got) != 2 || !got["up"] || !got["starting"] {
		t.Fatalf("activeKeys = %v", got)
	}
}

func TestAnyActive(t *testing.T) {
	sessions := map[string]tunnel.Status{"mine-failed": tunnel.Failed, "mine-up": tunnel.Up, "other": tunnel.Up}
	get := func(key string) (tunnel.Session, bool) {
		st, ok := sessions[key]
		return tunnel.Session{Status: st}, ok
	}
	if !anyActive(get, []string{"mine-failed", "mine-up"}) {
		t.Fatal("an own tunnel is up")
	}
	// Tunnels started by someone else do not keep `up` running.
	if anyActive(get, []string{"mine-failed", "mine-gone"}) {
		t.Fatal("no own tunnel is active")
	}
}
