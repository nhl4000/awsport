package bugreport

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/nhl4000/awsport/internal/doctor"
)

const (
	// RepoOwner and RepoName name the repository issues are filed in.
	RepoOwner = "nhl4000"
	RepoName  = "awsport"
	// Template is the issue form in .github/ISSUE_TEMPLATE.
	Template = "bug_report.yml"
	// MaxURLLen bounds the prefilled issue-form URL; browsers and GitHub
	// reject much longer ones.
	MaxURLLen = 7500
	// LogLines is how many agent.log lines a report includes.
	LogLines = 40

	noResponse = "_No response_"
	truncated  = "\n[… truncated to fit the link]"
)

// Issue form field labels; GitHub renders a submitted form as
// "### <label>" sections, and API-created issues use the same layout.
const (
	labelWhat        = "What happened?"
	labelSteps       = "Steps to reproduce"
	labelEnvironment = "Environment"
	labelDiagnostics = "Diagnostics"
)

// Input is the raw, unredacted material of a report.
type Input struct {
	Title, What string
	Doctor      doctor.Result
	LogName     string   // e.g. "agent.log"
	Log         []string // tail of the agent log, oldest first
	CrashName   string   // crash log file name; "" when none
	Crash       string
}

// Report is a redacted bug report.
type Report struct {
	Title       string
	What        string
	Environment string // Markdown list
	Doctor      string // doctor summary and non-ok checks
	LogName     string
	Log         []string
	CrashName   string
	Crash       string
}

// Build redacts in with r into a Report.
func Build(in Input, r *Redactor) Report {
	rep := Report{
		Title:       r.Redact(strings.TrimSpace(in.Title)),
		What:        r.Redact(strings.TrimSpace(in.What)),
		Environment: r.Redact(environment(in.Doctor.Environment)),
		Doctor:      r.Redact(doctorSummary(in.Doctor)),
		LogName:     in.LogName,
		CrashName:   in.CrashName,
		Crash:       r.Redact(strings.TrimRight(in.Crash, "\n")),
	}
	for _, l := range in.Log {
		rep.Log = append(rep.Log, r.Redact(l))
	}
	return rep
}

func environment(e doctor.Environment) string {
	version := e.Version
	if e.Commit != "" {
		version += " (" + e.Commit + ")"
	}
	osLine := e.OS + "/" + e.Arch
	if e.WSL {
		osLine += " (WSL)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- awsport: %s\n", version)
	fmt.Fprintf(&b, "- OS: %s\n", osLine)
	fmt.Fprintf(&b, "- AWS CLI: %s\n", orDefault(e.AWSCLI, "not found"))
	fmt.Fprintf(&b, "- session-manager-plugin: %s\n", orDefault(e.Plugin, "not found"))
	fmt.Fprintf(&b, "- Environment variables set: %s", orDefault(strings.Join(e.EnvVars, ", "), "none"))
	return b.String()
}

// doctorSummary is the doctor tally followed by every warning and failure
// with its detail and fix.
func doctorSummary(res doctor.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "awsport doctor: %s", res.Summary())
	for _, sec := range res.Sections {
		for _, c := range sec.Checks {
			if c.Status == doctor.OK {
				continue
			}
			fmt.Fprintf(&b, "\n%s %s: %s", c.Status.Symbol(), sec.Name, c.Title)
			for _, l := range nonEmptyLines(c.Detail) {
				b.WriteString("\n    " + l)
			}
			for i, l := range nonEmptyLines(c.Fix) {
				prefix := "\n    → fix: "
				if i > 0 {
					prefix = "\n           "
				}
				b.WriteString(prefix + l)
			}
		}
	}
	return b.String()
}

// Diagnostics is the plain-text diagnostics section: doctor summary, the
// last logLines log lines and the crash log cut to crashLen bytes
// (negative: all of it).
func (r Report) Diagnostics() string { return r.diagnostics(len(r.Log), -1) }

