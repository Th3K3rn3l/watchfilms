//go:build windows

package tunnel

import (
	"os/exec"
	"syscall"
)

// hideWindow prevents cloudflared from opening a second console window.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
