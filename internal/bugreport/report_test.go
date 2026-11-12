package bugreport

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nhl4000/awsport/internal/doctor"
)

func testInputResult() Input {
	return Input{
		Title: "TUI freezes after login to acme-prod",
		What:  "Opened a tunnel to billing-api on acme-prod.\nIt hung.",
		Doctor: doctor.Result{
			Generated: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC),
			Environment: doctor.Environment{
				Version: "v1.2.3", Commit: "abc1234", OS: "linux", Arch: "amd64", WSL: true,
				AWSCLI: "2.15.0", EnvVars: []string{"AWS_PROFILE"},
			},
			Sections: []doctor.Section{
				{Name: "Tools", Checks: []doctor.Check{{Status: doctor.OK, Title: "aws 2.15.0"}}},
				{Name: "Profile acme-prod (sso, 123456789012 AcmeDeveloper)", Checks: []doctor.Check{
					{Status: doctor.Fail, Title: "credentials expired", Detail: "token for acme-sso expired", Fix: "awsport login acme-prod"},
					{Status: doctor.Warn, Title: "rds:DescribeDBInstances denied", Fix: "ask an admin\nto grant it"},
				}},
			},
		},
		LogName: "agent.log",
		Log: []string{
			"2026/11/11 15:35:18 start billing-api mailpit (acme-prod/ca-central-1/i-0123456789abcdef0/8025) on 127.0.0.1:18025",
			"2026/11/11 15:35:19 billing-api mailpit (acme-prod/ca-central-1/i-0123456789abcdef0/8025): up",
		},
		CrashName: "crash-20261111-153600.log",
		Crash:     "panic: runtime error\n\ngoroutine 1 [running]:\nmain.main()\n\t/home/jdoe/src/awsport/main.go:10\n",
	}
}

func TestBuildRedactsAndRendersBody(t *testing.T) {
	rep := Build(testInputResult(), testNames().Redactor(testHome))
	body := rep.Body()
	for _, want := range []string{
		"### What happened?\n\nOpened a tunnel to resource-1 on profile-1.\nIt hung.\n",
		"### Steps to reproduce\n\n_No response_\n",
		"- awsport: v1.2.3 (abc1234)\n- OS: linux/amd64 (WSL)\n- AWS CLI: 2.15.0\n- session-manager-plugin: not found\n- Environment variables set: AWS_PROFILE",
		"```text\nawsport doctor: 1 failure, 1 warning, 1 ok\n",
		"✗ Profile profile-1 (sso, <account-id> role-1): credentials expired\n    token for sso-session-1 expired\n    → fix: awsport login profile-1\n",
		"⚠ Profile profile-1 (sso, <account-id> role-1): rds:DescribeDBInstances denied\n    → fix: ask an admin\n           to grant it\n",
		"agent.log (last 2 lines):\n2026/11/11 15:35:18 start resource-1 mailpit (profile-1/ca-central-1/i-<redacted>/8025) on 127.0.0.1:18025\n",
		"Latest crash log (crash-20261111-153600.log):\npanic: runtime error\n",
		"~/src/awsport/main.go:10\n```\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "aws 2.15.0") {
		t.Error("ok checks must not be listed")
	}
	if rep.Title != "TUI freezes after login to profile-1" {
		t.Errorf("title %q", rep.Title)
	}
}

func TestBodyFenceLongerThanContent(t *testing.T) {
	rep := Report{Doctor: "x ``` y ```` z"}
	if !strings.Contains(rep.Body(), "`````text\n") {
		t.Errorf("fence not lengthened:\n%s", rep.Body())
	}
}

func formValues(t *testing.T, u string) url.Values {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if p.Host != "github.com" || p.Path != "/nhl4000/awsport/issues/new" {
		t.Fatalf("unexpected form URL %s", u)
	}
	return p.Query()
}

func TestFormURLShortReportIsComplete(t *testing.T) {
	rep := Build(testInputResult(), testNames().Redactor(testHome))
	u := rep.FormURL()
	v := formValues(t, u)
	if v.Get("template") != Template || v.Get("title") != rep.Title || v.Get("what") != rep.What ||
		v.Get("environment") != rep.Environment || v.Get("diagnostics") != rep.Diagnostics() {
		t.Errorf("fields not prefilled verbatim: %v", v)
	}
}

func TestFormURLLengthCap(t *testing.T) {
	long := make([]string, LogLines)
	for i := range long {
		long[i] = fmt.Sprintf("2026/11/11 15:35:%02d resource-%d mailpit (profile-1/ca-central-1/i-<redacted>/8025): failed: %s", i, i, strings.Repeat("ε", 120))
	}
	base := Report{Title: "t", What: "w", Environment: "- awsport: dev", Doctor: "awsport doctor: 3 ok", LogName: "agent.log", Log: long}

	t.Run("drops oldest log lines with a note", func(t *testing.T) {
		u := base.FormURL()
		if len(u) > MaxURLLen {
			t.Fatalf("URL is %d chars", len(u))
		}
		diag := formValues(t, u).Get("diagnostics")
		if !strings.Contains(diag, "earlier lines omitted to fit the link") {
			t.Errorf("no truncation note:\n%s", diag)
		}
		if !strings.Contains(diag, long[len(long)-1]) || strings.Contains(diag, long[0]+"\n") {
			t.Errorf("should keep the newest lines and drop the oldest:\n%s", diag)
		}
	})
	t.Run("cuts a huge crash log", func(t *testing.T) {
		rep := base
		rep.CrashName, rep.Crash = "crash-20261111-153600.log", "panic: boom\n"+strings.Repeat("goroutine frame\n", 2000)
		u := rep.FormURL()
		if len(u) > MaxURLLen {
			t.Fatalf("URL is %d chars", len(u))
		}
		diag := formValues(t, u).Get("diagnostics")
		if !strings.Contains(diag, "panic: boom") || !strings.HasSuffix(diag, truncated) {
			t.Errorf("crash log not cut with a note:\n%s", diag)
		}
	})
	t.Run("cuts the description last", func(t *testing.T) {
		rep := base
		rep.What = strings.Repeat("very long description ", 1000)
		u := rep.FormURL()
		if len(u) > MaxURLLen {
			t.Fatalf("URL is %d chars", len(u))
		}
		v := formValues(t, u)
		if v.Get("title") != "t" || !strings.HasSuffix(v.Get("what"), truncated) {
			t.Errorf("unexpected fields: title %q, what suffix %q", v.Get("title"), v.Get("what")[max(0, len(v.Get("what"))-40):])
		}
	})
}

func TestCutKeepsUTF8(t *testing.T) {
	if got := cut("aεb", 2); got != "a" {
		t.Errorf("cut = %q", got)
	}
}
