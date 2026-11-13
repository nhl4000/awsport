// Package winports inspects the Windows side of a WSL machine: which
// programs listen on which TCP ports, and which `netsh interface portproxy`
// rules redirect ports. A Windows browser reaches WSL's localhost only
// through WSL's own forwarder (wslrelay.exe); any other Windows listener on
// the same port is reached instead, silently shadowing a WSL tunnel.
package winports

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// RelayProcess is WSL's localhost forwarder; its listeners are expected.
const RelayProcess = "wslrelay.exe"

// Listener is a TCP socket listening on Windows.
type Listener struct {
	Addr    string // e.g. "0.0.0.0", "127.0.0.1", "::"
	Port    int
	PID     int
	Process string // image name, e.g. "svchost.exe"; "" if unknown
}

// Proxy is one `netsh interface portproxy` rule.
type Proxy struct {
	Kind        string // "v4tov4", "v4tov6", "v6tov4" or "v6tov6"
	ListenAddr  string
	ListenPort  int
	ConnectAddr string
	ConnectPort int
}

// DeleteCommand is the admin PowerShell command that removes the rule.
func (p Proxy) DeleteCommand() string {
	return fmt.Sprintf("netsh interface portproxy delete %s listenaddress=%s listenport=%d", p.Kind, p.ListenAddr, p.ListenPort)
}

// commandTimeout bounds each Windows tool invocation.
const commandTimeout = 8 * time.Second

// Inspect runs netstat.exe, tasklist.exe and netsh.exe from system32 (the
// Windows System32 directory as seen from WSL, e.g. /mnt/c/Windows/System32)
// with the working directory cDrive (e.g. /mnt/c) so Windows tools don't
// warn about a WSL (UNC) working directory.
func Inspect(ctx context.Context, system32, cDrive string) ([]Listener, []Proxy, error) {
	netstat, err := run(ctx, system32, cDrive, "netstat.exe", "-ano", "-p", "TCP")
	if err != nil {
		return nil, nil, err
	}
	netstat6, _ := run(ctx, system32, cDrive, "netstat.exe", "-ano", "-p", "TCPv6") // IPv6 may be off
	listeners := append(ParseNetstat(netstat), ParseNetstat(netstat6)...)
	if tasks, err := run(ctx, system32, cDrive, "tasklist.exe", "/FO", "CSV", "/NH"); err == nil {
		names := ParseTasklist(tasks)
		for i := range listeners {
			listeners[i].Process = names[listeners[i].PID]
		}
	}
	proxyOut, err := run(ctx, system32, cDrive, "netsh.exe", "interface", "portproxy", "show", "all")
	if err != nil {
		return listeners, nil, err
	}
	return listeners, ParsePortproxy(proxyOut), nil
}

func run(ctx context.Context, system32, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, strings.TrimRight(system32, "/")+"/"+name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			return "", fmt.Errorf("can't run %s (is WSL interop enabled?): %w", name, err)
		}
		return "", fmt.Errorf("%s: %w %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return strings.ReplaceAll(string(out), "\r", ""), nil
}

// ParseNetstat extracts listening TCP sockets from `netstat -ano` output.
// A socket is listening when its foreign address is 0.0.0.0:0 / [::]:0;
// unlike the State column ("LISTENING"), that doesn't depend on the
// Windows display language.
func ParseNetstat(out string) []Listener {
	var ls []Listener
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		// Proto  Local Address  Foreign Address  State  PID
		if len(f) != 5 || !strings.HasPrefix(f[0], "TCP") || (f[2] != "0.0.0.0:0" && f[2] != "[::]:0") {
			continue
		}
		addr, port, ok := splitHostPort(f[1])
		pid, err := strconv.Atoi(f[4])
		if !ok || err != nil {
			continue
		}
		ls = append(ls, Listener{Addr: addr, Port: port, PID: pid})
	}
	return ls
}

// splitHostPort splits "0.0.0.0:80" and "[::]:80".
func splitHostPort(s string) (string, int, bool) {
	i := strings.LastIndexByte(s, ':')
	if i <= 0 {
		return "", 0, false
	}
	port, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	return strings.Trim(s[:i], "[]"), port, true
}

// ParseTasklist maps PIDs to image names from `tasklist /FO CSV /NH` output.
func ParseTasklist(out string) map[int]string {
	names := map[int]string{}
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	for {
		rec, err := r.Read()
		if err != nil {
			break
		}
		if len(rec) < 2 {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(rec[1])); err == nil {
			names[pid] = strings.TrimSpace(rec[0])
		}
	}
	return names
}

// ParsePortproxy extracts rules from `netsh interface portproxy show all`.
// The output has one table per kind, headed e.g. "Listen on ipv4: Connect to
// ipv6:" (translated on non-English Windows, but the ipv4/ipv6 tokens stay),
// with rows "<listen addr> <listen port> <connect addr> <connect port>".
func ParsePortproxy(out string) []Proxy {
	var rules []Proxy
	kind := ""
	for _, line := range strings.Split(out, "\n") {
		if fams := ipFamilies(line); len(fams) == 2 {
			kind = fams[0] + "to" + fams[1]
			continue
		}
		f := strings.Fields(line)
		if kind == "" || len(f) != 4 {
			continue
		}
		lp, err1 := strconv.Atoi(f[1])
		cp, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		rules = append(rules, Proxy{Kind: kind, ListenAddr: f[0], ListenPort: lp, ConnectAddr: f[2], ConnectPort: cp})
	}
	return rules
}

// ipFamilies returns "v4"/"v6" for each "ipv4"/"ipv6" token in line, in order.
func ipFamilies(line string) []string {
	var fams []string
	l := strings.ToLower(line)
	for {
		i := strings.Index(l, "ipv")
		if i < 0 || i+3 >= len(l) {
			return fams
		}
		switch l[i+3] {
		case '4':
			fams = append(fams, "v4")
		case '6':
			fams = append(fams, "v6")
		}
		l = l[i+4:]
	}
}
