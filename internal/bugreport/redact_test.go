package bugreport

import (
	"strings"
	"testing"

	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/state"
	"github.com/nhl4000/awsport/internal/tunnel"
)

const testHome = "/home/jdoe"

// testNames is a realistic machine: SSO profiles of two accounts, an
// assume-role profile, discovered EC2/RDS resources, saved state, a group
// and a running tunnel.
func testNames() *Names {
	n := &Names{}
	for _, p := range []core.Profile{
		{Name: "acme-prod", Kind: core.AuthSSO, AccountID: "123456789012", RoleName: "AcmeDeveloper", SSOSession: "acme-sso"},
		{Name: "acme-staging", Kind: core.AuthSSO, AccountID: "210987654321", RoleName: "AcmeDeveloper", SSOSession: "acme-sso"},
		{Name: "acme-prod-admin", Kind: core.AuthAssumeRole, RoleName: "PlatformAdmin"},
		{Name: "default", Kind: core.AuthStatic},
	} {
		n.AddProfile(p)
	}
	n.AddResource(core.Resource{Kind: core.KindEC2, Profile: "acme-prod", ID: "i-0123456789abcdef0", Name: "billing-api"})
	n.AddResource(core.Resource{Kind: core.KindRDS, Profile: "acme-prod", ID: "orders-db", Name: "orders-db",
		Host: "orders-db.cabc123xyz.ca-central-1.rds.amazonaws.com"})
	n.AddConfig(&config.Config{
		SSO:    config.SSO{StartURL: "https://acme-corp.awsapps.com/start/#/", Session: "acme-sso"},
		Groups: []core.Group{{Name: "nightly-sync", Members: []core.GroupMember{{Profile: "acme-staging", Name: "etl-runner"}}}},
	})
	n.AddState(state.Data{
		Ports:       map[string]int{"acme-staging/ca-central-1/staging-web/8080": 18080},
		ManualPorts: map[string][]core.PortSpec{"acme-prod/us-east-1/legacy-box": {{Remote: 9000}}},
		Bastions:    map[string]string{"acme-ops/ca-central-1": "i-0fedcba9876543210"},
	})
	n.AddSession(tunnel.Session{Spec: core.TunnelSpec{
		Key: "acme-prod/ca-central-1/reports-db/5432", Label: "reports-db 5432", Profile: "acme-prod",
	}})
	n.AddLogLines([]string{
		"2026/11/11 15:35:18 start search-node mailpit (acme-sandbox/eu-west-1/i-0aaaabbbbccccdddd/8025) on 127.0.0.1:18025",
		"2026/11/11 15:35:19 analytics-db 3306 (acme-data/eu-west-1/analytics-db/3306): up",
		"2026/11/11 15:51:09 shutting down: idle for 30s",
	})
	return n
}

// secrets are the identifying strings of testNames and testInput that must
// not survive redaction (compared case-insensitively).
var secrets = []string{
	"acme-prod", "acme-staging", "acme-prod-admin", "acme-ops", "acme-sandbox", "acme-data",
	"AcmeDeveloper", "PlatformAdmin", "acme-sso", "acme-corp",
	"billing-api", "orders-db", "nightly-sync", "etl-runner", "staging-web", "legacy-box", "reports-db",
	"search-node", "analytics-db",
	"123456789012", "210987654321", "0123456789abcdef0", "0fedcba9876543210", "0aaaabbbbccccdddd",
	"cabc123xyz", "d-1234567890", "jdoe", "jane.doe@example.com", "AKIAIOSFODNN7EXAMPLE",
	"10.0.12.34", "172.20.1.5", "192.168.1.10", "100.101.102.103", "ip-10-0-1-23",
	"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
}

const testInput = `Profile acme-prod (sso, 123456789012 AcmeDeveloper)
  ✗ ec2:DescribeInstances denied
    User: arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_AcmeDeveloper_0123abcd/jane.doe@example.com is not authorized
    → fix: awsport login acme-prod
Profile ACME-STAGING (sso, 210987654321 AcmeDeveloper)
Profile acme-prod-admin (assume-role, PlatformAdmin)
  role_arn = arn:aws:iam::123456789012:role/PlatformAdmin
SSO start URL https://acme-corp.awsapps.com/start/#/ (session acme-sso)
Other portal https://d-1234567890.awsapps.com/start
2026/11/11 15:35:18 start billing-api mailpit (acme-prod/ca-central-1/i-0123456789abcdef0/8025) on 127.0.0.1:18025
2026/11/11 15:35:22 orders-db 5432 (acme-prod/ca-central-1/orders-db/5432): failed: dial orders-db.cabc123xyz.ca-central-1.rds.amazonaws.com:5432
bastion i-0fedcba9876543210 for acme-ops at ip-10-0-1-23.ca-central-1.compute.internal (10.0.12.34)
group nightly-sync: etl-runner, staging-web, legacy-box, reports-db
search-node mailpit; analytics-db 3306 (acme-sandbox, acme-data)
unknown acme-prod-worker host 172.20.1.5 via 192.168.1.10 and 100.101.102.103
config /home/jdoe/.aws/config, Windows C:\Users\jdoe\AppData and /mnt/c/Users/jdoe/Downloads
key AKIAIOSFODNN7EXAMPLE token ghp_abcdefghijklmnopqrstuvwxyz0123456789
`

