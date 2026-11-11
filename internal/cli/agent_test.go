package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

func session(key, label, profile, region string, remote int, rds bool) tunnel.Session {
	spec := core.TunnelSpec{Key: key, Label: label, Profile: profile, Region: region, RemotePort: remote}
	if rds {
		spec.RemoteHost = "db.example.internal"
	}
	return tunnel.Session{Spec: spec, Status: tunnel.Up}
}

func TestMatchSessions(t *testing.T) {
	sessions := []tunnel.Session{
		session("example-dev/us-east-1/i-0a/8025", "app mailpit", "example-dev", "us-east-1", 8025, false),
		session("example-dev/us-east-1/i-0a/8080", "app 8080", "example-dev", "us-east-1", 8080, false),
		session("example-dev/us-east-1/i-0b/80", "app server 80", "example-dev", "us-east-1", 80, false),
		session("example-dev/us-east-1/db/5432", "db postgres", "example-dev", "us-east-1", 5432, true),
		session("example-prod/us-east-1/i-0c/8080", "app 8080", "example-prod", "us-east-1", 8080, false),
	}
	groups := []core.Group{
		{Name: "stack", Members: []core.GroupMember{
			{Profile: "example-dev", Name: "app", Remote: 8080},
			{Profile: "example-dev", Name: "db", Kind: core.KindRDS},
		}},
		{Name: "west", Members: []core.GroupMember{{Profile: "example-dev", Region: "us-west-2", Name: "app"}}},
		{Name: "mailpit", Members: []core.GroupMember{{Profile: "example-prod", Name: "app"}}},
	}
	tests := []struct {
		name      string
		targets   []string
		want      []string
		unmatched []string
	}{
		{"exact key", []string{"example-dev/us-east-1/i-0a/8080"}, []string{"example-dev/us-east-1/i-0a/8080"}, nil},
		{"label substring is case insensitive", []string{"MailPit"},
			[]string{"example-dev/us-east-1/i-0a/8025"}, nil},
		{"label substring matches several", []string{"8080"},
			[]string{"example-dev/us-east-1/i-0a/8080", "example-prod/us-east-1/i-0c/8080"}, nil},
		{"group members by name and port, rds by name", []string{"stack"},
			[]string{"example-dev/us-east-1/db/5432", "example-dev/us-east-1/i-0a/8080"}, nil},
		{"group name wins over label", []string{"mailpit"}, []string{"example-prod/us-east-1/i-0c/8080"}, nil},
		{"group region filters", []string{"west"}, []string{}, []string{"west"}},
		{"overlapping targets and a miss", []string{"db", "postgres", "redis"},
			[]string{"example-dev/us-east-1/db/5432"}, []string{"redis"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, unmatched := matchSessions(sessions, tt.targets, groups)
			if !slices.Equal(got, tt.want) || !slices.Equal(unmatched, tt.unmatched) {
				t.Fatalf("matchSessions(%v) = %v, unmatched %v; want %v, unmatched %v",
					tt.targets, got, unmatched, tt.want, tt.unmatched)
			}
		})
	}
}

func TestMemberMatchesWholeName(t *testing.T) {
	spec := core.TunnelSpec{Label: "app server 80", Profile: "p", RemotePort: 80}
	if memberMatches(core.GroupMember{Profile: "p", Name: "app"}, spec) {
		t.Fatal(`member "app" must not match resource "app server"`)
	}
	if !memberMatches(core.GroupMember{Profile: "p", Name: "app server"}, spec) {
		t.Fatal(`member "app server" must match`)
	}
}

func TestWriteSessionsTable(t *testing.T) {
	now := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)
	sessions := []tunnel.Session{
		{Spec: core.TunnelSpec{Label: "app mailpit", Profile: "example-dev", LocalPort: 18025}, Status: tunnel.Up,
			Since: now.Add(-90 * time.Second), LastLog: []string{"Starting session", "Waiting for connections..."}},
		{Spec: core.TunnelSpec{Label: "db postgres", Profile: "example-dev", LocalPort: 15433, Temporary: true},
			Status: tunnel.Failed, Since: now, LastLog: []string{strings.Repeat("x", 100)}},
	}
	var b strings.Builder
	if err := writeSessionsTable(&b, sessions, now); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"app mailpit", "127.0.0.1:18025", "1m30s", "Waiting for connections...",
		"127.0.0.1:15433 *", "failed", strings.Repeat("x", lastLogWidth-1) + "…"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}
