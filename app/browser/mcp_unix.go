//go:build !windows

package browser

import (
	"os/exec"
	"syscall"
)

// configureMCPProcess puts the child in its own process group. The launcher is
// usually `npx`, which spawns the real server as a further child, so the group
// is the only handle that reaches every process we started.
func configureMCPProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killMCPProcess terminates the whole process group.
//
// Closing stdin is the documented way to stop an MCP server, but it is not
// sufficient here: under `npx` the wrapper keeps the pipe alive and the server
// never sees EOF, so it survives as an orphan still holding the WebBrain
// bridge port -- which then blocks every later attempt to start one.
func killMCPProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Negative pid addresses the group. Setpgid made the child its leader, so
	// its pid is the group id.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
