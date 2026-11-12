package tui

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/open"
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
	return app.ConnString(spec) + tempMarker(spec)
}

// linkAddr is localAddr with the address as a clickable OSC 8 terminal
// hyperlink when the tunnel likely serves a web UI.
func linkAddr(spec core.TunnelSpec) string {
	addr := app.ConnString(spec)
	if u, ok := open.URL(spec); ok {
		addr = hyperlink(u, addr)
	}
	return addr + tempMarker(spec)
}

func tempMarker(spec core.TunnelSpec) string {
	if spec.Temporary {
		return " (temporary: saved port busy)"
	}
	return ""
}

// hyperlink wraps text in an OSC 8 hyperlink to target.
func hyperlink(target, text string) string {
	return "\x1b]8;;" + target + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

type tunnelAction int

const (
	actStart tunnelAction = iota
	actStop
	actRestart
)

// portAction is what enter does on a port: stop its active tunnel, or start
// one (replacing a failed or auth-expired session).
func portAction(s tunnel.Session, exists bool) tunnelAction {
	if exists && tunnel.IsActive(s.Status) {
		return actStop
	}
	return actStart
}

// dashboardAction is what enter does on a dashboard row: stop an active
// tunnel, restart any other.
func dashboardAction(s tunnel.Session) tunnelAction {
	if tunnel.IsActive(s.Status) {
		return actStop
	}
	return actRestart
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
	parts := []string{statusText(s), linkAddr(s.Spec)}
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

// flashLevel is the severity of the status-line message.
type flashLevel int

const (
	flashOK flashLevel = iota
	flashWarn
	flashError
)

// maxPreambleHeight bounds the context block above a screen's list.
const maxPreambleHeight = 8

// failureText is "failed: <reason>" for a failed session with a reason, else "".
func failureText(s tunnel.Session) string {
	if s.Status != tunnel.Failed || s.Err == "" {
		return ""
	}
	return "failed: " + s.Err
}

// permissionLines turns denied AWS calls into calm warnings, one per action
// (regions merged in order), e.g. "⚠ Can't list RDS databases in
// ca-central-1, us-east-1 — missing rds:DescribeDBInstances", followed by
// one admin hint per permission set.
func permissionLines(perms []*awsx.PermissionError) (warnings, hints []string) {
	type group struct {
		what, action string
		regions      []string
	}
	var groups []*group
	byKey := map[[2]string]*group{}
	for _, p := range perms {
		key := [2]string{p.Action, p.What}
		g := byKey[key]
		if g == nil {
			g = &group{what: p.What, action: p.Action}
			byKey[key] = g
			groups = append(groups, g)
		}
		if p.Region != "" && !slices.Contains(g.regions, p.Region) {
			g.regions = append(g.regions, p.Region)
		}
	}
	for _, g := range groups {
		w := "⚠ Can't " + g.what
		if len(g.regions) > 0 {
			w += " in " + strings.Join(g.regions, ", ")
		}
		warnings = append(warnings, w+" — missing "+g.action)
	}
	return warnings, awsx.Hints(perms)
}

// permissionNotice is permissionLines as one multi-line status message.
func permissionNotice(perms []*awsx.PermissionError) string {
	warnings, hints := permissionLines(perms)
	return strings.Join(append(warnings, hints...), "\n")
}

// missingActions is "missing a, b" for the distinct denied actions.
func missingActions(perms []*awsx.PermissionError) string {
	var actions []string
	for _, p := range perms {
		if !slices.Contains(actions, p.Action) {
			actions = append(actions, p.Action)
		}
	}
	return "missing " + strings.Join(actions, ", ")
}

// Status-line lifetimes: confirmations fade quickly, problems stay longer.
const (
	flashTTL    = 6 * time.Second
	flashErrTTL = 20 * time.Second
)

// flashExpired reports whether a status message of lvl shown at `at` should
// be cleared at now.
func flashExpired(lvl flashLevel, at, now time.Time) bool {
	ttl := flashTTL
	if lvl != flashOK {
		ttl = flashErrTTL
	}
	return now.Sub(at) >= ttl
}

// plural is "1 tunnel" / "2 tunnels".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// ssmDenied reports whether the SSM status call was denied.
func ssmDenied(perms []*awsx.PermissionError) bool {
	for _, p := range perms {
		if p.Action == "ssm:DescribeInstanceInformation" {
			return true
		}
	}
	return false
}

// activeByRef groups sessions that are not stopped by resource Ref.
func activeByRef(sessions []tunnel.Session) map[string][]tunnel.Session {
	out := map[string][]tunnel.Session{}
	for _, s := range sessions {
		if s.Status == tunnel.Stopped {
			continue
		}
		if ref, ok := tunnelRef(s.Spec); ok {
			out[ref] = append(out[ref], s)
		}
	}
	return out
}

// tunnelsNote summarizes a resource's tunnels for its list row, e.g.
// "● mailpit 127.0.0.1:18025" or "● 3 tunnels"; "" without tunnels.
func tunnelsNote(r core.Resource, sessions []tunnel.Session) string {
	switch len(sessions) {
	case 0:
		return ""
	case 1:
		s := sessions[0]
		what := statusText(s)
		if r.Kind == core.KindEC2 {
			what = portLabel(r, s.Spec.RemotePort) + " " + what
		}
		return statusGlyph(s.Status) + " " + what + " → " + app.ConnString(s.Spec)
	}
	glyph := statusGlyph(tunnel.Up)
	for _, s := range sessions {
		if s.Status != tunnel.Up {
			glyph = statusGlyph(s.Status)
		}
	}
	return glyph + " " + plural(len(sessions), "tunnel")
}

func portLabel(r core.Resource, remote int) string {
	for _, p := range r.Ports {
		if p.Remote == remote && p.Name != "" {
			return p.Name
		}
	}
	return strconv.Itoa(remote)
}

// bastionNamesNote renders the configured bastion_names for the bastion
// picker hint: "" when none are configured.
func bastionNamesNote(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return " or named " + strings.Join(names, ", ")
}

// profileTunnels counts active tunnels per profile.
func profileTunnels(sessions []tunnel.Session) map[string]int {
	out := map[string]int{}
	for _, s := range sessions {
		if tunnel.IsActive(s.Status) {
			out[s.Spec.Profile]++
		}
	}
	return out
}

// groupUp counts g's members with an active tunnel. Sessions don't carry
// the resource name, so a member matches by profile, region (when set),
// remote port and the label, which starts with the resource name.
func groupUp(g core.Group, sessions []tunnel.Session) int {
	n := 0
	for _, gm := range g.Members {
		for _, s := range sessions {
			if tunnel.IsActive(s.Status) && s.Spec.Profile == gm.Profile && s.Spec.RemotePort == gm.Remote &&
				(gm.Region == "" || s.Spec.Region == gm.Region) && strings.HasPrefix(s.Spec.Label, gm.Name+" ") {
				n++
				break
			}
		}
	}
	return n
}
