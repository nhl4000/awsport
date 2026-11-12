package tui

import (
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"
)

func bind(help, desc string, keys ...string) key.Binding {
	return key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))
}

var keys = struct {
	Up, Down, Enter, Back, Filter                        key.Binding
	ShowAll, AddPort, Refresh, Shell                     key.Binding
	Dashboard, Groups, Login, Quit, ForceQuit, Onboard   key.Binding
	Select, SaveGroup, Copy, Stop, Restart, Delete, Open key.Binding
	Install, Recheck, Skip, Yes, No, NextField           key.Binding
	Help, HelpLess, Bug                                  key.Binding
}{
	Up:        bind("↑/k", "up", "up", "k"),
	Down:      bind("↓/j", "down", "down", "j"),
	Enter:     bind("enter", "select", "enter"),
	Back:      bind("esc", "back", "esc"),
	Filter:    bind("/", "filter", "/"),
	ShowAll:   bind("a", "show all", "a"),
	AddPort:   bind("p", "add port", "p"),
	Refresh:   bind("r", "refresh", "r"),
	Shell:     bind("e", "shell", "e"),
	Dashboard: bind("t", "tunnels", "t"),
	Groups:    bind("g", "groups", "g"),
	Login:     bind("L", "re-login", "L"),
	Quit:      bind("q", "quit", "q"),
	ForceQuit: bind("ctrl+c", "quit", "ctrl+c"),
	Onboard:   bind("o", "setup", "o"),
	Select:    bind("space", "select", " ", "space"),
	SaveGroup: bind("s", "save group", "s"),
	Copy:      bind("c", "copy", "c"),
	Stop:      bind("x", "stop", "x"),
	Restart:   bind("r", "restart", "r"),
	Delete:    bind("d", "delete", "d"),
	Open:      bind("o", "open in browser", "o"),
	Install:   bind("i", "install", "i"),
	Recheck:   bind("r", "recheck", "r"),
	Skip:      bind("s", "skip", "s"),
	Yes:       bind("y", "yes", "y", "Y"),
	No:        bind("n/esc", "cancel", "n", "N", "esc", "q"),
	NextField: bind("tab", "next field", "tab", "shift+tab", "down", "up"),
	Help:      bind("?", "more", "?"),
	HelpLess:  bind("?", "less", "?"),
	Bug:       bind("!", "report bug", "!"),
}

// globalKeys work on every list screen.
var globalKeys = []key.Binding{keys.Dashboard, keys.Groups, keys.Login, keys.Bug, keys.Quit}

// helpKeys adapts a flat binding slice to help.KeyMap.
type helpKeys []key.Binding

func (h helpKeys) ShortHelp() []key.Binding  { return h }
func (h helpKeys) FullHelp() [][]key.Binding { return [][]key.Binding{h} }

// screenHelp is a list screen's help: its own keys, most important first,
// and the global keys. The short form shows as many leading screen keys as
// fit and always ends with "? more" and "q quit"; the full form lists
// everything in columns.
type screenHelp struct {
	keys  []key.Binding
	omit  []key.Binding // global keys pointless here, e.g. t on the tunnels screen
	short []key.Binding // set by fit
}

const helpColumnRows = 4

func (h screenHelp) ShortHelp() []key.Binding { return h.short }

func (h screenHelp) FullHelp() [][]key.Binding {
	var cols [][]key.Binding
	for i := 0; i < len(h.keys); i += helpColumnRows {
		cols = append(cols, h.keys[i:min(i+helpColumnRows, len(h.keys))])
	}
	var global []key.Binding
	for _, g := range globalKeys {
		if !hasHelpKey(h.keys, g) && !hasHelpKey(h.omit, g) {
			global = append(global, g)
		}
	}
	return append(cols, append(global, keys.HelpLess))
}

// fit picks the short help for width cells: the leading screen keys that fit
// before the always-shown "? more" and "q quit".
func (h screenHelp) fit(width int) screenHelp {
	tail := []key.Binding{keys.Help, keys.Quit}
	if hasHelpKey(h.keys, keys.Quit) {
		tail = tail[:1]
	}
	used := helpWidth(tail)
	h.short = nil
	for _, k := range h.keys {
		w := helpWidth([]key.Binding{k}) + helpSepWidth
		if used+w > width {
			break
		}
		used += w
		h.short = append(h.short, k)
	}
	h.short = append(h.short, tail...)
	return h
}

// helpSepWidth is the width of help.Model's short separator " • ".
const helpSepWidth = 3

// helpWidth is the rendered width of bindings in the short help.
func helpWidth(bs []key.Binding) int {
	w := 0
	for i, b := range bs {
		if i > 0 {
			w += helpSepWidth
		}
		w += lipgloss.Width(b.Help().Key) + 1 + lipgloss.Width(b.Help().Desc)
	}
	return w
}

func hasHelpKey(bs []key.Binding, k key.Binding) bool {
	for _, b := range bs {
		if b.Help().Key == k.Help().Key {
			return true
		}
	}
	return false
}
