package awsx

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nhl4000/awsport/internal/config"
)

var testSSO = config.SSO{StartURL: "https://d-0000000000.awsapps.com/start", Region: "us-east-1", Session: "example"}

var testProfiles = []SSOProfile{
	{Name: "sandbox-admin", AccountID: "210987654321", AccountName: "Sandbox", RoleName: "Admin", Region: "ca-central-1"},
	{Name: "example-readonly", AccountID: "123456789012", AccountName: "Example", RoleName: "ReadOnly"},
}

const wantBlock = BlockBegin + `
[sso-session example]
sso_start_url = https://d-0000000000.awsapps.com/start
sso_region = us-east-1
sso_registration_scopes = sso:account:access

[profile example-readonly]
sso_session = example
sso_account_id = 123456789012
sso_role_name = ReadOnly
region = us-east-1
output = json

[profile sandbox-admin]
sso_session = example
sso_account_id = 210987654321
sso_role_name = Admin
region = ca-central-1
output = json
` + BlockEnd + "\n"

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteManagedBlock(t *testing.T) {
	tests := []struct {
		name     string
		original *string // nil = file absent
		want     string
	}{
		{name: "missing file", want: wantBlock},
		{
			name:     "append with leading blank line",
			original: new("[default]\nregion = us-east-1 ; keep me"),
			want:     "[default]\nregion = us-east-1 ; keep me\n\n" + wantBlock,
		},
		{
			name: "replace preserves outside bytes",
			original: new("# top comment\n[default]\nregion   =   us-east-1\n\n" + BlockBegin + "\n[profile stale]\nsso_session = example\n" +
				BlockEnd + "\n\n[profile after]\nregion=eu-west-1\n"),
			want: "# top comment\n[default]\nregion   =   us-east-1\n\n" + wantBlock + "\n[profile after]\nregion=eu-west-1\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "aws", "config")
			if tt.original != nil {
				writeFile(t, path, *tt.original)
			}
			if err := WriteManagedBlock(path, testSSO, testProfiles); err != nil {
				t.Fatal(err)
			}
			if got := readString(t, path); got != tt.want {
				t.Errorf("content mismatch\n got:\n%s\nwant:\n%s", got, tt.want)
			}
			bak := path + ".awsport.bak"
			if tt.original != nil {
				if got := readString(t, bak); got != *tt.original {
					t.Errorf("backup = %q, want original %q", got, *tt.original)
				}
			} else if _, err := os.Stat(bak); !os.IsNotExist(err) {
				t.Errorf("unexpected backup for new file: %v", err)
			}
			if runtime.GOOS != "windows" {
				st, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if st.Mode().Perm() != 0o600 {
					t.Errorf("mode = %v, want 0600", st.Mode().Perm())
				}
			}

			// Idempotent: a second write with the same input changes nothing.
			if err := WriteManagedBlock(path, testSSO, testProfiles); err != nil {
				t.Fatal(err)
			}
			if got := readString(t, path); got != tt.want {
				t.Errorf("second write changed content\n got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestWriteManagedBlockSkipsSectionsDefinedOutside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	outside := "[sso-session example]\nsso_start_url = https://d-0000000000.awsapps.com/start\nsso_region = us-east-1\n\n" +
		"[profile sandbox-admin]\nregion = us-west-2\n"
	writeFile(t, path, outside)
	if err := WriteManagedBlock(path, testSSO, testProfiles); err != nil {
		t.Fatal(err)
	}
	got := readString(t, path)
	if !strings.HasPrefix(got, outside) {
		t.Fatalf("outside bytes changed:\n%s", got)
	}
	if n := strings.Count(got, "[sso-session example]"); n != 1 {
		t.Errorf("sso-session sections = %d, want 1", n)
	}
	if n := strings.Count(got, "[profile sandbox-admin]"); n != 1 {
		t.Errorf("sandbox-admin sections = %d, want 1", n)
	}
	if !strings.Contains(got, "[profile example-readonly]\nsso_session = example\n") {
		t.Errorf("managed profile missing:\n%s", got)
	}
}

func TestWriteManagedBlockErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeFile(t, path, "[default]\n"+BlockBegin+"\n[profile x]\n")
	if err := WriteManagedBlock(path, testSSO, testProfiles); err == nil {
		t.Error("expected error for unterminated block")
	}
	if err := WriteManagedBlock(path, config.SSO{Region: "us-east-1"}, nil); err == nil {
		t.Error("expected error for missing start URL")
	}
	bad := []SSOProfile{{Name: "x]\n[evil", AccountID: "1", RoleName: "r"}}
	if err := WriteManagedBlock(path, testSSO, bad); err == nil {
		t.Error("expected error for injected section")
	}
}

func TestWriteManagedBlockMarksProfilesManaged(t *testing.T) {
	cfgPath, _ := awsEnv(t)
	writeFile(t, cfgPath, "[profile mine]\nregion = us-east-1\n")
	if err := WriteManagedBlock(cfgPath, testSSO, testProfiles); err != nil {
		t.Fatal(err)
	}
	profiles, err := ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	managed := map[string]bool{}
	for _, p := range profiles {
		managed[p.Name] = p.Managed
	}
	want := map[string]bool{"mine": false, "example-readonly": true, "sandbox-admin": true}
	if len(managed) != len(want) {
		t.Fatalf("profiles = %v, want %v", managed, want)
	}
	for name, m := range want {
		if managed[name] != m {
			t.Errorf("%s managed = %v, want %v", name, managed[name], m)
		}
	}
}

func TestProfileName(t *testing.T) {
	tests := []struct{ account, role, want string }{
		{"Example Production", "AdministratorAccess", "example-production-administratoraccess"},
		{"example_prod", "Read-Only", "example-prod-read-only"},
		{"  --Example--  ", "Admin!!", "example-admin"},
		{"Ünïcode Acct", "Dev", "n-code-acct-dev"},
		{"", "Admin", "admin"},
		{"Example", "", "example"},
	}
	for _, tt := range tests {
		if got := ProfileName(tt.account, tt.role); got != tt.want {
			t.Errorf("ProfileName(%q, %q) = %q, want %q", tt.account, tt.role, got, tt.want)
		}
	}
}
