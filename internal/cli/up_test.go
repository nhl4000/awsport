package cli

import (
	"strings"
	"testing"

	"github.com/nhl4000/awsport/internal/core"
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
