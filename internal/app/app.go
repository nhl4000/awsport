// Package app orchestrates config, state, discovery and tunnels. It is the
// single entry point shared by the TUI and the CLI.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/ports"
	"github.com/nhl4000/awsport/internal/state"
	"github.com/nhl4000/awsport/internal/tunnel"
	"gopkg.in/ini.v1"
)

// discoveryTTL bounds how long a discovery result is reused for bastion
// resolution and group starts.
const discoveryTTL = 2 * time.Minute

// defaultSSOSession is the sso-session name used when the config sets none.
const defaultSSOSession = "awsport"

// App wires together everything a front end needs.
type App struct {
	Cfg     *config.Config
	State   *state.Store
	Tunnels *tunnel.Manager
	AWS     *awsx.Discoverer
	AWSPath string

	cacheMu sync.Mutex
	cache   map[string]cacheEntry // "profile/region" -> resources

	connectMu sync.Mutex // serializes port assignment + start
}

type cacheEntry struct {
	at        time.Time
	resources []core.Resource
}

// New loads config and state and builds the tunnel manager from config.
func New() (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	st, err := state.Open()
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	awsPath := "aws"
	if p, err := exec.LookPath("aws"); err == nil {
		awsPath = p
	}
	mgr := tunnel.NewManager(tunnel.Options{
		AWSPath:     awsPath,
		Reconnect:   cfg.ReconnectEnabled(),
		MaxFailures: cfg.MaxFail(),
	})
	return &App{
		Cfg:     cfg,
		State:   st,
		Tunnels: mgr,
		AWS:     awsx.NewDiscoverer(),
		AWSPath: awsPath,
		cache:   map[string]cacheEntry{},
	}, nil
}

// Profiles lists the profiles in the AWS config file.
func (a *App) Profiles() ([]core.Profile, error) {
	ps, err := awsx.ListProfiles()
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	return ps, nil
}

// Profile looks up a profile by name.
func (a *App) Profile(name string) (core.Profile, error) {
	ps, err := a.Profiles()
	if err != nil {
		return core.Profile{}, err
	}
	for _, p := range ps {
		if p.Name == name {
			return p, nil
		}
	}
	return core.Profile{}, fmt.Errorf("profile %q not found in %s", name, awsx.ConfigFilePath())
}

// Discover runs a fresh discovery of every configured region of p and
// refreshes the discovery cache. Partial results are returned together with
// the aggregated error.
func (a *App) Discover(ctx context.Context, p core.Profile) ([]core.Resource, error) {
	return a.discover(ctx, p, awsx.Regions(p, a.Cfg.Regions))
}

func (a *App) discover(ctx context.Context, p core.Profile, regions []string) ([]core.Resource, error) {
	if len(regions) == 0 {
		return nil, fmt.Errorf("profile %q has no region and no regions are configured", p.Name)
	}
	res, err := a.AWS.Discover(ctx, p, regions, a.Cfg.Presets, a.Cfg.EffectiveShell(), a.manualPorts)
	if err != nil {
		return res, fmt.Errorf("discover %s: %w", p.Name, err)
	}
	byRegion := make(map[string][]core.Resource, len(regions))
	for _, r := range res {
		byRegion[r.Region] = append(byRegion[r.Region], r)
	}
	now := time.Now()
	a.cacheMu.Lock()
	for _, region := range regions {
		a.cache[cacheKey(p.Name, region)] = cacheEntry{at: now, resources: byRegion[region]}
	}
	a.cacheMu.Unlock()
	return res, nil
}

// cachedResources returns resources for p across regions, discovering only
// regions without a fresh cache entry.
func (a *App) cachedResources(ctx context.Context, p core.Profile, regions []string) ([]core.Resource, error) {
	var out []core.Resource
	var missing []string
	a.cacheMu.Lock()
	for _, region := range regions {
		e, ok := a.cache[cacheKey(p.Name, region)]
		if ok && time.Since(e.at) < discoveryTTL {
			out = append(out, e.resources...)
			continue
		}
		missing = append(missing, region)
	}
	a.cacheMu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	fresh, err := a.discover(ctx, p, missing)
	return append(out, fresh...), err
}

