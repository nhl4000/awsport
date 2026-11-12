package doctor

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

var (
	accessKeyRe = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
	emailRe     = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	directoryRe = regexp.MustCompile(`\bd-[0-9a-z]{10}\b`)
	accountRe   = regexp.MustCompile(`\b\d{8}(\d{4})\b`)
)

// Redact removes personal and account identifiers from s: the home
// directory becomes "~", AWS access key IDs "<key>", email addresses
// "<email>", IAM Identity Center directory IDs "d-**********" and 12-digit
// account IDs keep only their last four digits ("********1234"). Profile,
// role and resource names are kept.
func Redact(s, home string) string {
	if home = strings.TrimRight(home, `/\`); len(home) > 1 {
		homeRe := regexp.MustCompile(regexp.QuoteMeta(home) + `([/\\]|$|[^\w.-])`)
		s = homeRe.ReplaceAllString(s, "~$1")
	}
	s = accessKeyRe.ReplaceAllString(s, "<key>")
	s = emailRe.ReplaceAllString(s, "<email>")
	s = directoryRe.ReplaceAllString(s, "d-**********")
	return accountRe.ReplaceAllString(s, "********$1")
}

// Markdown renders r as a shareable report: environment, summary and every
// check, redacted with Redact. home is the user's home directory.
func Markdown(r Result, home string) string {
	var body strings.Builder
	env := r.Environment
	osLine := env.OS + "/" + env.Arch
	if env.WSL {
		osLine += " (WSL)"
	}
	version := env.Version
	if env.Commit != "" {
		version += " (" + env.Commit + ")"
	}
	body.WriteString("## Environment\n\n| | |\n|---|---|\n")
	row := func(k, v string) { fmt.Fprintf(&body, "| %s | %s |\n", k, mdCell(v)) }
	row("awsport", version)
	row("OS", osLine)
	row("AWS CLI", orNone(env.AWSCLI, "not found"))
	row("session-manager-plugin", orNone(env.Plugin, "not found"))
	row("Environment variables set", orNone(strings.Join(env.EnvVars, ", "), "none"))

	fmt.Fprintf(&body, "\n## Summary\n\n**%s**\n\n## Checks\n\n```text\n", r.Summary())
	var checks bytes.Buffer
	_ = Render(&checks, r) // a bytes.Buffer is not a terminal: plain text
	body.Write(checks.Bytes())
	body.WriteString("```\n")

	var out strings.Builder
	out.WriteString("# awsport doctor report\n\n")
	fmt.Fprintf(&out, "Generated %s by `awsport doctor --report`.\n\n", r.Generated.UTC().Format("2006-01-02 15:04 UTC"))
	out.WriteString("> **Redacted:** AWS account IDs (all but the last 4 digits), email addresses, " +
		"the IAM Identity Center directory ID in the SSO start URL, AWS access key IDs and your " +
		"home directory (shown as `~`). Profile, role and resource names are kept; review before sharing.\n\n")
	out.WriteString(Redact(body.String(), home))
	return out.String()
}

func orNone(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

func mdCell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}
