//go:build unix

package tunnel

import (
	"testing"
	"time"
)

// Unix-only: Stop interrupts the process group and only kills after the grace
// period. Windows has no interrupt for child processes and kills immediately.
func TestStopKillsAfterGrace(t *testing.T) {
	m, _ := newTestManager(t, Options{}, "stubborn")
	m.killAfter = 200 * time.Millisecond
	spec := testSpec("k", "l")
	if err := m.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, spec.Key, Up)
	start := time.Now()
	m.Stop(spec.Key)
	if d := time.Since(start); d < m.killAfter || d > 5*time.Second {
		t.Fatalf("Stop of a process ignoring interrupt took %v, want ~%v", d, m.killAfter)
	}
}
