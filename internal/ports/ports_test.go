package ports

import (
	"errors"
	"net"
	"strconv"
	"testing"
)

// occupy binds 127.0.0.1:port (0 = any) and returns the bound port.
func occupy(t *testing.T, port int) int {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// freePort returns a port that is free right now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

type recorder map[string]int

func (r recorder) save(key string, port int) error {
	r[key] = port
	return nil
}

func TestDefault(t *testing.T) {
	tests := []struct{ remote, want int }{
		{22, 10022},
		{5432, 15432},
		{55535, 65535},
		{55536, 55536},
		{60000, 60000},
	}
	for _, tt := range tests {
		if got := Default(tt.remote); got != tt.want {
			t.Errorf("Default(%d) = %d, want %d", tt.remote, got, tt.want)
		}
	}
}

func TestFree(t *testing.T) {
	p := occupy(t, 0)
	if Free(p) {
		t.Fatalf("port %d is bound but reported free", p)
	}
	if !Free(freePort(t)) {
		t.Fatal("unbound port reported busy")
	}
	if Free(0) || Free(70000) {
		t.Fatal("out-of-range ports must not be free")
	}
}

func TestAssignSavedReuse(t *testing.T) {
	p := freePort(t)
	rec := recorder{}
	got, temp, err := Assign("k", 80, map[string]int{"k": p}, nil, rec.save)
	if err != nil {
		t.Fatal(err)
	}
	if got != p || temp {
		t.Fatalf("got %d temp=%v, want %d false", got, temp, p)
	}
	if len(rec) != 0 {
		t.Fatalf("reuse must not re-save: %v", rec)
	}
}

func TestAssignBusySavedIsTemporary(t *testing.T) {
	busy := occupy(t, 0)
	rec := recorder{}
	got, temp, err := Assign("k", 80, map[string]int{"k": busy}, nil, rec.save)
	if err != nil {
		t.Fatal(err)
	}
	if !temp || got == busy || !Free(got) {
		t.Fatalf("got %d temp=%v; want a free temporary port other than %d", got, temp, busy)
	}
	if len(rec) != 0 {
		t.Fatalf("temporary port must not be persisted: %v", rec)
	}
}

func TestAssignReservedSavedIsTemporary(t *testing.T) {
	p := freePort(t)
	rec := recorder{}
	got, temp, err := Assign("k", 80, map[string]int{"k": p}, map[int]bool{p: true}, rec.save)
	if err != nil {
		t.Fatal(err)
	}
	if !temp || got == p {
		t.Fatalf("got %d temp=%v", got, temp)
	}
	if len(rec) != 0 {
		t.Fatalf("temporary port must not be persisted: %v", rec)
	}
}

func TestAssignNewAllocation(t *testing.T) {
	// Find a remote whose default port and the next one are both free so the
	// expectations are deterministic.
	var remote int
	for r := 20000; r < 30000; r++ {
		if Free(Default(r)) && Free(Default(r)+1) && Free(Default(r)+2) {
			remote = r
			break
		}
	}
	if remote == 0 {
		t.Skip("no free port range found")
	}
	def := Default(remote)

	tests := []struct {
		name     string
		reserved map[int]bool
		busy     []int
		want     int
	}{
		{name: "default port", want: def},
		{name: "reserved skipped", reserved: map[int]bool{def: true}, want: def + 1},
		{name: "busy skipped", busy: []int{def}, want: def + 1},
		{name: "busy and reserved skipped", reserved: map[int]bool{def + 1: true}, busy: []int{def}, want: def + 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, b := range tt.busy {
				occupy(t, b)
			}
			rec := recorder{}
			got, temp, err := Assign("k", remote, map[string]int{"other": def + 5}, tt.reserved, rec.save)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want || temp {
				t.Fatalf("got %d temp=%v, want %d false", got, temp, tt.want)
			}
			if rec["k"] != tt.want || len(rec) != 1 {
				t.Fatalf("persisted %v, want k=%d", rec, tt.want)
			}
		})
	}
}

func TestAssignSaveError(t *testing.T) {
	boom := errors.New("boom")
	_, _, err := Assign("k", 20001, nil, nil, func(string, int) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped boom", err)
	}
}
