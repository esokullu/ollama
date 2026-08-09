//go:build windows

package browser

import (
	"os/exec"
	"strconv"
	"syscall"
)

// configureMCPProcess keeps the node/npx child from flashing a console window
// in front of the app.
func configureMCPProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

// killMCPProcess terminates the server and anything it spawned. `npx.cmd`
// launches the real server as a further child, and killing only the launcher
// would leave that child holding the WebBrain bridge port.
func killMCPProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// /T takes the process tree; there is no process-group equivalent here.
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := kill.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
