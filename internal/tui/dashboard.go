package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

// ---- dashboard ----

func (m *model) rebuildDashboard() tea.Cmd {
	sessions := m.a.Tunnels.Sessions()
	now := time.Now()
	present := make(map[string]bool, len(sessions))
	items := make([]list.Item, 0, len(sessions))
	for _, s := range sessions {
		present[s.Spec.Key] = true
		box := "[ ]"
		if m.selected[s.Spec.Key] {
			box = "[x]"
		}
		items = append(items, item{
			title: fmt.Sprintf("%s %s %s", box, statusGlyph(s.Status), s.Spec.Label),
			desc:  sessionDetail(s, now),
			key:   s.Spec.Key,
		})
	}
	for k := range m.selected {
		if !present[k] {
			delete(m.selected, k)
		}
	}
	l := m.lists[scrDashboard]
	l.Title = "Tunnels"
	if n := len(m.selected); n > 0 {
		l.Title = fmt.Sprintf("Tunnels · %d selected", n)
	}
	return m.setItems(scrDashboard, items)
}

func (m *model) selectedSession() (tunnel.Session, bool) {
	k, ok := selectedKey(m.lists[scrDashboard])
	if !ok {
		return tunnel.Session{}, false
	}
	return m.a.Tunnels.Get(k)
}

func (m *model) dashboardKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	if !key.Matches(msg, keys.Select, keys.SaveGroup, keys.Copy, keys.Stop, keys.Restart) {
		return false, nil
	}
	if key.Matches(msg, keys.SaveGroup) {
		return true, m.promptSaveGroup()
	}
	s, ok := m.selectedSession()
	if !ok {
		m.setErr(fmt.Errorf("%w: no tunnel under the cursor", errNoSelection))
		return true, nil
	}
	mgr, spec := m.a.Tunnels, s.Spec
	switch {
	case key.Matches(msg, keys.Select):
		m.selected[spec.Key] = !m.selected[spec.Key]
		if !m.selected[spec.Key] {
			delete(m.selected, spec.Key)
		}
		return true, m.rebuildDashboard()
	case key.Matches(msg, keys.Copy):
		m.copyConn(spec)
		return true, nil
	case key.Matches(msg, keys.Stop):
		return true, m.run("stopping "+spec.Label, tunnelOpTimeout, func(context.Context) tea.Msg {
			mgr.Stop(spec.Key)
			return tunnelOpMsg{verb: "stopped", label: spec.Label}
		})
	default:
		return true, m.run("restarting "+spec.Label, tunnelOpTimeout, func(context.Context) tea.Msg {
			return tunnelOpMsg{verb: "restarted", label: spec.Label, err: mgr.Restart(spec.Key)}
		})
	}
}

func (m *model) copyConn(spec core.TunnelSpec) {
	conn := app.ConnString(spec)
	if clipboard.Unsupported {
		m.setFlash("clipboard unavailable; connection string: " + conn)
		return
	}
	if err := clipboard.WriteAll(conn); err != nil {
		m.setFlash(fmt.Sprintf("clipboard unavailable (%s); connection string: %s", oneLine(err.Error()), conn))
		return
	}
	m.setFlash("copied " + conn)
}

// groupSpecs returns the selected tunnels' specs, or the one under the cursor.
func (m *model) groupSpecs() []core.TunnelSpec {
	var specs []core.TunnelSpec
	for _, s := range m.a.Tunnels.Sessions() {
		if m.selected[s.Spec.Key] {
			specs = append(specs, s.Spec)
		}
	}
	if len(specs) == 0 {
		if s, ok := m.selectedSession(); ok {
			specs = append(specs, s.Spec)
		}
	}
	return specs
}

func (m *model) promptSaveGroup() tea.Cmd {
	specs := m.groupSpecs()
	if len(specs) == 0 {
		m.setErr(fmt.Errorf("%w: select tunnels with space first", errNoSelection))
		return nil
	}
	label := fmt.Sprintf("Save %d tunnel(s) as group:", len(specs))
	return m.prompt(label, "group name", func(name string) (tea.Cmd, error) {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, errors.New("group name is empty")
		}
		return m.saveGroup(name, specs), nil
	})
}

