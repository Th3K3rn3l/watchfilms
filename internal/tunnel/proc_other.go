//go:build !windows

package tunnel

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}
