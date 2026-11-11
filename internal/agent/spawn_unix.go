//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

// detach starts the agent in its own session, so that it survives the
// terminal and the process group of the invoking command.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
