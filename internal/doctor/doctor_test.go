package doctor

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func result(statuses ...Status) Result {
	var checks []Check
	for _, s := range statuses {
		checks = append(checks, Check{Status: s, Title: string(s)})
	}
	return Result{Sections: []Section{{Name: "A", Checks: checks}}}
}

func TestResultAggregation(t *testing.T) {
	tests := []struct {
		name     string
		statuses []Status
		worst    Status
		exit     int
		summary  string
	}{
		{name: "empty", worst: OK, exit: 0, summary: "0 ok"},
		{name: "all ok", statuses: []Status{OK, OK}, worst: OK, exit: 0, summary: "2 ok"},
		{name: "warnings only", statuses: []Status{OK, Warn, Warn}, worst: Warn, exit: 0, summary: "2 warnings, 1 ok"},
		{name: "one failure", statuses: []Status{Warn, Fail, OK}, worst: Fail, exit: 1, summary: "1 failure, 1 warning, 1 ok"},
		{name: "failures", statuses: []Status{Fail, Fail}, worst: Fail, exit: 1, summary: "2 failures, 0 ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := result(tt.statuses...)
			if got := r.Worst(); got != tt.worst {
				t.Errorf("Worst = %q, want %q", got, tt.worst)
			}
			if got := r.ExitCode(); got != tt.exit {
				t.Errorf("ExitCode = %d, want %d", got, tt.exit)
			}
			if got := r.Summary(); got != tt.summary {
				t.Errorf("Summary = %q, want %q", got, tt.summary)
			}
		})
	}
}

func TestRender(t *testing.T) {
	r := Result{Sections: []Section{
		{Name: "Tools", Checks: []Check{
			{Status: OK, Title: "AWS CLI 2.15.30", Detail: "/usr/local/bin/aws", Fix: "ignored for ok"},
			{Status: Fail, Title: "session-manager-plugin not found in PATH", Fix: "curl -o x && sudo dpkg -i x"},
		}},
		{Name: "SSO", Checks: []Check{
			{Status: Warn, Title: "SSO sign-in expires in 42m", Detail: "line one\nline two", Fix: "first\nsecond"},
		}},
	}}
	var buf bytes.Buffer
	if err := Render(&buf, r); err != nil {
		t.Fatal(err)
	}
	want := `Tools
  ✓ AWS CLI 2.15.30
    /usr/local/bin/aws
  ✗ session-manager-plugin not found in PATH
    → fix: curl -o x && sudo dpkg -i x

SSO
  ⚠ SSO sign-in expires in 42m
    line one
    line two
    → fix: first
           second

1 failure, 1 warning, 1 ok
`
	if got := buf.String(); got != want {
		t.Errorf("Render =\n%s\nwant\n%s", got, want)
	}
}

func TestRedact(t *testing.T) {
	const home = "/home/jane"
	tests := []struct {
		name, in, want string
	}{
		{"account in ARN", "arn:aws:sts::123456789012:assumed-role/Dev/x", "arn:aws:sts::********9012:assumed-role/Dev/x"},
		{"bare account", "account 123456789012.", "account ********9012."},
		{"longer number kept", "id 12345678901234", "id 12345678901234"},
		{"version kept", "aws-cli/2.15.30 1.2.553.0", "aws-cli/2.15.30 1.2.553.0"},
		{"email", "session jane.doe+x@example.co.uk here", "session <email> here"},
		{"email in ARN", "assumed-role/AWSReservedSSO_Dev_0123abcd/jane@example.com", "assumed-role/AWSReservedSSO_Dev_0123abcd/<email>"},
		{"start URL", "https://d-0000000000.awsapps.com/start", "https://d-**********.awsapps.com/start"},
		{"start URL hex id", "https://d-9a8b7c6d5e.awsapps.com/start/#", "https://d-**********.awsapps.com/start/#"},
		{"home path", "/home/jane/.aws/config", "~/.aws/config"},
		{"home itself", "home is /home/jane", "home is ~"},
		{"other user kept", "/home/janet/.aws", "/home/janet/.aws"},
		{"access key", "key AKIAABCDEFGHIJKLMNOP used", "key <key> used"},
		{"session key", "ASIAABCDEFGHIJKLMNOP", "<key>"},
		{"names kept", "Profile example-dev-developer (sso, us-east-1)", "Profile example-dev-developer (sso, us-east-1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Redact(tt.in, home); got != tt.want {
				t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMarkdownRedactsAndSummarizes(t *testing.T) {
	r := Result{
		Generated: time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC),
		Environment: Environment{Version: "v0.4.0", Commit: "abc1234", OS: "linux", Arch: "amd64", WSL: true,
			AWSCLI: "2.15.30", EnvVars: []string{"AWS_PROFILE"}},
		Sections: []Section{{Name: "SSO", Checks: []Check{
			{Status: OK, Title: "SSO portal https://d-0000000000.awsapps.com/start (us-east-1)"},
			{Status: Fail, Title: "can't write /home/jane/.aws/sso/cache", Fix: "sudo chown -R $(whoami) ~/.aws"},
		}}},
	}
	md := Markdown(r, "/home/jane")
	for _, want := range []string{
		"# awsport doctor report",
		"Profile, role and resource names are kept",
		"| OS | linux/amd64 (WSL) |",
		"| session-manager-plugin | not found |",
		"| Environment variables set | AWS_PROFILE |",
		"**1 failure, 1 ok**",
		"https://d-**********.awsapps.com/start",
		"✗ can't write ~/.aws/sso/cache",
		"→ fix: sudo chown -R $(whoami) ~/.aws",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "d-0000000000") || strings.Contains(md, "/home/jane") {
		t.Errorf("report not redacted:\n%s", md)
	}
}
