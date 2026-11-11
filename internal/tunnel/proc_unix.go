//go:build unix

package tunnel

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// configureCmd puts the session in its own process group so that stopping it
// also reaches the session-manager-plugin child, and makes cancellation send
// SIGINT to the group; exec kills the process after killAfter.
func configureCmd(cmd *exec.Cmd, killAfter time.Duration) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
			return cmd.Process.Signal(os.Interrupt)
		}
		return nil
	}
	cmd.WaitDelay = killAfter
}

// killGroup kills any process left in the session's process group after the
// leader exited (e.g. an orphaned session-manager-plugin holding the port).
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
