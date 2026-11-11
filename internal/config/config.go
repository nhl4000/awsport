// Package config loads and saves awsport's user and team configuration and
// resolves its on-disk locations.
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nhl4000/awsport/internal/core"
)

// DefaultSSOSession is the sso-session name used when SSO.Session is empty.
const DefaultSSOSession = "awsport"

// DefaultMaxFailures is used when MaxFailures is unset.
const DefaultMaxFailures = 5

// maxTeamConfigSize bounds a downloaded or read team config.
const maxTeamConfigSize = 1 << 20

// teamFetchTimeout bounds fetching a team config over HTTP.
const teamFetchTimeout = 15 * time.Second

// SSO holds IAM Identity Center settings.
type SSO struct {
	StartURL string `yaml:"start_url,omitempty"`
	Region   string `yaml:"region,omitempty"`
	Session  string `yaml:"session,omitempty"` // sso-session name to write into ~/.aws/config; default "awsport"
}

// Config is the user or team configuration (and the merged effective view).
type Config struct {
	SSO          SSO             `yaml:"sso,omitempty"`
	Regions      []string        `yaml:"regions,omitempty"` // extra regions to scan
	Presets      []core.PortSpec `yaml:"presets,omitempty"` // EC2 port presets
	Groups       []core.Group    `yaml:"groups,omitempty"`
	ShellCommand string          `yaml:"shell_command,omitempty"`
	Reconnect    *bool           `yaml:"reconnect,omitempty"`    // default true
	MaxFailures  int             `yaml:"max_failures,omitempty"` // default 5
	TeamSource   string          `yaml:"team_source,omitempty"`  // URL/path last imported (for re-import)
}

// DefaultPresets are the built-in EC2 port presets.
var DefaultPresets = []core.PortSpec{{Name: "mailpit", Remote: 8025}, {Name: "http", Remote: 80}, {Name: "https", Remote: 443}, {Name: "app", Remote: 8080}, {Name: "node", Remote: 3000}}

// Load returns the effective config: defaults <- team.yaml <- config.yaml.
// Scalars from later layers win when set; Regions from the user replace the
// team's when non-empty; Presets merge by Remote and Groups by Name, with
// later layers winning. Missing files are not an error.
func Load() (*Config, error) {
	teamPath, err := TeamConfigPath()
	if err != nil {
		return nil, err
	}
	team, err := readFile(teamPath)
	if err != nil {
		return nil, err
	}
	user, err := LoadUser()
	if err != nil {
		return nil, err
	}
	eff := &Config{Presets: append([]core.PortSpec(nil), DefaultPresets...)}
	if team != nil {
		team.TeamSource = ""
		eff = merge(eff, team)
	}
	return merge(eff, user), nil
}

// LoadUser returns only config.yaml (empty config if missing), for editing.
func LoadUser() (*Config, error) {
	p, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	c, err := readFile(p)
	if err != nil {
		return nil, err
	}
	if c == nil {
		c = &Config{}
	}
	return c, nil
}

// SaveUser atomically writes c to config.yaml with mode 0600.
func SaveUser(c *Config) error {
	p, err := ConfigPath()
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return WriteFileAtomic(p, data)
}

// ImportTeam fetches a team config from an http(s) URL or reads it from a
// file path, validates it strictly (unknown keys rejected), stores it as
// team.yaml and records src as TeamSource in the user config.
func ImportTeam(ctx context.Context, src string) (*Config, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return nil, errors.New("import team config: empty source")
	}
	var (
		data   []byte
		err    error
		source = src
	)
	if isURL(src) {
		data, err = fetch(ctx, src)
	} else {
		if abs, aerr := filepath.Abs(src); aerr == nil {
			source = abs
		}
		data, err = readLimited(source)
	}
	if err != nil {
		return nil, fmt.Errorf("import team config from %s: %w", src, err)
	}
	team, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("import team config from %s: %w", src, err)
	}
	if team == nil {
		return nil, fmt.Errorf("import team config from %s: document is empty", src)
	}
	teamPath, err := TeamConfigPath()
	if err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(teamPath, data); err != nil {
		return nil, fmt.Errorf("save team config: %w", err)
	}
	user, err := LoadUser()
	if err != nil {
		return nil, err
	}
	user.TeamSource = source
	if err := SaveUser(user); err != nil {
		return nil, fmt.Errorf("record team source: %w", err)
	}
	return team, nil
}

