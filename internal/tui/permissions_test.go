package tui

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/tunnel"
)

const devPrincipal = "arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Developer_0123abcd/jane"

func denied(action, what, region string) *awsx.PermissionError {
	return &awsx.PermissionError{Profile: "example-dev", Region: region, Action: action, What: what, Principal: devPrincipal, Err: errors.New("AccessDenied")}
}

func TestPermissionLines(t *testing.T) {
	// Discovery across two regions: DescribeDBInstances denied in both.
	err := fmt.Errorf("discover example-dev: %w", errors.Join(
		errors.Join(nil, denied("rds:DescribeDBInstances", "list RDS databases", "ca-central-1")),
		errors.Join(nil, denied("rds:DescribeDBInstances", "list RDS databases", "us-east-1"),
			denied("rds:DescribeDBClusters", "list Aurora clusters", "us-east-1")),
	))
	warnings, _ := permissionLines(awsx.PermissionErrors(err))
	wantWarnings := []string{
		"⚠ Can't list RDS databases in ca-central-1, us-east-1 — missing rds:DescribeDBInstances",
		"⚠ Can't list Aurora clusters in us-east-1 — missing rds:DescribeDBClusters",
	}
	if !reflect.DeepEqual(warnings, wantWarnings) {
		t.Errorf("warnings = %q\nwant %q", warnings, wantWarnings)
	}
	if got := missingActions(awsx.PermissionErrors(err)); got != "missing rds:DescribeDBInstances, rds:DescribeDBClusters" {
		t.Errorf("missingActions = %q", got)
	}
}

func TestPermissionLinesGlobalCall(t *testing.T) {
	warnings, _ := permissionLines([]*awsx.PermissionError{
		denied("ec2:DescribeInstances", "list EC2 instances", ""),
		denied("ec2:DescribeInstances", "list EC2 instances", ""),
	})
	if want := []string{"⚠ Can't list EC2 instances — missing ec2:DescribeInstances"}; !reflect.DeepEqual(warnings, want) {
		t.Errorf("warnings = %q; want %q", warnings, want)
	}
}

func TestFailureText(t *testing.T) {
	long := "not allowed to start an SSM session on i-0123456789abcdef0 (missing ssm:StartSession). Ask your AWS admin to allow it — see https://github.com/nhl4000/awsport#iam-permissions"
	if got := failureText(tunnel.Session{Status: tunnel.Failed, Err: long}); got != "failed: "+long {
		t.Errorf("failureText = %q", got)
	}
	if got := failureText(tunnel.Session{Status: tunnel.Reconnecting, Err: "x"}); got != "" {
		t.Errorf("non-failed session: %q", got)
	}
}
