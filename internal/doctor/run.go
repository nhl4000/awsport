package doctor

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nhl4000/awsport/internal/agent"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/tunnel"
)

const (
	// releaseTimeout bounds the latest-release lookup.
	releaseTimeout = 5 * time.Second
	// profileTimeout bounds the credential and permission checks of one profile.
	profileTimeout = 20 * time.Second
	// profileWorkers is how many profiles are checked at once.
	profileWorkers = 4
)

// Deps are the doctor's inputs and side-effecting operations. System
// returns the real ones; tests substitute fakes.
type Deps struct {
	Version, Commit string
	GOOS, GOARCH    string
	WSL             bool
	Now             func() time.Time
	Environ         func() []string

	// LatestRelease returns the newest release tag; errors (offline,
	// rate-limited) skip the check silently.
	LatestRelease func(ctx context.Context) (string, error)
	Tools         func(ctx context.Context) []deps.Check

	AWSConfigFile    string // AWS shared config file
	AWSConfigFromEnv bool   // AWSConfigFile comes from $AWS_CONFIG_FILE
	AWSDir           string // ~/.aws
	SSOCacheDir      string // ~/.aws/sso/cache
	// WriteProblem explains how to make a path under ~/.aws writable.
	WriteProblem func(path string) (where, fix string)
	ConfigDir    func() (string, error) // awsport config directory
	StateDir     func() (string, error) // awsport state directory

	LoadConfig func() (*config.Config, error)
	ConfigPath string        // awsport config.yaml, for messages
	SSOSession func() string // first [sso-session] in the AWS config file, "" if none

	Browser func() (string, error)

	Profiles    func() ([]core.Profile, error)
	Credentials func(ctx context.Context, p core.Profile) error
	// Discover lists the resources of p in one region; denied calls come
	// back as *awsx.PermissionError inside the error.
	Discover func(ctx context.Context, p core.Profile, region string) ([]core.Resource, error)

	// Agent queries the background agent; info is nil when none runs.
	Agent    func() (*agent.Info, []tunnel.Session, error)
	AgentLog string
}

// Options select what Run checks.
type Options struct {
	// Profiles limits the per-profile checks to these names (all when empty).
	Profiles []string
}

// Run executes every check, independent groups concurrently and profiles
// profileWorkers at a time, and returns the sections in a fixed order.
func Run(ctx context.Context, d Deps, opts Options) Result {
	now := d.Now()
	cfg, cfgErr := d.LoadConfig()

	var (
		wg                                      sync.WaitGroup
		latest                                  string
		tools                                   []deps.Check
		files, sso, browser, agentRes, profSumm []Check
		profSecs                                []Section
	)
	run := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
		}()
	}
	run(func() {
		rctx, cancel := context.WithTimeout(ctx, releaseTimeout)
		defer cancel()
		if tag, err := d.LatestRelease(rctx); err == nil {
			latest = tag
		}
	})
	run(func() { tools = d.Tools(ctx) })
	run(func() { files = fileChecks(d) })
	run(func() {
		nonSSO := 0
		if ps, err := d.Profiles(); err == nil {
			for _, p := range ps {
				if p.Kind != core.AuthSSO {
					nonSSO++
				}
			}
		}
		sso = ssoChecks(ssoInputs{
			cfg: cfg, cfgErr: cfgErr, cfgPath: d.ConfigPath, awsSession: d.SSOSession(), nonSSO: nonSSO,
			cacheDir: d.SSOCacheDir, now: now, writeAdvice: d.WriteProblem,
		})
	})
	run(func() { browser = []Check{browserCheck(d.Browser())} })
	run(func() {
		info, sessions, err := d.Agent()
		agentRes = agentChecks(info, sessions, err, d.Version, d.AgentLog, now)
	})
	run(func() { profSumm, profSecs = profileChecks(ctx, d, cfg, opts) })
	wg.Wait()

	res := Result{
		Generated: now,
		Environment: Environment{
			Version: d.Version, Commit: d.Commit, OS: d.GOOS, Arch: d.GOARCH, WSL: d.WSL,
			EnvVars: envNames(d.Environ()),
		},
	}
	for _, t := range tools {
		switch t.Name {
		case "aws":
			res.Environment.AWSCLI = t.Version
		case "session-manager-plugin":
			res.Environment.Plugin = t.Version
		}
	}
	res.Sections = append(res.Sections,
		Section{Name: "awsport", Checks: versionChecks(d.Version, d.GOOS, d.GOARCH, latest)},
		Section{Name: "Tools", Checks: toolChecks(tools)},
		Section{Name: "Files", Checks: files},
		Section{Name: "SSO", Checks: sso},
		Section{Name: "Browser", Checks: browser},
		Section{Name: "Profiles", Checks: profSumm},
	)
	res.Sections = append(res.Sections, profSecs...)
	res.Sections = append(res.Sections, Section{Name: "Agent", Checks: agentRes})
	return res
}

func fileChecks(d Deps) []Check {
	out := []Check{
		configFileCheck(d.AWSConfigFile, d.AWSConfigFromEnv),
		writableCheck(d.AWSDir, d.WriteProblem),
		writableCheck(d.SSOCacheDir, d.WriteProblem),
	}
	for _, dir := range []struct {
		resolve func() (string, error)
		env     string
		what    string
	}{
		{d.ConfigDir, config.EnvConfigDir, "awsport config directory"},
		{d.StateDir, config.EnvStateDir, "awsport state directory"},
	} {
		p, err := dir.resolve()
		if err != nil {
			out = append(out, Check{Status: Fail, Title: "can't locate the " + dir.what, Detail: shortErr(err),
				Fix: "set " + dir.env + " to a writable directory"})
			continue
		}
		out = append(out, writableCheck(p, awsportDirFix(dir.env)))
	}
	return out
}

// profileChecks lists the profiles and checks each selected one, at most
// profileWorkers at a time with profileTimeout each, in name order.
func profileChecks(ctx context.Context, d Deps, cfg *config.Config, opts Options) ([]Check, []Section) {
	all, err := d.Profiles()
	summary, selected := profileSummary(all, err, opts.Profiles, ssoConfigured(cfg))
	session := config.DefaultSSOSession
	var regions []string
	if cfg != nil {
		regions = cfg.Regions
		if cfg.SSO.Session != "" {
			session = cfg.SSO.Session
		}
	}
	secs := make([]Section, len(selected))
	sem := make(chan struct{}, profileWorkers)
	var wg sync.WaitGroup
	for i, p := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, profileTimeout)
			defer cancel()
			t := profileTarget{profile: p, region: strings.TrimSpace(p.Region), ssoSession: session}
			if t.region == "" && len(regions) > 0 {
				t.region = regions[0]
			}
			secs[i] = profileSection(pctx, t, d.Credentials, d.Discover)
		}()
	}
	wg.Wait()
	return summary, secs
}

// envNames returns the sorted names of set AWS_* and AWSPORT_* variables.
func envNames(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, val, _ := strings.Cut(kv, "=")
		if val != "" && (strings.HasPrefix(name, "AWS_") || strings.HasPrefix(name, "AWSPORT_")) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
