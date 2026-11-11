// Package tui is awsport's interactive Bubble Tea interface.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/tunnel"
)

// Timeouts for background work.
const (
	depsTimeout     = 30 * time.Second
	credTimeout     = 20 * time.Second
	discoverTimeout = 2 * time.Minute
	connectTimeout  = 2 * time.Minute
	groupTimeout    = 4 * time.Minute
	configTimeout   = 30 * time.Second
	syncTimeout     = 90 * time.Second
	tunnelOpTimeout = 15 * time.Second
	refreshInterval = 2 * time.Second
	maxCredChecks   = 6
)

// NeedsOnboarding reports whether setup is required: a prerequisite is
// missing or no AWS profiles are configured.
func NeedsOnboarding(a *app.App) bool {
	ctx, cancel := context.WithTimeout(context.Background(), depsTimeout)
	defer cancel()
	if !depsOK(deps.CheckAll(ctx)) {
		return true
	}
	ps, err := a.Profiles()
	return err != nil || len(ps) == 0
}

// Run starts the full-screen UI and blocks until the user quits. All tunnels
// are stopped before it returns.
func Run(a *app.App) error {
	m := newModel(a)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	m.cancel()
	if n := a.Tunnels.ActiveCount(); n > 0 {
		fmt.Fprintf(os.Stderr, "awsport: stopping %d tunnel(s)…\n", n)
	}
	a.Close()
	if err != nil {
		return fmt.Errorf("run tui: %w", err)
	}
	return nil
}

type screen int

const (
	scrOnboarding screen = iota
	scrProfiles
	scrResources
	scrPorts
	scrBastion
	scrDashboard
	scrGroups
)

type credState struct {
	done bool
	err  error
}

type pendingConnect struct {
	r      core.Resource
	remote int
}

type confirmPrompt struct {
	text string
	yes  func() tea.Cmd
}

type inputPrompt struct {
	label  string
	ti     textinput.Model
	submit func(string) (tea.Cmd, error)
	err    string
}

// Messages.
type (
	opDone struct {
		op  int
		msg tea.Msg
	}
	listMsg struct {
		id  screen
		msg tea.Msg
	}
	eventMsg        tunnel.Event
	eventsClosedMsg struct{}
	tickMsg         struct{}
	quitMsg         struct{}
	depsMsg         struct {
		checks     []deps.Check
		background bool
	}
	profilesMsg struct {
		profiles []core.Profile
		err      error
		flash    string
	}
	credMsg struct {
		profile string
		err     error
	}
	resourcesMsg struct {
		profile string
		rs      []core.Resource
		err     error
	}
	connectMsg struct {
		r      core.Resource
		remote int
		spec   core.TunnelSpec
		err    error
	}
	manualPortMsg struct {
		r   core.Resource
		p   core.PortSpec
		err error
	}
	shellDoneMsg struct{ err error }
	loginDoneMsg struct{ err error }
	restartedMsg struct{ keys []string }
	tunnelOpMsg  struct {
		verb, label string
		err         error
	}
	groupSavedMsg struct {
		name       string
		members    int
		unresolved []string
		discovered map[string][]core.Resource
		err        error
	}
	groupStartedMsg struct {
		name  string
		specs []core.TunnelSpec
		errs  []error
	}
	groupDeletedMsg struct {
		name string
		err  error
	}
)

type model struct {
	a      *app.App
	ctx    context.Context
	cancel context.CancelFunc
	events <-chan tunnel.Event

	width, height int
	stack         []screen

	spin  spinner.Model
	ops   map[int]string
	opSeq int
	help  help.Model

	flash        string
	flashErr     bool
	authProfiles map[string]bool // profiles whose AWS calls failed with auth errors
	depsWarning  string

	confirm    *confirmPrompt
	input      *inputPrompt
	loginQueue []*exec.Cmd

	profiles  []core.Profile
	creds     map[string]credState
	credSem   chan struct{}
	resources map[string][]core.Resource
	resErr    map[string]error
	loading   map[string]bool
	known     map[string]core.Resource // by Resource.Ref()

	cur      core.Profile
	curRes   core.Resource
	showAll  bool
	pending  pendingConnect
	bastion  *awsx.NeedBastionError
	selected map[string]bool

	onb   onboarding
	lists map[screen]*list.Model
}

