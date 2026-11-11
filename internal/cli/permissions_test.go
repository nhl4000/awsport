package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nhl4000/awsport/internal/awsx"
)

func TestReportStartError(t *testing.T) {
	perm := &awsx.PermissionError{
		Profile: "example-dev", Region: "ca-central-1", Action: "rds:DescribeDBInstances", What: "list RDS databases",
		Principal: "arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Developer_0123abcd/jane", Err: errors.New("AccessDenied"),
	}

	var b bytes.Buffer
	reportStartError(&b, "example-dev/db: ", "example-dev", fmt.Errorf("discover example-dev: %w", perm))
	got := b.String()
	if !strings.HasPrefix(got, "⚠ example-dev: Can't list RDS databases in ca-central-1 — missing rds:DescribeDBInstances\n  Ask your AWS admin") {
		t.Fatalf("permission-only error:\n%s", got)
	}
	if strings.Contains(got, "error:") {
		t.Fatalf("permission-only error printed as an error:\n%s", got)
	}

	b.Reset()
	reportStartError(&b, "example-dev/db: ", "example-dev", errors.Join(perm, errors.New("dial tcp: i/o timeout")))
	got = b.String()
	if !strings.HasPrefix(got, "error: example-dev/db: dial tcp: i/o timeout\n⚠ example-dev: Can't list RDS databases") {
		t.Fatalf("mixed error:\n%s", got)
	}
}
