package doctor

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/nhl4000/awsport/internal/tunnel"
	"github.com/nhl4000/awsport/internal/winports"
)

// awsportPorts maps each local port awsport uses (remembered in state, or
// held by a running tunnel) to the labels of the tunnels behind it.
func awsportPorts(saved map[string]int, sessions []tunnel.Session) map[int][]string {
	ports := map[int][]string{}
	add := func(port int, label string) {
		if port > 0 && !slices.Contains(ports[port], label) {
			ports[port] = append(ports[port], label)
		}
	}
	for key, port := range saved {
		add(port, portKeyLabel(key))
	}
	for _, s := range sessions {
		if tunnel.IsActive(s.Status) {
			add(s.Spec.LocalPort, s.Spec.Label+" ("+s.Spec.Profile+")")
		}
	}
	return ports
}

// portKeyLabel turns a saved-port key "profile/region/name/remote" into
// "name:remote (profile)".
func portKeyLabel(key string) string {
	f := strings.Split(key, "/")
	if len(f) != 4 {
		return key
	}
	return f[2] + ":" + f[3] + " (" + f[0] + ")"
}

// windowsPortChecks reports, for WSL, Windows listeners and portproxy rules
// on awsport's local ports. A browser on Windows reaches a WSL tunnel only
// through WSL's forwarder (wslrelay.exe); anything else listening on the
// same Windows port is reached instead, so the tunnel silently shows the
// wrong service.
func windowsPortChecks(ports map[int][]string, listeners []winports.Listener, proxies []winports.Proxy, err error) []Check {
	if err != nil {
		return []Check{{Status: Warn, Title: "couldn't inspect Windows ports", Detail: shortErr(err),
			Fix: "check that WSL can run Windows programs (try: cmd.exe /c ver), then rerun awsport doctor"}}
	}
	if len(ports) == 0 {
		return []Check{{Status: OK, Title: "no awsport ports to check yet (none saved)"}}
	}
	nums := make([]int, 0, len(ports))
	for p := range ports {
		nums = append(nums, p)
	}
	slices.Sort(nums)

	var out []Check
	for _, port := range nums {
		who := strings.Join(ports[port], ", ")
		proxied := false
		for _, r := range proxies {
			if r.ListenPort != port {
				continue
			}
			proxied = true
			out = append(out, Check{Status: Fail,
				Title: fmt.Sprintf("Windows forwards port %d to %s (netsh portproxy rule), so browsers on Windows don't reach awsport's tunnel for %s",
					port, net.JoinHostPort(r.ConnectAddr, strconv.Itoa(r.ConnectPort)), who),
				Fix: "in an administrator PowerShell: " + r.DeleteCommand()})
		}
		seen := map[int]bool{}
		for _, l := range listeners {
			if l.Port != port || seen[l.PID] || strings.EqualFold(l.Process, winports.RelayProcess) {
				continue
			}
			// portproxy rules are served by svchost (IP Helper); already reported above.
			if proxied && strings.EqualFold(l.Process, "svchost.exe") {
				continue
			}
			seen[l.PID] = true
			proc := l.Process
			if proc == "" {
				proc = "a Windows program"
			}
			out = append(out, Check{Status: Warn,
				Title: fmt.Sprintf("%s (PID %d) listens on Windows port %d; browsers on Windows reach it instead of awsport's tunnel for %s",
					proc, l.PID, port, who),
				Fix: fmt.Sprintf("stop that program or move it off port %d (in PowerShell: Get-Process -Id %d)", port, l.PID)})
		}
	}
	if len(out) == 0 {
		return []Check{{Status: OK, Title: fmt.Sprintf("no Windows program or port forwarding rule uses awsport's %d port(s)", len(nums))}}
	}
	return out
}
