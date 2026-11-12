// Package tui is awsport's interactive Bubble Tea interface.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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
	"github.com/nhl4000/awsport/internal/login"
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

// Run starts the full-screen UI and blocks until the user quits. Tunnels
// still running when it returns stay up in the background agent.
func Run(a *app.App) error {
	m := newModel(a)
	g := guard(m)
	_, err := tea.NewProgram(g, tea.WithAltScreen()).Run()
	m.cancel()
	n := a.Tunnels.ActiveCount()
	a.Close()
	if n > 0 {
		fmt.Fprintf(os.Stderr, "awsport: %d tunnel(s) still running in the background (awsport ps lists them, awsport down --all stops them)\n", n)
	}
	g.repanic(err)
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
	resumedMsg      struct {
		n       int
		warning string
	}
	depsMsg struct {
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
	restartedMsg struct {
		keys []string
		err  error
	}
	tunnelOpMsg struct {
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
	// fullHelp shows every key in columns instead of the one-line summary.
	fullHelp bool

	flash        string
	flashLvl     flashLevel
	flashAt      time.Time
	authProfiles map[string]bool // profiles whose AWS calls failed with auth errors
	depsWarning  string
	started      time.Time // UI start; older Up events are snapshots, not news

	modal      *modal
	input      *inputPrompt
	loginQueue []*login.Cmd

	profiles  []core.Profile
	creds     map[string]credState
	credSem   chan struct{}
	resources map[string][]core.Resource
	resAt     map[string]time.Time               // when resources[profile] was discovered
	resDisk   map[string]bool                    // resources[profile] came from the disk cache and awaits revalidation
	resErr    map[string]error                   // discovery failures other than missing permissions
	resPerms  map[string][]*awsx.PermissionError // discovery calls denied by IAM, per profile
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
		started:      time.Now(),
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
		resAt:        map[string]time.Time{},
		resDisk:      map[string]bool{},
		resErr:       map[string]error{},
		resPerms:     map[string][]*awsx.PermissionError{},
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
	cmds := []tea.Cmd{m.waitEvent(), m.tick(), m.checkResumed()}
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

func (m *model) setFlash(s string) { m.setStatus(s, flashOK) }

func (m *model) setWarn(s string) { m.setStatus(s, flashWarn) }

// setStatus shows s in the status line until flashExpired says otherwise.
func (m *model) setStatus(s string, lvl flashLevel) {
	m.flash, m.flashLvl, m.flashAt = s, lvl, time.Now()
}

// setErr shows err in the status line. Errors that consist only of missing
// IAM permissions are shown as a calm warning with the admin hint.
func (m *model) setErr(err error) {
	if perms := awsx.PermissionErrors(err); len(perms) > 0 && awsx.WithoutPermissionErrors(err) == nil {
		m.setWarn(permissionNotice(perms))
		return
	}
	m.setStatus(oneLine(err.Error()), flashError)
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
		if m.flash != "" && flashExpired(m.flashLvl, m.flashAt, time.Now()) {
			m.flash = ""
		}
		switch m.screen() {
		case scrDashboard:
			return tea.Batch(m.rebuildDashboard(), m.tick())
		case scrResources:
			m.lists[scrResources].Title = m.resourcesTitle()
		}
		return m.tick()
	case eventMsg:
		return tea.Batch(m.onEvent(tunnel.Event(msg)), m.waitEvent())
	case eventsClosedMsg:
		return nil
	case resumedMsg:
		return m.onResumed(msg)
	case stoppedAllMsg:
		return m.onStoppedAll(msg)
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
	case bugDoneMsg:
		var exit *exec.ExitError
		switch {
		case errors.As(msg.err, &exit):
			m.setWarn("bug report not filed (awsport bug exited with status " + strconv.Itoa(exit.ExitCode()) + ")")
		case msg.err != nil:
			m.setErr(fmt.Errorf("awsport bug: %w", msg.err))
		}
		return nil
	case loginDoneMsg:
		return m.onLoginDone(msg)
	case restartedMsg:
		if msg.err != nil {
			m.setErr(fmt.Errorf("restart tunnels: %w", msg.err))
		} else if len(msg.keys) > 0 {
			m.setFlash(fmt.Sprintf("logged in; restarting %d tunnel(s)", len(msg.keys)))
		}
		return m.rebuildTunnelViews()
	case tunnelOpMsg:
		if msg.err != nil {
			m.setErr(fmt.Errorf("%s %s: %w", msg.verb, msg.label, msg.err))
		} else {
			m.setFlash(fmt.Sprintf("%s %s", msg.verb, msg.label))
		}
		return m.rebuildTunnelViews()
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

// setItems replaces a list's items, keeping the cursor on the same key when
// it is still listed; filtering follow-ups are tagged.
func (m *model) setItems(id screen, items []list.Item) tea.Cmd {
	l := m.lists[id]
	prev, hadPrev := selectedKey(l)
	cmd := l.SetItems(items)
	if hadPrev && l.FilterState() == list.Unfiltered {
		if i := itemIndex(items, prev); i >= 0 {
			l.Select(i)
		}
	}
	return tagList(id, cmd)
}

func itemIndex(items []list.Item, k string) int {
	for i, it := range items {
		if x, ok := it.(item); ok && x.key == k {
			return i
		}
	}
	return -1
}

func (m *model) onKey(msg tea.KeyMsg) tea.Cmd {
	if m.modal != nil {
		return m.onModalKey(msg)
	}
	if key.Matches(msg, keys.ForceQuit) {
		return m.quit()
	}
	if m.input != nil {
		return m.onInputKey(msg)
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
	case key.Matches(msg, keys.Help):
		m.fullHelp = !m.fullHelp
		return nil
	case key.Matches(msg, keys.Bug):
		return m.reportBug()
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

// ---- tunnel events ----

func (m *model) onEvent(ev tunnel.Event) tea.Cmd {
	label := ev.Session.Spec.Label
	switch ev.Session.Status {
	case tunnel.AuthExpired:
		m.setStatus(fmt.Sprintf("%s: credentials expired, press L to log in again", label), flashError)
	case tunnel.Failed:
		m.setStatus(fmt.Sprintf("%s failed: %s", label, ev.Session.Err), flashError)
	case tunnel.Up:
		// Snapshots of tunnels that came up before this UI started are not news.
		if !ev.Session.Since.Before(m.started) {
			m.setFlash(fmt.Sprintf("%s is up on %s", label, linkAddr(ev.Session.Spec)))
		}
	}
	return m.rebuildTunnelViews()
}

// rebuildTunnelViews refreshes every list that shows tunnel state.
func (m *model) rebuildTunnelViews() tea.Cmd {
	return tea.Batch(m.rebuildDashboard(), m.rebuildPorts(), m.rebuildResources(), m.rebuildProfiles(), m.rebuildGroups())
}

// checkResumed looks for tunnels kept running by the agent since an earlier
// session. It runs off the UI goroutine since it may connect to the agent.
func (m *model) checkResumed() tea.Cmd {
	c := m.a.Tunnels
	return func() tea.Msg {
		msg := resumedMsg{n: len(c.Sessions())}
		if w, ok := c.(interface{ Warning() string }); ok {
			msg.warning = w.Warning()
		}
		return msg
	}
}

func (m *model) onResumed(msg resumedMsg) tea.Cmd {
	var parts []string
	if msg.n > 0 {
		parts = append(parts, fmt.Sprintf("%s still running in the background — t shows them", plural(msg.n, "tunnel")))
	}
	if msg.warning != "" {
		parts = append(parts, oneLine(msg.warning))
	}
	if len(parts) == 0 {
		return nil
	}
	lvl := flashOK
	if msg.warning != "" {
		lvl = flashError
	}
	m.setStatus(strings.Join(parts, "; "), lvl)
	return m.rebuildTunnelViews()
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
	var cmds []*login.Cmd
	for _, s := range sessions {
		cmds = append(cmds, login.SSOSession(m.a.AWSPath, s))
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

func execLogin(c *login.Cmd) tea.Cmd {
	return tea.Exec(c, func(err error) tea.Msg { return loginDoneMsg{err: err} })
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
			keys, err := mgr.RestartAuthExpired()
			return restartedMsg{keys: keys, err: err}
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
	switch {
	case m.modal != nil:
		body = lipgloss.Place(m.width-4, m.bodyHeight(), lipgloss.Center, lipgloss.Center, m.modalView(m.width-4))
	case m.screen() == scrOnboarding:
		body = m.onboardingView()
	default:
		body = m.listView()
	}
	parts = append(parts, lipgloss.NewStyle().Height(m.bodyHeight()).MaxHeight(m.bodyHeight()).Render(bodyStyle.Render(body)))
	parts = append(parts, m.footerView())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// headerView is the title bar with the navigation stack as breadcrumbs,
// e.g. "profiles › acme-dev › web-app".
func (m *model) headerView() string {
	crumbs := make([]string, 0, len(m.stack))
	for i, s := range m.stack {
		c := m.crumb(s)
		if i == len(m.stack)-1 {
			c = boldStyle.Render(c)
		} else {
			c = subtleStyle.Render(c)
		}
		crumbs = append(crumbs, c)
	}
	line := titleBarStyle.Render("awsport") + " " + strings.Join(crumbs, subtleStyle.Render(" › ")) + subtleStyle.Render(" · unofficial")
	return lipgloss.NewStyle().MaxWidth(max(m.width, 20)).Render(line)
}

func (m *model) crumb(s screen) string {
	switch s {
	case scrOnboarding:
		return "setup"
	case scrProfiles:
		return "profiles"
	case scrResources:
		return m.cur.Name
	case scrPorts:
		if m.curRes.Name != "" {
			return m.curRes.Name
		}
		return m.curRes.ID
	case scrBastion:
		return "pick bastion"
	case scrDashboard:
		return "tunnels"
	case scrGroups:
		return "groups"
	}
	return ""
}

// listView renders the current list, or its title and an explanation when
// the list is empty.
func (m *model) listView() string {
	l := m.activeList()
	if len(l.Items()) > 0 || l.FilterState() != list.Unfiltered {
		return l.View()
	}
	text := m.emptyText()
	if text == "" {
		return l.View()
	}
	title := l.Styles.TitleBar.Render(l.Styles.Title.Render(l.Title))
	return title + "\n\n" + lipgloss.NewStyle().Width(max(m.width-8, 20)).PaddingLeft(2).Render(text)
}

// emptyText explains an empty list and what to do next.
func (m *model) emptyText() string {
	switch m.screen() {
	case scrProfiles:
		return fmt.Sprintf("No AWS profiles in %s.\n\nPress o to set up SSO or import a team config, or r to reload.", awsx.DisplayPath(awsx.ConfigFilePath()))
	case scrResources:
		return m.resourcesEmptyText()
	case scrDashboard:
		return "No tunnels running.\n\n" +
			"To start one, open a profile (esc), pick a resource and press enter on a port.\n" +
			"Press g to start a saved group."
	case scrGroups:
		return "No groups yet.\n\n" +
			"A group starts several tunnels at once. On the tunnels screen (t), mark tunnels with space and press s to save them.\n" +
			"Groups from a team config show up here too."
	case scrBastion:
		return "Nothing to pick. Press esc to go back."
	}
	return ""
}

func (m *model) resourcesEmptyText() string {
	name := m.cur.Name
	regions := strings.Join(awsx.Regions(m.cur, m.a.Cfg.Regions), ", ")
	err := m.resErr[name]
	switch {
	case m.loading[name]:
		return m.spin.View() + fmt.Sprintf(" Discovering EC2 instances and RDS databases in %s (%s)…", name, regions)
	case err != nil && awsx.IsAuthError(err):
		return "Nothing to show until you log in again (L)."
	case err != nil:
		return "Can't list resources (error above).\n\nPress r to retry."
	case hiddenCount(m.resources[name]) > 0:
		return fmt.Sprintf("No connectable resources.\n\n%d unreachable resource(s) are hidden; press a to show them and why.", hiddenCount(m.resources[name]))
	case len(m.resPerms[name]) > 0:
		return "Nothing this role is allowed to list (see above).\n\nPress r to retry after your permissions change."
	}
	return fmt.Sprintf("No EC2 instances or RDS databases found in %s (%s).\n\n"+
		"Wrong region? Set regions in the awsport config or the profile's region. Press r to refresh.", name, regions)
}

// preamble is optional context above the current screen's list: discovery
// problems, the selected resource, or the full error of a failed tunnel.
func (m *model) preamble() string {
	width := m.width - 4
	var s string
	switch m.screen() {
	case scrResources:
		var parts []string
		if panel := permissionPanel(m.resPerms[m.cur.Name], width); panel != "" {
			parts = append(parts, panel)
		}
		switch err := m.resErr[m.cur.Name]; {
		case err == nil:
		case awsx.IsAuthError(err):
			parts = append(parts, errStyle.Render(fmt.Sprintf("✗ The login for %s expired. Press L to log in again; resources reload afterwards.", m.cur.Name)))
		default:
			parts = append(parts, errStyle.Render(oneLine(err.Error())))
		}
		s = strings.Join(parts, "\n")
	case scrPorts:
		s = subtleStyle.Render(resourceSummary(m.curRes))
		if remote, sess, exists, ok := m.selectedPort(); ok && exists {
			if f := failureText(sess); f != "" {
				s += "\n" + errStyle.Render(fmt.Sprintf("✗ port %d %s", remote, f))
			}
		}
	case scrDashboard:
		if sess, ok := m.selectedSession(); ok {
			if f := failureText(sess); f != "" {
				s = errStyle.Render("✗ " + sess.Spec.Label + " " + f)
			}
		}
	case scrBastion:
		if m.bastion != nil {
			s = fmt.Sprintf("%s:%d needs a bastion in %s/%s. No SSM-online instance is tagged %s=true or named %q.",
				m.pending.r.Name, m.pending.remote, m.bastion.Profile, m.bastion.Region, core.TagBastion, core.DefaultBastionName)
			if len(m.bastion.Candidates) == 0 {
				s += "\n" + errStyle.Render("No running SSM-online instance found; start one or tag a bastion, then retry.")
				if ssmDenied(m.resPerms[m.bastion.Profile]) {
					s += "\n" + warnStyle.Render("SSM agent status couldn't be checked (missing ssm:DescribeInstanceInformation), so no instance counts as online.")
				}
			} else {
				s += " Your choice is remembered."
			}
		}
	}
	if s == "" {
		return ""
	}
	return bodyStyle.Render(lipgloss.NewStyle().Width(width).MaxHeight(maxPreambleHeight).Render(s))
}

// permissionPanel renders denied discovery calls as a small warning box.
func permissionPanel(perms []*awsx.PermissionError, width int) string {
	warnings, hints := permissionLines(perms)
	if len(warnings) == 0 {
		return ""
	}
	lines := make([]string, 0, len(warnings)+len(hints))
	for _, w := range warnings {
		lines = append(lines, warnStyle.Render(w))
	}
	for _, h := range hints {
		lines = append(lines, subtleStyle.Render(h))
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorWarn).Padding(0, 1).Width(max(width-2, 10))
	return box.Render(strings.Join(lines, "\n"))
}

func (m *model) footerView() string {
	width := m.width
	if width < 20 {
		width = 20
	}
	wrap := lipgloss.NewStyle().Width(width).MaxHeight(4)
	var lines []string
	switch {
	case m.input != nil:
		l := boldStyle.Render(m.input.label) + " " + m.input.ti.View()
		if m.input.err != "" {
			l += "\n" + errStyle.Render(m.input.err)
		}
		lines = append(lines, l)
	case len(m.ops) > 0 && m.screen() != scrOnboarding:
		lines = append(lines, m.spin.View()+" "+m.opLabels())
	case m.flash != "":
		st := okStyle
		switch m.flashLvl {
		case flashWarn:
			st = warnStyle
		case flashError:
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
		status = okStyle.Render("● " + plural(n, "active tunnel"))
	}
	km := m.helpKeys()
	sh, isScreen := km.(screenHelp)
	if isScreen && m.fullHelp {
		m.help.ShowAll = true
		m.help.Width = width
		lines = append(lines, status, m.help.View(sh))
		return strings.Join(lines, "\n")
	}
	m.help.ShowAll = false
	m.help.Width = width - lipgloss.Width(status) - 3
	if isScreen {
		km = sh.fit(m.help.Width)
	}
	lines = append(lines, status+"   "+m.help.View(km))
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
	if m.modal != nil {
		if m.modal.kind == modalQuit {
			return helpKeys{bind("k/q", "keep & quit", "k"), bind("s", "stop all & quit", "s"), bind("esc", "cancel", "esc")}
		}
		return helpKeys{keys.Yes, keys.No}
	}
	if m.input != nil {
		return helpKeys{bind("enter", "confirm", "enter"), bind("esc", "cancel", "esc")}
	}
	if m.screen() == scrOnboarding {
		return m.onboardingHelp()
	}
	l := m.activeList()
	if l.SettingFilter() {
		return helpKeys{bind("enter", "apply filter", "enter"), bind("esc", "cancel", "esc")}
	}
	back := keys.Back
	var clear []key.Binding
	if l.FilterState() == list.FilterApplied {
		back = bind("esc", "clear filter", "esc")
		clear = []key.Binding{back}
	}
	empty := len(l.Items()) == 0
	switch m.screen() {
	case scrProfiles:
		return screenHelp{keys: append(clear, bind("enter", "open", "enter"), keys.Filter, keys.Dashboard, bind("r", "reload", "r"), keys.Onboard)}
	case scrResources:
		return screenHelp{keys: []key.Binding{bind("enter", "open", "enter"), back, keys.Refresh, keys.Filter, keys.ShowAll, keys.AddPort, keys.Shell}}
	case scrPorts:
		return screenHelp{keys: []key.Binding{bind("enter", "start/stop", "enter"), keys.Copy, back, keys.Open, keys.Stop, keys.AddPort, keys.Shell}}
	case scrBastion:
		if empty {
			return screenHelp{keys: []key.Binding{back}}
		}
		return screenHelp{keys: []key.Binding{bind("enter", "use as bastion", "enter"), back}}
	case scrDashboard:
		self := []key.Binding{keys.Dashboard}
		if empty {
			return screenHelp{keys: []key.Binding{back, keys.Groups}, omit: self}
		}
		return screenHelp{keys: []key.Binding{bind("enter", "stop/restart", "enter"), keys.Select, keys.SaveGroup, back, keys.Copy, keys.Open, keys.Stop, keys.Restart}, omit: self}
	case scrGroups:
		self := []key.Binding{keys.Groups}
		if empty && clear == nil {
			return screenHelp{keys: []key.Binding{back, keys.Dashboard}, omit: self}
		}
		return screenHelp{keys: []key.Binding{bind("enter", "start group", "enter"), keys.Delete, back, keys.Filter}, omit: self}
	}
	return screenHelp{}
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
