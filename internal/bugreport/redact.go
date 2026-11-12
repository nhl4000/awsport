// Package bugreport builds public awsport bug reports: it collects the
// environment, doctor results, the agent log tail and the latest crash log,
// redacts everything identifying, and submits the issue to GitHub through
// the REST API or a prefilled issue-form URL. It also writes crash logs.
package bugreport

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/doctor"
	"github.com/nhl4000/awsport/internal/state"
	"github.com/nhl4000/awsport/internal/tunnel"
)

// minNameLen is the shortest name that is mapped; shorter ones would
// rewrite unrelated words.
const minNameLen = 2

// keptNames are never mapped: they identify nothing and appear in
// awsport's own messages ("default" profile, "awsport" sso-session).
var keptNames = map[string]bool{"default": true, "awsport": true, "aws": true}

// Names collects the identifiers to replace in a report: AWS profile,
// resource (EC2 Name tags, RDS identifiers, groups, tunnel labels), role and
// sso-session names, and SSO start URLs. It is safe for concurrent use.
type Names struct {
	mu        sync.Mutex
	profiles  []string
	resources []string
	roles     []string
	sessions  []string
	startURLs []string
}

func (n *Names) add(list *[]string, names ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, s := range names {
		if s = strings.TrimSpace(s); s != "" {
			*list = append(*list, s)
		}
	}
}

// AddProfile records a profile's name, role and sso-session.
func (n *Names) AddProfile(p core.Profile) {
	n.add(&n.profiles, p.Name)
	n.add(&n.roles, p.RoleName)
	n.add(&n.sessions, p.SSOSession)
}

// AddProfileName records a profile name.
func (n *Names) AddProfileName(name string) { n.add(&n.profiles, name) }

// AddResource records a discovered resource's profile and names. Its ID is
// added too unless it is an EC2 instance ID (those are redacted by pattern).
func (n *Names) AddResource(r core.Resource) {
	n.add(&n.profiles, r.Profile)
	n.add(&n.resources, r.Name)
	if r.Kind == core.KindRDS {
		n.add(&n.resources, r.ID)
	}
}

// AddResourceName records an EC2 Name tag, RDS identifier, group name or
// tunnel label.
func (n *Names) AddResourceName(name string) { n.add(&n.resources, name) }

// AddSSO records an sso-session name and its start URL.
func (n *Names) AddSSO(session, startURL string) {
	n.add(&n.sessions, session)
	n.add(&n.startURLs, startURL)
}

// AddConfig records the SSO settings and groups of an awsport config.
func (n *Names) AddConfig(c *config.Config) {
	if c == nil {
		return
	}
	n.AddSSO(c.SSO.Session, c.SSO.StartURL)
	n.addGroups(c.Groups)
}

func (n *Names) addGroups(groups []core.Group) {
	for _, g := range groups {
		n.add(&n.resources, g.Name)
		for _, m := range g.Members {
			n.add(&n.profiles, m.Profile)
			n.add(&n.resources, m.Name)
		}
	}
}

// AddState records the profiles and resource names in saved ports, manual
// ports, bastions and groups.
func (n *Names) AddState(d state.Data) {
	for k := range d.Ports {
		// profile/region/name/remote; a Name tag may itself contain "/".
		seg := strings.Split(k, "/")
		if len(seg) >= 4 {
			n.add(&n.profiles, seg[0])
			n.add(&n.resources, strings.Join(seg[2:len(seg)-1], "/"))
		}
	}
	for k := range d.ManualPorts {
		if profile, rest, ok := strings.Cut(k, "/"); ok {
			n.add(&n.profiles, profile)
			if _, name, ok := strings.Cut(rest, "/"); ok {
				n.add(&n.resources, name)
			}
		}
	}
	for k := range d.Bastions {
		if profile, _, ok := strings.Cut(k, "/"); ok {
			n.add(&n.profiles, profile)
		}
	}
	n.addGroups(d.Groups)
}

