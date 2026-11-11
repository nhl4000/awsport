package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

// ---- profiles ----

func (m *model) rebuildProfiles() tea.Cmd {
	items := make([]list.Item, 0, len(m.profiles))
	for _, p := range m.profiles {
		items = append(items, item{
			title: fmt.Sprintf("%s  [%s]", p.Name, kindBadge(p.Kind)),
			desc:  m.profileDesc(p),
			key:   p.Name,
		})
	}
	return m.setItems(scrProfiles, items)
}

func (m *model) profileDesc(p core.Profile) string {
	var parts []string
	if p.AccountID != "" {
		parts = append(parts, p.AccountID)
	}
	if p.RoleName != "" {
		parts = append(parts, p.RoleName)
	}
	if p.Region != "" {
		parts = append(parts, p.Region)
	} else {
		parts = append(parts, "no region")
	}
	if p.Managed {
		parts = append(parts, "managed by awsport")
	}
	if c, ok := m.creds[p.Name]; ok {
		switch {
		case !c.done:
			parts = append(parts, "checking login…")
		case c.err == nil:
			parts = append(parts, "✓ logged in")
		case awsx.IsAuthError(c.err):
			parts = append(parts, "✗ login expired (L to log in)")
		case errors.Is(c.err, context.Canceled):
		default:
			parts = append(parts, "? "+oneLine(c.err.Error()))
		}
	}
	return strings.Join(parts, " · ")
}

func (m *model) onProfiles(msg profilesMsg) tea.Cmd {
	if msg.err != nil {
		m.setErr(fmt.Errorf("read AWS profiles: %w", msg.err))
		return nil
	}
	m.profiles = msg.profiles
	switch {
	case len(m.profiles) == 0:
		m.setErr(errors.New("no AWS profiles found; press o to set up SSO"))
	case msg.flash != "":
		m.setFlash(msg.flash)
	}
	return m.checkAllCreds()
}

func (m *model) profilesKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Enter):
		name, ok := selectedKey(m.lists[scrProfiles])
		if !ok {
			return true, nil
		}
		p, ok := m.profileByName(name)
		if !ok {
			return true, nil
		}
		return true, m.openProfile(p)
	case key.Matches(msg, keys.Refresh):
		return true, m.loadProfiles("profiles reloaded")
	case key.Matches(msg, keys.Onboard):
		m.onb = newOnboarding(true)
		m.goTo(scrOnboarding)
		return true, m.checkDeps(false)
	}
	return false, nil
}

func (m *model) openProfile(p core.Profile) tea.Cmd {
	if m.cur.Name != p.Name {
		m.lists[scrResources].ResetFilter()
		m.lists[scrResources].ResetSelected()
	}
	m.cur = p
	m.goTo(scrResources)
	cmd := m.rebuildResources()
	if _, cached := m.resources[p.Name]; !cached && !m.loading[p.Name] {
		cmd = tea.Batch(cmd, m.discover(p))
	}
	return cmd
}

// ---- resources ----

func (m *model) discover(p core.Profile) tea.Cmd {
	m.loading[p.Name] = true
	a := m.a
	return tea.Batch(m.rebuildResources(), m.run("discovering resources in "+p.Name, discoverTimeout, func(ctx context.Context) tea.Msg {
		rs, err := a.Discover(ctx, p)
		return resourcesMsg{profile: p.Name, rs: rs, err: err}
	}))
}

func (m *model) onResources(msg resourcesMsg) tea.Cmd {
	m.loading[msg.profile] = false
	m.resErr[msg.profile] = msg.err
	if msg.err != nil {
		m.noteAuth(msg.profile, msg.err)
		if awsx.IsAuthError(msg.err) {
			m.creds[msg.profile] = credState{done: true, err: msg.err}
		}
	} else {
		delete(m.authProfiles, msg.profile)
	}
	if msg.err == nil || len(msg.rs) > 0 {
		m.resources[msg.profile] = msg.rs
		for _, r := range msg.rs {
			m.known[r.Ref()] = r
		}
		if m.curRes.Profile == msg.profile {
			if r, ok := m.known[m.curRes.Ref()]; ok {
				m.curRes = r
			}
		}
	}
	if msg.err == nil && m.cur.Name == msg.profile {
		m.setFlash(fmt.Sprintf("%s: %d resource(s)", msg.profile, len(msg.rs)))
	}
	return tea.Batch(m.rebuildResources(), m.rebuildPorts(), m.rebuildProfiles())
}

