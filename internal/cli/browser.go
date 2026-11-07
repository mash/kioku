package cli

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// browserOrigin rejects non-HTTP URLs and launcher options before any process
// starts. Callers validate before displaying the URL.
func browserOrigin(server, target string) (string, error) {
	expected, err := normalizeOrigin(server)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(target)
	if err == nil && u.User == nil && u.Opaque == "" && u.Host != "" {
		if actual, err := normalizeOrigin(u.Scheme + "://" + u.Host); err == nil && actual == expected {
			return actual, nil
		}
	}
	return "", fmt.Errorf("browser URL %q does not match server origin %q", target, expected)
}

// Replaceable in tests so login can exercise the real loopback listener without
// opening an interactive browser. Keep validation here as well as at callers.
var openBrowser = func(server, target string) error {
	if _, err := browserOrigin(server, target); err != nil {
		return err
	}
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
		args = []string{target}
	case "windows":
		command = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", target}
	default:
		command = "xdg-open"
		args = []string{target}
	}
	cmd := exec.Command(command, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
