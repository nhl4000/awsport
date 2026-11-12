package tui

import (
	"errors"
	"runtime/debug"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nhl4000/awsport/internal/bugreport"
)

// crashGuard wraps the root model so a panic in Init, Update, View or a
// command is recorded with its stack. The panic then continues into Bubble
// Tea, which restores the terminal; repanic re-raises it afterwards for the
// CLI's crash handler.
type crashGuard struct {
	m    tea.Model
	slot *panicSlot
}

type panicSlot struct {
	mu sync.Mutex
	p  *bugreport.Panic
}

func guard(m tea.Model) crashGuard { return crashGuard{m: m, slot: &panicSlot{}} }

func (g crashGuard) Init() tea.Cmd {
	defer g.slot.catch()
	return g.slot.wrap(g.m.Init())
}

func (g crashGuard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	defer g.slot.catch()
	m, cmd := g.m.Update(msg)
	g.m = m
	return g, g.slot.wrap(cmd)
}

func (g crashGuard) View() string {
	defer g.slot.catch()
	return g.m.View()
}

// repanic re-raises a recorded panic as a *bugreport.Panic. A panic Bubble
// Tea caught outside the guard (in a tea.Sequence step) is re-raised
// without its stack, which Bubble Tea does not expose.
func (g crashGuard) repanic(runErr error) {
	g.slot.mu.Lock()
	p := g.slot.p
	g.slot.mu.Unlock()
	if p == nil && errors.Is(runErr, tea.ErrProgramPanic) {
		p = &bugreport.Panic{Value: runErr, Stack: []byte("(stack unavailable: the panic was recovered inside Bubble Tea)\n")}
	}
	if p != nil {
		panic(p)
	}
}

// catch records the first panic and re-panics; it must be deferred
// directly so recover sees the panic.
func (s *panicSlot) catch() {
	r := recover()
	if r == nil {
		return
	}
	s.mu.Lock()
	if s.p == nil {
		s.p = &bugreport.Panic{Value: r, Stack: debug.Stack()}
	}
	s.mu.Unlock()
	panic(r)
}

// wrap guards cmd and, when it returns a tea.BatchMsg, the batched commands.
func (s *panicSlot) wrap(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		defer s.catch()
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			out := make(tea.BatchMsg, len(batch))
			for i, c := range batch {
				out[i] = s.wrap(c)
			}
			return out
		}
		return msg
	}
}
