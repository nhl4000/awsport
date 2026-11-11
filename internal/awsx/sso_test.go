package awsx

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ssoapi "github.com/aws/aws-sdk-go-v2/service/sso"
	ssotypes "github.com/aws/aws-sdk-go-v2/service/sso/types"
	"github.com/aws/smithy-go"

	"github.com/nhl4000/awsport/internal/config"
)

func TestCachedSSOToken(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	const url = "https://d-0000000000.awsapps.com/start"
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{name: "no cache dir"},
		{
			name: "expired and mismatched skipped, longest valid wins",
			files: map[string]string{
				"a.json":           `{"startUrl":"` + url + `","accessToken":"expired","expiresAt":"2026-01-02T11:00:00Z"}`,
				"b.json":           `{"startUrl":"https://example.com/start","accessToken":"other","expiresAt":"2026-01-03T00:00:00Z"}`,
				"c.json":           `{"startUrl":"` + url + `/","accessToken":"short","expiresAt":"2026-01-02T13:00:00Z"}`,
				"d.json":           `{"startUrl":"` + url + `#","accessToken":"long","expiresAt":"2026-01-02T20:00:00UTC"}`,
				"botocore-id.json": `{"clientId":"x","clientSecret":"y"}`,
				"broken.json":      `{`,
			},
			want: "long",
		},
		{
			name:  "about to expire",
			files: map[string]string{"a.json": `{"startUrl":"` + url + `","accessToken":"t","expiresAt":"2026-01-02T12:00:30Z"}`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			for name, content := range tt.files {
				writeFile(t, filepath.Join(dir, name), content)
			}
			got, err := cachedSSOToken(dir, url, now)
			if tt.want == "" {
				if !errors.Is(err, ErrSSOTokenExpired) {
					t.Fatalf("err = %v, want ErrSSOTokenExpired", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got (%q, %v), want %q", got, err, tt.want)
			}
		})
	}
}

func TestListSSOAccessNeedsToken(t *testing.T) {
	awsEnv(t)
	_, err := ListSSOAccess(context.Background(), config.SSO{StartURL: "https://d-0000000000.awsapps.com/start", Region: "us-east-1"})
	if !errors.Is(err, ErrSSOTokenExpired) {
		t.Fatalf("err = %v, want ErrSSOTokenExpired", err)
	}
}

type fakeSSO struct {
	accounts [][]ssotypes.AccountInfo         // pages
	roles    map[string][][]ssotypes.RoleInfo // account -> pages
	err      error
}

func (f *fakeSSO) ListAccounts(_ context.Context, in *ssoapi.ListAccountsInput, _ ...func(*ssoapi.Options)) (*ssoapi.ListAccountsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	i := pageIndex(in.NextToken)
	return &ssoapi.ListAccountsOutput{AccountList: f.accounts[i], NextToken: nextToken(i, len(f.accounts))}, nil
}

func (f *fakeSSO) ListAccountRoles(_ context.Context, in *ssoapi.ListAccountRolesInput, _ ...func(*ssoapi.Options)) (*ssoapi.ListAccountRolesOutput, error) {
	pages := f.roles[aws.ToString(in.AccountId)]
	if len(pages) == 0 {
		return &ssoapi.ListAccountRolesOutput{}, nil
	}
	i := pageIndex(in.NextToken)
	return &ssoapi.ListAccountRolesOutput{RoleList: pages[i], NextToken: nextToken(i, len(pages))}, nil
}

func TestListSSOAccessPaginatesAndNames(t *testing.T) {
	acct := func(id, name string) ssotypes.AccountInfo {
		return ssotypes.AccountInfo{AccountId: aws.String(id), AccountName: aws.String(name)}
	}
	role := func(name string) ssotypes.RoleInfo { return ssotypes.RoleInfo{RoleName: aws.String(name)} }
	f := &fakeSSO{
		accounts: [][]ssotypes.AccountInfo{
			{acct("111111111111", "Example Prod")},
			{acct("222222222222", "Example-Prod"), acct("333333333333", "Sandbox")},
		},
		roles: map[string][][]ssotypes.RoleInfo{
			"111111111111": {{role("Admin")}, {role("ReadOnly")}},
			"222222222222": {{role("Admin")}},
			"333333333333": {{role("Dev")}},
		},
	}
	got, err := listSSOAccess(context.Background(), f, "token", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
		if p.Region != "us-east-1" {
			t.Errorf("%s region = %q", p.Name, p.Region)
		}
	}
	want := []string{
		"example-prod-111111111111-admin",
		"example-prod-222222222222-admin",
		"example-prod-readonly",
		"sandbox-dev",
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestListSSOAccessUnauthorized(t *testing.T) {
	f := &fakeSSO{err: &smithy.GenericAPIError{Code: "UnauthorizedException", Message: "Session token not found or invalid"}}
	_, err := listSSOAccess(context.Background(), f, "token", "us-east-1")
	if !errors.Is(err, ErrSSOTokenExpired) {
		t.Fatalf("err = %v, want ErrSSOTokenExpired", err)
	}
}
