package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/login"
)

type onbStep int

const (
	onbDeps      onbStep = iota // prerequisite check / install offer
	onbSSOChoice                // pick how to configure SSO
	onbImport                   // team config URL/path entry
	onbManual                   // start URL + region entry
	onbWorking                  // saving config, logging in, syncing
	onbFailed                   // error with retry
)

type choiceKind int

const (
	choiceConfigured choiceKind = iota
	choiceImport
	choiceReuse
	choiceManual
	choiceSkip
)

type ssoChoice struct {
	kind        choiceKind
	title, desc string
}

type foundSession struct {
	name, url, region string
	ok                bool
}

type onboarding struct {
	step     onbStep
	fromApp  bool // opened from the profile list; esc returns there
	checks   []deps.Check
	choices  []ssoChoice
	cursor   int
	found    foundSession
	importIn textinput.Model
	fields   []textinput.Model // start URL, region, session name
	focus    int
	working  string
	err      string
	retry    func() tea.Cmd
	back     onbStep // step esc returns to from onbFailed
}

// Onboarding messages.
type (
	ssoFoundMsg struct {
		found foundSession
		err   error
	}
	cfgMsg struct {
		cfg  *config.Config
		err  error
		from onbStep
	}
	ensureMsg   struct{ err error }
	onbLoginMsg struct{ err error }
	syncMsg     struct {
		n   int
		err error
	}
	installDoneMsg struct{ err error }
)

func newOnboarding(fromApp bool) onboarding {
	o := onboarding{step: onbDeps, fromApp: fromApp}
	o.importIn = textinput.New()
	o.importIn.Prompt = "> "
	o.importIn.Placeholder = "https://example.com/awsport.team.yaml or ./awsport.team.yaml"
	placeholders := []string{"https://d-0000000000.awsapps.com/start", "us-east-1", defaultSSOSession}
	prompts := []string{"Start URL    ", "SSO region   ", "Session name "}
	for i := range placeholders {
		ti := textinput.New()
		ti.Prompt = prompts[i]
		ti.Placeholder = placeholders[i]
		o.fields = append(o.fields, ti)
	}
	return o
}

func (o *onboarding) setWidth(w int) {
	if w < 20 {
		w = 20
	}
	o.importIn.Width = w - 4
	for i := range o.fields {
		o.fields[i].Width = w - 16
	}
}

// updateInputs forwards non-key messages (cursor blink) to the focused input.
func (o *onboarding) updateInputs(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch o.step {
	case onbImport:
		o.importIn, cmd = o.importIn.Update(msg)
	case onbManual:
		o.fields[o.focus], cmd = o.fields[o.focus].Update(msg)
	}
	return cmd
}

func (o *onboarding) focusField(i int) tea.Cmd {
	for j := range o.fields {
		o.fields[j].Blur()
	}
	o.focus = (i + len(o.fields)) % len(o.fields)
	return o.fields[o.focus].Focus()
}

func (m *model) onDeps(msg depsMsg) tea.Cmd {
	if depsOK(msg.checks) {
		m.depsWarning = ""
	} else {
		m.depsWarning = fmt.Sprintf("⚠ missing prerequisite(s): %s — press o to set up", strings.Join(failingNames(msg.checks), ", "))
	}
	if msg.background || m.screen() != scrOnboarding || m.onb.step != onbDeps {
		return nil
	}
	m.onb.checks = msg.checks
	if depsOK(msg.checks) {
		return m.enterSSOChoice()
	}
	return nil
}

func (m *model) enterSSOChoice() tea.Cmd {
	m.onb.step = onbSSOChoice
	m.onb.choices = nil
	m.onb.err = ""
	return m.run("looking for existing SSO sessions", configTimeout, func(context.Context) tea.Msg {
		name, url, region, ok, err := awsx.FindSSOSession()
		return ssoFoundMsg{found: foundSession{name: name, url: url, region: region, ok: ok}, err: err}
	})
}

