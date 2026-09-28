//go:build windows

package development

import (
	"os/exec"
	"time"
)

func configureProcess(*exec.Cmd) {}

func stopProcess(process *exec.Cmd, done <-chan error, timeout time.Duration) {
	_ = process.Process.Kill()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = process.Process.Kill()
	}
}
