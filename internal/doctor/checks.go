package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nhl4000/awsport/internal/agent"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/open"
	"github.com/nhl4000/awsport/internal/tunnel"
)

// tokenWarnWindow: an SSO sign-in expiring sooner than this is a warning.
const tokenWarnWindow = time.Hour

// maxErrLen bounds error text quoted in a check.
const maxErrLen = 300

// teamConfigExample is the documented form of a team config source.
const teamConfigExample = "github:<org>/<repo>/<path>"

// ---- awsport ----

// versionChecks reports the running version and, when the latest release
// is known (latest != ""), whether it is newer.
func versionChecks(version, goos, goarch, latest string) []Check {
	c := Check{Status: OK, Title: fmt.Sprintf("awsport %s (%s/%s)", version, goos, goarch)}
	if latest == "" {
		return []Check{c}
	}
	cur, ok := parseVersion(version)
	if !ok {
		c.Detail = "development build; the latest release is " + latest
		return []Check{c}
	}
	rel, ok := parseVersion(latest)
	if !ok || compareVersions(rel, cur) <= 0 {
		c.Title += ", the latest release"
		return []Check{c}
	}
	return []Check{c, {
		Status: Warn,
		Title:  fmt.Sprintf("awsport %s is available (this is %s)", latest, version),
		Fix:    "awsport update",
	}}
}