func newModel(a *app.App) *model {
	ctx, cancel := context.WithCancel(context.Background())
	m := &model{
		a:            a,
		ctx:          ctx,
		cancel:       cancel,
		events:       a.Tunnels.Events(),
		spin:         spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(cursorStyle)),
		ops:          map[int]string{},
		help:         help.New(),
		authProfiles: map[string]bool{},
		creds:        map[string]credState{},
		credSem:      make(chan struct{}, maxCredChecks),
		resources:    map[string][]core.Resource{},
		resErr:       map[string]error{},
		loading:      map[string]bool{},
		known:        map[string]core.Resource{},
		selected:     map[string]bool{},
		lists:        map[screen]*list.Model{},
	}
	for _, s := range []screen{scrProfiles, scrResources, scrPorts, scrBastion, scrDashboard, scrGroups} {
		l := newList(s)
		m.lists[s] = &l
	}
	ps, err := a.Profiles()
	if err != nil {
		m.setErr(fmt.Errorf("read AWS profiles: %w", err))
	}
	m.profiles = ps
	if len(ps) == 0 {
		m.stack = []screen{scrOnboarding}
		m.onb = newOnboarding(false)
	} else {
		m.stack = []screen{scrProfiles}
		m.rebuildProfiles()
	}
	return m
}

func newList(s screen) list.Model {
	d := list.NewDefaultDelegate()
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.Foreground(colorAccent).BorderForeground(colorAccent)
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.Foreground(colorAccent).BorderForeground(colorAccent)
	l := list.New(nil, d, 0, 0)
	l.SetShowHelp(false)
	l.DisableQuitKeybindings()
	l.Styles.Title = l.Styles.Title.Background(colorAccent)
	switch s {
	case scrProfiles:
		l.Title = "AWS profiles"
		l.SetStatusBarItemName("profile", "profiles")
	case scrResources:
		l.SetStatusBarItemName("resource", "resources")
	case scrPorts:
		l.SetFilteringEnabled(false)
		l.SetStatusBarItemName("option", "options")
	case scrBastion:
		l.Title = "Pick a bastion"
		l.SetFilteringEnabled(false)
		l.SetStatusBarItemName("instance", "instances")
	case scrDashboard:
		l.SetFilteringEnabled(false)
		l.SetStatusBarItemName("tunnel", "tunnels")
	case scrGroups:
		l.Title = "Groups"
		l.SetStatusBarItemName("group", "groups")
	}
	return l
}

// item is a generic list entry; key identifies the underlying value.
type item struct {
	title, desc, key string
}

func (i item) Title() string       { return i.title }
func (i item) Description() string { return i.desc }
func (i item) FilterValue() string { return i.title }

func selectedKey(l *list.Model) (string, bool) {
	it, ok := l.SelectedItem().(item)
	if !ok {
		return "", false
	}
	return it.key, true
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.waitEvent(), m.tick()}
	if m.screen() == scrOnboarding {
		cmds = append(cmds, m.checkDeps(false))
	} else {
		cmds = append(cmds, m.checkDeps(true), m.checkAllCreds())
	}
	return tea.Batch(cmds...)
}

// ---- navigation ----

func (m *model) screen() screen { return m.stack[len(m.stack)-1] }

// goTo shows s, truncating the stack if s is already on it.
func (m *model) goTo(s screen) {
	for i, x := range m.stack {
		if x == s {
			m.stack = m.stack[:i+1]
			return
		}
	}
	m.stack = append(m.stack, s)
}

func (m *model) back() {
	if len(m.stack) > 1 {
		m.stack = m.stack[:len(m.stack)-1]
	}
}

func (m *model) resetTo(s screen) { m.stack = []screen{s} }

// ---- async helpers ----