func TestRedactRemovesEverySecret(t *testing.T) {
	got := testNames().Redactor(testHome).Redact(testInput)
	lower := strings.ToLower(got)
	for _, s := range secrets {
		if strings.Contains(lower, strings.ToLower(s)) {
			t.Errorf("%q survived redaction:\n%s", s, got)
		}
	}
	for _, keep := range []string{"127.0.0.1:18025", "ec2:DescribeInstances", "mailpit", "ca-central-1", "awsport login profile-1"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction removed %q:\n%s", keep, got)
		}
	}
}

func TestRedactCases(t *testing.T) {
	r := testNames().Redactor(testHome)
	for _, tc := range []struct{ name, in, want string }{
		{"profile", "awsport login acme-prod", "awsport login profile-1"},
		{"profile case-insensitive", "Profile ACME-STAGING", "Profile profile-2"},
		{"longest name wins", "acme-prod-admin and acme-prod", "profile-3 and profile-1"},
		{"unknown suffix keeps no profile part", "acme-prod-worker", "profile-1-worker"},
		{"not inside a longer word", "acme-production xacme-prod", "acme-production xacme-prod"},
		{"default is kept", "profile default", "profile default"},
		{"resource", "billing-api mailpit", "resource-1 mailpit"},
		{"rds id from resource", "orders-db is available", "resource-2 is available"},
		{"group and member", "nightly-sync: etl-runner", "resource-3: resource-4"},
		{"state ports and manual ports", "staging-web legacy-box", "resource-5 resource-6"},
		{"role", "role AcmeDeveloper", "role role-1"},
		{"sso-session", "[sso-session acme-sso]", "[sso-session sso-session-1]"},
		{"configured start URL", "start URL https://acme-corp.awsapps.com/start/#/ ok", "start URL https://<sso-portal>/start ok"},
		{"directory start URL", "https://d-1234567890.awsapps.com/start#/", "https://<sso-portal>/start"},
		{"bare portal host", "acme-corp.awsapps.com", "<sso-portal>"},
		{"sts ARN", "User: arn:aws:sts::123456789012:assumed-role/AcmeDeveloper/jane is not authorized",
			"User: arn:aws:sts::****:<redacted> is not authorized"},
		{"rds ARN", "arn:aws:rds:ca-central-1:123456789012:db:orders-db.", "arn:aws:rds:ca-central-1:****:<redacted>"},
		{"gov partition ARN", "arn:aws-us-gov:iam::123456789012:role/x", "arn:aws-us-gov:iam::****:<redacted>"},
		{"rds endpoint", "dial orders-db.cabc123xyz.ca-central-1.rds.amazonaws.com:5432", "dial <aws-endpoint>:5432"},
		{"service endpoint", "https://ssm.ca-central-1.amazonaws.com/", "https://<aws-endpoint>/"},
		{"instance IDs", "i-0123456789abcdef0 i-1a2b3c4d mi-0123456789abcdef0", "i-<redacted> i-<redacted> i-<redacted>"},
		{"account ID", "account 123456789012", "account <account-id>"},
		{"email", "jane.doe@example.com", "<email>"},
		{"access key", "AKIAIOSFODNN7EXAMPLE", "<key>"},
		{"github token", "token ghp_abcdefghijklmnopqrstuvwxyz0123456789", "token <token>"},
		{"home directory", "/home/jdoe/.aws/config", "~/.aws/config"},
		{"other user paths", `/mnt/c/Users/jdoe/x C:\Users\jdoe\y /home/other/z`, `/mnt/c/Users/<user>/x C:\Users\<user>\y /home/<user>/z`},
		{"private IPs", "10.0.12.34 172.16.0.1 172.31.255.255 192.168.1.10 169.254.169.254 100.64.0.1",
			"<ip> <ip> <ip> <ip> <ip> <ip>"},
		{"EC2 private DNS", "ip-10-0-1-23.ca-central-1.compute.internal", "<ip>"},
		{"public, loopback and versions kept", "127.0.0.1:18025 8.8.8.8 172.32.0.1 aws-cli/2.10.1.2", "127.0.0.1:18025 8.8.8.8 172.32.0.1 aws-cli/2.10.1.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Redact(tc.in); got != tc.want {
				t.Errorf("Redact(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactConsistentMapping(t *testing.T) {
	r := testNames().Redactor(testHome)
	a := r.Redact("acme-staging then billing-api")
	b := r.Redact("billing-api, again acme-staging")
	if a != "profile-2 then resource-1" || b != "resource-1, again profile-2" {
		t.Errorf("inconsistent mapping: %q / %q", a, b)
	}
}

func TestNamesFromLogAndSessions(t *testing.T) {
	n := &Names{}
	n.AddLogLines([]string{
		"2026/11/11 15:35:18 start my app mailpit (team-x/eu-west-1/i-0123456789abcdef0/8025) on 127.0.0.1:18025",
		"2026/11/11 15:35:19 warehouse 5432 (team-y/eu-west-1/warehouse/5432): up",
		"not a log line (a/b/c/1)",
	})
	n.AddSession(tunnel.Session{Spec: core.TunnelSpec{Key: "team-z/us-east-1/i-0aaaabbbbccccdddd/22", Label: "jump-host ssh", Profile: "team-z"}})
	r := n.Redactor("")
	got := r.Redact("team-x team-y team-z | my app | warehouse | jump-host | i-0aaaabbbbccccdddd")
	want := "profile-1 profile-2 profile-3 | resource-1 | resource-2 | resource-3 | i-<redacted>"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestRedactShortNamesIgnored(t *testing.T) {
	n := &Names{}
	n.AddProfileName("a")
	n.AddResourceName("x")
	if got := n.Redactor("").Redact("a box x"); got != "a box x" {
		t.Errorf("got %q", got)
	}
}