func (m *model) rebuildResources() tea.Cmd {
	l := m.lists[scrResources]
	rs := m.resources[m.cur.Name]
	title := "Resources · " + m.cur.Name
	if n := hiddenCount(rs); n > 0 && !m.showAll {
		title += fmt.Sprintf(" · %d unreachable hidden (a)", n)
	}
	if m.loading[m.cur.Name] {
		title += " · discovering…"
	}
	l.Title = title
	vis := visibleResources(rs, m.showAll)
	items := make([]list.Item, 0, len(vis))
	for _, r := range vis {
		items = append(items, item{title: resourceTitle(r), desc: resourceDesc(r), key: r.Ref()})
	}
	return m.setItems(scrResources, items)
}

func resourceTitle(r core.Resource) string {
	kind := strings.ToUpper(string(r.Kind))
	if r.Name == "" || r.Name == r.ID {
		return fmt.Sprintf("%s  [%s]", r.ID, kind)
	}
	return fmt.Sprintf("%s  [%s] %s", r.Name, kind, r.ID)
}

func resourceDesc(r core.Resource) string {
	var parts []string
	if r.Unreachable != "" {
		parts = append(parts, "⚠ "+r.Unreachable)
	}
	parts = append(parts, r.Region)
	switch r.Kind {
	case core.KindRDS:
		if r.Engine != "" {
			parts = append(parts, r.Engine)
		}
		if r.Host != "" {
			parts = append(parts, r.Host+":"+strconv.Itoa(r.Port))
		}
		parts = append(parts, r.State)
	default:
		parts = append(parts, r.State)
		if r.IsBastion {
			parts = append(parts, "bastion")
		}
		if len(r.Ports) > 0 {
			ps := make([]string, 0, len(r.Ports))
			for _, p := range r.Ports {
				ps = append(ps, p.String())
			}
			parts = append(parts, "ports "+strings.Join(ps, ", "))
		}
	}
	return strings.Join(parts, " · ")
}

func resourceSummary(r core.Resource) string {
	return resourceTitle(r) + " · " + resourceDesc(r)
}

func (m *model) selectedResource() (core.Resource, bool) {
	ref, ok := selectedKey(m.lists[scrResources])
	if !ok {
		return core.Resource{}, false
	}
	for _, r := range m.resources[m.cur.Name] {
		if r.Ref() == ref {
			return r, true
		}
	}
	return core.Resource{}, false
}

func (m *model) resourcesKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Refresh):
		return true, m.discover(m.cur)
	case key.Matches(msg, keys.ShowAll):
		m.showAll = !m.showAll
		return true, m.rebuildResources()
	}
	if !key.Matches(msg, keys.Enter, keys.AddPort, keys.Shell) {
		return false, nil
	}
	r, ok := m.selectedResource()
	if !ok {
		return true, nil
	}
	switch {
	case key.Matches(msg, keys.Enter):
		if r.Unreachable != "" {
			m.setErr(fmt.Errorf("%s is not connectable: %s", r.Name, r.Unreachable))
			return true, nil
		}
		if r.Kind == core.KindRDS {
			return true, m.connect(r, r.Port)
		}
		m.curRes = r
		m.lists[scrPorts].ResetSelected()
		m.goTo(scrPorts)
		return true, m.rebuildPorts()
	case key.Matches(msg, keys.AddPort):
		if r.Kind != core.KindEC2 {
			m.setErr(errors.New("manual ports apply to EC2 instances; RDS uses its endpoint port"))
			return true, nil
		}
		return true, m.promptManualPort(r)
	default:
		return true, m.shell(r)
	}
}

