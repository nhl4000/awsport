package bugreport

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWriteCrashKeepsNewestTen(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 11, 1, 9, 0, 0, 0, time.Local)
	var paths []string
	for i := range 12 {
		p, err := WriteCrash(dir, Crash{
			Value: "boom", Stack: []byte("goroutine 1 [running]:\n"), Version: "v1.2.3", Commit: "abc1234",
			Args: []string{"awsport", "up"}, Time: start.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, filepath.Base(p))
	}
	if paths[0] != "crash-20261101-090000.log" {
		t.Errorf("name %q", paths[0])
	}
	if got := crashFiles(dir); !slices.Equal(got, paths[2:]) {
		t.Errorf("kept %v, want %v", got, paths[2:])
	}
	b, err := os.ReadFile(filepath.Join(dir, paths[11]))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"version: v1.2.3 (abc1234)", "command: awsport up", "panic: boom", "goroutine 1 [running]:"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("crash log lacks %q:\n%s", want, b)
		}
	}
}

func TestLatestCrash(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok := LatestCrash(dir, now); ok {
		t.Error("empty dir has a crash")
	}
	write("crash-20261101-100000.log") // 11 days old
	if _, _, ok := LatestCrash(dir, now); ok {
		t.Error("crash older than 7 days attached")
	}
	write("crash-20261110-100000.log")
	write("crash-notes.log") // not ours, sorts last
	name, content, ok := LatestCrash(dir, now)
	if !ok || name != "crash-20261110-100000.log" || content != name {
		t.Errorf("LatestCrash = %q, %q, %v", name, content, ok)
	}
}

func TestTailFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.log")
	var lines []string
	for i := range 100 {
		lines = append(lines, strings.Repeat("x", i))
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := TailFile(p, 40)
	if err != nil || !slices.Equal(got, lines[60:]) {
		t.Errorf("TailFile = %d lines, %v", len(got), err)
	}
	empty := filepath.Join(t.TempDir(), "empty.log")
	_ = os.WriteFile(empty, nil, 0o600)
	if got, err := TailFile(empty, 40); err != nil || len(got) != 0 {
		t.Errorf("empty file: %q, %v", got, err)
	}
}
