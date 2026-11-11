package tunnel

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// processGone reports whether pid has exited (absent or a zombie awaiting reaping).
func processGone(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	// Format: "pid (comm) state ..."; comm may contain spaces, so split after ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && strings.HasPrefix(strings.TrimSpace(s[i+1:]), "Z")
}

func TestStopKillsProcessGroup(t *testing.T) {
	m, _ := newTestManager(t, Options{}, "family")
	m.killAfter = 200 * time.Millisecond
	spec := testSpec("k", "l")
	if err := m.Start(spec); err != nil {
		t.Fatal(err)
	}
	s := waitStatus(t, m, spec.Key, Up)
	var child int
	for _, l := range s.LastLog {
		if _, err := fmt.Sscanf(l, "child %d", &child); err == nil {
			break
		}
	}
	if child == 0 {
		t.Fatalf("child pid not reported: %q", s.LastLog)
	}
	if processGone(child) {
		t.Fatalf("child %d not running before Stop", child)
	}
	m.Stop(spec.Key)
	deadline := time.Now().Add(5 * time.Second)
	for !processGone(child) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived Stop", child)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
