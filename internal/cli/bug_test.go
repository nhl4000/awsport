package cli

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nhl4000/awsport/internal/bugreport"
)

func TestAskDescription(t *testing.T) {
	for _, tc := range []struct {
		name, in, title, what string
		wantErr               bool
	}{
		{"empty line ends", "Tunnel drops\nIt stops after 5 min.\nEvery time.\n\nignored\n", "Tunnel drops", "It stops after 5 min.\nEvery time.", false},
		{"end of input ends", "Crash on start\nstack below\nno newline", "Crash on start", "stack below\nno newline", false},
		{"no description", "Only a title\n", "Only a title", "", false},
		{"no title", "\n", "", "", true},
		{"closed input", "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title, what, err := askDescription(bufio.NewReader(strings.NewReader(tc.in)), io.Discard)
			if (err != nil) != tc.wantErr || title != tc.title || what != tc.what {
				t.Errorf("got %q, %q, %v", title, what, err)
			}
		})
	}
}

func TestConfirm(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, "n\n": false, "\n": false, "": false, "sure\n": false} {
		got, err := confirm(bufio.NewReader(strings.NewReader(in)), io.Discard, "? ")
		if err != nil || got != want {
			t.Errorf("confirm(%q) = %v, %v", in, got, err)
		}
	}
}

func TestCrashedWritesLogAndExits2(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWSPORT_STATE_DIR", dir)
	if code := crashed(&bugreport.Panic{Value: "boom", Stack: []byte("goroutine 7 [running]:\ntui.Update()\n")}, []byte("recover stack"), "v1.0.0", "abc"); code != 2 {
		t.Errorf("exit code %d", code)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "crash-*.log"))
	if len(files) != 1 {
		t.Fatalf("crash files %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if !strings.Contains(string(b), "panic: boom") || !strings.Contains(string(b), "tui.Update()") || strings.Contains(string(b), "recover stack") {
		t.Errorf("crash log should carry the *Panic's value and stack:\n%s", b)
	}
}
