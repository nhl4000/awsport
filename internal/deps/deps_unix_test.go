//go:build unix

package deps

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin writes an executable shell script named name into dir.
func fakeBin(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCheckBinary(t *testing.T) {
	tests := []struct {
		name        string
		script      string // "" = binary absent
		wantFound   bool
		wantOK      bool
		wantVersion string
		wantProblem string
	}{
		{"aws v2", `echo "aws-cli/2.15.30 Python/3.11.8 Linux/6.5.0 exe/x86_64.ubuntu.22"`, true, true, "2.15.30", ""},
		{"aws v1 on stderr", `echo "aws-cli/1.29.62 Python/3.10.12 Linux/5.15.0 botocore/1.31.62" >&2`, true, false, "1.29.62", "version 2 is required"},
		{"broken binary", `echo "boom" >&2; exit 2`, true, false, "", "--version failed"},
		{"missing", "", false, false, "", "not found in PATH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.script != "" {
				fakeBin(t, dir, "aws", tt.script)
			}
			t.Setenv("PATH", dir)
			c := checkBinary(context.Background(), nameAWS, parseAWSVersion, Install{DocsURL: awsDocsURL})
			if c.Found != tt.wantFound || c.OK != tt.wantOK || c.Version != tt.wantVersion || !strings.Contains(c.Problem, tt.wantProblem) {
				t.Fatalf("check = %+v", c)
			}
			if tt.wantOK && c.Problem != "" {
				t.Fatalf("OK check has problem %q", c.Problem)
			}
			if c.Found && c.Path != filepath.Join(dir, "aws") {
				t.Fatalf("Path = %q", c.Path)
			}
		})
	}
}

func TestCheckAll(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "aws", `echo "aws-cli/2.15.30 Python/3.11.8"`)
	fakeBin(t, dir, "session-manager-plugin", `echo "1.2.553.0"`)
	t.Setenv("PATH", dir)
	checks := CheckAll(context.Background())
	if len(checks) != 2 || checks[0].Name != "aws" || checks[1].Name != "session-manager-plugin" {
		t.Fatalf("checks = %+v", checks)
	}
	for _, c := range checks {
		if !c.OK || c.Install.DocsURL == "" {
			t.Fatalf("check %+v", c)
		}
	}
	if checks[1].Version != "1.2.553.0" {
		t.Fatalf("plugin version = %q", checks[1].Version)
	}
}

func TestRun(t *testing.T) {
	var out, errOut bytes.Buffer
	in := Install{Commands: [][]string{{"sh", "-c", "echo one"}, {"sh", "-c", "echo two >&2"}}}
	if err := Run(context.Background(), in, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "$ sh -c echo one\none\n$ sh -c echo two >&2\n" {
		t.Fatalf("stdout = %q", got)
	}
	if errOut.String() != "two\n" {
		t.Fatalf("stderr = %q", errOut.String())
	}

	out.Reset()
	failing := Install{Commands: [][]string{{"sh", "-c", "exit 3"}, {"sh", "-c", "echo never"}}}
	err := Run(context.Background(), failing, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "install step 1") || strings.Contains(out.String(), "never") {
		t.Fatalf("Run failing = %v, out %q", err, out.String())
	}

	if err := Run(context.Background(), Install{DocsURL: pluginDocsURL}, &out, &errOut); err == nil || !strings.Contains(err.Error(), pluginDocsURL) {
		t.Fatalf("Run without commands = %v", err)
	}
}
