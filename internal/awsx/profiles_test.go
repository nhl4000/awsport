package awsx

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nhl4000/awsport/internal/core"
)

// awsEnv points HOME and the AWS shared files at a temp dir and returns the
// config and credentials paths.
func awsEnv(t *testing.T) (cfgPath, credsPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgPath = filepath.Join(home, ".aws", "config")
	credsPath = filepath.Join(home, ".aws", "credentials")
	t.Setenv("AWS_CONFIG_FILE", cfgPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credsPath)
	return cfgPath, credsPath
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListProfilesClassification(t *testing.T) {
	cfgPath, credsPath := awsEnv(t)
	writeFile(t, cfgPath, `[default]
region = us-east-1

[profile legacy-sso]
sso_start_url = https://d-0000000000.awsapps.com/start#/
sso_region = us-east-1
sso_account_id = 123456789012
sso_role_name = ReadOnly

[profile assumer]
role_arn = arn:aws:iam::210987654321:role/team/Deployer
source_profile = default
region = eu-west-1

[profile inline-keys]
aws_access_key_id = AKIAEXAMPLE
aws_secret_access_key = secret

[profile process]
credential_process = /bin/true

[sso-session example]
sso_start_url = https://d-0000000000.awsapps.com/start
sso_region = us-east-1

[services local]
s3 =
  endpoint_url = http://localhost:9000

`+BlockBegin+`
[profile example-admin]
sso_session = example
sso_account_id = 123456789012
sso_role_name = Admin
region = ca-central-1
output = json
`+BlockEnd+`
`)
	writeFile(t, credsPath, `[default]
aws_access_key_id = AKIAEXAMPLE
aws_secret_access_key = secret

[creds-only]
aws_access_key_id = AKIAEXAMPLE
aws_secret_access_key = secret
`)

	got, err := ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Profile{
		{Name: "assumer", Region: "eu-west-1", Kind: core.AuthAssumeRole, AccountID: "210987654321", RoleName: "Deployer"},
		{Name: "creds-only", Kind: core.AuthStatic},
		{Name: "default", Region: "us-east-1", Kind: core.AuthStatic},
		{Name: "example-admin", Region: "ca-central-1", Kind: core.AuthSSO, AccountID: "123456789012", RoleName: "Admin", SSOSession: "example", Managed: true},
		{Name: "inline-keys", Kind: core.AuthStatic},
		{Name: "legacy-sso", Kind: core.AuthSSO, AccountID: "123456789012", RoleName: "ReadOnly"},
		{Name: "process", Kind: core.AuthUnknown},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListProfiles mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestListProfilesMissingFiles(t *testing.T) {
	awsEnv(t)
	got, err := ListProfiles()
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want empty, nil", got, err)
	}
}

func TestFindSSOSession(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    [3]string
		ok      bool
	}{
		{name: "missing file", ok: false},
		{name: "no session", content: "[default]\nregion = us-east-1\n", ok: false},
		{
			name: "first session wins",
			content: "[profile a]\nsso_session = one\n\n[sso-session one]\nsso_start_url = https://d-0000000000.awsapps.com/start\nsso_region = us-west-2\n\n" +
				"[sso-session two]\nsso_start_url = https://example.com/start\nsso_region = eu-west-1\n",
			want: [3]string{"one", "https://d-0000000000.awsapps.com/start", "us-west-2"},
			ok:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath, _ := awsEnv(t)
			if tt.content != "" {
				writeFile(t, cfgPath, tt.content)
			}
			name, url, region, ok, err := FindSSOSession()
			if err != nil {
				t.Fatal(err)
			}
			if ok != tt.ok || [3]string{name, url, region} != tt.want {
				t.Errorf("got (%q, %q, %q, %v), want (%v, %v)", name, url, region, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestConfigFilePathOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AWS_CONFIG_FILE", "")
	if got, want := ConfigFilePath(), filepath.Join(home, ".aws", "config"); got != want {
		t.Errorf("default path = %q, want %q", got, want)
	}
	t.Setenv("AWS_CONFIG_FILE", "~/custom/config")
	if got, want := ConfigFilePath(), filepath.Join(home, "custom", "config"); got != want {
		t.Errorf("override path = %q, want %q", got, want)
	}
}
