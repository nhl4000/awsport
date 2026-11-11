// Package open maps tunnels to browsable local URLs and opens them in the
// user's default browser.
package open

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/nhl4000/awsport/internal/core"
)

// nonHTTPPorts are well-known remote ports that never speak HTTP.
var nonHTTPPorts = map[int]bool{
	22: true, 25: true, 465: true, 587: true, 1025: true, 1433: true, 1521: true,
	3306: true, 5432: true, 5439: true, 6379: true, 11211: true, 27017: true,
}

// URL returns the local browser URL for a tunnel, if its remote port is
// likely to serve HTTP. Only EC2 tunnels (no RemoteHost) qualify: RDS
// endpoints are databases. Remote 443/8443 map to https, the rest to http.
func URL(spec core.TunnelSpec) (string, bool) {
	if spec.RemoteHost != "" || spec.LocalPort <= 0 || nonHTTPPorts[spec.RemotePort] {
		return "", false
	}
	scheme := "http"
	if spec.RemotePort == 443 || spec.RemotePort == 8443 {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.LocalPort)), true
}

// Browser opens url in the default browser without waiting for it.
func Browser(url string) error {
	name, args, err := browserCommand(runtime.GOOS, isWSL(), exec.LookPath, url)
	if err != nil {
		return err
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open %s: %w", url, err)
	}
	go func() { _ = cmd.Wait() }() // reap; the launcher's exit status is irrelevant
	return nil
}

// browserCommand picks the launcher for goos. On WSL it prefers wslview and
// falls back to cmd.exe, where & must be escaped as ^&.
func browserCommand(goos string, wsl bool, lookPath func(string) (string, error), url string) (string, []string, error) {
	switch goos {
	case "darwin":
		return "open", []string{url}, nil
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}, nil
	}
	if wsl {
		if p, err := lookPath("wslview"); err == nil {
			return p, []string{url}, nil
		}
		return "cmd.exe", []string{"/c", "start", "", strings.ReplaceAll(url, "&", "^&")}, nil
	}
	if goos == "linux" || strings.HasSuffix(goos, "bsd") {
		return "xdg-open", []string{url}, nil
	}
	return "", nil, errors.New("opening a browser is not supported on " + goos)
}

// isWSL reports whether the process runs under Windows Subsystem for Linux.
func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	b, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}
