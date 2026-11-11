package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type modalKind int

const (
	modalConfirm modalKind = iota // generic yes/no question
	modalQuit                     // quit while tunnels are active
)

// modal is a centered dialog that captures every key until dismissed.
type modal struct {
	kind     modalKind
	text     string         // modalConfirm: the question
	yes      func() tea.Cmd // modalConfirm: runs on y
	stopping bool           // modalQuit: StopAll in flight
	err      string         // modalQuit: last StopAll failure
}

type modalOutcome int

const (
	outNone     modalOutcome = iota
	outCancel                // close the modal
	outYes                   // confirm: run yes
	outKeep                  // quit, leaving tunnels running in the agent
	outStop                  // stop all tunnels, then quit
	outQuitFlow              // close the confirm and start the quit flow
)

// modalKey maps a key (tea.KeyMsg.String()) to what the modal should do.
// In the quit dialog q and ctrl+c mean the non-destructive keep & quit, so
// quitting never blocks; while stopping only those remain available.
func modalKey(kind modalKind, stopping bool, k string) modalOutcome {
	if kind == modalQuit {
		switch k {
		case "k", "K", "q", "ctrl+c":
			return outKeep
		case "s", "S":
			if !stopping {
				return outStop
			}
		case "esc":
			if !stopping {
				return outCancel
			}
		}
		return outNone
	}
	switch k {
	case "y", "Y":
		return outYes
	case "n", "N", "esc", "q":
		return outCancel
	case "ctrl+c":
		return outQuitFlow
	}
	return outNone
}

type stoppedAllMsg struct{ err error }

func (m *model) confirm(text string, yes func() tea.Cmd) {
	m.modal = &modal{kind: modalConfirm, text: text, yes: yes}
}

// quit exits at once when no tunnel is active, otherwise asks whether to keep
// the tunnels running in the background.
func (m *model) quit() tea.Cmd {
	if m.a.Tunnels.ActiveCount() == 0 {
		return tea.Quit
	}
	m.modal = &modal{kind: modalQuit}
	return nil
}

func (m *model) onModalKey(msg tea.KeyMsg) tea.Cmd {
	md := m.modal
	switch modalKey(md.kind, md.stopping, msg.String()) {
	case outCancel:
		m.modal = nil
	case outYes:
		m.modal = nil
		return md.yes()
	case outKeep:
		return tea.Quit
	case outQuitFlow:
		m.modal = nil
		return m.quit()
	case outStop:
		md.stopping, md.err = true, ""
		a := m.a
		return m.run("stopping tunnels", time.Minute, func(context.Context) tea.Msg {
			return stoppedAllMsg{err: a.StopAll()}
		})
	}
	return nil
}

func (m *model) onStoppedAll(msg stoppedAllMsg) tea.Cmd {
	if msg.err == nil {
		return tea.Quit
	}
	m.setErr(fmt.Errorf("stop tunnels: %w", msg.err))
	if m.modal == nil || m.modal.kind != modalQuit {
		m.modal = &modal{kind: modalQuit}
	}
	m.modal.stopping = false
	m.modal.err = m.flash
	return tea.Batch(m.rebuildDashboard(), m.rebuildPorts())
}

var modalStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorWarn).Padding(1, 3)

func keyHint(k, desc string) string {
	return cursorStyle.Render("["+k+"]") + " " + desc
}

// modalView renders the dialog box, at most width cells wide.
func (m *model) modalView(width int) string {
	md := m.modal
	var lines []string
	switch md.kind {
	case modalQuit:
		lines = append(lines, warnStyle.Bold(true).Render(fmt.Sprintf("%d tunnel(s) running.", m.a.Tunnels.ActiveCount())), "")
		if md.stopping {
			lines = append(lines, m.spin.View()+" stopping tunnels…", "", keyHint("ctrl+c", "quit now, keep the rest running"))
			break
		}
		if md.err != "" {
			lines = append(lines, errStyle.Render(md.err), "")
		}
		lines = append(lines,
			keyHint("k", "keep running in background & quit"),
			keyHint("s", "stop all & quit"),
			keyHint("esc", "cancel"))
	default:
		lines = append(lines, boldStyle.Render(md.text), "", keyHint("y", "yes")+"   "+keyHint("n/esc", "cancel"))
	}
	w := min(64, width) - modalStyle.GetHorizontalBorderSize()
	return modalStyle.Width(max(w, 20)).Render(strings.Join(lines, "\n"))
}
