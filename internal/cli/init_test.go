package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
)

func setInitDirs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, filepath.Join(dir, "config"))
	t.Setenv(config.EnvStateDir, filepath.Join(dir, "state"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "aws", "config"))
	return dir
}

func runInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newInitCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

const teamV1 = `regions: [us-east-2, us-west-2]
presets:
  - {name: web, remote: 80}
  - {name: grafana, remote: 3001}
groups:
  - name: dev
    members: [{profile: example-dev, name: app, kind: ec2, remote: 8080}]
  - name: data
    members: [{profile: example-dev, name: db, kind: rds, remote: 5432}]
`

const teamV2 = `regions: [us-east-2]
presets:
  - {name: web, remote: 80}
  - {name: grafana, remote: 3001}
groups:
  - name: dev
    members: [{profile: example-dev, name: app, kind: ec2, remote: 8080}]
  - name: ops
    members: [{profile: example-dev, name: bastion, kind: ec2, remote: 22}]
`

func TestInitRefresh(t *testing.T) {
	dir := setInitDirs(t)
	src := filepath.Join(dir, "awsport.team.yaml")
	if err := os.WriteFile(src, []byte(teamV1), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runInit(t, "--from", src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Imported team config from "+src+" (2 groups, 2 regions, 2 presets)") {
		t.Fatalf("import output:\n%s", out)
	}

	if err := os.WriteFile(src, []byte(teamV2), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runInit(t, "--refresh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Re-imported team config from " + src,
		"groups   2 → 2  (+ops, -data)",
		"regions  2 → 1  (-us-west-2)",
		"presets  2 → 2\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refresh output missing %q:\n%s", want, out)
		}
	}
	team, err := config.LoadTeam()
	if err != nil || len(team.Regions) != 1 {
		t.Fatalf("team.yaml after refresh = %+v, %v", team, err)
	}

	// No flags re-imports from the recorded source as well.
	out, err = runInit(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Re-imported team config from "+src) || !strings.Contains(out, "no changes (2 groups, 1 regions, 2 presets)") {
		t.Fatalf("no-flag output:\n%s", out)
	}
}

func TestInitRefreshErrors(t *testing.T) {
	setInitDirs(t)
	if _, err := runInit(t, "--refresh"); err == nil || !strings.Contains(err.Error(), "no team config to refresh") {
		t.Fatalf("refresh without source: %v", err)
	}
	if _, err := runInit(t, "--refresh", "--from", "x.yaml"); err == nil {
		t.Fatal("--from with --refresh accepted")
	}
	// A failed re-import leaves the previous team.yaml in place.
	dir := setInitDirs(t)
	src := filepath.Join(dir, "awsport.team.yaml")
	if err := os.WriteFile(src, []byte(teamV1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runInit(t, "--from", src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("bogus_key: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runInit(t, "--refresh"); err == nil || !strings.Contains(err.Error(), "invalid awsport config YAML") {
		t.Fatalf("refresh of invalid source: %v", err)
	}
	if team, err := config.LoadTeam(); err != nil || len(team.Groups) != 2 {
		t.Fatalf("team.yaml after failed refresh = %+v, %v", team, err)
	}
}

func TestWriteTeamDiff(t *testing.T) {
	base := &config.Config{
		Regions: []string{"us-east-1"},
		Presets: []core.PortSpec{{Name: "web", Remote: 80}},
		Groups:  []core.Group{{Name: "dev"}},
	}
	tests := []struct {
		name          string
		before, after *config.Config
		want          []string
	}{
		{"unchanged", base, base, []string{"no changes (1 groups, 1 regions, 1 presets)"}},
		{"first import", &config.Config{}, base, []string{"groups   0 → 1  (+dev)", "regions  0 → 1  (+us-east-1)", "presets  0 → 1  (+web:80)"}},
		{"preset port changed", base, &config.Config{Regions: base.Regions, Groups: base.Groups, Presets: []core.PortSpec{{Name: "web", Remote: 8080}}},
			[]string{"presets  1 → 1  (+web:8080, -web:80)"}},
		{"members only", base, &config.Config{Regions: base.Regions, Presets: base.Presets,
			Groups: []core.Group{{Name: "dev", Members: []core.GroupMember{{Profile: "example-dev", Name: "app"}}}}},
			[]string{"groups   1 → 1\n", "other settings changed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			writeTeamDiff(&out, tt.before, tt.after)
			for _, w := range tt.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output missing %q:\n%s", w, out.String())
				}
			}
		})
	}
}