// run executes fn off the UI goroutine with a timeout, tracking it in the
// spinner line until it returns.
func (m *model) run(label string, timeout time.Duration, fn func(ctx context.Context) tea.Msg) tea.Cmd {
	m.opSeq++
	id := m.opSeq
	startSpin := len(m.ops) == 0
	m.ops[id] = label
	parent := m.ctx
	work := func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		return opDone{op: id, msg: fn(ctx)}
	}
	if startSpin {
		return tea.Batch(m.spin.Tick, work)
	}
	return work
}

func (m *model) waitEvent() tea.Cmd {
	ch := m.events
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg(ev)
	}
}

func (m *model) tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) awsPath() string {
	if m.a.AWSPath != "" {
		return m.a.AWSPath
	}
	return "aws"
}

func (m *model) checkDeps(background bool) tea.Cmd {
	return m.run("checking prerequisites", depsTimeout, func(ctx context.Context) tea.Msg {
		return depsMsg{checks: deps.CheckAll(ctx), background: background}
	})
}

func (m *model) loadProfiles(flash string) tea.Cmd {
	a := m.a
	return m.run("reading AWS profiles", configTimeout, func(context.Context) tea.Msg {
		ps, err := a.Profiles()
		return profilesMsg{profiles: ps, err: err, flash: flash}
	})
}

func (m *model) checkCreds(p core.Profile) tea.Cmd {
	m.creds[p.Name] = credState{}
	sem, d, parent := m.credSem, m.a.AWS, m.ctx
	return func() tea.Msg {
		select {
		case sem <- struct{}{}:
		case <-parent.Done():
			return credMsg{profile: p.Name, err: parent.Err()}
		}
		defer func() { <-sem }()
		ctx, cancel := context.WithTimeout(parent, credTimeout)
		defer cancel()
		return credMsg{profile: p.Name, err: d.CheckCredentials(ctx, p)}
	}
}

func (m *model) checkAllCreds() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.profiles))
	for _, p := range m.profiles {
		cmds = append(cmds, m.checkCreds(p))
	}
	m.rebuildProfiles()
	return tea.Batch(cmds...)
}

// ---- status line ----

func (m *model) setFlash(s string) { m.flash, m.flashErr = s, false }

func (m *model) setErr(err error) {
	m.flash, m.flashErr = strings.Join(strings.Fields(err.Error()), " "), true
}

// noteAuth records profiles whose calls failed due to expired credentials.
func (m *model) noteAuth(profile string, err error) {
	if err != nil && awsx.IsAuthError(err) {
		m.authProfiles[profile] = true
	}
}

func (m *model) bannerProfiles() []string {
	names := authExpiredProfiles(m.a.Tunnels.Sessions())
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for n := range m.authProfiles {
		if !seen[n] {
			names = append(names, n)
		}
	}
	return names
}

