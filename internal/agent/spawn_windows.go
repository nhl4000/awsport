//go:build windows

package agent

import (
	"os/exec"
	"syscall"
)

// detachedProcess is DETACHED_PROCESS: the agent gets no console.
const detachedProcess = 0x00000008

// detach starts the agent without a console and outside the invoking
// console's Ctrl-C group, so that it survives the invoking command.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
		HideWindow:    true,
	}
}
