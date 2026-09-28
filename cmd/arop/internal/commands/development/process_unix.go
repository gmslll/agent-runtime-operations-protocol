//go:build !windows

package development

import (
	"os/exec"
	"syscall"
	"time"
)

func configureProcess(process *exec.Cmd) {
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopProcess(process *exec.Cmd, done <-chan error, timeout time.Duration) {
	_ = syscall.Kill(-process.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(timeout):
		_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
		<-done
	}
}
