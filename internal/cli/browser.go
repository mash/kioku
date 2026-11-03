package cli

import (
	"os/exec"
	"runtime"
)

func openBrowser(u string) {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
		args = []string{u}
	case "windows":
		command = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", u}
	default:
		command = "xdg-open"
		args = []string{u}
	}
	cmd := exec.Command(command, args...)
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
}