func (a *App) invalidate(profile, region string) {
	a.cacheMu.Lock()
	delete(a.cache, cacheKey(profile, region))
	a.cacheMu.Unlock()
}

func cacheKey(profile, region string) string { return profile + "/" + region }

func manualKey(profile, region, name string) string {
	return profile + "/" + region + "/" + name
}

func (a *App) manualPorts(profile, region, name string) []core.PortSpec {
	return a.State.Get().ManualPorts[manualKey(profile, region, name)]
}

// profileOrStub resolves a profile by name; resources always carry a profile
// name, so a profile missing from the file still gets a usable stub.
func (a *App) profileOrStub(name, region string) core.Profile {
	if p, err := a.Profile(name); err == nil {
		return p
	}
	return core.Profile{Name: name, Region: region, Kind: core.AuthUnknown}
}

// Connect resolves the tunnel spec for r and remote, assigns a stable local
// port and starts the tunnel. For RDS the remote port is the database port and
// the session runs through the profile+region bastion; a *awsx.NeedBastionError
// is returned unwrapped when the user must pick one. Connecting a key that is
// already active returns the running spec.
func (a *App) Connect(ctx context.Context, r core.Resource, remote int) (core.TunnelSpec, error) {
	if r.Unreachable != "" {
		return core.TunnelSpec{}, fmt.Errorf("%s is not connectable: %s", r.Name, r.Unreachable)
	}
	spec := core.TunnelSpec{Profile: r.Profile, Region: r.Region}
	switch r.Kind {
	case core.KindEC2:
		spec.Target = r.ID
	case core.KindRDS:
		if r.Port > 0 {
			remote = r.Port
		}
		if r.Host == "" {
			return core.TunnelSpec{}, fmt.Errorf("%s has no endpoint", r.Name)
		}
		resources, err := a.cachedResources(ctx, a.profileOrStub(r.Profile, r.Region), []string{r.Region})
		if err != nil && len(resources) == 0 {
			return core.TunnelSpec{}, fmt.Errorf("find bastion for %s: %w", r.Name, err)
		}
		saved := a.State.Get().Bastions[cacheKey(r.Profile, r.Region)]
		b, err := awsx.PickBastion(resources, r.Profile, r.Region, saved)
		if err != nil {
			var nb *awsx.NeedBastionError
			if errors.As(err, &nb) {
				return core.TunnelSpec{}, nb
			}
			return core.TunnelSpec{}, fmt.Errorf("find bastion for %s: %w", r.Name, err)
		}
		spec.Target = b.ID
		spec.RemoteHost = r.Host
	default:
		return core.TunnelSpec{}, fmt.Errorf("unsupported resource kind %q", r.Kind)
	}
	if remote <= 0 || remote > 65535 {
		return core.TunnelSpec{}, fmt.Errorf("invalid remote port %d for %s", remote, r.Name)
	}
	spec.RemotePort = remote
	spec.Key = r.TunnelKey(remote)
	spec.Label = r.Name + " " + portLabel(r, remote)

	a.connectMu.Lock()
	defer a.connectMu.Unlock()
	if s, ok := a.Tunnels.Get(spec.Key); ok && isActive(s.Status) {
		return s.Spec, nil
	}
	port, temporary, err := a.assignPort(state.PortKey(r.Profile, r.Region, r.Name, remote), spec.Key, remote)
	if err != nil {
		return core.TunnelSpec{}, fmt.Errorf("assign local port for %s: %w", spec.Label, err)
	}
	spec.LocalPort = port
	spec.Temporary = temporary
	if err := a.Tunnels.Start(spec); err != nil {
		return core.TunnelSpec{}, fmt.Errorf("start tunnel %s: %w", spec.Label, err)
	}
	return spec, nil
}

