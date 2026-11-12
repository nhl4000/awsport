// Package doctor diagnoses an awsport installation: tools, files, SSO,
// browser, AWS profiles and the background agent. Checks produce a pure
// result model (sections of checks, each ok/warn/fail with a fix) that is
// rendered for the terminal, as JSON or as a redacted Markdown report.
package doctor

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Status is the outcome of one check.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Symbol is the status mark shown before a check.
func (s Status) Symbol() string {
	switch s {
	case Fail:
		return "✗"
	case Warn:
		return "⚠"
	}
	return "✓"
}

// Check is one finding. Fix is a concrete action (a command to run or what
// to ask an admin); it is set for every warn and fail. Detail and Fix may
// span several lines.
type Check struct {
	Status Status `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// Section groups related checks under a heading.
type Section struct {
	Name   string  `json:"name"`
	Checks []Check `json:"checks"`
}

// Environment describes the machine the checks ran on.
type Environment struct {
	Version string   `json:"version"`
	Commit  string   `json:"commit,omitempty"`
	OS      string   `json:"os"`
	Arch    string   `json:"arch"`
	WSL     bool     `json:"wsl"`
	AWSCLI  string   `json:"aws_cli,omitempty"`                // version, empty when missing
	Plugin  string   `json:"session_manager_plugin,omitempty"` // version, empty when missing
	EnvVars []string `json:"env_vars,omitempty"`               // names of AWS_* / AWSPORT_* variables that are set
}

// Result is the outcome of a doctor run.
type Result struct {
	Generated   time.Time   `json:"generated"`
	Environment Environment `json:"environment"`
	Sections    []Section   `json:"sections"`
}

// Count returns how many checks have status s.
func (r Result) Count(s Status) int {
	n := 0
	for _, sec := range r.Sections {
		for _, c := range sec.Checks {
			if c.Status == s {
				n++
			}
		}
	}
	return n
}

// Worst is the most severe status of all checks (OK when there are none).
func (r Result) Worst() Status {
	switch {
	case r.Count(Fail) > 0:
		return Fail
	case r.Count(Warn) > 0:
		return Warn
	}
	return OK
}

// ExitCode is 1 when any check failed, else 0.
func (r Result) ExitCode() int {
	if r.Worst() == Fail {
		return 1
	}
	return 0
}

// Summary is a one-line tally, e.g. "1 failure, 2 warnings, 14 ok".
func (r Result) Summary() string {
	var parts []string
	if n := r.Count(Fail); n > 0 {
		parts = append(parts, plural(n, "failure", "failures"))
	}
	if n := r.Count(Warn); n > 0 {
		parts = append(parts, plural(n, "warning", "warnings"))
	}
	parts = append(parts, fmt.Sprintf("%d ok", r.Count(OK)))
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Render writes the result for a terminal: a heading per section and one
// line per check, followed by indented detail and "→ fix:" lines. Colors are
// used only when w is a color terminal.
func Render(w io.Writer, r Result) error {
	re := lipgloss.NewRenderer(w)
	bold := re.NewStyle().Bold(true)
	styles := map[Status]lipgloss.Style{
		OK:   re.NewStyle().Foreground(lipgloss.Color("2")), // ANSI colors follow the terminal theme
		Warn: re.NewStyle().Foreground(lipgloss.Color("3")),
		Fail: re.NewStyle().Foreground(lipgloss.Color("1")),
	}
	faint := re.NewStyle().Faint(true)
	var b strings.Builder
	for i, sec := range r.Sections {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(bold.Render(sec.Name) + "\n")
		for _, c := range sec.Checks {
			b.WriteString("  " + styles[c.Status].Render(c.Status.Symbol()) + " " + c.Title + "\n")
			for _, l := range lines(c.Detail) {
				b.WriteString("    " + faint.Render(l) + "\n")
			}
			if c.Fix != "" && c.Status != OK {
				for j, l := range lines(c.Fix) {
					prefix := "    → fix: "
					if j > 0 {
						prefix = "           "
					}
					b.WriteString(prefix + l + "\n")
				}
			}
		}
	}
	b.WriteString("\n" + bold.Render(r.Summary()) + "\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func lines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