// ReconnectEnabled reports whether dropped tunnels reconnect (default true).
func (c *Config) ReconnectEnabled() bool {
	return c.Reconnect == nil || *c.Reconnect
}

// EffectiveShell returns ShellCommand or core.DefaultShellCommand.
func (c *Config) EffectiveShell() string {
	if c.ShellCommand != "" {
		return c.ShellCommand
	}
	return core.DefaultShellCommand
}

// MaxFail returns MaxFailures or DefaultMaxFailures when unset.
func (c *Config) MaxFail() int {
	if c.MaxFailures > 0 {
		return c.MaxFailures
	}
	return DefaultMaxFailures
}

// merge returns base overlaid with over (over wins).
func merge(base, over *Config) *Config {
	out := *base
	if over.SSO.StartURL != "" {
		out.SSO.StartURL = over.SSO.StartURL
	}
	if over.SSO.Region != "" {
		out.SSO.Region = over.SSO.Region
	}
	if over.SSO.Session != "" {
		out.SSO.Session = over.SSO.Session
	}
	if len(over.Regions) > 0 {
		out.Regions = append([]string(nil), over.Regions...)
	}
	out.Presets = mergePresets(base.Presets, over.Presets)
	out.Groups = mergeGroups(base.Groups, over.Groups)
	if over.ShellCommand != "" {
		out.ShellCommand = over.ShellCommand
	}
	if over.Reconnect != nil {
		v := *over.Reconnect
		out.Reconnect = &v
	}
	if over.MaxFailures != 0 {
		out.MaxFailures = over.MaxFailures
	}
	if over.TeamSource != "" {
		out.TeamSource = over.TeamSource
	}
	return &out
}

// mergePresets keeps base order, replacing entries whose Remote appears in
// over and appending new ones.
func mergePresets(base, over []core.PortSpec) []core.PortSpec {
	out := append([]core.PortSpec(nil), base...)
	idx := make(map[int]int, len(out))
	for i, p := range out {
		idx[p.Remote] = i
	}
	for _, p := range over {
		if i, ok := idx[p.Remote]; ok {
			out[i] = p
			continue
		}
		idx[p.Remote] = len(out)
		out = append(out, p)
	}
	return out
}

// mergeGroups keeps base order, replacing groups whose Name appears in over
// and appending new ones.
func mergeGroups(base, over []core.Group) []core.Group {
	out := make([]core.Group, 0, len(base)+len(over))
	idx := make(map[string]int, len(base)+len(over))
	add := func(g core.Group) {
		g.Members = append([]core.GroupMember(nil), g.Members...)
		if i, ok := idx[g.Name]; ok {
			out[i] = g
			return
		}
		idx[g.Name] = len(out)
		out = append(out, g)
	}
	for _, g := range base {
		add(g)
	}
	for _, g := range over {
		add(g)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// readFile parses path; a missing file yields (nil, nil).
func readFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	c, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// parse strictly decodes a single YAML document; an empty document yields
// (nil, nil).
func parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, fmt.Errorf("invalid awsport config YAML: %w", err)
	}
	return &c, nil
}

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func fetch(ctx context.Context, src string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, teamFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	client := &http.Client{Timeout: teamFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch: unexpected status %s", resp.Status)
	}
	return readAllLimited(resp.Body)
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	return readAllLimited(f)
}

func readAllLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxTeamConfigSize+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(data) > maxTeamConfigSize {
		return nil, fmt.Errorf("read: exceeds %d bytes", maxTeamConfigSize)
	}
	return data, nil
}
