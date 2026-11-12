package tui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

type bugDoneMsg struct{ err error }

// reportBug hands the terminal to `awsport bug` (this executable) and
// returns to the UI once the user has read its output.
func (m *model) reportBug() tea.Cmd {
	exe, err := os.Executable()
	if err != nil {
		m.setErr(fmt.Errorf("report bug: find awsport executable: %w", err))
		return nil
	}
	return tea.Exec(&pausedExec{cmd: exec.Command(exe, "bug")}, func(err error) tea.Msg { return bugDoneMsg{err: err} })
}

// pausedExec runs cmd on the released terminal, then waits for Enter so its
// output stays readable before the full-screen UI comes back.
type pausedExec struct {
	cmd            *exec.Cmd
	stdin          io.Reader
	stdout, stderr io.Writer
}

func (e *pausedExec) SetStdin(r io.Reader)  { e.stdin = r }
func (e *pausedExec) SetStdout(w io.Writer) { e.stdout = w }
func (e *pausedExec) SetStderr(w io.Writer) { e.stderr = w }

func (e *pausedExec) Run() error {
	if e.stdin == nil {
		e.stdin = os.Stdin
	}
	if e.stdout == nil {
		e.stdout = os.Stdout
	}
	if e.stderr == nil {
		e.stderr = os.Stderr
	}
	e.cmd.Stdin, e.cmd.Stdout, e.cmd.Stderr = e.stdin, e.stdout, e.stderr
	err := e.cmd.Run()
	fmt.Fprint(e.stdout, "\nPress Enter to return to awsport…")
	_, _ = bufio.NewReader(e.stdin).ReadString('\n')
	return err
}