// parseVersion parses "v1.2.3" / "1.2.3-rc.1" into numeric components; the
// pre-release suffix is ignored.
func parseVersion(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "-")
	s, _, _ = strings.Cut(s, "+")
	if s == "" {
		return nil, false
	}
	var out []int
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func compareVersions(a, b []int) int {
	for i := range max(len(a), len(b)) {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ---- tools ----

// toolChecks maps prerequisite detection results to checks; a missing or
// unusable tool fails with its official install command as the fix.
func toolChecks(results []deps.Check) []Check {
	out := make([]Check, 0, len(results))
	for _, r := range results {
		name := r.Name
		if name == "aws" {
			name = "AWS CLI"
		}
		if r.OK {
			out = append(out, Check{Status: OK, Title: name + " " + r.Version, Detail: awsx.DisplayPath(r.Path)})
			continue
		}
		c := Check{Status: Fail, Title: r.Problem, Detail: r.Install.Description}
		if c.Title == "" {
			c.Title = name + " is not usable"
		}
		switch {
		case len(r.Install.Commands) > 0:
			steps := make([]string, len(r.Install.Commands))
			for i, argv := range r.Install.Commands {
				steps[i] = shellJoin(argv)
			}
			c.Fix = strings.Join(steps, " && ")
			if r.Install.DocsURL != "" {
				c.Detail = strings.TrimSpace(c.Detail + "\nguide: " + r.Install.DocsURL)
			}
		case r.Install.DocsURL != "":
			c.Fix = "install it following " + r.Install.DocsURL
		default:
			c.Fix = "install " + r.Name
		}
		out = append(out, c)
	}
	return out
}

// shellJoin renders argv as a copy-pasteable command line.
func shellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\"'$&|;<>()*?`\\") {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

// ---- files ----

// configFileCheck reports whether the AWS config file can be read.
func configFileCheck(path string, fromEnv bool) Check {
	disp := awsx.DisplayPath(path)
	var detail string
	if fromEnv {
		detail = "set by AWS_CONFIG_FILE"
	}
	_, err := os.ReadFile(path)
	switch {
	case err == nil:
		return Check{Status: OK, Title: disp + " is readable", Detail: detail}
	case errors.Is(err, fs.ErrNotExist):
		return Check{Status: Warn, Title: "no AWS config file at " + disp + " yet", Detail: detail,
			Fix: "awsport login (writes one profile per account and role into it)"}
	}
	return Check{Status: Fail, Title: "can't read " + disp, Detail: joinLines(detail, shortErr(err)),
		Fix: "make it readable by your user: sudo chown $(whoami) " + disp}
}

// probeWritable checks that dir can be written by creating and removing a
// temp file in it. A missing dir is probed at its nearest existing ancestor
// (where it would be created); probed is the directory actually tested.
func probeWritable(dir string) (probed string, err error) {
	probed = filepath.Clean(dir)
	for {
		fi, err := os.Stat(probed)
		if err == nil {
			if !fi.IsDir() {
				return probed, fmt.Errorf("%s is not a directory", probed)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return probed, err
		}
		parent := filepath.Dir(probed)
		if parent == probed {
			return probed, err
		}
		probed = parent
	}
	f, err := os.CreateTemp(probed, ".awsport-doctor-*")
	if err != nil {
		return probed, err
	}
	name := f.Name()
	cerr := f.Close()
	rerr := os.Remove(name)
	return probed, errors.Join(cerr, rerr)
}

// writableCheck reports whether dir is writable. fix builds the fix for a
// failure (where names the directory, possibly with more context).
func writableCheck(dir string, fix func(dir string) (where, fix string)) Check {
	disp := awsx.DisplayPath(dir)
	probed, err := probeWritable(dir)
	if err == nil {
		if filepath.Clean(probed) != filepath.Clean(dir) {
			return Check{Status: OK, Title: fmt.Sprintf("%s can be created (%s is writable)", disp, awsx.DisplayPath(probed))}
		}
		return Check{Status: OK, Title: disp + " is writable"}
	}
	where, f := fix(probed)
	return Check{Status: Fail, Title: "can't write " + awsx.DisplayPath(where), Detail: shortErr(err), Fix: f}
}

// awsportDirFix is the fix for an unwritable awsport config or state dir.
func awsportDirFix(env string) func(string) (string, string) {
	return func(dir string) (string, string) {
		return dir, fmt.Sprintf("make it yours: sudo chown -R $(whoami) %s (or point %s at a writable directory)",
			awsx.DisplayPath(dir), env)
	}
}

// ---- SSO ----

// ssoInputs is what the SSO checks look at.
type ssoInputs struct {
	cfg         *config.Config
	cfgErr      error
	cfgPath     string // config.yaml, for messages
	awsSession  string // first [sso-session] in the AWS config file, "" if none
	nonSSO      int    // profiles that work without SSO (assume-role, static keys)
	cacheDir    string // ~/.aws/sso/cache
	now         time.Time
	writeAdvice func(string) (string, string)
}

// ssoConfigured reports whether cfg has the SSO start URL and region.
func ssoConfigured(cfg *config.Config) bool {
	return cfg != nil && strings.TrimSpace(cfg.SSO.StartURL) != "" && strings.TrimSpace(cfg.SSO.Region) != ""
}

func ssoChecks(in ssoInputs) []Check {
	if in.cfgErr != nil {
		return []Check{{Status: Fail, Title: "can't load the awsport config", Detail: shortErr(in.cfgErr),
			Fix: "fix or remove the file named above (" + awsx.DisplayPath(in.cfgPath) + " or the team.yaml next to it), then rerun awsport doctor"}}
	}
	var out []Check
	team := strings.TrimSpace(in.cfg.TeamSource)
	if !ssoConfigured(in.cfg) {
		c := Check{Status: Fail, Title: "SSO is not configured (no start URL and region in the awsport config)"}
		if in.nonSSO > 0 {
			// Assume-role / static-key profiles work without it; SSO only adds profile sync.
			c.Status = Warn
			c.Title = fmt.Sprintf("SSO is not configured; your %d non-SSO profile(s) work without it, but awsport can't sync SSO accounts and roles", in.nonSSO)
		}
		switch {
		case team != "":
			c.Detail = "the team config imported from " + team + " has no sso section"
			c.Fix = "ask the maintainer of the team config to add sso.start_url and sso.region, then: awsport init --from " + team
		case in.awsSession != "":
			c.Fix = fmt.Sprintf("awsport init (adopts [sso-session %s] from %s)", in.awsSession, awsx.DisplayPath(awsx.ConfigFilePath()))
		default:
			c.Fix = "awsport init --from " + teamConfigExample + " (ask your team for it)\n" +
				"or: awsport init --start-url https://<directory>.awsapps.com/start --region <sso-region>"
		}
		return []Check{c}
	}
	sso := in.cfg.SSO
	session := sso.Session
	if session == "" {
		session = config.DefaultSSOSession
	}
	out = append(out, Check{Status: OK, Title: fmt.Sprintf("SSO portal %s (%s), sso-session %q", sso.StartURL, sso.Region, session)})
	if team != "" {
		out = append(out, Check{Status: OK, Title: "team config from " + team, Detail: "pull changes with: awsport init --refresh"})
	}
	return append(out, tokenCheck(in))
}

// tokenCheck reports the cached SSO sign-in for the configured start URL.
func tokenCheck(in ssoInputs) Check {
	exp, err := awsx.SSOTokenExpiry(in.cacheDir, in.cfg.SSO.StartURL, in.now)
	switch {
	case err != nil && !errors.Is(err, awsx.ErrSSOTokenExpired):
		where, fix := in.writeAdvice(in.cacheDir)
		return Check{Status: Fail, Title: "can't read the SSO token cache " + awsx.DisplayPath(where), Detail: shortErr(err), Fix: fix}
	case exp.IsZero():
		return Check{Status: Fail, Title: "not signed in to SSO (no cached token)", Fix: "awsport login"}
	case !exp.After(in.now):
		return Check{Status: Fail, Title: fmt.Sprintf("SSO sign-in expired %s ago (%s)", humanDuration(in.now.Sub(exp)), clock(exp, in.now)),
			Fix: "awsport login"}
	case err != nil || exp.Sub(in.now) < tokenWarnWindow:
		return Check{Status: Warn, Title: fmt.Sprintf("SSO sign-in expires in %s (%s)", humanDuration(exp.Sub(in.now)), clock(exp, in.now)),
			Fix: "awsport login (renew now so tunnels don't pause for a re-login)"}
	}
	return Check{Status: OK, Title: fmt.Sprintf("signed in to SSO; expires in %s (%s)", humanDuration(exp.Sub(in.now)), clock(exp, in.now))}
}

// humanDuration renders d as "7h12m", "42m" or "<1m".
func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// clock renders t in now's location: "at 15:04 UTC" today, else
// "Jan 2 15:04 UTC".
func clock(t, now time.Time) string {
	t = t.In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return "at " + t.Format("15:04 MST")
	}
	return t.Format("Jan 2 15:04 MST")
}

// ---- browser ----

func browserCheck(launcher string, err error) Check {
	if err == nil {
		return Check{Status: OK, Title: "browser opens via " + awsx.DisplayPath(launcher)}
	}
	c := Check{Status: Warn, Title: "can't open a browser: " + err.Error(),
		Detail: "awsport login still works: open the sign-in link it prints in any browser"}
	switch {
	case errors.Is(err, open.ErrNoDisplay):
		c.Title = "can't open a browser: no graphical display"
		c.Fix = "open the link `awsport login` prints yourself (or run it in a desktop session)"
	case errors.Is(err, open.ErrNoWindowsInterop):
		c.Title = "can't reach Windows from WSL to open a browser"
		c.Fix = "sudo apt install wslu (provides wslview), or enable WSL interop; until then open the printed link yourself"
	default:
		c.Fix = "open the link `awsport login` prints yourself"
	}
	return c
}

// ---- profiles ----

// startSessionNote explains the one permission discovery can't verify.
const startSessionNote = "ssm:StartSession (tunnels and shells) can only be verified by opening a tunnel: awsport up <profile>/<name>"

// profileSummary counts profiles by kind and reports requested profiles
// that don't exist. It returns the profiles to check.
func profileSummary(all []core.Profile, err error, requested []string, ssoReady bool) ([]Check, []core.Profile) {
	cfgPath := awsx.DisplayPath(awsx.ConfigFilePath())
	if err != nil {
		return []Check{{Status: Fail, Title: "can't read profiles from " + cfgPath, Detail: shortErr(err),
			Fix: "fix the syntax error in " + cfgPath + " (awsport only edits its own managed block)"}}, nil
	}
	if len(all) == 0 {
		fix := "awsport login (writes one profile per account and role)"
		if !ssoReady {
			fix = "set up SSO first (see the SSO section), then: awsport login"
		}
		return []Check{{Status: Fail, Title: "no AWS profiles in " + cfgPath, Fix: fix}}, nil
	}
	kinds := map[core.AuthKind]int{}
	managed := 0
	for _, p := range all {
		kinds[p.Kind]++
		if p.Managed {
			managed++
		}
	}
	var parts []string
	for _, k := range []core.AuthKind{core.AuthSSO, core.AuthAssumeRole, core.AuthStatic, core.AuthUnknown} {
		if n := kinds[k]; n > 0 {
			s := fmt.Sprintf("%d %s", n, k)
			if k == core.AuthSSO && managed > 0 {
				s += fmt.Sprintf(" (%d synced by awsport)", managed)
			}
			parts = append(parts, s)
		}
	}
	out := []Check{{Status: OK, Title: plural(len(all), "profile", "profiles") + ": " + strings.Join(parts, ", "),
		Detail: startSessionNote}}
	if len(requested) == 0 {
		return out, all
	}
	var selected []core.Profile
	for _, name := range requested {
		i := slices.IndexFunc(all, func(p core.Profile) bool { return p.Name == name })
		if i < 0 {
			out = append(out, Check{Status: Fail, Title: fmt.Sprintf("profile %q not found in %s", name, cfgPath),
				Fix: "check the name with `awsport ls`, or run `awsport login` to sync SSO profiles"})
			continue
		}
		selected = append(selected, all[i])
	}
	return out, selected
}

// profileTarget is one profile check: the profile, the region discovery
// runs in ("" when none is known) and the awsport SSO session name.
type profileTarget struct {
	profile    core.Profile
	region     string
	ssoSession string
}

// profileSection checks credentials and then permissions of one profile.
// check and discover are the AWS calls; ctx bounds both.
func profileSection(ctx context.Context, t profileTarget,
	creds func(context.Context, core.Profile) error,
	discover func(context.Context, core.Profile, string) ([]core.Resource, error)) Section {
	p := t.profile
	name := "Profile " + p.Name + " (" + string(p.Kind)
	if t.region != "" {
		name += ", " + t.region
	}
	sec := Section{Name: name + ")"}
	if err := creds(ctx, p); err != nil {
		sec.Checks = append(sec.Checks, credentialCheck(ctx, t, err))
		return sec
	}
	sec.Checks = append(sec.Checks, Check{Status: OK, Title: "credentials valid"})
	if t.region == "" {
		sec.Checks = append(sec.Checks, Check{Status: Warn, Title: "no region set; permissions not checked",
			Fix: fmt.Sprintf("aws configure set region <region> --profile %s (or add regions to the awsport config)", p.Name)})
		return sec
	}
	resources, err := discover(ctx, p, t.region)
	sec.Checks = append(sec.Checks, discoveryChecks(ctx, t, resources, err)...)
	return sec
}

// discoveryChecks maps a discovery outcome to checks: one warning for
// missing IAM permissions (with the admin hints as fix), a failure for any
// other error, and the resource counts when everything succeeded.
func discoveryChecks(ctx context.Context, t profileTarget, resources []core.Resource, err error) []Check {
	var out []Check
	if perms := awsx.PermissionErrors(err); len(perms) > 0 {
		out = append(out, permissionCheck(perms))
	}
	if rest := awsx.WithoutPermissionErrors(err); rest != nil {
		if awsx.IsAuthError(rest) {
			return append(out, credentialCheck(ctx, t, rest))
		}
		title := "discovery in " + t.region + " failed"
		if ctx.Err() != nil {
			title = "discovery in " + t.region + " timed out"
		}
		return append(out, Check{Status: Fail, Title: title, Detail: shortErr(rest),
			Fix: "check network access to AWS from this machine (proxy, VPN, firewall), then: awsport doctor --profile " + t.profile.Name})
	}
	if err != nil {
		return out
	}
	return append(out, Check{Status: OK, Title: resourceCounts(resources, t.region)})
}

// permissionCheck turns denied API calls into one warning listing each
// call, with one admin hint per permission set as the fix.
func permissionCheck(perms []*awsx.PermissionError) Check {
	descs := make([]string, len(perms))
	for i, p := range perms {
		descs[i] = "Can't " + p.What
		if p.Region != "" {
			descs[i] += " in " + p.Region
		}
		descs[i] += " — missing " + p.Action
	}
	c := Check{Status: Warn, Fix: strings.Join(awsx.Hints(perms), "\n")}
	if len(descs) == 1 {
		c.Title = descs[0]
	} else {
		c.Title = fmt.Sprintf("%d IAM permissions missing", len(descs))
		c.Detail = strings.Join(descs, "\n")
	}
	return c
}

func resourceCounts(rs []core.Resource, region string) string {
	var ec2, reachable, rds int
	for _, r := range rs {
		switch r.Kind {
		case core.KindEC2:
			ec2++
			if r.Unreachable == "" {
				reachable++
			}
		case core.KindRDS:
			rds++
		}
	}
	return fmt.Sprintf("permissions ok: %s (%d reachable via SSM), %s in %s",
		plural(ec2, "EC2 instance", "EC2 instances"), reachable, plural(rds, "database", "databases"), region)
}

// credentialCheck is the failure for a profile whose credentials could not
// be verified, with a fix matching how the profile gets credentials.
func credentialCheck(ctx context.Context, t profileTarget, err error) Check {
	p := t.profile
	cfgPath := awsx.DisplayPath(awsx.ConfigFilePath())
	if !awsx.IsAuthError(err) {
		title := "can't verify credentials"
		if ctx.Err() != nil {
			title = "timed out verifying credentials"
		}
		return Check{Status: Fail, Title: title, Detail: shortErr(err),
			Fix: "check network access to AWS from this machine (proxy, VPN, firewall), then: awsport doctor --profile " + p.Name}
	}
	c := Check{Status: Fail, Title: "credentials expired or invalid", Detail: shortErr(err)}
	switch p.Kind {
	case core.AuthSSO:
		if p.Managed || (p.SSOSession != "" && p.SSOSession == t.ssoSession) {
			c.Fix = "awsport login"
		} else {
			c.Fix = "awsport login " + p.Name
		}
	case core.AuthAssumeRole:
		c.Fix = fmt.Sprintf("renew the credentials of the profile's source_profile (see [profile %s] in %s), e.g. awsport login <source-profile>", p.Name, cfgPath)
	case core.AuthStatic:
		c.Fix = fmt.Sprintf("the access key is invalid, inactive or expired: create a new one (IAM console → Security credentials), then: aws configure --profile %s", p.Name)
	default:
		c.Fix = fmt.Sprintf("check [profile %s] in %s: aws configure list --profile %s", p.Name, cfgPath, p.Name)
	}
	return c
}

// ---- agent ----

// agentChecks reports the background agent and its tunnels.
func agentChecks(info *agent.Info, sessions []tunnel.Session, err error, version, logPath string, now time.Time) []Check {
	logHint := "see the agent log: " + awsx.DisplayPath(logPath)
	if info == nil {
		if err != nil {
			return []Check{{Status: Warn, Title: "the agent socket exists but the agent does not answer", Detail: shortErr(err), Fix: logHint}}
		}
		return []Check{{Status: OK, Title: "agent not running (it starts with the first tunnel)"}}
	}
	running := Check{Status: OK, Title: fmt.Sprintf("agent running: pid %d, awsport %s, up %s", info.PID, info.Version, humanDuration(now.Sub(info.Started)))}
	out := []Check{running}
	if info.Version != version {
		if info.Sessions == 0 {
			out[0].Detail = "it runs another awsport version and is replaced by the next tunnel"
		} else {
			out = append(out, Check{Status: Warn, Title: fmt.Sprintf("the agent runs awsport %s (this is %s)", info.Version, version),
				Detail: "it is replaced once all its tunnels are stopped",
				Fix:    "awsport down --all, then start your tunnels again"})
		}
	}
	if err != nil {
		return append(out, Check{Status: Warn, Title: "can't list the agent's tunnels", Detail: shortErr(err), Fix: logHint})
	}
	var active int
	var failed, expired []string
	for _, s := range sessions {
		switch s.Status {
		case tunnel.Starting, tunnel.Up, tunnel.Reconnecting:
			active++
		case tunnel.Failed:
			line := s.Spec.Label
			if s.Err != "" {
				line += ": " + s.Err
			}
			failed = append(failed, line)
		case tunnel.AuthExpired:
			expired = append(expired, s.Spec.Label)
		}
	}
	out = append(out, Check{Status: OK, Title: plural(active, "active tunnel", "active tunnels")})
	if len(expired) > 0 {
		out = append(out, Check{Status: Warn, Title: plural(len(expired), "tunnel waits", "tunnels wait") + " for a new SSO sign-in",
			Detail: strings.Join(expired, "\n"), Fix: "awsport login (they resume on the same ports; check with awsport ps)"})
	}
	if len(failed) > 0 {
		out = append(out, Check{Status: Warn, Title: plural(len(failed), "tunnel failed", "tunnels failed"),
			Detail: strings.Join(failed, "\n"), Fix: "awsport ps shows the error; awsport down <name> removes the tunnel"})
	}
	return out
}

// ---- helpers ----

// shortErr is err's text on one line, bounded to maxErrLen runes.
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(s); len(r) > maxErrLen {
		s = string(r[:maxErrLen-1]) + "…"
	}
	return s
}

func joinLines(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}