// AddSession records a tunnel session's profile, label and resource.
func (n *Names) AddSession(s tunnel.Session) {
	n.add(&n.profiles, s.Spec.Profile)
	n.addLabel(s.Spec.Label)
	n.addKey(s.Spec.Key)
}

// addLabel records a tunnel label ("<resource name> <port name>"): the
// resource name, and the whole label when it has no port part.
func (n *Names) addLabel(label string) {
	if i := strings.LastIndexByte(label, ' '); i > 0 {
		label = label[:i]
	}
	n.add(&n.resources, label)
}

// addKey records the profile and RDS identifier of a tunnel key
// (profile/region/id/remote).
func (n *Names) addKey(key string) {
	seg := strings.Split(key, "/")
	if len(seg) != 4 {
		return
	}
	n.add(&n.profiles, seg[0])
	if !instanceIDRe.MatchString(seg[2]) {
		n.add(&n.resources, seg[2])
	}
}

// agentLogRe matches the agent's per-tunnel log lines:
// "2006/01/02 15:04:05 [start ]<label> (<profile>/<region>/<id>/<remote>)...".
var agentLogRe = regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d (?:start )?(.+?) \(([^()\s/]+/[^()\s/]+/[^()\s/]+/\d+)\)`)

// AddLogLines records the labels, profiles and RDS identifiers of agent
// log lines.
func (n *Names) AddLogLines(lines []string) {
	for _, l := range lines {
		if m := agentLogRe.FindStringSubmatch(l); m != nil {
			n.addLabel(m[1])
			n.addKey(m[2])
		}
	}
}

var (
	startURLRe = regexp.MustCompile(`https?://[A-Za-z0-9-]+\.awsapps\.(?:com|cn)/start(?:[/#?][^\s"'<>)\]]*)?`)
	awsappsRe  = regexp.MustCompile(`\b[A-Za-z0-9-]+\.awsapps\.(?:com|cn)\b`)
	// arnRe captures partition, service and region; account and resource
	// are dropped.
	arnRe        = regexp.MustCompile(`\barn:(aws(?:-[a-z]+)*):([a-z0-9-]*):([a-z0-9-]*):[^:\s]*:[^\s"'<>,;)\]}]+`)
	endpointRe   = regexp.MustCompile(`\b(?:[A-Za-z0-9-]+\.)+amazonaws\.com(?:\.cn)?\b`)
	ipHostRe     = regexp.MustCompile(`\bip-\d{1,3}(?:-\d{1,3}){3}(?:\.[A-Za-z0-9-]+)*\b`)
	instanceIDRe = regexp.MustCompile(`\bm?i-(?:[0-9a-f]{17}|[0-9a-f]{8})\b`)
	accountIDRe  = regexp.MustCompile(`\b\d{12}\b`)
	userPathRe   = regexp.MustCompile(`(?i)([/\\](?:Users|home)[/\\])[^/\\\s"'<>:]+`)
	// privateIPRe matches RFC 1918, CGNAT (100.64/10) and link-local IPv4
	// addresses not preceded by a digit or dot (version numbers). Loopback
	// is kept: it shows where tunnels listen.
	privateIPRe = regexp.MustCompile(`(^|[^\d.])(?:10\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])|192\.168|169\.254|100\.(?:6[4-9]|[7-9]\d|1[01]\d|12[0-7]))\.\d{1,3}\.\d{1,3}\b`)
	ghTokenRe   = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
)

// Placeholders that replace redacted values.
const (
	SSOPortal   = "https://<sso-portal>/start"
	AWSEndpoint = "<aws-endpoint>"
	InstanceID  = "i-<redacted>"
	AccountID   = "<account-id>"
	IP          = "<ip>"
)

// Redactor replaces identifying data in report text. Build it with
// Names.Redactor; the same name always maps to the same placeholder.
type Redactor struct {
	home      string
	startURLs []string // longest first
	names     []mapping
}

