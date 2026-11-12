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

// ErrNoDisplay means there is no graphical session to show a browser in
// (Linux/BSD without DISPLAY or WAYLAND_DISPLAY, e.g. over SSH). Launching
// xdg-open there may start a text browser inside the terminal.
var ErrNoDisplay = errors.New("no graphical display (DISPLAY is not set)")

// Browser opens url in the default browser without waiting for it.
func Browser(url string) error {
	l, err := defaultLauncher(url)
	if err != nil {
		return err
	}
	cmd := exec.Command(l.name, l.args...)
	cmd.Dir = l.dir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open %s: %w", url, err)
	}
	go func() { _ = cmd.Wait() }() // reap; the launcher's exit status is irrelevant
	return nil
}

// Check resolves the program Browser would start, without starting it, and
// returns its name or path. Errors are those Browser returns before launching
// (ErrNoDisplay, ErrNoWindowsInterop, unsupported OS).
func Check() (string, error) {
	l, err := defaultLauncher("")
	if err != nil {
		return "", err
	}
	if l.name == "xdg-open" {
		p, err := exec.LookPath(l.name)
		if err != nil {
			return "", fmt.Errorf("xdg-open not found: %w", err)
		}
		return p, nil
	}
	return l.name, nil
}

func defaultLauncher(url string) (launcher, error) {
	display := os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	return browserCommand(runtime.GOOS, isWSL(), display, exec.LookPath, fileExists, wslMountRoot(), url)
}

// ErrNoWindowsInterop means awsport runs under WSL but cannot reach a Windows
// program to open the browser (interop disabled or the C: drive not mounted).
var ErrNoWindowsInterop = errors.New("cannot reach Windows from WSL to open a browser (install wslu for wslview, or enable WSL interop)")

type launcher struct {
	name string
	args []string
	dir  string // working directory; empty = inherit
}

// browserCommand picks the launcher for goos. On WSL it prefers wslview, then
// Windows' rundll32 (which hands the URL to the default browser without
// cmd.exe quoting rules), found on PATH or under the WSL mount root since
// many setups don't append Windows directories to PATH. Windows programs run
// from the C: mount to avoid cmd/UNC working-directory warnings. display
// reports a graphical session on Linux/BSD outside WSL.
func browserCommand(goos string, wsl, display bool, lookPath func(string) (string, error), exists func(string) bool, mountRoot, url string) (launcher, error) {
	switch goos {
	case "darwin":
		return launcher{name: "open", args: []string{url}}, nil
	case "windows":
		return launcher{name: "rundll32", args: []string{"url.dll,FileProtocolHandler", url}}, nil
	}
	if wsl {
		if p, err := lookPath("wslview"); err == nil {
			return launcher{name: p, args: []string{url}}, nil
		}
		cDrive := strings.TrimRight(mountRoot, "/") + "/c"
		rundll := ""
		if p, err := lookPath("rundll32.exe"); err == nil {
			rundll = p
		} else if p := cDrive + "/Windows/System32/rundll32.exe"; exists(p) {
			rundll = p
		}
		if rundll == "" {
			return launcher{}, ErrNoWindowsInterop
		}
		dir := ""
		if exists(cDrive) {
			dir = cDrive
		}
		return launcher{name: rundll, args: []string{"url.dll,FileProtocolHandler", url}, dir: dir}, nil
	}
	if goos == "linux" || strings.HasSuffix(goos, "bsd") {
		if !display {
			return launcher{}, ErrNoDisplay
		}
		return launcher{name: "xdg-open", args: []string{url}}, nil
	}
	return launcher{}, errors.New("opening a browser is not supported on " + goos)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// wslMountRoot returns the [automount] root from /etc/wsl.conf (default "/mnt/").
func wslMountRoot() string {
	b, err := os.ReadFile("/etc/wsl.conf")
	if err != nil {
		return "/mnt/"
	}
	return parseMountRoot(string(b))
}

func parseMountRoot(conf string) string {
	section := ""
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[] "))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && section == "automount" && strings.TrimSpace(strings.ToLower(k)) == "root" {
			if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
				return v
			}
		}
	}
	return "/mnt/"
}

// isWSL reports whether the process runs under Windows Subsystem for Linux.
func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	b, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}