// assignPort picks the local port for portKey, reserving ports held by other
// manager sessions and ports saved for other keys.
func (a *App) assignPort(portKey, tunnelKey string, remote int) (int, bool, error) {
	saved := a.State.Get().Ports
	reserved := map[int]bool{}
	for _, s := range a.Tunnels.Sessions() {
		if s.Spec.Key != tunnelKey && s.Status != tunnel.Stopped && s.Spec.LocalPort > 0 {
			reserved[s.Spec.LocalPort] = true
		}
	}
	for k, p := range saved {
		if k != portKey {
			reserved[p] = true
		}
	}
	save := func(key string, port int) error {
		return a.State.Update(func(d *state.Data) {
			if d.Ports == nil {
				d.Ports = map[string]int{}
			}
			d.Ports[key] = port
		})
	}
	return ports.Assign(portKey, remote, saved, reserved, save)
}

func isActive(s tunnel.Status) bool {
	return s == tunnel.Starting || s == tunnel.Up || s == tunnel.Reconnecting
}

func portLabel(r core.Resource, remote int) string {
	for _, p := range r.Ports {
		if p.Remote == remote && p.Name != "" {
			return p.Name
		}
	}
	return strconv.Itoa(remote)
}

// SetBastion saves the bastion instance for profile+region.
func (a *App) SetBastion(profile, region, instanceID string) error {
	if instanceID == "" {
		return errors.New("bastion instance ID is empty")
	}
	err := a.State.Update(func(d *state.Data) {
		if d.Bastions == nil {
			d.Bastions = map[string]string{}
		}
		d.Bastions[cacheKey(profile, region)] = instanceID
	})
	if err != nil {
		return fmt.Errorf("save bastion: %w", err)
	}
	return nil
}

// AddManualPort saves an extra port for an EC2 resource (keyed by Name so it
// survives instance replacement). A port with the same remote is replaced.
func (a *App) AddManualPort(r core.Resource, p core.PortSpec) error {
	if r.Kind != core.KindEC2 {
		return fmt.Errorf("manual ports are only supported for EC2 instances")
	}
	if p.Remote <= 0 || p.Remote > 65535 {
		return fmt.Errorf("invalid port %d", p.Remote)
	}
	key := manualKey(r.Profile, r.Region, r.Name)
	err := a.State.Update(func(d *state.Data) {
		if d.ManualPorts == nil {
			d.ManualPorts = map[string][]core.PortSpec{}
		}
		list := d.ManualPorts[key]
		for i, existing := range list {
			if existing.Remote == p.Remote {
				list[i] = p
				d.ManualPorts[key] = list
				return
			}
		}
		d.ManualPorts[key] = append(list, p)
	})
	if err != nil {
		return fmt.Errorf("save manual port: %w", err)
	}
	a.invalidate(r.Profile, r.Region)
	return nil
}

// Groups returns config groups merged with state groups (state wins by
// name), sorted by name.
func (a *App) Groups() []core.Group {
	return mergeGroups(a.Cfg.Groups, a.State.Get().Groups)
}

func mergeGroups(cfg, saved []core.Group) []core.Group {
	byName := map[string]core.Group{}
	for _, g := range cfg {
		byName[g.Name] = g
	}
	for _, g := range saved {
		byName[g.Name] = g
	}
	out := make([]core.Group, 0, len(byName))
	for _, g := range byName {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SaveGroup stores g in state, replacing a saved group with the same name.
func (a *App) SaveGroup(g core.Group) error {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" {
		return errors.New("group name is empty")
	}
	if len(g.Members) == 0 {
		return fmt.Errorf("group %q has no members", g.Name)
	}
	err := a.State.Update(func(d *state.Data) {
		for i, existing := range d.Groups {
			if existing.Name == g.Name {
				d.Groups[i] = g
				return
			}
		}
		d.Groups = append(d.Groups, g)
	})
	if err != nil {
		return fmt.Errorf("save group: %w", err)
	}
	return nil
}

// DeleteGroup removes a state-saved group. Config groups are read-only here.
func (a *App) DeleteGroup(name string) error {
	found := false
	err := a.State.Update(func(d *state.Data) {
		kept := d.Groups[:0]
		for _, g := range d.Groups {
			if g.Name == name {
				found = true
				continue
			}
			kept = append(kept, g)
		}
		d.Groups = kept
	})
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if found {
		return nil
	}
	for _, g := range a.Cfg.Groups {
		if g.Name == name {
			return fmt.Errorf("group %q is defined in config; edit the config file to remove it", name)
		}
	}
	return fmt.Errorf("group %q not found", name)
}

// FindGroup returns the merged group with the given name.
func (a *App) FindGroup(name string) (core.Group, bool) {
	for _, g := range a.Groups() {
		if g.Name == name {
			return g, true
		}
	}
	return core.Group{}, false
}

// StartGroup connects every member of the named group. Profiles are
// discovered once each (cached); errors are reported per member.
func (a *App) StartGroup(ctx context.Context, name string) ([]core.TunnelSpec, []error) {
	g, ok := a.FindGroup(name)
	if !ok {
		return nil, []error{fmt.Errorf("group %q not found", name)}
	}
	var specs []core.TunnelSpec
	var errs []error
	for _, m := range g.Members {
		spec, err := a.ConnectMember(ctx, m)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", MemberString(m), err))
			continue
		}
		specs = append(specs, spec)
	}
	return specs, errs
}