// ---- update ----

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.layout()
	return m, cmd
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return nil
	case opDone:
		delete(m.ops, msg.op)
		return m.update(msg.msg)
	case spinner.TickMsg:
		if len(m.ops) == 0 {
			return nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return cmd
	case listMsg:
		return m.routeListMsg(msg)
	case tickMsg:
		if m.screen() == scrDashboard {
			return tea.Batch(m.rebuildDashboard(), m.tick())
		}
		return m.tick()
	case eventMsg:
		return tea.Batch(m.onEvent(tunnel.Event(msg)), m.waitEvent())
	case eventsClosedMsg:
		return nil
	case quitMsg:
		return tea.Quit
	case tea.KeyMsg:
		return m.onKey(msg)
	case depsMsg:
		return m.onDeps(msg)
	case profilesMsg:
		return m.onProfiles(msg)
	case credMsg:
		m.creds[msg.profile] = credState{done: true, err: msg.err}
		if msg.err == nil {
			delete(m.authProfiles, msg.profile)
		}
		return m.rebuildProfiles()
	case resourcesMsg:
		return m.onResources(msg)
	case connectMsg:
		return m.onConnect(msg)
	case manualPortMsg:
		return m.onManualPort(msg)
	case shellDoneMsg:
		if msg.err != nil {
			m.setErr(fmt.Errorf("shell session ended: %w", msg.err))
		} else {
			m.setFlash("shell session closed")
		}
		return nil
	case loginDoneMsg:
		return m.onLoginDone(msg)
	case restartedMsg:
		if len(msg.keys) > 0 {
			m.setFlash(fmt.Sprintf("logged in; restarting %d tunnel(s)", len(msg.keys)))
		}
		return m.rebuildDashboard()
	case tunnelOpMsg:
		if msg.err != nil {
			m.setErr(fmt.Errorf("%s %s: %w", msg.verb, msg.label, msg.err))
		} else {
			m.setFlash(fmt.Sprintf("%s %s", msg.verb, msg.label))
		}
		return m.rebuildDashboard()
	case groupSavedMsg:
		return m.onGroupSaved(msg)
	case groupStartedMsg:
		return m.onGroupStarted(msg)
	case groupDeletedMsg:
		if msg.err != nil {
			m.setErr(fmt.Errorf("delete group %s: %w", msg.name, msg.err))
		} else {
			m.setFlash("deleted group " + msg.name)
		}
		return m.rebuildGroups()
	}
	if cmd, ok := m.updateOnboardingMsg(msg); ok {
		return cmd
	}
	if m.input != nil {
		var cmd tea.Cmd
		m.input.ti, cmd = m.input.ti.Update(msg)
		return cmd
	}
	if l := m.activeList(); l != nil {
		return m.updateList(m.screen(), msg)
	}
	return m.onb.updateInputs(msg)
}

func (m *model) activeList() *list.Model { return m.lists[m.screen()] }

// updateList forwards msg to list id and tags its follow-up messages so
// they come back to the same list even if the screen changes meanwhile.
func (m *model) updateList(id screen, msg tea.Msg) tea.Cmd {
	l := m.lists[id]
	nl, cmd := l.Update(msg)
	*l = nl
	return tagList(id, cmd)
}

func tagList(id screen, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg { return listMsg{id: id, msg: cmd()} }
}

func (m *model) routeListMsg(msg listMsg) tea.Cmd {
	switch inner := msg.msg.(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		cmds := make([]tea.Cmd, 0, len(inner))
		for _, c := range inner {
			cmds = append(cmds, tagList(msg.id, c))
		}
		return tea.Batch(cmds...)
	case tea.KeyMsg, tea.WindowSizeMsg:
		return m.update(inner)
	default:
		if _, ok := m.lists[msg.id]; !ok {
			return m.update(inner)
		}
		return m.updateList(msg.id, inner)
	}
}

// setItems replaces a list's items; filtering follow-ups are tagged.
func (m *model) setItems(id screen, items []list.Item) tea.Cmd {
	return tagList(id, m.lists[id].SetItems(items))
}

func (m *model) onKey(msg tea.KeyMsg) tea.Cmd {
	if m.confirm != nil {
		c := m.confirm
		switch {
		case key.Matches(msg, keys.Yes):
			m.confirm = nil
			return c.yes()
		case key.Matches(msg, keys.ForceQuit):
			m.confirm = nil
			return m.stopAndQuit()
		case key.Matches(msg, keys.No):
			m.confirm = nil
		}
		return nil
	}
	if m.input != nil {
		return m.onInputKey(msg)
	}
	if key.Matches(msg, keys.ForceQuit) {
		return m.quit()
	}
	if m.screen() == scrOnboarding {
		return m.onboardingKey(msg)
	}
	l := m.activeList()
	if l.SettingFilter() {
		return m.updateList(m.screen(), msg)
	}
	switch {
	case key.Matches(msg, keys.Quit):
		return m.quit()
	case key.Matches(msg, keys.Dashboard):
		m.goTo(scrDashboard)
		return m.rebuildDashboard()
	case key.Matches(msg, keys.Groups):
		m.goTo(scrGroups)
		return m.rebuildGroups()
	case key.Matches(msg, keys.Login):
		return m.relogin()
	}
	var handled bool
	var cmd tea.Cmd
	switch m.screen() {
	case scrProfiles:
		handled, cmd = m.profilesKey(msg)
	case scrResources:
		handled, cmd = m.resourcesKey(msg)
	case scrPorts:
		handled, cmd = m.portsKey(msg)
	case scrBastion:
		handled, cmd = m.bastionKey(msg)
	case scrDashboard:
		handled, cmd = m.dashboardKey(msg)
	case scrGroups:
		handled, cmd = m.groupsKey(msg)
	}
	if handled {
		return cmd
	}
	if key.Matches(msg, keys.Back) && l.FilterState() == list.Unfiltered {
		m.back()
		return nil
	}
	return m.updateList(m.screen(), msg)
}

