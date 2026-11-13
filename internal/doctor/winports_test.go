package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
	"github.com/nhl4000/awsport/internal/winports"
)

func TestAWSPortPorts(t *testing.T) {
	saved := map[string]int{
		"acme-prod/us-east-1/web-app/8025": 18025,
		"acme-prod/us-east-1/wp-app/8025":  18026,
		"acme-dev/us-east-1/web-app/5432":  15432,
	}
	sessions := []tunnel.Session{
		{Status: tunnel.Up, Spec: core.TunnelSpec{Label: "web-app mailpit", Profile: "acme-prod", LocalPort: 18030}}, // temporary port
		{Status: tunnel.Stopped, Spec: core.TunnelSpec{Label: "old", Profile: "acme-prod", LocalPort: 19999}},
	}
	got := awsportPorts(saved, sessions)
	if got[18026][0] != "wp-app:8025 (acme-prod)" || got[18030][0] != "web-app mailpit (acme-prod)" {
		t.Errorf("labels = %v", got)
	}
	if _, ok := got[19999]; ok {
		t.Error("stopped session port included")
	}
	if len(got) != 4 {
		t.Errorf("got %d ports, want 4: %v", len(got), got)
	}
}

func TestWindowsPortChecks(t *testing.T) {
	ports := map[int][]string{18025: {"web-app:8025 (acme-prod)"}, 18026: {"wp-app:8025 (acme-prod)"}}
	relay := winports.Listener{Addr: "127.0.0.1", Port: 18025, PID: 3140, Process: "wslrelay.exe"}
	proxySvc := winports.Listener{Addr: "0.0.0.0", Port: 18026, PID: 5084, Process: "svchost.exe"}
	rule := winports.Proxy{Kind: "v4tov4", ListenAddr: "0.0.0.0", ListenPort: 18026, ConnectAddr: "127.0.0.1", ConnectPort: 18025}

	t.Run("portproxy rule shadows a tunnel", func(t *testing.T) {
		got := windowsPortChecks(ports, []winports.Listener{relay, proxySvc}, []winports.Proxy{rule}, nil)
		if len(got) != 1 || got[0].Status != Fail {
			t.Fatalf("got %+v, want one failure (svchost serving the rule not reported twice)", got)
		}
		if !strings.Contains(got[0].Title, "port 18026 to 127.0.0.1:18025") || !strings.Contains(got[0].Title, "wp-app:8025 (acme-prod)") ||
			got[0].Fix != "in an administrator PowerShell: netsh interface portproxy delete v4tov4 listenaddress=0.0.0.0 listenport=18026" {
			t.Errorf("check = %+v", got[0])
		}
	})
	t.Run("other Windows program on a port", func(t *testing.T) {
		other := winports.Listener{Addr: "0.0.0.0", Port: 18025, PID: 777, Process: "node.exe"}
		got := windowsPortChecks(ports, []winports.Listener{relay, other, other}, nil, nil)
		if len(got) != 1 || got[0].Status != Warn || !strings.Contains(got[0].Title, "node.exe (PID 777)") || !strings.Contains(got[0].Fix, "Get-Process -Id 777") {
			t.Fatalf("got %+v, want one warning about node.exe", got)
		}
	})
	t.Run("only WSL's own forwarder", func(t *testing.T) {
		got := windowsPortChecks(ports, []winports.Listener{relay}, []winports.Proxy{{ListenPort: 3001, ConnectPort: 3001}}, nil)
		if len(got) != 1 || got[0].Status != OK || !strings.Contains(got[0].Title, "2 port(s)") {
			t.Fatalf("got %+v, want one ok", got)
		}
	})
	t.Run("nothing saved yet", func(t *testing.T) {
		if got := windowsPortChecks(nil, nil, nil, nil); len(got) != 1 || got[0].Status != OK {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("inspection failed", func(t *testing.T) {
		got := windowsPortChecks(ports, nil, nil, errors.New("can't run netstat.exe"))
		if len(got) != 1 || got[0].Status != Warn || got[0].Fix == "" {
			t.Fatalf("got %+v", got)
		}
	})
}
