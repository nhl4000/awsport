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
	const u = "http://127.0.0.1:8080/#/device?a=1&b=2"
	only := func(names ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			for _, x := range names {
				if x == n {
					return "/usr/bin/" + n, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	files := func(paths ...string) func(string) bool {
		return func(p string) bool {
			for _, x := range paths {
				if x == p {
					return true
				}
			}
			return false
		}
	}
	const sys = "/mnt/c/Windows/System32/rundll32.exe"
	rundllArgs := []string{"url.dll,FileProtocolHandler", u}
	tests := []struct {
		name    string
		goos    string
		wsl     bool
		display bool
		look    func(string) (string, error)
		exists  func(string) bool
		mount   string
		want    launcher
		wantErr bool
	}{
		{"macos", "darwin", false, false, only(), files(), "/mnt/", launcher{name: "open", args: []string{u}}, false},
		{"windows", "windows", false, false, only(), files(), "/mnt/", launcher{name: "rundll32", args: rundllArgs}, false},
		{"linux", "linux", false, true, only("xdg-open"), files(), "/mnt/", launcher{name: "xdg-open", args: []string{u}}, false},
		{"linux without display", "linux", false, false, only("xdg-open"), files(), "/mnt/", launcher{}, true},
		{"wsl wslview", "linux", true, false, only("wslview"), files(), "/mnt/", launcher{name: "/usr/bin/wslview", args: []string{u}}, false},
		{"wsl rundll32 on PATH", "linux", true, false, only("rundll32.exe"), files("/mnt/c"), "/mnt/",
			launcher{name: "/usr/bin/rundll32.exe", args: rundllArgs, dir: "/mnt/c"}, false},
		{"wsl windows not on PATH", "linux", true, false, only(), files(sys, "/mnt/c"), "/mnt/",
			launcher{name: sys, args: rundllArgs, dir: "/mnt/c"}, false},
		{"wsl custom mount root", "linux", true, false, only(), files("/win/c/Windows/System32/rundll32.exe", "/win/c"), "/win",
			launcher{name: "/win/c/Windows/System32/rundll32.exe", args: rundllArgs, dir: "/win/c"}, false},
		{"wsl without interop", "linux", true, false, only(), files(), "/mnt/", launcher{}, true},
		{"unsupported", "plan9", false, true, only(), files(), "/mnt/", launcher{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := browserCommand(tt.goos, tt.wsl, tt.display, tt.look, tt.exists, tt.mount, u)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseMountRoot(t *testing.T) {
	tests := map[string]string{
		"":                                     "/mnt/",
		"[boot]\nsystemd=true\n":               "/mnt/",
		"[automount]\nroot = /win/\n":          "/win/",
		"[automount]\nroot=\"/drives\"\n":      "/drives",
		"[network]\nroot=/nope\n[automount]\n": "/mnt/",
	}
	for conf, want := range tests {
		if got := parseMountRoot(conf); got != want {
			t.Errorf("parseMountRoot(%q) = %q, want %q", conf, got, want)
		}
	}
}
