package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/x/ansi"

	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

func TestModalKey(t *testing.T) {
	tests := []struct {
		name     string
		kind     modalKind
		stopping bool
		key      string
		want     modalOutcome
	}{
		{"quit k keeps", modalQuit, false, "k", outKeep},
		{"quit q again keeps", modalQuit, false, "q", outKeep},
		{"quit ctrl+c keeps", modalQuit, false, "ctrl+c", outKeep},
		{"quit s stops", modalQuit, false, "s", outStop},
		{"quit esc cancels", modalQuit, false, "esc", outCancel},
		{"quit other ignored", modalQuit, false, "x", outNone},
		{"stopping ctrl+c still quits", modalQuit, true, "ctrl+c", outKeep},
		{"stopping q still quits", modalQuit, true, "q", outKeep},
		{"stopping s ignored", modalQuit, true, "s", outNone},
		{"stopping esc ignored", modalQuit, true, "esc", outNone},
		{"confirm y", modalConfirm, false, "y", outYes},
		{"confirm Y", modalConfirm, false, "Y", outYes},
		{"confirm n", modalConfirm, false, "n", outCancel},
		{"confirm esc", modalConfirm, false, "esc", outCancel},
		{"confirm q cancels", modalConfirm, false, "q", outCancel},
		{"confirm ctrl+c quits", modalConfirm, false, "ctrl+c", outQuitFlow},
		{"confirm other ignored", modalConfirm, false, "k", outNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modalKey(tt.kind, tt.stopping, tt.key); got != tt.want {
				t.Fatalf("modalKey(%v, %v, %q) = %v, want %v", tt.kind, tt.stopping, tt.key, got, tt.want)
			}
		})
	}
}

func TestToggleActions(t *testing.T) {
	for _, st := range []tunnel.Status{tunnel.Starting, tunnel.Up, tunnel.Reconnecting} {
		s := tunnel.Session{Status: st}
		if got := portAction(s, true); got != actStop {
			t.Errorf("portAction(%s) = %v, want stop", st, got)
		}
		if got := dashboardAction(s); got != actStop {
			t.Errorf("dashboardAction(%s) = %v, want stop", st, got)
		}
	}
	for _, st := range []tunnel.Status{tunnel.Failed, tunnel.AuthExpired, tunnel.Stopped} {
		s := tunnel.Session{Status: st}
		if got := portAction(s, true); got != actStart {
			t.Errorf("portAction(%s) = %v, want start", st, got)
		}
		if got := dashboardAction(s); got != actRestart {
			t.Errorf("dashboardAction(%s) = %v, want restart", st, got)
		}
	}
	if got := portAction(tunnel.Session{Status: tunnel.Up}, false); got != actStart {
		t.Errorf("portAction(missing) = %v, want start", got)
	}
}

func TestLinkAddr(t *testing.T) {
	web := core.TunnelSpec{RemotePort: 8080, LocalPort: 18080}
	if got, want := linkAddr(web), "\x1b]8;;http://127.0.0.1:18080\x1b\\127.0.0.1:18080\x1b]8;;\x1b\\"; got != want {
		t.Fatalf("linkAddr(web) = %q, want %q", got, want)
	}
	db := core.TunnelSpec{RemoteHost: "db.example.com", RemotePort: 5432, LocalPort: 15432}
	if got := linkAddr(db); got != "127.0.0.1:15432" {
		t.Fatalf("linkAddr(rds) = %q", got)
	}
	if w := ansi.StringWidth(linkAddr(web)); w != len("127.0.0.1:18080") {
		t.Fatalf("link width = %d", w)
	}
}

// TestLinkedDescWidth renders a dashboard row holding a hyperlink through
// the list delegate at several widths: truncation must count only visible
// cells and keep the closing OSC 8 so the link never leaks.
func TestLinkedDescWidth(t *testing.T) {
	s := tunnel.Session{
		Spec:    core.TunnelSpec{Label: "web-app app", RemotePort: 8080, LocalPort: 18080},
		Status:  tunnel.Up,
		LastLog: []string{"Waiting for connections..."},
	}
	desc := sessionDetail(s, s.Since)
	if !strings.Contains(desc, "\x1b]8;;http://127.0.0.1:18080\x1b\\") {
		t.Fatalf("desc has no hyperlink: %q", desc)
	}
	for _, width := range []int{12, 20, 30, 80} {
		l := newList(scrDashboard)
		l.SetItems([]list.Item{item{title: "web-app app", desc: desc, key: "k"}})
		l.SetSize(width, 10)
		out := l.View()
		for _, line := range strings.Split(out, "\n") {
			if w := ansi.StringWidth(line); w > width {
				t.Fatalf("width %d: line %q is %d cells", width, line, w)
			}
		}
		opens := strings.Count(out, "\x1b]8;;http")
		closes := strings.Count(out, "\x1b]8;;\x1b\\")
		if opens != closes || (width == 80 && opens == 0) {
			t.Fatalf("width %d: %d link opens, %d closes in %q", width, opens, closes, out)
		}
	}
}
