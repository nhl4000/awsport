package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"

	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

func helpNames(bs []key.Binding) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Help().Key)
	}
	return out
}

func TestScreenHelpFit(t *testing.T) {
	h := screenHelp{keys: []key.Binding{keys.Enter, keys.Back, keys.Refresh, keys.Filter}}
	tail := helpWidth([]key.Binding{keys.Help, keys.Quit})
	tests := []struct {
		width int
		want  []string
	}{
		{0, []string{"?", "q"}},
		{tail + helpSepWidth + helpWidth(h.keys[:1]), []string{"enter", "?", "q"}},
		{tail + helpSepWidth + helpWidth(h.keys[:1]) - 1, []string{"?", "q"}},
		{200, []string{"enter", "esc", "r", "/", "?", "q"}},
	}
	for _, tt := range tests {
		got := helpNames(h.fit(tt.width).ShortHelp())
		if len(got) != len(tt.want) {
			t.Fatalf("fit(%d) = %v, want %v", tt.width, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("fit(%d) = %v, want %v", tt.width, got, tt.want)
			}
		}
		if w := helpWidth(h.fit(tt.width).ShortHelp()); tt.width >= tail && w > tt.width {
			t.Fatalf("fit(%d) renders %d cells", tt.width, w)
		}
	}
}

func TestScreenHelpFull(t *testing.T) {
	h := screenHelp{
		keys: []key.Binding{keys.Enter, keys.Select, keys.SaveGroup, keys.Back, keys.Copy, keys.Groups},
		omit: []key.Binding{keys.Dashboard},
	}
	cols := h.FullHelp()
	if len(cols) != 3 || len(cols[0]) != helpColumnRows || len(cols[1]) != 2 {
		t.Fatalf("columns = %v", cols)
	}
	global := helpNames(cols[2])
	want := []string{"L", "!", "q", "?"} // t omitted, g listed with the screen keys
	if len(global) != len(want) {
		t.Fatalf("global column = %v, want %v", global, want)
	}
	for i := range want {
		if global[i] != want[i] {
			t.Fatalf("global column = %v, want %v", global, want)
		}
	}
}

func TestFlashExpired(t *testing.T) {
	at := time.Now()
	if flashExpired(flashOK, at, at.Add(flashTTL-time.Millisecond)) || !flashExpired(flashOK, at, at.Add(flashTTL)) {
		t.Error("info lifetime")
	}
	if flashExpired(flashError, at, at.Add(flashTTL)) || !flashExpired(flashWarn, at, at.Add(flashErrTTL)) {
		t.Error("problems should outlive info messages")
	}
}

func TestTunnelSummaries(t *testing.T) {
	web := core.Resource{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0a", Name: "web",
		Ports: []core.PortSpec{{Name: "app", Remote: 8080}}}
	db := core.Resource{Kind: core.KindRDS, Profile: "example-dev", Region: "us-east-1", ID: "db", Name: "db", Port: 5432}
	session := func(r core.Resource, remote, local int, st tunnel.Status) tunnel.Session {
		return tunnel.Session{Status: st, Spec: core.TunnelSpec{
			Key: r.TunnelKey(remote), Profile: r.Profile, Region: r.Region, RemotePort: remote, LocalPort: local,
			Label: r.Name + " " + portLabel(r, remote),
		}}
	}
	sessions := []tunnel.Session{
		session(web, 8080, 18080, tunnel.Up),
		session(web, 9000, 19000, tunnel.Starting),
		session(db, 5432, 15432, tunnel.Up),
		session(db, 3306, 13306, tunnel.Stopped),
		{Status: tunnel.Up, Spec: core.TunnelSpec{Key: "malformed", Profile: "example-prod", RemotePort: 1}},
	}
	byRef := activeByRef(sessions)
	if len(byRef[web.Ref()]) != 2 || len(byRef[db.Ref()]) != 1 {
		t.Fatalf("activeByRef = %v", byRef)
	}
	if got := tunnelsNote(web, byRef[web.Ref()][:1]); got != "● app up → 127.0.0.1:18080" {
		t.Errorf("one EC2 tunnel: %q", got)
	}
	if got := tunnelsNote(web, byRef[web.Ref()]); got != "◌ 2 tunnels" {
		t.Errorf("two EC2 tunnels, one starting: %q", got)
	}
	if got := tunnelsNote(db, byRef[db.Ref()]); got != "● up → 127.0.0.1:15432" {
		t.Errorf("RDS tunnel: %q", got)
	}
	if got := tunnelsNote(db, nil); got != "" {
		t.Errorf("no tunnel: %q", got)
	}
	if !rdsActive(byRef[db.Ref()]) || rdsActive(nil) {
		t.Error("rdsActive")
	}

	counts := profileTunnels(sessions)
	if counts["example-dev"] != 3 || counts["example-prod"] != 1 {
		t.Errorf("profileTunnels = %v", counts)
	}

	g := core.Group{Name: "dev", Members: []core.GroupMember{
		{Profile: "example-dev", Name: "web", Remote: 8080},
		{Profile: "example-dev", Region: "us-east-1", Name: "db", Remote: 5432},
		{Profile: "example-dev", Region: "us-west-2", Name: "db", Remote: 5432},
		{Profile: "example-dev", Name: "we", Remote: 8080},
		{Profile: "example-dev", Name: "db", Remote: 3306},
	}}
	if n := groupUp(g, sessions); n != 2 {
		t.Errorf("groupUp = %d, want 2", n)
	}
}

func TestPlural(t *testing.T) {
	if plural(1, "tunnel") != "1 tunnel" || plural(0, "tunnel") != "0 tunnels" || plural(3, "tunnel") != "3 tunnels" {
		t.Error("plural")
	}
}

func TestSetItemsKeepsCursor(t *testing.T) {
	l := newList(scrResources)
	l.SetSize(80, 40)
	m := &model{lists: map[screen]*list.Model{scrResources: &l}}
	items := func(keys ...string) []list.Item {
		out := make([]list.Item, 0, len(keys))
		for _, k := range keys {
			out = append(out, item{title: k, key: k})
		}
		return out
	}
	m.setItems(scrResources, items("a", "c", "d"))
	l.Select(1)
	m.setItems(scrResources, items("a", "b", "c", "d"))
	if k, _ := selectedKey(&l); k != "c" {
		t.Fatalf("cursor on %q after insert, want c", k)
	}
	m.setItems(scrResources, items("a", "b", "d"))
	if k, _ := selectedKey(&l); k != "d" {
		t.Fatalf("cursor on %q after removing the selected item, want d (the item now at its index)", k)
	}
}