func (m *model) onInputKey(msg tea.KeyMsg) tea.Cmd {
	in := m.input
	switch msg.Type {
	case tea.KeyEsc:
		m.input = nil
		return nil
	case tea.KeyEnter:
		cmd, err := in.submit(in.ti.Value())
		if err != nil {
			in.err = err.Error()
			return nil
		}
		m.input = nil
		return cmd
	}
	var cmd tea.Cmd
	in.ti, cmd = in.ti.Update(msg)
	in.err = ""
	return cmd
}

func (m *model) prompt(label, placeholder string, submit func(string) (tea.Cmd, error)) tea.Cmd {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.Prompt = ""
	ti.Width = 40
	m.input = &inputPrompt{label: label, ti: ti, submit: submit}
	return m.input.ti.Focus()
}

func (m *model) quit() tea.Cmd {
	if n := m.a.Tunnels.ActiveCount(); n > 0 {
		m.confirm = &confirmPrompt{
			text: fmt.Sprintf("%d tunnel(s) active. Stop them and quit?", n),
			yes:  m.stopAndQuit,
		}
		return nil
	}
	return tea.Quit
}

func (m *model) stopAndQuit() tea.Cmd {
	a := m.a
	return m.run("stopping tunnels", time.Minute, func(context.Context) tea.Msg {
		a.Close()
		return quitMsg{}
	})
}

// ---- tunnel events ----

func (m *model) onEvent(ev tunnel.Event) tea.Cmd {
	label := ev.Session.Spec.Label
	switch ev.Session.Status {
	case tunnel.AuthExpired:
		m.flash, m.flashErr = fmt.Sprintf("%s: credentials expired, press L to log in again", label), true
	case tunnel.Failed:
		m.flash, m.flashErr = fmt.Sprintf("%s failed: %s", label, ev.Session.Err), true
	case tunnel.Up:
		m.setFlash(fmt.Sprintf("%s is up on %s", label, localAddr(ev.Session.Spec)))
	}
	cmd := m.rebuildDashboard()
	if m.screen() == scrPorts {
		cmd = tea.Batch(cmd, m.rebuildPorts())
	}
	return cmd
}

// ---- login ----

// relogin logs in again for auth-expired tunnels/profiles, or the profile
// under the cursor when nothing is expired.
func (m *model) relogin() tea.Cmd {
	names := m.bannerProfiles()
	if len(names) == 0 {
		switch m.screen() {
		case scrProfiles:
			if k, ok := selectedKey(m.lists[scrProfiles]); ok {
				names = []string{k}
			}
		case scrResources, scrPorts, scrBastion:
			names = []string{m.cur.Name}
		}
	}
	if len(names) == 0 {
		m.setFlash("nothing needs a login")
		return nil
	}
	return m.loginProfiles(names)
}

func (m *model) loginProfiles(names []string) tea.Cmd {
	sessions, profs, unknown := loginPlan(names, m.profiles)
	var cmds []*exec.Cmd
	for _, s := range sessions {
		cmds = append(cmds, awsx.SSOLoginCmd(m.awsPath(), s))
	}
	for _, n := range profs {
		if p, ok := m.profileByName(n); ok {
			if c := m.a.LoginCmd(p); c != nil {
				cmds = append(cmds, c)
			}
		}
	}
	if len(cmds) == 0 {
		m.setErr(fmt.Errorf("cannot log in %s from awsport: not SSO profiles; refresh their credentials outside awsport", strings.Join(unknown, ", ")))
		return nil
	}
	m.loginQueue = cmds[1:]
	return execLogin(cmds[0])
}

