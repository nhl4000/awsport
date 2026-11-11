package tui

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/tunnel"
)

// defaultSSOSession is the sso-session name used when config leaves it empty.
const defaultSSOSession = "awsport"

// visibleResources returns the resources to list: connectable ones always,
// unreachable ones only when showAll is set. Order is preserved.
func visibleResources(rs []core.Resource, showAll bool) []core.Resource {
	out := make([]core.Resource, 0, len(rs))
	for _, r := range rs {
		if r.Unreachable == "" || showAll {
			out = append(out, r)
		}
	}
	return out
}

// hiddenCount reports how many resources visibleResources would hide.
func hiddenCount(rs []core.Resource) int {
	n := 0
	for _, r := range rs {
		if r.Unreachable != "" {
			n++
		}
	}
	return n
}

// parsePortSpec parses "name:port" or a bare "port".
func parsePortSpec(s string) (core.PortSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return core.PortSpec{}, errors.New("enter a port, e.g. 8080 or app:8080")
	}
	name, portText := "", s
	if i := strings.LastIndex(s, ":"); i >= 0 {
		name, portText = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
		if name == "" {
			return core.PortSpec{}, fmt.Errorf("invalid port %q: name before ':' is empty", s)
		}
		if strings.ContainsAny(name, " \t,/") {
			return core.PortSpec{}, fmt.Errorf("invalid port name %q: no spaces, commas or slashes", name)
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return core.PortSpec{}, fmt.Errorf("invalid port %q: not a number", portText)
	}
	if port < 1 || port > 65535 {
		return core.PortSpec{}, fmt.Errorf("invalid port %d: must be 1-65535", port)
	}
	return core.PortSpec{Name: name, Remote: port}, nil
}

// statusText is the human status of a tunnel session.
func statusText(s tunnel.Session) string {
	switch s.Status {
	case tunnel.Reconnecting:
		if s.Attempts > 0 {
			return fmt.Sprintf("reconnecting (attempt %d)", s.Attempts)
		}
	case tunnel.AuthExpired:
		return "auth expired (press L)"
	case tunnel.Failed:
		if s.Err != "" {
			return "failed: " + s.Err
		}
	}
	return string(s.Status)
}

// statusGlyph is a one-rune status indicator.
func statusGlyph(st tunnel.Status) string {
	switch st {
	case tunnel.Up:
		return "●"
	case tunnel.Starting, tunnel.Reconnecting:
		return "◌"
	case tunnel.AuthExpired, tunnel.Failed:
		return "✗"
	default:
		return "○"
	}
}

// lastLogLine returns the most recent non-blank log line, whitespace-collapsed.
func lastLogLine(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.Join(strings.Fields(lines[i]), " "); l != "" {
			return l
		}
	}
	return ""
}

// localAddr is the connection string plus a temporary marker.
func localAddr(spec core.TunnelSpec) string {
	addr := app.ConnString(spec)
	if spec.Temporary {
		addr += " (temporary: saved port busy)"
	}
	return addr
}

// humanDuration renders d compactly: 45s, 12m, 3h05m.
func humanDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}

// sessionDetail is the dashboard description line for a session.
func sessionDetail(s tunnel.Session, now time.Time) string {
	parts := []string{statusText(s), localAddr(s.Spec)}
	if !s.Since.IsZero() {
		parts = append(parts, humanDuration(now.Sub(s.Since)))
	}
	if l := lastLogLine(s.LastLog); l != "" {
		parts = append(parts, l)
	}
	return strings.Join(parts, " · ")
}

// authExpiredProfiles lists (sorted, unique) profiles with auth-expired sessions.
func authExpiredProfiles(sessions []tunnel.Session) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sessions {
		if s.Status == tunnel.AuthExpired && !seen[s.Spec.Profile] {
			seen[s.Spec.Profile] = true
			out = append(out, s.Spec.Profile)
		}
	}
	sort.Strings(out)
	return out
}

// tunnelRef extracts the resource Ref from a spec key
// (Key = Resource.TunnelKey(remote) = "<profile>/<region>/<id>/<remote>").
func tunnelRef(spec core.TunnelSpec) (string, bool) {
	prefix := spec.Profile + "/" + spec.Region + "/"
	suffix := "/" + strconv.Itoa(spec.RemotePort)
	k := spec.Key
	if len(k) <= len(prefix)+len(suffix) || !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, suffix) {
		return "", false
	}
	return k[:len(k)-len(suffix)], true
}

