package server

import (
	"strings"
	"testing"
)

func TestHomeProjectOnboardingUsesCurrentCLI(t *testing.T) {
	if !strings.Contains(homeTemplate, "kioku project create &lt;name&gt;") {
		t.Fatal("new users have no create instruction")
	}
	for _, removed := range []string{"kioku init", "kioku use", "kioku hook"} {
		if strings.Contains(homeTemplate, removed) {
			t.Fatalf("onboarding recommends removed command: %s", removed)
		}
	}
}
