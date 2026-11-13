package winports

import (
	"reflect"
	"testing"
)

func TestParseNetstat(t *testing.T) {
	out := `
Active Connections

  Proto  Local Address          Foreign Address        State           PID
  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       1632
  TCP    0.0.0.0:18026          0.0.0.0:0              LISTENING       5084
  TCP    127.0.0.1:18025        0.0.0.0:0              LISTENING       3140
  TCP    127.0.0.1:50000        127.0.0.1:50001        ESTABLISHED     900
  TCP    [::]:445               [::]:0                 LISTENING       4
  TCP    [::1]:18025            [::]:0                 ABHÖREN         3140
  UDP    0.0.0.0:5353           *:*                                    1200
`
	want := []Listener{
		{Addr: "0.0.0.0", Port: 135, PID: 1632},
		{Addr: "0.0.0.0", Port: 18026, PID: 5084},
		{Addr: "127.0.0.1", Port: 18025, PID: 3140},
		{Addr: "::", Port: 445, PID: 4},
		{Addr: "::1", Port: 18025, PID: 3140}, // localized state column still detected
	}
	if got := ParseNetstat(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseNetstat =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseTasklist(t *testing.T) {
	out := `"System Idle Process","0","Services","0","8 K"
"svchost.exe","5084","Services","0","36,968 K"
"wslrelay.exe","3140","Console","1","38,344 K"
`
	want := map[int]string{0: "System Idle Process", 5084: "svchost.exe", 3140: "wslrelay.exe"}
	if got := ParseTasklist(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseTasklist = %v, want %v", got, want)
	}
}

func TestParsePortproxy(t *testing.T) {
	out := `
Listen on ipv4:             Connect to ipv4:

Address         Port        Address         Port
--------------- ----------  --------------- ----------
0.0.0.0         18026       127.0.0.1       18025
172.17.131.129  443         127.0.0.1       443

Abhören an ipv6:            Verbinden mit ipv4:

Adresse         Port        Adresse         Port
--------------- ----------  --------------- ----------
::              8080        127.0.0.1       8080
`
	want := []Proxy{
		{Kind: "v4tov4", ListenAddr: "0.0.0.0", ListenPort: 18026, ConnectAddr: "127.0.0.1", ConnectPort: 18025},
		{Kind: "v4tov4", ListenAddr: "172.17.131.129", ListenPort: 443, ConnectAddr: "127.0.0.1", ConnectPort: 443},
		{Kind: "v6tov4", ListenAddr: "::", ListenPort: 8080, ConnectAddr: "127.0.0.1", ConnectPort: 8080},
	}
	got := ParsePortproxy(out)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParsePortproxy =\n%+v\nwant\n%+v", got, want)
	}
	if cmd := got[0].DeleteCommand(); cmd != "netsh interface portproxy delete v4tov4 listenaddress=0.0.0.0 listenport=18026" {
		t.Errorf("DeleteCommand = %q", cmd)
	}
}
