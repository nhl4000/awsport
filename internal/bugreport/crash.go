package bugreport

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

const (
	crashPrefix = "crash-"
	crashSuffix = ".log"
	crashLayout = "20060102-150405"
	// KeepCrashes is how many crash logs are kept; older ones are deleted.
	KeepCrashes = 10
	// CrashMaxAge is how old a crash log may be to be attached to a report.
	CrashMaxAge = 7 * 24 * time.Hour
)

// Panic is a recovered panic with the stack of the goroutine that panicked.
// Code that recovers a panic away from the top-level handler (e.g. inside
// the TUI) re-panics with a *Panic so the original stack is kept.
type Panic struct {
	Value any
	Stack []byte
}

func (p *Panic) Error() string { return fmt.Sprint(p.Value) }

// Crash describes a crash for WriteCrash.
type Crash struct {
	Value           any    // the panic value
	Stack           []byte // goroutine stack trace
	Version, Commit string
	Args            []string // command line
	Time            time.Time
}

// WriteCrash writes c to dir/crash-YYYYMMDD-HHMMSS.log (mode 0600) and
// deletes all but the newest KeepCrashes crash logs. It returns the path.
func WriteCrash(dir string, c Crash) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "awsport crash at %s\n", c.Time.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "version: %s (%s) %s/%s %s\n", c.Version, c.Commit, runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Fprintf(&b, "command: %s\n", strings.Join(c.Args, " "))
	fmt.Fprintf(&b, "panic: %v\n\n%s", c.Value, c.Stack)
	path := filepath.Join(dir, crashPrefix+c.Time.Format(crashLayout)+crashSuffix)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("write crash log: %w", err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return "", fmt.Errorf("write crash log: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write crash log: %w", err)
	}
	pruneCrashes(dir, KeepCrashes)
	return path, nil
}

// crashFiles returns the crash log names in dir, oldest first (the
// timestamped names sort chronologically).
func crashFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(n, crashPrefix) && strings.HasSuffix(n, crashSuffix) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func pruneCrashes(dir string, keep int) {
	files := crashFiles(dir)
	for len(files) > keep {
		_ = os.Remove(filepath.Join(dir, files[0]))
		files = files[1:]
	}
}

// LatestCrash returns the name and content of the newest crash log in dir
// written within CrashMaxAge of now; ok is false when there is none.
func LatestCrash(dir string, now time.Time) (name, content string, ok bool) {
	files := crashFiles(dir)
	for i := len(files) - 1; i >= 0; i-- {
		stamp := strings.TrimSuffix(strings.TrimPrefix(files[i], crashPrefix), crashSuffix)
		t, err := time.ParseInLocation(crashLayout, stamp, time.Local)
		if err != nil {
			continue // not one of ours
		}
		if now.Sub(t) > CrashMaxAge {
			return "", "", false
		}
		b, err := os.ReadFile(filepath.Join(dir, files[i]))
		if err != nil {
			return "", "", false
		}
		return files[i], string(b), true
	}
	return "", "", false
}

// TailFile returns the last n lines of the file at path, reading at most
// the final 256 KiB.
func TailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const maxRead = 256 << 10
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := max(fi.Size()-maxRead, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // partial first line
	}
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines[max(len(lines)-n, 0):], nil
}