func (r Report) diagnostics(logLines, crashLen int) string {
	var b strings.Builder
	b.WriteString(r.Doctor)
	if r.LogName != "" {
		logLines = min(logLines, len(r.Log))
		fmt.Fprintf(&b, "\n\n%s (last %d lines):", r.LogName, len(r.Log))
		if omitted := len(r.Log) - logLines; omitted > 0 {
			fmt.Fprintf(&b, "\n[%d earlier lines omitted to fit the link; run `awsport bug --dry-run` to see them]", omitted)
		}
		if len(r.Log) == 0 {
			b.WriteString("\n(empty)")
		}
		for _, l := range r.Log[len(r.Log)-logLines:] {
			b.WriteString("\n" + l)
		}
	}
	if r.CrashName != "" {
		fmt.Fprintf(&b, "\n\nLatest crash log (%s):\n", r.CrashName)
		if crashLen >= 0 && crashLen < len(r.Crash) {
			b.WriteString(cut(r.Crash, crashLen) + truncated)
		} else {
			b.WriteString(r.Crash)
		}
	}
	return b.String()
}

// Body renders the issue body in the issue form's layout.
func (r Report) Body() string {
	diag := r.Diagnostics()
	f := fence(diag)
	var b strings.Builder
	section := func(label, value string) {
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", label, orDefault(value, noResponse))
	}
	section(labelWhat, r.What)
	section(labelSteps, "")
	section(labelEnvironment, r.Environment)
	section(labelDiagnostics, f+"text\n"+diag+"\n"+f)
	b.WriteString("---\n<sub>Sent with `awsport bug`. Profile, resource, role and session names, account IDs, ARNs, " +
		"AWS endpoints, instance IDs, emails, private IPs and paths are redacted.</sub>\n")
	return b.String()
}

// FormURL returns the GitHub new-issue link with the issue form prefilled,
// at most MaxURLLen long: it drops the oldest log lines first, then cuts
// the crash log, the diagnostics and finally the description.
func (r Report) FormURL() string {
	what, diag := r.What, ""
	build := func() string {
		v := url.Values{}
		v.Set("template", Template)
		v.Set("title", r.Title)
		v.Set("what", what)
		v.Set("environment", r.Environment)
		v.Set("diagnostics", diag)
		return fmt.Sprintf("https://github.com/%s/%s/issues/new?%s", RepoOwner, RepoName, v.Encode())
	}
	fits := func() bool { return len(build()) <= MaxURLLen }
	for n := len(r.Log); n >= 0; n-- {
		if diag = r.diagnostics(n, -1); fits() {
			return build()
		}
	}
	if r.CrashName != "" {
		if n := longest(len(r.Crash), func(n int) bool { diag = r.diagnostics(0, n); return fits() }); n >= 0 {
			diag = r.diagnostics(0, n)
			return build()
		}
	}
	full := r.diagnostics(0, 0)
	shorten := func(s string, n int) string {
		if n >= len(s) {
			return s
		}
		return cut(s, n) + truncated
	}
	if n := longest(len(full), func(n int) bool { diag = shorten(full, n); return fits() }); n >= 0 {
		diag = shorten(full, n)
		return build()
	}
	diag = ""
	fullWhat := r.What
	n := max(longest(len(fullWhat), func(n int) bool { what = shorten(fullWhat, n); return fits() }), 0)
	what = shorten(fullWhat, n)
	return build()
}

// longest returns the largest n in [0, max] with ok(n), assuming ok is
// monotonic, or -1 when even ok(0) is false.
func longest(maxN int, ok func(int) bool) int {
	if !ok(0) {
		return -1
	}
	lo, hi := 0, maxN // ok(lo) holds
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if ok(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// cut returns at most n bytes of s without splitting a UTF-8 sequence.
func cut(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// fence returns a backtick fence longer than any backtick run in s.
func fence(s string) string {
	longestRun, run := 0, 0
	for i := range len(s) {
		if s[i] == '`' {
			run++
			longestRun = max(longestRun, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longestRun+1))
}

func nonEmptyLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