// ConnectMember resolves a group member against (cached) discovery and
// connects it.
func (a *App) ConnectMember(ctx context.Context, m core.GroupMember) (core.TunnelSpec, error) {
	r, err := a.ResolveMember(ctx, m)
	if err != nil {
		return core.TunnelSpec{}, err
	}
	return a.Connect(ctx, r, m.Remote)
}

// ResolveMember finds the resource a member refers to, using cached discovery
// of the member's region (or all configured regions of its profile).
func (a *App) ResolveMember(ctx context.Context, m core.GroupMember) (core.Resource, error) {
	p, err := a.Profile(m.Profile)
	if err != nil {
		return core.Resource{}, err
	}
	regions := awsx.Regions(p, a.Cfg.Regions)
	if m.Region != "" {
		regions = []string{m.Region}
	}
	resources, derr := a.cachedResources(ctx, p, regions)
	r, err := MatchMember(resources, m)
	if err != nil {
		if derr != nil {
			return core.Resource{}, fmt.Errorf("%w (discovery: %v)", err, derr)
		}
		return core.Resource{}, err
	}
	return r, nil
}

// MatchMember picks the resource matching m by Kind (any kind if empty), Name
// and Region (any region if empty). Connectable matches win over unreachable
// ones; several connectable matches in one region resolve to the lowest ID,
// while matches spread over regions are ambiguous.
func MatchMember(resources []core.Resource, m core.GroupMember) (core.Resource, error) {
	var reachable, unreachable []core.Resource
	for _, r := range resources {
		if r.Profile != m.Profile || r.Name != m.Name {
			continue
		}
		if m.Kind != "" && r.Kind != m.Kind {
			continue
		}
		if m.Region != "" && r.Region != m.Region {
			continue
		}
		if r.Unreachable != "" {
			unreachable = append(unreachable, r)
		} else {
			reachable = append(reachable, r)
		}
	}
	if len(reachable) == 0 {
		if len(unreachable) > 0 {
			r := unreachable[0]
			return core.Resource{}, fmt.Errorf("%s (%s) is not connectable: %s", r.Name, r.ID, r.Unreachable)
		}
		return core.Resource{}, fmt.Errorf("no resource named %q in profile %s", m.Name, m.Profile)
	}
	sort.Slice(reachable, func(i, j int) bool {
		if reachable[i].Region != reachable[j].Region {
			return reachable[i].Region < reachable[j].Region
		}
		if reachable[i].Kind != reachable[j].Kind {
			return reachable[i].Kind < reachable[j].Kind
		}
		return reachable[i].ID < reachable[j].ID
	})
	first := reachable[0]
	for _, r := range reachable[1:] {
		if r.Region != first.Region {
			return core.Resource{}, fmt.Errorf("%q exists in regions %s and %s; set a region", m.Name, first.Region, r.Region)
		}
		if r.Kind != first.Kind {
			return core.Resource{}, fmt.Errorf("%q matches both %s and %s resources; set a kind", m.Name, first.Kind, r.Kind)
		}
	}
	return first, nil
}