// ---- ports ----

func (m *model) rebuildPorts() tea.Cmd {
	r := m.curRes
	if r.ID == "" {
		return nil
	}
	l := m.lists[scrPorts]
	l.Title = "Ports · " + resourceTitle(r)
	items := make([]list.Item, 0, len(r.Ports)+2)
	for _, p := range r.Ports {
		desc := "enter to start a tunnel"
		if s, ok := m.a.Tunnels.Get(r.TunnelKey(p.Remote)); ok && s.Status != tunnel.Stopped {
			hint := "enter to retry, x to dismiss"
			if tunnel.IsActive(s.Status) {
				hint = "enter to stop"
			}
			desc = statusGlyph(s.Status) + " " + statusText(s) + " · " + linkAddr(s.Spec) + " · " + hint
		}
		items = append(items, item{title: p.String(), desc: desc, key: "port:" + strconv.Itoa(p.Remote)})
	}
	items = append(items, item{title: "+ add a port…", desc: "forward another remote port (remembered for this instance name)", key: "add"})
	if r.SSMOnline {
		items = append(items, item{title: "⌨ interactive shell", desc: "runs: " + r.ShellCommand, key: "shell"})
	}
	return m.setItems(scrPorts, items)
}

// selectedPort returns the remote port under the cursor and its session.
func (m *model) selectedPort() (remote int, s tunnel.Session, exists, ok bool) {
	k, ok := selectedKey(m.lists[scrPorts])
	if !ok || !strings.HasPrefix(k, "port:") {
		return 0, tunnel.Session{}, false, false
	}
	remote, err := strconv.Atoi(strings.TrimPrefix(k, "port:"))
	if err != nil {
		return 0, tunnel.Session{}, false, false
	}
	s, exists = m.a.Tunnels.Get(m.curRes.TunnelKey(remote))
	if exists && s.Status == tunnel.Stopped {
		exists = false
	}
	return remote, s, exists, true
}

func (m *model) portsKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.AddPort):
		return true, m.promptManualPort(m.curRes)
	case key.Matches(msg, keys.Shell):
		return true, m.shell(m.curRes)
	case key.Matches(msg, keys.Stop, keys.Open, keys.Copy):
		remote, s, exists, ok := m.selectedPort()
		switch {
		case !ok:
			m.setErr(fmt.Errorf("%w: no port under the cursor", errNoSelection))
		case !exists:
			m.setFlash(fmt.Sprintf("%s:%d is not running; enter starts it", m.curRes.Name, remote))
		case key.Matches(msg, keys.Stop):
			return true, m.stopTunnel(s.Spec)
		case key.Matches(msg, keys.Open):
			m.openURL(s.Spec)
		default:
			m.copyConn(s.Spec)
		}
		return true, nil
	case key.Matches(msg, keys.Enter):
		k, ok := selectedKey(m.lists[scrPorts])
		if !ok {
			return true, nil
		}
		switch k {
		case "add":
			return true, m.promptManualPort(m.curRes)
		case "shell":
			return true, m.shell(m.curRes)
		}
		remote, s, exists, ok := m.selectedPort()
		if !ok {
			return true, nil
		}
		if portAction(s, exists) == actStop {
			return true, m.stopTunnel(s.Spec)
		}
		return true, m.connect(m.curRes, remote)
	}
	return false, nil
}

func (m *model) promptManualPort(r core.Resource) tea.Cmd {
	a := m.a
	return m.prompt(fmt.Sprintf("Add port to %s (name:port or port):", r.Name), "app:8080", func(s string) (tea.Cmd, error) {
		ps, err := parsePortSpec(s)
		if err != nil {
			return nil, err
		}
		return m.run("saving port", configTimeout, func(context.Context) tea.Msg {
			return manualPortMsg{r: r, p: ps, err: a.AddManualPort(r, ps)}
		}), nil
	})
}

