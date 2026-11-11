package tui

import "github.com/charmbracelet/bubbles/key"

func bind(help, desc string, keys ...string) key.Binding {
	return key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))
}

var keys = struct {
	Up, Down, Enter, Back, Filter                      key.Binding
	ShowAll, AddPort, Refresh, Shell                   key.Binding
	Dashboard, Groups, Login, Quit, ForceQuit, Onboard key.Binding
	Select, SaveGroup, Copy, Stop, Restart, Delete     key.Binding
	Install, Recheck, Skip, Yes, No, NextField         key.Binding
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
	Install:   bind("i", "install", "i"),
	Recheck:   bind("r", "recheck", "r"),
	Skip:      bind("s", "skip", "s"),
	Yes:       bind("y", "yes", "y", "Y"),
	No:        bind("n", "no", "n", "N", "esc"),
	NextField: bind("tab", "next field", "tab", "shift+tab", "down", "up"),
}

// helpKeys adapts a flat binding slice to help.KeyMap.
type helpKeys []key.Binding

func (h helpKeys) ShortHelp() []key.Binding  { return h }
func (h helpKeys) FullHelp() [][]key.Binding { return [][]key.Binding{h} }