// MemberString renders a member as "profile/name:remote" (with region and
// kind when set).
func MemberString(m core.GroupMember) string {
	var b strings.Builder
	b.WriteString(m.Profile)
	b.WriteString("/")
	b.WriteString(m.Name)
	if m.Remote > 0 {
		b.WriteString(":")
		b.WriteString(strconv.Itoa(m.Remote))
	}
	var extra []string
	if m.Kind != "" {
		extra = append(extra, string(m.Kind))
	}
	if m.Region != "" {
		extra = append(extra, m.Region)
	}
	if len(extra) > 0 {
		b.WriteString(" (" + strings.Join(extra, ", ") + ")")
	}
	return b.String()
}

// ShellCmd builds an interactive shell session to an EC2 instance.
func (a *App) ShellCmd(r core.Resource) *exec.Cmd {
	command := r.ShellCommand
	if command == "" {
		command = a.Cfg.EffectiveShell()
	}
	return tunnel.ShellCmd(a.AWSPath, r.Profile, r.Region, r.ID, command)
}

// LoginCmd returns the SSO login command for SSO profiles, nil otherwise.
func (a *App) LoginCmd(p core.Profile) *exec.Cmd {
	if p.Kind != core.AuthSSO {
		return nil
	}
	return awsx.ProfileLoginCmd(a.AWSPath, p.Name)
}

// SSOConfig returns the configured SSO settings with the default session name
// applied.
func (a *App) SSOConfig() config.SSO {
	s := a.Cfg.SSO
	if s.Session == "" {
		s.Session = defaultSSOSession
	}
	return s
}

// EnsureSSOSession makes sure the AWS config file contains the
// [sso-session <name>] section `aws sso login --sso-session` needs. When it is
// missing, the managed block is rewritten with the session and the profiles it
// already holds.
func (a *App) EnsureSSOSession() error {
	s := a.SSOConfig()
	if s.StartURL == "" || s.Region == "" {
		return errors.New("SSO is not configured (start URL and region required); run `awsport init`")
	}
	path := awsx.ConfigFilePath()
	f, err := ini.LoadSources(ini.LoadOptions{Loose: true, AllowNonUniqueSections: true}, path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if f.HasSection("sso-session " + s.Session) {
		return nil
	}
	all, err := awsx.ListProfiles()
	if err != nil {
		return fmt.Errorf("list profiles: %w", err)
	}
	var managed []awsx.SSOProfile
	for _, p := range all {
		if !p.Managed {
			continue
		}
		managed = append(managed, awsx.SSOProfile{
			Name:      p.Name,
			AccountID: p.AccountID,
			RoleName:  p.RoleName,
			Region:    p.Region,
		})
	}
	if err := awsx.WriteManagedBlock(path, s, managed); err != nil {
		return fmt.Errorf("write sso-session %q to %s: %w", s.Session, path, err)
	}
	return nil
}

// SyncSSOProfiles lists every account/role reachable through the configured
// SSO session and writes them into the managed block of the AWS config file.
func (a *App) SyncSSOProfiles(ctx context.Context) (int, error) {
	s := a.SSOConfig()
	if s.StartURL == "" || s.Region == "" {
		return 0, errors.New("SSO is not configured (start URL and region required); run `awsport init`")
	}
	profiles, err := awsx.ListSSOAccess(ctx, s)
	if err != nil {
		return 0, fmt.Errorf("list SSO accounts: %w", err)
	}
	for i := range profiles {
		if profiles[i].Name == "" {
			profiles[i].Name = awsx.ProfileName(profiles[i].AccountName, profiles[i].RoleName)
		}
		if profiles[i].Region == "" {
			profiles[i].Region = s.Region
		}
	}
	if err := awsx.WriteManagedBlock(awsx.ConfigFilePath(), s, profiles); err != nil {
		return 0, fmt.Errorf("write AWS config: %w", err)
	}
	a.cacheMu.Lock()
	a.cache = map[string]cacheEntry{}
	a.cacheMu.Unlock()
	return len(profiles), nil
}

// Close stops every tunnel.
func (a *App) Close() {
	a.Tunnels.StopAll()
}

// ConnString is the local address of a tunnel.
func ConnString(spec core.TunnelSpec) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.LocalPort))
}