func (m *model) buildChoices() {
	var cs []ssoChoice
	if sso := m.a.Cfg.SSO; sso.StartURL != "" {
		cs = append(cs, ssoChoice{choiceConfigured, "Use configured SSO", fmt.Sprintf("%s (%s)", sso.StartURL, sso.Region)})
	}
	cs = append(cs, ssoChoice{choiceImport, "Import team config", "URL or file path shared by your team (SSO, regions, port presets, groups)"})
	if f := m.onb.found; f.ok {
		cs = append(cs, ssoChoice{choiceReuse, "Reuse sso-session " + f.name, fmt.Sprintf("found in %s: %s (%s)", awsx.DisplayPath(awsx.ConfigFilePath()), f.url, f.region)})
	}
	cs = append(cs, ssoChoice{choiceManual, "Enter SSO start URL and region", "from your AWS access portal"})
	if len(m.profiles) > 0 {
		cs = append(cs, ssoChoice{choiceSkip, "Skip", "use the profiles already in your AWS config"})
	}
	m.onb.choices = cs
	m.onb.cursor = 0
}

// updateOnboardingMsg handles onboarding-only messages.
func (m *model) updateOnboardingMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case ssoFoundMsg:
		m.onb.found = msg.found
		if msg.err != nil {
			m.onb.err = "could not read existing AWS config: " + oneLine(msg.err.Error())
		}
		m.buildChoices()
		return nil, true
	case installDoneMsg:
		if msg.err != nil {
			m.setErr(fmt.Errorf("install: %w", msg.err))
		} else {
			m.setFlash("install finished; rechecking")
		}
		m.onb.step = onbDeps
		m.onb.checks = nil
		return m.checkDeps(false), true
	case cfgMsg:
		if msg.err != nil {
			from := msg.from
			return m.onbFail(fmt.Errorf("save configuration: %w", msg.err), from, func() tea.Cmd { return m.enterStep(from) }), true
		}
		m.a.Cfg = msg.cfg
		if strings.TrimSpace(msg.cfg.SSO.StartURL) == "" {
			cmd := m.enterStep(onbManual)
			m.onb.err = "the team config has no sso.start_url; enter it below"
			return cmd, true
		}
		return m.ensureAndLogin(), true
	case ensureMsg:
		if msg.err != nil {
			return m.onbFail(fmt.Errorf("write sso-session to %s: %w", awsx.DisplayPath(awsx.ConfigFilePath()), msg.err), onbSSOChoice, m.ensureAndLogin), true
		}
		m.onb.working = "waiting for SSO login in your browser"
		session := m.a.SSOConfig().Session
		return tea.Exec(login.SSOSession(m.a.AWSPath, session), func(err error) tea.Msg { return onbLoginMsg{err: err} }), true
	case onbLoginMsg:
		if msg.err != nil {
			return m.onbFail(fmt.Errorf("SSO login: %w", msg.err), onbSSOChoice, m.ensureAndLogin), true
		}
		return m.syncProfiles(), true
	case syncMsg:
		if msg.err != nil {
			retry := m.syncProfiles
			if errors.Is(msg.err, awsx.ErrSSOTokenExpired) {
				retry = m.ensureAndLogin
			}
			return m.onbFail(fmt.Errorf("sync SSO profiles: %w", msg.err), onbSSOChoice, retry), true
		}
		return m.finishOnboarding(fmt.Sprintf("synced %d SSO profile(s)", msg.n)), true
	}
	return nil, false
}

func (m *model) onbFail(err error, back onbStep, retry func() tea.Cmd) tea.Cmd {
	m.onb.step = onbFailed
	m.onb.err = oneLine(err.Error())
	m.onb.back = back
	m.onb.retry = retry
	return nil
}

func (m *model) enterStep(s onbStep) tea.Cmd {
	m.onb.err = ""
	m.onb.step = s
	switch s {
	case onbImport:
		if src := m.a.Cfg.TeamSource; src != "" && m.onb.importIn.Value() == "" {
			m.onb.importIn.SetValue(src)
		}
		return m.onb.importIn.Focus()
	case onbManual:
		sso := m.a.Cfg.SSO
		vals := []string{sso.StartURL, sso.Region, sso.Session}
		for i, v := range vals {
			if m.onb.fields[i].Value() == "" && v != "" {
				m.onb.fields[i].SetValue(v)
			}
		}
		return m.onb.focusField(0)
	case onbSSOChoice:
		return m.enterSSOChoice()
	case onbDeps:
		m.onb.checks = nil
		return m.checkDeps(false)
	}
	return nil
}

func (m *model) importTeam(src string) tea.Cmd {
	m.onb.step = onbWorking
	m.onb.working = "importing team config"
	return m.run("importing team config", configTimeout, func(ctx context.Context) tea.Msg {
		if _, err := config.ImportTeam(ctx, src); err != nil {
			return cfgMsg{err: err, from: onbImport}
		}
		cfg, err := config.Load()
		return cfgMsg{cfg: cfg, err: err, from: onbImport}
	})
}