type mapping struct {
	name, repl string
}

// Redactor returns a redactor for the collected names. home is the user's
// home directory. Profiles map to profile-1..N, resources to
// resource-1..N, roles to role-1..N and sso-sessions to sso-session-1..N,
// numbered in the order they were first added. A name in several categories
// takes the first of profile, resource, role, sso-session.
func (n *Names) Redactor(home string) *Redactor {
	n.mu.Lock()
	defer n.mu.Unlock()
	r := &Redactor{home: home}
	seen := map[string]bool{}
	for _, cat := range []struct {
		prefix string
		names  []string
	}{
		{"profile", n.profiles},
		{"resource", n.resources},
		{"role", n.roles},
		{"sso-session", n.sessions},
	} {
		i := 0
		for _, name := range cat.names {
			key := strings.ToLower(name)
			if seen[key] || keptNames[key] || len(name) < minNameLen {
				continue
			}
			seen[key] = true
			i++
			r.names = append(r.names, mapping{name: name, repl: fmt.Sprintf("%s-%d", cat.prefix, i)})
		}
	}
	slices.SortStableFunc(r.names, func(a, b mapping) int { return cmp.Compare(len(b.name), len(a.name)) })
	for _, u := range n.startURLs {
		// The URL as configured and without its "/#/" tail.
		for _, v := range []string{u, strings.TrimRight(u, "/#")} {
			if v != "" && !slices.Contains(r.startURLs, v) {
				r.startURLs = append(r.startURLs, v)
			}
		}
	}
	slices.SortStableFunc(r.startURLs, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	return r
}

// Redact returns s with SSO start URLs, ARNs, AWS endpoints, instance IDs,
// account IDs, access key IDs, emails, IAM Identity Center directory IDs,
// the home directory and user names in paths, GitHub tokens, private IPv4
// addresses and every collected name replaced.
func (r *Redactor) Redact(s string) string {
	for _, u := range r.startURLs {
		s = strings.ReplaceAll(s, u, SSOPortal)
	}
	s = startURLRe.ReplaceAllString(s, SSOPortal)
	s = awsappsRe.ReplaceAllString(s, "<sso-portal>")
	s = arnRe.ReplaceAllString(s, "arn:$1:$2:$3:****:<redacted>")
	s = endpointRe.ReplaceAllString(s, AWSEndpoint)
	s = ipHostRe.ReplaceAllString(s, IP)
	s = instanceIDRe.ReplaceAllString(s, InstanceID)
	s = accountIDRe.ReplaceAllString(s, AccountID)
	s = ghTokenRe.ReplaceAllString(s, "<token>")
	s = doctor.Redact(s, r.home)
	s = r.replaceNames(s)
	s = userPathRe.ReplaceAllString(s, "${1}<user>")
	return privateIPRe.ReplaceAllString(s, "${1}"+IP)
}

// replaceNames replaces every collected name, case-insensitively and
// longest first, where it is not part of a longer word: a name that starts
// (ends) with a letter, digit or underscore must not be preceded (followed)
// by one. Hyphens and dots delimit, so an unknown "<profile>-app" still
// loses its profile part.
func (r *Redactor) replaceNames(s string) string {
	if len(r.names) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if m, ok := r.matchAt(s, i); ok {
			b.WriteString(s[last:i])
			b.WriteString(m.repl)
			i += len(m.name)
			last = i
			continue
		}
		i++
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func (r *Redactor) matchAt(s string, i int) (mapping, bool) {
	for _, m := range r.names {
		end := i + len(m.name)
		if end > len(s) || !strings.EqualFold(s[i:end], m.name) {
			continue
		}
		if isWord(m.name[0]) && i > 0 && isWord(s[i-1]) {
			continue
		}
		if isWord(m.name[len(m.name)-1]) && end < len(s) && isWord(s[end]) {
			continue
		}
		return m, true
	}
	return mapping{}, false
}

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
