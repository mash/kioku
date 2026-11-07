//go:build !linux

package execrunner

import "syscall"

func processAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
