//go:build windows

package tunnel

import (
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// configureCmd detaches the session from the console's Ctrl-C group and makes
// cancellation terminate the whole process tree (aws.exe and its
// session-manager-plugin child); exec force-closes I/O after killAfter.
func configureCmd(cmd *exec.Cmd, killAfter time.Duration) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
	cmd.Cancel = func() error {
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = killAfter
}

// killGroup is a no-op on Windows: the tree is terminated by Cancel.
func killGroup(*exec.Cmd) {}
