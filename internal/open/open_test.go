package open

import (
	"errors"
	"reflect"
	"testing"

	"github.com/nhl4000/awsport/internal/core"
)

func TestURL(t *testing.T) {
	tests := []struct {
		name   string
		spec   core.TunnelSpec
		want   string
		wantOK bool
	}{
		{"ec2 http", core.TunnelSpec{RemotePort: 8080, LocalPort: 18080}, "http://127.0.0.1:18080", true},
		{"ec2 80", core.TunnelSpec{RemotePort: 80, LocalPort: 8000}, "http://127.0.0.1:8000", true},
		{"ec2 443 https", core.TunnelSpec{RemotePort: 443, LocalPort: 8443}, "https://127.0.0.1:8443", true},
		{"ec2 8443 https", core.TunnelSpec{RemotePort: 8443, LocalPort: 9443}, "https://127.0.0.1:9443", true},
		{"ec2 ssh", core.TunnelSpec{RemotePort: 22, LocalPort: 2222}, "", false},
		{"ec2 postgres", core.TunnelSpec{RemotePort: 5432, LocalPort: 5432}, "", false},
		{"ec2 redis", core.TunnelSpec{RemotePort: 6379, LocalPort: 6379}, "", false},
		{"ec2 smtp sink", core.TunnelSpec{RemotePort: 1025, LocalPort: 1025}, "", false},
		{"ec2 mongo", core.TunnelSpec{RemotePort: 27017, LocalPort: 27017}, "", false},
		{"rds never", core.TunnelSpec{RemoteHost: "db.example.com", RemotePort: 8080, LocalPort: 8080}, "", false},
		{"no local port", core.TunnelSpec{RemotePort: 8080}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := URL(tt.spec)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("URL = %q, %v; want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestBrowserCommand(t *testing.T) {
	const u = "http://127.0.0.1:8080/?a=1&b=2"
	found := func(string) (string, error) { return "/usr/bin/wslview", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	tests := []struct {
		name     string
		goos     string
		wsl      bool
		look     func(string) (string, error)
		wantName string
		wantArgs []string
		wantErr  bool
	}{
		{"macos", "darwin", false, missing, "open", []string{u}, false},
		{"windows", "windows", false, missing, "rundll32", []string{"url.dll,FileProtocolHandler", u}, false},
		{"linux", "linux", false, found, "xdg-open", []string{u}, false},
		{"wsl wslview", "linux", true, found, "/usr/bin/wslview", []string{u}, false},
		{"wsl cmd fallback", "linux", true, missing, "cmd.exe", []string{"/c", "start", "", "http://127.0.0.1:8080/?a=1^&b=2"}, false},
		{"unsupported", "plan9", false, missing, "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, args, err := browserCommand(tt.goos, tt.wsl, tt.look, u)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if name != tt.wantName || !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("got %q %q, want %q %q", name, args, tt.wantName, tt.wantArgs)
			}
		})
	}
}