func execLogin(c *exec.Cmd) tea.Cmd {
	return tea.ExecProcess(c, func(err error) tea.Msg { return loginDoneMsg{err: err} })
}

func (m *model) onLoginDone(msg loginDoneMsg) tea.Cmd {
	if msg.err != nil {
		m.loginQueue = nil
		m.setErr(fmt.Errorf("login failed: %w", msg.err))
		return nil
	}
	if len(m.loginQueue) > 0 {
		next := m.loginQueue[0]
		m.loginQueue = m.loginQueue[1:]
		return execLogin(next)
	}
	m.authProfiles = map[string]bool{}
	m.setFlash("login complete")
	mgr := m.a.Tunnels
	cmds := []tea.Cmd{
		m.run("restarting tunnels", tunnelOpTimeout, func(context.Context) tea.Msg {
			return restartedMsg{keys: mgr.RestartAuthExpired()}
		}),
		m.checkAllCreds(),
	}
	if (m.screen() == scrResources || m.screen() == scrPorts) && m.resErr[m.cur.Name] != nil {
		cmds = append(cmds, m.discover(m.cur))
	}
	return tea.Batch(cmds...)
}

func (m *model) profileByName(name string) (core.Profile, bool) {
	for _, p := range m.profiles {
		if p.Name == name {
			return p, true
		}
	}
	return core.Profile{}, false
}

// ---- layout & view ----

func (m *model) bodyHeight() int {
	h := m.height - lipgloss.Height(m.headerView()) - lipgloss.Height(m.footerView())
	if p := m.preamble(); p != "" {
		h -= lipgloss.Height(p)
	}
	if h < 3 {
		h = 3
	}
	return h
}

func (m *model) layout() {
	if m.width == 0 {
		return
	}
	m.help.Width = m.width
	if l := m.activeList(); l != nil {
		l.SetSize(m.width-4, m.bodyHeight())
	}
	m.onb.setWidth(m.width - 6)
}

func (m *model) View() string {
	if m.width == 0 {
		return ""
	}
	parts := []string{m.headerView()}
	if p := m.preamble(); p != "" {
		parts = append(parts, p)
	}
	var body string
	if m.screen() == scrOnboarding {
		body = m.onboardingView()
	} else {
		body = m.activeList().View()
	}
	parts = append(parts, lipgloss.NewStyle().Height(m.bodyHeight()).MaxHeight(m.bodyHeight()).Render(bodyStyle.Render(body)))
	parts = append(parts, m.footerView())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m *model) headerView() string {
	titles := map[screen]string{
		scrOnboarding: "setup",
		scrProfiles:   "profiles",
		scrResources:  "resources",
		scrPorts:      "ports",
		scrBastion:    "bastion",
		scrDashboard:  "tunnels",
		scrGroups:     "groups",
	}
	return titleBarStyle.Render("awsport") + subtleStyle.Render(" · "+titles[m.screen()]+" · unofficial")
}

// preamble is an optional context line above the current screen's list.
func (m *model) preamble() string {
	w := lipgloss.NewStyle().Width(m.width - 4).MaxHeight(4)
	var s string
	switch m.screen() {
	case scrResources:
		if err := m.resErr[m.cur.Name]; err != nil {
			s = errStyle.Render(oneLine(err.Error()))
		}
	case scrPorts:
		s = subtleStyle.Render(resourceSummary(m.curRes))
	case scrBastion:
		if m.bastion != nil {
			s = fmt.Sprintf("%s:%d needs a bastion in %s/%s. No instance is tagged %s=true or named %q.",
				m.pending.r.Name, m.pending.remote, m.bastion.Profile, m.bastion.Region, core.TagBastion, core.DefaultBastionName)
			if len(m.bastion.Candidates) == 0 {
				s += "\n" + errStyle.Render("No running SSM-online instance found; start one or tag a bastion, then retry.")
			} else {
				s += " Your choice is remembered."
			}
		}
	}
	if s == "" {
		return ""
	}
	return bodyStyle.Render(w.Render(s))
}