// persistSSO saves sso into the user config and reloads the effective config.
func (m *model) persistSSO(sso config.SSO, from onbStep) tea.Cmd {
	m.onb.step = onbWorking
	m.onb.working = "saving SSO settings"
	return m.run("saving SSO settings", configTimeout, func(context.Context) tea.Msg {
		u, err := config.LoadUser()
		if err != nil {
			return cfgMsg{err: err, from: from}
		}
		u.SSO = sso
		if err := config.SaveUser(u); err != nil {
			return cfgMsg{err: err, from: from}
		}
		cfg, err := config.Load()
		return cfgMsg{cfg: cfg, err: err, from: from}
	})
}

func (m *model) ensureAndLogin() tea.Cmd {
	m.onb.step = onbWorking
	m.onb.working = "preparing AWS config"
	a := m.a
	return m.run("preparing AWS config", configTimeout, func(context.Context) tea.Msg {
		return ensureMsg{err: a.EnsureSSOSession()}
	})
}

func (m *model) syncProfiles() tea.Cmd {
	m.onb.step = onbWorking
	m.onb.working = "syncing SSO accounts and roles"
	a := m.a
	return m.run("syncing SSO accounts and roles", syncTimeout, func(ctx context.Context) tea.Msg {
		n, err := a.SyncSSOProfiles(ctx)
		return syncMsg{n: n, err: err}
	})
}

func (m *model) finishOnboarding(flash string) tea.Cmd {
	m.onb = onboarding{}
	m.resetTo(scrProfiles)
	if flash == "" {
		return tea.Batch(m.rebuildProfiles(), m.checkAllCreds())
	}
	return m.loadProfiles(flash)
}

func (m *model) leaveOnboarding() tea.Cmd {
	if m.onb.fromApp {
		m.onb = onboarding{}
		m.back()
	}
	return nil
}

func (m *model) onboardingKey(msg tea.KeyMsg) tea.Cmd {
	o := &m.onb
	if key.Matches(msg, keys.Bug) && (o.step == onbDeps || o.step == onbSSOChoice || o.step == onbFailed) {
		return m.reportBug()
	}
	switch o.step {
	case onbDeps:
		switch {
		case key.Matches(msg, keys.Quit):
			return m.quit()
		case o.checks == nil:
			return nil
		case key.Matches(msg, keys.Install):
			return m.offerInstall()
		case key.Matches(msg, keys.Recheck):
			return m.enterStep(onbDeps)
		case key.Matches(msg, keys.Skip, keys.Enter):
			return m.enterSSOChoice()
		case key.Matches(msg, keys.Back):
			return m.leaveOnboarding()
		}
	case onbSSOChoice:
		switch {
		case key.Matches(msg, keys.Quit):
			return m.quit()
		case key.Matches(msg, keys.Up):
			if o.cursor > 0 {
				o.cursor--
			}
		case key.Matches(msg, keys.Down):
			if o.cursor < len(o.choices)-1 {
				o.cursor++
			}
		case key.Matches(msg, keys.Enter):
			if o.cursor < len(o.choices) {
				return m.choose(o.choices[o.cursor].kind)
			}
		case key.Matches(msg, keys.Back):
			if o.fromApp {
				return m.leaveOnboarding()
			}
			return m.enterStep(onbDeps)
		}
	case onbImport:
		switch msg.Type {
		case tea.KeyEsc:
			return m.enterStep(onbSSOChoice)
		case tea.KeyEnter:
			src := strings.TrimSpace(o.importIn.Value())
			if src == "" {
				o.err = "enter a URL or file path"
				return nil
			}
			return m.importTeam(src)
		}
		var cmd tea.Cmd
		o.importIn, cmd = o.importIn.Update(msg)
		o.err = ""
		return cmd
	case onbManual:
		switch {
		case msg.Type == tea.KeyEsc:
			return m.enterStep(onbSSOChoice)
		case msg.Type == tea.KeyEnter && o.focus < len(o.fields)-1:
			return o.focusField(o.focus + 1)
		case msg.Type == tea.KeyEnter:
			sso := normalizeSSO(config.SSO{StartURL: o.fields[0].Value(), Region: o.fields[1].Value(), Session: o.fields[2].Value()})
			if err := validateSSO(sso); err != nil {
				o.err = err.Error()
				return nil
			}
			return m.persistSSO(sso, onbManual)
		case msg.Type == tea.KeyTab || msg.Type == tea.KeyDown:
			return o.focusField(o.focus + 1)
		case msg.Type == tea.KeyShiftTab || msg.Type == tea.KeyUp:
			return o.focusField(o.focus - 1)
		}
		var cmd tea.Cmd
		o.fields[o.focus], cmd = o.fields[o.focus].Update(msg)
		o.err = ""
		return cmd
	case onbFailed:
		switch {
		case key.Matches(msg, keys.Quit):
			return m.quit()
		case key.Matches(msg, keys.Enter):
			if o.retry != nil {
				return o.retry()
			}
		case key.Matches(msg, keys.Back):
			return m.enterStep(o.back)
		}
	case onbWorking:
		if key.Matches(msg, keys.Quit) {
			return m.quit()
		}
	}
	return nil
}

