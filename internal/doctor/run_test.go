package doctor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nhl4000/awsport/internal/agent"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/tunnel"
)

func fakeDeps(t *testing.T, profiles []core.Profile) Deps {
	root := t.TempDir()
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	cacheDir := filepath.Join(root, ".aws", "sso", "cache")
	writeToken(t, cacheDir, "t.json", testStartURL, now.Add(8*time.Hour))
	return Deps{
		Version: "v1.0.0", GOOS: "linux", GOARCH: "amd64",
		Now:           func() time.Time { return now },
		Environ:       func() []string { return []string{"AWS_PROFILE=x"} },
		LatestRelease: func(context.Context) (string, error) { return "", errors.New("offline") },
		Tools: func(context.Context) []deps.Check {
			return []deps.Check{{Name: "aws", OK: true, Found: true, Version: "2.15.30"}, {Name: "session-manager-plugin", OK: true, Found: true, Version: "1.2.0"}}
		},
		AWSConfigFile: filepath.Join(root, ".aws", "config"),
		AWSDir:        filepath.Join(root, ".aws"),
		SSOCacheDir:   cacheDir,
		WriteProblem:  func(p string) (string, string) { return p, "chown" },
		ConfigDir:     func() (string, error) { return filepath.Join(root, "cfg"), nil },
		StateDir:      func() (string, error) { return filepath.Join(root, "state"), nil },
		LoadConfig: func() (*config.Config, error) {
			return &config.Config{SSO: config.SSO{StartURL: testStartURL, Region: "us-east-1"}, Regions: []string{"eu-west-1"}}, nil
		},
		SSOSession: func() string { return "" },
		Browser:    func() (string, error) { return "xdg-open", nil },
		Profiles:   func() ([]core.Profile, error) { return profiles, nil },
		Credentials: func(_ context.Context, p core.Profile) error {
			return nil
		},
		Discover: func(_ context.Context, p core.Profile, region string) ([]core.Resource, error) {
			return []core.Resource{{Kind: core.KindEC2, Name: p.Name}}, nil
		},
		Agent: func() (*agent.Info, []tunnel.Session, error) { return nil, nil, nil },
	}
}

func TestRunSectionsAndConcurrency(t *testing.T) {
	var profiles []core.Profile
	for i := range 10 {
		profiles = append(profiles, core.Profile{Name: fmt.Sprintf("p%02d", i), Kind: core.AuthSSO, Managed: true, Region: "us-east-1"})
	}
	profiles[9].Region = "" // falls back to the first configured region
	d := fakeDeps(t, profiles)

	var cur, peak atomic.Int32
	var mu sync.Mutex
	regions := map[string]string{}
	d.Credentials = func(ctx context.Context, p core.Profile) error {
		n := cur.Add(1)
		defer cur.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("profile check has no deadline")
		}
		time.Sleep(10 * time.Millisecond)
		if p.Name == "p03" {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	}
	discover := d.Discover
	d.Discover = func(ctx context.Context, p core.Profile, region string) ([]core.Resource, error) {
		mu.Lock()
		regions[p.Name] = region
		mu.Unlock()
		return discover(ctx, p, region)
	}

	res := Run(context.Background(), d, Options{})
	var names []string
	for _, s := range res.Sections {
		names = append(names, s.Name)
	}
	want := []string{"awsport", "Tools", "Files", "SSO", "Browser", "Profiles"}
	for _, p := range profiles {
		want = append(want, "Profile "+p.Name+" (sso, "+map[bool]string{true: "eu-west-1", false: "us-east-1"}[p.Region == ""]+")")
	}
	want = append(want, "Agent")
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("sections =\n%v\nwant\n%v", names, want)
	}
	if p := peak.Load(); p > profileWorkers || p < 2 {
		t.Errorf("peak concurrency = %d, want 2..%d", p, profileWorkers)
	}
	if regions["p09"] != "eu-west-1" || regions["p00"] != "us-east-1" {
		t.Errorf("regions = %v", regions)
	}
	if _, ok := regions["p03"]; ok {
		t.Error("discovery ran despite failed credentials")
	}
	if res.ExitCode() != 1 || res.Count(Fail) != 1 {
		t.Errorf("exit = %d, failures = %d, want 1 failure (p03):\n%+v", res.ExitCode(), res.Count(Fail), res.Sections)
	}
	if res.Environment.AWSCLI != "2.15.30" || fmt.Sprint(res.Environment.EnvVars) != "[AWS_PROFILE]" {
		t.Errorf("environment = %+v", res.Environment)
	}
}

func TestRunSelectedProfiles(t *testing.T) {
	d := fakeDeps(t, []core.Profile{{Name: "a", Kind: core.AuthStatic, Region: "us-east-1"}, {Name: "b", Kind: core.AuthStatic, Region: "us-east-1"}})
	res := Run(context.Background(), d, Options{Profiles: []string{"b"}})
	var profileSecs []string
	for _, s := range res.Sections {
		if strings.HasPrefix(s.Name, "Profile ") {
			profileSecs = append(profileSecs, s.Name)
		}
	}
	if fmt.Sprint(profileSecs) != "[Profile b (static, us-east-1)]" {
		t.Errorf("profile sections = %v", profileSecs)
	}
	if res.ExitCode() != 0 {
		t.Errorf("exit = %d, want 0:\n%+v", res.ExitCode(), res.Sections)
	}
}
