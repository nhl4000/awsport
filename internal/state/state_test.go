package state

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
)

func setStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv(config.EnvStateDir, dir)
	return dir
}

func TestPortKey(t *testing.T) {
	if got := PortKey("example", "us-east-1", "app", 8080); got != "example/us-east-1/app/8080" {
		t.Fatalf("PortKey = %q", got)
	}
}

func TestOpenMissingFile(t *testing.T) {
	dir := setStateDir(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if s.Path() != filepath.Join(dir, "state.json") {
		t.Fatalf("Path = %q", s.Path())
	}
	d := s.Get()
	if d.Ports == nil || d.Bastions == nil || d.ManualPorts == nil {
		t.Fatalf("maps should be initialized: %+v", d)
	}
	if len(d.Ports)+len(d.Bastions)+len(d.ManualPorts)+len(d.Groups) != 0 {
		t.Fatalf("expected empty state: %+v", d)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatalf("Open must not create the file, stat err = %v", err)
	}
}

func TestOpenCorruptFile(t *testing.T) {
	dir := setStateDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestUpdatePersists(t *testing.T) {
	setStateDir(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	key := PortKey("example", "us-east-1", "app", 8080)
	err = s.Update(func(d *Data) {
		d.Ports[key] = 18080
		d.Bastions["example/us-east-1"] = "i-0123456789abcdef0"
		d.ManualPorts["example/us-east-1/app"] = []core.PortSpec{{Name: "redis", Remote: 6379}}
		d.Groups = append(d.Groups, core.Group{Name: "g", Members: []core.GroupMember{{Profile: "example", Name: "app", Remote: 8080}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(s.Path())
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", fi.Mode().Perm())
		}
	}

	re, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	d := re.Get()
	if d.Ports[key] != 18080 || d.Bastions["example/us-east-1"] != "i-0123456789abcdef0" {
		t.Fatalf("reloaded = %+v", d)
	}
	if mp := d.ManualPorts["example/us-east-1/app"]; len(mp) != 1 || mp[0].Remote != 6379 {
		t.Fatalf("manual ports = %v", mp)
	}
	if len(d.Groups) != 1 || d.Groups[0].Members[0].Name != "app" {
		t.Fatalf("groups = %v", d.Groups)
	}
}

func TestGetIsDeepCopy(t *testing.T) {
	setStateDir(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *Data) {
		d.ManualPorts["k"] = []core.PortSpec{{Name: "a", Remote: 1}}
		d.Groups = []core.Group{{Name: "g", Members: []core.GroupMember{{Name: "m"}}}}
	}); err != nil {
		t.Fatal(err)
	}
	d := s.Get()
	d.Ports["x"] = 1
	d.ManualPorts["k"][0].Remote = 99
	d.Groups[0].Members[0].Name = "changed"
	again := s.Get()
	if _, ok := again.Ports["x"]; ok || again.ManualPorts["k"][0].Remote != 1 || again.Groups[0].Members[0].Name != "m" {
		t.Fatalf("Get leaked internal state: %+v", again)
	}
}

func TestConcurrentUpdate(t *testing.T) {
	setStateDir(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Update(func(d *Data) { d.Ports[fmt.Sprintf("k%d", i)] = 20000 + i })
			_ = s.Get()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	re, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(re.Get().Ports); got != n {
		t.Fatalf("persisted %d ports, want %d", got, n)
	}
}