func (m *model) onManualPort(msg manualPortMsg) tea.Cmd {
	if msg.err != nil {
		m.setErr(fmt.Errorf("add port %s to %s: %w", msg.p, msg.r.Name, msg.err))
		return nil
	}
	rs := m.resources[msg.r.Profile]
	for i := range rs {
		if rs[i].Ref() == msg.r.Ref() {
			rs[i].Ports = withPort(rs[i].Ports, msg.p)
			m.known[rs[i].Ref()] = rs[i]
		}
	}
	if m.curRes.Ref() == msg.r.Ref() {
		m.curRes.Ports = withPort(m.curRes.Ports, msg.p)
	}
	m.setFlash(fmt.Sprintf("added port %s to %s", msg.p, msg.r.Name))
	return tea.Batch(m.rebuildResources(), m.rebuildPorts())
}

// withPort adds p to ports, replacing an entry with the same remote port.
func withPort(ports []core.PortSpec, p core.PortSpec) []core.PortSpec {
	out := make([]core.PortSpec, 0, len(ports)+1)
	for _, x := range ports {
		if x.Remote != p.Remote {
			out = append(out, x)
		}
	}
	return append(out, p)
}

func (m *model) shell(r core.Resource) tea.Cmd {
	if r.Kind != core.KindEC2 || !r.SSMOnline {
		m.setErr(fmt.Errorf("%s has no online SSM agent; interactive shells need an EC2 instance with SSM", r.Name))
		return nil
	}
	return tea.ExecProcess(m.a.ShellCmd(r), func(err error) tea.Msg { return shellDoneMsg{err: err} })
}

// ---- connect & bastion ----

func (m *model) connect(r core.Resource, remote int) tea.Cmd {
	a := m.a
	return m.run(fmt.Sprintf("connecting %s:%d", r.Name, remote), connectTimeout, func(ctx context.Context) tea.Msg {
		spec, err := a.Connect(ctx, r, remote)
		return connectMsg{r: r, remote: remote, spec: spec, err: err}
	})
}

func (m *model) onConnect(msg connectMsg) tea.Cmd {
	var nb *awsx.NeedBastionError
	switch {
	case errors.As(msg.err, &nb):
		m.bastion = nb
		m.pending = pendingConnect{r: msg.r, remote: msg.remote}
		m.lists[scrBastion].ResetSelected()
		m.goTo(scrBastion)
		return m.rebuildBastion()
	case msg.err != nil:
		m.noteAuth(msg.r.Profile, msg.err)
		m.setErr(fmt.Errorf("connect %s:%d: %w", msg.r.Name, msg.remote, msg.err))
	default:
		m.setFlash(fmt.Sprintf("started %s → %s", msg.spec.Label, localAddr(msg.spec)))
	}
	return tea.Batch(m.rebuildPorts(), m.rebuildDashboard())
}

func (m *model) rebuildBastion() tea.Cmd {
	if m.bastion == nil {
		return nil
	}
	items := make([]list.Item, 0, len(m.bastion.Candidates))
	for _, c := range m.bastion.Candidates {
		items = append(items, item{title: resourceTitle(c), desc: resourceDesc(c), key: c.ID})
	}
	return m.setItems(scrBastion, items)
}

func (m *model) bastionKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	if !key.Matches(msg, keys.Enter) || m.bastion == nil {
		return false, nil
	}
	id, ok := selectedKey(m.lists[scrBastion])
	if !ok {
		return true, nil
	}
	a, nb, pc := m.a, m.bastion, m.pending
	m.bastion = nil
	m.back()
	return true, m.run("saving bastion and connecting", connectTimeout, func(ctx context.Context) tea.Msg {
		if err := a.SetBastion(nb.Profile, nb.Region, id); err != nil {
			return connectMsg{r: pc.r, remote: pc.remote, err: fmt.Errorf("save bastion: %w", err)}
		}
		spec, err := a.Connect(ctx, pc.r, pc.remote)
		return connectMsg{r: pc.r, remote: pc.remote, spec: spec, err: err}
	})
}
