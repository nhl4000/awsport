package open

import (
	"errors"
	"testing"

	"github.com/nhl4000/awsport/internal/core"
)

func TestAutoOpener(t *testing.T) {
	var opened []string
	fail := false
	a := NewAutoOpener(func(u string) error {
		opened = append(opened, u)
		if fail {
			return errors.New("no display")
		}
		return nil
	})
	web := core.TunnelSpec{Key: "web", RemotePort: 8025, LocalPort: 18025}
	db := core.TunnelSpec{Key: "db", RemoteHost: "db.example.internal", RemotePort: 5432, LocalPort: 15432}
	ec2DB := core.TunnelSpec{Key: "ec2db", RemotePort: 3306, LocalPort: 13306}

	if a.Expect(db) || a.Expect(ec2DB) {
		t.Fatal("RDS and database ports must not be expected")
	}
	if !a.Expect(web) || a.Pending() != 1 {
		t.Fatal("web tunnel not expected")
	}
	if _, did, _ := a.Update("web", false, true); did {
		t.Fatal("opened while still starting")
	}
	u, did, err := a.Update("web", true, true)
	if !did || err != nil || u != "http://127.0.0.1:18025" {
		t.Fatalf("Update up = %q, %v, %v", u, did, err)
	}
	if _, did, _ := a.Update("web", true, true); did {
		t.Fatal("opened twice (reconnect must not open again)")
	}
	if _, did, _ := a.Update("db", true, true); did {
		t.Fatal("opened a tunnel that was never expected")
	}

	// A tunnel that fails before coming up is forgotten, so a later Up
	// (e.g. a manual restart) doesn't open a surprise tab.
	a.Expect(web)
	a.Update("web", false, false)
	if a.Pending() != 0 {
		t.Fatal("failed tunnel still pending")
	}

	fail = true
	a.Expect(web)
	if _, did, err := a.Update("web", true, true); !did || err == nil {
		t.Fatalf("browser error not reported: did=%v err=%v", did, err)
	}
	if len(opened) != 2 {
		t.Errorf("opened %v, want 2 launches", opened)
	}
}
