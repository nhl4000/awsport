package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/nhl4000/awsport/internal/agent"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/login"
	"github.com/nhl4000/awsport/internal/open"
)

// latestReleaseURL is the GitHub API endpoint of awsport's newest release.
const latestReleaseURL = "https://api.github.com/repos/nhl4000/awsport/releases/latest"

// System returns the Deps of this machine for the awsport binary of
// version/commit.
func System(version, commit string) Deps {
	cacheDir := awsx.SSOCacheDir()
	disc := awsx.NewDiscoverer()
	configPath, _ := config.ConfigPath()
	logPath, _ := agent.LogPath()
	return Deps{
		Version: version, Commit: commit,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, WSL: deps.IsWSL(),
		Now:     time.Now,
		Environ: os.Environ,
		LatestRelease: func(ctx context.Context) (string, error) {
			return latestRelease(ctx, http.DefaultClient, latestReleaseURL, os.Getenv("GITHUB_TOKEN"))
		},
		Tools:            deps.CheckAll,
		AWSConfigFile:    awsx.ConfigFilePath(),
		AWSConfigFromEnv: os.Getenv("AWS_CONFIG_FILE") != "",
		AWSDir:           filepath.Dir(filepath.Dir(cacheDir)),
		SSOCacheDir:      cacheDir,
		WriteProblem:     login.WriteProblem,
		ConfigDir:        config.ConfigDir,
		StateDir:         config.StateDir,
		LoadConfig:       config.Load,
		ConfigPath:       configPath,
		SSOSession: func() string {
			name, _, _, ok, err := awsx.FindSSOSession()
			if err != nil || !ok {
				return ""
			}
			return name
		},
		Browser:     open.Check,
		Profiles:    awsx.ListProfiles,
		Credentials: disc.CheckCredentials,
		Discover: func(ctx context.Context, p core.Profile, region string) ([]core.Resource, error) {
			return disc.Discover(ctx, p, []string{region}, nil, "", nil)
		},
		Agent:    agent.Inspect,
		AgentLog: logPath,
	}
}

// latestRelease returns the tag of the latest GitHub release at url.
func latestRelease(ctx context.Context, client *http.Client, url, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "awsport-doctor")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", fmt.Errorf("parse release: %w", err)
	}
	if rel.TagName == "" {
		return "", errors.New("latest release has no tag")
	}
	return rel.TagName, nil
}