// buildGroup turns tunnel specs into a named group, resolving each spec's
// resource (for its stable Name and Kind) via lookup. Specs that can't be
// resolved are returned in unresolved (by key) and left out of the group.
func buildGroup(name string, specs []core.TunnelSpec, lookup func(ref string) (core.Resource, bool)) (core.Group, []string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return core.Group{}, nil, errors.New("group name is empty")
	}
	if len(specs) == 0 {
		return core.Group{}, nil, errors.New("no tunnels selected")
	}
	g := core.Group{Name: name}
	seen := map[core.GroupMember]bool{}
	var unresolved []string
	for _, spec := range specs {
		ref, ok := tunnelRef(spec)
		var r core.Resource
		if ok {
			r, ok = lookup(ref)
		}
		if !ok {
			unresolved = append(unresolved, spec.Key)
			continue
		}
		memberName := r.Name
		if memberName == "" {
			memberName = r.ID
		}
		gm := core.GroupMember{Profile: spec.Profile, Region: spec.Region, Name: memberName, Kind: r.Kind, Remote: spec.RemotePort}
		if seen[gm] {
			continue
		}
		seen[gm] = true
		g.Members = append(g.Members, gm)
	}
	if len(g.Members) == 0 {
		return core.Group{}, unresolved, errors.New("none of the selected tunnels could be resolved to a resource")
	}
	return g, unresolved, nil
}

// groupSummary describes a group's members on one line.
func groupSummary(g core.Group) string {
	parts := make([]string, 0, len(g.Members))
	for _, gm := range g.Members {
		p := gm.Profile + "/" + gm.Name + ":" + strconv.Itoa(gm.Remote)
		if gm.Region != "" {
			p = gm.Profile + "/" + gm.Region + "/" + gm.Name + ":" + strconv.Itoa(gm.Remote)
		}
		parts = append(parts, p)
	}
	return fmt.Sprintf("%d tunnel(s): %s", len(g.Members), strings.Join(parts, ", "))
}

// loginPlan decides how to re-authenticate the named profiles: one
// `aws sso login --sso-session` per distinct session, `--profile` logins for
// SSO profiles without a session, and the rest are unknown (cannot log in).
func loginPlan(names []string, all []core.Profile) (sessions, profiles, unknown []string) {
	byName := make(map[string]core.Profile, len(all))
	for _, p := range all {
		byName[p.Name] = p
	}
	seenS, seenP := map[string]bool{}, map[string]bool{}
	for _, n := range names {
		p, ok := byName[n]
		switch {
		case ok && p.SSOSession != "":
			if !seenS[p.SSOSession] {
				seenS[p.SSOSession] = true
				sessions = append(sessions, p.SSOSession)
			}
		case ok && p.Kind == core.AuthSSO:
			if !seenP[n] {
				seenP[n] = true
				profiles = append(profiles, n)
			}
		default:
			unknown = append(unknown, n)
		}
	}
	return sessions, profiles, unknown
}

// kindBadge is a short label for a profile's auth kind.
func kindBadge(k core.AuthKind) string {
	switch k {
	case core.AuthSSO:
		return "SSO"
	case core.AuthAssumeRole:
		return "ROLE"
	case core.AuthStatic:
		return "KEYS"
	default:
		return "?"
	}
}

// depsOK reports whether every prerequisite check passed.
func depsOK(checks []deps.Check) bool {
	for _, c := range checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// firstFailing returns the first failing check, if any.
func firstFailing(checks []deps.Check) (deps.Check, bool) {
	for _, c := range checks {
		if !c.OK {
			return c, true
		}
	}
	return deps.Check{}, false
}

// failingNames lists the names of failing checks.
func failingNames(checks []deps.Check) []string {
	var out []string
	for _, c := range checks {
		if !c.OK {
			out = append(out, c.Name)
		}
	}
	return out
}

// normalizeSSO trims fields and fills the default session name.
func normalizeSSO(s config.SSO) config.SSO {
	s.StartURL = strings.TrimSpace(s.StartURL)
	s.Region = strings.TrimSpace(s.Region)
	s.Session = strings.TrimSpace(s.Session)
	if s.Session == "" {
		s.Session = defaultSSOSession
	}
	return s
}

var regionRE = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`)

// validateSSO checks a manually entered SSO configuration.
func validateSSO(s config.SSO) error {
	u, err := url.Parse(s.StartURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("start URL must be an https URL, e.g. https://d-0000000000.awsapps.com/start")
	}
	if !regionRE.MatchString(s.Region) {
		return fmt.Errorf("invalid SSO region %q, e.g. us-east-1", s.Region)
	}
	if strings.ContainsAny(s.Session, " \t[]") {
		return fmt.Errorf("invalid session name %q: no spaces or brackets", s.Session)
	}
	return nil
}