func (m *model) choose(k choiceKind) tea.Cmd {
	switch k {
	case choiceConfigured:
		return m.ensureAndLogin()
	case choiceImport:
		return m.enterStep(onbImport)
	case choiceReuse:
		f := m.onb.found
		return m.persistSSO(normalizeSSO(config.SSO{StartURL: f.url, Region: f.region, Session: f.name}), onbSSOChoice)
	case choiceManual:
		return m.enterStep(onbManual)
	default:
		return m.finishOnboarding("")
	}
}

func (m *model) offerInstall() tea.Cmd {
	c, ok := firstFailing(m.onb.checks)
	if !ok {
		return nil
	}
	if len(c.Install.Commands) == 0 {
		m.setErr(fmt.Errorf("no automatic install for %s on this system; see %s", c.Name, c.Install.DocsURL))
		return nil
	}
	text := fmt.Sprintf("Run %d command(s) to install %s now?", len(c.Install.Commands), c.Name)
	if c.Install.NeedsSudo {
		text += " (uses sudo; you may be asked for your password)"
	}
	in := c.Install
	m.confirm(text, func() tea.Cmd {
		return tea.Exec(&installExec{in: in}, func(err error) tea.Msg { return installDoneMsg{err: err} })
	})
	return nil
}

// installExec runs deps.Run on the released terminal and waits for Enter so
// the user can read the installer output before the UI returns.
type installExec struct {
	in             deps.Install
	stdin          io.Reader
	stdout, stderr io.Writer
}

func (e *installExec) SetStdin(r io.Reader)  { e.stdin = r }
func (e *installExec) SetStdout(w io.Writer) { e.stdout = w }
func (e *installExec) SetStderr(w io.Writer) { e.stderr = w }

func (e *installExec) Run() error {
	if e.stdin == nil {
		e.stdin = os.Stdin
	}
	if e.stdout == nil {
		e.stdout = os.Stdout
	}
	if e.stderr == nil {
		e.stderr = os.Stderr
	}
	fmt.Fprintf(e.stdout, "\nawsport: %s\n\n", e.in.Description)
	err := deps.Run(context.Background(), e.in, e.stdout, e.stderr)
	if err != nil {
		fmt.Fprintf(e.stderr, "\nawsport: install failed: %v\n", err)
		if e.in.DocsURL != "" {
			fmt.Fprintf(e.stderr, "awsport: manual instructions: %s\n", e.in.DocsURL)
		}
	} else {
		fmt.Fprintln(e.stdout, "\nawsport: install finished.")
	}
	fmt.Fprint(e.stdout, "Press Enter to return to awsport…")
	_, _ = bufio.NewReader(e.stdin).ReadString('\n')
	return err
}

// ---- view ----