// saveGroup resolves each spec to its resource (discovering profiles whose
// resources aren't cached yet), builds the group and stores it.
func (m *model) saveGroup(name string, specs []core.TunnelSpec) tea.Cmd {
	a := m.a
	known := make(map[string]core.Resource, len(m.known))
	for k, v := range m.known {
		known[k] = v
	}
	profiles := append([]core.Profile(nil), m.profiles...)
	return m.run("saving group "+name, groupTimeout, func(ctx context.Context) tea.Msg {
		lookup := func(ref string) (core.Resource, bool) { r, ok := known[ref]; return r, ok }
		g, unresolved, err := buildGroup(name, specs, lookup)
		discovered := map[string][]core.Resource{}
		if len(unresolved) > 0 {
			want := map[string]bool{}
			for _, s := range specs {
				for _, k := range unresolved {
					if s.Key == k {
						want[s.Profile] = true
					}
				}
			}
			for pname := range want {
				p := core.Profile{Name: pname}
				for _, x := range profiles {
					if x.Name == pname {
						p = x
					}
				}
				rs, _ := a.Discover(ctx, p)
				discovered[pname] = rs
				for _, r := range rs {
					known[r.Ref()] = r
				}
			}
			g, unresolved, err = buildGroup(name, specs, lookup)
		}
		if err != nil {
			return groupSavedMsg{name: name, unresolved: unresolved, discovered: discovered, err: err}
		}
		if err := a.SaveGroup(g); err != nil {
			return groupSavedMsg{name: name, discovered: discovered, err: err}
		}
		return groupSavedMsg{name: name, members: len(g.Members), unresolved: unresolved, discovered: discovered}
	})
}

func (m *model) onGroupSaved(msg groupSavedMsg) tea.Cmd {
	for pname, rs := range msg.discovered {
		if len(rs) == 0 {
			continue
		}
		m.resources[pname] = rs
		for _, r := range rs {
			m.known[r.Ref()] = r
		}
	}
	if msg.err != nil {
		m.setErr(fmt.Errorf("save group %s: %w", msg.name, msg.err))
		return nil
	}
	text := fmt.Sprintf("saved group %s with %d tunnel(s)", msg.name, msg.members)
	if len(msg.unresolved) > 0 {
		text += fmt.Sprintf("; skipped %d tunnel(s) whose resource could not be found", len(msg.unresolved))
	}
	m.setFlash(text)
	m.selected = map[string]bool{}
	return tea.Batch(m.rebuildDashboard(), m.rebuildGroups())
}

// ---- groups ----

func (m *model) rebuildGroups() tea.Cmd {
	gs := m.a.Groups()
	items := make([]list.Item, 0, len(gs))
	for _, g := range gs {
		items = append(items, item{title: g.Name, desc: groupSummary(g), key: g.Name})
	}
	return m.setItems(scrGroups, items)
}

func (m *model) groupsKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	if !key.Matches(msg, keys.Enter, keys.Delete) {
		return false, nil
	}
	name, ok := selectedKey(m.lists[scrGroups])
	if !ok {
		return true, nil
	}
	a := m.a
	if key.Matches(msg, keys.Delete) {
		m.confirm = &confirmPrompt{
			text: fmt.Sprintf("Delete group %s? (groups from config files are kept)", name),
			yes: func() tea.Cmd {
				return m.run("deleting group "+name, configTimeout, func(context.Context) tea.Msg {
					return groupDeletedMsg{name: name, err: a.DeleteGroup(name)}
				})
			},
		}
		return true, nil
	}
	return true, m.run("starting group "+name, groupTimeout, func(ctx context.Context) tea.Msg {
		specs, errs := a.StartGroup(ctx, name)
		return groupStartedMsg{name: name, specs: specs, errs: errs}
	})
}

func (m *model) onGroupStarted(msg groupStartedMsg) tea.Cmd {
	text := fmt.Sprintf("group %s: started %d tunnel(s)", msg.name, len(msg.specs))
	if len(msg.errs) > 0 {
		m.flash, m.flashErr = fmt.Sprintf("%s, %d failed: %s", text, len(msg.errs), errorsSummary(msg.errs)), true
	} else {
		m.setFlash(text)
	}
	m.goTo(scrDashboard)
	return m.rebuildDashboard()
}
