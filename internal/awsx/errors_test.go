package awsx

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/smithy-go"
)

func TestIsAuthError(t *testing.T) {
	api := func(code string) error { return &smithy.GenericAPIError{Code: code, Message: "request failed"} }
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ExpiredToken", api("ExpiredToken"), true},
		{"ExpiredTokenException", api("ExpiredTokenException"), true},
		{"InvalidClientTokenId", api("InvalidClientTokenId"), true},
		{"UnrecognizedClientException", api("UnrecognizedClientException"), true},
		{"UnauthorizedException", api("UnauthorizedException"), true},
		{"wrapped code", fmt.Errorf("region x: ec2: %w", api("ExpiredToken")), true},
		{"joined code", errors.Join(errors.New("other"), api("InvalidClientTokenId")), true},
		{"AccessDeniedException", api("AccessDeniedException"), false},
		{"UnauthorizedOperation", api("UnauthorizedOperation"), false},
		{"throttling", api("ThrottlingException"), false},
		{"sso invalid token", &ssocreds.InvalidTokenError{Err: errors.New("x")}, true},
		{"token expired message", errors.New("operation error STS: The SSO session token has expired"), true},
		{"sso session message", errors.New("The SSO session associated with this profile has expired or is otherwise invalid"), true},
		{"cached token message", errors.New("failed to refresh cached credentials, failed to read cached SSO token file"), true},
		{"sentinel credentials", fmt.Errorf("x: %w", ErrCredentials), true},
		{"sentinel sso", fmt.Errorf("x: %w", ErrSSOTokenExpired), true},
		{"plain", errors.New("connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAuthError(tt.err); got != tt.want {
				t.Errorf("IsAuthError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestAuthWrap(t *testing.T) {
	d := NewDiscoverer()
	d.cfgs["example"] = d.cfgs["other"]
	err := d.authWrap("example", &smithy.GenericAPIError{Code: "ExpiredToken"})
	if !errors.Is(err, ErrCredentials) {
		t.Errorf("err = %v, want ErrCredentials", err)
	}
	if _, ok := d.cfgs["example"]; ok {
		t.Error("cached config not dropped after auth error")
	}
	plain := errors.New("boom")
	if got := d.authWrap("example", plain); got != plain {
		t.Errorf("non-auth error changed: %v", got)
	}
}

func TestParseAccessDenied(t *testing.T) {
	tests := []struct {
		name, msg, action, principal string
		ok                           bool
	}{
		{"ssm start session",
			"An error occurred (AccessDeniedException) when calling the StartSession operation: User: " + testPrincipal +
				" is not authorized to perform: ssm:StartSession on resource: arn:aws:ssm:us-east-1::document/AWS-StartPortForwardingSessionToRemoteHost because no identity-based policy allows the ssm:StartSession action",
			"ssm:StartSession", testPrincipal, true},
		{"explicit deny",
			"User: arn:aws:iam::123456789012:user/jane is not authorized to perform: rds:DescribeDBClusters on resource: arn:aws:rds:ca-central-1:123456789012:cluster:* with an explicit deny in an identity-based policy",
			"rds:DescribeDBClusters", "arn:aws:iam::123456789012:user/jane", true},
		{"policy phrase only", "AccessDenied: no identity-based policy allows the ec2:DescribeInstances action",
			"ec2:DescribeInstances", "", true},
		{"bare ec2", "You are not authorized to perform this operation.", "", "", true},
		{"code only", "An error occurred (AccessDeniedException) when calling the StartSession operation", "", "", true},
		{"not denied", "An error occurred (TargetNotConnected) when calling the StartSession operation: i-0123456789abcdef0 is not connected.", "", "", false},
		{"expired", "The security token included in the request is expired", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, principal, ok := ParseAccessDenied(tt.msg)
			if action != tt.action || principal != tt.principal || ok != tt.ok {
				t.Errorf("ParseAccessDenied = (%q, %q, %v), want (%q, %q, %v)", action, principal, ok, tt.action, tt.principal, tt.ok)
			}
		})
	}
}

func TestDeniedResource(t *testing.T) {
	tests := []struct{ msg, want string }{
		{"User: " + testPrincipal + " is not authorized to perform: ssm:StartSession on resource: arn:aws:ssm:us-east-1::document/AWS-StartPortForwardingSession because no identity-based policy allows the ssm:StartSession action",
			"arn:aws:ssm:us-east-1::document/AWS-StartPortForwardingSession"},
		{"is not authorized to perform: ssm:StartSession on resource: arn:aws:ec2:us-east-1:123456789012:instance/i-0123456789abcdef0.",
			"arn:aws:ec2:us-east-1:123456789012:instance/i-0123456789abcdef0"},
		{"An error occurred (AccessDeniedException) when calling the StartSession operation", ""},
	}
	for _, tt := range tests {
		if got := DeniedResource(tt.msg); got != tt.want {
			t.Errorf("DeniedResource(%q) = %q, want %q", tt.msg, got, tt.want)
		}
	}
}

func TestPermissionError(t *testing.T) {
	tests := []struct {
		principal, set, hint string
	}{
		{testPrincipal, "Developer", `in the "Developer" permission set`},
		{"arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Read_Only_Access_0123456789abcdef/jane", "Read_Only_Access", `in the "Read_Only_Access" permission set`},
		{"arn:aws:iam::123456789012:role/aws-reserved/sso.amazonaws.com/AWSReservedSSO_Admin_0123456789abcdef", "Admin", `in the "Admin" permission set`},
		{"arn:aws:sts::123456789012:assumed-role/deploy/ci", "deploy", `for the "deploy" role`},
		{"arn:aws:iam::123456789012:user/jane", "", `for the role of profile "example"`},
		{"", "", `for the role of profile "example"`},
	}
	for _, tt := range tests {
		apiErr := &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied"}
		e := &PermissionError{Profile: "example", Region: "ca-central-1", Action: "rds:DescribeDBInstances",
			What: "list RDS databases", Principal: tt.principal, Err: apiErr}
		if got := e.PermissionSet(); got != tt.set {
			t.Errorf("PermissionSet(%q) = %q, want %q", tt.principal, got, tt.set)
		}
		want := "Ask your AWS admin to allow rds:DescribeDBInstances " + tt.hint + " (see " + IAMDocsURL + ")."
		if got := e.Hint(); got != want {
			t.Errorf("Hint = %q, want %q", got, want)
		}
		if !errors.Is(e, apiErr) || IsAuthError(e) {
			t.Errorf("unwrap/auth classification wrong for %v", e)
		}
	}
	e := &PermissionError{Profile: "example", Region: "ca-central-1", Action: "rds:DescribeDBInstances", What: "list RDS databases"}
	if got, want := e.Error(), "example: not allowed to list RDS databases in ca-central-1 (missing rds:DescribeDBInstances)"; got != want {
		t.Errorf("Error = %q, want %q", got, want)
	}
	e.Region = ""
	if got, want := e.Error(), "example: not allowed to list RDS databases (missing rds:DescribeDBInstances)"; got != want {
		t.Errorf("Error = %q, want %q", got, want)
	}
}

func TestPermissionErrors(t *testing.T) {
	pe := func(action, region string) *PermissionError {
		return &PermissionError{Profile: "example", Region: region, Action: action, What: "x"}
	}
	a, b, c := pe("rds:DescribeDBInstances", "ca-central-1"), pe("rds:DescribeDBInstances", "us-east-1"), pe("ec2:DescribeInstances", "ca-central-1")
	err := errors.Join(
		fmt.Errorf("region: %w", errors.Join(a, errors.New("boom"))),
		b,
		fmt.Errorf("again: %w", pe("rds:DescribeDBInstances", "ca-central-1")),
		fmt.Errorf("multi: %w and %w", c, ErrCredentials),
	)
	got := PermissionErrors(err)
	if len(got) != 3 || got[0] != a || got[1] != b || got[2] != c {
		t.Errorf("PermissionErrors = %v, want [%v %v %v]", got, a, b, c)
	}
	if got := PermissionErrors(errors.New("plain")); got != nil {
		t.Errorf("PermissionErrors(plain) = %v, want nil", got)
	}
}

func TestWithoutPermissionErrors(t *testing.T) {
	pe := func(action string) *PermissionError {
		return &PermissionError{Profile: "example", Region: "ca-central-1", Action: action, What: "x"}
	}
	boom := errors.New("boom")
	expired := &smithy.GenericAPIError{Code: "ExpiredToken", Message: "expired"}

	onlyDenied := fmt.Errorf("discover example: %w", errors.Join(pe("ec2:DescribeInstances"), fmt.Errorf("x: %w", pe("rds:DescribeDBClusters"))))
	if got := WithoutPermissionErrors(onlyDenied); got != nil {
		t.Errorf("only permission errors: got %v, want nil", got)
	}
	if WithoutPermissionErrors(nil) != nil {
		t.Error("nil: want nil")
	}
	if got := WithoutPermissionErrors(boom); got != boom {
		t.Errorf("unchanged error replaced: %v", got)
	}

	mixed := errors.Join(
		fmt.Errorf("discover a: %w", errors.Join(pe("ec2:DescribeInstances"), boom)),
		pe("rds:DescribeDBInstances"),
		fmt.Errorf("discover b: %w", pe("rds:DescribeDBClusters")),
	)
	got := WithoutPermissionErrors(mixed)
	if got == nil || got.Error() != "discover a: boom" || !errors.Is(got, boom) || len(PermissionErrors(got)) != 0 {
		t.Errorf("mixed: got %v", got)
	}

	auth := fmt.Errorf("%w: profile example: %w", ErrCredentials, errors.Join(pe("ssm:DescribeInstanceInformation"), expired))
	got = WithoutPermissionErrors(auth)
	if got == nil || got.Error() != "credentials expired or invalid: profile example: "+expired.Error() ||
		!errors.Is(got, ErrCredentials) || !IsAuthError(got) {
		t.Errorf("auth wrapper: got %v", got)
	}
}

func TestHintsGroupByPermissionSet(t *testing.T) {
	dev := testPrincipal // AWSReservedSSO_Developer_...
	ops := "arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Ops_0123456789abcdef/jane"
	perm := func(principal, region, action string) *PermissionError {
		return &PermissionError{Profile: "example", Region: region, Action: action, Principal: principal}
	}
	got := Hints([]*PermissionError{
		perm(dev, "ca-central-1", "rds:DescribeDBInstances"),
		perm(dev, "us-east-2", "rds:DescribeDBInstances"), // same action, other region: listed once
		perm(ops, "ca-central-1", "ssm:DescribeInstanceInformation"),
		perm(dev, "ca-central-1", "rds:DescribeDBClusters"),
		perm(dev, "ca-central-1", "ec2:DescribeInstances"),
	})
	want := []string{
		`Ask your AWS admin to allow rds:DescribeDBInstances, rds:DescribeDBClusters and ec2:DescribeInstances in the "Developer" permission set (see ` + IAMDocsURL + `).`,
		`Ask your AWS admin to allow ssm:DescribeInstanceInformation in the "Ops" permission set (see ` + IAMDocsURL + `).`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Hints =\n%q\nwant\n%q", got, want)
	}
}