func (m *model) footerView() string {
	width := m.width
	if width < 20 {
		width = 20
	}
	wrap := lipgloss.NewStyle().Width(width).MaxHeight(3)
	var lines []string
	switch {
	case m.input != nil:
		l := boldStyle.Render(m.input.label) + " " + m.input.ti.View()
		if m.input.err != "" {
			l += "\n" + errStyle.Render(m.input.err)
		}
		lines = append(lines, l)
	case m.confirm != nil:
		lines = append(lines, warnStyle.Bold(true).Render(m.confirm.text)+subtleStyle.Render("  [y/n]"))
	case len(m.ops) > 0 && m.screen() != scrOnboarding:
		lines = append(lines, m.spin.View()+" "+m.opLabels())
	case m.flash != "":
		st := okStyle
		if m.flashErr {
			st = errStyle
		}
		lines = append(lines, wrap.Render(st.Render(m.flash)))
	default:
		lines = append(lines, "")
	}
	if names := m.bannerProfiles(); len(names) > 0 {
		lines = append(lines, bannerStyle.MaxWidth(width).Render(fmt.Sprintf("⚠ credentials expired for %s — press L to log in again", strings.Join(names, ", "))))
	}
	if m.depsWarning != "" && m.screen() != scrOnboarding {
		lines = append(lines, warnBanner.MaxWidth(width).Render(m.depsWarning))
	}
	n := m.a.Tunnels.ActiveCount()
	status := subtleStyle.Render("○ no active tunnels")
	if n > 0 {
		status = okStyle.Render(fmt.Sprintf("● %d active tunnel(s)", n))
	}
	m.help.Width = width - lipgloss.Width(status) - 3
	lines = append(lines, status+"   "+m.help.View(m.helpKeys()))
	return strings.Join(lines, "\n")
}

func (m *model) opLabels() string {
	seen := map[string]bool{}
	var labels []string
	for i := 1; i <= m.opSeq; i++ {
		if l, ok := m.ops[i]; ok && !seen[l] {
			seen[l] = true
			labels = append(labels, l)
		}
	}
	return strings.Join(labels, ", ") + "…"
}

func (m *model) helpKeys() help.KeyMap {
	if m.input != nil {
		return helpKeys{bind("enter", "confirm", "enter"), bind("esc", "cancel", "esc")}
	}
	if m.confirm != nil {
		return helpKeys{keys.Yes, keys.No}
	}
	global := []key.Binding{keys.Dashboard, keys.Groups, keys.Login, keys.Quit}
	switch m.screen() {
	case scrOnboarding:
		return m.onboardingHelp()
	case scrProfiles:
		return helpKeys(append([]key.Binding{keys.Enter, keys.Filter, bind("r", "reload", "r"), keys.Onboard}, global...))
	case scrResources:
		return helpKeys(append([]key.Binding{keys.Enter, keys.Filter, keys.ShowAll, keys.AddPort, keys.Shell, keys.Refresh, keys.Back}, global...))
	case scrPorts:
		return helpKeys(append([]key.Binding{bind("enter", "connect", "enter"), keys.AddPort, keys.Back}, global...))
	case scrBastion:
		return helpKeys{bind("enter", "use as bastion", "enter"), keys.Back, keys.Quit}
	case scrDashboard:
		return helpKeys{keys.Select, keys.SaveGroup, keys.Copy, keys.Stop, keys.Restart, keys.Login, keys.Groups, keys.Back, keys.Quit}
	case scrGroups:
		return helpKeys{bind("enter", "start group", "enter"), keys.Delete, keys.Filter, keys.Dashboard, keys.Back, keys.Quit}
	}
	return helpKeys(global)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// errorsSummary joins up to three errors into one line.
func errorsSummary(errs []error) string {
	var parts []string
	for i, err := range errs {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(errs)-3))
			break
		}
		parts = append(parts, oneLine(err.Error()))
	}
	return strings.Join(parts, "; ")
}

var errNoSelection = errors.New("nothing selected")
