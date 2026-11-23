//go:build windows

package daemon

import (
	"os"
	"os/exec"
)

func detachCmd(cmd *exec.Cmd) {}

func terminateProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