func (m *model) onboardingView() string {
	o := &m.onb
	var b strings.Builder
	b.WriteString(m.onbBreadcrumb() + "\n\n")
	switch o.step {
	case onbDeps:
		b.WriteString(m.depsView())
	case onbSSOChoice:
		b.WriteString(boldStyle.Render("How should awsport sign in to AWS?") + "\n\n")
		if o.choices == nil {
			b.WriteString(m.spin.View() + " looking for existing SSO sessions…\n")
		}
		for i, c := range o.choices {
			cur, title := "  ", c.title
			if i == o.cursor {
				cur, title = cursorStyle.Render("› "), cursorStyle.Render(c.title)
			}
			b.WriteString(cur + title + "\n")
			if c.desc != "" {
				b.WriteString("    " + subtleStyle.Render(c.desc) + "\n")
			}
		}
	case onbImport:
		b.WriteString(boldStyle.Render("Import team config") + "\n")
		b.WriteString(subtleStyle.Render("An http(s) URL or a local file path to an awsport team YAML.") + "\n\n")
		b.WriteString(o.importIn.View() + "\n")
	case onbManual:
		b.WriteString(boldStyle.Render("AWS IAM Identity Center (SSO)") + "\n")
		b.WriteString(subtleStyle.Render("The start URL and region are shown in your AWS access portal settings.") + "\n\n")
		for _, f := range o.fields {
			b.WriteString(f.View() + "\n")
		}
	case onbWorking:
		b.WriteString(m.spin.View() + " " + o.working + "…\n")
	case onbFailed:
		b.WriteString(errStyle.Render("✗ "+o.err) + "\n\n")
		b.WriteString(subtleStyle.Render("enter to retry · esc to go back") + "\n")
	}
	if o.err != "" && o.step != onbFailed {
		b.WriteString("\n" + errStyle.Render(o.err) + "\n")
	}
	return b.String()
}

func (m *model) onbBreadcrumb() string {
	steps := []string{"Prerequisites", "SSO", "Login", "Profiles"}
	cur := 0
	switch m.onb.step {
	case onbSSOChoice, onbImport, onbManual:
		cur = 1
	case onbWorking, onbFailed:
		cur = 2
	}
	parts := make([]string, len(steps))
	for i, s := range steps {
		label := fmt.Sprintf("%d %s", i+1, s)
		if i == cur {
			parts[i] = cursorStyle.Render(label)
		} else {
			parts[i] = subtleStyle.Render(label)
		}
	}
	return boldStyle.Render("Welcome to awsport") + "   " + strings.Join(parts, subtleStyle.Render(" › "))
}

func (m *model) depsView() string {
	o := &m.onb
	if o.checks == nil {
		return m.spin.View() + " checking prerequisites…\n"
	}
	var b strings.Builder
	b.WriteString(boldStyle.Render("Prerequisites") + "\n\n")
	for _, c := range o.checks {
		mark := okStyle.Render("✓")
		detail := c.Version
		if !c.OK {
			mark = errStyle.Render("✗")
			detail = c.Problem
			if detail == "" && !c.Found {
				detail = "not found"
			}
		}
		line := fmt.Sprintf("  %s %-24s %s", mark, c.Name, detail)
		if c.Path != "" {
			line += "  " + subtleStyle.Render(c.Path)
		}
		b.WriteString(line + "\n")
	}
	c, ok := firstFailing(o.checks)
	if !ok {
		return b.String()
	}
	b.WriteString("\n" + boldStyle.Render("Install "+c.Name) + "\n")
	if c.Install.Description != "" {
		b.WriteString(c.Install.Description + "\n")
	}
	for _, cmd := range c.Install.Commands {
		b.WriteString("  " + codeStyle.Render("$ "+shellJoin(cmd)) + "\n")
	}
	if c.Install.NeedsSudo {
		b.WriteString(warnStyle.Render("  requires sudo") + "\n")
	}
	if c.Install.DocsURL != "" {
		b.WriteString(subtleStyle.Render("  docs: "+c.Install.DocsURL) + "\n")
	}
	b.WriteString("\n" + subtleStyle.Render("i install · r recheck · s skip (tunnels need every prerequisite)") + "\n")
	return b.String()
}

// shellJoin renders argv for display, quoting arguments that need it.
func shellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\"'$&|;<>()*?`\\") {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

func (m *model) onboardingHelp() helpKeys {
	switch m.onb.step {
	case onbDeps:
		if m.onb.checks == nil {
			return helpKeys{keys.Quit}
		}
		h := helpKeys{keys.Install, keys.Recheck, keys.Skip}
		if m.onb.fromApp {
			h = append(h, keys.Back)
		}
		return append(h, keys.Quit)
	case onbSSOChoice:
		return helpKeys{keys.Up, keys.Down, keys.Enter, keys.Back, keys.Quit}
	case onbImport:
		return helpKeys{bind("enter", "import", "enter"), bind("esc", "back", "esc")}
	case onbManual:
		return helpKeys{keys.NextField, bind("enter", "next/save", "enter"), bind("esc", "back", "esc")}
	case onbFailed:
		return helpKeys{bind("enter", "retry", "enter"), keys.Back, keys.Bug, keys.Quit}
	}
	return helpKeys{keys.ForceQuit}
}
